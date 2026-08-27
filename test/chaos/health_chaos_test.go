//go:build chaos

package chaos

import (
	"fmt"
	"strings"
	"testing"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/health"
)

// Epic-10 Task 5, health scenarios: probes and traffic sharing one
// backend, and every backend coming back at once.

// TestChaosHealthProbeRacesTraffic: a server capped at one concurrent
// transaction, probed continuously. A probe is not a transaction — it must
// not consume the single slot (which would answer real mail with 451
// 4.3.2), and traffic must not starve the probes either.
func TestChaosHealthProbeRacesTraffic(t *testing.T) {
	const messages = 25

	f := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	cfg := poolConfig(chaosTimeouts(), "none", f.Addr())
	cfg.Pools[0].Servers[0].MaxTransactions = 1 // concurrency 1
	s := newHealthStack(t, cfg)

	waitFor(t, "the first probe to complete", func() bool { return probeCount(s, "s0") > 0 })
	probesBefore := probeCount(s, "s0")

	c := openSession(t, s.addr, "raceprobe")
	if c == nil {
		return
	}
	for i := 0; i < messages; i++ {
		marker := fmt.Sprintf("raceprobe-%d", i)
		got, err := c.message(marker)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got != "250 2.0.0 OK: queued" {
			t.Fatalf("message %d verdict = %q, want the backend's 250: a probe took the transaction slot?", i, got)
		}
		if n := recordedBodies(f)[wantBody(marker)]; n != 1 {
			t.Errorf("message %d arrived %d times, want 1", i, n)
		}
	}

	// Probing kept up throughout, so traffic never starved it either.
	waitFor(t, "probing to continue under traffic", func() bool { return probeCount(s, "s0") > probesBefore })
	if got := s.checker.Status("p", "s0").Op; got != health.OpUp {
		t.Errorf("Op after %d transactions racing the probes = %v, want UP", messages, got)
	}
}

// TestChaosRecoveryStorm: every backend is down, then every one of them
// comes back in the same instant. All three must be picked up by the
// checker and traffic must redistribute across all of them — no server
// left behind because the recovery of another one happened to be noticed
// first.
func TestChaosRecoveryStorm(t *testing.T) {
	const messages = 30

	fakes := []*fakesmtp.Server{
		fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps(), OnEOD: []fakesmtp.Step{{Reply: "250 2.0.0 OK: queued by s0"}}}),
		fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps(), OnEOD: []fakesmtp.Step{{Reply: "250 2.0.0 OK: queued by s1"}}}),
		fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps(), OnEOD: []fakesmtp.Step{{Reply: "250 2.0.0 OK: queued by s2"}}}),
	}
	addrs := make([]string, len(fakes))
	for i, f := range fakes {
		addrs[i] = f.Addr()
	}
	s := newHealthStack(t, poolConfig(chaosTimeouts(), "none", addrs...))

	for _, f := range fakes {
		f.SetDown(fakesmtp.DownListenerClosed)
	}
	for i := range fakes {
		waitOp(t, s, fmt.Sprintf("s%d", i), health.OpDown)
	}

	// Nothing eligible: transaction-scoped 451, session intact.
	c := openSession(t, s.addr, "storm")
	if c == nil {
		return
	}
	got, err := c.message("during-the-outage")
	if err != nil {
		t.Fatalf("message during the outage: %v", err)
	}
	if !strings.HasPrefix(got, "451") {
		t.Errorf("verdict during a total outage = %q, want a 451", got)
	}

	// Everything comes back at once.
	for _, f := range fakes {
		f.SetUp()
	}
	for i := range fakes {
		waitOp(t, s, fmt.Sprintf("s%d", i), health.OpUp)
	}

	verdicts := map[string]int{}
	for i := 0; i < messages; i++ {
		got, err := c.message(fmt.Sprintf("storm-%d", i))
		if err != nil {
			t.Fatalf("post-recovery message %d: %v", i, err)
		}
		if !strings.HasPrefix(got, "250") {
			t.Fatalf("post-recovery message %d verdict = %q, want a backend 250", i, got)
		}
		verdicts[got]++
	}
	for i := range fakes {
		want := fmt.Sprintf("250 2.0.0 OK: queued by s%d", i)
		if verdicts[want] == 0 {
			t.Errorf("server s%d took no traffic after the recovery storm (verdicts: %v)", i, verdicts)
		}
	}
}

//go:build integration

package integration

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/health"
)

// startChecker builds a Checker over cfg with the real clock, running
// (Run) in its own goroutine, and returns it plus a bounded stop func.
// TestMain (m1_test.go) already wraps this package's whole run in
// goleak, so nothing here needs its own leak check — only a clean,
// bounded shutdown before the test returns.
func startChecker(t *testing.T, cfg *config.Config) (*health.Checker, *config.Holder, func()) {
	t.Helper()
	holder := &config.Holder{}
	holder.Swap(cfg)
	c := health.New(holder, nil, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Checker.Run did not return after ctx cancel")
		}
	}
	return c, holder, stop
}

// waitOpHealth polls (bounded) until (pool,server)'s Status.Op reaches
// want — event-driven synchronization instead of a wall-clock sleep.
func waitOpHealth(t *testing.T, c *health.Checker, pool, server string, want health.OpState) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := c.Status(pool, server).Op
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Status(%s,%s).Op never reached %v (stuck at %v)", pool, server, want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitConsecAtLeast polls (bounded) until get(Status) >= n.
func waitConsecAtLeast(t *testing.T, c *health.Checker, pool, server string, n int, get func(health.Status) int, label string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := get(c.Status(pool, server)); got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s(%s,%s) never reached %d", label, pool, server, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// checkServer builds a one-pool, one-server config with the given
// CheckParams applied.
func checkServer(name, addr string, check config.CheckParams) *config.Config {
	return &config.Config{Pools: []config.Pool{{
		Name:    "p",
		Servers: []config.Server{{Name: name, Address: addr, Check: check}},
	}}}
}

// TestFlapScriptMatchesPredictedTransitions: an L0 (connect-level) probe
// against a fake whose listener is closed and reopened on command makes
// the FSM's rise/fall counter math directly observable — SetDown always
// refuses new connections deterministically, so waiting for
// ConsecFail/ConsecOK to cross fall/rise is an exact, event-driven proxy
// for "the predicted number of failed/successful probes has elapsed",
// with no wall-clock guessing. The recorded Op transition log (sampled
// by a fast background poller) must equal exactly what that counter
// math predicts: UP -> DOWN (at fall) -> UP (at rise), no more, no
// fewer.
func TestFlapScriptMatchesPredictedTransitions(t *testing.T) {
	const (
		rise = 2
		fall = 3
	)
	srv := fakesmtp.Start(t, fakesmtp.Script{})
	cfg := checkServer("s1", srv.Addr(), config.CheckParams{
		Level: "connect", Interval: 30 * time.Millisecond, DownInterval: 30 * time.Millisecond,
		Timeout: time.Second, Rise: rise, Fall: fall,
	})
	c, _, stop := startChecker(t, cfg)
	defer stop()

	waitOpHealth(t, c, "p", "s1", health.OpUp) // steady state confirmed before flapping

	// A fast background sampler records every distinct Op value seen
	// (collapsing consecutive repeats) and is its own authority on when
	// it's done: it self-terminates the instant it has recorded the
	// full predicted sequence, rather than waiting for an externally
	// signaled stop. An external stop races the sampler's own ticker —
	// the main goroutine can observe the terminal state and signal stop
	// before the sampler's next tick fires, dropping the final sample.
	// Self-termination has no such race: "done" and "recorded" are the
	// same event.
	want := []health.OpState{health.OpUp, health.OpDown, health.OpUp}
	logDone := make(chan []health.OpState, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		var log []health.OpState
		for range ticker.C {
			op := c.Status("p", "s1").Op
			if len(log) == 0 || log[len(log)-1] != op {
				log = append(log, op)
			}
			if len(log) >= len(want) {
				logDone <- log
				return
			}
		}
	}()

	srv.SetDown(fakesmtp.DownListenerClosed)
	waitConsecAtLeast(t, c, "p", "s1", fall, func(s health.Status) int { return s.ConsecFail }, "ConsecFail")
	if got := c.Status("p", "s1").Op; got != health.OpDown {
		t.Fatalf("Op once ConsecFail >= fall = %v, want DOWN", got)
	}

	srv.SetUp()
	waitConsecAtLeast(t, c, "p", "s1", rise, func(s health.Status) int { return s.ConsecOK }, "ConsecOK")
	if got := c.Status("p", "s1").Op; got != health.OpUp {
		t.Fatalf("Op once ConsecOK >= rise = %v, want UP", got)
	}

	var got []health.OpState
	select {
	case got = <-logDone:
	case <-time.After(10 * time.Second):
		t.Fatal("sampler never recorded the predicted transition sequence")
	}

	if len(got) != len(want) {
		t.Fatalf("transition log = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transition log = %v, want %v", got, want)
		}
	}
}

// TestHundredBackendsProbeLoad: 100 fakes (20 of them accept-then-hang),
// probed at a 200ms interval, running for the better part of 30s.
// Claims: the goroutine count stays flat (no per-probe leak — the
// ctx-watcher goroutines probeBanner/backend.Dial start per attempt must
// all exit), and the 80 healthy fakes' probe cadence is unaffected by
// the 20 hanging ones (no head-of-line blocking from the
// goroutine-per-server design).
//
// down_interval is set generously (10s, PROJECT.md's own "reduces
// dead-server churn" rationale) so a hanging fake — which fails every
// attempt — only gets re-probed a handful of times over the run, rather
// than every 200ms: that keeps this test's own resource use (each
// attempt against a hung fake leaves its accept-side goroutine parked
// until fakesmtp.Server.Stop, since AcceptThenHang has no way to know
// the prober gave up) bounded and unrelated to whatever this test is
// actually trying to measure.
func TestHundredBackendsProbeLoad(t *testing.T) {
	const (
		total    = 100
		hanging  = 20
		interval = 200 * time.Millisecond
		settle   = 6 * time.Second
		runFor   = 30 * time.Second
	)

	fakes := make([]*fakesmtp.Server, total)
	for i := range fakes {
		fakes[i] = fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING"}})
		if i < hanging {
			fakes[i].SetDown(fakesmtp.DownAcceptThenHang)
		}
	}

	pool := config.Pool{Name: "p"}
	for i, f := range fakes {
		pool.Servers = append(pool.Servers, config.Server{
			Name:    fmt.Sprintf("s%d", i),
			Address: f.Addr(),
			Check: config.CheckParams{
				Level: "banner", Interval: interval, DownInterval: 10 * time.Second,
				Timeout: 500 * time.Millisecond, Rise: 2, Fall: 3,
			},
		})
	}
	_, _, stop := startChecker(t, &config.Config{Pools: []config.Pool{pool}})
	defer stop()

	// Let the initial spread (up to interval, well under 5s) and the
	// hanging fakes' first fall failures (a few seconds at fastinter
	// cadence) settle before treating the goroutine count as a
	// baseline.
	time.Sleep(settle)
	base := runtime.NumGoroutine()

	time.Sleep(runFor - settle)
	final := runtime.NumGoroutine()

	// Generous but discriminating: a genuine per-probe leak over
	// ~(runFor-settle)/interval*  (total-hanging) probe cycles would be
	// in the thousands: this tolerance only absorbs the bounded,
	// expected drift from the hanging fakes' own occasional re-probes
	// (at most a couple of rounds at the 10s down_interval) plus normal
	// runtime background noise.
	if drift := final - base; drift > 150 {
		t.Errorf("goroutine count drifted by %d (base=%d final=%d) over %s, want it flat", drift, base, final, runFor-settle)
	}

	// Cadence + no head-of-line blocking: every healthy fake kept pace
	// on its own, independent of the 20 hanging ones.
	wantMin := int(runFor/interval) / 3 // generous floor: a third of the naive full-run count
	for i := hanging; i < total; i++ {
		if got := fakes[i].DialCount(); got < wantMin {
			t.Errorf("fake s%d DialCount = %d over %s at interval %s, want >= %d (head-of-line blocking from the hanging fakes?)",
				i, got, runFor, interval, wantMin)
		}
	}
}

// TestDNSFailureCountsAsProbeFailure: an unresolvable host counts as a
// probe failure like any other (bounded by the probe's own timeout, not
// however long DNS resolution would otherwise take) — and recovery
// works the same way any other reload does: a config swap to a valid
// address for the same (pool, server) identity, then rise consecutive
// successes against the new address.
func TestDNSFailureCountsAsProbeFailure(t *testing.T) {
	check := config.CheckParams{
		Level: "connect", Interval: 50 * time.Millisecond, DownInterval: 50 * time.Millisecond,
		Timeout: 2 * time.Second, Rise: 2, Fall: 1,
	}
	// RFC 2606 reserves .invalid for exactly this: guaranteed never to
	// resolve.
	cfg1 := checkServer("s1", "this-host-does-not-exist.invalid:25", check)
	c, holder, stop := startChecker(t, cfg1)
	defer stop()

	waitOpHealth(t, c, "p", "s1", health.OpDown) // bounded by waitOpHealth's own deadline: proves it didn't hang on DNS

	srv := fakesmtp.Start(t, fakesmtp.Script{})
	holder.Swap(checkServer("s1", srv.Addr(), check))

	waitOpHealth(t, c, "p", "s1", health.OpUp) // reload noticed, rise successes against the new address bring it back
}

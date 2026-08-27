//go:build integration

package integration

import (
	"bytes"
	"context"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/admin"
	"github.com/littlebugger/bifrost/internal/balance"
	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/health"
	"github.com/littlebugger/bifrost/internal/metrics"
	"github.com/littlebugger/bifrost/internal/proxy"
	"github.com/littlebugger/bifrost/internal/smtpdrv"
)

// This file is epic-09 Task 5's M2 slice: the drain-visibility proof.
// TestMain (m1_test.go) already wraps this package in goleak.

// drainCheck is a fast, deterministic L0 (plain TCP connect, no SMTP
// bytes) health check: against fakesmtp's always-accepting listener it
// never fails, so both servers stay OpState UP for this test's whole
// run — only AdminState (ready/drain) ever changes their eligibility.
func drainCheck() config.CheckParams {
	return config.CheckParams{
		Level: "connect", Rise: 1, Fall: 1,
		Interval: 20 * time.Millisecond, DownInterval: 20 * time.Millisecond, Timeout: time.Second,
	}
}

// drainConfig is a two-server pool "p": "a" listed first, "b" second,
// equal weight — WRR's own tie-break on a fresh state picks the earlier
// one first (wrr.go), so "a" is msg1's deterministic lead pick with no
// seed dependence. Each backend is scripted to name itself in its
// end-of-data reply, which is how this test tells "which backend
// answered" apart from "how many TCP connections it saw" — the health
// checker above is also dialing both servers throughout, so a raw
// connection/dial count is not a trustworthy signal here.
func drainConfig(addrA, addrB string) *config.Config {
	pool := config.Pool{Name: "p", Balance: "roundrobin", BackendTLS: "none", EhloName: "bifrost.test"}
	for _, sv := range []struct{ name, addr string }{{"a", addrA}, {"b", addrB}} {
		pool.Servers = append(pool.Servers, config.Server{
			Name: sv.name, Address: sv.addr, Weight: 1, Check: drainCheck(),
		})
	}
	return &config.Config{
		Defaults: config.Defaults{Timeouts: m1Timeouts()},
		Listener: config.Listener{Hostname: "bifrost.test", Capabilities: []string{"PIPELINING", "8BITMIME"}},
		Pools:    []config.Pool{pool},
		Routing:  config.Routing{DefaultPool: "p"},
	}
}

func namedBackend(t *testing.T, name string) *fakesmtp.Server {
	t.Helper()
	return fakesmtp.Start(t, fakesmtp.Script{
		Caps:  backendCaps(),
		OnEOD: []fakesmtp.Step{{Reply: "250 2.0.0 OK: queued via " + name}},
	})
}

// postAdmin POSTs body to path against h and fails the test if the
// response is not 200.
func postAdmin(t *testing.T, h http.Handler, path, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST %s %s: status = %d, body = %s", path, body, rr.Code, rr.Body.String())
	}
}

// probeTotal sums (pool, server)'s cumulative active-probe count across
// every level/result bucket.
func probeTotal(c *health.Checker, pool, server string) int64 {
	var total int64
	for _, n := range c.ProbeCounts(pool, server) {
		total += n
	}
	return total
}

// waitProbeCountAbove polls (bounded) until (pool, server)'s probe
// count exceeds floor — proof that probing kept running.
func waitProbeCountAbove(t *testing.T, c *health.Checker, pool, server string, floor int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for probeTotal(c, pool, server) <= floor {
		if time.Now().After(deadline) {
			t.Fatalf("ProbeCounts(%s,%s) never advanced past %d", pool, server, floor)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDrainMidClientConnection is the R3 drain-converges-in-one-
// transaction proof: a client holds one connection open across two
// messages; an admin drain issued between them changes which backend
// the very next message attaches to, with no reconnect and no delay
// beyond the next MAIL — and the drained server's health probes keep
// running the whole time (DRAIN is not MAINT), and un-draining it
// restores eligibility immediately.
func TestDrainMidClientConnection(t *testing.T) {
	a := namedBackend(t, "A")
	b := namedBackend(t, "B")
	cfg := drainConfig(a.Addr(), b.Addr())

	holder := &config.Holder{}
	holder.Swap(cfg)
	quiet := slog.New(slog.DiscardHandler)
	checker := health.New(holder, nil, quiet)
	router := balance.NewRouter(holder, checker.Eligible, rand.New(rand.NewSource(1)))
	m := metrics.New()
	adminH := admin.New("", holder, checker, router, m, nil).Handler()

	checkerCtx, checkerCancel := context.WithCancel(context.Background())
	checkerDone := make(chan struct{})
	go func() { checker.Run(checkerCtx); close(checkerDone) }()
	defer func() {
		checkerCancel()
		<-checkerDone
	}()

	relay := proxy.NewRelay(router.Pick, holder, quiet, checker, router.Lease, m)
	addr := serve(t, cfg, relay)

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	// msg1: "a" is the deterministic lead pick -- both servers are
	// eligible, nothing drained yet.
	reply1 := c.SendMsg(0)
	if want := "250 2.0.0 OK: queued via A"; len(reply1.Lines) != 1 || reply1.Lines[0] != want {
		t.Fatalf("msg1 verdict = %q, want [%q]", reply1.Lines, want)
	}

	// Admin drains "a", mid-connection -- no new MAIL sent yet.
	postAdmin(t, adminH, "/servers/p/a/state", `{"state":"drain"}`)
	if checker.Eligible("p", "a") {
		t.Fatalf("Eligible(a) after drain = true, want false")
	}

	// "a"'s probes keep running while drained (unlike MAINT, which
	// stops them — health.Checker's own admin.go/scheduler.go
	// contract): wait for at least one more completed probe.
	before := probeTotal(checker, "p", "a")
	waitProbeCountAbove(t, checker, "p", "a", before)

	// msg2, same connection, converges to "b" in this one transaction:
	// no reconnect, no extra round trip beyond the next MAIL.
	reply2 := c.SendMsg(1)
	if want := "250 2.0.0 OK: queued via B"; len(reply2.Lines) != 1 || reply2.Lines[0] != want {
		t.Fatalf("msg2 verdict = %q, want [%q]", reply2.Lines, want)
	}

	// Un-drain: "a" is eligible again immediately (its OpState never
	// left UP -- only AdminState changed).
	postAdmin(t, adminH, "/servers/p/a/state", `{"state":"ready"}`)
	if !checker.Eligible("p", "a") {
		t.Fatalf("Eligible(a) after un-drain = false, want true")
	}

	c.Send("QUIT")
	c.Expect("221")
}

//go:build integration

package health

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// TestMain wraps this package's whole integration-tagged test run in
// goleak, the same way internal/backend's own integration file does —
// every test in this package, not just TestPassiveIntegration below,
// must leave no goroutine behind once its cleanup has run.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestPassiveIntegration proves the passive path downs a server within
// a computable bound that has nothing to do with the (deliberately
// slow, here) active check cadence: a real Checker, a real scheduler
// goroutine running against a real fake backend, and errorLimit
// DialFailure signals — simulating what the relay would report while
// silently failing candidates over — take it DOWN synchronously, before
// the long active interval could ever have fired on its own.
func TestPassiveIntegration(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING", "8BITMIME"}})

	cfg := &config.Config{
		Pools: []config.Pool{{
			Name: "p",
			Servers: []config.Server{{
				Name:    "s1",
				Address: srv.Addr(),
				Check: config.CheckParams{
					Level:        "connect",
					Interval:     time.Hour, // must not fire during this test
					DownInterval: time.Hour,
					Timeout:      time.Second,
					Rise:         2,
					Fall:         1, // one synthetic failure is enough to down it
				},
			}},
		}},
	}
	target := &cfg.Pools[0].Servers[0]

	holder := &config.Holder{}
	holder.Swap(cfg)

	c := New(holder, nil, discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	if got := c.Status("p", "s1").Op; got != OpUp {
		t.Fatalf("Status before any signal: Op = %v, want UP (initState)", got)
	}

	for i := 0; i < errorLimit; i++ {
		c.DialFailure(target)
	}

	// Synchronous: recordPassive mutates op state directly, with no
	// timer or goroutine hand-off in between — there is no wall-clock
	// bound to wait out here.
	if got := c.Status("p", "s1").Op; got != OpDown {
		t.Fatalf("Status after %d DialFailure signals: Op = %v, want DOWN", errorLimit, got)
	}
	if got := srv.DialCount(); got != 0 {
		t.Errorf("fake DialCount = %d, want 0 (the DOWN verdict came from passive signals, not an active probe — Interval is 1h)", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

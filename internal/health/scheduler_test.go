package health

import (
	"context"
	"log/slog"
	"math/rand"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/littlebugger/bifrost/internal/config"
)

// pending reports how many timers are registered on fc and not yet
// fired/stopped.
func (f *fakeClock) pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// waitForTimers polls, bounded, until fc has at least n pending timers:
// the same bounded-poll pattern test/integration/m1_test.go's
// settleGoroutines uses for goroutine counts, applied here to
// synchronize a test with goroutines that are about to register a
// timer, without a real sleep standing in for the thing under test.
func waitForTimers(t *testing.T, fc *fakeClock, n int) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if fc.pending() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waitForTimers: %d pending timers never arrived (have %d)", n, fc.pending())
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// schedServer builds a config.Server with a hand-set Check, bypassing
// the HCL loader entirely — this package's unit tests never need it.
func schedServer(name string, rise, fall int, interval time.Duration) config.Server {
	return config.Server{
		Name:    name,
		Address: "127.0.0.1:0",
		Check: config.CheckParams{
			Level:        "connect",
			Interval:     interval,
			DownInterval: interval,
			Timeout:      interval,
			Rise:         rise,
			Fall:         fall,
		},
	}
}

func schedConfig(pool string, servers ...config.Server) *config.Config {
	return &config.Config{
		Pools: []config.Pool{{Name: pool, Servers: servers}},
	}
}

// TestJitterBounds: 10k draws all land within ±5% of base.
func TestJitterBounds(t *testing.T) {
	rnd := rand.New(rand.NewSource(42))
	const base = 5 * time.Second
	lo := time.Duration(float64(base) * 0.95)
	hi := time.Duration(float64(base) * 1.05)

	for i := 0; i < 10000; i++ {
		got := jitter(base, rnd.Float64())
		if got < lo || got > hi {
			t.Fatalf("draw %d: jitter() = %v, want within [%v, %v]", i, got, lo, hi)
		}
	}
}

// TestInitialSpread: N draws land within [0, min(interval,5s)) and are
// not all identical (not synchronized); a large interval clamps the
// window to 5s.
func TestInitialSpread(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	const interval = 3 * time.Second // under the 5s cap: window == interval
	const n = 20

	offsets := make([]time.Duration, n)
	for i := range offsets {
		offsets[i] = initialOffset(rnd.Float64(), interval)
		if offsets[i] < 0 || offsets[i] >= interval {
			t.Fatalf("draw %d: initialOffset() = %v, want within [0, %v)", i, offsets[i], interval)
		}
	}
	allSame := true
	for _, o := range offsets[1:] {
		if o != offsets[0] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Fatalf("initialOffset() produced the same offset for all %d draws, want spread", n)
	}

	if big := initialOffset(0.999999, 10*time.Second); big >= 5*time.Second {
		t.Errorf("initialOffset(interval=10s) = %v, want < 5s (window capped)", big)
	}
}

// TestGenerationTokenDiscardsStaleResult: a probe dispatched for a
// server that a reload then removes must be discarded on completion —
// no state mutated, no panic — and the server's goroutine retires
// instead of scheduling another round.
func TestGenerationTokenDiscardsStaleResult(t *testing.T) {
	fc := newFakeClock()
	holder := &config.Holder{}
	const interval = 100 * time.Millisecond
	holder.Swap(schedConfig("p", schedServer("s1", 2, 3, interval)))

	started := make(chan struct{})
	proceed := make(chan probeResult)
	stub := func(ctx context.Context, _ *config.Server, _ config.CheckParams, _ []string) probeResult {
		close(started)
		select {
		case r := <-proceed:
			return r
		case <-ctx.Done():
			return probeResult{}
		}
	}

	c := newChecker(holder, fc, discardLog(), stub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() { c.Run(ctx); close(runDone) }()

	// One timer for s1's initial spread, one for Run's own reload-poll.
	waitForTimers(t, fc, 2)
	fc.Advance(interval) // >= any possible initial-spread draw for this interval

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("stub probe never dispatched")
	}

	// Remove s1 from the config the checker will pick up on its next
	// reload-detection tick.
	holder.Swap(schedConfig("p"))
	waitForTimers(t, fc, 1)
	fc.Advance(fastinter)

	// Give Run's goroutine a moment to actually process the reload
	// (synchronous inside Run, but this goroutine and the test are
	// still two different goroutines). No channel here signals "reload
	// applied" without a production change, so this stays a poll — on
	// c.lookup, not Status: s1's entry hasn't completed its first probe
	// yet (it's the one blocked in the stub below), so its Status is
	// ALL ZERO VALUES already — Op:UP, Admin:READY, Override:AUTO,
	// everything false/0 — identical to Status{}'s "not found" sentinel.
	// Polling Status()!=Status{} would therefore exit on the very first
	// check, before the reload has actually run, and race the rest of
	// the test unpredictably (worse under load, exactly the reported
	// flake). lookup reports registration directly, with no such
	// ambiguity, and a generous deadline: it only ever waits this long
	// when the test is about to fail anyway.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := c.lookup("p", "s1"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reload never dropped s1 from the registry")
		}
		time.Sleep(time.Millisecond)
	}

	// Now let the stale probe finish. If the generation check didn't
	// work, this would resurrect s1's entry or panic on a nil map value.
	proceed <- probeResult{ok: true}

	if _, err := c.lookup("p", "s1"); err == nil {
		t.Errorf("Status(p,s1) resolvable after a stale result was applied, want still removed")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestSchedulerStopsOnCtxCancel: every per-server goroutine Run started
// exits once ctx is cancelled, and Run itself returns — goleak-clean.
func TestSchedulerStopsOnCtxCancel(t *testing.T) {
	fc := newFakeClock()
	holder := &config.Holder{}
	const interval = 50 * time.Millisecond
	holder.Swap(schedConfig("p",
		schedServer("s1", 2, 3, interval),
		schedServer("s2", 2, 3, interval),
	))

	probed := make(chan string, 16)
	stub := func(_ context.Context, srv *config.Server, _ config.CheckParams, _ []string) probeResult {
		probed <- srv.Name
		return probeResult{ok: true}
	}

	c := newChecker(holder, fc, discardLog(), stub)
	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan struct{})
	go func() { c.Run(ctx); close(runDone) }()

	// 2 servers' initial-spread timers + Run's poll ticker.
	waitForTimers(t, fc, 3)
	fc.Advance(interval)

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case name := <-probed:
			seen[name] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only saw probes from %v, want both s1 and s2", seen)
		}
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if err := goleak.Find(); err != nil {
		t.Errorf("goroutine leak after Run returned: %v", err)
	}
}

// TestIncompatibleDrivesEligibleFalse: applyResult wires a probe's
// incompatible verdict onto the registered entry's health end to end —
// a probe that is op-wise a success but capability-incompatible must
// still gate Eligible false, while leaving OpState alone.
func TestIncompatibleDrivesEligibleFalse(t *testing.T) {
	fc := newFakeClock()
	holder := &config.Holder{}
	const interval = 30 * time.Millisecond
	holder.Swap(schedConfig("p", schedServer("s1", 2, 3, interval)))

	stub := func(context.Context, *config.Server, config.CheckParams, []string) probeResult {
		return probeResult{ok: true, incompatible: true}
	}
	c := newChecker(holder, fc, discardLog(), stub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { c.Run(ctx); close(runDone) }()

	waitForTimers(t, fc, 2)
	fc.Advance(interval)

	deadline := time.Now().Add(2 * time.Second)
	for !c.Status("p", "s1").Incompatible {
		if time.Now().After(deadline) {
			t.Fatal("Incompatible never became true after an incompatible probe result")
		}
		time.Sleep(time.Millisecond)
	}

	if got := c.Status("p", "s1").Op; got != OpUp {
		t.Errorf("Status.Op = %v, want UP (incompatible is orthogonal to op state)", got)
	}
	if c.Eligible("p", "s1") {
		t.Errorf("Eligible = true, want false (Incompatible gates eligibility despite the op-wise success)")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

package health

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/config"
)

// waitCallCount polls (bounded) until *n reaches at least want.
func waitCallCount(t *testing.T, n *int32, want int32) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if atomic.LoadInt32(n) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waitCallCount: only reached %d, want >= %d", atomic.LoadInt32(n), want)
}

// advanceUntil repeatedly advances fc by step, yielding real time
// between steps, until cond reports true (bounded). A single
// waitForTimers-then-Advance can race a goroutine that hasn't yet
// registered its NEXT timer (e.g. Run's always-pending poll ticker
// satisfies "N pending timers" without the per-server loop's own timer
// having re-armed yet) — repeatedly nudging the clock forward and
// yielding sidesteps that ambiguity instead of trying to count exactly
// right.
func advanceUntil(t *testing.T, fc *fakeClock, step time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		fc.Advance(step)
		time.Sleep(time.Millisecond)
		if time.Now().After(deadline) {
			t.Fatalf("advanceUntil: condition never became true")
		}
	}
}

// waitOp polls (bounded) until (pool,server)'s Status.Op reaches want.
func waitOp(t *testing.T, c *Checker, pool, server string, want OpState) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if c.Status(pool, server).Op == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waitOp: Status(%s,%s).Op never reached %v (have %v)", pool, server, want, c.Status(pool, server).Op)
}

// startScheduled builds a Checker over one server, running against a
// fake clock and a counting stub prober, and returns it started
// (Run in its own goroutine) plus a cancel func to stop it.
func startScheduled(t *testing.T, fc *fakeClock, cfg *config.Config, result probeResult, calls *int32) (*Checker, func()) {
	t.Helper()
	holder := &config.Holder{}
	holder.Swap(cfg)

	stub := func(_ context.Context, _ *config.Server, _ config.CheckParams, _ []string) probeResult {
		atomic.AddInt32(calls, 1)
		return result
	}
	c := newChecker(holder, fc, discardLog(), stub)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after ctx cancel")
		}
	}
	return c, stop
}

// TestDrainExcludesFromEligibleProbesContinue: DRAIN makes Eligible
// false, but the scheduler keeps probing normally — unlike MAINT.
func TestDrainExcludesFromEligibleProbesContinue(t *testing.T) {
	fc := newFakeClock()
	const interval = 30 * time.Millisecond
	var calls int32
	c, stop := startScheduled(t, fc, schedConfig("p", schedServer("s1", 2, 1, interval)), probeResult{ok: true}, &calls)
	defer stop()

	waitForTimers(t, fc, 2)
	fc.Advance(interval)
	waitCallCount(t, &calls, 1)

	if !c.Eligible("p", "s1") {
		t.Fatalf("Eligible before drain = false, want true")
	}
	if err := c.SetAdminState("p", "s1", AdminDrain); err != nil {
		t.Fatalf("SetAdminState: %v", err)
	}
	if c.Eligible("p", "s1") {
		t.Errorf("Eligible while DRAIN = true, want false")
	}

	before := atomic.LoadInt32(&calls)
	advanceUntil(t, fc, interval, func() bool { return atomic.LoadInt32(&calls) > before }) // proves probing continued despite DRAIN
}

// TestMaintStopsProbes: MAINT cancels a probe already in flight ("idle
// probe conns closed") and stops the scheduler from dispatching any
// more until it leaves MAINT.
func TestMaintStopsProbes(t *testing.T) {
	fc := newFakeClock()
	holder := &config.Holder{}
	const interval = 50 * time.Millisecond
	holder.Swap(schedConfig("p", schedServer("s1", 2, 3, interval)))

	var calls int32
	cancelled := make(chan struct{}, 1)
	stub := func(ctx context.Context, _ *config.Server, _ config.CheckParams, _ []string) probeResult {
		atomic.AddInt32(&calls, 1)
		<-ctx.Done()
		select {
		case cancelled <- struct{}{}:
		default:
		}
		return probeResult{ok: true}
	}
	c := newChecker(holder, fc, discardLog(), stub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	waitForTimers(t, fc, 2)
	fc.Advance(interval) // dispatches the probe; the stub blocks on ctx.Done()
	waitCallCount(t, &calls, 1)

	if err := c.SetAdminState("p", "s1", AdminMaint); err != nil {
		t.Fatalf("SetAdminState: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight probe was never cancelled by SetAdminState(MAINT)")
	}

	before := atomic.LoadInt32(&calls)
	for i := 0; i < 20; i++ {
		fc.Advance(fastinter)
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != before {
		t.Errorf("probe calls after MAINT = %d, want still %d (scheduler paused)", got, before)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestForceDownBeatsProbeSuccess: FORCE_DOWN freezes Eligible at false
// even while every probe keeps succeeding.
func TestForceDownBeatsProbeSuccess(t *testing.T) {
	fc := newFakeClock()
	const interval = 30 * time.Millisecond
	var calls int32
	c, stop := startScheduled(t, fc, schedConfig("p", schedServer("s1", 2, 3, interval)), probeResult{ok: true}, &calls)
	defer stop()

	waitForTimers(t, fc, 2)
	fc.Advance(interval)
	waitOp(t, c, "p", "s1", OpUp)

	if err := c.SetOverride("p", "s1", OverrideForceDown); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if c.Eligible("p", "s1") {
		t.Fatalf("Eligible with FORCE_DOWN = true, want false")
	}

	before := atomic.LoadInt32(&calls)
	advanceUntil(t, fc, interval, func() bool { return atomic.LoadInt32(&calls) > before })
	if got := c.Status("p", "s1").Op; got != OpUp {
		t.Errorf("Status.Op = %v, want UP (probes kept recording normally)", got)
	}
	if c.Eligible("p", "s1") {
		t.Errorf("Eligible with FORCE_DOWN = true after more successes, want still false (verdict frozen)")
	}
}

// TestForceUpBeatsProbeFailure: FORCE_UP keeps Eligible true even while
// every probe keeps failing.
func TestForceUpBeatsProbeFailure(t *testing.T) {
	fc := newFakeClock()
	const interval = 30 * time.Millisecond
	var calls int32
	c, stop := startScheduled(t, fc, schedConfig("p", schedServer("s1", 2, 1, interval)), probeResult{ok: false}, &calls)
	defer stop()

	waitForTimers(t, fc, 2)
	fc.Advance(interval)
	waitOp(t, c, "p", "s1", OpDown)

	if err := c.SetOverride("p", "s1", OverrideForceUp); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if !c.Eligible("p", "s1") {
		t.Fatalf("Eligible with FORCE_UP = false, want true despite probes failing")
	}

	before := atomic.LoadInt32(&calls)
	advanceUntil(t, fc, interval, func() bool { return atomic.LoadInt32(&calls) > before })
	if got := c.Status("p", "s1").Op; got != OpDown {
		t.Errorf("Status.Op = %v, want DOWN (probes kept recording normally)", got)
	}
	if !c.Eligible("p", "s1") {
		t.Errorf("Eligible with FORCE_UP = false after more failures, want still true (verdict frozen)")
	}
}

// TestAutoResumes: switching back to AUTO makes Eligible track OpState
// again, instead of whatever the override last forced.
func TestAutoResumes(t *testing.T) {
	fc := newFakeClock()
	const interval = 30 * time.Millisecond
	var calls int32
	c, stop := startScheduled(t, fc, schedConfig("p", schedServer("s1", 2, 1, interval)), probeResult{ok: false}, &calls)
	defer stop()

	waitForTimers(t, fc, 2)
	fc.Advance(interval)
	waitOp(t, c, "p", "s1", OpDown)

	if err := c.SetOverride("p", "s1", OverrideForceUp); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if !c.Eligible("p", "s1") {
		t.Fatalf("Eligible with FORCE_UP = false, want true")
	}

	if err := c.SetOverride("p", "s1", OverrideAuto); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if c.Eligible("p", "s1") {
		t.Errorf("Eligible after returning to AUTO = true, want false (OpState is still DOWN)")
	}
}

// TestEligibleUnknownServerFalse: Eligible must fail closed for a
// (pool, server) that was never registered, or no longer is — Status's
// zero value ({OpUp, AdminReady, OverrideAuto}) would otherwise read as
// eligible if Eligible went through Status the way it used to.
func TestEligibleUnknownServerFalse(t *testing.T) {
	holder := &config.Holder{}
	holder.Swap(schedConfig("p", schedServer("s1", 2, 3, time.Second)))
	c := newChecker(holder, newFakeClock(), discardLog(), func(context.Context, *config.Server, config.CheckParams, []string) probeResult {
		return probeResult{ok: true}
	})

	if c.Eligible("nope", "nope") {
		t.Errorf("Eligible(unknown pool, unknown server) = true, want false")
	}
	if c.Eligible("p", "nope") {
		t.Errorf("Eligible(known pool, unknown server) = true, want false")
	}

	c.sync(schedConfig("p")) // reload removes s1
	if c.Eligible("p", "s1") {
		t.Errorf("Eligible(just-removed server) = true, want false")
	}
}

// TestAdminStateSurvivesReload: a server whose (pool, server) identity
// persists across a reload keeps its admin state; a removed server's
// state is dropped outright (D15's reload-survival matrix).
func TestAdminStateSurvivesReload(t *testing.T) {
	holder := &config.Holder{}
	cfg1 := schedConfig("p", schedServer("s1", 2, 3, time.Second), schedServer("s2", 2, 3, time.Second))
	holder.Swap(cfg1)
	c := newChecker(holder, newFakeClock(), discardLog(), func(context.Context, *config.Server, config.CheckParams, []string) probeResult {
		return probeResult{ok: true}
	})

	if err := c.SetAdminState("p", "s1", AdminDrain); err != nil {
		t.Fatalf("SetAdminState: %v", err)
	}

	cfg2 := schedConfig("p", schedServer("s1", 2, 3, time.Second)) // s2 removed
	c.sync(cfg2)

	if got := c.Status("p", "s1").Admin; got != AdminDrain {
		t.Errorf("s1 Admin after reload = %v, want DRAIN (survived)", got)
	}
	if got := c.Status("p", "s2"); got != (Status{}) {
		t.Errorf("s2 Status after removal = %+v, want zero Status (dropped)", got)
	}
}

// TestOverrideSurvivesReload is TestAdminStateSurvivesReload's
// counterpart for force-up/force-down.
func TestOverrideSurvivesReload(t *testing.T) {
	holder := &config.Holder{}
	cfg1 := schedConfig("p", schedServer("s1", 2, 3, time.Second), schedServer("s2", 2, 3, time.Second))
	holder.Swap(cfg1)
	c := newChecker(holder, newFakeClock(), discardLog(), func(context.Context, *config.Server, config.CheckParams, []string) probeResult {
		return probeResult{ok: true}
	})

	if err := c.SetOverride("p", "s1", OverrideForceUp); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if err := c.SetOverride("p", "s2", OverrideForceDown); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}

	cfg2 := schedConfig("p", schedServer("s1", 2, 3, time.Second)) // s2 removed
	c.sync(cfg2)

	if got := c.Status("p", "s1").Override; got != OverrideForceUp {
		t.Errorf("s1 Override after reload = %v, want FORCE_UP (survived)", got)
	}
	if got := c.Status("p", "s2"); got != (Status{}) {
		t.Errorf("s2 Status after removal = %+v, want zero Status (dropped)", got)
	}
}

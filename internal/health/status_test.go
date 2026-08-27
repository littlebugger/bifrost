package health

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
)

// varResultProber is a probeFunc whose canned result can change between
// calls, unlike admin_test.go's startScheduled (one fixed result for the
// whole test) — needed to observe an actual UP->DOWN transition and the
// LastChange/StateChanges bookkeeping epic-09 adds around it.
type varResultProber struct {
	mu     sync.Mutex
	result probeResult
	calls  int32
}

func (p *varResultProber) set(r probeResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.result = r
}

func (p *varResultProber) probe(context.Context, *config.Server, config.CheckParams, []string) probeResult {
	atomic.AddInt32(&p.calls, 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result
}

// runScheduled starts c.Run in its own goroutine and returns a stop func
// mirroring admin_test.go's startScheduled, minus the fixed-result stub
// (varResultProber supplies that instead).
func runScheduled(t *testing.T, c *Checker) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after ctx cancel")
		}
	}
}

// TestStatusLastChangeAndStateChanges: the first-ever active probe
// stamps LastChange even though Op does not visibly flip (it starts,
// and stays, UP) — "since when has this been UP" must be answerable
// from the first check onward. A later UP->DOWN transition stamps it
// again and bumps StateChanges; a same-outcome repeat probe does
// neither.
func TestStatusLastChangeAndStateChanges(t *testing.T) {
	fc := newFakeClock()
	const interval = 30 * time.Millisecond
	prober := &varResultProber{result: probeResult{ok: true}}

	holder := &config.Holder{}
	holder.Swap(schedConfig("p", schedServer("s1", 1, 1, interval)))
	c := newChecker(holder, fc, discardLog(), prober.probe)
	defer runScheduled(t, c)()

	waitForTimers(t, fc, 2)
	before := atomic.LoadInt32(&prober.calls)
	advanceUntil(t, fc, interval, func() bool { return atomic.LoadInt32(&prober.calls) > before }) // probe #1: ok=true

	st := c.Status("p", "s1")
	if st.LastChange.IsZero() {
		t.Fatalf("LastChange still zero after the first probe")
	}
	if st.StateChanges != 0 {
		t.Fatalf("StateChanges = %d after the first (non-flipping) probe, want 0", st.StateChanges)
	}
	firstChange := st.LastChange

	before = atomic.LoadInt32(&prober.calls)
	advanceUntil(t, fc, interval, func() bool { return atomic.LoadInt32(&prober.calls) > before }) // probe #2: still ok=true, no flip
	st = c.Status("p", "s1")
	if !st.LastChange.Equal(firstChange) {
		t.Fatalf("LastChange moved on a same-outcome probe: %v -> %v", firstChange, st.LastChange)
	}

	prober.set(probeResult{reason: "connect-refused"})
	advanceUntil(t, fc, interval, func() bool { return c.Status("p", "s1").Op == OpDown }) // probe #3: ok=false, fall=1 -> flips DOWN

	st = c.Status("p", "s1")
	if st.StateChanges != 1 {
		t.Fatalf("StateChanges = %d after one flip, want 1", st.StateChanges)
	}
	if !st.LastChange.After(firstChange) {
		t.Fatalf("LastChange did not advance on the flip: %v, want after %v", st.LastChange, firstChange)
	}
}

// TestStatusLastProbeDetail: LastProbe and ProbeCounts reflect the most
// recent/cumulative active probe outcomes (level, result, detail); a
// passive signal (TransportError) never touches LastProbe — it is an
// active-only record.
func TestStatusLastProbeDetail(t *testing.T) {
	fc := newFakeClock()
	const interval = 30 * time.Millisecond
	prober := &varResultProber{result: probeResult{reason: "wrong-banner"}}

	cfg := schedConfig("p", schedServer("s1", 1, 1, interval))
	cfg.Pools[0].Servers[0].Check.Level = "banner"
	holder := &config.Holder{}
	holder.Swap(cfg)
	c := newChecker(holder, fc, discardLog(), prober.probe)
	defer runScheduled(t, c)()

	waitForTimers(t, fc, 2)
	fc.Advance(interval)
	waitCallCount(t, &prober.calls, 1)
	waitOp(t, c, "p", "s1", OpDown) // fall=1: the one failing probe already flipped it

	st := c.Status("p", "s1")
	want := ProbeInfo{Level: "banner", Result: "fail", Detail: "wrong-banner"}
	if st.LastProbe.Level != want.Level || st.LastProbe.Result != want.Result || st.LastProbe.Detail != want.Detail {
		t.Fatalf("LastProbe = %+v, want %+v (latency ignored)", st.LastProbe, want)
	}
	if st.LastProbe.Latency < 0 {
		t.Fatalf("LastProbe.Latency = %v, want >= 0", st.LastProbe.Latency)
	}

	counts := c.ProbeCounts("p", "s1")
	if counts["banner|fail"] != 1 {
		t.Fatalf("ProbeCounts = %v, want banner|fail=1", counts)
	}

	c.TransportError(&cfg.Pools[0].Servers[0])
	st2 := c.Status("p", "s1")
	if st2.LastProbe != st.LastProbe {
		t.Fatalf("a passive signal changed LastProbe: %+v -> %+v", st.LastProbe, st2.LastProbe)
	}
}

// TestProbeCountsUnregisteredIsNil: ProbeCounts fails closed like every
// other per-server accessor in this package.
func TestProbeCountsUnregisteredIsNil(t *testing.T) {
	holder := &config.Holder{}
	holder.Swap(schedConfig("p"))
	c := newChecker(holder, newFakeClock(), discardLog(), func(context.Context, *config.Server, config.CheckParams, []string) probeResult {
		return probeResult{ok: true}
	})
	if got := c.ProbeCounts("p", "nope"); got != nil {
		t.Fatalf("ProbeCounts(unregistered) = %v, want nil", got)
	}
}

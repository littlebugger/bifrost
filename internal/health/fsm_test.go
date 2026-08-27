package health

import (
	"sync"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/config"
)

// fakeClock is the Clock test double every fake-clock unit test in this
// package (fsm/scheduler/passive/admin) uses. Defined once here since
// Task 1 introduces Clock; later tasks' test files in this same package
// reuse it unmodified.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

type fakeWaiter struct {
	deadline time.Time
	c        chan time.Time
	stopped  bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{deadline: f.now.Add(d), c: make(chan time.Time, 1)}
	f.waiters = append(f.waiters, w)
	return &fakeTimer{fc: f, w: w}
}

// Advance moves the fake clock forward by d and fires (buffered,
// non-blocking send) every pending timer whose deadline has now passed.
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	var fire []*fakeWaiter
	remaining := f.waiters[:0]
	for _, w := range f.waiters {
		if !w.stopped && !w.deadline.After(now) {
			fire = append(fire, w)
		} else {
			remaining = append(remaining, w)
		}
	}
	f.waiters = remaining
	f.mu.Unlock()
	for _, w := range fire {
		w.c <- now
	}
}

type fakeTimer struct {
	fc *fakeClock
	w  *fakeWaiter
}

func (t *fakeTimer) C() <-chan time.Time { return t.w.c }

func (t *fakeTimer) Stop() bool {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	if t.w.stopped {
		return false
	}
	t.w.stopped = true
	for i, w := range t.fc.waiters {
		if w == t.w {
			t.fc.waiters = append(t.fc.waiters[:i], t.fc.waiters[i+1:]...)
			break
		}
	}
	return true
}

func TestClockRealAndFake(t *testing.T) {
	rc := NewClock()
	tm := rc.NewTimer(time.Millisecond)
	select {
	case <-tm.C():
	case <-time.After(time.Second):
		t.Fatal("real clock timer never fired")
	}

	fc := newFakeClock()
	ft := fc.NewTimer(10 * time.Second)
	select {
	case <-ft.C():
		t.Fatal("fake timer fired before Advance")
	default:
	}
	fc.Advance(10 * time.Second)
	select {
	case <-ft.C():
	default:
		t.Fatal("fake timer did not fire after Advance")
	}
}

// TestFallMath: fall-1 consecutive failures stay UP; the fall-th takes
// it DOWN; a success interleaved among failures resets the streak
// (proving failures don't accumulate across it).
func TestFallMath(t *testing.T) {
	f := newFSM(2, 3) // rise=2, fall=3

	for i := 0; i < 2; i++ {
		f.recordActive(false)
		if f.op != OpUp {
			t.Fatalf("after %d failures: op = %v, want UP (fall=3)", i+1, f.op)
		}
	}
	if f.consecFail != 2 {
		t.Fatalf("consecFail = %d, want 2", f.consecFail)
	}

	// Interleaved success resets the streak.
	f.recordActive(true)
	if f.consecFail != 0 {
		t.Fatalf("consecFail after success = %d, want 0 (reset)", f.consecFail)
	}
	if f.op != OpUp {
		t.Fatalf("op after interleaved success = %v, want UP", f.op)
	}

	// Two more failures: only 2 in the current streak, still UP.
	f.recordActive(false)
	f.recordActive(false)
	if f.op != OpUp {
		t.Fatalf("op = %v, want still UP (streak reset by the interleaved success)", f.op)
	}

	// The 3rd consecutive failure of THIS streak takes it down.
	f.recordActive(false)
	if f.op != OpDown {
		t.Fatalf("op after fall-th consecutive failure = %v, want DOWN", f.op)
	}
}

// TestRiseMath is TestFallMath's symmetric counterpart for recovery.
func TestRiseMath(t *testing.T) {
	f := newFSM(2, 3)
	for i := 0; i < 3; i++ {
		f.recordActive(false)
	}
	if f.op != OpDown {
		t.Fatalf("setup: op = %v, want DOWN", f.op)
	}

	// rise-1 successes: still DOWN.
	f.recordActive(true)
	if f.op != OpDown {
		t.Fatalf("after 1 success (rise=2): op = %v, want still DOWN", f.op)
	}
	if f.consecOK != 1 {
		t.Fatalf("consecOK = %d, want 1", f.consecOK)
	}

	// Interleaved failure resets the rise streak.
	f.recordActive(false)
	if f.consecOK != 0 {
		t.Fatalf("consecOK after interleaved failure = %d, want 0 (reset)", f.consecOK)
	}
	if f.op != OpDown {
		t.Fatalf("op = %v, want still DOWN", f.op)
	}

	// rise consecutive successes of the new streak bring it back up.
	f.recordActive(true)
	if f.op != OpDown {
		t.Fatalf("after 1 success of new streak: op = %v, want still DOWN", f.op)
	}
	f.recordActive(true)
	if f.op != OpUp {
		t.Fatalf("after rise-th consecutive success: op = %v, want UP", f.op)
	}
}

// TestIntervalTable is the interval table's five buckets, exhaustively:
// unchecked, steady-UP, transitional-down, steady-DOWN, transitional-up.
func TestIntervalTable(t *testing.T) {
	params := config.CheckParams{Interval: 5 * time.Second, DownInterval: 15 * time.Second}

	t.Run("unchecked", func(t *testing.T) {
		f := newFSM(2, 3)
		if got := f.nextInterval(params); got != fastinter {
			t.Errorf("nextInterval() = %v, want fastinter (%v)", got, fastinter)
		}
	})

	t.Run("steady-UP", func(t *testing.T) {
		f := newFSM(2, 3)
		f.recordActive(true)
		if got := f.nextInterval(params); got != params.Interval {
			t.Errorf("nextInterval() = %v, want Interval (%v)", got, params.Interval)
		}
	})

	t.Run("transitional-down", func(t *testing.T) {
		f := newFSM(2, 3) // fall=3
		f.recordActive(true)
		f.recordActive(false) // 1 of 3 failures: still UP, but mid-streak
		if f.op != OpUp {
			t.Fatalf("setup: op = %v, want UP", f.op)
		}
		if got := f.nextInterval(params); got != fastinter {
			t.Errorf("nextInterval() = %v, want fastinter (%v)", got, fastinter)
		}
	})

	t.Run("steady-DOWN", func(t *testing.T) {
		f := newFSM(2, 1) // fall=1: one failure downs it
		f.recordActive(false)
		if f.op != OpDown {
			t.Fatalf("setup: op = %v, want DOWN", f.op)
		}
		if got := f.nextInterval(params); got != params.DownInterval {
			t.Errorf("nextInterval() = %v, want DownInterval (%v)", got, params.DownInterval)
		}
	})

	t.Run("transitional-up", func(t *testing.T) {
		f := newFSM(2, 1) // rise=2, fall=1
		f.recordActive(false)
		f.recordActive(true) // 1 of 2 successes: still DOWN, but mid-streak
		if f.op != OpDown {
			t.Fatalf("setup: op = %v, want DOWN", f.op)
		}
		if got := f.nextInterval(params); got != fastinter {
			t.Errorf("nextInterval() = %v, want fastinter (%v)", got, fastinter)
		}
	})
}

// TestInitStateUp: a server starts selectable (UP, though unchecked);
// the first failed probe downs it — there is no fully_down mode in v1.
func TestInitStateUp(t *testing.T) {
	f := newFSM(2, 3)
	if f.op != OpUp {
		t.Fatalf("newFSM: op = %v, want UP", f.op)
	}
	if f.checked {
		t.Fatalf("newFSM: checked = true, want false (no active probe has run yet)")
	}

	f2 := newFSM(2, 1) // fall=1: exercises "the first failed probe downs it"
	f2.recordActive(false)
	if f2.op != OpDown {
		t.Fatalf("after first failed probe (fall=1): op = %v, want DOWN", f2.op)
	}
}

// TestCountersNeverCorruptedByAdminChanges: admin/override are stored
// alongside the FSM in serverHealth but are a separate axis entirely —
// flipping them must never read or write fsm's counters.
func TestCountersNeverCorruptedByAdminChanges(t *testing.T) {
	h := newServerHealth(2, 3)
	h.fsm.recordActive(false)
	h.fsm.recordActive(false)
	wantFail, wantOK, wantOp := h.fsm.consecFail, h.fsm.consecOK, h.fsm.op

	h.admin = AdminDrain
	h.override = OverrideForceDown
	h.admin = AdminMaint
	h.override = OverrideForceUp
	h.admin = AdminReady
	h.override = OverrideAuto

	if h.fsm.consecFail != wantFail || h.fsm.consecOK != wantOK || h.fsm.op != wantOp {
		t.Fatalf("fsm state changed by admin/override churn: got (fail=%d,ok=%d,op=%v), want (fail=%d,ok=%d,op=%v)",
			h.fsm.consecFail, h.fsm.consecOK, h.fsm.op, wantFail, wantOK, wantOp)
	}
}

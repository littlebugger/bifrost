package health

import "time"

// Clock abstracts wall-clock time behind an interface the scheduler
// consults for "now" and for arming timers, so unit tests drive the FSM
// and scheduler deterministically (fake clock, no sleeps) while
// production wiring uses NewClock's real one. See clock_test.go's
// fakeClock for the test double — it lives in a _test.go file because
// nothing outside this package's own tests needs it.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer abstracts a single-fire timer. C is a method (not a bare
// channel field) so a fake implementation can hand back a distinct
// channel per timer without embedding one.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// NewClock returns the real, wall-clock Clock production code uses.
func NewClock() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer {
	return &realTimer{t: time.NewTimer(d)}
}

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time { return r.t.C }
func (r *realTimer) Stop() bool          { return r.t.Stop() }

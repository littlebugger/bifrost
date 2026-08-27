package health

import (
	"testing"

	"github.com/littlebugger/bifrost/internal/config"
)

// TestPassiveEventsCount: DialFailure/TransportError both grow the same
// consecutive-failure streak; Success resets it.
func TestPassiveEventsCount(t *testing.T) {
	f := newFSM(2, 3)

	f.recordPassive(true) // DialFailure
	f.recordPassive(true) // TransportError
	if f.passiveFails != 2 {
		t.Fatalf("passiveFails = %d, want 2", f.passiveFails)
	}

	f.recordPassive(false) // Success
	if f.passiveFails != 0 {
		t.Fatalf("passiveFails after Success = %d, want 0 (reset)", f.passiveFails)
	}
}

// TestPassiveNeverCounts4xx5xxVerdicts documents the passive surface's
// shape: DialFailure/TransportError/Success (proxy.HealthSignals) are
// the only three passive inputs that exist, and none of them takes a
// reply code — a MAIL/RCPT/DATA verdict has no path into passive health
// at all. If that surface ever grows a fourth, code-shaped method, this
// test still compiles and still passes, but the comment above stops
// being true — the enforcement here is the API's shape, not a runtime
// check.
func TestPassiveNeverCounts4xx5xxVerdicts(t *testing.T) {
	c := newChecker(&config.Holder{}, newFakeClock(), discardLog(), runProbe)
	if c == nil {
		t.Fatal("newChecker returned nil")
	}
	srv := &config.Server{Name: "s1"}
	c.DialFailure(srv)
	c.TransportError(srv)
	c.Success(srv)
}

// TestErrorLimitFiresFailCheck: errorLimit consecutive passive failures
// synthesize exactly one active-equivalent failure (consecFail goes from
// 0 to 1, not further), which also engages fastinter scheduling; one
// more passive failure right after does not fire again immediately (the
// streak reset when it fired).
func TestErrorLimitFiresFailCheck(t *testing.T) {
	f := newFSM(2, 5) // fall=5: the synthetic failure alone must not down it

	for i := 0; i < errorLimit-1; i++ {
		f.recordPassive(true)
	}
	if f.consecFail != 0 {
		t.Fatalf("consecFail = %d after %d passive fails, want 0 (errorLimit=%d not yet reached)", f.consecFail, errorLimit-1, errorLimit)
	}

	f.recordPassive(true) // the errorLimit-th
	if f.consecFail != 1 {
		t.Fatalf("consecFail = %d after errorLimit consecutive passive fails, want 1 (exactly one synthetic failure)", f.consecFail)
	}
	if got := f.nextInterval(config.CheckParams{Interval: 5_000_000_000}); got != fastinter {
		t.Errorf("nextInterval() = %v, want fastinter (transitional after the synthetic failure)", got)
	}

	f.recordPassive(true) // one more: must not fire again immediately
	if f.consecFail != 1 {
		t.Fatalf("consecFail = %d after one more passive fail, want still 1 (streak reset when it fired)", f.consecFail)
	}
}

// TestPassiveCannotRecover is the anti-flap invariant: a DOWN server
// stays DOWN no matter how many passive Success signals arrive; only
// rise consecutive ACTIVE successes bring it back up.
func TestPassiveCannotRecover(t *testing.T) {
	f := newFSM(2, 1) // fall=1: one active failure downs it
	f.recordActive(false)
	if f.op != OpDown {
		t.Fatalf("setup: op = %v, want DOWN", f.op)
	}

	for i := 0; i < 100; i++ {
		f.recordPassive(false) // Success
	}
	if f.op != OpDown {
		t.Fatalf("op after 100 passive successes = %v, want still DOWN (recovery is active-only)", f.op)
	}

	for i := 0; i < f.rise; i++ {
		f.recordActive(true)
	}
	if f.op != OpUp {
		t.Fatalf("op after rise (%d) active successes = %v, want UP", f.rise, f.op)
	}
}

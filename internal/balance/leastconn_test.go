package balance

import (
	"sync"
	"testing"

	"github.com/revolee/bifrost/internal/config"
)

// TestLeaseRelease proves Lease's in-flight counter is correct under 100
// concurrent goroutines, in both directions (acquire, then release) —
// the race detector is the actual assertion here as much as the counts
// are.
func TestLeaseRelease(t *testing.T) {
	l := NewLease()
	const n = 100

	releases := make(chan func(), n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			releases <- l.Acquire("p", "s")
		}()
	}
	wg.Wait()
	close(releases)

	if got := l.Count("p", "s"); got != n {
		t.Fatalf("Count after %d concurrent Acquire = %d, want %d", n, got, n)
	}

	var wg2 sync.WaitGroup
	for release := range releases {
		wg2.Add(1)
		go func(release func()) {
			defer wg2.Done()
			release()
		}(release)
	}
	wg2.Wait()

	if got := l.Count("p", "s"); got != 0 {
		t.Fatalf("Count after releasing all %d = %d, want 0", n, got)
	}
}

// acquire calls l.Acquire(pool, name) n times and leaks the releases —
// these tests only care about the resulting in-flight count.
func acquire(l *Lease, pool, name string, n int) {
	for i := 0; i < n; i++ {
		l.Acquire(pool, name)
	}
}

// TestLeastconnPicksFewest asserts the documented formula — compare by
// inflight/weight, not raw inflight — and its two consequences: a
// genuinely less-loaded server wins outright, and a server that is only
// less loaded in raw terms but not weight-adjusted terms ties instead of
// winning (and the tie is broken by wrr). It also proves a weight-0
// (drained) server never wins the comparison even when its own raw
// inflight is the lowest possible: zero.
func TestLeastconnPicksFewest(t *testing.T) {
	t.Run("clear winner by weight-adjusted ratio", func(t *testing.T) {
		l := NewLease()
		acquire(l, "p", "x", 3) // ratio 3/1
		acquire(l, "p", "y", 1) // ratio 1/1
		got, ok := Leastconn(l, NewWRR(), "p", []*config.Server{srv("x", 1), srv("y", 1)})
		if !ok || got.Name != "y" {
			t.Fatalf("Leastconn = (%v, %v), want (y, true)", got, ok)
		}
	})

	t.Run("equal ratio ties, broken by wrr", func(t *testing.T) {
		l := NewLease()
		acquire(l, "p", "x", 2) // ratio 2/2 = 1
		acquire(l, "p", "y", 1) // ratio 1/1 = 1 -- raw inflight is lower, but the
		// weight-adjusted ratio is identical: this must NOT pick y outright.
		got, ok := Leastconn(l, NewWRR(), "p", []*config.Server{srv("x", 2), srv("y", 1)})
		if !ok {
			t.Fatalf("Leastconn ok = false, want true")
		}
		// wrr.Pick("p", [x(2), y(1)]) picks x first (weight 2 > 1 on an
		// idle counter) -- see TestWRRInterleaving's "3-2-1" case for the
		// same arithmetic.
		if got.Name != "x" {
			t.Fatalf("Leastconn tie-break = %v, want x", got.Name)
		}
	})

	t.Run("drained server never wins even at zero inflight", func(t *testing.T) {
		l := NewLease()
		acquire(l, "p", "y", 5) // y is busy; z is completely idle but weight 0
		got, ok := Leastconn(l, NewWRR(), "p", []*config.Server{srv("z", 0), srv("y", 1)})
		if !ok || got.Name != "y" {
			t.Fatalf("Leastconn = (%v, %v), want (y, true)", got, ok)
		}
	})

	t.Run("all-drained set picks nothing", func(t *testing.T) {
		l := NewLease()
		got, ok := Leastconn(l, NewWRR(), "p", []*config.Server{srv("z", 0)})
		if ok || got != nil {
			t.Fatalf("Leastconn = (%v, %v), want (nil, false)", got, ok)
		}
	})
}

// TestLeastconnDegradesToWRRWhenIdle: with every candidate's in-flight
// count at zero, every weight-adjusted ratio is 0/weight = 0 — a tie
// across the board — so leastconn's pick reduces to exactly wrr's own
// pick, every time. This asserts the same weights-3/2/1 sequence
// TestWRRInterleaving proves for wrr alone.
func TestLeastconnDegradesToWRRWhenIdle(t *testing.T) {
	l := NewLease()
	wrr := NewWRR()
	servers := []*config.Server{srv("a", 3), srv("b", 2), srv("c", 1)}
	want := []string{"a", "b", "a", "c", "b", "a", "a", "b", "a", "c", "b", "a"}

	got := make([]string, 0, len(want))
	for i := 0; i < len(want); i++ {
		s, ok := Leastconn(l, wrr, "p", servers)
		if !ok {
			t.Fatalf("pick %d: ok = false, want true", i)
		}
		got = append(got, s.Name)
	}
	if !equalStrs(got, want) {
		t.Errorf("sequence = %v, want %v", got, want)
	}
}

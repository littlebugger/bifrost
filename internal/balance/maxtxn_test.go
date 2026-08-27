package balance

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/littlebugger/bifrost/internal/config"
)

// capPool is a single-server pool with a configurable max_transactions
// cap, isolating the saturation filter from the weight/health filtering
// candidates_test.go already covers.
func capPool(maxTxn int) *config.Pool {
	return &config.Pool{
		Name:    "p",
		Balance: "roundrobin",
		Servers: []config.Server{{Name: "a", Weight: 1, MaxTransactions: maxTxn}},
	}
}

// TestMaxTransactionsSkipsSaturated: a server holding as many in-flight
// transactions as its max_transactions cap is excluded from the
// candidate list entirely -- the same treatment an unhealthy server
// gets, not a demotion to the tail.
func TestMaxTransactionsSkipsSaturated(t *testing.T) {
	pool := capPool(2)
	lease := NewLease()
	release1 := lease.Acquire(pool.Name, "a")
	release2 := lease.Acquire(pool.Name, "a")
	defer release1()
	defer release2()

	got, allSaturated := Candidates(pool, allEligible, NewWRR(), lease, rand.New(rand.NewSource(1)))
	if len(got) != 0 {
		t.Fatalf("candidates = %v, want empty (server at its cap)", namesOf(got))
	}
	if !allSaturated {
		t.Errorf("allSaturated = false, want true (eligible but every one is at its cap)")
	}
}

// TestMaxTransactionsZeroMeansUnlimited: MaxTransactions 0 never filters,
// no matter how many transactions are in flight.
func TestMaxTransactionsZeroMeansUnlimited(t *testing.T) {
	pool := capPool(0)
	lease := NewLease()
	for i := 0; i < 50; i++ {
		lease.Acquire(pool.Name, "a")
	}

	got, allSaturated := Candidates(pool, allEligible, NewWRR(), lease, rand.New(rand.NewSource(1)))
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("candidates = %v, want [a] (unlimited)", namesOf(got))
	}
	if allSaturated {
		t.Errorf("allSaturated = true, want false (unlimited server can't saturate)")
	}
}

// TestSaturationReleasedRestoresEligibility: once a released transaction
// drops the in-flight count back under the cap, the server is a
// candidate again -- the filter reads live state on every call, nothing
// sticky about it.
func TestSaturationReleasedRestoresEligibility(t *testing.T) {
	pool := capPool(1)
	lease := NewLease()
	release := lease.Acquire(pool.Name, "a")

	if got, _ := Candidates(pool, allEligible, NewWRR(), lease, rand.New(rand.NewSource(1))); len(got) != 0 {
		t.Fatalf("candidates while saturated = %v, want empty", namesOf(got))
	}

	release()

	got, _ := Candidates(pool, allEligible, NewWRR(), lease, rand.New(rand.NewSource(1)))
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("candidates after release = %v, want [a]", namesOf(got))
	}
}

// TestLeaseTryAcquireRaceSafe is TryAcquire's own concurrency proof:
// Candidates' cap filter (proved above) only ever reads a snapshot, so
// the hard limit lives here instead, in the atomic check-and-increment
// under Lease's own mutex. Many goroutines race TryAcquire at once
// (started together via a closed gate, to make the contention as fierce
// as the scheduler allows), and exactly cap of them must win, never
// more -- the property internal/proxy's attach.go relies on when it
// treats a nil release as "lost the race, try the next candidate".
func TestLeaseTryAcquireRaceSafe(t *testing.T) {
	const (
		racers = 50
		maxTxn = 3
	)
	lease := NewLease()

	gate := make(chan struct{})
	var wg sync.WaitGroup
	var granted atomic.Int64
	releases := make(chan func(), racers)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			release, ok := lease.TryAcquire("p", "a", maxTxn)
			if ok {
				granted.Add(1)
				releases <- release
			}
		}()
	}
	close(gate)
	wg.Wait()
	close(releases)

	if got := granted.Load(); got != maxTxn {
		t.Fatalf("granted = %d, want exactly %d (racers = %d)", got, maxTxn, racers)
	}
	if got := lease.Count("p", "a"); got != maxTxn {
		t.Fatalf("Count after the race = %d, want %d", got, maxTxn)
	}

	for release := range releases {
		release()
	}
	if got := lease.Count("p", "a"); got != 0 {
		t.Fatalf("Count after releasing every granted lease = %d, want 0", got)
	}
}

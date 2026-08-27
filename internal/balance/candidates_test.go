package balance

import (
	"math/rand"
	"testing"

	"github.com/revolee/bifrost/internal/config"
)

// allEligible is an EligibleFunc that admits everything.
func allEligible(string, string) bool { return true }

// except returns an EligibleFunc that admits everything but the named
// servers.
func except(names ...string) EligibleFunc {
	excluded := map[string]bool{}
	for _, n := range names {
		excluded[n] = true
	}
	return func(_, server string) bool { return !excluded[server] }
}

func namesOf(servers []*config.Server) []string {
	out := make([]string, len(servers))
	for i, s := range servers {
		out[i] = s.Name
	}
	return out
}

// sameSet reports whether a and b hold the same names, order ignored.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := map[string]int{}
	for _, n := range a {
		count[n]++
	}
	for _, n := range b {
		count[n]--
	}
	for _, c := range count {
		if c != 0 {
			return false
		}
	}
	return true
}

// pool3plus1 is a pool with three weighted primaries and one backup, all
// distinct weights so a wrong tier assignment is easy to spot.
func pool3plus1() *config.Pool {
	return &config.Pool{
		Name:    "p",
		Balance: "roundrobin",
		Servers: []config.Server{
			{Name: "a", Weight: 3},
			{Name: "b", Weight: 2},
			{Name: "c", Weight: 1},
			{Name: "bkup", Weight: 5, Backup: true},
		},
	}
}

// TestCandidatesOrder: the primary the pool's own algorithm would pick
// leads, the remaining eligible primaries follow in a weighted-shuffled
// (but seed-reproducible) order, and the eligible backup is last.
func TestCandidatesOrder(t *testing.T) {
	pool := pool3plus1()

	got, allSaturated := Candidates(pool, allEligible, NewWRR(), NewLease(), rand.New(rand.NewSource(1)))
	if allSaturated {
		t.Fatalf("allSaturated = true, want false (nothing is saturated here)")
	}
	if len(got) != 4 {
		t.Fatalf("len(candidates) = %d, want 4: %v", len(got), namesOf(got))
	}
	// wrr.Pick("p", [a(3), b(2), c(1)]) on a fresh WRR picks "a" first --
	// see TestWRRInterleaving's "3-2-1" case for the same arithmetic.
	if got[0].Name != "a" {
		t.Errorf("head = %q, want %q (the algorithm's own pick)", got[0].Name, "a")
	}
	if middle := namesOf(got[1:3]); !sameSet(middle, []string{"b", "c"}) {
		t.Errorf("middle segment = %v, want {b, c} in some order", middle)
	}
	if got[3].Name != "bkup" {
		t.Errorf("tail = %q, want the backup last", got[3].Name)
	}

	// Same seed, fresh state: the exact same order comes out again. This
	// is the "seeded rand -> deterministic" half of the property; a
	// distribution test over many seeds is out of scope here (that is
	// wrr's and leastconn's own job, proven in their own tests) -- this
	// one only needs to prove the shuffle is reproducible, not random.
	again, _ := Candidates(pool, allEligible, NewWRR(), NewLease(), rand.New(rand.NewSource(1)))
	gotNames, againNames := namesOf(got), namesOf(again)
	for i := range gotNames {
		if gotNames[i] != againNames[i] {
			t.Errorf("same seed produced a different order: %v vs %v", gotNames, againNames)
			break
		}
	}
}

// TestCandidatesFilterIneligible: an ineligible primary is excluded
// entirely -- not demoted, not shuffled in later, just gone -- and the
// algorithm pick is computed over the eligible survivors only.
func TestCandidatesFilterIneligible(t *testing.T) {
	pool := &config.Pool{
		Name:    "p",
		Balance: "roundrobin",
		Servers: []config.Server{
			{Name: "a", Weight: 1},
			{Name: "b", Weight: 1},
			{Name: "c", Weight: 1},
		},
	}

	got, _ := Candidates(pool, except("b"), NewWRR(), NewLease(), rand.New(rand.NewSource(1)))

	want := []string{"a", "c"} // wrr.Pick("p", [a(1), c(1)]) picks a first; c is the only one left
	if !sameOrder(namesOf(got), want) {
		t.Fatalf("candidates = %v, want %v", namesOf(got), want)
	}
	for _, s := range got {
		if s.Name == "b" {
			t.Fatalf("ineligible server %q appeared in %v", "b", namesOf(got))
		}
	}
}

func sameOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestBackupTierOnlyWhenNoPrimaries: a healthy primary keeps backups in
// the tail; only the absence of ANY eligible primary promotes the
// backup tier to the lead.
func TestBackupTierOnlyWhenNoPrimaries(t *testing.T) {
	t.Run("primary healthy: backup stays in the tail", func(t *testing.T) {
		pool := &config.Pool{
			Name:    "p",
			Balance: "roundrobin",
			Servers: []config.Server{
				{Name: "a", Weight: 1},
				{Name: "z", Weight: 1, Backup: true},
			},
		}
		candidates, _ := Candidates(pool, allEligible, NewWRR(), NewLease(), rand.New(rand.NewSource(1)))
		got := namesOf(candidates)
		want := []string{"a", "z"}
		if !sameOrder(got, want) {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	})

	t.Run("no eligible primary: backups lead", func(t *testing.T) {
		pool := &config.Pool{
			Name:    "p",
			Balance: "roundrobin",
			Servers: []config.Server{
				{Name: "a", Weight: 1}, // made ineligible below
				{Name: "z", Weight: 1, Backup: true},
				{Name: "y", Weight: 1, Backup: true},
			},
		}
		candidates, _ := Candidates(pool, except("a"), NewWRR(), NewLease(), rand.New(rand.NewSource(1)))
		got := namesOf(candidates)
		// wrr.Pick("p", [z(1), y(1)]) picks z first; y is all that's left.
		// No primaries prefix at all: len is 2, not 3.
		want := []string{"z", "y"}
		if !sameOrder(got, want) {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	})
}

// TestCandidatesEmptyWhenAllIneligible: nothing eligible anywhere (in
// either tier) means an empty list, not an error -- the relay is the one
// that turns that into the client-visible 451.
func TestCandidatesEmptyWhenAllIneligible(t *testing.T) {
	pool := pool3plus1()
	got, allSaturated := Candidates(pool, func(string, string) bool { return false }, NewWRR(), NewLease(), rand.New(rand.NewSource(1)))
	if len(got) != 0 {
		t.Fatalf("candidates = %v, want empty", namesOf(got))
	}
	if allSaturated {
		t.Errorf("allSaturated = true, want false (empty because unhealthy, not because full)")
	}

	empty := &config.Pool{Name: "p", Balance: "roundrobin"}
	got, allSaturated = Candidates(empty, allEligible, NewWRR(), NewLease(), rand.New(rand.NewSource(1)))
	if len(got) != 0 {
		t.Fatalf("candidates for a serverless pool = %v, want empty", namesOf(got))
	}
	if allSaturated {
		t.Errorf("allSaturated = true, want false (empty because there is nothing to be saturated)")
	}
}

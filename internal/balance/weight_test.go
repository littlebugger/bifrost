package balance

import (
	"math/rand"
	"testing"

	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/proxy"
)

// TestWeightsOfNilAndOverride: a nil *Weights (every pre-epic-09 call
// site) always returns configured; once set, the override wins.
func TestWeightsOfNilAndOverride(t *testing.T) {
	var wt *Weights
	if got := wt.of("p", "s", 5); got != 5 {
		t.Fatalf("nil Weights.of = %d, want 5 (configured)", got)
	}

	wt = &Weights{}
	if got := wt.of("p", "s", 5); got != 5 {
		t.Fatalf("Weights.of with no override = %d, want 5", got)
	}
	wt.set("p", "s", 9)
	if got := wt.of("p", "s", 5); got != 9 {
		t.Fatalf("Weights.of after set = %d, want 9 (override)", got)
	}
	// A different server, or a different pool of the same name, is
	// unaffected.
	if got := wt.of("p", "other", 5); got != 5 {
		t.Fatalf("Weights.of for an unrelated server = %d, want 5", got)
	}
	if got := wt.of("other-pool", "s", 5); got != 5 {
		t.Fatalf("Weights.of for an unrelated pool = %d, want 5", got)
	}
}

// TestWeightsResetAll discards every override and reports exactly which
// (pool, server) identities had one, sorted.
func TestWeightsResetAll(t *testing.T) {
	wt := &Weights{}
	if got := wt.resetAll(); got != nil {
		t.Fatalf("resetAll on empty = %v, want nil", got)
	}

	wt.set("p", "b", 1)
	wt.set("p", "a", 2)
	got := wt.resetAll()
	want := []string{"p/a", "p/b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("resetAll = %v, want %v", got, want)
	}
	// Discarded for good: the overrides are gone, and a second call has
	// nothing left to report.
	if wt.of("p", "a", 5) != 5 {
		t.Fatalf("override for p/a survived resetAll")
	}
	if got := wt.resetAll(); got != nil {
		t.Fatalf("second resetAll = %v, want nil", got)
	}
}

// TestRouterSetWeightRebuildsWRR: SetWeight shifts the very next Pick's
// distribution — it is visible to WRR.Pick without any config reload,
// and Router.Weight reports the override.
func TestRouterSetWeightRebuildsWRR(t *testing.T) {
	holder := &config.Holder{}
	holder.Swap(&config.Config{
		Pools: []config.Pool{{
			Name: "p",
			Servers: []config.Server{
				{Name: "a", Weight: 1},
				{Name: "b", Weight: 1},
			},
		}},
		Routing: config.Routing{DefaultPool: "p"},
	})
	router := NewRouter(holder, allEligible, rand.New(rand.NewSource(1)))

	if w, ok := router.Weight("p", "a"); !ok || w != 1 {
		t.Fatalf("Weight before override = (%d, %v), want (1, true)", w, ok)
	}

	// Equal weights: "a" and "b" alternate one-for-one (TestWRRInterleaving's
	// "1-1" case). Drive one full cycle to confirm the baseline, then boost
	// "b" heavily and confirm the very next pick already reflects it.
	for i := 0; i < 2; i++ {
		if _, err := router.Pick(proxy.TxnMeta{}); err != nil {
			t.Fatalf("baseline Pick #%d: %v", i, err)
		}
	}

	if err := router.SetWeight("p", "b", 100); err != nil {
		t.Fatalf("SetWeight: %v", err)
	}
	if w, ok := router.Weight("p", "b"); !ok || w != 100 {
		t.Fatalf("Weight after override = (%d, %v), want (100, true)", w, ok)
	}

	bWins := 0
	const n = 20
	for i := 0; i < n; i++ {
		got, err := router.Pick(proxy.TxnMeta{})
		if err != nil {
			t.Fatalf("Pick #%d: %v", i, err)
		}
		if got[0].Name == "b" {
			bWins++
		}
	}
	if bWins < n-1 {
		t.Fatalf("b won the lead pick %d/%d times after a 100x override, want ~%d", bWins, n, n)
	}

	if discarded := router.ResetWeights(); len(discarded) != 1 || discarded[0] != "p/b" {
		t.Fatalf("ResetWeights = %v, want [p/b]", discarded)
	}
	if w, ok := router.Weight("p", "b"); !ok || w != 1 {
		t.Fatalf("Weight after ResetWeights = (%d, %v), want (1, true) -- back to configured", w, ok)
	}
}

// TestRouterSetWeightUnknownServer: SetWeight reports the "not found"
// admin/internal handler maps to 404, mirroring health.Checker.
func TestRouterSetWeightUnknownServer(t *testing.T) {
	holder := &config.Holder{}
	holder.Swap(&config.Config{Pools: []config.Pool{{Name: "p", Servers: []config.Server{{Name: "a", Weight: 1}}}}})
	router := NewRouter(holder, allEligible, rand.New(rand.NewSource(1)))

	if err := router.SetWeight("p", "nope", 5); err == nil {
		t.Fatalf("SetWeight on an unknown server: err = nil, want an error")
	}
	if err := router.SetWeight("nope", "a", 5); err == nil {
		t.Fatalf("SetWeight on an unknown pool: err = nil, want an error")
	}
	if _, ok := router.Weight("p", "nope"); ok {
		t.Fatalf("Weight on an unknown server: ok = true, want false")
	}
}

// TestWeightsArgRejectsMultiple: more than one optional override is a
// caller bug — it must panic rather than silently pick one and drop the
// rest. A nil first entry followed by a real one is exactly the case
// that would otherwise silently discard the real table.
func TestWeightsArgRejectsMultiple(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("weightsArg with 2 overrides: want a panic, got none")
		}
	}()
	weightsArg([]*Weights{nil, {}})
}

func TestWeightsArgZeroOrOne(t *testing.T) {
	if got := weightsArg(nil); got != nil {
		t.Fatalf("weightsArg(nil) = %v, want nil", got)
	}
	if got := weightsArg([]*Weights{nil}); got != nil {
		t.Fatalf("weightsArg([nil]) = %v, want nil", got)
	}
	wt := &Weights{}
	if got := weightsArg([]*Weights{wt}); got != wt {
		t.Fatalf("weightsArg([wt]) = %v, want wt", got)
	}
}

package balance

import (
	"testing"

	"github.com/revolee/bifrost/internal/config"
)

// srv is a test-only *config.Server builder: name and weight are all any
// balance test needs from a server.
func srv(name string, weight int) *config.Server {
	return &config.Server{Name: name, Weight: weight}
}

// pickNames calls Pick n times and returns the winner's name each time;
// it fails the test outright if any pick reports ok=false.
func pickNames(t *testing.T, w *WRR, pool string, servers []*config.Server, n int) []string {
	t.Helper()
	out := make([]string, n)
	for i := 0; i < n; i++ {
		s, ok := w.Pick(pool, servers)
		if !ok {
			t.Fatalf("pick %d: ok = false, want true", i)
		}
		out[i] = s.Name
	}
	return out
}

// TestWRRInterleaving asserts the exact nginx-algorithm output (current
// += weight; pick max; current -= total) for three weight sets, each
// verified by hand simulation and each carried across two full cycles to
// prove the sequence repeats. The second cycle of the first case uses
// brand-new *config.Server pointers with the same names/weights — a
// stand-in for a config reload — to prove state is keyed by (pool,
// server) name, never by pointer.
func TestWRRInterleaving(t *testing.T) {
	tests := []struct {
		name    string
		weights []int // a, b, c...
		cycle   []string
	}{
		{"5-1-1", []int{5, 1, 1}, []string{"a", "a", "b", "a", "c", "a", "a"}},
		{"1-1", []int{1, 1}, []string{"a", "b"}},
		{"3-2-1", []int{3, 2, 1}, []string{"a", "b", "a", "c", "b", "a"}},
	}

	names := []string{"a", "b", "c"}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWRR()
			build := func() []*config.Server {
				out := make([]*config.Server, len(tc.weights))
				for i, wt := range tc.weights {
					out[i] = srv(names[i], wt)
				}
				return out
			}

			var want []string
			want = append(want, tc.cycle...)
			want = append(want, tc.cycle...)

			var got []string
			got = append(got, pickNames(t, w, "p", build(), len(tc.cycle))...)
			// Fresh pointers, same names: state must follow the name, not
			// the old slice's identity.
			got = append(got, pickNames(t, w, "p", build(), len(tc.cycle))...)

			if !equalStrs(got, want) {
				t.Errorf("sequence = %v, want %v", got, want)
			}
		})
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestWRRSingleServer: a pool of one server always picks that server.
func TestWRRSingleServer(t *testing.T) {
	w := NewWRR()
	only := srv("only", 4)
	for i := 0; i < 6; i++ {
		s, ok := w.Pick("p", []*config.Server{only})
		if !ok || s.Name != "only" {
			t.Fatalf("pick %d = (%v, %v), want (only, true)", i, s, ok)
		}
	}
}

// TestWRRZeroWeightSkipped: weight 0 is a config-level drain. It never
// wins while a positive-weight server exists, and a pool that is nothing
// but weight-0 servers (even a single one, alone) picks nothing at all.
func TestWRRZeroWeightSkipped(t *testing.T) {
	t.Run("skipped while others exist", func(t *testing.T) {
		w := NewWRR()
		servers := []*config.Server{srv("z", 0), srv("a", 1), srv("b", 1)}
		for i := 0; i < 20; i++ {
			s, ok := w.Pick("p", servers)
			if !ok {
				t.Fatalf("pick %d: ok = false, want true", i)
			}
			if s.Name == "z" {
				t.Fatalf("pick %d picked the weight-0 server", i)
			}
		}
	})

	t.Run("zero-weight-only pool picks nothing", func(t *testing.T) {
		w := NewWRR()
		s, ok := w.Pick("p", []*config.Server{srv("z1", 0), srv("z2", 0)})
		if ok || s != nil {
			t.Fatalf("Pick = (%v, %v), want (nil, false)", s, ok)
		}
	})

	t.Run("a single weight-0 server, alone, still picks nothing", func(t *testing.T) {
		w := NewWRR()
		s, ok := w.Pick("p", []*config.Server{srv("z", 0)})
		if ok || s != nil {
			t.Fatalf("Pick = (%v, %v), want (nil, false)", s, ok)
		}
	})
}

// TestWRRDistribution: 10k picks over weights 3/2/1 land within ±2% of
// each server's proportional share — the load property, as distinct
// from TestWRRInterleaving's exact-sequence property.
func TestWRRDistribution(t *testing.T) {
	const n = 10000
	w := NewWRR()
	servers := []*config.Server{srv("a", 3), srv("b", 2), srv("c", 1)}

	counts := map[string]int{}
	for i := 0; i < n; i++ {
		s, ok := w.Pick("p", servers)
		if !ok {
			t.Fatalf("pick %d: ok = false, want true", i)
		}
		counts[s.Name]++
	}

	total := 0
	for _, s := range servers {
		total += s.Weight
	}
	for _, s := range servers {
		want := float64(n) * float64(s.Weight) / float64(total)
		got := float64(counts[s.Name])
		tolerance := 0.02 * want
		if got < want-tolerance || got > want+tolerance {
			t.Errorf("server %q got %d picks, want %.0f ±2%%", s.Name, counts[s.Name], want)
		}
	}
}

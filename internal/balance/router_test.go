package balance

import (
	"math/rand"
	"testing"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/proxy"
)

// twoPoolConfig has a b.com routing rule to "p2" and everything else
// falling through to "p1".
func twoPoolConfig() *config.Config {
	return &config.Config{
		Pools: []config.Pool{
			{Name: "p1", Balance: "roundrobin", Servers: []config.Server{{Name: "s1", Weight: 1}}},
			{Name: "p2", Balance: "roundrobin", Servers: []config.Server{{Name: "t1", Weight: 1}}},
		},
		Routing: config.Routing{
			Rules:       []config.RoutingRule{{MailFromDomain: []string{"b.com"}, Pool: "p2"}},
			DefaultPool: "p1",
		},
	}
}

// TestRouterPickEndToEnd: the right pool, the right ordering, and a
// config swap takes effect at the very next Pick (no config captured at
// construction time).
func TestRouterPickEndToEnd(t *testing.T) {
	holder := &config.Holder{}
	holder.Swap(twoPoolConfig())
	router := NewRouter(holder, allEligible, rand.New(rand.NewSource(1)))

	got, err := router.Pick(proxy.TxnMeta{MailFromDomain: "a.com"})
	if err != nil || len(got) != 1 || got[0].Name != "s1" {
		t.Fatalf("Pick(a.com) = (%v, %v), want ([s1], nil)", namesOf(got), err)
	}

	got, err = router.Pick(proxy.TxnMeta{MailFromDomain: "b.com"})
	if err != nil || len(got) != 1 || got[0].Name != "t1" {
		t.Fatalf("Pick(b.com) = (%v, %v), want ([t1], nil)", namesOf(got), err)
	}

	// p1 gains a second server; the very next Pick must see it.
	cfg2 := twoPoolConfig()
	cfg2.Pools[0].Servers = append(cfg2.Pools[0].Servers, config.Server{Name: "s2", Weight: 5})
	holder.Swap(cfg2)

	got, err = router.Pick(proxy.TxnMeta{MailFromDomain: "a.com"})
	if err != nil {
		t.Fatalf("Pick after swap: err = %v", err)
	}
	if !sameSet(namesOf(got), []string{"s1", "s2"}) {
		t.Fatalf("Pick after swap = %v, want {s1, s2}", namesOf(got))
	}
}

// TestRouterPickErrorsWhenPoolMissing: the one documented error case --
// the resolved pool (here, an unset default_pool) does not exist in the
// loaded config. config.Validate prevents this in a real deployment;
// Router still reports it rather than panic.
func TestRouterPickErrorsWhenPoolMissing(t *testing.T) {
	holder := &config.Holder{}
	holder.Swap(&config.Config{Routing: config.Routing{DefaultPool: "ghost"}})
	router := NewRouter(holder, allEligible, rand.New(rand.NewSource(1)))

	if _, err := router.Pick(proxy.TxnMeta{}); err == nil {
		t.Fatal("Pick with a nonexistent default_pool: err = nil, want non-nil")
	}
}

// TestRouterLease: Lease resolves srv to its pool and counts against
// (pool, server) name, and a server no longer present in the currently
// loaded config degrades to an uncounted no-op instead of guessing.
func TestRouterLease(t *testing.T) {
	cfg := twoPoolConfig()
	holder := &config.Holder{}
	holder.Swap(cfg)
	router := NewRouter(holder, allEligible, rand.New(rand.NewSource(1)))

	srv := &cfg.Pools[0].Servers[0] // p1/s1
	release := router.Lease(srv)
	if got := router.InFlight("p1", "s1"); got != 1 {
		t.Fatalf("InFlight after Lease = %d, want 1", got)
	}
	release()
	if got := router.InFlight("p1", "s1"); got != 0 {
		t.Fatalf("InFlight after release = %d, want 0", got)
	}

	stale := &config.Server{Name: "gone", Address: "10.0.0.9:25", Weight: 1}
	release = router.Lease(stale) // not part of cfg at all
	release()                     // must not panic
	if got := router.InFlight("p1", "gone"); got != 0 {
		t.Fatalf("InFlight for an unresolvable server = %d, want 0", got)
	}
}

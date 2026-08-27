package balance

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"

	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/proxy"
)

// Router is the R1 facade internal/proxy's relay is wired to: Pick is
// its PickFunc (rule-match → pool → ordered candidates, rules.go +
// candidates.go), Lease is its LeaseFunc (in-flight accounting shared
// with leastconn).
type Router struct {
	cfg  *config.Holder
	elig EligibleFunc

	rndMu sync.Mutex
	rnd   *rand.Rand

	wrr     *WRR
	lease   *Lease
	weights *Weights
}

// NewRouter builds a Router over cfg's live configuration. elig gates
// eligibility (internal/health.Checker.Eligible in production; tests may
// stub it directly). rnd drives Candidates' weighted-shuffle tail and
// must be non-nil — seeded for deterministic tests, real randomness in
// production; Router serializes its own use of it, so concurrent
// transactions never race it.
func NewRouter(cfg *config.Holder, elig EligibleFunc, rnd *rand.Rand) *Router {
	return &Router{cfg: cfg, elig: elig, rnd: rnd, wrr: NewWRR(), lease: NewLease(), weights: &Weights{}}
}

// Pick implements proxy.PickFunc: rule-match → pool → ordered
// candidates, read fresh from the holder on every call (a config swap
// takes effect at the next Pick, not the next restart). It errors when
// the resolved pool does not exist in the loaded config — which
// config.Validate prevents in a real deployment (an unknown rule/
// default_pool target is a load error), so this is a defensive report,
// not an expected outcome — or when the pool has at least one eligible
// server and every one of them is at its max_transactions cap
// (proxy.ErrAllSaturated, epic 08), so the relay can answer the
// contract's 451 4.3.2 instead of the unhealthy-empty 451 4.4.1. Any
// other empty, nil-error result means the pool exists but nothing in it
// is healthy; the relay turns that into the client-visible 451 4.4.1.
func (r *Router) Pick(tx proxy.TxnMeta) ([]*config.Server, error) {
	cfg := r.cfg.Load()
	if cfg == nil {
		return nil, errors.New("balance: no configuration loaded")
	}

	poolName := MatchPool(cfg.Routing, tx)
	pool := findPool(cfg, poolName)
	if pool == nil {
		return nil, fmt.Errorf("balance: pool %q not found", poolName)
	}

	r.rndMu.Lock()
	defer r.rndMu.Unlock()
	candidates, allSaturated := Candidates(pool, r.elig, r.wrr, r.lease, r.rnd, r.weights)
	if allSaturated {
		return nil, proxy.ErrAllSaturated
	}
	return candidates, nil
}

// Lease implements proxy.LeaseFunc: it registers one in-flight
// transaction against srv, keyed by srv's (pool, server) name, and
// returns the release to call when the transaction ends — or nil when
// srv is already carrying its resolved max_transactions cap's worth of
// in-flight transactions at this exact instant (epic 08). A caller that
// gets nil back MUST NOT treat srv as attached: Pick's own cap filter
// (candidates.go) only ever reads a snapshot, and multiple transactions
// can race through it having all seen the same under-cap snapshot
// before any of them reaches this call — TryAcquire is where the cap is
// actually, atomically, enforced.
//
// srv is resolved against the config currently loaded, which in the
// overwhelmingly common case is the exact same snapshot Pick returned
// srv from — but not a moment later: the caller dials and handshakes
// srv in between, a window bounded by BackendConnect+BackendHandshake
// (up to ~20s at the documented defaults), not "moments". A reload
// landing in that window is the only way the lookup can miss, and when
// it does this is a no-op rather than a guess — an uncounted lease
// costs leastconn nothing worth misattributing it over.
func (r *Router) Lease(srv *config.Server) func() {
	pool := poolFor(r.cfg.Load(), srv)
	if pool == nil {
		return func() {}
	}
	release, _ := r.lease.TryAcquire(pool.Name, srv.Name, srv.MaxTransactions)
	return release
}

// InFlight returns (pool, server)'s current in-flight count.
func (r *Router) InFlight(pool, server string) int {
	return r.lease.Count(pool, server)
}

// Weight returns (pool, server)'s effective weight: an admin override
// (SetWeight) if one is set, else its configured value. 0, false means
// (pool, server) does not exist in the currently loaded config.
func (r *Router) Weight(pool, server string) (weight int, ok bool) {
	srv := r.findServer(pool, server)
	if srv == nil {
		return 0, false
	}
	return r.weights.of(pool, server, srv.Weight), true
}

// SetWeight installs a runtime weight override for (pool, server) —
// epic-09's admin API, "runtime-only, documented ephemeral" (PROJECT.md
// D15): it never touches the config tree, so it is visible to the very
// next Pick and gone entirely on the next successful reload
// (ResetWeights). It errors when (pool, server) does not exist in the
// currently loaded config; the caller (internal/admin) maps that to 404.
func (r *Router) SetWeight(pool, server string, weight int) error {
	if r.findServer(pool, server) == nil {
		return fmt.Errorf("balance: server %q in pool %q not found", server, pool)
	}
	r.weights.set(pool, server, weight)
	return nil
}

// ResetWeights discards every runtime weight override — the reload
// endpoint calls this after a successful swap (D15) — and returns the
// sorted "pool/server" identities that had one, for its discard log.
func (r *Router) ResetWeights() []string {
	return r.weights.resetAll()
}

// findServer returns (pool, server)'s *config.Server in the currently
// loaded config, or nil.
func (r *Router) findServer(pool, server string) *config.Server {
	cfg := r.cfg.Load()
	if cfg == nil {
		return nil
	}
	p := findPool(cfg, pool)
	if p == nil {
		return nil
	}
	for i := range p.Servers {
		if p.Servers[i].Name == server {
			return &p.Servers[i]
		}
	}
	return nil
}

// findPool returns cfg's pool named name, or nil.
func findPool(cfg *config.Config, name string) *config.Pool {
	for i := range cfg.Pools {
		if cfg.Pools[i].Name == name {
			return &cfg.Pools[i]
		}
	}
	return nil
}

// poolFor returns the pool srv belongs to, by pointer identity, or nil
// if srv is not part of cfg's current tree at all.
func poolFor(cfg *config.Config, srv *config.Server) *config.Pool {
	if cfg == nil {
		return nil
	}
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		for j := range pool.Servers {
			if &pool.Servers[j] == srv {
				return pool
			}
		}
	}
	return nil
}

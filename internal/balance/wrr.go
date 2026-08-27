// Package balance is Bifrost's R1 core: which pool a transaction routes
// to (the rule engine, rules.go) and which servers it may attach to, in
// what order (the balance algorithms and the ordered failover candidate
// list). Router (router.go) is the facade the relay is wired to.
package balance

import (
	"sync"

	"github.com/littlebugger/bifrost/internal/config"
)

// svrKey identifies a server by its stable (pool, server) name pair, not
// by *config.Server pointer: a config reload replaces every pointer
// wholesale (internal/config.Holder.Swap), so name is the only identity
// that survives it. Shared by every piece of per-server state balance
// keeps (wrr's current-weight counters, lease's in-flight counts).
type svrKey struct {
	pool   string
	server string
}

// WRR is smooth weighted round-robin selection state — nginx's
// algorithm: a running counter per candidate gains its own weight each
// pick, the highest counter wins, and the winner's counter loses the
// picked set's total weight. Applied fresh over whichever candidate set
// the caller passes each time (the healthy, non-drained subset of one
// pool's servers), so a server excluded for a while and then reinstated
// resumes from wherever its counter was left.
//
// State is keyed by (pool, server) name, so a server that persists
// across a config reload keeps its position in the rotation; the mutex
// guards concurrent picks from many transactions at once.
type WRR struct {
	mu      sync.Mutex
	current map[svrKey]int
}

// NewWRR returns an empty WRR.
func NewWRR() *WRR {
	return &WRR{current: make(map[svrKey]int)}
}

// Pick returns the next server from servers per the smooth
// weighted-round-robin algorithm. ok is false when none of them has
// positive weight: weight 0 is a config-level drain (never picked), and
// a set that is nothing but drained servers — even a single one, alone —
// has nothing to pick.
//
// servers must be in a stable order (config order): a tie between two
// candidates' counters goes to whichever is earlier in servers.
// overrides is optional (variadic so every pre-epic-09 call site,
// including this package's own tests, keeps compiling unchanged): see
// weight.go's Weights for why a runtime weight override lives here
// rather than on the config tree.
func (w *WRR) Pick(pool string, servers []*config.Server, overrides ...*Weights) (*config.Server, bool) {
	wt := weightsArg(overrides)
	servers = positiveWeight(pool, servers, wt)
	if len(servers) == 0 {
		return nil, false
	}

	total := 0
	for _, s := range servers {
		total += wt.of(pool, s.Name, s.Weight)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	best := servers[0]
	bestCur := w.bump(pool, best, wt)
	for _, s := range servers[1:] {
		if cur := w.bump(pool, s, wt); cur > bestCur {
			best, bestCur = s, cur
		}
	}
	w.current[svrKey{pool, best.Name}] = bestCur - total
	return best, true
}

// bump adds s's effective weight to its running counter (creating it at
// 0 first if this is the first time (pool, s.Name) has been seen) and
// returns the new value.
func (w *WRR) bump(pool string, s *config.Server, wt *Weights) int {
	key := svrKey{pool, s.Name}
	cur := w.current[key] + wt.of(pool, s.Name, s.Weight)
	w.current[key] = cur
	return cur
}

// positiveWeight returns the servers in list with effective weight > 0 —
// the config-level (or admin-overridden) drain rule applied by every
// algorithm below.
func positiveWeight(pool string, servers []*config.Server, wt *Weights) []*config.Server {
	out := make([]*config.Server, 0, len(servers))
	for _, s := range servers {
		if wt.of(pool, s.Name, s.Weight) > 0 {
			out = append(out, s)
		}
	}
	return out
}

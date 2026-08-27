package balance

import (
	"math/rand"

	"github.com/revolee/bifrost/internal/config"
)

// EligibleFunc reports whether (pool, server) is currently usable —
// internal/health.Checker.Eligible's shape, fail-closed for any pair it
// does not recognize.
type EligibleFunc func(pool, server string) bool

// Candidates returns pool's ordered failover candidate list for one
// transaction: healthy-filter, then the pool's balance algorithm picks
// the lead, then the rest of that tier is weighted-shuffled behind it
// (decision D13's "Mireka Upstream shape" — primaries, then backups).
//
// Backups always sit in the tail behind the primaries when at least one
// primary is eligible — insurance for a primary that looks healthy here
// but fails to dial a moment later. They lead only when there is no
// eligible primary at all, promoted to get the same algorithm-pick+
// shuffle treatment primaries would have gotten.
//
// A server is dropped entirely — from either tier, at either position —
// when it fails elig, when its weight is 0 (config-level drain), or when
// it is already carrying its resolved max_transactions cap's worth of
// in-flight transactions (epic 08; 0 means unlimited). Candidates never
// errors: an empty result means nothing is usable, and the relay is the
// one that turns that into the client-visible 451.
//
// allSaturated is the epic-05-review distinction that empty result
// alone can't carry: it is true exactly when at least one server passed
// the elig+weight gate (so something here is merely full, not down) and
// every one of those got dropped for saturation, leaving nothing.
// Router.Pick (epic 08) turns that into proxy.ErrAllSaturated so the
// relay answers 451 4.3.2 instead of the unhealthy-empty 451 4.4.1.
//
// wrr and lease carry this pool's algorithm state across calls, keyed by
// server name so a config reload doesn't reset it for a server that
// persists. rnd drives the weighted shuffle; it is not safe for
// concurrent use, so a caller sharing one rnd across goroutines (Router
// does) must serialize its own calls into Candidates.
// overrides is optional (variadic, see weight.go's Weights): a runtime
// weight override (Router.SetWeight) shadows a server's configured
// Weight everywhere below, including the weight<=0 drain check.
func Candidates(pool *config.Pool, elig EligibleFunc, wrr *WRR, lease *Lease, rnd *rand.Rand, overrides ...*Weights) (candidates []*config.Server, allSaturated bool) {
	wt := weightsArg(overrides)
	var primaries, backups []*config.Server
	sawEligible := false
	for i := range pool.Servers {
		s := &pool.Servers[i]
		if wt.of(pool.Name, s.Name, s.Weight) <= 0 || !elig(pool.Name, s.Name) {
			continue
		}
		sawEligible = true
		if saturated(pool.Name, s, lease) {
			continue
		}
		if s.Backup {
			backups = append(backups, s)
		} else {
			primaries = append(primaries, s)
		}
	}

	alg := algorithmFor(pool.Balance, wrr, lease, wt)
	lead := tier(pool.Name, primaries, alg, rnd, wt)
	if len(lead) > 0 {
		candidates = append(lead, tier(pool.Name, backups, alg, rnd, wt)...)
	} else {
		candidates = tier(pool.Name, backups, alg, rnd, wt)
	}
	return candidates, sawEligible && len(candidates) == 0
}

// saturated reports whether s is at or over its resolved
// max_transactions cap (epic 08). 0 means unlimited, so it is never
// saturated; otherwise it is filtered out of Candidates exactly like an
// ineligible server -- dropped entirely, not demoted.
func saturated(pool string, s *config.Server, lease *Lease) bool {
	return s.MaxTransactions > 0 && lease.Count(pool, s.Name) >= s.MaxTransactions
}

// pickFunc is one balance algorithm's call shape: wrr.Pick and
// Leastconn (curried over its Lease/WRR) both fit it.
type pickFunc func(pool string, servers []*config.Server) (*config.Server, bool)

// algorithmFor resolves a pool's configured balance mode. Anything other
// than "leastconn" — including "roundrobin" and an unset value — is wrr,
// v1's built-in default (see config's Pool.Balance doc).
func algorithmFor(balance string, wrr *WRR, lease *Lease, wt *Weights) pickFunc {
	if balance == "leastconn" {
		return func(pool string, servers []*config.Server) (*config.Server, bool) {
			return Leastconn(lease, wrr, pool, servers, wt)
		}
	}
	return func(pool string, servers []*config.Server) (*config.Server, bool) {
		return wrr.Pick(pool, servers, wt)
	}
}

// tier orders one already-filtered set (all eligible, all weight > 0):
// the algorithm's own pick leads, the rest follows weighted-shuffled.
// Returns nil for an empty set.
func tier(pool string, servers []*config.Server, alg pickFunc, rnd *rand.Rand, wt *Weights) []*config.Server {
	if len(servers) == 0 {
		return nil
	}
	picked, ok := alg(pool, servers)
	if !ok {
		// Can't happen: servers is already weight>0-filtered, and that is
		// the only reason either algorithm ever declines to pick.
		return nil
	}

	rest := make([]*config.Server, 0, len(servers)-1)
	for _, s := range servers {
		if s != picked {
			rest = append(rest, s)
		}
	}

	out := make([]*config.Server, 0, len(servers))
	out = append(out, picked)
	return append(out, weightedShuffle(rnd, pool, rest, wt)...)
}

// weightedShuffle returns servers in a random order weighted by weight
// (higher weight, more likely to land earlier) — the failover order
// behind the algorithm's own primary pick. servers must all have
// positive weight.
//
// ponytail: O(n²) (a linear removal-scan per draw); fine at realistic
// pool sizes (tens of servers), a weighted-sample structure is the
// upgrade if pools ever get huge.
func weightedShuffle(rnd *rand.Rand, pool string, servers []*config.Server, wt *Weights) []*config.Server {
	if len(servers) == 0 {
		return nil
	}
	remaining := append([]*config.Server(nil), servers...)
	out := make([]*config.Server, 0, len(remaining))
	for len(remaining) > 0 {
		total := 0
		for _, s := range remaining {
			total += wt.of(pool, s.Name, s.Weight)
		}
		draw := rnd.Intn(total)
		idx, acc := 0, 0
		for i, s := range remaining {
			acc += wt.of(pool, s.Name, s.Weight)
			if draw < acc {
				idx = i
				break
			}
		}
		out = append(out, remaining[idx])
		remaining = append(remaining[:idx], remaining[idx+1:]...)
	}
	return out
}

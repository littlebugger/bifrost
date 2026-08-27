package balance

import "github.com/littlebugger/bifrost/internal/config"

// Leastconn picks the server with the fewest in-flight transactions,
// weight-adjusted: a server carrying 2 transactions at weight 2 is
// exactly as loaded as one carrying 1 at weight 1 (the ratio
// inflight/weight, compared here by cross-multiplication so the
// comparison never trips on floating-point rounding). Ties — every
// candidate idle is the common case, since 0/weight is 0 for all of
// them — are broken by wrr, which is how leastconn degrades to plain
// weighted round-robin whenever nothing is in flight.
//
// Weight-0 servers (config-level drain) are filtered out up front, same
// as wrr.Pick; ok is false when nothing else is left.
// overrides is optional (variadic, see weight.go's Weights and WRR.Pick's
// same-shaped doc comment).
func Leastconn(lease *Lease, wrr *WRR, pool string, servers []*config.Server, overrides ...*Weights) (*config.Server, bool) {
	wt := weightsArg(overrides)
	servers = positiveWeight(pool, servers, wt)
	if len(servers) == 0 {
		return nil, false
	}

	best := servers[0]
	bestWeight := wt.of(pool, best.Name, best.Weight)
	bestInflight := lease.Count(pool, best.Name)
	tied := []*config.Server{best}

	for _, s := range servers[1:] {
		sWeight := wt.of(pool, s.Name, s.Weight)
		inflight := lease.Count(pool, s.Name)
		lhs, rhs := inflight*bestWeight, bestInflight*sWeight
		switch {
		case lhs < rhs: // s's ratio is lower: it is the new, sole best
			best, bestWeight, bestInflight = s, sWeight, inflight
			tied = []*config.Server{s}
		case lhs == rhs: // equal ratio: both stay in contention
			tied = append(tied, s)
		}
	}

	if len(tied) == 1 {
		return tied[0], true
	}
	return wrr.Pick(pool, tied, wt)
}

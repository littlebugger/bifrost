package config

import (
	"net"
	"net/netip"
	"slices"
)

// TemplateKey names one server_dns template: the pool it lives in, and its
// own label within that pool.
type TemplateKey struct{ Pool, Template string }

// maxTemplateMembers is the per-template expanded-member cap (the design
// doc's "excess logged and truncated deterministically by sorted IP").
const maxTemplateMembers = 256

// ExpandDNS returns a snapshot derived from base (the operator's loaded
// config): every pool's DNSTemplates expanded into concrete Server entries
// named "<template>-<ip>" (port from the template's own address), appended
// after the pool's static servers in netip.Addr.Compare order and
// truncated at maxTemplateMembers per template (sorted tail dropped). It
// never mutates base; a pool with no templates shares base's Pool value
// untouched.
//
// Every call is a plain, deterministic rebuild: no prior snapshot is
// consulted, and no *Server is ever reused across calls — two expansions
// of the same membership return equal names/addresses/fields at fresh
// pointers, not the same pointers. Continuity across a membership delta is
// NAME-based, not pointer-based: internal/health.Checker already keys its
// per-server state (svrKey = pool+server name) and refreshes srv/params in
// place on every sync, so a member whose name persists keeps its health
// FSM regardless of *Server identity. The one thing pointer identity would
// have bought — internal/proxy's reuse-affinity cache recognizing a
// survivor across a swap — degrades gracefully to its own documented
// fallback (one silent re-dial) when the pointer doesn't match; that's a
// deliberately cheaper trade than carrying a backing-array-reuse
// invariant for it (see docs/superpowers/specs/2026-11-09-dns-discovery-
// design.md, amended).
//
// Callers must not derive two snapshots concurrently (e.g. two templates'
// resolver goroutines racing to expand the same base at once): nothing
// here is guarded by a lock, so the discovery driver (Task 3) must
// serialize ExpandDNS calls and the Holder.Swap that follows each one.
func ExpandDNS(base *Config, members map[TemplateKey][]netip.Addr) *Config {
	out := *base
	out.Pools = make([]Pool, len(base.Pools))
	for i, bp := range base.Pools {
		if len(bp.DNSTemplates) == 0 {
			out.Pools[i] = bp
			continue
		}
		out.Pools[i] = expandPool(bp, members)
	}
	return &out
}

// expandPool rebuilds one pool's Servers: bp's static servers, then every
// template's expanded members, in declaration order.
func expandPool(bp Pool, members map[TemplateKey][]netip.Addr) Pool {
	combined := make([]Server, 0, len(bp.Servers))
	combined = append(combined, bp.Servers...)
	for _, tmpl := range bp.DNSTemplates {
		combined = append(combined, expandTemplate(tmpl, members[TemplateKey{Pool: bp.Name, Template: tmpl.Name}])...)
	}
	bp.Servers = combined
	return bp
}

// expandTemplate expands one server_dns template into its member Servers,
// sorted by netip.Addr.Compare and truncated to maxTemplateMembers (the
// sorted tail dropped). Every member clones tmpl's Weight/Backup/
// MaxTransactions/Check verbatim; only Name and Address are per-member.
func expandTemplate(tmpl ServerDNS, addrs []netip.Addr) []Server {
	if len(addrs) == 0 {
		return nil
	}
	sorted := slices.Clone(addrs)
	slices.SortFunc(sorted, netip.Addr.Compare)
	if len(sorted) > maxTemplateMembers {
		sorted = sorted[:maxTemplateMembers]
	}

	_, port, _ := net.SplitHostPort(tmpl.Address)
	out := make([]Server, len(sorted))
	for i, ip := range sorted {
		out[i] = Server{
			Name:            tmpl.Name + "-" + ip.String(),
			Address:         net.JoinHostPort(ip.String(), port),
			Weight:          tmpl.Weight,
			Backup:          tmpl.Backup,
			MaxTransactions: tmpl.MaxTransactions,
			Check:           tmpl.Check,
		}
	}
	return out
}

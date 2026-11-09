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
// prev (nil on the very first expansion) supplies survivor identity: a
// member whose pool, name, and address match one already in prev reuses
// prev's exact *Server — the pointer identity internal/health and
// internal/proxy's reuse-affinity key on. This only holds when prev is the
// immediately preceding ExpandDNS output for the same base (the discovery
// driver must thread calls one at a time, never two derivations racing off
// the same prev): a pure membership GROWTH reuses prev's Servers backing
// array outright (new members land in its already-reserved spare capacity,
// which is why every build reserves maxTemplateMembers per template up
// front, memory no published snapshot's own length can reach — safe
// without a lock); anything else for a pool (a removal, or a reordering
// insert) falls back to a fresh rebuild, which starts that pool's identity
// over from scratch. Good enough for the common case (a farm that mostly
// grows); ponytail: full general-case identity preservation under
// arbitrary churn needs Pool.Servers to become []*Server, a field-type
// change out of scope here (flagged in the task report).
func ExpandDNS(base, prev *Config, members map[TemplateKey][]netip.Addr) *Config {
	out := *base
	out.Pools = make([]Pool, len(base.Pools))
	for i, bp := range base.Pools {
		if len(bp.DNSTemplates) == 0 {
			out.Pools[i] = bp
			continue
		}
		out.Pools[i] = expandPool(bp, findPool(prev, bp.Name), members)
	}
	return &out
}

// expandPool rebuilds one pool's Servers: bp's static servers, then every
// template's expanded members in declaration order, reusing prevPool's
// backing array when that is a pure append (see ExpandDNS's doc).
func expandPool(bp Pool, prevPool *Pool, members map[TemplateKey][]netip.Addr) Pool {
	static := bp.Servers
	var target []Server
	for _, tmpl := range bp.DNSTemplates {
		target = append(target, expandTemplate(tmpl, members[TemplateKey{Pool: bp.Name, Template: tmpl.Name}])...)
	}

	if prevPool != nil && len(prevPool.Servers) >= len(static) &&
		serversEqual(prevPool.Servers[:len(static)], static) {
		prevMembers := prevPool.Servers[len(static):]
		if isPrefixOf(prevMembers, target) {
			bp.Servers = append(prevPool.Servers, target[len(prevMembers):]...)
			return bp
		}
	}

	combined := make([]Server, 0, len(static)+maxTemplateMembers*len(bp.DNSTemplates))
	combined = append(combined, static...)
	combined = append(combined, target...)
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

// findPool returns prev's pool named name, or nil if prev is nil or has no
// such pool.
func findPool(prev *Config, name string) *Pool {
	if prev == nil {
		return nil
	}
	for i := range prev.Pools {
		if prev.Pools[i].Name == name {
			return &prev.Pools[i]
		}
	}
	return nil
}

// isPrefixOf reports whether prevMembers, in order, is exactly the first
// len(prevMembers) entries of target (by Name+Address) — the "pure
// append" condition expandPool's survivor reuse requires.
func isPrefixOf(prevMembers, target []Server) bool {
	if len(prevMembers) > len(target) {
		return false
	}
	return serversEqual(prevMembers, target[:len(prevMembers)])
}

// serversEqual reports whether a and b name+address-match pairwise (the
// only fields that identify a server for survivor/prefix purposes here).
func serversEqual(a, b []Server) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Address != b[i].Address {
			return false
		}
	}
	return true
}

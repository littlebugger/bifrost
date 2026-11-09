package config

import (
	"net/netip"
	"reflect"
	"sort"
	"testing"
)

// dnsTestBase returns a small base Config: pool "routes" with one static
// server and one server_dns template ("node", svc.local:587) whose fields
// (weight/backup/max_transactions/check incl. auth creds) every expanded
// member must inherit verbatim.
func dnsTestBase() *Config {
	return &Config{
		Pools: []Pool{
			{
				Name: "routes",
				Servers: []Server{
					{Name: "static-extra", Address: "10.9.8.7:587", Weight: 1},
				},
				DNSTemplates: []ServerDNS{
					{
						Name:            "node",
						Address:         "svc.local:587",
						Weight:          3,
						Backup:          true,
						MaxTransactions: 200,
						Check: CheckParams{
							Level:        "banner",
							AuthUsername: "rttskr-team",
							AuthPassword: "pa55w0rd",
						},
					},
				},
			},
		},
	}
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func nodeKey() TemplateKey { return TemplateKey{Pool: "routes", Template: "node"} }

// TestExpandDNSMembersSortedByAddrCompareNotLexical proves members land in
// the Servers slice ordered by netip.Addr.Compare: fed .11 before .2 (which
// would sort the OTHER way lexically), the expansion must still place
// node-192.0.2.2 before node-192.0.2.11.
func TestExpandDNSMembersSortedByAddrCompareNotLexical(t *testing.T) {
	base := dnsTestBase()
	members := map[TemplateKey][]netip.Addr{
		nodeKey(): addrs("192.0.2.11", "192.0.2.2"),
	}

	out := ExpandDNS(base, members)

	routes := poolNamed(out.Pools, "routes")
	if routes == nil {
		t.Fatalf("no pool %q in expanded config", "routes")
	}
	// index 0 is the static server; members follow.
	if len(routes.Servers) != 3 {
		t.Fatalf("len(routes.Servers) = %d, want 3 (1 static + 2 members)", len(routes.Servers))
	}
	if got, want := routes.Servers[1].Name, "node-192.0.2.2"; got != want {
		t.Errorf("routes.Servers[1].Name = %q, want %q (sorted ascending)", got, want)
	}
	if got, want := routes.Servers[2].Name, "node-192.0.2.11"; got != want {
		t.Errorf("routes.Servers[2].Name = %q, want %q (sorted ascending)", got, want)
	}
}

// TestExpandDNSMemberFieldsClonedFromTemplate proves a member inherits the
// whole template verbatim: weight, backup, max_transactions, and the
// resolved Check (auth creds included).
func TestExpandDNSMemberFieldsClonedFromTemplate(t *testing.T) {
	base := dnsTestBase()
	members := map[TemplateKey][]netip.Addr{nodeKey(): addrs("192.0.2.2")}

	out := ExpandDNS(base, members)
	routes := poolNamed(out.Pools, "routes")
	member := serverNamed(routes.Servers, "node-192.0.2.2")
	if member == nil {
		t.Fatalf("no member %q", "node-192.0.2.2")
	}

	tmpl := base.Pools[0].DNSTemplates[0]
	if member.Weight != tmpl.Weight {
		t.Errorf("member.Weight = %d, want %d", member.Weight, tmpl.Weight)
	}
	if member.Backup != tmpl.Backup {
		t.Errorf("member.Backup = %v, want %v", member.Backup, tmpl.Backup)
	}
	if member.MaxTransactions != tmpl.MaxTransactions {
		t.Errorf("member.MaxTransactions = %d, want %d", member.MaxTransactions, tmpl.MaxTransactions)
	}
	if !reflect.DeepEqual(member.Check, tmpl.Check) {
		t.Errorf("member.Check = %+v, want %+v (cloned from template, auth creds included)", member.Check, tmpl.Check)
	}
	if want := "192.0.2.2:587"; member.Address != want {
		t.Errorf("member.Address = %q, want %q (port from template address)", member.Address, want)
	}
	if member.DNSTemplate != tmpl.Name {
		t.Errorf("member.DNSTemplate = %q, want %q (the template name)", member.DNSTemplate, tmpl.Name)
	}
}

// TestExpandDNSStaticServerHasNoTemplateMarker proves a static server
// (not expanded from any server_dns block) keeps DNSTemplate empty --
// admin's /servers row builder (Task 5) reads this field to decide
// "discovered": true/absent.
func TestExpandDNSStaticServerHasNoTemplateMarker(t *testing.T) {
	base := dnsTestBase()
	out := ExpandDNS(base, map[TemplateKey][]netip.Addr{nodeKey(): addrs("192.0.2.2")})
	routes := poolNamed(out.Pools, "routes")
	static := serverNamed(routes.Servers, "static-extra")
	if static == nil {
		t.Fatalf("no server %q", "static-extra")
	}
	if static.DNSTemplate != "" {
		t.Errorf("static.DNSTemplate = %q, want empty", static.DNSTemplate)
	}
}

// TestExpandDNSStaticServersPrecedeMembers proves the pool's static
// servers always come first, with discovered members appended after.
func TestExpandDNSStaticServersPrecedeMembers(t *testing.T) {
	base := dnsTestBase()
	members := map[TemplateKey][]netip.Addr{nodeKey(): addrs("192.0.2.2")}

	out := ExpandDNS(base, members)
	routes := poolNamed(out.Pools, "routes")
	if len(routes.Servers) != 2 {
		t.Fatalf("len(routes.Servers) = %d, want 2", len(routes.Servers))
	}
	if routes.Servers[0].Name != "static-extra" {
		t.Errorf("routes.Servers[0].Name = %q, want the static server first", routes.Servers[0].Name)
	}
	if routes.Servers[1].Name != "node-192.0.2.2" {
		t.Errorf("routes.Servers[1].Name = %q, want the discovered member after it", routes.Servers[1].Name)
	}
}

// TestExpandDNSContinuityIsNameBasedNotPointerBased is the (amended) hard
// rule: ExpandDNS is a plain rebuild. A member whose membership persists
// across two calls keeps the same name/address (the key internal/health
// and internal/proxy's continuity actually ride on) but NEVER the same
// *Server — every call hands out entirely fresh objects, even for
// unchanged members. This documents the rebuild semantics the survivor-
// pointer-reuse mechanism was deliberately dropped in favor of.
func TestExpandDNSContinuityIsNameBasedNotPointerBased(t *testing.T) {
	base := dnsTestBase()

	gen1 := ExpandDNS(base, map[TemplateKey][]netip.Addr{
		nodeKey(): addrs("192.0.2.2"),
	})
	gen2 := ExpandDNS(base, map[TemplateKey][]netip.Addr{
		nodeKey(): addrs("192.0.2.2", "192.0.2.11"),
	})

	r1 := poolNamed(gen1.Pools, "routes")
	r2 := poolNamed(gen2.Pools, "routes")

	persisted1 := serverNamed(r1.Servers, "node-192.0.2.2")
	persisted2 := serverNamed(r2.Servers, "node-192.0.2.2")
	if persisted1 == nil || persisted2 == nil {
		t.Fatalf("persisted member missing: gen1=%v gen2=%v", persisted1, persisted2)
	}
	// Name-based continuity: same logical member, same Name/Address/fields.
	if persisted1.Name != persisted2.Name || persisted1.Address != persisted2.Address {
		t.Errorf("persisted member's identity drifted: gen1=%+v gen2=%+v", persisted1, persisted2)
	}
	if !reflect.DeepEqual(*persisted1, *persisted2) {
		t.Errorf("persisted member's cloned fields drifted: gen1=%+v gen2=%+v", *persisted1, *persisted2)
	}
	// NOT pointer-based: ExpandDNS never reuses a *Server across calls.
	if persisted1 == persisted2 {
		t.Errorf("persisted member kept the same *Server pointer across calls; ExpandDNS must always rebuild fresh")
	}

	added := serverNamed(r2.Servers, "node-192.0.2.11")
	if added == nil {
		t.Fatalf("no member %q in gen2", "node-192.0.2.11")
	}
	if added == persisted1 || added == persisted2 {
		t.Errorf("freshly added member aliased an existing *Server pointer")
	}
}

// TestExpandDNSTruncatesAt256SortedTailDropped proves a template with more
// than 256 resolved addresses is capped at 256, keeping the 256 smallest
// (by netip.Addr.Compare) and dropping the sorted tail — deterministically,
// regardless of the input slice's own order.
func TestExpandDNSTruncatesAt256SortedTailDropped(t *testing.T) {
	base := dnsTestBase()

	const total = 300
	all := make([]netip.Addr, total)
	for i := 0; i < total; i++ {
		all[i] = netip.AddrFrom4([4]byte{10, 0, byte(i / 256), byte(i % 256)})
	}
	shuffled := make([]netip.Addr, total)
	copy(shuffled, all)
	sort.Slice(shuffled, func(i, j int) bool { return shuffled[i].String() > shuffled[j].String() })

	out := ExpandDNS(base, map[TemplateKey][]netip.Addr{nodeKey(): shuffled})
	routes := poolNamed(out.Pools, "routes")
	members := routes.Servers[1:] // after the one static server
	if len(members) != 256 {
		t.Fatalf("len(members) = %d, want 256 (truncated)", len(members))
	}
	if got, want := members[0].Name, "node-"+all[0].String(); got != want {
		t.Errorf("members[0].Name = %q, want %q (smallest survives)", got, want)
	}
	if got, want := members[255].Name, "node-"+all[255].String(); got != want {
		t.Errorf("members[255].Name = %q, want %q (256th smallest survives)", got, want)
	}
	if serverNamed(members, "node-"+all[256].String()) != nil {
		t.Errorf("the 257th-smallest address survived truncation, want it dropped")
	}
}

// TestExpandDNSZeroMembersExpandsToNothing proves a template with no
// resolved addresses (an absent or empty map entry) contributes no
// members: the pool's Servers is exactly its static servers.
func TestExpandDNSZeroMembersExpandsToNothing(t *testing.T) {
	base := dnsTestBase()
	out := ExpandDNS(base, map[TemplateKey][]netip.Addr{})
	routes := poolNamed(out.Pools, "routes")
	if len(routes.Servers) != 1 {
		t.Fatalf("len(routes.Servers) = %d, want 1 (static only, template unexpanded)", len(routes.Servers))
	}
	if routes.Servers[0].Name != "static-extra" {
		t.Errorf("routes.Servers[0].Name = %q, want %q", routes.Servers[0].Name, "static-extra")
	}
}

// TestExpandDNSBaseNeverMutated proves ExpandDNS never writes through
// base: a deep copy taken before the call still deep-equals base after it,
// across two successive expansions.
func TestExpandDNSBaseNeverMutated(t *testing.T) {
	base := dnsTestBase()
	before := deepCopyConfigForTest(t, base)

	ExpandDNS(base, map[TemplateKey][]netip.Addr{nodeKey(): addrs("192.0.2.2")})
	ExpandDNS(base, map[TemplateKey][]netip.Addr{
		nodeKey(): addrs("192.0.2.2", "192.0.2.11"),
	})

	if !reflect.DeepEqual(before, base) {
		t.Errorf("base mutated by ExpandDNS:\n before=%+v\n after=%+v", before, base)
	}
}

// deepCopyConfigForTest returns a deep copy of cfg's pools/servers/templates
// good enough for TestExpandDNSBaseNeverMutated's before/after comparison
// (no hcl.Range content in these hand-built fixtures to worry about).
func deepCopyConfigForTest(t *testing.T, cfg *Config) *Config {
	t.Helper()
	cp := *cfg
	cp.Pools = make([]Pool, len(cfg.Pools))
	for i, p := range cfg.Pools {
		p.Servers = append([]Server(nil), p.Servers...)
		p.DNSTemplates = append([]ServerDNS(nil), p.DNSTemplates...)
		cp.Pools[i] = p
	}
	return &cp
}

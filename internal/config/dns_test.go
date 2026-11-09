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

	out := ExpandDNS(base, nil, members)

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

	out := ExpandDNS(base, nil, members)
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
}

// TestExpandDNSStaticServersPrecedeMembers proves the pool's static
// servers always come first, with discovered members appended after.
func TestExpandDNSStaticServersPrecedeMembers(t *testing.T) {
	base := dnsTestBase()
	members := map[TemplateKey][]netip.Addr{nodeKey(): addrs("192.0.2.2")}

	out := ExpandDNS(base, nil, members)
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

// TestExpandDNSSurvivorKeepsPointerAcrossAdd is the hard rule: a second
// expansion that only ADDS a member must reuse the exact *Server for the
// unchanged IP, and hand out a fresh one for the added IP.
func TestExpandDNSSurvivorKeepsPointerAcrossAdd(t *testing.T) {
	base := dnsTestBase()

	expanded1 := ExpandDNS(base, nil, map[TemplateKey][]netip.Addr{
		nodeKey(): addrs("192.0.2.2"),
	})
	expanded2 := ExpandDNS(base, expanded1, map[TemplateKey][]netip.Addr{
		nodeKey(): addrs("192.0.2.2", "192.0.2.11"),
	})

	r1 := poolNamed(expanded1.Pools, "routes")
	r2 := poolNamed(expanded2.Pools, "routes")

	survivor1 := serverNamed(r1.Servers, "node-192.0.2.2")
	survivor2 := serverNamed(r2.Servers, "node-192.0.2.2")
	if survivor1 == nil || survivor2 == nil {
		t.Fatalf("survivor member missing: gen1=%v gen2=%v", survivor1, survivor2)
	}
	if survivor1 != survivor2 {
		t.Errorf("survivor *Server changed identity across an add-only delta: %p != %p", survivor1, survivor2)
	}

	added := serverNamed(r2.Servers, "node-192.0.2.11")
	if added == nil {
		t.Fatalf("no member %q in gen2", "node-192.0.2.11")
	}
	if added == survivor1 {
		t.Errorf("freshly added member reused an existing *Server pointer")
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

	out := ExpandDNS(base, nil, map[TemplateKey][]netip.Addr{nodeKey(): shuffled})
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

// TestExpandDNSNilPrev proves a nil prev (first-ever expansion) builds the
// snapshot without panicking and with no survivor to reuse.
func TestExpandDNSNilPrev(t *testing.T) {
	base := dnsTestBase()
	out := ExpandDNS(base, nil, map[TemplateKey][]netip.Addr{nodeKey(): addrs("192.0.2.2")})
	routes := poolNamed(out.Pools, "routes")
	if serverNamed(routes.Servers, "node-192.0.2.2") == nil {
		t.Fatalf("expected member node-192.0.2.2 with a nil prev")
	}
}

// TestExpandDNSZeroMembersExpandsToNothing proves a template with no
// resolved addresses (an absent or empty map entry) contributes no
// members: the pool's Servers is exactly its static servers.
func TestExpandDNSZeroMembersExpandsToNothing(t *testing.T) {
	base := dnsTestBase()
	out := ExpandDNS(base, nil, map[TemplateKey][]netip.Addr{})
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
// across a chain of two expansions (nil prev, then a non-nil prev).
func TestExpandDNSBaseNeverMutated(t *testing.T) {
	base := dnsTestBase()
	before := deepCopyConfigForTest(t, base)

	expanded1 := ExpandDNS(base, nil, map[TemplateKey][]netip.Addr{nodeKey(): addrs("192.0.2.2")})
	ExpandDNS(base, expanded1, map[TemplateKey][]netip.Addr{
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

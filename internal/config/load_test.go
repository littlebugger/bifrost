package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// findDiag returns the first diagnostic whose rendered message contains
// substr, or nil.
func findDiag(diags hcl.Diagnostics, substr string) *hcl.Diagnostic {
	for _, d := range diags {
		if strings.Contains(d.Error(), substr) {
			return d
		}
	}
	return nil
}

func listenerNamed(listeners []Listener, name string) *Listener {
	for i := range listeners {
		if listeners[i].Name == name {
			return &listeners[i]
		}
	}
	return nil
}

func poolNamed(pools []Pool, name string) *Pool {
	for i := range pools {
		if pools[i].Name == name {
			return &pools[i]
		}
	}
	return nil
}

func serverNamed(servers []Server, name string) *Server {
	for i := range servers {
		if servers[i].Name == name {
			return &servers[i]
		}
	}
	return nil
}

func serverDNSNamed(templates []ServerDNS, name string) *ServerDNS {
	for i := range templates {
		if templates[i].Name == name {
			return &templates[i]
		}
	}
	return nil
}

// mustLoad loads path and fails the test if it decodes with any error
// diagnostic (warnings, e.g. the missing-admin-block warning, are fine).
func mustLoad(t *testing.T, path string) *Config {
	t.Helper()
	cfg, diags := Load(path)
	if diags.HasErrors() {
		t.Fatalf("Load(%s) diags = %v", path, diags)
	}
	if cfg == nil {
		t.Fatalf("Load(%s) returned nil Config with no error diags", path)
	}
	return cfg
}

func TestLoadExample(t *testing.T) {
	cfg, diags := Load("../../examples/bifrost.hcl")
	if diags.HasErrors() {
		t.Fatalf("Load(examples/bifrost.hcl) diags = %v", diags)
	}
	if cfg == nil {
		t.Fatalf("Load(examples/bifrost.hcl) returned nil Config with no error diags")
	}

	if got := cfg.Defaults.EhloName; got != "mail.example.com" {
		t.Errorf("Defaults.EhloName = %q, want %q", got, "mail.example.com")
	}
	wantTimeouts := Timeouts{
		ClientIdle:       300 * time.Second,
		SessionMax:       time.Hour,
		BackendConnect:   5 * time.Second,
		BackendHandshake: 15 * time.Second,
		BackendMailReply: 30 * time.Second,
		Backend354Wait:   60 * time.Second,
		DataProgress:     60 * time.Second,
		BackendFinalDot:  600 * time.Second,
		LameDuck:         2 * time.Second,
		DrainTimeout:     30 * time.Second,
	}
	got := cfg.Defaults.Timeouts
	got.rng = hcl.Range{}
	got.fieldRanges = nil
	if !reflect.DeepEqual(got, wantTimeouts) {
		t.Errorf("Defaults.Timeouts = %+v, want %+v", got, wantTimeouts)
	}
	if cfg.Defaults.Check.Level != "ehlo" || cfg.Defaults.Check.Rise != 2 || cfg.Defaults.Check.Fall != 3 {
		t.Errorf("Defaults.Check = %+v, want level=ehlo rise=2 fall=3", cfg.Defaults.Check)
	}
	if cfg.Defaults.Check.Interval != 5*time.Second || cfg.Defaults.Check.DownInterval != 15*time.Second || cfg.Defaults.Check.Timeout != 5*time.Second {
		t.Errorf("Defaults.Check timings = %+v, want interval=5s down_interval=15s timeout=5s", cfg.Defaults.Check)
	}

	if cfg.PrimaryListener().Name != "default" {
		t.Errorf("Listener.Name = %q, want %q for a single unlabeled listener block", cfg.PrimaryListener().Name, "default")
	}
	if cfg.PrimaryListener().Bind != "0.0.0.0:25" || cfg.PrimaryListener().Hostname != "mail.example.com" {
		t.Errorf("Listener = %+v, want bind=0.0.0.0:25 hostname=mail.example.com", cfg.PrimaryListener())
	}
	if cfg.PrimaryListener().StartTLS == nil {
		t.Fatalf("Listener.StartTLS = nil, want a starttls block")
	}
	if !strings.HasSuffix(cfg.PrimaryListener().StartTLS.Cert, "examples/server.crt") {
		t.Errorf("StartTLS.Cert = %q, want it resolved under examples/", cfg.PrimaryListener().StartTLS.Cert)
	}
	if !strings.HasSuffix(cfg.PrimaryListener().StartTLS.Key, "examples/server.key") {
		t.Errorf("StartTLS.Key = %q, want it resolved under examples/", cfg.PrimaryListener().StartTLS.Key)
	}
	if cfg.PrimaryListener().StartTLS.MinVersion != "1.2" {
		t.Errorf("StartTLS.MinVersion = %q, want 1.2", cfg.PrimaryListener().StartTLS.MinVersion)
	}
	wantCaps := []string{"PIPELINING", "8BITMIME", "SIZE 10485760", "STARTTLS"}
	if strings.Join(cfg.PrimaryListener().Capabilities, ",") != strings.Join(wantCaps, ",") {
		t.Errorf("Listener.Capabilities = %v, want %v", cfg.PrimaryListener().Capabilities, wantCaps)
	}

	if len(cfg.Pools) != 2 {
		t.Fatalf("len(Pools) = %d, want 2", len(cfg.Pools))
	}
	internal := poolNamed(cfg.Pools, "internal")
	if internal == nil {
		t.Fatalf("no pool named %q", "internal")
	}
	if internal.Balance != "roundrobin" || internal.BackendTLS != "none" {
		t.Errorf("pool internal = %+v, want balance=roundrobin backend_tls=none", internal)
	}
	if internal.EhloName != "internal.mail.example.com" {
		t.Errorf("pool internal EhloName = %q, want %q", internal.EhloName, "internal.mail.example.com")
	}
	if len(internal.Servers) != 3 {
		t.Fatalf("len(internal.Servers) = %d, want 3", len(internal.Servers))
	}
	mta1 := serverNamed(internal.Servers, "mta1")
	if mta1 == nil || mta1.Address != "192.0.2.11:25" || mta1.Weight != 3 {
		t.Errorf("server mta1 = %+v, want address=192.0.2.11:25 weight=3", mta1)
	}
	mta2 := serverNamed(internal.Servers, "mta2")
	if mta2 == nil || mta2.Weight != 1 {
		t.Errorf("server mta2 = %+v, want weight=1", mta2)
	}
	spare := serverNamed(internal.Servers, "spare")
	if spare == nil || !spare.Backup {
		t.Fatalf("server spare = %+v, want backup=true", spare)
	}
	if spare.Check.Level != "connect" || spare.Check.Port != 9025 {
		t.Errorf("server spare Check = %+v, want level=connect port=9025", spare.Check)
	}

	bulk := poolNamed(cfg.Pools, "bulk")
	if bulk == nil {
		t.Fatalf("no pool named %q", "bulk")
	}
	if bulk.Balance != "leastconn" || bulk.MaxTransactions != 500 {
		t.Errorf("pool bulk = %+v, want balance=leastconn max_transactions=500", bulk)
	}
	if bulk.BackendTLS != "starttls-verify" || bulk.BackendTLSServerName != "mail.bulk.example.com" {
		t.Errorf("pool bulk TLS = %+v, want backend_tls=starttls-verify server_name=mail.bulk.example.com", bulk)
	}
	if !strings.HasSuffix(bulk.BackendTLSCA, "examples/server.crt") {
		t.Errorf("pool bulk BackendTLSCA = %q, want it resolved under examples/", bulk.BackendTLSCA)
	}
	if len(bulk.Servers) != 1 || bulk.Servers[0].Weight != 1 {
		t.Errorf("bulk.Servers = %+v, want one server with weight=1", bulk.Servers)
	}

	if len(cfg.Routing.Rules) != 2 {
		t.Fatalf("len(Routing.Rules) = %d, want 2", len(cfg.Routing.Rules))
	}
	r0, r1 := cfg.Routing.Rules[0], cfg.Routing.Rules[1]
	if len(r0.ClientCIDR) != 1 || r0.ClientCIDR[0] != "10.0.0.0/8" || r0.Pool != "internal" {
		t.Errorf("Routing.Rules[0] = %+v, want client_cidr=[10.0.0.0/8] pool=internal", r0)
	}
	if len(r1.MailFromDomain) != 1 || r1.MailFromDomain[0] != "*.news.example.com" || r1.Pool != "bulk" {
		t.Errorf("Routing.Rules[1] = %+v, want mail_from_domain=[*.news.example.com] pool=bulk", r1)
	}
	if cfg.Routing.DefaultPool != "internal" {
		t.Errorf("Routing.DefaultPool = %q, want %q", cfg.Routing.DefaultPool, "internal")
	}

	if cfg.Limits.GlobalMaxConn != 2048 {
		t.Errorf("Limits.GlobalMaxConn = %d, want 2048", cfg.Limits.GlobalMaxConn)
	}

	if cfg.Admin == nil {
		t.Fatalf("Admin = nil, want the loopback admin block")
	}
	if cfg.Admin.Bind != "127.0.0.1:8081" || cfg.Admin.AllowRemote {
		t.Errorf("Admin = %+v, want bind=127.0.0.1:8081 allow_remote=false", cfg.Admin)
	}
}

func TestLoadAuth(t *testing.T) {
	cfg := mustLoad(t, "testdata/auth.hcl")
	la := cfg.PrimaryListener().Auth
	if la == nil || len(la.Users) != 1 {
		t.Fatalf("Listener.Auth = %+v, want 1 user", la)
	}
	u := la.Users[0]
	if u.Name != "rttskr-team" || u.Salt != "aa11" ||
		u.HashedPassword != strings.ToLower("ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"[:64]) {
		t.Errorf("user = %+v (hash must be lowercased at load)", u)
	}
	outgoing := poolNamed(cfg.Pools, "outgoing")
	if outgoing == nil || outgoing.Auth == nil ||
		outgoing.Auth.Username != "rttskr-team" || outgoing.Auth.Password != "pa55w0rd" {
		t.Fatalf("pool auth = %+v", outgoing)
	}
	if outgoing.ReuseEnvelopes != 50 {
		t.Errorf("pool outgoing ReuseEnvelopes = %d, want 50", outgoing.ReuseEnvelopes)
	}
	if la.AllowCleartext {
		t.Errorf("Listener.Auth.AllowCleartext = true, want false (default, no knob set)")
	}
	if outgoing.Auth.AllowCleartext {
		t.Errorf("pool auth AllowCleartext = true, want false (default, no knob set)")
	}
}

// TestLoadAuthCleartext proves allow_cleartext decodes true on both auth
// legs, and that the pool's copy of the knob into CheckParams (the same
// path AuthUsername/AuthPassword take, see resolvePoolAuth) reaches the
// probe layer too. auth-cleartext.hcl pairs the knob with what would
// otherwise be load errors (no starttls block, backend_tls = "none") —
// loading clean at all proves both guards were actually lifted.
func TestLoadAuthCleartext(t *testing.T) {
	cfg := mustLoad(t, "testdata/auth-cleartext.hcl")
	if cfg.PrimaryListener().Auth == nil || !cfg.PrimaryListener().Auth.AllowCleartext {
		t.Fatalf("Listener.Auth = %+v, want AllowCleartext = true", cfg.PrimaryListener().Auth)
	}
	outgoing := poolNamed(cfg.Pools, "outgoing")
	if outgoing == nil || outgoing.Auth == nil || !outgoing.Auth.AllowCleartext {
		t.Fatalf("pool auth = %+v, want AllowCleartext = true", outgoing)
	}
	if !outgoing.Check.AuthAllowCleartext {
		t.Errorf("pool Check.AuthAllowCleartext = false, want true (resolved from pool.auth like AuthUsername/AuthPassword)")
	}
	if len(outgoing.Servers) == 0 || !outgoing.Servers[0].Check.AuthAllowCleartext {
		t.Errorf("server Check.AuthAllowCleartext = false, want true")
	}
}

// TestLoadMultiListener proves two labeled listener blocks decode as
// distinct Config.Listeners entries (rather than gohcl rejecting the
// second as an unexpected duplicate block, or silently overwriting the
// first), each keeping its own fields, and that a routing rule's
// listener attribute decodes as the matching listener names (Task 2
// makes that list drive actual routing; this only proves the decode).
func TestLoadMultiListener(t *testing.T) {
	cfg := mustLoad(t, "testdata/multi-listener.hcl")
	if len(cfg.Listeners) != 2 {
		t.Fatalf("len(Listeners) = %d, want 2", len(cfg.Listeners))
	}

	transactional := listenerNamed(cfg.Listeners, "transactional")
	if transactional == nil {
		t.Fatalf("no listener named %q", "transactional")
	}
	if transactional.Hostname != "transactional.bifrost.test" {
		t.Errorf("transactional.Hostname = %q, want %q", transactional.Hostname, "transactional.bifrost.test")
	}
	if transactional.Auth != nil {
		t.Errorf("transactional.Auth = %+v, want nil", transactional.Auth)
	}

	bulk := listenerNamed(cfg.Listeners, "bulk")
	if bulk == nil {
		t.Fatalf("no listener named %q", "bulk")
	}
	if bulk.Hostname != "bulk.bifrost.test" {
		t.Errorf("bulk.Hostname = %q, want %q", bulk.Hostname, "bulk.bifrost.test")
	}
	if bulk.StartTLS == nil {
		t.Fatalf("bulk.StartTLS = nil, want the fixture's cert/key")
	}
	if bulk.Auth == nil || len(bulk.Auth.Users) != 1 {
		t.Fatalf("bulk.Auth = %+v, want 1 user", bulk.Auth)
	}

	if len(cfg.Routing.Rules) != 1 {
		t.Fatalf("len(Routing.Rules) = %d, want 1", len(cfg.Routing.Rules))
	}
	if want := []string{"bulk"}; !reflect.DeepEqual(cfg.Routing.Rules[0].Listener, want) {
		t.Errorf("Routing.Rules[0].Listener = %v, want %v", cfg.Routing.Rules[0].Listener, want)
	}
}

// TestLoadAuthPasswordFile proves password_file resolves relative to the
// config file (like backend_tls_ca), gets trimmed of its trailing
// newline, and reaches CheckParams.AuthPassword — i.e. the file read runs
// before resolvePoolAuth's CheckParams copy, so the health prober gets
// the same password as the relay.
func TestLoadAuthPasswordFile(t *testing.T) {
	cfg := mustLoad(t, "testdata/auth-password-file.hcl")
	outgoing := poolNamed(cfg.Pools, "outgoing")
	if outgoing == nil || outgoing.Auth == nil || outgoing.Auth.Password != "pa55w0rd" {
		t.Fatalf("pool auth = %+v, want Password = %q", outgoing, "pa55w0rd")
	}
	if len(outgoing.Servers) == 0 || outgoing.Servers[0].Check.AuthPassword != "pa55w0rd" {
		t.Fatalf("Servers[0].Check.AuthPassword = %q, want %q", outgoing.Servers[0].Check.AuthPassword, "pa55w0rd")
	}
}

// TestLoadServerDNS proves a server_dns template decodes every field, rides
// the same defaults cascade as a Server (pool tier Check, pool auth creds
// copied into Check), and that a pool consisting of only a template (zero
// static servers) still validates.
func TestLoadServerDNS(t *testing.T) {
	cfg := mustLoad(t, "testdata/dns-template.hcl")

	routes := poolNamed(cfg.Pools, "routes")
	if routes == nil {
		t.Fatalf("no pool named %q", "routes")
	}
	if len(routes.DNSTemplates) != 1 {
		t.Fatalf("len(routes.DNSTemplates) = %d, want 1", len(routes.DNSTemplates))
	}
	node := serverDNSNamed(routes.DNSTemplates, "node")
	if node == nil {
		t.Fatalf("no server_dns template named %q", "node")
	}
	if node.Address != "mta.headless.svc.cluster.local:587" {
		t.Errorf("node.Address = %q, want %q", node.Address, "mta.headless.svc.cluster.local:587")
	}
	if node.ResolveInterval != 45*time.Second {
		t.Errorf("node.ResolveInterval = %s, want 45s", node.ResolveInterval)
	}
	if node.Weight != 3 {
		t.Errorf("node.Weight = %d, want 3", node.Weight)
	}
	if !node.Backup {
		t.Errorf("node.Backup = false, want true")
	}
	if node.MaxTransactions != 200 {
		t.Errorf("node.MaxTransactions = %d, want 200", node.MaxTransactions)
	}
	if node.Check.Level != "banner" {
		t.Errorf("node.Check.Level = %q, want %q (from the template's own check block)", node.Check.Level, "banner")
	}
	if node.Check.TLS != "starttls" {
		t.Errorf("node.Check.TLS = %q, want %q (resolved from pool.backend_tls)", node.Check.TLS, "starttls")
	}
	if node.Check.AuthUsername != "rttskr-team" || node.Check.AuthPassword != "pa55w0rd" {
		t.Errorf("node.Check auth = %+v, want pool auth creds copied in (mirroring servers)", node.Check)
	}

	extra := serverNamed(routes.Servers, "static-extra")
	if extra == nil {
		t.Fatalf("no server named %q (templates must not displace static servers)", "static-extra")
	}

	dnsOnly := poolNamed(cfg.Pools, "dns-only")
	if dnsOnly == nil {
		t.Fatalf("no pool named %q", "dns-only")
	}
	if len(dnsOnly.Servers) != 0 {
		t.Errorf("dns-only.Servers = %+v, want none", dnsOnly.Servers)
	}
	if len(dnsOnly.DNSTemplates) != 1 {
		t.Fatalf("len(dns-only.DNSTemplates) = %d, want 1", len(dnsOnly.DNSTemplates))
	}
	if got := dnsOnly.DNSTemplates[0].ResolveInterval; got != 30*time.Second {
		t.Errorf("dns-only template ResolveInterval = %s, want the 30s built-in default", got)
	}
}

func TestLoadUnknownAttribute(t *testing.T) {
	cfg, diags := Load("testdata/unknown-attribute.hcl")
	if !diags.HasErrors() {
		t.Fatalf("Load(unknown-attribute.hcl) diags = %v, want an error", diags)
	}
	if cfg != nil {
		t.Errorf("Load(unknown-attribute.hcl) cfg = %+v, want nil on error", cfg)
	}
	d := findDiag(diags, "wieght")
	if d == nil {
		t.Fatalf("diags = %v, want one mentioning %q", diags, "wieght")
	}
	if d.Subject == nil {
		t.Fatalf("diagnostic %v has no Subject range", d)
	}
	const wantLine = 6
	if d.Subject.Start.Line != wantLine {
		t.Errorf("diagnostic line = %d, want %d (%v)", d.Subject.Start.Line, wantLine, d)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	cfg, diags := Load("testdata/missing-required.hcl")
	if !diags.HasErrors() {
		t.Fatalf("Load(missing-required.hcl) diags = %v, want an error", diags)
	}
	if cfg != nil {
		t.Errorf("Load(missing-required.hcl) cfg = %+v, want nil on error", cfg)
	}
	d := findDiag(diags, "address")
	if d == nil {
		t.Fatalf("diags = %v, want one mentioning %q", diags, "address")
	}
}

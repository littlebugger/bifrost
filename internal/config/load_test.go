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

	if cfg.Listener.Bind != "0.0.0.0:25" || cfg.Listener.Hostname != "mail.example.com" {
		t.Errorf("Listener = %+v, want bind=0.0.0.0:25 hostname=mail.example.com", cfg.Listener)
	}
	if cfg.Listener.StartTLS == nil {
		t.Fatalf("Listener.StartTLS = nil, want a starttls block")
	}
	if !strings.HasSuffix(cfg.Listener.StartTLS.Cert, "examples/server.crt") {
		t.Errorf("StartTLS.Cert = %q, want it resolved under examples/", cfg.Listener.StartTLS.Cert)
	}
	if !strings.HasSuffix(cfg.Listener.StartTLS.Key, "examples/server.key") {
		t.Errorf("StartTLS.Key = %q, want it resolved under examples/", cfg.Listener.StartTLS.Key)
	}
	if cfg.Listener.StartTLS.MinVersion != "1.2" {
		t.Errorf("StartTLS.MinVersion = %q, want 1.2", cfg.Listener.StartTLS.MinVersion)
	}
	wantCaps := []string{"PIPELINING", "8BITMIME", "SIZE 10485760", "STARTTLS"}
	if strings.Join(cfg.Listener.Capabilities, ",") != strings.Join(wantCaps, ",") {
		t.Errorf("Listener.Capabilities = %v, want %v", cfg.Listener.Capabilities, wantCaps)
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
	la := cfg.Listener.Auth
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

package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// TestBuiltinDefaults loads a config that omits every optional attribute
// and checks every resolved value against PROJECT.md's tables verbatim.
func TestBuiltinDefaults(t *testing.T) {
	cfg, diags := Load("testdata/minimal-builtin-defaults.hcl")
	if diags.HasErrors() {
		t.Fatalf("Load(minimal-builtin-defaults.hcl) diags = %v", diags)
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

	// Capabilities omitted entirely: the built-in safe set, and no
	// STARTTLS because this fixture configures no certificate.
	wantCaps := []string{"PIPELINING", "8BITMIME"}
	if !reflect.DeepEqual(cfg.Listener.Capabilities, wantCaps) {
		t.Errorf("Listener.Capabilities = %q, want %q", cfg.Listener.Capabilities, wantCaps)
	}

	if cfg.Limits.GlobalMaxConn != 1024 {
		t.Errorf("Limits.GlobalMaxConn = %d, want 1024", cfg.Limits.GlobalMaxConn)
	}
	if cfg.Limits.MaxTransactions != 0 {
		t.Errorf("Limits.MaxTransactions = %d, want 0 (unlimited)", cfg.Limits.MaxTransactions)
	}

	pool := poolNamed(cfg.Pools, "internal")
	if pool == nil {
		t.Fatalf("no pool named %q", "internal")
	}
	if pool.BackendTLS != "none" {
		t.Errorf("Pool.BackendTLS = %q, want %q", pool.BackendTLS, "none")
	}
	if pool.MaxTransactions != 0 {
		t.Errorf("Pool.MaxTransactions = %d, want 0 (unlimited)", pool.MaxTransactions)
	}

	server := serverNamed(pool.Servers, "mta1")
	if server == nil {
		t.Fatalf("no server named %q", "mta1")
	}
	if server.Weight != 1 {
		t.Errorf("Server.Weight = %d, want 1", server.Weight)
	}
	if server.MaxTransactions != 0 {
		t.Errorf("Server.MaxTransactions = %d, want 0 (unlimited)", server.MaxTransactions)
	}

	ck := server.Check
	if ck.Level != "ehlo" {
		t.Errorf("Check.Level = %q, want %q", ck.Level, "ehlo")
	}
	if ck.Port != 0 {
		t.Errorf("Check.Port = %d, want 0 (traffic port)", ck.Port)
	}
	if ck.Interval != 5*time.Second {
		t.Errorf("Check.Interval = %s, want 5s", ck.Interval)
	}
	if ck.DownInterval != 15*time.Second {
		t.Errorf("Check.DownInterval = %s, want 15s", ck.DownInterval)
	}
	if ck.Timeout != 5*time.Second {
		// Built-in invariant: timeout (5s) <= interval (5s) is valid.
		t.Errorf("Check.Timeout = %s, want 5s", ck.Timeout)
	}
	if ck.Rise != 2 {
		t.Errorf("Check.Rise = %d, want 2", ck.Rise)
	}
	if ck.Fall != 3 {
		t.Errorf("Check.Fall = %d, want 3", ck.Fall)
	}
}

// TestBuiltinDefaultsPassValidation proves the resolved built-in defaults
// never fail their own validator: check.timeout (5s) <= check.interval
// (5s) is the load-bearing invariant, and every RFC 5321 floor miss is a
// warning, never an error.
func TestBuiltinDefaultsPassValidation(t *testing.T) {
	cfg, diags := Load("testdata/minimal-builtin-defaults.hcl")
	if cfg == nil {
		t.Fatalf("Load(minimal-builtin-defaults.hcl) = nil cfg, diags = %v", diags)
	}
	if diags.HasErrors() {
		t.Fatalf("Load(minimal-builtin-defaults.hcl) diags = %v, want zero errors from the built-in defaults", diags)
	}

	// Validate is independently callable per its doc comment; calling it
	// again directly must agree with what Load already found.
	if vdiags := cfg.Validate(); vdiags.HasErrors() {
		t.Errorf("cfg.Validate() diags = %v, want zero errors", vdiags)
	}
}

// TestDefaultsToPoolToServer exercises the full check-param cascade
// (server override > pool override > defaults > built-in), TLS/EhloName
// bypassing the defaults tier in favor of the pool's own BackendTLS and
// (already-resolved) EhloName, and EhloName's own three-tier fallback:
// pool -> defaults.ehlo_name -> listener hostname.
func TestDefaultsToPoolToServer(t *testing.T) {
	cfg, diags := Load("testdata/inheritance-cascade.hcl")
	if diags.HasErrors() {
		t.Fatalf("Load(inheritance-cascade.hcl) diags = %v", diags)
	}

	p1 := poolNamed(cfg.Pools, "p1")
	if p1 == nil {
		t.Fatalf("no pool named %q", "p1")
	}
	if p1.EhloName != "defaults.example.com" {
		t.Errorf("pool p1 EhloName = %q, want defaults.ehlo_name %q", p1.EhloName, "defaults.example.com")
	}

	poolOverride := serverNamed(p1.Servers, "s-pool-override")
	if poolOverride == nil {
		t.Fatalf("no server named %q", "s-pool-override")
	}
	if poolOverride.Check.Interval != 10*time.Second || poolOverride.Check.Timeout != 10*time.Second {
		t.Errorf("s-pool-override Check interval/timeout = %s/%s, want 10s/10s (pool override)", poolOverride.Check.Interval, poolOverride.Check.Timeout)
	}
	if poolOverride.Check.DownInterval != 15*time.Second {
		t.Errorf("s-pool-override Check.DownInterval = %s, want 15s (from defaults, not overridden)", poolOverride.Check.DownInterval)
	}
	if poolOverride.Check.TLS != "starttls" {
		t.Errorf("s-pool-override Check.TLS = %q, want %q (from pool.backend_tls, bypassing defaults.check)", poolOverride.Check.TLS, "starttls")
	}
	if poolOverride.Check.EhloName != "defaults.example.com" {
		t.Errorf("s-pool-override Check.EhloName = %q, want %q (from pool.EhloName)", poolOverride.Check.EhloName, "defaults.example.com")
	}

	serverOverride := serverNamed(p1.Servers, "s-server-override")
	if serverOverride == nil {
		t.Fatalf("no server named %q", "s-server-override")
	}
	if serverOverride.Check.Interval != 20*time.Second || serverOverride.Check.Timeout != 20*time.Second {
		t.Errorf("s-server-override Check interval/timeout = %s/%s, want 20s/20s (server override beats pool's 10s)", serverOverride.Check.Interval, serverOverride.Check.Timeout)
	}
	if serverOverride.Check.Rise != 2 || serverOverride.Check.Fall != 3 {
		t.Errorf("s-server-override Check rise/fall = %d/%d, want 2/3 (from defaults)", serverOverride.Check.Rise, serverOverride.Check.Fall)
	}

	p2 := poolNamed(cfg.Pools, "p2")
	if p2 == nil {
		t.Fatalf("no pool named %q", "p2")
	}
	if p2.BackendTLS != "none" {
		t.Errorf("pool p2 BackendTLS = %q, want built-in %q", p2.BackendTLS, "none")
	}
	defaultsOnly := serverNamed(p2.Servers, "s-defaults-only")
	if defaultsOnly == nil {
		t.Fatalf("no server named %q", "s-defaults-only")
	}
	if defaultsOnly.Check.Interval != 5*time.Second || defaultsOnly.Check.Timeout != 5*time.Second {
		t.Errorf("s-defaults-only Check interval/timeout = %s/%s, want 5s/5s (from defaults)", defaultsOnly.Check.Interval, defaultsOnly.Check.Timeout)
	}
	if defaultsOnly.Check.TLS != "none" {
		t.Errorf("s-defaults-only Check.TLS = %q, want %q (pool p2's built-in backend_tls)", defaultsOnly.Check.TLS, "none")
	}
	if defaultsOnly.Check.EhloName != "defaults.example.com" {
		t.Errorf("s-defaults-only Check.EhloName = %q, want %q", defaultsOnly.Check.EhloName, "defaults.example.com")
	}

	// Third tier: with no defaults.ehlo_name and no pool-level ehlo_name,
	// Pool.EhloName must fall back to the listener's hostname.
	fallbackCfg, fdiags := Load("testdata/ehlo-name-listener-fallback.hcl")
	if fdiags.HasErrors() {
		t.Fatalf("Load(ehlo-name-listener-fallback.hcl) diags = %v", fdiags)
	}
	fp := poolNamed(fallbackCfg.Pools, "p1")
	if fp == nil {
		t.Fatalf("no pool named %q", "p1")
	}
	if fp.EhloName != "listener-fallback.example.com" {
		t.Errorf("pool EhloName = %q, want the listener hostname %q", fp.EhloName, "listener-fallback.example.com")
	}
}

// TestCapabilitiesDefaultWithCertAppendsStartTLS is the append half of the
// EHLO capability policy: with the attribute omitted, the built-in set is
// resolved AND STARTTLS is added because a certificate is configured
// (TestBuiltinDefaults covers the no-certificate half, TestLoadExample the
// explicit-list dedupe).
func TestCapabilitiesDefaultWithCertAppendsStartTLS(t *testing.T) {
	cfg, diags := Load("testdata/caps-omitted-with-cert.hcl")
	if diags.HasErrors() {
		t.Fatalf("Load(caps-omitted-with-cert.hcl) diags = %v", diags)
	}
	if cfg.Listener.StartTLS == nil {
		t.Fatal("Listener.StartTLS = nil, want the fixture's cert/key")
	}

	want := []string{"PIPELINING", "8BITMIME", "STARTTLS"}
	if !reflect.DeepEqual(cfg.Listener.Capabilities, want) {
		t.Errorf("Listener.Capabilities = %q, want %q", cfg.Listener.Capabilities, want)
	}
}

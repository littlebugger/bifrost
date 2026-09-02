package config

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// TestValidateDiagnostics is a table test: one fixture per validation
// rule (see PROJECT.md's timeout table and epic-02's reject/warn list).
// Load runs the full decode+resolve+validate pipeline, so the returned
// diagnostics already include whatever Validate found; each case just
// checks that its rule's diagnostic is present with the right severity
// (other, unrelated diagnostics on the same minimal fixture — e.g. an
// omitted listener block — are expected and ignored).
//
// wantLine, where non-zero, additionally checks d.Subject.Start.Line:
// the epic brief requires "message substring AND line number" for every
// rule, and a line assertion is also what catches an anchor silently
// regressing to a coarser (e.g. whole-block) range later.
func TestValidateDiagnostics(t *testing.T) {
	cases := []struct {
		name     string
		fixture  string
		substr   string
		severity hcl.DiagnosticSeverity
		wantLine int
	}{
		{"nonexistent pool ref", "testdata/bad-nonexistent-pool-ref.hcl", "does-not-exist", hcl.DiagError, 9},
		{"duplicate pool name", "testdata/bad-duplicate-pool.hcl", "Duplicate pool name", hcl.DiagError, 0},
		{"duplicate server name", "testdata/bad-duplicate-server.hcl", "Duplicate server name", hcl.DiagError, 6},
		{"default_pool nonexistent", "testdata/bad-default-pool-nonexistent.hcl", "ghost", hcl.DiagError, 0},
		{"bad CIDR", "testdata/bad-cidr.hcl", "Invalid client_cidr", hcl.DiagError, 9},
		{"bad wildcard", "testdata/bad-wildcard.hcl", "Invalid mail_from_domain wildcard", hcl.DiagError, 9},
		{"weight out of range (>256)", "testdata/bad-weight.hcl", "Weight out of range", hcl.DiagError, 5},
		{"weight out of range (<0)", "testdata/bad-weight-negative.hcl", "Weight out of range", hcl.DiagError, 5},
		{"control character in hostname", "testdata/bad-hostname-control.hcl", "Control character in hostname", hcl.DiagError, 5},
		{"control character in capability", "testdata/bad-capability-control.hcl", "Control character in capability", hcl.DiagError, 4},
		{"STARTTLS advertised without cert", "testdata/bad-starttls-capability-no-cert.hcl", "STARTTLS advertised without a certificate", hcl.DiagError, 6},
		{"SIZE without value", "testdata/bad-size-capability.hcl", "SIZE capability without a value", hcl.DiagError, 0},
		{"SMTPUTF8 without 8BITMIME", "testdata/bad-smtputf8.hcl", "SMTPUTF8 without 8BITMIME", hcl.DiagError, 1},
		{"check level enum", "testdata/bad-check-level.hcl", "Invalid check level", hcl.DiagError, 0},
		{"check port range (>65535)", "testdata/bad-check-port.hcl", "Check port out of range", hcl.DiagError, 0},
		{"check port range (<0)", "testdata/bad-check-port-negative.hcl", "Check port out of range", hcl.DiagError, 6},
		{"rise < 1", "testdata/bad-rise-fall.hcl", "Invalid rise", hcl.DiagError, 0},
		{"fall < 1", "testdata/bad-fall.hcl", "Invalid fall", hcl.DiagError, 7},
		{"backend_tls enum", "testdata/bad-backend-tls.hcl", "Invalid backend_tls", hcl.DiagError, 0},
		{"starttls-verify missing ca/server_name", "testdata/bad-starttls-verify-missing-ca.hcl", "starttls-verify requires ca and server_name", hcl.DiagError, 0},
		{"starttls cert/key unreadable", "testdata/bad-cert-unreadable.hcl", "unreadable", hcl.DiagError, 4},
		{"admin bind malformed", "testdata/bad-admin-bind-malformed.hcl", "Malformed admin bind", hcl.DiagError, 0},
		{"admin non-loopback without allow_remote", "testdata/bad-admin-nonloopback.hcl", "not a loopback address", hcl.DiagError, 0},
		{"multiple listener blocks", "testdata/bad-multiple-listener.hcl", "Multiple listener blocks", hcl.DiagError, 0},
		{"empty pool", "testdata/bad-empty-pool.hcl", "Empty pool", hcl.DiagError, 0},
		{"all-backup pool", "testdata/bad-all-backup-pool.hcl", "All-backup pool", hcl.DiagError, 0},
		{"negative server max_transactions", "testdata/bad-max-transactions-negative.hcl", "Negative max_transactions", hcl.DiagError, 5},
		{"negative global_maxconn", "testdata/bad-global-maxconn-negative.hcl", "Negative global_maxconn", hcl.DiagError, 13},
		{"backend_tls_ca unreadable", "testdata/bad-backend-ca-unreadable.hcl", "backend_tls_ca unreadable", hcl.DiagError, 5},
		{"starttls min_version enum", "testdata/bad-min-version.hcl", "Invalid starttls min_version", hcl.DiagError, 4},
		{"non-positive timeout", "testdata/bad-timeout-nonpositive.hcl", "Non-positive timeout", hcl.DiagError, 0},
		{"check.timeout > check.interval", "testdata/bad-check-timeout-gt-interval.hcl", "check.timeout exceeds check.interval", hcl.DiagError, 0},
		{"backend_connect > backend_mail_reply (inverted timeout hierarchy)", "testdata/bad-backend-connect-gt-mail-reply.hcl", "backend_connect exceeds backend_mail_reply", hcl.DiagError, 5},
		{"RFC 5321 floor warning", "testdata/warn-rfc-floor.hcl", "RFC 5321", hcl.DiagWarning, 0},
		{"admin block absent warning", "testdata/warn-admin-absent.hcl", "No admin plane configured", hcl.DiagWarning, 0},

		{"client auth without starttls", "testdata/bad-auth-no-starttls.hcl", "client auth requires starttls", hcl.DiagError, 0},
		{"auth block without users", "testdata/bad-auth-no-users.hcl", "auth block without users", hcl.DiagError, 0},
		{"duplicate auth user", "testdata/bad-auth-dup-user.hcl", "duplicate auth user", hcl.DiagError, 0},
		{"malformed hashed_password", "testdata/bad-auth-short-hash.hcl", "malformed hashed_password", hcl.DiagError, 0},
		{"auth user without a salt", "testdata/bad-auth-empty-salt.hcl", "auth user without a salt", hcl.DiagError, 0},
		{"pool auth requires backend TLS", "testdata/bad-pool-auth-cleartext.hcl", "pool auth requires backend TLS", hcl.DiagError, 0},
		{"pool auth requires TLS probes", "testdata/bad-pool-auth-plaintext-probe.hcl", "pool auth requires TLS probes", hcl.DiagError, 0},
		{"pool auth without credentials", "testdata/bad-pool-auth-empty.hcl", "pool auth without credentials", hcl.DiagError, 0},
		{"control character in auth credential", "testdata/bad-auth-control-char.hcl", "Control character in auth credential", hcl.DiagError, 0},
		{"pool auth password conflict", "testdata/bad-pool-auth-both-passwords.hcl", "pool auth password conflict", hcl.DiagError, 0},
		{"pool auth password_file unreadable", "testdata/bad-pool-auth-file-unreadable.hcl", "pool auth password_file unreadable", hcl.DiagError, 0},
		{"pool auth password_file is empty", "testdata/bad-pool-auth-file-empty.hcl", "pool auth password_file is empty", hcl.DiagError, 0},
		{"negative reuse_envelopes", "testdata/bad-reuse-negative.hcl", "reuse_envelopes out of range", hcl.DiagError, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, diags := Load(tc.fixture)
			if cfg == nil {
				t.Fatalf("Load(%s) = nil cfg, diags = %v (fixture must decode even though it fails validation)", tc.fixture, diags)
			}
			d := findDiag(diags, tc.substr)
			if d == nil {
				t.Fatalf("Load(%s) diags = %v, want one containing %q", tc.fixture, diags, tc.substr)
			}
			if d.Severity != tc.severity {
				t.Errorf("diagnostic severity = %v, want %v (%v)", d.Severity, tc.severity, d)
			}
			if tc.severity == hcl.DiagError && !diags.HasErrors() {
				t.Errorf("diags.HasErrors() = false, want true for %v", diags)
			}
			if tc.wantLine != 0 {
				if d.Subject == nil {
					t.Fatalf("diagnostic %v has no Subject range, want line %d", d, tc.wantLine)
				}
				if d.Subject.Start.Line != tc.wantLine {
					t.Errorf("diagnostic line = %d, want %d (%v)", d.Subject.Start.Line, tc.wantLine, d)
				}
			}
		})
	}
}

// TestAdminBindNonLoopbackRejected is called out by name in the epic plan
// in addition to its row in the table above: a non-loopback TCP admin
// bind without allow_remote = true is D15's central safety rule, so it
// gets a dedicated, easy-to-find regression test of its own.
func TestAdminBindNonLoopbackRejected(t *testing.T) {
	cfg, diags := Load("testdata/bad-admin-nonloopback.hcl")
	if cfg == nil {
		t.Fatalf("Load(bad-admin-nonloopback.hcl) = nil cfg, diags = %v", diags)
	}
	if !diags.HasErrors() {
		t.Fatalf("Load diags = %v, want an error rejecting the non-loopback admin bind", diags)
	}
	d := findDiag(diags, "not a loopback address")
	if d == nil {
		t.Fatalf("Load diags = %v, want one mentioning %q", diags, "not a loopback address")
	}
	if d.Severity != hcl.DiagError {
		t.Errorf("severity = %v, want DiagError", d.Severity)
	}

	// allow_remote = true must lift the rejection. Validate is
	// independently callable (not just through Load), so this exercises
	// that directly.
	cfg.Admin.AllowRemote = true
	if d := findDiag(cfg.Validate(), "not a loopback address"); d != nil {
		t.Errorf("Validate() with allow_remote = true still rejected the bind: %v", d)
	}
}

// TestDefaultPoolMissing covers the "missing" half of "default_pool
// missing/nonexistent" — the other half (a default_pool that names a
// pool that doesn't exist) is TestValidateDiagnostics's own row. Missing
// entirely is a required-attribute decode error (default_pool has no
// ",optional" tag), not a Validate finding, so Load never gets past
// decode and this can't share TestValidateDiagnostics's table shape
// (which requires cfg != nil).
func TestDefaultPoolMissing(t *testing.T) {
	cfg, diags := Load("testdata/bad-default-pool-missing.hcl")
	if cfg != nil {
		t.Fatalf("Load(bad-default-pool-missing.hcl) cfg = %+v, want nil (default_pool is a required attribute)", cfg)
	}
	if !diags.HasErrors() {
		t.Fatalf("diags = %v, want an error for the missing default_pool", diags)
	}
	if d := findDiag(diags, "default_pool"); d == nil {
		t.Fatalf("diags = %v, want one mentioning %q", diags, "default_pool")
	}
}

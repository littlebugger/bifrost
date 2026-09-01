package config

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// rfc5321Floors are the RFC 5321 §4.5.3.2 minimum backend reply wait
// times. They are floors, not ceilings: Validate only warns when a
// configured value sits below them, and never rejects, and never caps
// from above.
var rfc5321Floors = map[string]time.Duration{
	"backend_handshake":  300 * time.Second,
	"backend_mail_reply": 300 * time.Second,
	"backend_354_wait":   120 * time.Second,
	"data_progress":      180 * time.Second,
	"backend_final_dot":  600 * time.Second,
}

// Validate runs Bifrost's semantic checks against c and returns every
// finding as an hcl.Diagnostics, anchored at the original source range
// where available. Errors mean the config must not be used; warnings are
// informational and never prevent loading (see HasErrors on the result).
//
// Validate reads whatever is currently in c: called on the result of Load
// it sees the fully resolved, inheritance-applied config; called directly
// on a hand-built Config (as tests do) it validates exactly those field
// values, with unset source ranges degrading to an unanchored diagnostic.
func (c *Config) Validate() hcl.Diagnostics {
	var diags hcl.Diagnostics
	diags = append(diags, c.validateListenerCount()...)
	diags = append(diags, c.validateListenerContent()...)
	diags = append(diags, c.validateStartTLSFiles()...)
	diags = append(diags, c.validateListenerAuth()...)
	diags = append(diags, c.validatePools()...)
	diags = append(diags, c.validateRouting()...)
	diags = append(diags, c.validateAdmin()...)
	diags = append(diags, c.validateTimeouts()...)
	diags = append(diags, c.validateLimits()...)
	return diags
}

// validateLimits rejects negative capacity limits. Zero is meaningful for
// both — max_transactions 0 means unlimited (PROJECT.md), global_maxconn
// 0-or-less means no accept cap — so a negative value silently reads as
// "unlimited", which is the exact opposite of what an operator writing
// -1 could plausibly intend. Rejected rather than clamped: guessing which
// direction they meant is worse than telling them.
func (c *Config) validateLimits() hcl.Diagnostics {
	var diags hcl.Diagnostics
	limitsRange := fallbackRange(c.Limits.rng, c.fileRange)
	if c.Limits.GlobalMaxConn < 0 {
		diags = append(diags, errDiag(fallbackRange(c.Limits.maxConnRange, limitsRange),
			"Negative global_maxconn",
			fmt.Sprintf("global_maxconn %d must be >= 0 (0 means unlimited).", c.Limits.GlobalMaxConn)))
	}
	if c.Limits.MaxTransactions < 0 {
		diags = append(diags, errDiag(fallbackRange(c.Limits.maxTxnRange, limitsRange),
			"Negative max_transactions",
			fmt.Sprintf("max_transactions %d must be >= 0 (0 means unlimited).", c.Limits.MaxTransactions)))
	}
	return diags
}

// negativeMaxTxn reports a negative max_transactions written at the
// pool or server tier. It reports only the tier that actually wrote the
// value (rng is zero for an inherited one), so a single bad number
// produces a single diagnostic instead of one per server that inherited
// it — the tier that inherited it is not where the operator has to edit.
func negativeMaxTxn(value int, rng hcl.Range, what string) hcl.Diagnostics {
	if value >= 0 || rng == (hcl.Range{}) {
		return nil
	}
	return hcl.Diagnostics{errDiag(rng, "Negative max_transactions",
		fmt.Sprintf("%s max_transactions %d must be >= 0 (0 means unlimited).", what, value))}
}

func errDiag(rng hcl.Range, summary, detail string) *hcl.Diagnostic {
	return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: summary, Detail: detail, Subject: &rng}
}

func warnDiag(rng hcl.Range, summary, detail string) *hcl.Diagnostic {
	return &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: summary, Detail: detail, Subject: &rng}
}

func validEnum(v string, options ...string) bool {
	for _, o := range options {
		if v == o {
			return true
		}
	}
	return false
}

// fieldRange looks up key in fr (a Timeouts/CheckParams.fieldRanges map)
// and returns it if present, else fallback. fr is nil for a Timeouts or
// CheckParams built by hand rather than resolved by Load, in which case
// every lookup misses and every diagnostic anchors at fallback (normally
// the enclosing block's own range) instead of the precise attribute.
func fieldRange(fr map[string]hcl.Range, key string, fallback hcl.Range) hcl.Range {
	if rng, ok := fr[key]; ok {
		return rng
	}
	return fallback
}

// fallbackRange returns rng, or alt when rng was never populated (an
// attribute the operator omitted, or a hand-built Config that never went
// through Load).
func fallbackRange(rng, alt hcl.Range) hcl.Range {
	if rng.Filename == "" {
		return alt
	}
	return rng
}

// validateListenerCount enforces "exactly one listener block" (D: v1
// supports a single listener). It reads Config.listenerRanges rather than
// len(Config.Pools)-style counting because conversion already collapses
// to a single Listener value; this is the one place that needs to know
// how many blocks actually existed in the source.
func (c *Config) validateListenerCount() hcl.Diagnostics {
	switch len(c.listenerRanges) {
	case 0:
		return hcl.Diagnostics{errDiag(c.fileRange, "Missing listener block", "Bifrost requires exactly one listener block; none was found.")}
	case 1:
		return nil
	default:
		return hcl.Diagnostics{errDiag(c.listenerRanges[1], "Multiple listener blocks",
			fmt.Sprintf("only one listener is supported in v1; another was already defined at %s.", c.listenerRanges[0]))}
	}
}

// controlCharIndex returns the index of the first ASCII control
// character in s (CR and LF included), or -1. Those bytes are reply
// injection: hostname and capability strings are written verbatim into
// the banner and the 250- lines.
func controlCharIndex(s string) int {
	return strings.IndexFunc(s, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	})
}

// validateListenerContent checks the advertised capability list: no
// control characters in anything that reaches the wire, SIZE must carry
// a value, SMTPUTF8 requires 8BITMIME per RFC 6531, and STARTTLS may
// only be advertised when a certificate is configured.
func (c *Config) validateListenerContent() hcl.Diagnostics {
	var diags hcl.Diagnostics
	var hasSMTPUTF8, has8BitMIME bool

	if i := controlCharIndex(c.Listener.Hostname); i >= 0 {
		diags = append(diags, errDiag(fallbackRange(c.Listener.hostnameRange, c.Listener.rng),
			"Control character in hostname",
			fmt.Sprintf("hostname %q contains a control character at byte %d; it is written verbatim into the banner and EHLO reply.", c.Listener.Hostname, i)))
	}
	for _, capa := range c.Listener.Capabilities {
		if i := controlCharIndex(capa); i >= 0 {
			diags = append(diags, errDiag(fallbackRange(c.Listener.capabilitiesRange, c.Listener.rng),
				"Control character in capability",
				fmt.Sprintf("capability %q contains a control character at byte %d; it is written verbatim into the EHLO reply.", capa, i)))
		}
		if strings.EqualFold(strings.TrimSpace(capa), capStartTLS) && c.Listener.StartTLS == nil {
			diags = append(diags, errDiag(fallbackRange(c.Listener.capabilitiesRange, c.Listener.rng),
				"STARTTLS advertised without a certificate",
				"the listener advertises STARTTLS but has no starttls block; add cert/key or drop the capability."))
		}
	}

	for _, capa := range c.Listener.Capabilities {
		switch strings.TrimSpace(capa) {
		case "SIZE":
			diags = append(diags, errDiag(c.Listener.rng, "SIZE capability without a value",
				`"SIZE" must include a byte-count value, e.g. "SIZE 10485760".`))
		case "SMTPUTF8":
			hasSMTPUTF8 = true
		case "8BITMIME":
			has8BitMIME = true
		}
	}
	if hasSMTPUTF8 && !has8BitMIME {
		diags = append(diags, errDiag(c.Listener.rng, "SMTPUTF8 without 8BITMIME",
			"RFC 6531 requires 8BITMIME whenever SMTPUTF8 is advertised."))
	}
	return diags
}

// validateStartTLSFiles checks the listener's starttls block: the
// min_version enum, and that the cert and key are actually readable from
// disk. Paths are already resolved relative to the config file's directory
// by the time Validate runs (see resolveFilePaths in load.go).
func (c *Config) validateStartTLSFiles() hcl.Diagnostics {
	if c.Listener.StartTLS == nil {
		return nil
	}
	var diags hcl.Diagnostics
	st := c.Listener.StartTLS
	if st.MinVersion != "" && !validEnum(st.MinVersion, "1.0", "1.1", "1.2", "1.3") {
		// Rejected, never silently downgraded: an unrecognized value here
		// would otherwise fall back to the built-in minimum, which is the
		// one outcome an operator who wrote min_version cannot detect.
		diags = append(diags, errDiag(st.rng, "Invalid starttls min_version",
			fmt.Sprintf("min_version %q is not one of 1.0, 1.1, 1.2, 1.3.", st.MinVersion)))
	}
	if _, err := os.ReadFile(st.Cert); err != nil {
		diags = append(diags, errDiag(st.rng, "starttls cert unreadable", fmt.Sprintf("cert %q: %s", st.Cert, err)))
	}
	if _, err := os.ReadFile(st.Key); err != nil {
		diags = append(diags, errDiag(st.rng, "starttls key unreadable", fmt.Sprintf("key %q: %s", st.Key, err)))
	}
	return diags
}

// validateListenerAuth checks the listener's client-leg SMTP AUTH block.
// A non-nil Listener.Auth is a guarantee the rest of the codebase relies
// on: STARTTLS must be configured (credentials must never cross the wire
// before TLS), at least one user must exist, user names must be unique,
// every user needs a salt, and hashed_password must be a usable
// SHA-256 hex digest. Reuses controlCharIndex (already used for
// hostname/capability injection) so no credential field can smuggle a
// NUL or CRLF into whatever later reads it verbatim.
func (c *Config) validateListenerAuth() hcl.Diagnostics {
	auth := c.Listener.Auth
	if auth == nil {
		return nil
	}
	var diags hcl.Diagnostics
	if c.Listener.StartTLS == nil {
		diags = append(diags, errDiag(fallbackRange(auth.rng, c.Listener.rng),
			"client auth requires starttls",
			"listener.auth is configured but the listener has no starttls block; client credentials must never be sent before TLS."))
	}
	if len(auth.Users) == 0 {
		diags = append(diags, errDiag(auth.rng, "auth block without users",
			"listener.auth has no user blocks; add at least one or remove the auth block."))
	}

	seen := map[string]bool{}
	for _, u := range auth.Users {
		if seen[u.Name] {
			diags = append(diags, errDiag(u.rng, "duplicate auth user", fmt.Sprintf("user %q is already defined.", u.Name)))
		}
		seen[u.Name] = true

		if u.Salt == "" {
			diags = append(diags, errDiag(u.rng, "auth user without a salt", fmt.Sprintf("user %q has an empty salt.", u.Name)))
		}
		if !validHexHash(u.HashedPassword) {
			diags = append(diags, errDiag(u.rng, "malformed hashed_password",
				fmt.Sprintf("user %q hashed_password must be 64 lowercase hex characters (SHA-256), got %d.", u.Name, len(u.HashedPassword))))
		}
		if i := controlCharIndex(u.Name); i >= 0 {
			diags = append(diags, errDiag(u.rng, "Control character in auth credential",
				fmt.Sprintf("user name %q contains a control character at byte %d.", u.Name, i)))
		}
		if i := controlCharIndex(u.Salt); i >= 0 {
			diags = append(diags, errDiag(u.rng, "Control character in auth credential",
				fmt.Sprintf("salt for user %q contains a control character at byte %d.", u.Name, i)))
		}
	}
	return diags
}

// validHexHash reports whether h is a 64-character lowercase hex string
// (a SHA-256 digest). HashedPassword is already lowercased at load
// (load.go's rawListenerAuth.convert), so this never needs to case-fold.
func validHexHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, r := range h {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// validatePoolAuth checks a pool's backend-leg SMTP AUTH block: it must
// only be used over an encrypted backend leg — both the traffic path
// (backend_tls) and every health probe in the pool (check.tls, which can
// override backend_tls per server or per pool, see resolveCheck) — and
// username plus at least one of password/password_file must be present.
// By the time this runs, resolveBackendCAs has already turned a readable
// password_file into Password, so this only needs to check Password —
// password_file's own unreadable/empty/conflict cases are load-time
// diagnostics raised there, not here. Reuses controlCharIndex for the
// same injection reason as validateListenerAuth.
func validatePoolAuth(p Pool) hcl.Diagnostics {
	auth := p.Auth
	if auth == nil {
		return nil
	}
	var diags hcl.Diagnostics
	rng := fallbackRange(auth.rng, p.rng)
	if p.BackendTLS == "none" {
		diags = append(diags, errDiag(rng, "pool auth requires backend TLS",
			fmt.Sprintf("pool %q sets auth but backend_tls = \"none\"; backend credentials must never be sent in cleartext.", p.Name)))
	}
	if checkRng, plaintext := poolHasCleartextCheck(p); plaintext {
		diags = append(diags, errDiag(checkRng, "pool auth requires TLS probes",
			fmt.Sprintf("pool %q sets auth but a check resolves tls = \"none\"; probes carry the pool credentials, and a check { tls = \"none\" } override would hand them to whatever answers the cleartext EHLO.", p.Name)))
	}
	if auth.Username == "" || (auth.Password == "" && auth.PasswordFile == "") {
		diags = append(diags, errDiag(rng, "pool auth without credentials",
			fmt.Sprintf("pool %q auth block is missing username or password.", p.Name)))
	}
	if i := controlCharIndex(auth.Username); i >= 0 {
		diags = append(diags, errDiag(rng, "Control character in auth credential",
			fmt.Sprintf("pool %q auth username contains a control character at byte %d.", p.Name, i)))
	}
	if i := controlCharIndex(auth.Password); i >= 0 {
		diags = append(diags, errDiag(rng, "Control character in auth credential",
			fmt.Sprintf("pool %q auth password contains a control character at byte %d.", p.Name, i)))
	}
	return diags
}

// poolHasCleartextCheck reports whether any resolved check TLS setting in
// pool p — its own check{} block, or any server's — is "none", along with
// that check's own range. TLS has no attr_range companion of its own (see
// validateCheckContent), so the anchor is the check block's range
// (cp.rng), falling back to the pool/server block when a check was never
// decoded at all (fully defaulted, in which case TLS can only be "none"
// via poolBackendTLS itself, already covered by the backend_tls check
// above).
func poolHasCleartextCheck(p Pool) (hcl.Range, bool) {
	if p.Check.TLS == "none" {
		return fallbackRange(p.Check.rng, p.rng), true
	}
	for _, s := range p.Servers {
		if s.Check.TLS == "none" {
			return fallbackRange(s.Check.rng, s.rng), true
		}
	}
	return hcl.Range{}, false
}

// validatePools checks structural pool/server rules (duplicates, empty or
// all-backup pools, weight range, backend TLS) and each server's check
// content.
func (c *Config) validatePools() hcl.Diagnostics {
	var diags hcl.Diagnostics
	seenPool := map[string]bool{}
	for _, p := range c.Pools {
		if seenPool[p.Name] {
			diags = append(diags, errDiag(p.rng, "Duplicate pool name", fmt.Sprintf("pool %q is already defined.", p.Name)))
		}
		seenPool[p.Name] = true

		diags = append(diags, validatePoolShape(p)...)
		diags = append(diags, validatePoolBackendTLS(p)...)
		diags = append(diags, validatePoolAuth(p)...)
		diags = append(diags, negativeMaxTxn(p.MaxTransactions, p.maxTxnRange, "pool")...)

		seenServer := map[string]bool{}
		for _, s := range p.Servers {
			if seenServer[s.Name] {
				diags = append(diags, errDiag(s.rng, "Duplicate server name",
					fmt.Sprintf("server %q is already defined in pool %q.", s.Name, p.Name)))
			}
			seenServer[s.Name] = true

			if s.Weight < 0 || s.Weight > 256 {
				weightRng := s.rng
				if s.weightRange != (hcl.Range{}) {
					weightRng = s.weightRange
				}
				diags = append(diags, errDiag(weightRng, "Weight out of range", fmt.Sprintf("weight %d must be between 0 and 256.", s.Weight)))
			}
			diags = append(diags, negativeMaxTxn(s.MaxTransactions, s.maxTxnRange, "server")...)
			diags = append(diags, validateCheckContent(s.Check)...)
		}
	}
	return diags
}

func validatePoolShape(p Pool) hcl.Diagnostics {
	if len(p.Servers) == 0 {
		return hcl.Diagnostics{errDiag(p.rng, "Empty pool", fmt.Sprintf("pool %q has no servers.", p.Name))}
	}
	for _, s := range p.Servers {
		if !s.Backup {
			return nil
		}
	}
	return hcl.Diagnostics{errDiag(p.rng, "All-backup pool", fmt.Sprintf("pool %q has no primary (non-backup) server.", p.Name))}
}

func validatePoolBackendTLS(p Pool) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if p.BackendTLS != "" && !validEnum(p.BackendTLS, "none", "starttls", "starttls-verify") {
		diags = append(diags, errDiag(p.rng, "Invalid backend_tls",
			fmt.Sprintf("backend_tls %q is not one of none, starttls, starttls-verify.", p.BackendTLS)))
	}
	if p.BackendTLS == "starttls-verify" && (p.BackendTLSServerName == "" || p.BackendTLSCA == "") {
		diags = append(diags, errDiag(p.rng, "starttls-verify requires ca and server_name",
			fmt.Sprintf("pool %q sets backend_tls = \"starttls-verify\" but is missing backend_tls_ca or backend_tls_server_name.", p.Name)))
	}
	return diags
}

// validateCheckContent checks the fully-resolved per-server check config.
// A zero cp.rng means no check block was ever decoded for this server at
// any tier (Load hasn't run, or the config predates inheritance
// resolution): there is nothing user-authored to validate yet, so this
// returns no diagnostics rather than complaining about zero-valued
// fields that are really just "not decided yet".
//
// level/port/rise/fall/interval anchor at their own attribute's range
// (cp.fieldRanges, populated by resolveCheck) rather than at the whole
// check{} block, since a bad port and a bad rise are different lines.
// TLS falls back to cp.rng: it has no attr_range companion (see rawCheck)
// because it's a plain pass-through with no per-attribute tracking need
// beyond the enum check below.
func validateCheckContent(cp CheckParams) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if cp.rng == (hcl.Range{}) {
		return diags
	}
	if cp.Level != "" && !validEnum(cp.Level, "connect", "banner", "ehlo", "deep") {
		diags = append(diags, errDiag(fieldRange(cp.fieldRanges, "level", cp.rng), "Invalid check level", fmt.Sprintf("level %q is not one of connect, banner, ehlo, deep.", cp.Level)))
	}
	if cp.TLS != "" && !validEnum(cp.TLS, "none", "starttls", "starttls-verify") {
		diags = append(diags, errDiag(cp.rng, "Invalid check tls", fmt.Sprintf("tls %q is not one of none, starttls, starttls-verify.", cp.TLS)))
	}
	if cp.Port < 0 || cp.Port > 65535 {
		diags = append(diags, errDiag(fieldRange(cp.fieldRanges, "port", cp.rng), "Check port out of range", fmt.Sprintf("port %d must be between 1 and 65535 (0 = server's traffic port).", cp.Port)))
	}
	if cp.Rise < 1 {
		diags = append(diags, errDiag(fieldRange(cp.fieldRanges, "rise", cp.rng), "Invalid rise", fmt.Sprintf("rise %d must be >= 1.", cp.Rise)))
	}
	if cp.Fall < 1 {
		diags = append(diags, errDiag(fieldRange(cp.fieldRanges, "fall", cp.rng), "Invalid fall", fmt.Sprintf("fall %d must be >= 1.", cp.Fall)))
	}
	if cp.Timeout > 0 && cp.Interval > 0 && cp.Timeout > cp.Interval {
		diags = append(diags, errDiag(fieldRange(cp.fieldRanges, "timeout", cp.rng), "check.timeout exceeds check.interval",
			fmt.Sprintf("timeout %s must not exceed interval %s.", cp.Timeout, cp.Interval)))
	}
	return diags
}

// validateRouting checks rule/default_pool references, CIDR syntax, and
// the mail_from_domain wildcard form.
func (c *Config) validateRouting() hcl.Diagnostics {
	var diags hcl.Diagnostics
	poolNames := map[string]bool{}
	for _, p := range c.Pools {
		poolNames[p.Name] = true
	}

	for _, rule := range c.Routing.Rules {
		if rule.Pool != "" && !poolNames[rule.Pool] {
			diags = append(diags, errDiag(rule.rng, "Rule references a nonexistent pool", fmt.Sprintf("pool %q is not defined.", rule.Pool)))
		}
		for _, cidr := range rule.ClientCIDR {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				diags = append(diags, errDiag(rule.rng, "Invalid client_cidr", fmt.Sprintf("%q is not a valid CIDR: %s", cidr, err)))
			}
		}
		for _, domain := range rule.MailFromDomain {
			if strings.Contains(domain, "*") && !isWildcardSuffix(domain) {
				diags = append(diags, errDiag(rule.rng, "Invalid mail_from_domain wildcard",
					fmt.Sprintf("%q must be a literal domain or a \"*.suffix\" wildcard.", domain)))
			}
		}
	}

	if c.Routing.DefaultPool != "" && !poolNames[c.Routing.DefaultPool] {
		diags = append(diags, errDiag(c.Routing.rng, "default_pool references a nonexistent pool",
			fmt.Sprintf("pool %q is not defined.", c.Routing.DefaultPool)))
	}
	return diags
}

// isWildcardSuffix reports whether s is exactly of the form "*.suffix":
// a leading "*." followed by a non-empty, "*"-free remainder.
func isWildcardSuffix(s string) bool {
	if !strings.HasPrefix(s, "*.") || len(s) <= 2 {
		return false
	}
	return !strings.Contains(s[1:], "*")
}

// validateAdmin warns when no admin block is configured, and otherwise
// checks the bind syntax and the loopback-unless-allow_remote policy (D15).
func (c *Config) validateAdmin() hcl.Diagnostics {
	if c.Admin == nil {
		return hcl.Diagnostics{warnDiag(c.fileRange, "No admin plane configured",
			"admin { } is absent; the runtime admin API (stats, drain, weight) will not be started.")}
	}

	bind := c.Admin.Bind
	if strings.HasPrefix(bind, "unix://") {
		return nil // unix sockets are exempt from the loopback restriction
	}
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return hcl.Diagnostics{errDiag(c.Admin.rng, "Malformed admin bind",
			fmt.Sprintf("%q is neither unix://<path> nor host:port: %s", bind, err))}
	}
	if !isLoopbackHost(host) && !c.Admin.AllowRemote {
		return hcl.Diagnostics{errDiag(c.Admin.rng, "Admin bind is not loopback",
			fmt.Sprintf("%q is not a loopback address; set allow_remote = true to bind non-loopback.", bind))}
	}
	return nil
}

// isLoopbackHost reports whether host (the host part of a bind address)
// refers only to the local machine. An empty host (e.g. ":8081", meaning
// "all interfaces") is deliberately not loopback.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateTimeouts checks Defaults.Timeouts: positivity, the two ordering
// invariants, and RFC 5321 §4.5.3.2 floor warnings. A zero Timeouts.rng
// means no timeouts block was ever decoded (see validateCheckContent for
// why that skips rather than complains). Each of the ten fields anchors
// at its own attribute (t.fieldRanges, populated by resolveTimeouts) —
// ten attributes share one block, so a block-level anchor would put
// every one of their diagnostics on the same "timeouts {" line.
func (c *Config) validateTimeouts() hcl.Diagnostics {
	var diags hcl.Diagnostics
	t := c.Defaults.Timeouts
	if t.rng == (hcl.Range{}) {
		return diags
	}

	named := map[string]time.Duration{
		"client_idle":        t.ClientIdle,
		"session_max":        t.SessionMax,
		"backend_connect":    t.BackendConnect,
		"backend_handshake":  t.BackendHandshake,
		"backend_mail_reply": t.BackendMailReply,
		"backend_354_wait":   t.Backend354Wait,
		"data_progress":      t.DataProgress,
		"backend_final_dot":  t.BackendFinalDot,
		"lame_duck":          t.LameDuck,
		"drain_timeout":      t.DrainTimeout,
	}
	for _, name := range timeoutFieldOrder {
		val := named[name]
		rng := fieldRange(t.fieldRanges, name, t.rng)
		if val <= 0 {
			diags = append(diags, errDiag(rng, "Non-positive timeout", fmt.Sprintf("%s = %s must be greater than zero.", name, val)))
			continue
		}
		if floor, ok := rfc5321Floors[name]; ok && val < floor {
			diags = append(diags, warnDiag(rng, "Timeout below RFC 5321 floor",
				fmt.Sprintf("%s = %s is below the RFC 5321 §4.5.3.2 floor of %s; this is a documented, deliberate deviation for a balancer between cooperating parties.", name, val, floor)))
		}
	}

	if t.BackendConnect > 0 && t.BackendMailReply > 0 && t.BackendConnect > t.BackendMailReply {
		diags = append(diags, errDiag(fieldRange(t.fieldRanges, "backend_connect", t.rng), "backend_connect exceeds backend_mail_reply",
			fmt.Sprintf("backend_connect = %s must not exceed backend_mail_reply = %s.", t.BackendConnect, t.BackendMailReply)))
	}
	return diags
}

// timeoutFieldOrder gives validateTimeouts's diagnostics a stable,
// deterministic order (map iteration order is not stable).
var timeoutFieldOrder = []string{
	"client_idle", "session_max", "backend_connect", "backend_handshake",
	"backend_mail_reply", "backend_354_wait", "data_progress", "backend_final_dot",
	"lame_duck", "drain_timeout",
}

package config

import (
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// Built-in defaults, applied whenever a value is omitted at every tier.
// These are PROJECT.md's tables verbatim; TestBuiltinDefaultsPassValidation
// proves they satisfy Validate on their own.
const (
	builtinClientIdle       = 300 * time.Second
	builtinSessionMax       = time.Hour
	builtinBackendConnect   = 5 * time.Second
	builtinBackendHandshake = 15 * time.Second
	builtinBackendMailReply = 30 * time.Second
	builtinBackend354Wait   = 60 * time.Second
	builtinDataProgress     = 60 * time.Second
	builtinBackendFinalDot  = 600 * time.Second
	builtinLameDuck         = 2 * time.Second
	builtinDrainTimeout     = 30 * time.Second

	builtinCheckLevel        = "ehlo"
	builtinCheckInterval     = 5 * time.Second
	builtinCheckDownInterval = 15 * time.Second
	builtinCheckTimeout      = 5 * time.Second
	builtinCheckRise         = 2
	builtinCheckFall         = 3
	builtinCheckPort         = 0

	// capStartTLS is the one capability Bifrost adds on its own, and only
	// because it is the one it terminates itself (PROJECT.md's EHLO
	// capability policy).
	capStartTLS = "STARTTLS"

	builtinBackendTLS      = "none"
	builtinMaxTransactions = 0
	builtinWeight          = 1
	builtinGlobalMaxConn   = 1024
)

// resolveInheritance fills every inheritance-eligible field of cfg,
// walking the tier order documented in PROJECT.md: server > pool >
// defaults > built-in. It reads raw (the strictly-decoded tree, still
// carrying per-tier presence as nil pointers / empty strings) rather than
// cfg itself, because cfg's fields no longer distinguish "omitted" from
// "explicitly the zero value" once convert has run.
//
// It performs no validation and returns no diagnostics: malformed
// duration text was already reported by convert (Load returns before
// resolveInheritance ever runs in that case), and everything else here is
// pure precedence, not correctness.
func resolveInheritance(cfg *Config, raw *rawFile) {
	var defaultsCheck *rawCheck
	var defaultsTimeouts *rawTimeouts
	if raw.Defaults != nil {
		defaultsCheck = raw.Defaults.Check
		defaultsTimeouts = raw.Defaults.Timeouts
	}
	resolveTimeouts(&cfg.Defaults.Timeouts, defaultsTimeouts, cfg.fileRange)
	resolveCapabilities(&cfg.Listener)

	cfg.Limits.GlobalMaxConn = builtinGlobalMaxConn
	cfg.Limits.MaxTransactions = builtinMaxTransactions
	if raw.Limits != nil {
		if raw.Limits.GlobalMaxConn != nil {
			cfg.Limits.GlobalMaxConn = *raw.Limits.GlobalMaxConn
		}
		if raw.Limits.MaxTransactions != nil {
			cfg.Limits.MaxTransactions = *raw.Limits.MaxTransactions
		}
	}

	for pi := range cfg.Pools {
		rp := &raw.Pools[pi]
		pool := &cfg.Pools[pi]

		if pool.BackendTLS == "" {
			pool.BackendTLS = builtinBackendTLS
		}
		if pool.EhloName == "" {
			pool.EhloName = raw.Defaults.ehloNameOr("")
		}
		if pool.EhloName == "" {
			pool.EhloName = cfg.Listener.Hostname
		}
		if rp.MaxTransactions != nil {
			pool.MaxTransactions = *rp.MaxTransactions
			pool.maxTxnRange = rp.MaxTransactionsRange
		} else {
			pool.MaxTransactions = cfg.Limits.MaxTransactions
		}

		poolChain := []*rawCheck{rp.Check, defaultsCheck}
		pool.Check = resolveCheck(poolChain, pool.BackendTLS, pool.EhloName, pool.rng)

		for si := range pool.Servers {
			rs := &rp.Servers[si]
			server := &pool.Servers[si]

			if rs.Weight != nil {
				server.Weight = *rs.Weight
				server.weightRange = rs.WeightRange
			} else {
				server.Weight = builtinWeight
				server.weightRange = server.rng
			}
			if rs.MaxTransactions != nil {
				server.MaxTransactions = *rs.MaxTransactions
				server.maxTxnRange = rs.MaxTransactionsRange
			} else {
				server.MaxTransactions = pool.MaxTransactions
			}

			serverChain := []*rawCheck{rs.Check, rp.Check, defaultsCheck}
			server.Check = resolveCheck(serverChain, pool.BackendTLS, pool.EhloName, server.rng)
		}
	}
}

// builtinCapabilities is the advertised EHLO set for a listener that
// omits the attribute entirely: PROJECT.md's safe defaults, minus SIZE
// (which needs an operator-chosen byte count) and minus STARTTLS (added
// by resolveCapabilities iff a certificate is configured).
func builtinCapabilities() []string {
	return []string{"PIPELINING", "8BITMIME"}
}

// resolveCapabilities fills the listener's advertised capability set:
// the built-in default when the attribute was omitted, plus STARTTLS
// whenever a certificate is configured (deduplicated, so an operator who
// listed it explicitly gets it once). An explicit list is otherwise left
// exactly as written; Validate rejects a hand-written STARTTLS when
// there is no certificate to back it.
func resolveCapabilities(l *Listener) {
	if l.Capabilities == nil {
		l.Capabilities = builtinCapabilities()
	}
	if l.StartTLS == nil || hasCapability(l.Capabilities, capStartTLS) {
		return
	}
	l.Capabilities = append(l.Capabilities, capStartTLS)
}

// hasCapability reports whether caps advertises name, comparing the way
// an SMTP client would: trimmed and case-insensitively.
func hasCapability(caps []string, name string) bool {
	return slices.ContainsFunc(caps, func(c string) bool {
		return strings.EqualFold(strings.TrimSpace(c), name)
	})
}

// ehloNameOr returns d.EhloName, or fallback if d is nil (no defaults
// block at all).
func (d *rawDefaults) ehloNameOr(fallback string) string {
	if d == nil {
		return fallback
	}
	return d.EhloName
}

// resolveTimeouts fills t's zero-valued (omitted) fields from built-ins,
// and anchors each of the ten fields at its own attribute rather than
// the whole timeouts{} block (a bad backend_handshake and a bad
// backend_mail_reply are different lines, and should report as such).
// raw is nil when there was no defaults.timeouts block at all, in which
// case blockRange (normally the file's range) anchors every field —
// t.rng/t.fieldRanges would otherwise stay zero forever, which would
// make Validate skip Timeouts even after this resolution has fully
// populated it.
func resolveTimeouts(t *Timeouts, raw *rawTimeouts, fallbackRange hcl.Range) {
	var r rawTimeouts
	blockRange := fallbackRange
	if raw != nil {
		r = *raw
		blockRange = raw.Range
	}

	fr := make(map[string]hcl.Range, 10)
	resolve := func(name string, dst *time.Duration, text string, textRange hcl.Range, builtin time.Duration) {
		if text == "" {
			*dst = builtin
			fr[name] = blockRange
			return
		}
		// *dst already holds convert's parsed value; only the range is
		// new here.
		fr[name] = textRange
	}

	resolve("client_idle", &t.ClientIdle, r.ClientIdle, r.ClientIdleRange, builtinClientIdle)
	resolve("session_max", &t.SessionMax, r.SessionMax, r.SessionMaxRange, builtinSessionMax)
	resolve("backend_connect", &t.BackendConnect, r.BackendConnect, r.BackendConnectRange, builtinBackendConnect)
	resolve("backend_handshake", &t.BackendHandshake, r.BackendHandshake, r.BackendHandshakeRange, builtinBackendHandshake)
	resolve("backend_mail_reply", &t.BackendMailReply, r.BackendMailReply, r.BackendMailReplyRange, builtinBackendMailReply)
	resolve("backend_354_wait", &t.Backend354Wait, r.Backend354Wait, r.Backend354WaitRange, builtinBackend354Wait)
	resolve("data_progress", &t.DataProgress, r.DataProgress, r.DataProgressRange, builtinDataProgress)
	resolve("backend_final_dot", &t.BackendFinalDot, r.BackendFinalDot, r.BackendFinalDotRange, builtinBackendFinalDot)
	resolve("lame_duck", &t.LameDuck, r.LameDuck, r.LameDuckRange, builtinLameDuck)
	resolve("drain_timeout", &t.DrainTimeout, r.DrainTimeout, r.DrainTimeoutRange, builtinDrainTimeout)

	t.rng = blockRange
	t.fieldRanges = fr
}

// resolveCheck cascades a server-or-pool-tier CheckParams from chain,
// ordered most-specific first (e.g. [server.Check, pool.Check,
// defaults.Check]), falling back to the built-in defaults. TLS and
// EhloName deliberately bypass the last (defaults) element of chain and
// fall straight to poolBackendTLS/poolEhloName instead — the pool's own
// already-resolved traffic settings — per PROJECT.md's Pool.Check comment.
//
// level/port/interval/down_interval/timeout/rise/fall each get their own
// entry in the result's fieldRanges, anchored at whichever tier's own
// attribute actually won the cascade (or blockRange/fallbackRange when
// none did — the built-in applies, but a diagnostic still needs
// somewhere real to point at). fallbackRange anchors the whole result
// when no tier in chain has a check block at all, so a fully-defaulted
// server still gets a real, non-zero range for Validate to point at.
func resolveCheck(chain []*rawCheck, poolBackendTLS, poolEhloName string, fallbackRange hcl.Range) CheckParams {
	trafficTierChain := chain
	if len(chain) > 1 {
		trafficTierChain = chain[:len(chain)-1] // exclude the defaults tier
	}

	blockRange := fallbackRange
	for _, rc := range chain {
		if rc != nil {
			blockRange = rc.Range
			break
		}
	}

	fr := make(map[string]hcl.Range, 7)

	level, levelRng := firstCheckStringRanged(chain, func(rc *rawCheck) string { return rc.Level }, func(rc *rawCheck) hcl.Range { return rc.LevelRange })
	if level == "" {
		level = builtinCheckLevel
		levelRng = blockRange
	}
	fr["level"] = levelRng

	tls := firstCheckString(trafficTierChain, func(rc *rawCheck) string { return rc.TLS })
	if tls == "" {
		tls = poolBackendTLS
	}
	ehloName := firstCheckString(trafficTierChain, func(rc *rawCheck) string { return rc.EhloName })
	if ehloName == "" {
		ehloName = poolEhloName
	}
	probeRcpt := firstCheckString(chain, func(rc *rawCheck) string { return rc.ProbeRcpt })

	port, portRng, ok := firstCheckInt(chain, func(rc *rawCheck) *int { return rc.Port }, func(rc *rawCheck) hcl.Range { return rc.PortRange })
	if !ok {
		port = builtinCheckPort
		portRng = blockRange
	}
	fr["port"] = portRng

	rise, riseRng, ok := firstCheckInt(chain, func(rc *rawCheck) *int { return rc.Rise }, func(rc *rawCheck) hcl.Range { return rc.RiseRange })
	if !ok {
		rise = builtinCheckRise
		riseRng = blockRange
	}
	fr["rise"] = riseRng

	fall, fallRng, ok := firstCheckInt(chain, func(rc *rawCheck) *int { return rc.Fall }, func(rc *rawCheck) hcl.Range { return rc.FallRange })
	if !ok {
		fall = builtinCheckFall
		fallRng = blockRange
	}
	fr["fall"] = fallRng

	interval, intervalRng, ok := firstCheckDuration(chain, func(rc *rawCheck) string { return rc.Interval }, func(rc *rawCheck) hcl.Range { return rc.IntervalRange })
	if !ok {
		interval = builtinCheckInterval
		intervalRng = blockRange
	}
	fr["interval"] = intervalRng

	downInterval, downIntervalRng, ok := firstCheckDuration(chain, func(rc *rawCheck) string { return rc.DownInterval }, func(rc *rawCheck) hcl.Range { return rc.DownIntervalRange })
	if !ok {
		downInterval = builtinCheckDownInterval
		downIntervalRng = blockRange
	}
	fr["down_interval"] = downIntervalRng

	timeout, timeoutRng, ok := firstCheckDuration(chain, func(rc *rawCheck) string { return rc.Timeout }, func(rc *rawCheck) hcl.Range { return rc.TimeoutRange })
	if !ok {
		timeout = builtinCheckTimeout
		timeoutRng = blockRange
	}
	fr["timeout"] = timeoutRng

	return CheckParams{
		Level:        level,
		Port:         port,
		Interval:     interval,
		DownInterval: downInterval,
		Timeout:      timeout,
		Rise:         rise,
		Fall:         fall,
		EhloName:     ehloName,
		ProbeRcpt:    probeRcpt,
		TLS:          tls,
		rng:          blockRange,
		fieldRanges:  fr,
	}
}

// firstCheckString returns the first non-empty string get(rc) yields,
// walking chain in order and skipping nil entries. Used for the fields
// that don't need a precise per-attribute range (see firstCheckStringRanged).
func firstCheckString(chain []*rawCheck, get func(*rawCheck) string) string {
	v, _ := firstCheckStringRanged(chain, get, func(rc *rawCheck) hcl.Range { return rc.Range })
	return v
}

// firstCheckStringRanged is firstCheckString plus the attribute's own
// range from the winning tier (the zero Range if nothing won).
func firstCheckStringRanged(chain []*rawCheck, get func(*rawCheck) string, getRange func(*rawCheck) hcl.Range) (string, hcl.Range) {
	for _, rc := range chain {
		if rc == nil {
			continue
		}
		if v := get(rc); v != "" {
			return v, getRange(rc)
		}
	}
	return "", hcl.Range{}
}

// firstCheckInt returns the first tier in chain where get(rc) is
// non-nil (i.e. the attribute was actually written), that attribute's
// own range, and whether any tier had one at all.
func firstCheckInt(chain []*rawCheck, get func(*rawCheck) *int, getRange func(*rawCheck) hcl.Range) (int, hcl.Range, bool) {
	for _, rc := range chain {
		if rc == nil {
			continue
		}
		if v := get(rc); v != nil {
			return *v, getRange(rc), true
		}
	}
	return 0, hcl.Range{}, false
}

// firstCheckDuration returns the first tier in chain where get(rc) is
// non-empty, parsed as a duration, that attribute's own range, and
// whether any tier had one at all. A parse error here was already
// reported by convert during the initial decode pass (Load bails before
// resolveInheritance runs in that case), so it is deliberately ignored
// rather than re-diagnosed.
func firstCheckDuration(chain []*rawCheck, get func(*rawCheck) string, getRange func(*rawCheck) hcl.Range) (time.Duration, hcl.Range, bool) {
	for _, rc := range chain {
		if rc == nil {
			continue
		}
		if v := get(rc); v != "" {
			d, _ := time.ParseDuration(v)
			return d, getRange(rc), true
		}
	}
	return 0, hcl.Range{}, false
}

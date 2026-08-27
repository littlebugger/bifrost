package balance

import (
	"net/netip"
	"strings"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/proxy"
)

// MatchPool evaluates routing.Rules in config file order against tx and
// returns the first matching rule's pool, or routing.DefaultPool when
// none match. This is the only place internal/balance imports
// internal/proxy: TxnMeta is everything a rule may match on.
func MatchPool(routing config.Routing, tx proxy.TxnMeta) string {
	for _, rule := range routing.Rules {
		if ruleMatches(rule, tx) {
			return rule.Pool
		}
	}
	return routing.DefaultPool
}

// ruleMatches reports whether every key rule actually specifies matches
// tx (AND semantics between ClientCIDR and MailFromDomain; a key rule
// leaves empty takes no part in the decision at all).
func ruleMatches(rule config.RoutingRule, tx proxy.TxnMeta) bool {
	if len(rule.ClientCIDR) > 0 && !matchesAnyCIDR(rule.ClientCIDR, tx.ClientIP) {
		return false
	}
	if len(rule.MailFromDomain) > 0 && !matchesAnyDomain(rule.MailFromDomain, tx.MailFromDomain) {
		return false
	}
	return true
}

// matchesAnyCIDR reports whether ip falls inside any of cidrs. An
// invalid Addr (no client peer) or an unparseable CIDR (config.Validate
// already rejects one, so this is only a defensive fallback) never
// matches.
func matchesAnyCIDR(cidrs []string, ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	for _, c := range cidrs {
		if prefix, err := netip.ParsePrefix(c); err == nil && prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// matchesAnyDomain reports whether domain matches any of patterns
// (exact, case-insensitive, or a "*.suffix" wildcard). A null sender —
// domain == "", from MAIL FROM:<> or anything the relay couldn't parse —
// never matches a domain rule: that is a deliberate decision, not an
// oversight, since a domain-keyed rule exists to route on a real
// sender's domain and a null sender has none.
func matchesAnyDomain(patterns []string, domain string) bool {
	if domain == "" {
		return false
	}
	domain = strings.ToLower(domain)
	for _, p := range patterns {
		if matchesDomain(p, domain) {
			return true
		}
	}
	return false
}

// matchesDomain matches one pattern against domain (already lowercased
// by the caller). A "*.suffix" pattern matches any strict subdomain of
// suffix — "a.suffix", "a.b.suffix" — but never suffix itself: the
// wildcard's whole point is to route mail from names *under* a domain,
// not the domain's own postmaster.
func matchesDomain(pattern, domain string) bool {
	pattern = strings.ToLower(pattern)
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return domain != suffix && strings.HasSuffix(domain, "."+suffix)
	}
	return domain == pattern
}

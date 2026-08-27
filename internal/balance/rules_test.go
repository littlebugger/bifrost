package balance

import (
	"net/netip"
	"testing"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/proxy"
)

func routing(defaultPool string, rules ...config.RoutingRule) config.Routing {
	return config.Routing{Rules: rules, DefaultPool: defaultPool}
}

func rule(pool string, cidrs, domains []string) config.RoutingRule {
	return config.RoutingRule{ClientCIDR: cidrs, MailFromDomain: domains, Pool: pool}
}

// TestRuleFirstMatchWins: an earlier rule that matches shadows a later
// one that would also have matched.
func TestRuleFirstMatchWins(t *testing.T) {
	r := routing("default",
		rule("first", nil, []string{"a.com"}),
		rule("second", nil, []string{"a.com"}),
	)
	tx := proxy.TxnMeta{MailFromDomain: "a.com"}
	if got := MatchPool(r, tx); got != "first" {
		t.Errorf("MatchPool = %q, want %q", got, "first")
	}
}

// TestRuleClientCIDR: both address families, netip-based.
func TestRuleClientCIDR(t *testing.T) {
	tests := []struct {
		name string
		cidr string
		ip   string
		want bool
	}{
		{"v4 inside", "10.0.0.0/8", "10.1.2.3", true},
		{"v4 outside", "10.0.0.0/8", "192.168.1.1", false},
		{"v6 inside", "2001:db8::/32", "2001:db8::1", true},
		{"v6 outside", "2001:db8::/32", "2001:db9::1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := routing("default", rule("matched", []string{tc.cidr}, nil))
			tx := proxy.TxnMeta{ClientIP: netip.MustParseAddr(tc.ip)}
			want := "default"
			if tc.want {
				want = "matched"
			}
			if got := MatchPool(r, tx); got != want {
				t.Errorf("MatchPool(ip=%s, cidr=%s) = %q, want %q", tc.ip, tc.cidr, got, want)
			}
		})
	}
}

// TestRuleMailFromDomain: exact match, case-insensitive on both sides.
func TestRuleMailFromDomain(t *testing.T) {
	r := routing("default", rule("matched", nil, []string{"Example.COM"}))

	tests := []struct {
		domain string
		want   string
	}{
		{"example.com", "matched"}, // config text mixed-case, tx already lowercased
		{"EXAMPLE.COM", "matched"}, // tx not lowercased -- rules.go must fold anyway
		{"other.example.com", "default"},
		{"notexample.com", "default"},
	}
	for _, tc := range tests {
		tx := proxy.TxnMeta{MailFromDomain: tc.domain}
		if got := MatchPool(r, tx); got != tc.want {
			t.Errorf("MatchPool(domain=%q) = %q, want %q", tc.domain, got, tc.want)
		}
	}
}

// TestRuleWildcardDomain: "*.suffix" matches subdomains at any depth,
// but never the apex itself.
func TestRuleWildcardDomain(t *testing.T) {
	r := routing("default", rule("matched", nil, []string{"*.news.example.com"}))

	tests := []struct {
		domain string
		want   string
	}{
		{"a.news.example.com", "matched"},
		{"a.b.news.example.com", "matched"},
		{"news.example.com", "default"},  // the apex itself: not a match
		{"xnews.example.com", "default"}, // suffix collision, not a subdomain
		{"other.example.com", "default"},
	}
	for _, tc := range tests {
		tx := proxy.TxnMeta{MailFromDomain: tc.domain}
		if got := MatchPool(r, tx); got != tc.want {
			t.Errorf("MatchPool(domain=%q) = %q, want %q", tc.domain, got, tc.want)
		}
	}
}

// TestRuleANDSemantics: a rule with both keys set requires both to
// match; either one alone is not enough.
func TestRuleANDSemantics(t *testing.T) {
	r := routing("default", rule("matched", []string{"10.0.0.0/8"}, []string{"a.com"}))
	inCIDR := netip.MustParseAddr("10.1.2.3")
	outCIDR := netip.MustParseAddr("192.168.1.1")

	tests := []struct {
		name   string
		ip     netip.Addr
		domain string
		want   string
	}{
		{"both match", inCIDR, "a.com", "matched"},
		{"only cidr matches", inCIDR, "b.com", "default"},
		{"only domain matches", outCIDR, "a.com", "default"},
		{"neither matches", outCIDR, "b.com", "default"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tx := proxy.TxnMeta{ClientIP: tc.ip, MailFromDomain: tc.domain}
			if got := MatchPool(r, tx); got != tc.want {
				t.Errorf("MatchPool = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRuleDefaultPool: no rule (or no rule that matches) falls through
// to default_pool.
func TestRuleDefaultPool(t *testing.T) {
	t.Run("no rules at all", func(t *testing.T) {
		r := routing("default")
		if got := MatchPool(r, proxy.TxnMeta{}); got != "default" {
			t.Errorf("MatchPool = %q, want %q", got, "default")
		}
	})
	t.Run("rules present, none match", func(t *testing.T) {
		r := routing("default", rule("other", nil, []string{"a.com"}))
		tx := proxy.TxnMeta{MailFromDomain: "b.com"}
		if got := MatchPool(r, tx); got != "default" {
			t.Errorf("MatchPool = %q, want %q", got, "default")
		}
	})
}

// TestRuleEmptyMailFrom: a null sender (MAIL FROM:<>, or anything the
// relay could not parse a domain from) never matches a domain-keyed
// rule -- decision: it falls straight through to a rule with no domain
// key at all, or to default_pool.
func TestRuleEmptyMailFrom(t *testing.T) {
	t.Run("domain rule never matches a null sender", func(t *testing.T) {
		r := routing("default", rule("matched", nil, []string{"*.example.com", "a.com"}))
		tx := proxy.TxnMeta{MailFromDomain: ""}
		if got := MatchPool(r, tx); got != "default" {
			t.Errorf("MatchPool = %q, want %q (null sender must not match a domain rule)", got, "default")
		}
	})

	t.Run("a CIDR-only rule still matches a null sender", func(t *testing.T) {
		r := routing("default", rule("matched", []string{"10.0.0.0/8"}, nil))
		tx := proxy.TxnMeta{ClientIP: netip.MustParseAddr("10.1.2.3"), MailFromDomain: ""}
		if got := MatchPool(r, tx); got != "matched" {
			t.Errorf("MatchPool = %q, want %q (no domain key in this rule to fail)", got, "matched")
		}
	})
}

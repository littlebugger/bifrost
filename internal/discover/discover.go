// Package discover is the DNS-discovery concurrency core: one resolver
// goroutine per server_dns template, fail-static on error/empty answers
// (docs/superpowers/specs/2026-11-09-dns-discovery-design.md), swapping
// a derived config.Config into the shared config.Holder only on an
// actual membership delta.
package discover

import (
	"context"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
)

// maxTemplateMembers mirrors config's own (unexported) expanded-member
// cap: ExpandDNS enforces the truncation itself, this package only logs
// when an answer exceeds it.
const maxTemplateMembers = 256

// jitterFraction is the design doc's ±10% spread on ResolveInterval.
const jitterFraction = 0.10

// LookupFunc resolves a hostname to its A/AAAA set. Injected; production
// wiring passes net.DefaultResolver.LookupNetIP (ip4+ip6).
type LookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// Metrics receives discovery's observability events (design doc's
// Observability section). A plain interface, minimal on purpose: Task 5
// implements it in internal/metrics, with no import needed on its side.
type Metrics interface {
	// Discovery reports one resolution attempt's outcome
	// (bifrost_dns_discovery_total{template, result}).
	Discovery(template string, result string)
	// DiscoveryChange reports one membership delta that caused a swap
	// (bifrost_dns_discovery_changes_total{template}).
	DiscoveryChange(template string)
	// DiscoveredMembers reports a template's current member count
	// (bifrost_pool_discovered_members{pool, template} gauge).
	DiscoveredMembers(pool, template string, n int)
}

// noMetrics is the do-nothing Metrics used when nothing was wired in.
type noMetrics struct{}

func (noMetrics) Discovery(string, string)              {}
func (noMetrics) DiscoveryChange(string)                {}
func (noMetrics) DiscoveredMembers(string, string, int) {}

// Runner owns DNS membership and every holder swap derived from it. All
// swaps — its own membership deltas AND operator reloads via Rerender —
// go through mu: config.ExpandDNS itself has no locking (its own doc
// comment says so), so this is the one place that serializes "expand,
// then swap" against every other goroutine that might also expand+swap
// at the same time.
type Runner struct {
	holder  *config.Holder
	lookup  LookupFunc
	lg      *slog.Logger
	metrics Metrics

	mu      sync.Mutex
	members map[config.TemplateKey][]netip.Addr
	base    *config.Config
}

// New returns a Runner ready for Run. lg nil means slog.Default(); m nil
// means a no-op Metrics.
func New(h *config.Holder, lookup LookupFunc, lg *slog.Logger, m Metrics) *Runner {
	if lg == nil {
		lg = slog.Default()
	}
	if m == nil {
		m = noMetrics{}
	}
	return &Runner{
		holder:  h,
		lookup:  lookup,
		lg:      lg,
		metrics: m,
		members: make(map[config.TemplateKey][]netip.Addr),
	}
}

// Run starts one goroutine per server_dns template in the CURRENT base
// config (r.holder.Load() at call time) and blocks until ctx is
// cancelled, then joins every goroutine it started before returning.
// Templates added by a later Rerender are not picked up by an
// already-running Run — Rerender only re-expands the existing
// membership against the new base; it never grows the template set
// Run is looping over.
func (r *Runner) Run(ctx context.Context) {
	base := r.holder.Load()

	r.mu.Lock()
	r.base = base
	r.mu.Unlock()

	var wg sync.WaitGroup
	if base != nil {
		for _, pool := range base.Pools {
			for _, tmpl := range pool.DNSTemplates {
				key := config.TemplateKey{Pool: pool.Name, Template: tmpl.Name}
				wg.Add(1)
				go func(key config.TemplateKey, tmpl config.ServerDNS) {
					defer wg.Done()
					r.loop(ctx, key, tmpl)
				}(key, tmpl)
			}
		}
	}

	<-ctx.Done()
	wg.Wait()
}

// Rerender installs a newly loaded operator config: expands it with the
// last-known membership and swaps, atomically (via mu) with the loops'
// own delta-driven swaps. The reload path calls this instead of a bare
// holder.Swap whenever the config has any server_dns templates.
func (r *Runner) Rerender(base *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.base = base
	r.holder.Swap(config.ExpandDNS(r.base, r.members))
}

// loop is one template's whole resolution lifecycle: an immediate first
// resolution (fail-static applies here too — members become available
// ~immediately on success, or stay empty/unexpanded on failure, exactly
// as a later tick would handle it), then resolve on ResolveInterval ±10%
// jitter until ctx ends.
func (r *Runner) loop(ctx context.Context, key config.TemplateKey, tmpl config.ServerDNS) {
	r.resolve(ctx, key, tmpl)

	t := time.NewTimer(jitter(tmpl.ResolveInterval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.resolve(ctx, key, tmpl)
			t.Reset(jitter(tmpl.ResolveInterval))
		}
	}
}

// resolve runs one lookup for key/tmpl and applies the design doc's
// fail-static policy: a lookup error or an empty answer counts/logs and
// returns, leaving the last-known-good membership (and therefore the
// holder) untouched. A non-empty answer always counts "ok" and reports
// the current member count; only an actual delta against the
// last-known-good set re-expands and swaps.
func (r *Runner) resolve(ctx context.Context, key config.TemplateKey, tmpl config.ServerDNS) {
	host, _, err := net.SplitHostPort(tmpl.Address)
	if err != nil {
		host = tmpl.Address
	}

	addrs, err := r.lookup(ctx, host)
	switch {
	case err != nil:
		r.metrics.Discovery(key.Template, "error")
		r.lg.Warn("dns discovery lookup failed", "pool", key.Pool, "template", key.Template, "error", err)
		return
	case len(addrs) == 0:
		r.metrics.Discovery(key.Template, "empty")
		r.lg.Warn("dns discovery answer empty, keeping last-known-good", "pool", key.Pool, "template", key.Template)
		return
	}

	if len(addrs) > maxTemplateMembers {
		r.lg.Warn("dns discovery answer exceeds member cap, will be truncated",
			"pool", key.Pool, "template", key.Template, "answered", len(addrs), "cap", maxTemplateMembers)
	}

	sorted := slices.Clone(addrs)
	slices.SortFunc(sorted, netip.Addr.Compare)
	r.metrics.Discovery(key.Template, "ok")

	r.mu.Lock()
	defer r.mu.Unlock()

	prev := r.members[key]
	r.metrics.DiscoveredMembers(key.Pool, key.Template, len(sorted))
	if slices.Equal(prev, sorted) {
		return
	}

	added, removed := diffMembers(prev, sorted)
	r.members[key] = sorted
	r.holder.Swap(config.ExpandDNS(r.base, r.members))
	r.metrics.DiscoveryChange(key.Template)
	r.lg.Info("dns discovery membership changed",
		"pool", key.Pool, "template", key.Template, "added", added, "removed", removed)
}

// diffMembers returns the addrs added/removed between prev and cur
// (both already sorted) for the membership-change log line. Walking
// each sorted input once in order keeps the output sorted too.
func diffMembers(prev, cur []netip.Addr) (added, removed []netip.Addr) {
	prevSet := make(map[netip.Addr]struct{}, len(prev))
	for _, a := range prev {
		prevSet[a] = struct{}{}
	}
	curSet := make(map[netip.Addr]struct{}, len(cur))
	for _, a := range cur {
		curSet[a] = struct{}{}
		if _, ok := prevSet[a]; !ok {
			added = append(added, a)
		}
	}
	for _, a := range prev {
		if _, ok := curSet[a]; !ok {
			removed = append(removed, a)
		}
	}
	return added, removed
}

// jitter applies the design doc's ±10% uniform jitter to base. Not
// injected behind a seeded source (unlike internal/health's jitter):
// nothing here needs deterministic draws — tests use real, short
// (10-20ms) intervals and assert on resolve/swap outcomes, not on the
// exact sleep duration.
func jitter(base time.Duration) time.Duration {
	factor := 1 + (rand.Float64()*2-1)*jitterFraction
	return time.Duration(float64(base) * factor)
}

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
	// (bifrost_dns_discovery_total{template, result}). template is
	// "pool/name" (resolve's metricsTemplate): that metric carries no
	// separate "pool" label, so this keeps it unique across pools whose
	// templates happen to share a name.
	Discovery(template string, result string)
	// DiscoveryChange reports one membership delta that caused a swap
	// (bifrost_dns_discovery_changes_total{template}). template is
	// "pool/name", exactly like Discovery.
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
// Run's template set is fixed for its own lifetime: a later Rerender
// only re-expands the existing membership against a new base, so it can
// neither start a goroutine for a template the new base adds nor stop
// one for a template the new base drops. A dropped template's loop
// keeps ticking, but resolve's orphan guard makes it inert (no swap, no
// counters, no log) the moment its key no longer matches r.base — so an
// add or a remove of a server_dns block both require a process restart
// to actually take effect; Task 4 warns on a reload that changes the
// template set.
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
// Precondition: base must not be nil — callers always pass a freshly
// loaded and validated config.Config, exactly like holder.Swap's own
// callers.
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
// holder) untouched. DNS answers are also a trust boundary: an
// unspecified (0.0.0.0/::) or multicast address is dropped before it can
// ever become a member (docs/operations.md's "DNS answers are a trust
// boundary"), and an answer left empty by that filter is treated exactly
// like the empty-answer case. A non-empty, filtered, deduplicated answer
// always counts "ok" and reports the current (possibly capped) member
// count; only an actual delta against the last-known-good set re-expands
// and swaps. The lookup itself is bounded to one ResolveInterval so a
// hung resolver can't stall this template's loop past its own tick.
func (r *Runner) resolve(ctx context.Context, key config.TemplateKey, tmpl config.ServerDNS) {
	// Orphan guard, checked before anything else — including the lookup
	// and its Discovery counter: a later Rerender may have dropped this
	// template from r.base (Run never tears down the per-template
	// goroutine for it — see Run's doc comment). Once that happens this
	// resolve must go fully inert: no lookup, no counters, no log, no
	// swap — otherwise an orphaned template's loop would keep resolving
	// and counting/logging forever. The same check is re-taken under the
	// same lock right before the swap below, to close the narrow race
	// where a Rerender lands while this call is already past this guard.
	r.mu.Lock()
	orphaned := !templateExists(r.base, key)
	r.mu.Unlock()
	if orphaned {
		return
	}

	// metricsTemplate composes "pool/name": bifrost_dns_discovery_total
	// and bifrost_dns_discovery_changes_total carry only a "template"
	// label (design doc's Observability section), with no separate "pool"
	// label to disambiguate two pools that happen to name a template the
	// same. DiscoveredMembers below is unaffected: its gauge already
	// carries pool and template as two separate labels.
	metricsTemplate := key.Pool + "/" + key.Template

	host, _, err := net.SplitHostPort(tmpl.Address)
	if err != nil {
		host = tmpl.Address
	}

	lookupCtx, cancel := context.WithTimeout(ctx, tmpl.ResolveInterval)
	defer cancel()
	addrs, err := r.lookup(lookupCtx, host)
	switch {
	case err != nil:
		r.metrics.Discovery(metricsTemplate, "error")
		r.lg.Warn("dns discovery lookup failed", "pool", key.Pool, "template", key.Template, "error", err)
		return
	case len(addrs) == 0:
		r.metrics.Discovery(metricsTemplate, "empty")
		r.lg.Warn("dns discovery answer empty, keeping last-known-good", "pool", key.Pool, "template", key.Template)
		return
	}

	// Trust-boundary filter: a sinkhole answer (0.0.0.0/::) or a
	// multicast one must never become a dialable member — see
	// docs/operations.md. An answer left with nothing safe is fail-static
	// exactly like the empty case above.
	safe := make([]netip.Addr, 0, len(addrs))
	var dropped []netip.Addr
	for _, a := range addrs {
		if a.IsUnspecified() || a.IsMulticast() {
			dropped = append(dropped, a)
			continue
		}
		safe = append(safe, a)
	}
	if len(safe) == 0 {
		r.metrics.Discovery(metricsTemplate, "empty")
		r.lg.Warn("dns discovery answer only unspecified/multicast addresses, keeping last-known-good",
			"pool", key.Pool, "template", key.Template, "dropped", dropped)
		return
	}

	sorted := slices.Clone(safe)
	slices.SortFunc(sorted, netip.Addr.Compare)
	sorted = slices.Compact(sorted) // duplicate A/AAAA records collapse to one member each

	if len(sorted) > config.MaxTemplateMembers {
		r.lg.Warn("dns discovery answer exceeds member cap, will be truncated",
			"pool", key.Pool, "template", key.Template, "answered", len(sorted), "cap", config.MaxTemplateMembers)
	}
	r.metrics.Discovery(metricsTemplate, "ok")

	r.mu.Lock()
	defer r.mu.Unlock()

	// Re-check the orphan guard above: this closes the narrow race where
	// a Rerender dropped the template between that check and this lock.
	if !templateExists(r.base, key) {
		return
	}

	prev := r.members[key]
	r.metrics.DiscoveredMembers(key.Pool, key.Template, min(len(sorted), config.MaxTemplateMembers))
	if slices.Equal(prev, sorted) {
		return
	}

	added, removed := diffMembers(prev, sorted)
	r.members[key] = sorted
	r.holder.Swap(config.ExpandDNS(r.base, r.members))
	r.metrics.DiscoveryChange(metricsTemplate)
	r.lg.Info("dns discovery membership changed",
		"pool", key.Pool, "template", key.Template, "added", added, "removed", removed)
}

// templateExists reports whether key's (pool, template) pair is still
// present in base — false once a Rerender has dropped it, which is
// resolve's signal to go inert for that key.
func templateExists(base *config.Config, key config.TemplateKey) bool {
	if base == nil {
		return false
	}
	for _, pool := range base.Pools {
		if pool.Name != key.Pool {
			continue
		}
		for _, tmpl := range pool.DNSTemplates {
			if tmpl.Name == key.Template {
				return true
			}
		}
		return false
	}
	return false
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

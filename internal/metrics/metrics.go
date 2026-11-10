// Package metrics implements Bifrost's Prometheus surface: the epic-09
// stable name/label contract in PROJECT.md's Produces block, golden-list
// tested by TestMetricNamesStable.
//
// Everything lives on a private *prometheus.Registry (never the global
// default registry), so tests can construct as many independent
// instances as they like without colliding.
//
// Two shapes, for two different sources of truth:
//
//   - Metrics is push-based: internal/proxy calls its methods at the
//     moment each traffic event happens (a session starts, a reply is
//     synthesized, a transaction concludes...). It satisfies
//     proxy.Metrics structurally — this package never imports
//     internal/proxy, the same closure-inversion internal/proxy already
//     uses for HealthSignals/PickFunc/LeaseFunc, so internal/proxy never
//     imports internal/metrics either.
//   - ServerCollector is pull-based: server_up/eligible/in_flight/
//     state_changes/probe_total all mirror state that already lives in
//     internal/health and internal/balance, so it is polled fresh at
//     /metrics scrape time (prometheus.Collector) instead of chasing it
//     with a second, independently-maintained copy. This package DOES
//     import internal/health/internal/balance/internal/config for this —
//     the reverse direction (health/balance importing metrics) would be
//     an import cycle, which is exactly why those packages only ever get
//     small read-only accessors added (Status.LastChange/StateChanges/
//     LastProbe, Checker.ProbeCounts), never a metrics call of their own.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/littlebugger/bifrost/internal/balance"
	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/health"
)

// Metrics holds Bifrost's push-based counters/gauges: the traffic-plane
// events internal/proxy's relay/session/listener report as they happen.
// A nil *Metrics is not valid; use New. The zero value of every field
// inside is never used directly — New always populates and registers
// them.
type Metrics struct {
	Registry *prometheus.Registry

	sessionsActive   *prometheus.GaugeVec
	sessionsTotal    *prometheus.CounterVec
	transactions     *prometheus.CounterVec
	synthReplies     *prometheus.CounterVec
	relayBytes       *prometheus.CounterVec
	backendDials     *prometheus.CounterVec
	backendConnReuse *prometheus.CounterVec
	duplicateRisk    prometheus.Counter

	dnsDiscovery        *prometheus.CounterVec
	dnsDiscoveryChanges *prometheus.CounterVec
	discoveredMembers   *prometheus.GaugeVec
}

// New returns a Metrics with its own private registry, every push-based
// metric created and registered.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		sessionsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bifrost_sessions_active", Help: "Client SMTP sessions currently open, by listener.",
		}, []string{"listener"}),
		sessionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bifrost_sessions_total", Help: "Client SMTP sessions accepted, cumulative, by listener.",
		}, []string{"listener"}),
		transactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bifrost_transactions_total", Help: "Mail transactions concluded, by listener, pool, server, and final verdict class.",
		}, []string{"listener", "pool", "server", "verdict_class"}),
		synthReplies: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bifrost_synthesized_replies_total", Help: "Replies Bifrost generated itself rather than relaying a backend's, by code and enhanced status.",
		}, []string{"code_enhanced"}),
		relayBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bifrost_relay_bytes_total", Help: "Bytes relayed verbatim between client and backend, by direction.",
		}, []string{"direction"}),
		backendDials: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bifrost_backend_dials_total", Help: "Backend dial attempts, by server and result.",
		}, []string{"server", "result"}),
		backendConnReuse: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bifrost_backend_conn_reuse_total", Help: "Backend connection reuse events, by server and outcome.",
		}, []string{"server", "outcome"}),
		duplicateRisk: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "bifrost_duplicate_risk_total", Help: "Transactions where a backend died after the final dot but before a verdict (the duplicate-delivery window).",
		}),
		dnsDiscovery: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bifrost_dns_discovery_total", Help: "server_dns resolution attempts, by template and result (ok/error/empty).",
		}, []string{"template", "result"}),
		dnsDiscoveryChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bifrost_dns_discovery_changes_total", Help: "server_dns membership deltas that triggered a config swap, by template.",
		}, []string{"template"}),
		discoveredMembers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bifrost_pool_discovered_members", Help: "Current member count of a server_dns template, by pool and template.",
		}, []string{"pool", "template"}),
	}
	reg.MustRegister(m.sessionsActive, m.sessionsTotal, m.transactions,
		m.synthReplies, m.relayBytes, m.backendDials, m.backendConnReuse, m.duplicateRisk,
		m.dnsDiscovery, m.dnsDiscoveryChanges, m.discoveredMembers)
	return m
}

// RegisterListener materializes bifrost_sessions_active/_total's
// listener="name" series at zero. A Vec has no child series until
// something increments it, so without this an idle fresh process scrapes
// no sessions_active row at all for a listener that has never had a
// session yet -- before these became Vecs it was a bare gauge, always
// present at 0, so a "sessions_active == 0" alert on it would silently
// stop firing. cmd/bifrost calls this once per configured listener at
// startup, and again (harmlessly -- WithLabelValues is idempotent) for
// every listener in a reloaded config.
func (m *Metrics) RegisterListener(name string) {
	m.sessionsActive.WithLabelValues(name)
	m.sessionsTotal.WithLabelValues(name)
}

// SessionStarted implements proxy.Metrics.
func (m *Metrics) SessionStarted(listener string) {
	m.sessionsActive.WithLabelValues(listener).Inc()
	m.sessionsTotal.WithLabelValues(listener).Inc()
}

// SessionEnded implements proxy.Metrics.
func (m *Metrics) SessionEnded(listener string) { m.sessionsActive.WithLabelValues(listener).Dec() }

// BackendDial implements proxy.Metrics.
func (m *Metrics) BackendDial(server string, ok bool) {
	m.backendDials.WithLabelValues(server, resultLabel(ok)).Inc()
}

// Transaction implements proxy.Metrics.
func (m *Metrics) Transaction(listener, pool, server, verdictClass string) {
	m.transactions.WithLabelValues(listener, pool, server, verdictClass).Inc()
}

// SynthesizedReply implements proxy.Metrics.
func (m *Metrics) SynthesizedReply(codeEnhanced string) {
	m.synthReplies.WithLabelValues(codeEnhanced).Inc()
}

// RelayBytes implements proxy.Metrics. n <= 0 is a no-op: Prometheus
// counters may never move backward or stay put on a zero-byte event
// worth recording elsewhere.
func (m *Metrics) RelayBytes(direction string, n int) {
	if n <= 0 {
		return
	}
	m.relayBytes.WithLabelValues(direction).Add(float64(n))
}

// DuplicateRisk implements proxy.Metrics.
func (m *Metrics) DuplicateRisk() { m.duplicateRisk.Inc() }

// BackendReuse implements proxy.Metrics.
func (m *Metrics) BackendReuse(server string, outcome string) {
	m.backendConnReuse.WithLabelValues(server, outcome).Inc()
}

// Discovery implements discover.Metrics: one server_dns resolution
// attempt's outcome. template is the caller's own label value -- Task 5's
// wiring passes "pool/name" here (discover.go) to keep it unique across
// pools, since this counter carries no separate "pool" label.
func (m *Metrics) Discovery(template, result string) {
	m.dnsDiscovery.WithLabelValues(template, result).Inc()
}

// DiscoveryChange implements discover.Metrics: one membership delta that
// triggered a holder swap.
func (m *Metrics) DiscoveryChange(template string) {
	m.dnsDiscoveryChanges.WithLabelValues(template).Inc()
}

// DiscoveredMembers implements discover.Metrics: a template's current
// member count. A gauge, not a counter -- a later call overwrites rather
// than accumulates.
func (m *Metrics) DiscoveredMembers(pool, template string, n int) {
	m.discoveredMembers.WithLabelValues(pool, template).Set(float64(n))
}

func resultLabel(ok bool) string {
	if ok {
		return "ok"
	}
	return "fail"
}

// ServerCollector is the pull half: server_up, server_eligible,
// in_flight, server_state_changes_total, and probe_total, all read fresh
// from internal/health and internal/balance at every /metrics scrape
// (prometheus.Collector.Collect), rather than tracked twice.
type ServerCollector struct {
	cfg     *config.Holder
	checker *health.Checker
	router  *balance.Router

	up, eligible, inFlight, stateChanges, probeTotal *prometheus.Desc
}

// NewServerCollector builds a ServerCollector over the given live config,
// health checker, and router. Register it once on a Metrics' Registry
// (m.Registry.MustRegister(sc)); nothing else calls its methods.
func NewServerCollector(cfg *config.Holder, checker *health.Checker, router *balance.Router) *ServerCollector {
	poolServer := []string{"pool", "server"}
	return &ServerCollector{
		cfg:     cfg,
		checker: checker,
		router:  router,
		up: prometheus.NewDesc("bifrost_server_up",
			"1 if the server's active health check currently reports it reachable, else 0.", poolServer, nil),
		eligible: prometheus.NewDesc("bifrost_server_eligible",
			"1 if the server is up, ready/no override-down, and capability-compatible -- actually eligible for traffic -- else 0.", poolServer, nil),
		inFlight: prometheus.NewDesc("bifrost_in_flight",
			"Transactions currently attached to this server.", poolServer, nil),
		stateChanges: prometheus.NewDesc("bifrost_server_state_changes_total",
			"Active-check UP/DOWN flips since start, cumulative (flap alerting).", poolServer, nil),
		probeTotal: prometheus.NewDesc("bifrost_probe_total",
			"Active health probes completed, by server, level, and result.", []string{"server", "level", "result"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *ServerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	ch <- c.eligible
	ch <- c.inFlight
	ch <- c.stateChanges
	ch <- c.probeTotal
}

// Collect implements prometheus.Collector: it walks the currently loaded
// config's pools/servers and emits one set of const metrics per server,
// read fresh from checker/router every call.
func (c *ServerCollector) Collect(ch chan<- prometheus.Metric) {
	cfg := c.cfg.Load()
	if cfg == nil {
		return
	}
	for _, pool := range cfg.Pools {
		for _, srv := range pool.Servers {
			st := c.checker.Status(pool.Name, srv.Name)
			ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, boolF(st.Op == health.OpUp), pool.Name, srv.Name)
			ch <- prometheus.MustNewConstMetric(c.eligible, prometheus.GaugeValue, boolF(c.checker.Eligible(pool.Name, srv.Name)), pool.Name, srv.Name)
			ch <- prometheus.MustNewConstMetric(c.inFlight, prometheus.GaugeValue, float64(c.router.InFlight(pool.Name, srv.Name)), pool.Name, srv.Name)
			ch <- prometheus.MustNewConstMetric(c.stateChanges, prometheus.CounterValue, float64(st.StateChanges), pool.Name, srv.Name)
			for key, n := range c.checker.ProbeCounts(pool.Name, srv.Name) {
				level, result, ok := splitProbeKey(key)
				if !ok {
					continue
				}
				ch <- prometheus.MustNewConstMetric(c.probeTotal, prometheus.CounterValue, float64(n), srv.Name, level, result)
			}
		}
	}
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// splitProbeKey reverses health.Checker.ProbeCounts' "level|result" key.
func splitProbeKey(key string) (level, result string, ok bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}

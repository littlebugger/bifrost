package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/littlebugger/bifrost/internal/admin"
	"github.com/littlebugger/bifrost/internal/balance"
	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/discover"
	"github.com/littlebugger/bifrost/internal/health"
	"github.com/littlebugger/bifrost/internal/metrics"
	"github.com/littlebugger/bifrost/internal/proxy"
)

// defaultLookup is the production internal/discover.LookupFunc: the
// stdlib resolver, A+AAAA together. A package-level var rather than a
// literal passed inline at the New call below so cmd/bifrost's own tests
// can swap in a fake resolver (main_test.go) without a config knob that
// would otherwise exist for nothing but tests. Caution: it is
// process-global, so no two tests that both drive a real Runner through
// it may run in parallel — main_test.go's override is restored via
// t.Cleanup, not scoped per-goroutine.
var defaultLookup discover.LookupFunc = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// This file is the whole assembly: every internal package meets here and
// nowhere else. It stays construction-only on purpose — the components
// were built and tested in epics 02–09, and anything resembling logic in
// here would be logic with no unit tests of its own.

// app is one assembled process. Every component reads its configuration
// from the same *config.Holder, which is what makes a reload a single
// atomic pointer swap (D14) instead of a fan-out of update calls.
type app struct {
	cfgPath string
	lg      *slog.Logger

	holder  *config.Holder
	metrics *metrics.Metrics
	checker *health.Checker
	router  *balance.Router
	relay   *proxy.Relay
	admin   *admin.Server    // nil when the config has no admin block
	runner  *discover.Runner // nil when the config has no server_dns templates

	smtpLns []namedListener
	adminLn net.Listener // nil when the config has no admin block

	// globalConns is global_maxconn's counter: ONE process-wide atomic
	// shared by every listener's proxy.Serve call, below, because the
	// cap it enforces is a process limit ("global_maxconn stays
	// global"), not a per-listener one. Allocated once here so N
	// listeners share N's worth of visibility into the same total,
	// instead of each silently getting its own cap.
	globalConns *atomic.Int64

	// certMu/certCache back every listener's GetCertificate: each
	// listener's certificate is parsed at most once per config
	// generation, not once per handshake — certEntry.gen is checked
	// against the live holder value the same way the old single-listener
	// certGen field was.
	certMu    sync.Mutex
	certCache map[string]certEntry // keyed by listener name
}

// namedListener is one configured listener's bound socket, its
// client-facing TLS config (nil when it has no certificate), and the
// config.Listener it serves. wire.go builds one per cfg.Listeners entry;
// main.go runs one proxy.Serve accept loop per entry.
type namedListener struct {
	name   string
	ln     net.Listener
	tlsCfg *tls.Config
	lst    *config.Listener
}

// certEntry is one listener's cached certificate, tagged with the config
// generation it was parsed from, plus whether currentCert has already
// logged the one-time warning for that listener having disappeared from
// the live config (renamed or removed) while its socket stays open.
type certEntry struct {
	cert        *tls.Certificate
	gen         *config.Config
	warnedStale bool
}

// newApp builds every component and opens both listeners, so a bind
// failure (or an unparseable certificate) is reported before anything
// starts serving. cfg must already be validated (config.Load's
// diagnostics checked by the caller).
func newApp(cfgPath string, cfg *config.Config, lg *slog.Logger) (*app, error) {
	holder := &config.Holder{}

	// metrics.New is called exactly once per process: admin.New registers
	// the pull-side ServerCollector on this registry, and registering the
	// same collector twice (or serving a different registry than the one
	// the relay pushes to) is a panic or a silently empty /metrics. Built
	// before the Runner below so discover.New can be handed the real
	// thing instead of a nil (no-op) Metrics.
	m := metrics.New()

	// A Runner is built only when the config actually declares a
	// server_dns template: it owns every holder swap from that point on
	// (its own doc comment), and the holder's very first published value
	// must already be the expanded form (zero members — the design doc's
	// "first-ever resolution failing at startup" case) so a template
	// never leaks unexpanded into a consumer that only ever reads
	// Pool.Servers (internal/health, internal/balance, internal/proxy).
	var runner *discover.Runner
	initial := cfg
	if hasDNSTemplates(cfg) {
		runner = discover.New(holder, defaultLookup, lg, m)
		initial = config.ExpandDNS(cfg, nil)
	}
	holder.Swap(initial)

	checker := health.New(holder, nil, lg)
	router := balance.NewRouter(holder, checker.Eligible, rand.New(rand.NewSource(time.Now().UnixNano())))

	a := &app{cfgPath: cfgPath, lg: lg, holder: holder, metrics: m, checker: checker, router: router, runner: runner, globalConns: new(atomic.Int64)}

	// router.Pick/router.Lease go in as closures, which is the whole
	// reason PickFunc/LeaseFunc are function types: internal/proxy never
	// imports internal/balance, and the dependency is inverted right here.
	a.relay = proxy.NewRelay(router.Pick, holder, lg, checker, router.Lease, m)

	if err := a.listen(cfg); err != nil {
		return nil, err
	}

	if cfg.Admin != nil {
		// a.applyReload (not a second, admin-local load/validate/swap
		// sequence) is what POST /reload drives — see its own doc comment
		// in reload.go for why there must be only one.
		a.admin = admin.New(holder, checker, router, m, a.applyReload, lg)
		adminLn, err := a.admin.Listen()
		if err != nil {
			a.closeListeners()
			return nil, fmt.Errorf("admin bind %q: %w", cfg.Admin.Bind, err)
		}
		a.adminLn = adminLn
	}
	return a, nil
}

// listen builds one net.Listener and one client-leg TLS config per entry
// in cfg.Listeners, failing fast (and unwinding every socket already
// opened) on the first bind error or unusable certificate — so a bad
// second listener never leaves a first one silently running.
func (a *app) listen(cfg *config.Config) error {
	for i := range cfg.Listeners {
		lst := &cfg.Listeners[i]
		ln, err := net.Listen("tcp", lst.Bind)
		if err != nil {
			a.closeListeners()
			return fmt.Errorf("listener %q bind %q: %w", lst.Name, lst.Bind, err)
		}

		var tlsCfg *tls.Config
		if lst.StartTLS != nil {
			tlsCfg = a.listenerTLS(lst)
			// Fail fast: config validation proves the cert/key files are
			// readable, not that they are a usable keypair.
			if _, err := a.currentCert(lst.Name); err != nil {
				_ = ln.Close()
				a.closeListeners()
				return fmt.Errorf("listener %q certificate: %w", lst.Name, err)
			}
		}
		a.smtpLns = append(a.smtpLns, namedListener{name: lst.Name, ln: ln, tlsCfg: tlsCfg, lst: lst})
		a.metrics.RegisterListener(lst.Name)
	}
	return nil
}

// closeListeners closes every bound SMTP socket. Exported to this file
// only (close, below, is the public teardown both listeners share).
func (a *app) closeListeners() {
	for _, nl := range a.smtpLns {
		_ = nl.ln.Close()
	}
}

// close releases every listener. Safe to call after a drain has already
// closed the SMTP ones.
func (a *app) close() {
	a.closeListeners()
	if a.adminLn != nil {
		_ = a.adminLn.Close()
	}
}

// adminAddr is the admin plane's bound address, or "" when there is none.
func (a *app) adminAddr() string {
	if a.adminLn == nil {
		return ""
	}
	return a.adminLn.Addr().String()
}

// listenerTLS builds the client-leg TLS config, or nil when no
// certificate is configured (STARTTLS is then neither advertised nor
// accepted — internal/proxy.Session's own rule).
//
// GetCertificate, rather than a fixed Certificates slice, is what makes
// the routine 90-day cert rotation a reload instead of a restart: it
// consults the live config holder on every handshake, so replacing the
// cert/key files on disk and sending SIGHUP (or POST /reload) puts the new
// certificate on the very next handshake. TLS sessions already
// established keep the certificate they negotiated — a live session's
// certificate cannot be changed under it, and does not need to be.
func (a *app) listenerTLS(lst *config.Listener) *tls.Config {
	return &tls.Config{
		MinVersion:     minTLSVersion(lst.StartTLS.MinVersion),
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return a.currentCert(lst.Name) },
	}
}

// listenerNamed finds the listener named name in cfg, or nil if cfg is nil
// or has none — the lookup currentCert uses to stay bound to a listener's
// name across a reload, rather than its position in the slice.
func listenerNamed(cfg *config.Config, name string) *config.Listener {
	if cfg == nil {
		return nil
	}
	for i := range cfg.Listeners {
		if cfg.Listeners[i].Name == name {
			return &cfg.Listeners[i]
		}
	}
	return nil
}

// currentCert returns the named listener's certificate, parsing it at most
// once per config generation: each cache entry's gen is checked against the
// live holder value, so a reload (the only thing that replaces it) is
// exactly what re-reads that listener's files from disk. Other listeners'
// cached entries are untouched — one listener's reload failure or cert
// rotation never invalidates another's.
func (a *app) currentCert(name string) (*tls.Certificate, error) {
	cfg := a.holder.Load()
	lst := listenerNamed(cfg, name)
	if lst == nil || lst.StartTLS == nil {
		// A reload can rename or remove a listener out from under a socket
		// that is still open (and whose GetCertificate closure is still
		// bound to its old name): docs promise that socket "keeps serving
		// under the stale name", so a cached cert from before the rename
		// is handed back here rather than failing every handshake while
		// plaintext traffic on the same socket keeps working.
		if lst == nil {
			if crt, ok := a.staleCert(name); ok {
				return crt, nil
			}
		}
		return nil, fmt.Errorf("no certificate configured for listener %q", name)
	}

	a.certMu.Lock()
	defer a.certMu.Unlock()
	if entry, ok := a.certCache[name]; ok && entry.gen == cfg {
		return entry.cert, nil
	}
	crt, err := tls.LoadX509KeyPair(lst.StartTLS.Cert, lst.StartTLS.Key)
	if err != nil {
		if entry, ok := a.certCache[name]; ok {
			// The two files are read as a pair, so a reload landing between
			// an operator's two writes sees a mismatched one. Keep serving
			// the certificate already loaded instead of failing handshakes,
			// and do not cache the failure: entry.gen is untouched, so the
			// next handshake re-reads and self-heals.
			a.lg.Warn("listener certificate unreadable; keeping the one already loaded", "listener", name, "error", err)
			return entry.cert, nil
		}
		return nil, err
	}
	if a.certCache == nil {
		a.certCache = make(map[string]certEntry)
	}
	a.certCache[name] = certEntry{cert: &crt, gen: cfg}
	a.lg.Info("listener certificate loaded", "listener", name, "cert", lst.StartTLS.Cert)
	return &crt, nil
}

// staleCert returns the certificate already cached for a listener name no
// longer present in the live config (currentCert's lst == nil case), so a
// rename or removal never breaks STARTTLS on that listener's still-open
// socket. The warning fires once per name, not once per handshake:
// certEntry.warnedStale is flipped under the same lock that reads it.
func (a *app) staleCert(name string) (*tls.Certificate, bool) {
	a.certMu.Lock()
	defer a.certMu.Unlock()
	entry, ok := a.certCache[name]
	if !ok {
		return nil, false
	}
	if !entry.warnedStale {
		entry.warnedStale = true
		a.certCache[name] = entry
		a.lg.Warn("listener renamed or removed from the live config; its still-open socket keeps serving the stale certificate", "listener", name)
	}
	return entry.cert, true
}

// hasDNSTemplates reports whether any pool in cfg declares a server_dns
// block — newApp's cue to build a discover.Runner and expand the holder's
// initial content instead of swapping cfg in verbatim.
func hasDNSTemplates(cfg *config.Config) bool {
	for _, p := range cfg.Pools {
		if len(p.DNSTemplates) > 0 {
			return true
		}
	}
	return false
}

// minTLSVersion maps the configured min_version onto crypto/tls. The enum
// is validated at load time (config.validateStartTLSFiles), so an
// unrecognized value never reaches here; an omitted one means TLS 1.2,
// matching crypto/tls' own server default.
func minTLSVersion(v string) uint16 {
	switch v {
	case "1.0":
		return tls.VersionTLS10
	case "1.1":
		return tls.VersionTLS11
	case "1.3":
		return tls.VersionTLS13
	default:
		return tls.VersionTLS12
	}
}

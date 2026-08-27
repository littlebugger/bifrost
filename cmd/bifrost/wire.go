package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/littlebugger/bifrost/internal/admin"
	"github.com/littlebugger/bifrost/internal/balance"
	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/health"
	"github.com/littlebugger/bifrost/internal/metrics"
	"github.com/littlebugger/bifrost/internal/proxy"
)

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
	admin   *admin.Server // nil when the config has no admin block

	smtpLn  net.Listener
	adminLn net.Listener // nil when the config has no admin block
	tlsCfg  *tls.Config  // nil when the listener has no certificate

	// certMu/certGen/certCache back GetCertificate: the certificate is
	// parsed once per config generation, not once per handshake.
	certMu    sync.Mutex
	certGen   *config.Config
	certCache *tls.Certificate
}

// newApp builds every component and opens both listeners, so a bind
// failure (or an unparseable certificate) is reported before anything
// starts serving. cfg must already be validated (config.Load's
// diagnostics checked by the caller).
func newApp(cfgPath string, cfg *config.Config, lg *slog.Logger) (*app, error) {
	holder := &config.Holder{}
	holder.Swap(cfg)

	// metrics.New is called exactly once per process: admin.New registers
	// the pull-side ServerCollector on this registry, and registering the
	// same collector twice (or serving a different registry than the one
	// the relay pushes to) is a panic or a silently empty /metrics.
	m := metrics.New()
	checker := health.New(holder, nil, lg)
	router := balance.NewRouter(holder, checker.Eligible, rand.New(rand.NewSource(time.Now().UnixNano())))

	a := &app{cfgPath: cfgPath, lg: lg, holder: holder, metrics: m, checker: checker, router: router}

	// router.Pick/router.Lease go in as closures, which is the whole
	// reason PickFunc/LeaseFunc are function types: internal/proxy never
	// imports internal/balance, and the dependency is inverted right here.
	a.relay = proxy.NewRelay(router.Pick, holder, lg, checker, router.Lease, m)

	a.tlsCfg = a.listenerTLS(cfg)
	if a.tlsCfg != nil {
		// Fail fast: config validation proves the cert/key files are
		// readable, not that they are a usable keypair.
		if _, err := a.currentCert(); err != nil {
			return nil, fmt.Errorf("listener certificate: %w", err)
		}
	}

	ln, err := net.Listen("tcp", cfg.Listener.Bind)
	if err != nil {
		return nil, fmt.Errorf("listener bind %q: %w", cfg.Listener.Bind, err)
	}
	a.smtpLn = ln

	if cfg.Admin != nil {
		a.admin = admin.New(cfgPath, holder, checker, router, m, lg)
		adminLn, err := a.admin.Listen()
		if err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("admin bind %q: %w", cfg.Admin.Bind, err)
		}
		a.adminLn = adminLn
	}
	return a, nil
}

// close releases both listeners. Safe to call after a drain has already
// closed the SMTP one.
func (a *app) close() {
	if a.smtpLn != nil {
		_ = a.smtpLn.Close()
	}
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
func (a *app) listenerTLS(cfg *config.Config) *tls.Config {
	if cfg.Listener.StartTLS == nil {
		return nil
	}
	return &tls.Config{
		MinVersion:     minTLSVersion(cfg.Listener.StartTLS.MinVersion),
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return a.currentCert() },
	}
}

// currentCert returns the certificate named by the live config, parsing
// it at most once per config generation: the holder's pointer identity is
// the cache key, so a reload (the only thing that replaces it) is exactly
// what re-reads the files from disk.
func (a *app) currentCert() (*tls.Certificate, error) {
	cfg := a.holder.Load()
	if cfg == nil || cfg.Listener.StartTLS == nil {
		return nil, errors.New("no listener certificate configured")
	}

	a.certMu.Lock()
	defer a.certMu.Unlock()
	if a.certCache != nil && a.certGen == cfg {
		return a.certCache, nil
	}
	crt, err := tls.LoadX509KeyPair(cfg.Listener.StartTLS.Cert, cfg.Listener.StartTLS.Key)
	if err != nil {
		if a.certCache == nil {
			return nil, err
		}
		// The two files are read as a pair, so a reload landing between an
		// operator's two writes sees a mismatched one. Keep serving the
		// certificate already loaded instead of failing handshakes, and do
		// not cache the failure: certGen is untouched, so the next
		// handshake re-reads and self-heals.
		a.lg.Warn("listener certificate unreadable; keeping the one already loaded", "error", err)
		return a.certCache, nil
	}
	a.certCache, a.certGen = &crt, cfg
	a.lg.Info("listener certificate loaded", "cert", cfg.Listener.StartTLS.Cert)
	return a.certCache, nil
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

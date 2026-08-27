// Package admin implements Bifrost's runtime admin API: the
// HAProxy-stats-socket analog over HTTP/unix (show state/stats, drain,
// force health, weight, reload, healthz) — PROJECT.md's D15 and
// docs/plans/epic-09-observability.md.
//
// Security model (normative, see PROJECT.md D15 and the epic): this
// API is unauthenticated by design and therefore loopback/unix-socket
// only. internal/config's Validate already rejects a non-loopback TCP
// bind without an explicit allow_remote = true, so admin.Server itself
// does no bind-address policing of its own — it trusts the config it is
// handed already passed validation, exactly like every other package
// downstream of internal/config.
package admin

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/revolee/bifrost/internal/balance"
	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/health"
	"github.com/revolee/bifrost/internal/metrics"
)

// shutdownGrace bounds Serve's graceful shutdown once its context is
// cancelled — an admin client that never reads must not hang process
// exit.
const shutdownGrace = 5 * time.Second

// Server is Bifrost's admin HTTP surface: one instance per process,
// wired over the same config/health/balance/metrics the rest of the
// balancer already runs.
type Server struct {
	cfgPath string // the file POST /reload re-Loads (write.go)
	cfg     *config.Holder
	checker *health.Checker
	router  *balance.Router
	m       *metrics.Metrics
	lg      *slog.Logger

	mux *http.ServeMux

	// draining is /healthz's lame-duck flag (SetDraining). It is
	// deliberately independent of any per-server health.AdminState:
	// healthz answers for the whole process, to an upstream L4 balancer
	// deciding whether to keep routing connections here at all — an
	// individual server drain (POST .../state) is a completely separate
	// axis (see health.Checker.SetAdminState).
	draining atomic.Bool
}

// New builds an admin Server over the given dependencies and registers
// every endpoint, read and write. cfgPath is the file POST /reload
// re-Loads — it should be the same path the process was started with.
// lg nil means slog.Default(). None of cfg, checker, router, m may be
// nil.
func New(cfgPath string, cfg *config.Holder, checker *health.Checker, router *balance.Router, m *metrics.Metrics, lg *slog.Logger) *Server {
	if lg == nil {
		lg = slog.Default()
	}
	// The pull half of the metrics contract (bifrost_server_up/_eligible/
	// _in_flight/_server_state_changes_total/_probe_total) has to be
	// registered on m's own registry to ever reach a real /metrics
	// scrape — Collect() is never called otherwise. Proven by
	// TestMetricsServed scraping the actual Handler(), not by
	// constructing a ServerCollector and checking it in isolation.
	m.Registry.MustRegister(metrics.NewServerCollector(cfg, checker, router))
	s := &Server{cfgPath: cfgPath, cfg: cfg, checker: checker, router: router, m: m, lg: lg, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /servers", s.handleServers)
	s.mux.HandleFunc("GET /stats", s.handleStats)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	s.mux.HandleFunc("POST /servers/{pool}/{server}/state", s.handleSetState)
	s.mux.HandleFunc("POST /servers/{pool}/{server}/health", s.handleSetOverride)
	s.mux.HandleFunc("POST /servers/{pool}/{server}/weight", s.handleSetWeight)
	s.mux.HandleFunc("POST /reload", s.handleReload)
	return s
}

// Handler returns the admin HTTP handler — for tests (httptest) and for
// Serve below.
func (s *Server) Handler() http.Handler { return s.mux }

// SetDraining flips /healthz's lame-duck flag. epic-10's SIGTERM drain
// sequence calls this first, before anything else (PROJECT.md's timeout
// table: "drain flips this FIRST").
func (s *Server) SetDraining(v bool) { s.draining.Store(v) }

// handleHealthz answers 200 while serving, 503 while draining.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if s.draining.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("draining\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// Listen binds the currently loaded config's Admin.Bind: "unix://<path>"
// (perms 0600, a stale socket from a previous run removed first) or
// "host:port" over TCP. It returns (nil, nil) when no admin plane is
// configured at all (Config.Admin == nil) — the caller's cue to skip
// serving entirely, same as config.Validate's own "no admin plane"
// warning.
func (s *Server) Listen() (net.Listener, error) {
	cfg := s.cfg.Load()
	if cfg == nil || cfg.Admin == nil {
		return nil, nil
	}
	bind := cfg.Admin.Bind
	if path, ok := strings.CutPrefix(bind, "unix://"); ok {
		_ = os.Remove(path) // a stale socket left behind by a previous, uncleanly-stopped run
		ln, err := net.Listen("unix", path)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			_ = ln.Close()
			return nil, err
		}
		return ln, nil
	}
	return net.Listen("tcp", bind)
}

// Serve runs the admin HTTP server over ln until ctx is cancelled, then
// shuts it down gracefully (bounded by shutdownGrace) and returns.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	httpSrv := &http.Server{Handler: s.mux}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = httpSrv.Shutdown(sctx)
		return nil
	}
}

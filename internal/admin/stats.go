package admin

import (
	"encoding/json"
	"net/http"

	"github.com/revolee/bifrost/internal/metrics"
)

// totalsView is GET /stats' balancer-wide half.
type totalsView struct {
	SessionsActive     float64 `json:"sessions_active"`
	SessionsTotal      float64 `json:"sessions_total"`
	DuplicateRiskTotal float64 `json:"duplicate_risk_total"`
}

// serverStats is GET /stats' per-server half.
type serverStats struct {
	Pool              string  `json:"pool"`
	Server            string  `json:"server"`
	Weight            int     `json:"weight"`
	InFlight          int     `json:"in_flight"`
	TransactionsTotal float64 `json:"transactions_total"`
	BackendDialsOK    float64 `json:"backend_dials_ok"`
	BackendDialsFail  float64 `json:"backend_dials_fail"`
}

// handleStats implements GET /stats: per pool/server counters (read from
// the Prometheus registry, the single source of truth also serving
// /metrics) plus balancer totals.
func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	mfs, err := s.m.Registry.Gather()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	totals := totalsView{
		SessionsActive:     metrics.GaugeValue(mfs, "bifrost_sessions_active", nil),
		SessionsTotal:      metrics.CounterValue(mfs, "bifrost_sessions_total", nil),
		DuplicateRiskTotal: metrics.CounterValue(mfs, "bifrost_duplicate_risk_total", nil),
	}

	cfg := s.cfg.Load()
	var servers []serverStats
	if cfg != nil {
		for _, pool := range cfg.Pools {
			for _, srv := range pool.Servers {
				weight, _ := s.router.Weight(pool.Name, srv.Name)
				servers = append(servers, serverStats{
					Pool:     pool.Name,
					Server:   srv.Name,
					Weight:   weight,
					InFlight: s.router.InFlight(pool.Name, srv.Name),
					TransactionsTotal: metrics.SumCounter(mfs, "bifrost_transactions_total",
						map[string]string{"pool": pool.Name, "server": srv.Name}),
					BackendDialsOK: metrics.CounterValue(mfs, "bifrost_backend_dials_total",
						map[string]string{"server": srv.Name, "result": "ok"}),
					BackendDialsFail: metrics.CounterValue(mfs, "bifrost_backend_dials_total",
						map[string]string{"server": srv.Name, "result": "fail"}),
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"totals": totals, "servers": servers})
}

// writeJSON writes v as the response body with the given status code.
// Every admin JSON response goes through this one function.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

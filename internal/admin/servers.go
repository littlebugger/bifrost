package admin

import (
	"net/http"
	"time"
)

// probeView is the JSON shape of health.ProbeInfo (PROJECT.md's Produces
// block: last_probe{level,result,latency,detail}).
type probeView struct {
	Level   string `json:"level"`
	Result  string `json:"result"`
	Latency string `json:"latency"`
	Detail  string `json:"detail"`
}

// serverView is one server's row in GET /servers.
type serverView struct {
	Pool         string    `json:"pool"`
	Server       string    `json:"server"`
	Op           string    `json:"op"`
	Admin        string    `json:"admin"`
	Override     string    `json:"override"`
	Incompatible bool      `json:"incompatible"`
	Weight       int       `json:"weight"`
	InFlight     int       `json:"in_flight"`
	ConsecFail   int       `json:"consec_fail"`
	LastChange   string    `json:"last_change,omitempty"` // RFC3339; omitted if never changed
	LastProbe    probeView `json:"last_probe"`
}

// serverViews walks the currently loaded config's pools/servers and
// builds one serverView per server, reading health.Checker/
// balance.Router state fresh on every call — this is the concurrent-read
// surface TestServersEndpointUnderTraffic exercises under -race.
func (s *Server) serverViews() []serverView {
	cfg := s.cfg.Load()
	if cfg == nil {
		return nil
	}
	out := make([]serverView, 0, len(cfg.Pools))
	for _, pool := range cfg.Pools {
		for _, srv := range pool.Servers {
			st := s.checker.Status(pool.Name, srv.Name)
			weight, _ := s.router.Weight(pool.Name, srv.Name) // ok=false can't happen: srv came from this same cfg
			v := serverView{
				Pool:         pool.Name,
				Server:       srv.Name,
				Op:           st.Op.String(),
				Admin:        st.Admin.String(),
				Override:     st.Override.String(),
				Incompatible: st.Incompatible,
				Weight:       weight,
				InFlight:     s.router.InFlight(pool.Name, srv.Name),
				ConsecFail:   st.ConsecFail,
				LastProbe: probeView{
					Level:   st.LastProbe.Level,
					Result:  st.LastProbe.Result,
					Latency: st.LastProbe.Latency.String(),
					Detail:  st.LastProbe.Detail,
				},
			}
			if !st.LastChange.IsZero() {
				v.LastChange = st.LastChange.Format(time.RFC3339)
			}
			out = append(out, v)
		}
	}
	return out
}

// handleServers implements GET /servers.
func (s *Server) handleServers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"servers": s.serverViews()})
}

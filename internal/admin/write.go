package admin

import (
	"encoding/json"
	"net/http"

	"github.com/hashicorp/hcl/v2"

	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/health"
)

// This file is epic-09's write half: set state/override/weight per
// server, and reload. Every handler here follows the same shape:
// decode the body, validate it (400 on a bad enum/range), resolve
// (pool, server) against the live registry (404 if it doesn't exist),
// apply, and report {"ok":true}.

// adminStates/overrideValues are the enums POST .../state and
// POST .../health accept, matching health.AdminState/Override's own
// three values each.
var adminStates = map[string]health.AdminState{
	"ready": health.AdminReady,
	"drain": health.AdminDrain,
	"maint": health.AdminMaint,
}

var overrideValues = map[string]health.Override{
	"auto":       health.OverrideAuto,
	"force-up":   health.OverrideForceUp,
	"force-down": health.OverrideForceDown,
}

// handleSetState implements POST /servers/{pool}/{server}/state.
func (s *Server) handleSetState(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	st, ok := adminStates[req.State]
	if !ok {
		writeError(w, http.StatusBadRequest, `state must be one of "ready", "drain", "maint"`)
		return
	}
	if err := s.checker.SetAdminState(r.PathValue("pool"), r.PathValue("server"), st); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetOverride implements POST /servers/{pool}/{server}/health.
func (s *Server) handleSetOverride(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Override string `json:"override"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ov, ok := overrideValues[req.Override]
	if !ok {
		writeError(w, http.StatusBadRequest, `override must be one of "auto", "force-up", "force-down"`)
		return
	}
	if err := s.checker.SetOverride(r.PathValue("pool"), r.PathValue("server"), ov); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetWeight implements POST /servers/{pool}/{server}/weight:
// runtime-only, ephemeral (PROJECT.md D15) — it never touches the
// config file and reverts on the next successful reload.
func (s *Server) handleSetWeight(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Weight int `json:"weight"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Weight < 0 || req.Weight > 256 {
		writeError(w, http.StatusBadRequest, "weight must be between 0 and 256")
		return
	}
	if err := s.router.SetWeight(r.PathValue("pool"), r.PathValue("server"), req.Weight); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReload implements POST /reload: load+validate cfgPath fresh,
// and only on success swap it in. An invalid new config never displaces
// the config currently serving traffic — Holder.Swap is the last step,
// not the first (PROJECT.md D14).
//
// A successful reload discards every runtime weight override
// (Router.ResetWeights, D15's "reverts to config on reload with a
// logged list of discarded overrides"); admin state and force overrides
// live in health.Checker, keyed by (pool, server) name, and survive
// automatically (health.Checker.Run's own reload path reuses the same
// serverHealth for a server that persists across the swap).
func (s *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	newCfg, diags := config.Load(s.cfgPath)
	if diags.HasErrors() {
		// Only the blocking diagnostics: config.Load's diags routinely
		// also carry warnings (e.g. an RFC-floor notice) that have
		// nothing to do with why this reload failed, and listing them
		// as "errors" here would bury the actual cause.
		msgs := make([]string, 0, len(diags))
		for _, d := range diags {
			if d.Severity != hcl.DiagError {
				continue
			}
			msgs = append(msgs, d.Error()) // hcl.Diagnostic.Error() includes file:line:col
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"errors": msgs})
		return
	}

	old := s.cfg.Load()
	if err := config.BindChange(old, newCfg); err != nil {
		// The listener and admin sockets are already open and v1 does not
		// re-bind them (D14), so this file is refused whole — exactly as
		// SIGHUP refuses it (cmd/bifrost/reload.go). Same status as a
		// validation failure: the new config is unprocessable, and the one
		// serving traffic is untouched.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"errors": []string{err.Error()}})
		return
	}

	s.cfg.Swap(newCfg)
	discarded := s.router.ResetWeights()
	diff := config.DiffSummary(old, newCfg)
	// Accepted-but-not-applied edits (the listener's identity, the accept
	// cap, the client-leg timeouts: read once per process) are reported
	// rather than dropped, or an operator reads "diff" and believes the
	// change is live.
	restart := config.RestartRequired(old, newCfg)
	s.lg.Info("config reloaded", "diff", diff, "discarded_weight_overrides", discarded,
		"restart_required", restart)
	writeJSON(w, http.StatusOK, map[string]any{
		"diff":                       diff,
		"discarded_weight_overrides": discarded,
		"restart_required":           restart,
	})
}

// decodeJSON decodes r's JSON body into v. net/http closes r.Body once
// the handler returns; nothing here needs to do that itself.
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// writeError writes a small {"error": msg} body with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

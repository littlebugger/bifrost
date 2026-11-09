package admin

import (
	"encoding/json"
	"net/http"

	"github.com/littlebugger/bifrost/internal/health"
)

// ReloadFunc performs one whole reload cycle — load the config file
// fresh, validate it, and swap it in only on success — and reports the
// outcome. POST /reload drives this closure instead of doing its own
// load/validate/swap, so cmd/bifrost's SIGHUP path and this endpoint are
// provably the same sequence, not two copies that can independently
// decide things like whether a swap goes through a DNS-discovery
// Runner (cmd/bifrost's applyReload is the one implementation; see its
// doc comment).
//
// errs is non-nil (and the other three return values zero) when the
// reload was rejected: one message per blocking config diagnostic, or a
// single message for a rejected bind change.
type ReloadFunc func() (diff string, discardedWeights, restartRequired, errs []string)

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

// handleReload implements POST /reload: run the one shared reload cycle
// (s.reload — cmd/bifrost's applyReload, the same method SIGHUP calls)
// and report its outcome. An invalid new config, or one that fails
// BindChange, never displaces the config currently serving traffic —
// that guarantee (PROJECT.md D14) lives entirely inside s.reload, not
// here; this handler only shapes the result into JSON. s.reload itself
// already logs the reload (success or rejection), so this handler does
// not log again.
//
// A successful reload discards every runtime weight override
// (Router.ResetWeights, D15's "reverts to config on reload with a
// logged list of discarded overrides"); admin state and force overrides
// live in health.Checker, keyed by (pool, server) name, and survive
// automatically (health.Checker.Run's own reload path reuses the same
// serverHealth for a server that persists across the swap).
func (s *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	diff, discarded, restart, errs := s.reload()
	if errs != nil {
		// Covers both a config.Load failure (one message per blocking
		// diagnostic) and a rejected bind change (one message) — s.reload
		// doesn't distinguish the two in its return shape, and neither did
		// this endpoint's response before the refactor.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"errors": errs})
		return
	}
	// Accepted-but-not-applied edits (the listener's identity, the accept
	// cap, the client-leg timeouts: read once per process) are reported
	// rather than dropped, or an operator reads "diff" and believes the
	// change is live.
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

package main

import (
	"github.com/hashicorp/hcl/v2"

	"github.com/revolee/bifrost/internal/config"
)

// reload is SIGHUP: re-read the config file, and publish it only if it is
// usable. The order is the guarantee (D14) — load, validate, check the
// binds, THEN swap — so a broken or unapplicable file never displaces the
// generation that is currently serving traffic.
//
// Nothing here has to tell the components about the new config: they all
// read it from the same holder, at the next MAIL (relay/router), the next
// scheduling tick (health checker), the next handshake (the listener's
// certificate) or the next drain (the timeout budget). That is the whole
// mechanism, and it is why a reload cannot orphan a long-lived session.
//
// POST /reload (internal/admin) runs the identical sequence; the only
// difference is that it answers the operator over HTTP instead of logging.
func (a *app) reload() {
	newCfg, diags := config.Load(a.cfgPath)
	if diags.HasErrors() {
		for _, d := range diags {
			if d.Severity == hcl.DiagError {
				a.lg.Error("reload rejected: invalid configuration", "diagnostic", d.Error())
			}
		}
		return
	}
	for _, d := range diags {
		a.lg.Warn("configuration warning", "diagnostic", d.Error())
	}

	old := a.holder.Load()
	if err := config.BindChange(old, newCfg); err != nil {
		a.lg.Error("reload rejected", "error", err, "config", a.cfgPath)
		return
	}

	// Accepted, but not everything in it takes effect: the listener's own
	// identity and the client-leg limits are read once per process. Named
	// rather than silently dropped (a "no changes" diff for an edit the
	// operator can see in the file is the worst of both).
	for _, warning := range config.RestartRequired(old, newCfg) {
		a.lg.Warn("reload applied with a limitation", "detail", warning)
	}

	a.holder.Swap(newCfg)
	// D15's survival matrix: admin state and force overrides are keyed by
	// (pool, server) inside the health checker and survive the swap on
	// their own; runtime weight overrides are deliberately discarded, and
	// which ones went is logged rather than left for an operator to
	// rediscover by watching traffic.
	discarded := a.router.ResetWeights()
	a.lg.Info("config reloaded", "diff", config.DiffSummary(old, newCfg),
		"discarded_weight_overrides", discarded)
}

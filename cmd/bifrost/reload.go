package main

import (
	"github.com/hashicorp/hcl/v2"

	"github.com/littlebugger/bifrost/internal/config"
)

// applyReload is the one reload sequence in the whole process: load the
// config file fresh, validate it, and only on success swap it in. The
// order is the guarantee (D14) — load, validate, check the binds, THEN
// swap — so a broken or unapplicable file never displaces the
// generation currently serving traffic.
//
// SIGHUP (reload, below) and POST /reload (internal/admin, via the
// admin.ReloadFunc closure newApp hands admin.New: a.applyReload itself)
// both call this exact method — not two independently-written copies of
// it — so there is exactly one place that decides whether a swap goes
// through discover.Runner.Rerender (fail-static, re-expands against
// last-known-good DNS membership) or a bare holder.Swap, and exactly one
// place that computes the post-swap diff against the holder's own
// (possibly Runner-expanded) value rather than the raw newCfg. A second
// copy of this decision is exactly how the admin path first shipped: a
// bare Swap that discarded discovered members on every admin reload and
// left Runner.base stale (see task-4-report.md's fix round).
//
// Nothing here has to tell the components about the new config: they all
// read it from the same holder, at the next MAIL (relay/router), the next
// scheduling tick (health checker), the next handshake (the listener's
// certificate) or the next drain (the timeout budget). That is the whole
// mechanism, and it is why a reload cannot orphan a long-lived session.
//
// Returns diff/discardedWeights/restartRequired on a successful swap;
// errs is non-nil (and the other three zero) when the reload was
// rejected outright — one message per blocking diagnostic for an invalid
// file, or a single message for a rejected bind change.
func (a *app) applyReload() (diff string, discardedWeights, restartRequired, errs []string) {
	newCfg, diags := config.Load(a.cfgPath)
	if diags.HasErrors() {
		for _, d := range diags {
			if d.Severity != hcl.DiagError {
				continue
			}
			a.lg.Error("reload rejected: invalid configuration", "diagnostic", d.Error())
			errs = append(errs, d.Error())
		}
		return "", nil, nil, errs
	}
	for _, d := range diags {
		a.lg.Warn("configuration warning", "diagnostic", d.Error())
	}

	old := a.holder.Load()
	if err := config.BindChange(old, newCfg); err != nil {
		a.lg.Error("reload rejected", "error", err, "config", a.cfgPath)
		return "", nil, nil, []string{err.Error()}
	}

	// Accepted, but not everything in it takes effect: the listener's own
	// identity and the client-leg limits are read once per process. Named
	// rather than silently dropped (a "no changes" diff for an edit the
	// operator can see in the file is the worst of both).
	restartRequired = config.RestartRequired(old, newCfg)
	for _, warning := range restartRequired {
		a.lg.Warn("reload applied with a limitation", "detail", warning)
	}

	// The Runner owns every holder swap once any server_dns template
	// exists (internal/discover's own doc comment): Rerender re-expands
	// newCfg against the last-known-good DNS membership and swaps it in
	// under the same mutex that serializes the Runner's own delta-driven
	// swaps, so an operator reload (SIGHUP or POST /reload) and a resolver
	// tick can never race each other, and neither entry point can ever
	// leave Runner.base stale. No Runner means no templates, so a bare
	// Swap is exactly equivalent.
	if a.runner != nil {
		a.runner.Rerender(newCfg)
	} else {
		a.holder.Swap(newCfg)
	}

	// D15's survival matrix: admin state and force overrides are keyed by
	// (pool, server) inside the health checker and survive the swap on
	// their own; runtime weight overrides are deliberately discarded, and
	// which ones went is logged rather than left for an operator to
	// rediscover by watching traffic.
	discardedWeights = a.router.ResetWeights()
	// DiffSummary reads the holder's own post-swap value rather than
	// newCfg directly: with a Runner active that value is EXPANDED (newCfg
	// plus discovered members), and old is likewise always an expanded
	// snapshot once discovery is live — diffing an expanded old against an
	// unexpanded newCfg would otherwise log every discovered member as
	// "removed" on every single reload. Without a Runner the holder holds
	// newCfg verbatim, so this is identical to the old behavior.
	diff = config.DiffSummary(old, a.holder.Load())
	a.lg.Info("config reloaded", "diff", diff, "discarded_weight_overrides", discardedWeights,
		"restart_required", restartRequired)
	return diff, discardedWeights, restartRequired, nil
}

// reload is SIGHUP's entry point. applyReload already does every bit of
// logging a reload (successful or rejected) needs, so SIGHUP has nothing
// left to do beyond triggering it — the returned values exist for POST
// /reload's sake, not this caller's.
func (a *app) reload() {
	a.applyReload()
}

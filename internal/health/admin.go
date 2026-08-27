package health

import (
	"context"
	"fmt"

	"github.com/revolee/bifrost/internal/config"
)

// SetAdminState sets (pool, server)'s admin state — READY, DRAIN, or
// MAINT. Setting MAINT also cancels that server's probe if one is
// currently in flight ("idle probe conns closed"): the scheduler itself
// (runServer, scheduler.go) is what stops dispatching further probes
// while MAINT holds.
func (c *Checker) SetAdminState(pool, server string, st AdminState) error {
	entry, err := c.lookup(pool, server)
	if err != nil {
		return err
	}

	entry.health.mu.Lock()
	entry.health.admin = st
	entry.health.mu.Unlock()

	if st == AdminMaint {
		entry.probeMu.Lock()
		if entry.probeCancel != nil {
			entry.probeCancel()
		}
		entry.probeMu.Unlock()
	}
	return nil
}

// SetOverride sets (pool, server)'s forced verdict — AUTO, FORCE_UP, or
// FORCE_DOWN.
func (c *Checker) SetOverride(pool, server string, ov Override) error {
	entry, err := c.lookup(pool, server)
	if err != nil {
		return err
	}
	entry.health.mu.Lock()
	entry.health.override = ov
	entry.health.mu.Unlock()
	return nil
}

// lookup finds server's registered entry, or an error if (pool, server)
// isn't currently registered.
func (c *Checker) lookup(pool, server string) (*serverEntry, error) {
	c.mu.Lock()
	entry, ok := c.servers[svrKey{pool: pool, server: server}]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("health: server %q in pool %q not registered", server, pool)
	}
	return entry, nil
}

// Eligible is the router's single question: is (pool, server) usable
// right now? AdminState and Incompatible gate unconditionally; Override
// then decides between the FSM's own OpState and a forced verdict.
//
// Truth table (Admin must be READY and Incompatible must be false for
// any of these rows to matter — DRAIN/MAINT/Incompatible always mean
// false regardless of Override/OpState):
//
//	Override    OpState  Eligible
//	FORCE_UP    UP       true
//	FORCE_UP    DOWN     true
//	AUTO        UP       true
//	AUTO        DOWN     false
//	FORCE_DOWN  UP       false
//	FORCE_DOWN  DOWN     false
func (c *Checker) Eligible(pool, server string) bool {
	entry, err := c.lookup(pool, server)
	if err != nil {
		return false // unregistered fails closed — Status{}'s zero value would otherwise read as UP/READY/AUTO
	}

	entry.health.mu.Lock()
	st := entry.health.snapshot()
	entry.health.mu.Unlock()

	if st.Admin != AdminReady || st.Incompatible {
		return false
	}
	return st.Override == OverrideForceUp || (st.Op == OpUp && st.Override == OverrideAuto)
}

// isMaint reports whether entry is currently admin-set to MAINT.
func (c *Checker) isMaint(entry *serverEntry) bool {
	entry.health.mu.Lock()
	defer entry.health.mu.Unlock()
	return entry.health.admin == AdminMaint
}

// probeCancellable runs one probe under a context SetAdminState(MAINT)
// can cancel mid-flight, via entry.probeCancel — registered only while
// the probe is actually running, so MAINT set at any other time has
// nothing to cancel (the scheduler's own isMaint check is what keeps it
// from dispatching a next one).
func (c *Checker) probeCancellable(ctx context.Context, entry *serverEntry, srv *config.Server, params config.CheckParams, caps []string) probeResult {
	pctx, cancel := context.WithCancel(ctx)
	entry.probeMu.Lock()
	entry.probeCancel = cancel
	entry.probeMu.Unlock()

	res := c.probe(pctx, srv, params, caps)

	entry.probeMu.Lock()
	entry.probeCancel = nil
	entry.probeMu.Unlock()
	cancel() // release pctx's resources regardless of how the probe ended

	return res
}

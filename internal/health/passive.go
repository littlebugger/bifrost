package health

import "github.com/littlebugger/bifrost/internal/config"

// DialFailure implements proxy.HealthSignals: an attach attempt that
// never became a usable backend leg.
func (c *Checker) DialFailure(srv *config.Server) { c.recordPassive(srv, true) }

// TransportError implements proxy.HealthSignals: a leg that was usable
// and then failed.
func (c *Checker) TransportError(srv *config.Server) { c.recordPassive(srv, true) }

// Success implements proxy.HealthSignals: the backend leg behaved,
// whatever verdicts it gave. It resets the passive failure streak; it
// never brings a DOWN server back up (see fsm.recordPassive) — that is
// the active-only-recovery invariant.
func (c *Checker) Success(srv *config.Server) { c.recordPassive(srv, false) }

// recordPassive resolves srv (a pointer into whatever config generation
// the caller last loaded) to its registered entry via byPtr, and applies
// the signal to that entry's health. An unresolvable srv (nil, or not
// currently registered — a reload race, or a server the relay picked
// from a config generation this Checker never saw) is a no-op: there is
// nothing to record against. That drop is accepted, by-design behavior
// (not an error), but still worth a Debug line — operators can otherwise
// have no idea it's happening at all.
func (c *Checker) recordPassive(srv *config.Server, fail bool) {
	if srv == nil {
		return
	}
	c.mu.Lock()
	key, ok := c.byPtr[srv]
	var entry *serverEntry
	if ok {
		entry = c.servers[key]
	}
	c.mu.Unlock()
	if entry == nil {
		n := c.passiveDropped.Add(1)
		c.lg.Debug("health: passive signal dropped, server not registered",
			"server", srv.Name, "address", srv.Address, "total_dropped", n)
		return
	}

	entry.health.mu.Lock()
	defer entry.health.mu.Unlock()
	before := entry.health.fsm.op
	entry.health.fsm.recordPassive(fail)
	if entry.health.fsm.op != before {
		entry.health.lastChange = c.clk.Now()
		entry.health.stateChanges++
	}
}

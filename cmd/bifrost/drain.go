package main

import (
	"time"

	"github.com/littlebugger/bifrost/internal/config"
)

// goodbyeGrace bounds process exit once the drain has said everything it
// has to say: PROJECT.md's "goodbye-write grace" row — how long a session
// gets to write its synthesized reply and close before the process stops
// waiting for it at all (see serve's stop, main.go).
const goodbyeGrace = 5 * time.Second

// drain is PROJECT.md's shutdown sequence, in its exact order, and the
// order is the point:
//
//  1. /healthz answers 503 FIRST, before anything stops working, so an
//     upstream L4 balancer can take this process out of rotation while it
//     is still perfectly able to serve what it already has.
//  2. lame duck: long enough for that upstream's own poll to notice.
//  3. cancelSessions, below, fires both of the next two at once: every
//     listener closes — no new connections — because each proxy.Serve's
//     own ctx watcher closes its ln the moment ctx.Done() fires (no
//     separate close loop here: closing a listener a second time once
//     Serve's watcher already has is just a spurious "closing the smtp
//     listener" warning, N times over, for every listener in the
//     config); AND every session already open is told the process is
//     going away: one sitting between commands answers 421 4.3.0 and
//     closes now, one inside a transaction finishes it first (the
//     TxnHandler owns the connection until it returns, which is exactly
//     what "in-flight transactions finish" means).
//  4. wait for those transactions.
//  5. force deadline: backend legs are aborted FIRST. An abort is a bare
//     disconnect, never a terminator a backend could mistake for a
//     completed message, so a message still streaming is cleanly
//     abandoned rather than half-delivered — and only then does each
//     session answer its own client (451 for the aborted transaction,
//     then the 421 4.3.0 above) and close.
//  6. the goodbye grace, and then exit regardless — serve's own stop owns
//     that step (a client that keeps feeding bytes can hold its session
//     open past every deadline here, and a shutdown that waits for it is
//     a shutdown that hangs).
//
// cancelSessions cancels the context proxy.Serve and every Session run
// under; serveDone is closed when proxy.Serve returns, which it only does
// once every session goroutine it started has finished. app.close (serve's
// own deferred teardown) remains the backstop that closes every listener
// socket on every exit path, drain or not — this function does not need
// to do it too.
func (a *app) drain(cancelSessions func(), serveDone <-chan struct{}) {
	t := a.timeouts()
	if a.admin != nil {
		a.admin.SetDraining(true)
	}
	a.lg.Info("draining", "lame_duck", t.LameDuck, "drain_timeout", t.DrainTimeout)

	if t.LameDuck > 0 {
		time.Sleep(t.LameDuck)
	}
	cancelSessions()

	if waitFor(serveDone, t.DrainTimeout) {
		a.lg.Info("drain complete: every session ended")
		return
	}

	// The wait that follows belongs to serve's stop (the goodbye grace,
	// which bounds process exit as a whole): waiting for the same channel
	// twice would double the shutdown's worst case for no gain.
	a.lg.Warn("drain force deadline reached; aborting in-flight backend legs",
		"legs", a.relay.CloseLegs(), "drain_timeout", t.DrainTimeout)
}

// waitFor reports whether done was closed within d. A non-positive d
// waits forever: a drain_timeout of zero can only come from a hand-built
// config (validation rejects it in a file), and waiting is the safer
// reading of "no deadline" than aborting live transactions immediately.
func waitFor(done <-chan struct{}, d time.Duration) bool {
	if d <= 0 {
		<-done
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// timeouts is the live config's timeout budget, read at the moment it is
// needed: a reload that retuned lame_duck or drain_timeout applies to the
// next drain, not only to the next process.
func (a *app) timeouts() config.Timeouts {
	if cfg := a.holder.Load(); cfg != nil {
		return cfg.Defaults.Timeouts
	}
	return config.Timeouts{}
}

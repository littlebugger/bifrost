// Package health implements Bifrost's active health checking:
// HAProxy-semantics rise/fall state machine, the probe ladder (L0-L3),
// a jittered per-server scheduler, passive transport signals with
// active-only recovery, and admin states/overrides. See PROJECT.md's R2
// and docs/plans/epic-06-health.md.
package health

import (
	"sync"
	"time"

	"github.com/revolee/bifrost/internal/config"
)

// fastinter is the probe cadence while a server is transitioning (a
// rise/fall streak in progress but not yet crossed) or has never been
// checked at all. It is a package constant, not a config knob: HAProxy
// calls this "fastinter" and Bifrost keeps the name and the fixed 1s
// value documented in the epic.
const fastinter = 1 * time.Second

// initState is the op state a server starts in: selectable. There is no
// "fully_down until first check" option in v1 — the first failed probe
// is what downs a server, exactly like any later one.
const initState = OpUp

// errorLimit is how many consecutive passive failure signals (see
// fsm.recordPassive, passive.go) synthesize one active-equivalent failed
// check. Package constant, not configurable.
const errorLimit = 10

// OpState is a server's active-check verdict: whether the rise/fall
// state machine currently considers it reachable.
type OpState int

// OpUp and OpDown are OpState's only two values: reachable/unreachable
// per the FSM's own accounting.
const (
	OpUp OpState = iota
	OpDown
)

func (s OpState) String() string {
	if s == OpUp {
		return "UP"
	}
	return "DOWN"
}

// AdminState is an operator's ready/drain/maint setting for a server —
// orthogonal to OpState (see admin.go's truth table).
type AdminState int

// AdminReady, AdminDrain, and AdminMaint are AdminState's three values.
const (
	AdminReady AdminState = iota
	AdminDrain
	AdminMaint
)

func (s AdminState) String() string {
	switch s {
	case AdminDrain:
		return "DRAIN"
	case AdminMaint:
		return "MAINT"
	default:
		return "READY"
	}
}

// Override is an operator's forced verdict, bypassing the FSM's own
// op state — orthogonal to AdminState.
type Override int

// OverrideAuto (OpState governs), OverrideForceUp (eligible regardless
// of OpState), and OverrideForceDown (ineligible regardless of OpState)
// are Override's three values.
const (
	OverrideAuto Override = iota
	OverrideForceUp
	OverrideForceDown
)

func (o Override) String() string {
	switch o {
	case OverrideForceUp:
		return "FORCE_UP"
	case OverrideForceDown:
		return "FORCE_DOWN"
	default:
		return "AUTO"
	}
}

// Status is a snapshot of one server's complete health record, returned
// by Checker.Status.
type Status struct {
	Op           OpState
	Admin        AdminState
	Override     Override
	Incompatible bool
	ConsecFail   int
	ConsecOK     int

	// LastChange is when Op last flipped UP<->DOWN (the admin API's "DOWN
	// since when" — epic-09). It is also stamped on the very first active
	// probe result ever recorded, even though Op itself may not have
	// changed from its initial value, so "since when has this been UP" is
	// answerable from the first check onward. Zero until then.
	LastChange time.Time

	// StateChanges counts every Op flip since this server was registered —
	// epic-09's bifrost_server_state_changes_total (flap alerting).
	// internal/health never imports internal/metrics (see epic-09's
	// deviation note): this is a plain read-only counter, polled from
	// outside.
	StateChanges int64

	// LastProbe is the most recent ACTIVE probe's outcome; the zero value
	// (Level == "") means no active probe has completed yet. Passive
	// signals (passive.go) never update this.
	LastProbe ProbeInfo
}

// ProbeInfo is one active probe attempt's outcome, as surfaced by the
// admin API's /servers endpoint (epic-09).
type ProbeInfo struct {
	Level   string // "connect" | "banner" | "ehlo" | "deep"
	Result  string // "ok" | "fail" | "incompatible"
	Latency time.Duration
	// Detail is the probe's own short reason label (probeResult.reason):
	// empty on a clean pass, e.g. "wrong-banner" or "handshake: <err>" on
	// failure/incompatibility -- not a verbatim capture of backend reply
	// bytes (ponytail: reusing the existing internal label is the small
	// diff; capturing raw banner text would need deeper probe.go plumbing
	// and no test needs it yet).
	Detail string
}

// fsm is one server's rise/fall active-check state machine: HAProxy
// semantics, applied per PROJECT.md/the epic. It is pure — no
// goroutines, no I/O, no wall clock — so it is exhaustively unit-tested
// on plain values; the scheduler (scheduler.go) is what actually calls
// it from a goroutine on a timer.
//
// ConsecOK/ConsecFail are "current streak" counters: a success always
// zeroes the failure streak (and vice versa), so interleaved results
// never accumulate across the opposite outcome. Only one of the two is
// ever nonzero at a time.
type fsm struct {
	op           OpState
	consecOK     int
	consecFail   int
	checked      bool // false until the first ACTIVE probe result ever recorded
	rise, fall   int
	passiveFails int // consecutive passive failure signals since the last success/synthetic-failure (see recordPassive)
}

// newFSM returns a fresh fsm at initState (selectable, unchecked).
func newFSM(rise, fall int) fsm {
	return fsm{op: initState, rise: rise, fall: fall}
}

// recordActive applies one active probe's pass/fail result. It is the
// only thing that ever sets checked=true — passive signals (passive.go)
// influence the same counters via apply, but never mark the server
// "checked": that flag exists purely to bootstrap active scheduling
// cadence (see nextInterval), and a passive signal about live traffic is
// not an active probe having run.
func (f *fsm) recordActive(ok bool) {
	f.checked = true
	f.apply(ok)
}

// apply is the counter math shared by recordActive and passive.go's
// error-limit synthetic failure: a success resets the failure streak and
// grows the success streak (crossing rise while DOWN flips it UP); a
// failure resets the success streak and grows the failure streak
// (crossing fall while UP flips it DOWN). Recovery (DOWN->UP) only ever
// happens through this function with ok=true, and passive.go never calls
// it with ok=true (see passive.go's doc comment) — that is the
// active-only-recovery invariant.
func (f *fsm) apply(ok bool) {
	if ok {
		f.consecFail = 0
		f.consecOK++
		if f.op == OpDown && f.consecOK >= f.rise {
			f.op = OpUp
		}
		return
	}
	f.consecOK = 0
	f.consecFail++
	if f.op == OpUp && f.consecFail >= f.fall {
		f.op = OpDown
	}
}

// recordPassive applies one passive transport signal (see passive.go):
// fail=false (Success) just resets the consecutive-failure streak;
// fail=true grows it, and on reaching errorLimit synthesizes exactly one
// active-equivalent failure through apply(false) — the passive path's
// only way to influence op state — then resets the streak so the next
// errorLimit run produces the next synthetic failure, not every event
// after the first. It never calls apply(true): passive signals only
// ever push DOWN-ward or accelerate; recovery is active-only (see
// apply's doc comment).
func (f *fsm) recordPassive(fail bool) {
	if !fail {
		f.passiveFails = 0
		return
	}
	f.passiveFails++
	if f.passiveFails >= errorLimit {
		f.passiveFails = 0
		f.apply(false)
	}
}

// nextInterval is the delay the scheduler should wait before this
// server's next probe, per the epic's interval table:
//
//	unchecked                        -> fastinter
//	steady UP   (op=UP,   streak=0)  -> p.Interval
//	transitional-down (op=UP,  consecFail>0) -> fastinter
//	steady DOWN (op=DOWN, streak=0)  -> p.DownInterval
//	transitional-up   (op=DOWN, consecOK>0)  -> fastinter
func (f *fsm) nextInterval(p config.CheckParams) time.Duration {
	switch {
	case !f.checked:
		return fastinter
	case f.op == OpUp && f.consecFail == 0:
		return p.Interval
	case f.op == OpUp:
		return fastinter
	case f.op == OpDown && f.consecOK == 0:
		return p.DownInterval
	default:
		return fastinter
	}
}

// serverHealth is one server's complete health record: the FSM, the
// admin/override axes, and the capability verdict, each independently
// mutable — Checker keys one of these per (pool, server) identity. Its
// own mutex (not Checker's) guards every field, so probing one server
// never contends with admin/passive/status calls on another.
type serverHealth struct {
	mu           sync.Mutex
	fsm          fsm
	admin        AdminState
	override     Override
	incompatible bool

	lastChange   time.Time
	stateChanges int64
	lastProbe    ProbeInfo

	// probeCounts accumulates every active probe's outcome, keyed by
	// "level|result" -- bifrost_probe_total's source (epic-09), polled by
	// internal/metrics via Checker.ProbeCounts. nil until the first probe.
	probeCounts map[string]int64
}

func newServerHealth(rise, fall int) *serverHealth {
	return &serverHealth{fsm: newFSM(rise, fall)}
}

// snapshot builds the Status value Checker.Status returns. Callers must
// hold h.mu.
func (h *serverHealth) snapshot() Status {
	return Status{
		Op:           h.fsm.op,
		Admin:        h.admin,
		Override:     h.override,
		Incompatible: h.incompatible,
		ConsecFail:   h.fsm.consecFail,
		ConsecOK:     h.fsm.consecOK,
		LastChange:   h.lastChange,
		StateChanges: h.stateChanges,
		LastProbe:    h.lastProbe,
	}
}

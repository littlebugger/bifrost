package health

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/revolee/bifrost/internal/config"
)

// probeFunc is the ladder's call signature — a seam so the scheduler's
// own unit tests substitute a fake and never touch a real socket.
// runProbe (probe.go) is the real implementation New wires in.
type probeFunc func(ctx context.Context, srv *config.Server, params config.CheckParams, requiredCaps []string) probeResult

// svrKey identifies a server by its stable (pool, server) name pair —
// not by *config.Server pointer, which a reload replaces wholesale.
// This is the identity D15's reload-survival matrix is keyed on.
type svrKey struct {
	pool   string
	server string
}

// serverEntry is one registered server's scheduling state: its health
// record plus the CURRENT config's view of it, refreshed in place by
// sync on every reload (so a persisting server keeps the same
// serverEntry, and therefore the same serverHealth, across a config
// swap). srv/params are guarded by Checker.mu, exactly like the
// c.servers map itself — not by health.mu, which guards only the
// health record.
type serverEntry struct {
	health *serverHealth
	srv    *config.Server
	params config.CheckParams

	// probeMu/probeCancel let admin.go's SetAdminState(MAINT) interrupt
	// a probe currently in flight for this server ("idle probe conns
	// closed") — a separate mutex from health.mu, since this changes on
	// every probe cycle (hot path) while health.mu guards the much
	// less frequently written admin/override/fsm fields.
	probeMu     sync.Mutex
	probeCancel context.CancelFunc
}

// Checker is Bifrost's active health checker: it owns one serverEntry
// per (pool, server), a jittered per-server probing goroutine for each,
// and the passive/admin surfaces the relay and the admin API call into.
type Checker struct {
	cfgH  *config.Holder
	clk   Clock
	lg    *slog.Logger
	probe probeFunc

	mu           sync.Mutex
	servers      map[svrKey]*serverEntry
	byPtr        map[*config.Server]svrKey
	listenerCaps []string

	randMu  sync.Mutex
	randSrc *rand.Rand

	wg sync.WaitGroup

	// passiveDropped counts passive signals recordPassive (passive.go)
	// couldn't resolve to a registered entry — observability only, for
	// the accepted silent-drop semantics; never read back into any
	// decision.
	passiveDropped atomic.Int64
}

// New returns a Checker wired to the real probe ladder. clk nil means
// the real wall clock; lg nil means slog.Default().
func New(cfgH *config.Holder, clk Clock, lg *slog.Logger) *Checker {
	return newChecker(cfgH, clk, lg, runProbe)
}

// newChecker is New's actual constructor, with the prober injected —
// the seam this package's own scheduler tests use to avoid real
// sockets.
func newChecker(cfgH *config.Holder, clk Clock, lg *slog.Logger, probe probeFunc) *Checker {
	if clk == nil {
		clk = NewClock()
	}
	if lg == nil {
		lg = slog.Default()
	}
	c := &Checker{
		cfgH:    cfgH,
		clk:     clk,
		lg:      lg,
		probe:   probe,
		servers: make(map[svrKey]*serverEntry),
		randSrc: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	if cfg := cfgH.Load(); cfg != nil {
		c.sync(cfg)
	}
	return c
}

// Status returns a snapshot of one server's health record, or the zero
// Status if (pool, server) isn't currently registered (removed by a
// reload, or never existed).
func (c *Checker) Status(pool, server string) Status {
	c.mu.Lock()
	entry, ok := c.servers[svrKey{pool: pool, server: server}]
	c.mu.Unlock()
	if !ok {
		return Status{}
	}
	entry.health.mu.Lock()
	defer entry.health.mu.Unlock()
	return entry.health.snapshot()
}

// Run starts one probing goroutine per registered server and blocks
// until ctx is cancelled, detecting and applying config reloads
// (new/removed servers) along the way. It returns only after every
// per-server goroutine it started has exited.
func (c *Checker) Run(ctx context.Context) {
	c.mu.Lock()
	last := c.cfgH.Load()
	for key, entry := range c.servers {
		c.wg.Add(1)
		go c.runServer(ctx, key, entry)
	}
	c.mu.Unlock()

	t := c.clk.NewTimer(fastinter)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.wg.Wait()
			return
		case <-t.C():
			if cur := c.cfgH.Load(); cur != last {
				last = cur
				c.reload(ctx, cur)
			}
			t = c.clk.NewTimer(fastinter)
		}
	}
}

// reload applies a newly-observed config: sync updates the registry in
// place (persisting servers keep their serverEntry/serverHealth; new
// ones get a fresh entry; removed ones are dropped), then a goroutine is
// started for each key sync actually added.
func (c *Checker) reload(ctx context.Context, cfg *config.Config) {
	c.mu.Lock()
	before := make(map[svrKey]struct{}, len(c.servers))
	for key := range c.servers {
		before[key] = struct{}{}
	}
	c.mu.Unlock()

	c.sync(cfg)

	c.mu.Lock()
	var toStart []svrKey
	for key := range c.servers {
		if _, existed := before[key]; !existed {
			toStart = append(toStart, key)
		}
	}
	entries := make(map[svrKey]*serverEntry, len(toStart))
	for _, key := range toStart {
		entries[key] = c.servers[key]
	}
	c.mu.Unlock()

	for key, entry := range entries {
		c.wg.Add(1)
		go c.runServer(ctx, key, entry)
	}
}

// sync reconciles the registry against cfg: every (pool, server) in cfg
// gets a serverEntry (reusing the existing one, and therefore its
// serverHealth, when that identity already exists — D15's reload
// survival), refreshed to cfg's current srv pointer and resolved
// CheckParams; any (pool, server) no longer in cfg is dropped outright.
func (c *Checker) sync(cfg *config.Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cfg == nil {
		return
	}
	c.listenerCaps = cfg.Listener.Capabilities

	want := make(map[svrKey]struct{})
	for pi := range cfg.Pools {
		pool := &cfg.Pools[pi]
		for si := range pool.Servers {
			srv := &pool.Servers[si]
			key := svrKey{pool: pool.Name, server: srv.Name}
			want[key] = struct{}{}

			entry, ok := c.servers[key]
			if !ok {
				entry = &serverEntry{health: newServerHealth(srv.Check.Rise, srv.Check.Fall)}
				c.servers[key] = entry
			}
			entry.srv = srv
			entry.params = srv.Check
		}
	}
	for key := range c.servers {
		if _, ok := want[key]; !ok {
			delete(c.servers, key)
		}
	}

	c.byPtr = make(map[*config.Server]svrKey, len(c.servers))
	for key, entry := range c.servers {
		c.byPtr[entry.srv] = key
	}
}

// runServer is one server's whole scheduling lifecycle: an initial
// jittered spread, then probe/apply/sleep forever until ctx is
// cancelled or a reload drops this server's identity from the registry
// (currentTarget/applyResult both re-check c.servers[key] == entry —
// the "generation token" — so a probe result computed after that has
// happened is discarded, no state mutated, and this goroutine retires
// instead of scheduling another round). A server admin-set to MAINT is
// skipped entirely (admin.go): the scheduler just re-checks at fastinter
// cadence until it leaves MAINT, without ever calling the prober.
func (c *Checker) runServer(ctx context.Context, key svrKey, entry *serverEntry) {
	defer c.wg.Done()

	_, initParams, _, ok := c.currentTarget(key, entry)
	if !ok {
		return
	}
	if !c.sleep(ctx, initialOffset(c.randFloat(), initParams.Interval)) {
		return
	}

	for {
		srv, params, caps, ok := c.currentTarget(key, entry)
		if !ok {
			return
		}

		if c.isMaint(entry) {
			if !c.sleep(ctx, fastinter) {
				return
			}
			continue
		}

		start := c.clk.Now()
		res := c.probeCancellable(ctx, entry, srv, params, caps)
		latency := c.clk.Now().Sub(start)

		next, ok := c.applyResult(key, entry, params, res, latency)
		if !ok {
			return
		}
		if !c.sleep(ctx, jitter(next, c.randFloat())) {
			return
		}
	}
}

// currentTarget returns entry's dial target and the caps to require,
// plus whether entry is still the one registered under key — false
// means a reload superseded or removed it, and the caller must retire
// without probing.
func (c *Checker) currentTarget(key svrKey, entry *serverEntry) (*config.Server, config.CheckParams, []string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.servers[key] != entry {
		return nil, config.CheckParams{}, nil, false
	}
	return entry.srv, entry.params, c.listenerCaps, true
}

// applyResult records one completed probe's result against entry's
// health, but only if entry is still current (see currentTarget's
// comment) — a stale result (its server removed, or superseded, by a
// reload while the probe was in flight) is discarded untouched. It
// returns the next interval to sleep and whether the result was applied
// at all.
func (c *Checker) applyResult(key svrKey, entry *serverEntry, params config.CheckParams, res probeResult, latency time.Duration) (time.Duration, bool) {
	c.mu.Lock()
	stillCurrent := c.servers[key] == entry
	c.mu.Unlock()
	if !stillCurrent {
		return 0, false
	}

	entry.health.mu.Lock()
	defer entry.health.mu.Unlock()
	before, beforeChecked := entry.health.fsm.op, entry.health.fsm.checked
	entry.health.incompatible = res.incompatible
	entry.health.fsm.recordActive(res.ok)
	flipped := entry.health.fsm.op != before
	if flipped {
		entry.health.stateChanges++
	}
	if flipped || !beforeChecked {
		// The first-ever check also stamps LastChange, even though Op
		// did not visibly flip: "since when has this been UP" must be
		// answerable from the first check onward, but establishing the
		// initial state is not itself a flap.
		entry.health.lastChange = c.clk.Now()
	}

	level := probeLevel(params)
	result := probeResultLabel(res)
	entry.health.lastProbe = ProbeInfo{Level: level, Result: result, Latency: latency, Detail: res.reason}
	if entry.health.probeCounts == nil {
		entry.health.probeCounts = make(map[string]int64)
	}
	entry.health.probeCounts[level+"|"+result]++

	return entry.health.fsm.nextInterval(params), true
}

// probeLevel normalizes params.Level for reporting: "" (never resolved
// through defaults, e.g. a hand-built CheckParams in a test) reads the
// same as an explicit "ehlo" -- runProbe's own default branch (probe.go).
func probeLevel(params config.CheckParams) string {
	if params.Level == "" {
		return "ehlo"
	}
	return params.Level
}

// probeResultLabel classifies a probeResult for LastProbe.Result and
// ProbeCounts: "incompatible" is op-wise a pass (see probeResult's own
// doc) but a distinct, third outcome operators care about separately
// from a plain "ok".
func probeResultLabel(res probeResult) string {
	switch {
	case res.incompatible:
		return "incompatible"
	case res.ok:
		return "ok"
	default:
		return "fail"
	}
}

// ProbeCounts returns a copy of (pool, server)'s cumulative active-probe
// outcome counts, keyed by "level|result" — bifrost_probe_total's source
// (epic-09), or nil if (pool, server) isn't currently registered or has
// never completed a probe.
func (c *Checker) ProbeCounts(pool, server string) map[string]int64 {
	c.mu.Lock()
	entry, ok := c.servers[svrKey{pool: pool, server: server}]
	c.mu.Unlock()
	if !ok {
		return nil
	}
	entry.health.mu.Lock()
	defer entry.health.mu.Unlock()
	if entry.health.probeCounts == nil {
		return nil
	}
	out := make(map[string]int64, len(entry.health.probeCounts))
	for k, v := range entry.health.probeCounts {
		out[k] = v
	}
	return out
}

// sleep waits for d (a no-op returning true immediately for d<=0),
// reporting false if ctx was cancelled first.
func (c *Checker) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := c.clk.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C():
		return true
	}
}

// randFloat draws a value in [0,1) from the Checker's own rand source —
// shared and mutex-guarded because every server's goroutine calls it
// independently.
func (c *Checker) randFloat() float64 {
	c.randMu.Lock()
	defer c.randMu.Unlock()
	return c.randSrc.Float64()
}

// jitter applies the epic's ±5% uniform jitter to base, using f (a
// value in [0,1)) as the draw.
func jitter(base time.Duration, f float64) time.Duration {
	factor := 1 + (f*2-1)*0.05
	return time.Duration(float64(base) * factor)
}

// initialSpreadWindow is min(interval, 5s): the window a server's very
// first probe is spread over, so N servers don't all probe in lockstep.
func initialSpreadWindow(interval time.Duration) time.Duration {
	const cap5s = 5 * time.Second
	if interval < cap5s {
		return interval
	}
	return cap5s
}

// initialOffset returns a server's first-probe delay: f (a value in
// [0,1)) scaled into [0, initialSpreadWindow(interval)).
func initialOffset(f float64, interval time.Duration) time.Duration {
	return time.Duration(f * float64(initialSpreadWindow(interval)))
}

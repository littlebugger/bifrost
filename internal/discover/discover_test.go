package discover

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/littlebugger/bifrost/internal/config"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// testBase builds a hand-rolled base Config with one pool and one
// server_dns template — bypassing config.Load entirely, since its
// ResolveInterval validation floor (1s) is far above the 10-20ms
// intervals these tests need to run fast.
func testBase(pool, tmpl, address string, interval time.Duration) *config.Config {
	return &config.Config{
		Pools: []config.Pool{
			{
				Name: pool,
				DNSTemplates: []config.ServerDNS{
					{Name: tmpl, Address: address, ResolveInterval: interval, Weight: 1},
				},
			},
		},
	}
}

func addrsOf(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func poolNamed(cfg *config.Config, name string) *config.Pool {
	for i := range cfg.Pools {
		if cfg.Pools[i].Name == name {
			return &cfg.Pools[i]
		}
	}
	return nil
}

func hasServer(servers []config.Server, name string) bool {
	for _, s := range servers {
		if s.Name == name {
			return true
		}
	}
	return false
}

// waitForSnapshot bounded-polls h until it holds a pointer other than
// prev, the same style internal/health's waitForTimers uses to
// synchronize with another goroutine without a fixed sleep standing in
// for the thing under test.
func waitForSnapshot(t *testing.T, h *config.Holder, prev *config.Config) *config.Config {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cur := h.Load(); cur != prev {
			return cur
		}
		if time.Now().After(deadline) {
			t.Fatal("holder snapshot never changed")
		}
		time.Sleep(time.Millisecond)
	}
}

// lookupStub is an injectable LookupFunc whose answer/error can be
// changed mid-test, signaling each invocation on a buffered channel so
// tests can wait for "the Nth resolution happened" instead of sleeping.
type lookupStub struct {
	mu    sync.Mutex
	addrs []netip.Addr
	err   error
	calls chan struct{}
}

func newLookupStub(addrs []netip.Addr) *lookupStub {
	s := &lookupStub{addrs: addrs, calls: make(chan struct{}, 1024)}
	return s
}

func (s *lookupStub) set(addrs []netip.Addr, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addrs, s.err = addrs, err
}

func (s *lookupStub) fn(_ context.Context, _ string) ([]netip.Addr, error) {
	s.mu.Lock()
	addrs, err := s.addrs, s.err
	s.mu.Unlock()
	s.calls <- struct{}{}
	return addrs, err
}

func (s *lookupStub) waitCall(t *testing.T) {
	t.Helper()
	select {
	case <-s.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("lookup was never called")
	}
}

// fakeMetrics is a test-only discover.Metrics: it records exactly the
// two calls these tests assert on — Discovery's result label and
// DiscoveredMembers' gauge value — each on its own buffered channel so a
// test can wait for "the Nth call happened" instead of sleeping.
// DiscoveryChange needs no recording yet; the embedded noMetrics' no-op
// covers it.
type fakeMetrics struct {
	noMetrics
	results chan string
	members chan int
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{results: make(chan string, 1024), members: make(chan int, 1024)}
}

func (f *fakeMetrics) Discovery(_, result string)           { f.results <- result }
func (f *fakeMetrics) DiscoveredMembers(_, _ string, n int) { f.members <- n }

func (f *fakeMetrics) waitResult(t *testing.T) string {
	t.Helper()
	select {
	case r := <-f.results:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("Discovery was never reported")
		return ""
	}
}

func (f *fakeMetrics) waitMembers(t *testing.T) int {
	t.Helper()
	select {
	case n := <-f.members:
		return n
	case <-time.After(2 * time.Second):
		t.Fatal("DiscoveredMembers was never reported")
		return 0
	}
}

// TestInitialResolvePopulatesMembers proves the first resolution runs
// inline (before any ticker) and its result lands in the holder: Run
// starts against an unexpanded base (no DNSTemplates member yet) and the
// very first snapshot change already carries the resolved member.
func TestInitialResolvePopulatesMembers(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 15*time.Millisecond)
	holder.Swap(base)

	stub := newLookupStub(addrsOf("192.0.2.1"))
	r := New(holder, stub.fn, discardLog(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	stub.waitCall(t)
	snap := waitForSnapshot(t, holder, base)

	routes := poolNamed(snap, "routes")
	if routes == nil {
		t.Fatalf("no pool %q in holder snapshot", "routes")
	}
	if !hasServer(routes.Servers, "node-192.0.2.1") {
		t.Errorf("routes.Servers = %+v, want member %q", routes.Servers, "node-192.0.2.1")
	}
}

// TestNoDeltaNeverSwaps proves a stable answer across ticks never
// triggers a holder.Swap: after the initial delta-driven swap, further
// resolutions returning the SAME set leave the snapshot pointer
// untouched (survivor pointer check against the holder's own
// snapshots).
func TestNoDeltaNeverSwaps(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 10*time.Millisecond)
	holder.Swap(base)

	stub := newLookupStub(addrsOf("192.0.2.1"))
	r := New(holder, stub.fn, discardLog(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	stub.waitCall(t)
	snap := waitForSnapshot(t, holder, base)

	// Three more ticks with the identical answer: no further swap, so
	// the snapshot pointer must survive unchanged.
	for i := 0; i < 3; i++ {
		stub.waitCall(t)
	}
	if cur := holder.Load(); cur != snap {
		t.Errorf("holder snapshot changed on a no-delta resolution: %p -> %p", snap, cur)
	}
}

// TestMembershipDeltaDrivesAddAndRemove drives {A} -> {A,B} -> {B} and
// checks each transition's holder snapshot: a new pointer each time
// (proving the swap happened) with exactly the expected member set.
func TestMembershipDeltaDrivesAddAndRemove(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 15*time.Millisecond)
	holder.Swap(base)

	stub := newLookupStub(addrsOf("192.0.2.1"))
	r := New(holder, stub.fn, discardLog(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	stub.waitCall(t)
	snapA := waitForSnapshot(t, holder, base)
	routesA := poolNamed(snapA, "routes")
	if len(routesA.Servers) != 1 || !hasServer(routesA.Servers, "node-192.0.2.1") {
		t.Fatalf("after {A}: routes.Servers = %+v, want exactly [node-192.0.2.1]", routesA.Servers)
	}

	stub.set(addrsOf("192.0.2.1", "192.0.2.2"), nil)
	stub.waitCall(t)
	snapAB := waitForSnapshot(t, holder, snapA)
	routesAB := poolNamed(snapAB, "routes")
	if len(routesAB.Servers) != 2 || !hasServer(routesAB.Servers, "node-192.0.2.1") || !hasServer(routesAB.Servers, "node-192.0.2.2") {
		t.Fatalf("after {A,B}: routes.Servers = %+v, want [node-192.0.2.1 node-192.0.2.2]", routesAB.Servers)
	}

	stub.set(addrsOf("192.0.2.2"), nil)
	stub.waitCall(t)
	snapB := waitForSnapshot(t, holder, snapAB)
	routesB := poolNamed(snapB, "routes")
	if len(routesB.Servers) != 1 || !hasServer(routesB.Servers, "node-192.0.2.2") {
		t.Fatalf("after {B}: routes.Servers = %+v, want exactly [node-192.0.2.2]", routesB.Servers)
	}
}

// TestLookupErrorKeepsLastKnownGood proves a lookup error never swaps:
// the holder snapshot survives unchanged across several consecutive
// erroring ticks.
func TestLookupErrorKeepsLastKnownGood(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 10*time.Millisecond)
	holder.Swap(base)

	stub := newLookupStub(addrsOf("192.0.2.1"))
	r := New(holder, stub.fn, discardLog(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	stub.waitCall(t)
	snap := waitForSnapshot(t, holder, base)

	stub.set(nil, errors.New("boom: lookup failed"))
	for i := 0; i < 3; i++ {
		stub.waitCall(t)
	}
	if cur := holder.Load(); cur != snap {
		t.Errorf("holder snapshot changed after a lookup error, want last-known-good kept")
	}
}

// TestEmptyAnswerKeepsLastKnownGood proves an empty (but error-free)
// answer is treated the same as an error: fail-static, no swap.
func TestEmptyAnswerKeepsLastKnownGood(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 10*time.Millisecond)
	holder.Swap(base)

	stub := newLookupStub(addrsOf("192.0.2.1"))
	r := New(holder, stub.fn, discardLog(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	stub.waitCall(t)
	snap := waitForSnapshot(t, holder, base)

	stub.set(nil, nil)
	for i := 0; i < 3; i++ {
		stub.waitCall(t)
	}
	if cur := holder.Load(); cur != snap {
		t.Errorf("holder snapshot changed after an empty answer, want last-known-good kept")
	}
}

// TestSinkholeAnswerFilteredFromMembers proves resolve drops unspecified
// (0.0.0.0/::) and multicast addresses before they can become pool
// members: dialing 0.0.0.0:<port> reaches the local host, which can be
// Bifrost's own listener (docs/operations.md's "DNS answers are a trust
// boundary").
func TestSinkholeAnswerFilteredFromMembers(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 15*time.Millisecond)
	holder.Swap(base)

	stub := newLookupStub(addrsOf("0.0.0.0", "10.0.0.1"))
	r := New(holder, stub.fn, discardLog(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	stub.waitCall(t)
	snap := waitForSnapshot(t, holder, base)
	routes := poolNamed(snap, "routes")
	if len(routes.Servers) != 1 || !hasServer(routes.Servers, "node-10.0.0.1") {
		t.Fatalf("routes.Servers = %+v, want exactly [node-10.0.0.1] (0.0.0.0 dropped)", routes.Servers)
	}
}

// TestAllSinkholeAnswerTreatedAsEmpty proves an answer containing ONLY
// unspecified/multicast addresses is treated exactly like an empty
// answer: fail-static, no swap, last-known-good membership kept.
func TestAllSinkholeAnswerTreatedAsEmpty(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 10*time.Millisecond)
	holder.Swap(base)

	stub := newLookupStub(addrsOf("0.0.0.0"))
	r := New(holder, stub.fn, discardLog(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	for i := 0; i < 3; i++ {
		stub.waitCall(t)
	}
	if cur := holder.Load(); cur != base {
		t.Errorf("holder snapshot changed for an all-sinkhole answer, want last-known-good kept (empty-path)")
	}
}

// TestDuplicateAnswerDeduped proves a resolver answer with duplicate
// A/AAAA records collapses to one member per unique address, instead of
// two identically-named pool members.
func TestDuplicateAnswerDeduped(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 15*time.Millisecond)
	holder.Swap(base)

	stub := newLookupStub(addrsOf("192.0.2.1", "192.0.2.1", "192.0.2.2"))
	r := New(holder, stub.fn, discardLog(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	stub.waitCall(t)
	snap := waitForSnapshot(t, holder, base)
	routes := poolNamed(snap, "routes")
	if len(routes.Servers) != 2 || !hasServer(routes.Servers, "node-192.0.2.1") || !hasServer(routes.Servers, "node-192.0.2.2") {
		t.Fatalf("routes.Servers = %+v, want exactly 2 members (duplicate 192.0.2.1 deduped)", routes.Servers)
	}
}

// TestDiscoveredMembersGaugeReportsPostTruncationCount proves the gauge
// reports the actual (capped) member count, not the raw answer size: a
// 300-address answer must gauge config.MaxTemplateMembers (256), the
// number ExpandDNS will actually keep, not 300.
func TestDiscoveredMembersGaugeReportsPostTruncationCount(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 50*time.Millisecond)
	holder.Swap(base)

	const total = 300
	addrs := make([]netip.Addr, total)
	for i := range addrs {
		addrs[i] = netip.AddrFrom4([4]byte{10, 0, byte(i / 256), byte(i % 256)})
	}

	stub := newLookupStub(addrs)
	fm := newFakeMetrics()
	r := New(holder, stub.fn, discardLog(), fm)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	stub.waitCall(t)
	if got := fm.waitMembers(t); got != config.MaxTemplateMembers {
		t.Errorf("DiscoveredMembers gauge = %d, want %d (post-truncation cap, not the raw 300-address answer)",
			got, config.MaxTemplateMembers)
	}
}

// TestHungLookupHitsPerLookupDeadline proves resolve bounds each lookup
// to one ResolveInterval: a lookup stub that blocks until its ctx is
// done must return promptly as result="error" on every tick, instead of
// stalling the loop past its interval forever.
func TestHungLookupHitsPerLookupDeadline(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 20*time.Millisecond)
	holder.Swap(base)

	calls := make(chan struct{}, 1024)
	blockingLookup := func(ctx context.Context, _ string) ([]netip.Addr, error) {
		calls <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	fm := newFakeMetrics()
	r := New(holder, blockingLookup, discardLog(), fm)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	for i := 0; i < 2; i++ {
		select {
		case <-calls:
		case <-time.After(2 * time.Second):
			t.Fatalf("lookup call %d never started: loop stalled past its per-lookup deadline", i+1)
		}
		if got := fm.waitResult(t); got != "error" {
			t.Errorf("call %d: Discovery result = %q, want %q", i+1, got, "error")
		}
	}
}

// TestRerenderAppliesEditKeepingMembership proves Rerender re-expands a
// newly supplied base against the LAST-KNOWN membership: the operator
// edit (a second static server here) shows up in the swapped snapshot
// alongside the already-discovered DNS member, with no re-resolution
// involved.
func TestRerenderAppliesEditKeepingMembership(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 50*time.Millisecond)
	holder.Swap(base)

	stub := newLookupStub(addrsOf("192.0.2.1"))
	r := New(holder, stub.fn, discardLog(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	stub.waitCall(t)
	snap := waitForSnapshot(t, holder, base)
	routes := poolNamed(snap, "routes")
	if !hasServer(routes.Servers, "node-192.0.2.1") {
		t.Fatalf("precondition failed: node-192.0.2.1 not yet discovered")
	}

	edited := testBase("routes", "node", "svc.local:587", 50*time.Millisecond)
	edited.Pools[0].Servers = []config.Server{{Name: "static-extra", Address: "10.9.8.7:587", Weight: 1}}

	r.Rerender(edited)

	after := holder.Load()
	if after == snap {
		t.Fatal("Rerender did not swap a new snapshot")
	}
	afterRoutes := poolNamed(after, "routes")
	if !hasServer(afterRoutes.Servers, "static-extra") {
		t.Errorf("routes.Servers = %+v, want the operator-added static-extra server", afterRoutes.Servers)
	}
	if !hasServer(afterRoutes.Servers, "node-192.0.2.1") {
		t.Errorf("routes.Servers = %+v, want the discovered member kept across Rerender", afterRoutes.Servers)
	}
}

// TestConcurrentRerenderAndDeltaSwapsRaceClean hammers Run's own
// delta-driven swaps against a concurrent stream of Rerender calls: the
// binding semantics is that both paths serialize through one mutex, so
// this must be race-clean under `go test -race` and leave no goroutine
// behind once ctx is cancelled.
func TestConcurrentRerenderAndDeltaSwapsRaceClean(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 5*time.Millisecond)
	holder.Swap(base)

	var calls atomic.Int64
	lookup := func(context.Context, string) ([]netip.Addr, error) {
		if calls.Add(1)%2 == 0 {
			return addrsOf("192.0.2.1", "192.0.2.2"), nil
		}
		return addrsOf("192.0.2.1"), nil
	}

	r := New(holder, lookup, discardLog(), nil)
	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan struct{})
	go func() { r.Run(ctx); close(runDone) }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		edited := testBase("routes", "node", "svc.local:587", 5*time.Millisecond)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				r.Rerender(edited)
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	cancel()
	wg.Wait()

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if holder.Load() == nil {
		t.Fatal("holder has no snapshot after concurrent Rerender+delta swaps")
	}
	if err := goleak.Find(); err != nil {
		t.Errorf("goroutine leak after Run and the Rerender spammer both returned: %v", err)
	}
}

// TestOrphanedTemplateGoesInert proves resolve's orphan guard sits above
// the lookup itself: once a Rerender drops a template from the base
// (Run never tears down its goroutine — see Run's doc comment), the
// template's loop must go fully inert — no further lookup call, no
// Discovery metric, and no swap — even though the loop keeps ticking.
func TestOrphanedTemplateGoesInert(t *testing.T) {
	holder := &config.Holder{}
	base := testBase("routes", "node", "svc.local:587", 10*time.Millisecond)
	holder.Swap(base)

	stub := newLookupStub(addrsOf("192.0.2.1"))
	fm := newFakeMetrics()
	r := New(holder, stub.fn, discardLog(), fm)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	stub.waitCall(t)
	_ = waitForSnapshot(t, holder, base)
	if got := fm.waitResult(t); got != "ok" {
		t.Fatalf("precondition: Discovery result = %q, want %q", got, "ok")
	}

	// Drop the "node" template from the base entirely (operator removed
	// the server_dns block) and Rerender it in.
	edited := &config.Config{Pools: []config.Pool{{Name: "routes"}}}
	r.Rerender(edited)
	snapOrphaned := holder.Load()
	if snapOrphaned == nil {
		t.Fatal("Rerender did not install a snapshot")
	}

	// At most one resolve can already be in flight at the exact instant
	// Rerender lands (it read r.base as still current a moment before);
	// drain that single possible call/metric before asserting silence.
	select {
	case <-stub.calls:
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case <-fm.results:
	default:
	}

	// From here on the orphan guard sits above the lookup: an orphaned
	// template's loop must call lookup no more and report no further
	// Discovery result, across several more tick intervals.
	for i := 0; i < 5; i++ {
		select {
		case <-stub.calls:
			t.Error("lookup called again for an orphaned template, want it inert before the lookup")
		case <-fm.results:
			t.Error("Discovery reported for an orphaned template, want none")
		case <-time.After(20 * time.Millisecond):
		}
	}

	if cur := holder.Load(); cur != snapOrphaned {
		t.Errorf("holder snapshot changed for an orphaned template, want it inert")
	}
}

// TestJitterWithinBounds proves jitter's ±10% spread never exceeds its
// bounds across many draws. No injection seam needed — jitter calls
// top-level math/rand directly, which is safe to call from a test loop
// exactly like production code does.
func TestJitterWithinBounds(t *testing.T) {
	const base = 100 * time.Millisecond
	const tenth = base / 10 // integer math: exact ±10% bound, no float rounding to fight
	lo := base - tenth
	hi := base + tenth

	for i := 0; i < 1000; i++ {
		got := jitter(base)
		if got < lo || got > hi {
			t.Fatalf("draw %d: jitter(%v) = %v, want within [%v, %v]", i, base, got, lo, hi)
		}
	}
}

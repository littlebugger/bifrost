//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/admin"
	"github.com/littlebugger/bifrost/internal/balance"
	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/discover"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/health"
	"github.com/littlebugger/bifrost/internal/metrics"
	"github.com/littlebugger/bifrost/internal/proxy"
	"github.com/littlebugger/bifrost/internal/smtpdrv"
)

// Task 6: the end-to-end proof for server_dns pool members (design doc
// docs/superpowers/specs/2026-11-09-dns-discovery-design.md). Tasks 1-5
// land the pieces (decode, ExpandDNS, discover.Runner, serve/reload
// wiring, metrics+admin fields) as unit tests; this file is the only
// place a discover.Runner, a real holder, a real health.Checker/admin
// plane, and a real relay all run together against real fakesmtp
// backends.
//
// The port problem: every member a template expands shares the
// template's own port (expandTemplate keeps the template's port and
// substitutes only the host — internal/config/dns.go), so the two "IPs"
// standing in for a server_dns answer have to be two fakesmtp backends on
// two different loopback addresses at the SAME port. 127.0.0.2 does not
// auto-alias on macOS (`ifconfig lo0` on this machine lists only
// 127.0.0.1; binding 127.0.0.2 fails with "can't assign requested
// address"), so the second address is the IPv6 loopback, ::1 — a
// distinct netip.Addr from 127.0.0.1, and a TCP listener on
// 127.0.0.1:P coexists just fine with one on [::1]:P (verified: they are
// different address families, not competing for the same socket).

// memberA and memberB are the two discovered members these scenarios
// drive: expandTemplate names a member "<template>-<ip>", and
// netip.Addr.Compare sorts an IPv4 address before any IPv6 one, so A is
// always the deterministic first candidate in balance's round-robin.
const (
	memberA = "node-127.0.0.1"
	memberB = "node-::1"
)

// dnsResolveInterval is every scenario's server_dns resolve_interval: far
// below production's 1s-minimum validation floor (bypassed here, exactly
// like internal/discover's own tests, since these configs are hand-built
// config.Config values, never run through config.Load), so a scenario's
// membership deltas land in well under a second.
const dnsResolveInterval = 20 * time.Millisecond

// dnsAnswer is the fake discover.LookupFunc these scenarios drive: a
// mutex-guarded answer flipped between {A,B}, {A}, and empty to stand in
// for a resolver whose answer changes over time, per the design doc's own
// testing section ("fake resolver flipping between {A}, {A,B}, {B}").
type dnsAnswer struct {
	mu    sync.Mutex
	addrs []netip.Addr
	err   error
}

func (d *dnsAnswer) set(addrs []netip.Addr, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrs, d.err = addrs, err
}

func (d *dnsAnswer) lookup(context.Context, string) ([]netip.Addr, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.addrs, d.err
}

// reserveSharedPort picks one free TCP port by binding 127.0.0.1:0 and
// releasing it immediately: the two fakesmtp backends need to agree on a
// port before either of them starts (see the port problem above), unlike
// fakesmtp.Start's own always-ephemeral bind.
func reserveSharedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split reserved addr: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return port
}

// startDNSBackends starts the two fakesmtp servers standing in for a
// server_dns template's discovered members, both on port and stopped via
// t.Cleanup (fakesmtp.StartAddr, unlike Start, has no testing.TB of its
// own to register that for us).
func startDNSBackends(t *testing.T, port string, scriptA, scriptB fakesmtp.Script) (a, b *fakesmtp.Server) {
	t.Helper()
	a, err := fakesmtp.StartAddr("127.0.0.1:"+port, scriptA)
	if err != nil {
		t.Fatalf("start A backend: %v", err)
	}
	t.Cleanup(a.Stop)
	b, err = fakesmtp.StartAddr("[::1]:"+port, scriptB)
	if err != nil {
		t.Fatalf("start B backend: %v", err)
	}
	t.Cleanup(b.Stop)
	return a, b
}

// dnsConfig is a listener plus one pool ("routes") holding a single
// server_dns template ("node") at port; the lookup injected into
// discover.Runner supplies the member IPs, so the template's own host
// ("mta.discovery.test") is never actually resolved.
func dnsConfig(port string, reuseEnvelopes int) *config.Config {
	return &config.Config{
		Defaults: config.Defaults{Timeouts: m1Timeouts()},
		Listener: config.Listener{
			Hostname:     "bifrost.test",
			Capabilities: []string{"PIPELINING", "8BITMIME", "SIZE 104857600"},
		},
		Pools: []config.Pool{{
			Name: "routes", Balance: "roundrobin", BackendTLS: "none", EhloName: "bifrost.test",
			ReuseEnvelopes: reuseEnvelopes,
			DNSTemplates: []config.ServerDNS{{
				Name: "node", Address: "mta.discovery.test:" + port,
				ResolveInterval: dnsResolveInterval, Weight: 1,
			}},
		}},
		Routing: config.Routing{DefaultPool: "routes"},
	}
}

// dnsCheck is a fast, deterministic L0 health check (admin_drain_test.
// go's own drainCheck): plain TCP connect, no SMTP bytes, so it passes
// against any accepting fakesmtp backend regardless of capability set.
func dnsCheck() config.CheckParams {
	return config.CheckParams{
		Level: "connect", Rise: 1, Fall: 1,
		Interval: 10 * time.Millisecond, DownInterval: 10 * time.Millisecond, Timeout: time.Second,
	}
}

// dnsMemberNames returns the names of pool's current Servers in holder's
// loaded config, or nil if the pool or the holder's config don't exist
// yet.
func dnsMemberNames(h *config.Holder, pool string) []string {
	cfg := h.Load()
	if cfg == nil {
		return nil
	}
	for _, p := range cfg.Pools {
		if p.Name != pool {
			continue
		}
		names := make([]string, len(p.Servers))
		for i, s := range p.Servers {
			names[i] = s.Name
		}
		return names
	}
	return nil
}

// sameNameSet reports whether got and want hold the same names,
// order-insensitive.
func sameNameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g, w := slices.Clone(got), slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	return slices.Equal(g, w)
}

// waitDNSMembers bounded-polls until pool's discovered membership in h
// equals exactly want.
func waitDNSMembers(t *testing.T, h *config.Holder, pool string, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := dnsMemberNames(h, pool)
		if sameNameSet(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool %q members = %v, want %v", pool, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// dnsServerRow is the slice of admin's GET /servers row shape these
// scenarios check: which server, and its discovered/template marker
// (internal/admin/servers.go's serverView).
type dnsServerRow struct {
	Server     string `json:"server"`
	Discovered bool   `json:"discovered"`
	Template   string `json:"template"`
}

type dnsServersResponse struct {
	Servers []dnsServerRow `json:"servers"`
}

// getDNSServers fetches GET /servers from adminH in process (no real
// socket — admin_drain_test.go's own postAdmin style) and decodes it.
func getDNSServers(t *testing.T, adminH http.Handler) dnsServersResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/servers", nil)
	rr := httptest.NewRecorder()
	adminH.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /servers: status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out dnsServersResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /servers: decode %v: body = %s", err, rr.Body.String())
	}
	return out
}

// findDNSServerRow returns the row named name, or ok=false.
func findDNSServerRow(rows []dnsServerRow, name string) (row dnsServerRow, ok bool) {
	for _, r := range rows {
		if r.Server == name {
			return r, true
		}
	}
	return dnsServerRow{}, false
}

// TestDNSDiscoveryMembershipAcrossBothMembers is scenario 1: a
// server_dns template resolving to {A,B} expands into two real pool
// members that both actually take traffic, and whose health is the
// normal per-member kind — a real health.Checker, synced onto the same
// holder discover.Runner swaps, probing each one independently and
// surfacing it through the real admin /servers endpoint with the
// discovered/template markers Task 5 added.
func TestDNSDiscoveryMembershipAcrossBothMembers(t *testing.T) {
	port := reserveSharedPort(t)
	a, b := startDNSBackends(t, port,
		fakesmtp.Script{Caps: backendCaps()},
		fakesmtp.Script{Caps: backendCaps()},
	)

	lk := &dnsAnswer{}
	lk.set([]netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}, nil)

	cfg := dnsConfig(port, 0)
	cfg.Pools[0].DNSTemplates[0].Check = dnsCheck()
	holder := &config.Holder{}
	holder.Swap(cfg)
	quiet := slog.New(slog.DiscardHandler)
	m := metrics.New()

	runner := discover.New(holder, lk.lookup, quiet, m)
	runnerCtx, runnerCancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() { runner.Run(runnerCtx); close(runnerDone) }()
	t.Cleanup(func() { runnerCancel(); <-runnerDone })

	waitDNSMembers(t, holder, "routes", memberA, memberB)

	// Built only now, after the first expansion has already landed: the
	// Checker's own constructor syncs against whatever the holder
	// currently has (internal/health/scheduler.go's newChecker), so both
	// discovered members are registered from the start rather than
	// racing the checker's own 1s reload-poll for their first sync.
	checker := health.New(holder, nil, quiet)
	router := balance.NewRouter(holder, checker.Eligible, rand.New(rand.NewSource(1)))
	adminH := admin.New(holder, checker, router, m, nil, nil).Handler()

	checkerCtx, checkerCancel := context.WithCancel(context.Background())
	checkerDone := make(chan struct{})
	go func() { checker.Run(checkerCtx); close(checkerDone) }()
	t.Cleanup(func() { checkerCancel(); <-checkerDone })

	relay := proxy.NewRelay(router.Pick, holder, quiet, checker, router.Lease, m)
	addr := serve(t, cfg, relay)

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	const messages = 10
	for i := 0; i < messages; i++ {
		wantQueued(t, i, c.SendMsg(i))
	}
	c.Send("QUIT")
	c.Expect("221")

	if got := a.DialCount(); got == 0 {
		t.Errorf("A (%s) DialCount = 0, want >0: traffic must spread across both discovered members", memberA)
	}
	if got := b.DialCount(); got == 0 {
		t.Errorf("B (%s) DialCount = 0, want >0: traffic must spread across both discovered members", memberB)
	}

	// Per-member health: each discovered member gets its OWN probe
	// history, not one shared verdict for the whole template.
	waitProbeCountAbove(t, checker, "routes", memberA, 0)
	waitProbeCountAbove(t, checker, "routes", memberB, 0)

	resp := getDNSServers(t, adminH)
	for _, name := range []string{memberA, memberB} {
		row, ok := findDNSServerRow(resp.Servers, name)
		if !ok {
			t.Fatalf("GET /servers has no row for %q; got %+v", name, resp.Servers)
		}
		if !row.Discovered {
			t.Errorf("row %q discovered = false, want true", name)
		}
		if row.Template != "node" {
			t.Errorf("row %q template = %q, want %q", name, row.Template, "node")
		}
	}
}

// TestDNSDiscoveryRemoveMemberFailsOverAndRedialsAffinity is scenario 2:
// B drops out of the next DNS answer while a transaction is already
// relaying to it. The in-flight transaction finishes on its own lease
// regardless (the relay/lease picked its candidate once, at attach time,
// from the snapshot that still had B); every transaction after the
// removal lands on A, the only member left; and with reuse_envelopes=3,
// the session's cached affinity leg — pointing at B's now-stale *Server
// — misses tryReuse's pointer check (attach.go: "candidates[0] !=
// a.srv") and silently re-dials instead of ever touching B again, per
// the design doc's "after ANY delta the affinity conn re-dials silently"
// rule.
func TestDNSDiscoveryRemoveMemberFailsOverAndRedialsAffinity(t *testing.T) {
	port := reserveSharedPort(t)
	const eodDelay = 200 * time.Millisecond
	a, b := startDNSBackends(t, port,
		fakesmtp.Script{Caps: backendCaps()},
		fakesmtp.Script{
			Caps: backendCaps(),
			// Stalls B's final reply so msg1 is still relaying — dialed,
			// mid-transaction — at the moment the test removes B from the
			// next DNS answer.
			OnEOD: []fakesmtp.Step{{Delay: eodDelay}},
		},
	)

	lk := &dnsAnswer{}
	lk.set([]netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}, nil)

	cfg := dnsConfig(port, 3) // reuse_envelopes = 3
	holder := &config.Holder{}
	holder.Swap(cfg)
	quiet := slog.New(slog.DiscardHandler)
	m := metrics.New()

	runner := discover.New(holder, lk.lookup, quiet, m)
	runnerCtx, runnerCancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() { runner.Run(runnerCtx); close(runnerDone) }()
	t.Cleanup(func() { runnerCancel(); <-runnerDone })

	waitDNSMembers(t, holder, "routes", memberA, memberB)

	router := balance.NewRouter(holder, alwaysEligible, rand.New(rand.NewSource(1)))
	relay := proxy.NewRelay(router.Pick, holder, quiet, nil, router.Lease, m)
	addr := serve(t, cfg, relay)

	c := smtpdrv.Dial(t, addr)
	c.SetFail(func(format string, args ...any) { t.Errorf(format, args...) }) // msg1 runs on another goroutine below
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	wantQueued(t, 0, c.SendMsg(0)) // round-robin's deterministic lead pick: A

	msg1 := make(chan smtpdrv.Reply, 1)
	go func() { msg1 <- c.SendMsg(1) }() // round-robin's second pick: B, stalled mid-transaction

	waitDialsAtLeast(t, b, 1) // B is genuinely dialed and relaying before we yank it

	lk.set([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil) // next resolution drops B
	waitDNSMembers(t, holder, "routes", memberA)

	wantQueued(t, 1, <-msg1) // in-flight msg1 still finishes cleanly on B

	// msg2: reuse_envelopes=3 cached msg1's leg to B as the session's
	// affinity, but B is no longer a candidate at all -- tryReuse's
	// candidates[0]!=a.srv check misses and this envelope silently
	// re-dials, landing on A (the only member left).
	wantQueued(t, 2, c.SendMsg(2))

	// msg3: now the affinity is msg2's own fresh leg to A, and nothing
	// else changed -- normal reuse resumes.
	wantQueued(t, 3, c.SendMsg(3))

	c.Send("QUIT")
	c.Expect("221")

	if got := b.DialCount(); got != 1 {
		t.Errorf("B DialCount = %d, want 1: msg1's leg is the only one ever opened to it", got)
	}
	if got := b.CmdCount("RSET"); got != 0 {
		t.Errorf("B RSET count = %d, want 0: msg2 never attempts to revalidate the stale cached leg", got)
	}
	if got := a.DialCount(); got != 2 {
		t.Errorf("A DialCount = %d, want 2: msg0's fresh dial, plus msg2's silent re-dial after B's affinity missed", got)
	}
	if got := a.CmdCount("RSET"); got != 1 {
		t.Errorf("A RSET count = %d, want 1: msg3 reuses msg2's fresh leg via RSET", got)
	}
}

// TestDNSDiscoveryEmptyAnswerKeepsMembership is scenario 3: an empty DNS
// answer is fail-static, not a removal — the design doc's "a resolution
// error or an EMPTY answer keeps the last-known-good membership". Proven
// at the strongest level available: the holder's own config pointer
// identity must not move at all (resolve's own "no delta, no swap" —
// internal/discover/discover.go), not just that the member names happen
// to read the same:  across several resolve ticks that each see an empty
// answer, membership stays {A,B} and mail keeps flowing through both.
func TestDNSDiscoveryEmptyAnswerKeepsMembership(t *testing.T) {
	port := reserveSharedPort(t)
	a, b := startDNSBackends(t, port,
		fakesmtp.Script{Caps: backendCaps()},
		fakesmtp.Script{Caps: backendCaps()},
	)

	lk := &dnsAnswer{}
	lk.set([]netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}, nil)

	cfg := dnsConfig(port, 0)
	holder := &config.Holder{}
	holder.Swap(cfg)
	quiet := slog.New(slog.DiscardHandler)
	m := metrics.New()

	runner := discover.New(holder, lk.lookup, quiet, m)
	runnerCtx, runnerCancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() { runner.Run(runnerCtx); close(runnerDone) }()
	t.Cleanup(func() { runnerCancel(); <-runnerDone })

	waitDNSMembers(t, holder, "routes", memberA, memberB)
	settled := holder.Load()

	lk.set(nil, nil) // fail-static: an empty answer, not an error

	// Several resolve ticks at this (short, jittered) interval, every one
	// seeing the empty answer: the holder's identity must never move.
	time.Sleep(10 * dnsResolveInterval)
	if got := holder.Load(); got != settled {
		t.Errorf("holder config identity changed on an empty DNS answer, want unchanged (fail-static: no delta, no swap)")
	}
	waitDNSMembers(t, holder, "routes", memberA, memberB) // still exactly {A,B}

	router := balance.NewRouter(holder, alwaysEligible, rand.New(rand.NewSource(1)))
	relay := proxy.NewRelay(router.Pick, holder, quiet, nil, router.Lease, m)
	addr := serve(t, cfg, relay)

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	const messages = 4
	for i := 0; i < messages; i++ {
		wantQueued(t, i, c.SendMsg(i))
	}
	c.Send("QUIT")
	c.Expect("221")

	if got := a.DialCount() + b.DialCount(); got != messages {
		t.Errorf("total DialCount = %d, want %d: mail keeps flowing through an empty-answer tick", got, messages)
	}
}

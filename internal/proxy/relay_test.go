//go:build integration

package proxy

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// TestMain wraps every test in this package — the pre-attach session
// tests included — in goleak: the relay starts goroutines (the reply pump
// during DATA) and holds backend connections, and none of that may
// outlive a transaction.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// Every reply asserted here is a literal, never one of replies.go's
// constants: /PROJECT.md's Transparency Contract is the spec, and
// comparing a constant against itself would prove nothing. Backend
// replies are asserted the same way — byte for byte, as the fake wrote
// them.

// relayTimeouts is the backend-side timeout budget for these tests:
// PROJECT.md's shape, seconds instead of minutes so a wedged backend
// fails a test instead of hanging it.
func relayTimeouts() config.Timeouts {
	return config.Timeouts{
		ClientIdle:       10 * time.Second,
		SessionMax:       time.Minute,
		BackendConnect:   2 * time.Second,
		BackendHandshake: 2 * time.Second,
		BackendMailReply: 2 * time.Second,
		Backend354Wait:   2 * time.Second,
		DataProgress:     5 * time.Second,
		BackendFinalDot:  5 * time.Second,
	}
}

// relayConfig is testConfig plus one pool holding a server per address,
// in candidate order.
func relayConfig(addrs ...string) *config.Config {
	cfg := testConfig()
	cfg.Defaults.Timeouts = relayTimeouts()
	pool := config.Pool{Name: "p", Balance: "roundrobin", BackendTLS: "none", EhloName: "bifrost.test"}
	for i, addr := range addrs {
		pool.Servers = append(pool.Servers, config.Server{
			Name: fmt.Sprintf("s%d", i), Address: addr, Weight: 1,
		})
	}
	cfg.Pools = []config.Pool{pool}
	cfg.Routing = config.Routing{DefaultPool: "p"}
	return cfg
}

// backendCaps is the capability set a fake must advertise to satisfy the
// superset check against testConfig's advertised set (SIZE bigger, the
// rest present) — without it every Dial fails as incompatible.
func backendCaps() []string { return []string{"PIPELINING", "8BITMIME", "SIZE 20971520"} }

// relayFake starts a fake backend that is compatible by default.
func relayFake(t *testing.T, s fakesmtp.Script) *fakesmtp.Server {
	t.Helper()
	if s.Caps == nil {
		s.Caps = backendCaps()
	}
	return fakesmtp.Start(t, s)
}

// stubSignals records the passive health signals the relay emits; epic 06
// implements the real thing.
type stubSignals struct {
	mu       sync.Mutex
	dial     []string
	trans    []string
	succeeds []string
}

func (s *stubSignals) DialFailure(srv *config.Server)    { s.record(&s.dial, srv) }
func (s *stubSignals) TransportError(srv *config.Server) { s.record(&s.trans, srv) }
func (s *stubSignals) Success(srv *config.Server)        { s.record(&s.succeeds, srv) }

func (s *stubSignals) record(dst *[]string, srv *config.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*dst = append(*dst, srv.Name)
}

// counts returns the recorded signals: dial failures, transport errors,
// successes.
func (s *stubSignals) counts() (dial, trans, succeeds []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.dial...), append([]string(nil), s.trans...), append([]string(nil), s.succeeds...)
}

// leaseCounter stands in for epic 07's in-flight accounting.
type leaseCounter struct {
	mu    sync.Mutex
	open  int
	total int
}

func (l *leaseCounter) lease(*config.Server) func() {
	l.mu.Lock()
	l.open++
	l.total++
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		l.open--
		l.mu.Unlock()
	}
}

func (l *leaseCounter) state() (open, total int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open, l.total
}

// leaseGate is the reuse tests' lease-denial stand-in: it denies every
// lease while held is true (a simulated concurrent holder pinning the
// pool's one max_transactions slot) and grants otherwise. Unlike
// leaseCounter there is no capacity model, just the one on/off switch
// TestReuseLeaseDenialRetainsCache needs.
type leaseGate struct {
	mu   sync.Mutex
	held bool
}

func (g *leaseGate) setHeld(held bool) {
	g.mu.Lock()
	g.held = held
	g.mu.Unlock()
}

func (g *leaseGate) lease(*config.Server) func() {
	g.mu.Lock()
	held := g.held
	g.mu.Unlock()
	if held {
		return nil
	}
	return func() {}
}

// reuseMetrics records BackendReuse outcomes only — every other Metrics
// method is the shared no-op every other fixture already gets from
// noMetrics. The reuse tests need "reused"/"capped" counts; nothing else
// in the interface matters to them.
type reuseMetrics struct {
	noMetrics
	mu     sync.Mutex
	events []string // "server:outcome", in call order
}

func (m *reuseMetrics) BackendReuse(server, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, server+":"+outcome)
}

// count returns how many BackendReuse calls recorded outcome, any server.
func (m *reuseMetrics) count(outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.events {
		if strings.HasSuffix(e, ":"+outcome) {
			n++
		}
	}
	return n
}

// relayFixture is a client talking to a real Session whose TxnHandler is
// a real Relay: the whole client leg plus the splice, with fakesmtp
// backends at the far end.
type relayFixture struct {
	*testClient
	sig    *stubSignals
	leases *leaseCounter
	picks  *pickStub
}

// pickStub is the epic-07 stand-in: it hands back the configured servers
// in order, and counts calls.
type pickStub struct {
	mu      sync.Mutex
	servers []*config.Server
	calls   int
	rr      bool // round-robin the head of the list, one step per call
}

func (p *pickStub) pick(TxnMeta) ([]*config.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if !p.rr || len(p.servers) == 0 {
		return p.servers, nil
	}
	out := make([]*config.Server, 0, len(p.servers))
	for i := range p.servers {
		out = append(out, p.servers[(p.calls-1+i)%len(p.servers)])
	}
	return out, nil
}

// serversOf returns pointers to every server in cfg's first pool, in
// config order — what a real PickFunc hands the relay.
func serversOf(cfg *config.Config) []*config.Server {
	var out []*config.Server
	for i := range cfg.Pools {
		for j := range cfg.Pools[i].Servers {
			out = append(out, &cfg.Pools[i].Servers[j])
		}
	}
	return out
}

// newRelayClient wires session+relay over net.Pipe and returns a client
// that has already read the banner and greeted.
func newRelayClient(t *testing.T, cfg *config.Config) *relayFixture {
	t.Helper()
	return newRelayClientLog(t, cfg, slog.New(slog.DiscardHandler))
}

func newRelayClientLog(t *testing.T, cfg *config.Config, lg *slog.Logger) *relayFixture {
	t.Helper()
	return newRelayClientPick(t, cfg, lg, serversOf(cfg))
}

// newRelayClientPick is newRelayClient with an explicit candidate list,
// for the cases where what the router hands over is not what the loaded
// config holds (a reload between the pick and the dial).
func newRelayClientPick(t *testing.T, cfg *config.Config, lg *slog.Logger, servers []*config.Server) *relayFixture {
	t.Helper()
	return newRelayClientPickMetrics(t, cfg, lg, servers, nil)
}

// newRelayClientPickMetrics is newRelayClientPick with an explicit
// Metrics override: the reuse tests need to observe BackendReuse calls,
// which no other fixture constructor exposes a way to do. nil keeps
// today's default (NewRelay's own variadic optional-argument handling
// falls back to noMetrics).
func newRelayClientPickMetrics(t *testing.T, cfg *config.Config, lg *slog.Logger, servers []*config.Server, metrics Metrics) *relayFixture {
	t.Helper()
	h := &config.Holder{}
	h.Swap(cfg)
	sig := &stubSignals{}
	leases := &leaseCounter{}
	picks := &pickStub{servers: servers}

	var r *Relay
	if metrics != nil {
		r = NewRelay(picks.pick, h, lg, sig, leases.lease, metrics)
	} else {
		r = NewRelay(picks.pick, h, lg, sig, leases.lease)
	}
	c := newTestClient(t, cfg, nil, r)
	f := &relayFixture{testClient: c, sig: sig, leases: leases, picks: picks}
	f.expect("220 bifrost.test ESMTP")
	f.send("EHLO client.example")
	f.reply()
	return f
}

// backendLines returns every raw line the fake's sessionIdx-th session
// received after the handshake, terminators included. The leading
// "EHLO <name>" is the backend leg's own handshake (epic 04's business,
// never the client's bytes) and is dropped.
func backendLines(t *testing.T, srv *fakesmtp.Server, sessionIdx int) []string {
	t.Helper()
	sessions := srv.Sessions()
	if sessionIdx >= len(sessions) {
		t.Fatalf("backend session %d: only %d sessions accepted", sessionIdx, len(sessions))
	}
	var out []string
	for _, ev := range sessions[sessionIdx].Transcript() {
		if ev.Verb == "EHLO" && len(out) == 0 {
			continue
		}
		out = append(out, string(ev.Line))
	}
	return out
}

// wantLines asserts the fake received exactly these raw lines.
func wantLines(t *testing.T, srv *fakesmtp.Server, sessionIdx int, want ...string) {
	t.Helper()
	got := backendLines(t, srv, sessionIdx)
	if len(got) != len(want) {
		t.Fatalf("backend lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backend line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMailVerdictVerbatim(t *testing.T) {
	// The contract's D3 payoff: MAIL is relayed, so a sender-level reject
	// reaches the client at MAIL — not a synthesized 250 followed by a
	// surprise at RCPT (Mireka's lazy-attach bug).
	srv := relayFake(t, fakesmtp.Script{OnMAIL: []fakesmtp.Step{{Reply: "552 5.3.4 message too big for system"}}})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example> SIZE=99999999")
	f.expect("552 5.3.4 message too big for system")

	wantLines(t, srv, 0, "MAIL FROM:<a@b.example> SIZE=99999999\r\n")
}

// TestMailFromMalformedAddressVerbatim and TestMailFromNullSenderVerbatim
// mirror TestMailVerdictVerbatim for the two MAIL FROM shapes
// mailFromDomain parses to "" (routing falls through to default_pool):
// deriving that routing key must not touch the wire in either
// direction, so the raw line still reaches the backend byte-exact and
// its verdict still relays back verbatim — R4 for the failure path, not
// just the well-formed one.
func TestMailFromMalformedAddressVerbatim(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{OnMAIL: []fakesmtp.Step{{Reply: "550 5.1.7 bad sender address syntax"}}})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<not-an-address-at-all> SIZE=100")
	f.expect("550 5.1.7 bad sender address syntax")

	wantLines(t, srv, 0, "MAIL FROM:<not-an-address-at-all> SIZE=100\r\n")
}

func TestMailFromNullSenderVerbatim(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{OnMAIL: []fakesmtp.Step{{Reply: "250 2.1.0 sender ok"}}})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<>")
	f.expect("250 2.1.0 sender ok")

	wantLines(t, srv, 0, "MAIL FROM:<>\r\n")
}

func TestEnvelopeRelayHappy(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example> BODY=8BITMIME")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<one@c.example> NOTIFY=SUCCESS")
	f.expect("250 2.1.5 OK")
	f.send("RCPT TO:<two@c.example>")
	f.expect("250 2.1.5 OK")

	// R4 in the other direction: the client's bytes, params included,
	// reach the backend untouched.
	wantLines(t, srv, 0,
		"MAIL FROM:<a@b.example> BODY=8BITMIME\r\n",
		"RCPT TO:<one@c.example> NOTIFY=SUCCESS\r\n",
		"RCPT TO:<two@c.example>\r\n",
	)
	if open, total := f.leases.state(); total != 1 {
		t.Errorf("leases = (open %d, total %d), want total 1", open, total)
	}
}

func TestReplyVerbatimMultiline(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{
		OnMAIL: []fakesmtp.Step{{Reply: "250-2.1.0 sender ok\r\n250-2.1.0 and another thing\r\n250 2.1.0 done"}},
	})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250-2.1.0 sender ok", "250-2.1.0 and another thing", "250 2.1.0 done")
}

func TestQueueReplayPipelined(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{
		OnRCPT: []fakesmtp.Step{{Reply: "250 2.1.5 first ok"}, {Reply: "550 5.1.1 second unknown"}},
	})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	// One write: the batch is queued by the session while the relay is
	// still dialing, then replayed to the backend in order.
	f.raw("MAIL FROM:<a@b.example>\r\nRCPT TO:<one@c.example>\r\nRCPT TO:<two@c.example>\r\n")
	f.expect("250 2.1.0 OK")
	f.expect("250 2.1.5 first ok")
	f.expect("550 5.1.1 second unknown")

	wantLines(t, srv, 0,
		"MAIL FROM:<a@b.example>\r\n",
		"RCPT TO:<one@c.example>\r\n",
		"RCPT TO:<two@c.example>\r\n",
	)
}

func TestBackendBannerNotLeaked(t *testing.T) {
	// The backend's own greeting and EHLO reply are consumed by the
	// backend leg's handshake; the client only ever sees Bifrost's.
	srv := relayFake(t, fakesmtp.Script{})
	h := &config.Holder{}
	cfg := relayConfig(srv.Addr())
	h.Swap(cfg)
	picks := &pickStub{servers: serversOf(cfg)}
	r := NewRelay(picks.pick, h, slog.New(slog.DiscardHandler), &stubSignals{}, nil)
	c := newTestClient(t, cfg, nil, r)

	var seen []string
	seen = append(seen, c.reply()...) // banner
	c.send("EHLO client.example")
	seen = append(seen, c.reply()...)
	c.send("MAIL FROM:<a@b.example>")
	seen = append(seen, c.reply()...)

	if got, want := seen[0], "220 bifrost.test ESMTP"; got != want {
		t.Errorf("banner = %q, want %q", got, want)
	}
	for i, line := range seen {
		if strings.Contains(line, "fakesmtp") {
			t.Errorf("client line %d leaked a backend line: %q", i, line)
		}
		if i > 0 && strings.HasPrefix(line, "220") {
			t.Errorf("client line %d is a second greeting: %q", i, line)
		}
	}
}

func TestRelayBackendAuth(t *testing.T) {
	// The relay sends AUTH PLAIN with pool credentials after handshake,
	// before relaying MAIL. Over a TLS-upgraded leg: backend.Dial refuses
	// to originate AUTH over a connection that was not TLS-upgraded (see
	// TestDialAuthRefusesCleartext), and config.Validate requires the
	// same of a real pool.auth block.
	srv := relayFake(t, fakesmtp.Script{
		TLS:  fakesmtp.TestCert(t),
		Caps: append(backendCaps(), "AUTH PLAIN"),
	})

	cfg := relayConfig(srv.Addr())
	cfg.Pools[0].BackendTLS = "starttls"
	cfg.Pools[0].Auth = &config.PoolAuth{
		Username: "u",
		Password: "p",
	}

	f := newRelayClient(t, cfg)
	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<one@c.example>")
	f.expect("250 2.1.5 OK")

	// The backend's transcript must contain AUTH PLAIN with base64-encoded
	// credentials before the relayed MAIL line, after the STARTTLS
	// upgrade and its re-EHLO. The exact encoding of "\x00u\x00p" is
	// "AHUAcA==".
	wantLines(t, srv, 0,
		"STARTTLS\r\n",
		"EHLO bifrost.test\r\n",
		"AUTH PLAIN AHUAcA==\r\n",
		"MAIL FROM:<a@b.example>\r\n",
		"RCPT TO:<one@c.example>\r\n",
	)
}

// sendOneEnvelope drives one full MAIL..DATA envelope to completion over
// f, asserting the client sees today's happy-path replies. Shared by the
// reuse tests below, which only differ in what they check afterward.
//
// The trailing NOOP round-trip is TestDetachAfterVerdict's own technique:
// the verdict reaches the client from the reply-pump goroutine before
// detachOrStash/detach necessarily runs on the txn goroutine (R4's
// ordering), so a caller that checked backend-side state (a QUIT sent, a
// dial count) right after the verdict would be racing it. NOOP is
// answered from the same goroutine, sequentially after detach — one
// round trip with the session proves detach has already run.
func sendOneEnvelope(f *relayFixture, from string) {
	f.send("MAIL FROM:<" + from + "@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")
	f.send("DATA")
	f.expect("354 Start mail input; end with <CRLF>.<CRLF>")
	f.raw("body " + from + "\r\n.\r\n")
	f.expect("250 2.0.0 OK: queued")
	f.send("NOOP")
	f.expect("250 2.0.0 OK")
}

// TestReuseStashKeepsConnOpen is task-3 scenario 1: a pool with
// reuse_envelopes > 1 stashes a leg that finished its envelope cleanly
// instead of QUITing it. Reuse itself (picking the stashed leg back up
// for the session's next envelope) is Task 4 — here only the stash side
// is observable: no QUIT went out, and nothing dialed again.
func TestReuseStashKeepsConnOpen(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(srv.Addr())
	cfg.Pools[0].ReuseEnvelopes = 2
	f := newRelayClient(t, cfg)

	sendOneEnvelope(f, "one")

	if got := srv.DialCount(); got != 1 {
		t.Errorf("DialCount = %d, want 1: the leg is stashed, not re-dialed", got)
	}
	if got := srv.CmdCount("QUIT"); got != 0 {
		t.Errorf("backend QUIT count = %d, want 0: a stashed leg is not QUIT", got)
	}
}

// TestReuseSessionEndClosesStashedConn is task-3 scenario 2: a leg
// stashed on the session's affinity slot must not outlive the session.
// The fake sets no read deadline of its own on an accepted connection
// (see fakesmtp's TestStopRacesSetUp), so Stop's wg.Wait hangs forever on
// a per-connection goroutine still blocked reading a leg Bifrost never
// closed — a bounded wait turns a leaked leg into a clean failure instead
// of stalling the suite, the same technique fakesmtp's own tests use to
// prove a connection was actually closed.
func TestReuseSessionEndClosesStashedConn(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(srv.Addr())
	cfg.Pools[0].ReuseEnvelopes = 2
	f := newRelayClient(t, cfg)

	sendOneEnvelope(f, "one")

	f.send("QUIT")
	f.expect("221 2.0.0 Bye")
	f.expectClosed()

	done := make(chan struct{})
	go func() { srv.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stashed backend leg was not closed on session end")
	}
}

// TestReuseEnvelopesZeroRegression pins today's behavior when reuse is
// not configured (the zero value, disabled): the leg is politely QUIT
// after a clean verdict exactly as before this task.
func TestReuseEnvelopesZeroRegression(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	f := newRelayClient(t, relayConfig(srv.Addr())) // ReuseEnvelopes: 0

	sendOneEnvelope(f, "one")

	if got := srv.CmdCount("QUIT"); got != 1 {
		t.Errorf("backend QUIT count = %d, want 1: reuse_envelopes=0 keeps today's polite close", got)
	}
}

// TestReuseSecondStashDoesNotLeakFirst guards the double-stash path Task
// 4 introduces: once attachAndRelay itself can consume the session's
// affinity slot, a second envelope in a row on the same leg goes through
// stash() again too — this proves the consume-then-restash round trip
// (tryReuse's own clear of the slot, stash()'s pre-existing closeIfAny)
// does not double-process or orphan the connection.
//
// reuse_envelopes=3 keeps both envelopes under the cap so both take the
// stash path — a k==N envelope caps and QUITs instead of stashing, which
// is TestReuseCapRollover's job, not this one — and Server.Stop below
// hangs forever on a leaked leg (fakesmtp sets no read deadline), which
// is exactly what makes it a real regression check and not a vacuous one.
func TestReuseSecondStashDoesNotLeakFirst(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(srv.Addr())
	cfg.Pools[0].ReuseEnvelopes = 3
	f := newRelayClient(t, cfg)

	sendOneEnvelope(f, "one")
	sendOneEnvelope(f, "two")

	if got := srv.DialCount(); got != 1 {
		t.Fatalf("DialCount = %d, want 1: envelope 2 reuses envelope 1's leg instead of dialing fresh", got)
	}

	f.send("QUIT")
	f.expect("221 2.0.0 Bye")
	f.expectClosed()

	done := make(chan struct{})
	go func() { srv.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stashed backend leg was never closed: the second stash leaked it")
	}
}

// TestReuseHappyPathAcrossEnvelopes is Task 4 scenario 1: three envelopes
// on a pool with reuse_envelopes=3 all share the one leg envelope 1
// dialed, each later envelope preceded by a revalidation RSET the client
// never sees, and the third envelope's clean finish hits the cap.
func TestReuseHappyPathAcrossEnvelopes(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(srv.Addr())
	cfg.Pools[0].ReuseEnvelopes = 3
	m := &reuseMetrics{}
	f := newRelayClientPickMetrics(t, cfg, slog.New(slog.DiscardHandler), serversOf(cfg), m)

	sendOneEnvelope(f, "one")
	sendOneEnvelope(f, "two")
	sendOneEnvelope(f, "three")

	if got := srv.DialCount(); got != 1 {
		t.Errorf("DialCount = %d, want 1: all three envelopes share one dialed leg", got)
	}
	if got := srv.CmdCount("RSET"); got != 2 {
		t.Errorf("backend RSET count = %d, want 2: one revalidation before envelope 2, one before envelope 3", got)
	}
	if got := m.count("reused"); got != 2 {
		t.Errorf(`BackendReuse("reused") = %d, want 2: envelopes 2 and 3 each reused the cached leg`, got)
	}
	if got := m.count("capped"); got != 1 {
		t.Errorf(`BackendReuse("capped") = %d, want 1: envelope 3 hit reuse_envelopes=3 and the leg was closed after it`, got)
	}
}

// TestReuseCapRollover is Task 4 scenario 2: reuse_envelopes=2 caps the
// reused leg after envelope 2, and envelope 3 dials fresh again — a
// second cycle starting, not a leak or a wedge.
func TestReuseCapRollover(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(srv.Addr())
	cfg.Pools[0].ReuseEnvelopes = 2
	m := &reuseMetrics{}
	f := newRelayClientPickMetrics(t, cfg, slog.New(slog.DiscardHandler), serversOf(cfg), m)

	sendOneEnvelope(f, "one")
	sendOneEnvelope(f, "two")
	sendOneEnvelope(f, "three")

	if got := srv.DialCount(); got != 2 {
		t.Errorf("DialCount = %d, want 2: envelope 2 caps the first leg, envelope 3 dials fresh", got)
	}
	if got := m.count("reused"); got != 1 {
		t.Errorf(`BackendReuse("reused") = %d, want 1: only envelope 2 reused the first leg`, got)
	}
	if got := m.count("capped"); got != 1 {
		t.Errorf(`BackendReuse("capped") = %d, want 1: envelope 2 hit the cap`, got)
	}
}

// TestReuseDeadCachedConnFailsOverTransparently is Task 4 scenario 3: the
// cached leg dies between envelopes (simulated here as dying to the
// revalidation RSET itself — the fake has no way to sever an established
// connection any earlier, and the effect on this test is identical
// either way: the leg fails before it ever reaches the client). The
// primary is also taken down for new dials, so envelope 2's failure
// really is invisible to the client only if the walk fails over to a
// second, healthy candidate.
func TestReuseDeadCachedConnFailsOverTransparently(t *testing.T) {
	primary := relayFake(t, fakesmtp.Script{
		OnRSET: []fakesmtp.Step{{Action: fakesmtp.ActDropConn}},
	})
	secondary := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(primary.Addr(), secondary.Addr())
	cfg.Pools[0].ReuseEnvelopes = 3
	f := newRelayClient(t, cfg)

	sendOneEnvelope(f, "one") // dials and stashes primary's leg

	primary.SetDown(fakesmtp.DownListenerClosed) // no fresh dial to primary either

	sendOneEnvelope(f, "two") // revalidation kills the cached leg; transparent failover to secondary

	if got := primary.DialCount(); got != 1 {
		t.Errorf("primary DialCount = %d, want 1: never re-dialed once it went down", got)
	}
	if got := secondary.DialCount(); got != 1 {
		t.Errorf("secondary DialCount = %d, want 1: envelope 2 fails over to it", got)
	}
}

// TestReuseServerMismatchDialsFreshNoReuseMetric is Task 4 scenario 4: the
// router's pick moves to a different server between envelopes (weighted
// or round-robin balance), so the cached leg's server no longer matches
// candidates[0] and reuse is skipped outright — envelope 2 dials the new
// server fresh, and the stale cached leg is left for stash()'s own
// closeIfAny to clean up the next time this session stashes (no proactive
// close needed in the reuse path itself).
func TestReuseServerMismatchDialsFreshNoReuseMetric(t *testing.T) {
	s1 := relayFake(t, fakesmtp.Script{})
	s2 := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(s1.Addr(), s2.Addr())
	cfg.Pools[0].ReuseEnvelopes = 3
	m := &reuseMetrics{}
	h := &config.Holder{}
	h.Swap(cfg)
	sig := &stubSignals{}
	leases := &leaseCounter{}
	picks := &pickStub{servers: serversOf(cfg), rr: true}
	r := NewRelay(picks.pick, h, slog.New(slog.DiscardHandler), sig, leases.lease, m)
	c := newTestClient(t, cfg, nil, r)
	f := &relayFixture{testClient: c, sig: sig, leases: leases, picks: picks}
	f.expect("220 bifrost.test ESMTP")
	f.send("EHLO client.example")
	f.reply()

	sendOneEnvelope(f, "one") // pick is [s1, s2]: dials and stashes s1

	sendOneEnvelope(f, "two") // pick rotates to [s2, s1]: candidates[0] != cached s1, no reuse

	if got := s1.DialCount(); got != 1 {
		t.Errorf("s1 DialCount = %d, want 1: envelope 1's own dial only", got)
	}
	if got := s2.DialCount(); got != 1 {
		t.Errorf("s2 DialCount = %d, want 1: envelope 2 dials the new pick fresh", got)
	}
	if got := m.count("reused"); got != 0 {
		t.Errorf(`BackendReuse("reused") = %d, want 0: the candidate pointer changed, so reuse is skipped`, got)
	}
}

// TestReuseLeaseDenialRetainsCache is Task 4 scenario 5: a lease denial
// on the reuse attempt must not lose the cached leg. The spec prefers
// the conn stay cached for a later envelope once whatever was holding
// the pool's slot lets go — proved here by envelope 3 reusing the very
// same leg envelope 1 dialed, with no dial of its own between it and
// envelope 2's own saturated attempt.
func TestReuseLeaseDenialRetainsCache(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(srv.Addr())
	cfg.Pools[0].ReuseEnvelopes = 3
	m := &reuseMetrics{}
	h := &config.Holder{}
	h.Swap(cfg)
	sig := &stubSignals{}
	gate := &leaseGate{}
	picks := &pickStub{servers: serversOf(cfg)}
	r := NewRelay(picks.pick, h, slog.New(slog.DiscardHandler), sig, gate.lease, m)
	c := newTestClient(t, cfg, nil, r)
	f := &relayFixture{testClient: c, sig: sig, picks: picks}
	f.expect("220 bifrost.test ESMTP")
	f.send("EHLO client.example")
	f.reply()

	sendOneEnvelope(f, "one") // dials and stashes the one leg

	gate.setHeld(true) // a concurrent transaction now holds the pool's slot

	f.send("MAIL FROM:<two@b.example>")
	f.expect("451 4.3.2 All backends busy, try again later")
	f.send("RSET")
	f.expect("250 2.0.0 OK")

	gate.setHeld(false) // the concurrent holder released

	sendOneEnvelope(f, "three") // reuses envelope 1's untouched cached leg

	if got := srv.DialCount(); got != 2 {
		t.Errorf("DialCount = %d, want 2: envelope 1's own dial, plus the walk's own fresh-dial fallback when envelope 2's reuse attempt found the lease denied; envelope 3 must not dial again", got)
	}
	if got := srv.CmdCount("RSET"); got != 2 {
		t.Errorf("backend RSET count = %d, want 2: revalidation before envelope 2's (denied) reuse attempt and before envelope 3's (granted) one", got)
	}
	if got := m.count("reused"); got != 1 {
		t.Errorf(`BackendReuse("reused") = %d, want 1: only envelope 3 actually reused the cache`, got)
	}
}

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
	h := &config.Holder{}
	h.Swap(cfg)
	sig := &stubSignals{}
	leases := &leaseCounter{}
	picks := &pickStub{servers: servers}

	r := NewRelay(picks.pick, h, lg, sig, leases.lease)
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

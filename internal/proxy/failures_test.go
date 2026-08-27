//go:build integration

package proxy

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// The failure half of the Transparency Contract: a backend can die at
// every stage, and every stage has exactly one client-visible answer —
// never a 5xx (that would bounce mail a healthy backend would take), and
// never two replies for one DATA.

// logCapture is a goroutine-safe log sink for the rows whose evidence is
// a structured log record.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *logCapture) logger() *slog.Logger { return slog.New(slog.NewTextHandler(c, nil)) }

// startBody drives a client through to a relayed 354, ready to stream a
// body one piece at a time.
func (f *relayFixture) startBody() {
	f.t.Helper()
	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")
	f.send("DATA")
	f.expect("354 Start mail input; end with <CRLF>.<CRLF>")
}

// bodyPieces writes n body lines, each in its own write, then the
// terminator: enough separate chunks that a backend which stopped reading
// or died is noticed while the body is still flowing.
func (f *relayFixture) bodyPieces(n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		f.raw("body line filler\r\n")
	}
	f.raw(".\r\n")
}

func TestBackendDiesPre354(t *testing.T) {
	// io-error-is-4xx-never-5xx: an infrastructure failure must never
	// become a permanent code, or an outage starts bouncing mail.
	srv := relayFake(t, fakesmtp.Script{OnRCPT: []fakesmtp.Step{{Action: fakesmtp.ActRST}}})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("451 4.4.1 Backend connection lost")

	// The transaction is fatally latched, the session is not.
	f.send("RCPT TO:<e@d.example>")
	f.expect("451 4.4.1 Backend connection lost")
	f.send("RSET")
	f.expect("250 2.0.0 OK")
	f.send("MAIL FROM:<a2@b.example>")
	f.expect("250 2.1.0 OK")
	if got := srv.DialCount(); got != 2 {
		t.Errorf("DialCount = %d, want 2", got)
	}
}

func TestBackendDiesMidDataDiscardToDot(t *testing.T) {
	// backend-dies-mid-data-client-gets-451, and the desync guard: the
	// balancer pays for the rest of the client's message rather than let
	// body bytes be parsed as commands.
	srv := relayFake(t, fakesmtp.Script{MidBody: []fakesmtp.Step{{Action: fakesmtp.ActRST}}})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.startBody()
	f.bodyPieces(12)
	f.expect("451 4.4.1 Backend connection lost")

	// In sync: the command after the terminator is a command.
	f.send("NOOP")
	f.expect("250 2.0.0 OK")
	f.send("MAIL FROM:<a2@b.example>")
	f.expect("250 2.1.0 OK")
}

func TestBackendDiesAfterDot(t *testing.T) {
	// The duplicate-delivery window: the backend has the whole message and
	// might have queued it, but said nothing.
	srv := relayFake(t, fakesmtp.Script{OnEOD: []fakesmtp.Step{{Action: fakesmtp.ActDropConn}}})
	logs := &logCapture{}
	f := newRelayClientLog(t, relayConfig(srv.Addr()), logs.logger())

	f.startBody()
	f.bodyPieces(2)
	f.expect("451 4.4.2 Backend timeout")

	f.send("NOOP")
	f.expect("250 2.0.0 OK")
	if out := logs.String(); !strings.Contains(out, "duplicate delivery risk") {
		t.Errorf("logs do not record the duplicate-risk event:\n%s", out)
	}
}

func TestBackend421Translated(t *testing.T) {
	// The one sanctioned rewrite: relaying 421 verbatim would announce
	// that the *client's* connection is closing, which it is not.
	srv := relayFake(t, fakesmtp.Script{
		OnRCPT: []fakesmtp.Step{{Reply: "421 4.7.0 closing connection"}},
	})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("451 4.4.2 Backend closed the transaction")

	// Session alive, leg dropped without a QUIT, next MAIL attaches anew.
	f.send("RSET")
	f.expect("250 2.0.0 OK")
	f.send("MAIL FROM:<a2@b.example>")
	f.expect("250 2.1.0 OK")
	if got := srv.CmdCount("QUIT"); got != 0 {
		t.Errorf("backend QUIT count = %d, want 0: a 421 leg is dropped, not chatted to", got)
	}
	if got := srv.DialCount(); got != 2 {
		t.Errorf("DialCount = %d, want 2", got)
	}
	_, trans, _ := f.sig.counts()
	if want := []string{"s0"}; !slices.Equal(trans, want) {
		t.Errorf("TransportError signals = %v, want %v", trans, want)
	}
}

func TestBackendEarlyReplyMidData(t *testing.T) {
	// The single-verdict rule: a final reply between the relayed 354 and
	// the dot IS the transaction's verdict. It is relayed verbatim, the
	// rest of the body is consumed, and nothing follows it.
	srv := relayFake(t, fakesmtp.Script{
		MidBody: []fakesmtp.Step{
			{Reply: "552 5.3.4 message too big, giving up"},
			{Action: fakesmtp.ActDropConn},
		},
		MidBodyLines: 2,
	})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.startBody()
	f.bodyPieces(12)
	f.expect("552 5.3.4 message too big, giving up")

	// Exactly one reply for this DATA: if anything had been emitted after
	// the dot, it would be sitting here instead of the NOOP's 250.
	f.send("NOOP")
	f.expect("250 2.0.0 OK")
	f.send("MAIL FROM:<a2@b.example>")
	f.expect("250 2.1.0 OK")
}

func TestBackendEarlyReplyMidDataKeepsPiping(t *testing.T) {
	// The other half of the mid-DATA row: an early verdict from a backend
	// that stays up does not stop the pipe — the rest of the message is
	// still relayed, terminator included, so the backend leg never sits
	// waiting on a message that will not finish.
	srv := relayFake(t, fakesmtp.Script{
		MidBody:      []fakesmtp.Step{{Reply: "552 5.3.4 too big, but I am still here"}},
		MidBodyLines: 1,
	})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.startBody()
	f.raw("first line\r\n")
	f.expect("552 5.3.4 too big, but I am still here")
	f.raw("second line\r\n")
	f.raw(".\r\n")

	// Still exactly one reply for this DATA: the backend's own end-of-data
	// reply is never read, let alone relayed.
	f.send("NOOP")
	f.expect("250 2.0.0 OK")

	srv.AssertWireBody(t, 0, []byte("first line\r\nsecond line\r\n"))
}

func TestBackend421MidData(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{
		MidBody: []fakesmtp.Step{
			{Reply: "421 4.7.0 shutting down mid message"},
			{Action: fakesmtp.ActDropConn},
		},
		MidBodyLines: 2,
	})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.startBody()
	f.bodyPieces(12)
	f.expect("451 4.4.2 Backend closed the transaction")

	f.send("NOOP")
	f.expect("250 2.0.0 OK")
	f.send("MAIL FROM:<a2@b.example>")
	f.expect("250 2.1.0 OK")
}

func TestMalformedBackendReplyTreatedAsDeath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{"garbage", "this is not an SMTP reply"},
		{"oversized", "250 " + strings.Repeat("x", 5000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := relayFake(t, fakesmtp.Script{OnMAIL: []fakesmtp.Step{{Reply: tc.reply}}})
			f := newRelayClient(t, relayConfig(srv.Addr()))

			f.send("MAIL FROM:<a@b.example>")
			f.expect("451 4.4.1 Backend connection lost")

			// Nothing unparseable is ever forwarded, and the session lives.
			f.send("NOOP")
			f.expect("250 2.0.0 OK")
		})
	}
}

func TestClientAbortMidData(t *testing.T) {
	// The client walks away mid-body: the backend leg must see a bare
	// disconnect, never a terminator it could mistake for a whole message.
	srv := relayFake(t, fakesmtp.Script{})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.startBody()
	f.raw("partial body with no terminator\r\n")
	if err := f.conn.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := f.wait(); err != nil {
		t.Fatalf("session Run = %v, want nil for a client hangup", err)
	}

	lines := backendLines(t, srv, 0)
	if slices.Contains(lines, ".\r\n") {
		t.Errorf("backend saw a DATA terminator for an aborted message: %q", lines)
	}
	if got := srv.CmdCount("QUIT"); got != 0 {
		t.Errorf("backend QUIT count = %d, want 0: an aborted body is a hard close", got)
	}
}

func TestBackendReplyTimeout451(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{OnMAIL: []fakesmtp.Step{{Action: fakesmtp.ActHang}}})
	cfg := relayConfig(srv.Addr())
	cfg.Defaults.Timeouts.BackendMailReply = 150 * time.Millisecond
	f := newRelayClient(t, cfg)

	f.send("MAIL FROM:<a@b.example>")
	f.expect("451 4.4.2 Backend timeout")

	f.send("NOOP")
	f.expect("250 2.0.0 OK")
	_, trans, _ := f.sig.counts()
	if want := []string{"s0"}; !slices.Equal(trans, want) {
		t.Errorf("TransportError signals = %v, want %v", trans, want)
	}
}

func TestSessionLifetimeInsideTransaction(t *testing.T) {
	// The session-lifetime cap is an absolute bound. The relay re-arms the
	// client deadline once per phase, so without a clamp a transaction
	// could hold the session open indefinitely — one phase at a time — and
	// the contract's lifetime row would never fire.
	srv := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(srv.Addr())
	cfg.Defaults.Timeouts.SessionMax = 300 * time.Millisecond
	f := newRelayClient(t, cfg)

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")

	// Now go quiet inside the transaction. The idle timeout is 10 s and
	// the backend is happy: only the lifetime cap can end this.
	if err := f.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	f.expect("421 4.4.2 Session lifetime exceeded, closing connection")
	f.expectClosed()
}

func TestMalformedAfterContinuationClosesConnection(t *testing.T) {
	// The malformed-reply exception: the first line of this reply is
	// already on the client's wire and cannot be un-sent, so no 451 may
	// be injected behind it. The connection closes instead.
	srv := relayFake(t, fakesmtp.Script{
		OnMAIL: []fakesmtp.Step{{Reply: "250-2.1.0 first line\r\nthis is not a reply line"}},
	})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	line, err := f.br.ReadString('\n')
	if err != nil {
		t.Fatalf("read continuation line: %v", err)
	}
	if want := "250-2.1.0 first line\r\n"; line != want {
		t.Fatalf("client line = %q, want %q", line, want)
	}
	if extra, err := f.br.ReadString('\n'); err == nil {
		t.Fatalf("client got %q after a torn reply, want the connection closed", extra)
	}
	if err := f.wait(); err != nil {
		t.Errorf("session Run = %v, want nil", err)
	}
	if got := srv.CmdCount("QUIT"); got != 0 {
		t.Errorf("backend QUIT count = %d, want 0: a torn leg is dropped", got)
	}
}

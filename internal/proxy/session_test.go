package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
)

// Every reply asserted in this package's tests is written out as a
// literal, never as one of replies.go's constants: the Transparency
// Contract table in /PROJECT.md is the spec, and comparing a constant
// against itself would prove nothing.

// testConfig is the baseline listener config: a hostname, the default
// capability set, no TLS. STARTTLS cases build on it in starttls_test.go.
func testConfig() *config.Config {
	return &config.Config{
		Listener: config.Listener{
			Hostname:     "bifrost.test",
			Capabilities: []string{"PIPELINING", "8BITMIME", "SIZE 10485760"},
		},
	}
}

// stubHandler stands in for epic-05's relay engine: it records the Txn it
// was handed and answers the contract's "no backend" reply. extra runs a
// test-supplied action instead of that default.
type stubHandler struct {
	mu      sync.Mutex
	txns    []*Txn
	drained [][]byte // lines an extra hook took off the pipelining queue
	extra   func(tx *Txn)
}

func (h *stubHandler) HandleTransaction(_ context.Context, tx *Txn) {
	h.mu.Lock()
	h.txns = append(h.txns, tx)
	extra := h.extra
	h.mu.Unlock()

	if extra != nil {
		extra(tx)
		return
	}
	_, _ = tx.W.WriteString(RplNoBackend)
	_ = tx.W.Flush()
}

func (h *stubHandler) seen() []*Txn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.txns)
}

func (h *stubHandler) drainedLines() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.drained)
}

// testClient is the client end of a session under test.
type testClient struct {
	t    testing.TB
	conn net.Conn
	br   *bufio.Reader
	done chan error

	runErr error
	ended  bool
}

// wait returns Run's error, waiting for the session goroutine to finish.
// It is safe to call more than once (t.Cleanup does).
func (c *testClient) wait() error {
	c.t.Helper()
	if !c.ended {
		select {
		case err := <-c.done:
			c.runErr, c.ended = err, true
		case <-time.After(5 * time.Second):
			c.t.Error("session did not return")
		}
	}
	return c.runErr
}

// startSession runs a Session over the server half of a connection pair
// and returns the client half. Run's error is available on done; the
// session is torn down (and its return awaited) by t.Cleanup.
func startSession(ctx context.Context, t *testing.T, cfg *config.Config, tlsCfg *tls.Config, h TxnHandler, srv, cli net.Conn) *testClient {
	t.Helper()
	s := NewSession(srv, cfg, tlsCfg, h, slog.New(slog.DiscardHandler))
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	c := &testClient{t: t, conn: cli, br: bufio.NewReader(cli), done: done}
	t.Cleanup(func() {
		_ = cli.Close()
		_ = c.wait()
	})
	return c
}

// newTestClient starts a session over net.Pipe: no listener, no accept
// loop (that is epic-08), just the two ends of one client connection.
func newTestClient(t *testing.T, cfg *config.Config, tlsCfg *tls.Config, h TxnHandler) *testClient {
	t.Helper()
	return newTestClientCtx(context.Background(), t, cfg, tlsCfg, h)
}

// newTestClientCtx is newTestClient with a caller-owned context, for the
// drain path (a canceled context ends the session with 421 4.3.0).
func newTestClientCtx(ctx context.Context, t *testing.T, cfg *config.Config, tlsCfg *tls.Config, h TxnHandler) *testClient {
	t.Helper()
	srv, cli := net.Pipe()
	return startSession(ctx, t, cfg, tlsCfg, h, srv, cli)
}

// newTCPTestClient starts a session over a real loopback TCP pair, for
// the cases that need a routable RemoteAddr (net.Pipe has none).
func newTCPTestClient(t *testing.T, cfg *config.Config, tlsCfg *tls.Config, h TxnHandler) *testClient {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		ch <- accepted{c, err}
	}()

	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatalf("accept: %v", got.err)
	}
	return startSession(context.Background(), t, cfg, tlsCfg, h, got.conn, cli)
}

func (c *testClient) send(line string) {
	c.t.Helper()
	c.raw(line + "\r\n")
}

func (c *testClient) raw(s string) {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(s)); err != nil {
		c.t.Fatalf("write %q: %v", s, err)
	}
}

// reply reads one full (possibly multiline) reply and returns its lines
// with the CRLF stripped — failing if any line is not CRLF-terminated.
func (c *testClient) reply() []string {
	c.t.Helper()
	var lines []string
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read reply after %v: %v", lines, err)
		}
		if !strings.HasSuffix(line, "\r\n") {
			c.t.Fatalf("reply line %q is not CRLF terminated", line)
		}
		line = strings.TrimSuffix(line, "\r\n")
		lines = append(lines, line)
		if len(line) < 4 || line[3] != '-' {
			return lines
		}
	}
}

// expect asserts the next reply is exactly want, line for line.
func (c *testClient) expect(want ...string) {
	c.t.Helper()
	if got := c.reply(); !slices.Equal(got, want) {
		c.t.Fatalf("reply = %q, want %q", got, want)
	}
}

// expectClosed asserts the session closed the connection with nothing
// further on the wire.
func (c *testClient) expectClosed() {
	c.t.Helper()
	if _, err := c.br.ReadByte(); err == nil {
		c.t.Fatalf("connection still open, want closed")
	}
}

func TestBannerAndEhlo(t *testing.T) {
	c := newTestClient(t, testConfig(), nil, &stubHandler{})

	c.expect("220 bifrost.test ESMTP")

	wantEhlo := []string{"250-bifrost.test", "250-PIPELINING", "250-8BITMIME", "250 SIZE 10485760"}
	c.send("EHLO client.example")
	c.expect(wantEhlo...)

	// RFC 5321 4.1.4: a second EHLO resets state and gets the same
	// capability reply.
	c.send("EHLO client.example")
	c.expect(wantEhlo...)
}

func TestHelo(t *testing.T) {
	c := newTestClient(t, testConfig(), nil, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	c.send("HELO client.example")
	c.expect("250 bifrost.test")
}

func TestPreAttachTrivia(t *testing.T) {
	c := newTestClient(t, testConfig(), nil, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	c.send("NOOP")
	c.expect("250 2.0.0 OK")
	c.send("RSET")
	c.expect("250 2.0.0 OK")
	c.send("VRFY postmaster")
	c.expect("252 2.5.2 Cannot VRFY user, but will accept message and attempt delivery")
	c.send("EXPN list")
	c.expect("502 5.5.1 Command not implemented")
	c.send("HELP")
	c.expect("214 2.0.0 See RFC 5321 for the supported commands")

	// Unknown command keeps the session open (RFC 5321 4.2.4).
	c.send("FROBNICATE now")
	c.expect("500 5.5.1 Command not recognized")
	c.send("NOOP")
	c.expect("250 2.0.0 OK")
}

func TestBareLfCommand500(t *testing.T) {
	h := &stubHandler{}
	c := newTestClient(t, testConfig(), nil, h)
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	// A bare-LF-terminated MAIL must never reach a handler: it is the
	// SMTP-smuggling vector, so it is answered 500 5.5.2 and dropped.
	c.raw("MAIL FROM:<a@b>\n")
	c.expect("500 5.5.2 Bare LF is not a valid line terminator")
	if got := h.seen(); len(got) != 0 {
		t.Fatalf("handler saw %d transactions, want 0", len(got))
	}

	// Still in sync: the next properly terminated command works.
	c.send("NOOP")
	c.expect("250 2.0.0 OK")
}

func TestBadSequence(t *testing.T) {
	c := newTestClient(t, testConfig(), nil, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	c.send("RCPT TO:<b@c>")
	c.expect("503 5.5.1 Bad sequence of commands")
	c.send("DATA")
	c.expect("503 5.5.1 Bad sequence of commands")

	// MAIL before any EHLO/HELO is the same violation.
	c2 := newTestClient(t, testConfig(), nil, &stubHandler{})
	c2.expect("220 bifrost.test ESMTP")
	c2.send("MAIL FROM:<a@b>")
	c2.expect("503 5.5.1 Bad sequence of commands")
}

func TestQuit(t *testing.T) {
	c := newTestClient(t, testConfig(), nil, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	c.send("QUIT")
	c.expect("221 2.0.0 Bye")
	c.expectClosed()
}

func TestAuthAndBdatRejected(t *testing.T) {
	c := newTestClient(t, testConfig(), nil, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	c.send("AUTH PLAIN AGFAYgBjCg==")
	c.expect("502 5.5.1 Command not implemented")
	c.send("BDAT 10")
	c.expect("502 5.5.1 Command not implemented")

	// Neither is advertised, and neither poisons the session.
	c.send("NOOP")
	c.expect("250 2.0.0 OK")
}

func TestMailReachesHandler(t *testing.T) {
	h := &stubHandler{}
	c := newTCPTestClient(t, testConfig(), nil, h)
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	c.send("MAIL FROM:<a@b> SIZE=100")
	c.expect("451 4.4.1 No backend available, try again later")

	got := h.seen()
	if len(got) != 1 {
		t.Fatalf("handler saw %d transactions, want 1", len(got))
	}
	tx := got[0]
	if tx.Helo != "client.example" {
		t.Errorf("Txn.Helo = %q, want %q", tx.Helo, "client.example")
	}
	if want := netip.MustParseAddr("127.0.0.1"); tx.ClientIP != want {
		t.Errorf("Txn.ClientIP = %v, want %v", tx.ClientIP, want)
	}
	if tx.R == nil || tx.W == nil {
		t.Errorf("Txn reader/writer handles = (%v, %v), want both set", tx.R, tx.W)
	}

	// The session owns the connection again once the handler returns.
	c.send("NOOP")
	c.expect("250 2.0.0 OK")
}

func TestEhloEmptyArgument501(t *testing.T) {
	c := newTestClient(t, testConfig(), nil, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	// RFC 5321 4.1.1.1: the domain argument is not optional.
	c.send("EHLO")
	c.expect("501 5.5.4 Syntax error: EHLO requires a domain")
	c.send("HELO   ")
	c.expect("501 5.5.4 Syntax error: EHLO requires a domain")

	// A refused greeting leaves the session ungreeted.
	c.send("MAIL FROM:<a@b>")
	c.expect("503 5.5.1 Bad sequence of commands")

	// And a real greeting still works, HELO included.
	c.send("HELO client.example")
	c.expect("250 bifrost.test")
}

func TestClientHangupAfterQuit(t *testing.T) {
	c := newTestClient(t, testConfig(), nil, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	// Hang up without reading the 221: the reply write fails on a closed
	// pipe, which is the client leaving, not a session error.
	c.send("QUIT")
	if err := c.conn.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := c.wait(); err != nil {
		t.Fatalf("Run = %v, want nil for a client hangup", err)
	}
}

func TestDrainGoodbye421(t *testing.T) {
	// The idle deadline is armed before the MAIL read and expires while
	// the handler runs, so the drain reply needs a write window of its
	// own — without one the 421 never reaches the wire.
	cfg := testConfig()
	cfg.Defaults.Timeouts.ClientIdle = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := &stubHandler{}
	h.extra = func(tx *Txn) {
		cancel()
		// A handler with phase timers of its own re-arms the deadline per
		// phase — and lets it expire again before returning, which is
		// what leaves the drain reply with no window.
		_ = tx.setWriteDeadline(time.Now().Add(30 * time.Millisecond))
		_ = tx.setReadDeadline(time.Now().Add(30 * time.Millisecond))
		_, _ = tx.W.WriteString(RplNoBackend)
		_ = tx.W.Flush()
		time.Sleep(60 * time.Millisecond)
	}

	c := newTestClientCtx(ctx, t, cfg, nil, h)
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()
	c.send("MAIL FROM:<a@b>")
	c.expect("451 4.4.1 No backend available, try again later")

	c.expect("421 4.3.0 Service shutting down, closing connection")
	c.expectClosed()
}

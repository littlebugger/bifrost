// Package smtpdrv implements a scripted SMTP client driver for tests.
//
// It plays every client role in later Bifrost epics' integration and
// load tests: happy paths, pipelining, and deliberate protocol
// violations. Like fakesmtp, it reads and writes the wire by hand
// (bufio only) and never through net/textproto, so a caller driving a
// protocol-violation test (a bare LF, a hard mid-body close) gets
// exactly the bytes it asked for, with no normalization surprises.
//
// Conn reports failures via testing.TB.Fatalf by default, which the
// testing package only allows from the goroutine running the test. A
// caller driving many Conns concurrently — a load generator, several
// goroutines in one test — must call SetFail on each Conn with a
// goroutine-safe reporter before touching it from any goroutine other
// than the one that called Dial.
package smtpdrv

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
)

// closeQuietly closes c, discarding the error. Used for best-effort
// cleanup where there is nothing sensible to do with a failure.
func closeQuietly(c io.Closer) {
	_ = c.Close()
}

// Conn is a scripted SMTP client connection.
type Conn struct {
	t    testing.TB
	conn net.Conn
	r    *bufio.Reader
	fail func(format string, args ...any)
}

// SetFail overrides how c reports a failure (default: t.Fatalf). Install
// a goroutine-safe reporter — e.g. t.Errorf followed by returning out of
// the calling goroutine — before driving c from any goroutine other than
// the one that called Dial; testing.TB forbids FailNow (what Fatalf
// does) anywhere else.
func (c *Conn) SetFail(f func(format string, args ...any)) {
	c.fail = f
}

// Reply is one full (possibly multiline) SMTP reply.
type Reply struct {
	Code  string   // the reply code, e.g. "250"
	Lines []string // each line's full text (code included), in order
}

// Dial connects to addr and returns a Conn ready to drive the session.
// It does not consume the banner itself — call Expect("220") first, like
// any other reply. The connection is closed automatically via t.Cleanup.
func Dial(t testing.TB, addr string) *Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("smtpdrv: dial %s: %v", addr, err)
	}
	t.Cleanup(func() { closeQuietly(conn) })
	c := &Conn{t: t, conn: conn, r: bufio.NewReader(conn)}
	c.fail = c.t.Fatalf
	return c
}

// nopTB is a minimal, private testing.TB stand-in for DialAddr. Every
// Conn method calls only Helper() on c.t (Fatalf/Cleanup are used solely
// by Dial's own testing.TB path above), so a no-op Helper is all it
// needs. Embedding the interface, rather than implementing it outright,
// is required to satisfy testing.TB's sealed private method from outside
// package testing; every other promoted method stays nil and unreachable
// since nothing ever calls it.
type nopTB struct{ testing.TB }

func (nopTB) Helper() {}

// DialAddr is Dial's non-testing counterpart, for callers with no
// testing.TB — a load generator's concurrent worker goroutines. It
// returns a dial error instead of calling Fatalf, and the returned
// Conn's default fail reporter records the message without aborting the
// goroutine (there is no test to fail; SetFail may still be used to
// install a more useful reporter). There is no t.Cleanup, so the caller
// must Close the connection itself.
//
// Deviation (recorded, epic-11): smtpdrv's whole API assumes a
// testing.TB. DialAddr is the minimal non-testing entry point
// cmd/loadgen needs; the testing.TB path (Dial, above) is unchanged.
func DialAddr(addr string) (*Conn, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("smtpdrv: dial %s: %w", addr, err)
	}
	c := &Conn{t: nopTB{}, conn: conn, r: bufio.NewReader(conn)}
	c.fail = func(string, ...any) {}
	return c, nil
}

// Close closes the underlying connection. Dial's Conn is closed
// automatically via t.Cleanup; Close exists for DialAddr callers, which
// have no test lifecycle to hook it to.
func (c *Conn) Close() error {
	return c.conn.Close()
}

// Send writes one command line, appending CRLF.
func (c *Conn) Send(line string) {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		c.fail("smtpdrv: send %q: %v", line, err)
	}
}

// Pipeline writes every line, each with CRLF appended, in a single Write
// call — for testing that a server replies to a pipelined batch in
// order.
func (c *Conn) Pipeline(lines ...string) {
	c.t.Helper()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	if _, err := c.conn.Write([]byte(b.String())); err != nil {
		c.fail("smtpdrv: pipeline: %v", err)
	}
}

// Raw writes b verbatim: no CRLF appended, no processing at all. For
// protocol-violation tests (bare LF, oversized/garbage lines, mid-command
// splits).
func (c *Conn) Raw(b []byte) {
	c.t.Helper()
	if _, err := c.conn.Write(b); err != nil {
		c.fail("smtpdrv: raw write: %v", err)
	}
}

// Expect reads one full reply and asserts its code starts with
// codePrefix (e.g. "2", "25", or "250").
func (c *Conn) Expect(codePrefix string) Reply {
	c.t.Helper()
	reply, err := c.readReply()
	if err != nil {
		c.fail("smtpdrv: read reply (want prefix %s): %v", codePrefix, err)
	}
	if !strings.HasPrefix(reply.Code, codePrefix) {
		c.fail("smtpdrv: reply code = %s, want prefix %s (lines: %v)", reply.Code, codePrefix, reply.Lines)
	}
	return reply
}

// ExpectN reads len(codePrefixes) replies in order, asserting each in
// turn. For a pipelined batch, where replies come back in command order
// (RFC 2920) but not necessarily one at a time.
func (c *Conn) ExpectN(codePrefixes ...string) []Reply {
	c.t.Helper()
	out := make([]Reply, len(codePrefixes))
	for i, p := range codePrefixes {
		out[i] = c.Expect(p)
	}
	return out
}

// readReply reads one full (possibly multiline) SMTP reply by hand:
// bufio only, manual "250-" (continues) vs "250 " (final line) handling
// per RFC 5321 §4.2.1 — no net/textproto, matching fakesmtp.
func (c *Conn) readReply() (Reply, error) {
	var rep Reply
	for {
		raw, err := c.r.ReadString('\n')
		if err != nil {
			return Reply{}, err
		}
		line := strings.TrimRight(raw, "\r\n")
		if len(line) < 4 {
			return Reply{}, fmt.Errorf("smtpdrv: short reply line %q", line)
		}
		rep.Code = line[:3]
		rep.Lines = append(rep.Lines, line)
		if line[3] != '-' {
			return rep, nil
		}
	}
}

// SendMsg runs the standard MAIL/RCPT/DATA/body/. sequence for message
// index i (the envelope and body are both derived from i, so callers —
// e.g. a load generator — can send many distinct messages without
// building their own each time), and returns the reply that ends the
// attempt: the end-of-data verdict on the normal path, or DATA's own
// reply immediately if it wasn't a 3yz go-ahead.
//
// That check is Expect("3")'s return value, not just its side effect on
// c.fail: with a non-aborting SetFail reporter installed (any concurrent
// caller must install one), Expect still returns after a mismatch
// instead of stopping the sequence, and writing a body the peer never
// invited — a real client wouldn't — would desync the session for
// whatever comes after. c.Expect("3") is kept (not a bare readReply) so
// the default aborting reporter still fires exactly where it always
// did.
func (c *Conn) SendMsg(i int) Reply {
	c.t.Helper()
	c.Send(fmt.Sprintf("MAIL FROM:<sender%d@test.example>", i))
	c.Expect("2")
	c.Send(fmt.Sprintf("RCPT TO:<rcpt%d@test.example>", i))
	c.Expect("2")
	c.Send("DATA")
	dataReply := c.Expect("3")
	if !strings.HasPrefix(dataReply.Code, "3") {
		return dataReply
	}
	c.writeDotStuffed(fmt.Sprintf("Subject: message %d\r\n\r\nbody %d\r\n", i, i))
	reply, err := c.readReply()
	if err != nil {
		c.fail("smtpdrv: read end-of-data reply: %v", err)
	}
	return reply
}

// writeDotStuffed writes body dot-stuffed and terminated, mirroring what
// a real SMTP client puts on the wire: any line starting with "." gets
// an extra "." prepended, followed by the ".\r\n" terminator. body may
// end in "\n" or "\r\n" (a single trailing one is trimmed first, so it
// isn't rendered as an extra blank line before the terminator).
func (c *Conn) writeDotStuffed(body string) {
	c.t.Helper()
	body = strings.TrimSuffix(strings.TrimSuffix(body, "\n"), "\r")

	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, ".") {
			b.WriteByte('.')
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	b.WriteString(".\r\n")
	if _, err := c.conn.Write([]byte(b.String())); err != nil {
		c.fail("smtpdrv: write body: %v", err)
	}
}

// AbortMidData runs MAIL/RCPT/DATA, writes afterBytes bytes of filler
// body content, then hard-closes the connection without ever sending
// the terminator — simulating a client that abandons a transaction
// mid-transfer.
func (c *Conn) AbortMidData(afterBytes int) {
	c.t.Helper()
	c.Send("MAIL FROM:<abort@test.example>")
	c.Expect("2")
	c.Send("RCPT TO:<rcpt@test.example>")
	c.Expect("2")
	c.Send("DATA")
	c.Expect("3")
	filler := strings.Repeat("x", afterBytes)
	if _, err := c.conn.Write([]byte(filler)); err != nil {
		c.fail("smtpdrv: write filler: %v", err)
	}
	closeQuietly(c.conn)
}

// StartTLS sends STARTTLS, expects the 220 go-ahead, then upgrades the
// connection to TLS using cfg.
func (c *Conn) StartTLS(cfg *tls.Config) {
	c.t.Helper()
	c.Send("STARTTLS")
	c.Expect("220")
	tconn := tls.Client(c.conn, cfg)
	if err := tconn.Handshake(); err != nil {
		c.fail("smtpdrv: TLS handshake: %v", err)
	}
	c.conn = tconn
	c.r = bufio.NewReader(tconn)
}

//go:build integration

package integration

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// This file is the epic-10 tests' own SMTP client. internal/smtpdrv is
// the driver for happy paths; the drain, reload and timeout tests need
// three things it cannot express — a deadline on every read ("the server
// said this within N"), an explicit "the server closed the connection",
// and byte-exact comparison against internal/proxy's reply enum.

// rawClient is a minimal hand-rolled SMTP client with a deadline on
// every read. internal/smtpdrv is the driver for happy paths, but it
// reports through Fatalf and has no deadlines, and these tests need three
// things it cannot express: "the server said this within N" (a drain or a
// timeout row), "the server closed the connection", and byte-exact
// comparison against internal/proxy's reply enum.
type rawClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func dialRaw(t *testing.T, addr string) *rawClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &rawClient{t: t, conn: conn, br: bufio.NewReader(conn)}
}

// send writes one command line with CRLF; raw writes bytes verbatim.
func (c *rawClient) send(line string) { c.raw([]byte(line + "\r\n")) }

func (c *rawClient) raw(b []byte) {
	c.t.Helper()
	if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		c.t.Fatalf("set write deadline: %v", err)
	}
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatalf("write %q: %v", b, err)
	}
}

// reply reads one whole (possibly multiline) reply within d and returns
// its final line, CRLF stripped.
func (c *rawClient) reply(d time.Duration) string {
	c.t.Helper()
	line, err := c.replyErr(d)
	if err != nil {
		c.t.Fatalf("read reply within %s: %v", d, err)
	}
	return line
}

func (c *rawClient) replyErr(d time.Duration) (string, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		return "", err
	}
	for {
		raw, err := c.br.ReadString('\n')
		if err != nil {
			return "", err
		}
		line := strings.TrimRight(raw, "\r\n")
		if len(line) < 4 || line[3] != '-' {
			return line, nil
		}
	}
}

// expectClosed asserts the peer closes the connection within d without
// sending anything more — the "+ close" half of every 421 row.
func (c *rawClient) expectClosed(d time.Duration) {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		c.t.Fatalf("set read deadline: %v", err)
	}
	if n, err := c.br.Read(make([]byte, 1)); err == nil {
		c.t.Fatalf("connection still open after %d more byte(s), want it closed", n)
	}
}

// wantReply compares a reply line against internal/proxy's synthesized
// enum byte for byte (the enum carries its CRLF; the wire line does not).
func wantReply(t *testing.T, what, got, enum string) {
	t.Helper()
	if want := strings.TrimSuffix(enum, "\r\n"); got != want {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

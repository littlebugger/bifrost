package backend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// testServer builds the minimal *config.Server Dial needs: a dial
// address. The other fields (weight, backup, checks...) belong to
// balance/health, not to one connection attempt.
func testServer(addr string) *config.Server {
	return &config.Server{Name: "test", Address: addr}
}

// testTimeouts returns generously long timeouts for tests that only care
// about the happy path; tests exercising a specific deadline override
// just that field.
func testTimeouts() config.Timeouts {
	return config.Timeouts{
		BackendConnect:   2 * time.Second,
		BackendHandshake: 2 * time.Second,
		BackendMailReply: 2 * time.Second,
		Backend354Wait:   2 * time.Second,
		DataProgress:     2 * time.Second,
		BackendFinalDot:  2 * time.Second,
	}
}

func dialTest(t *testing.T, addr string, opts Opts) (*Conn, error) {
	t.Helper()
	c, err := Dial(context.Background(), testServer(addr), opts)
	if c != nil {
		// c.conn (not the not-yet-existent Abort/Quit) since this helper
		// is Task 1's; later tasks' tests use the real teardown methods.
		t.Cleanup(func() { _ = c.conn.Close() })
	}
	return c, err
}

func TestDialHappyPath(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING", "8BITMIME", "SIZE 10485760"}})

	c, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: testTimeouts()})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	caps := c.Caps()
	if !caps.Has("PIPELINING") {
		t.Errorf("Caps() = %v, want PIPELINING", caps)
	}
	if !caps.Has("8BITMIME") {
		t.Errorf("Caps() = %v, want 8BITMIME", caps)
	}
	n, ok := caps.Size()
	if !ok || n != 10485760 {
		t.Errorf("Caps().Size() = (%d, %v), want (10485760, true)", n, ok)
	}
}

func TestDialMultilineGreeting(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{
		Banner: fakesmtp.Step{Reply: "220-a\r\n220 b"},
		Caps:   []string{"PIPELINING"},
	})

	c, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: testTimeouts()})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if !c.Caps().Has("PIPELINING") {
		t.Errorf("Caps() = %v, want PIPELINING", c.Caps())
	}
}

func TestDialBadGreeting(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Banner: fakesmtp.Step{Reply: "554 no"}})

	_, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: testTimeouts()})
	var herr *HandshakeError
	if !errors.As(err, &herr) {
		t.Fatalf("Dial err = %v (%T), want *HandshakeError", err, err)
	}
}

func TestDialRefused(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{})
	srv.SetDown(fakesmtp.DownListenerClosed)

	_, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: testTimeouts()})
	var derr *DialError
	if !errors.As(err, &derr) {
		t.Fatalf("Dial err = %v (%T), want *DialError", err, err)
	}
}

func TestDialGreetingTimeout(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{})
	srv.SetDown(fakesmtp.DownAcceptThenHang)

	to := testTimeouts()
	to.BackendHandshake = 150 * time.Millisecond

	start := time.Now()
	_, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: to})
	elapsed := time.Since(start)

	var herr *HandshakeError
	if !errors.As(err, &herr) {
		t.Fatalf("Dial err = %v (%T), want *HandshakeError (distinct from *DialError)", err, err)
	}
	var derr *DialError
	if errors.As(err, &derr) {
		t.Fatalf("Dial err = %v, want NOT *DialError (TCP connect succeeded; the hang is post-connect)", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Dial took %v, want well under 2s (BackendHandshake=150ms)", elapsed)
	}
}

func TestEhloRejected(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{
		OnEHLO: []fakesmtp.Step{{Reply: "502 5.5.1 command not implemented"}},
	})

	_, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: testTimeouts()})
	var herr *HandshakeError
	if !errors.As(err, &herr) {
		t.Fatalf("Dial err = %v (%T), want *HandshakeError", err, err)
	}
	if !strings.Contains(herr.Error(), "ehlo") && herr.Stage != "ehlo" {
		t.Errorf("HandshakeError stage = %q, want an ehlo-stage error: %v", herr.Stage, err)
	}
}

// TestDialRSTMidHandshake: the TCP connect itself succeeds (the fake
// accepts), so an RST that arrives instead of a banner must surface as
// a *HandshakeError (the greeting stage), never a *DialError — the same
// distinction TestDialGreetingTimeout draws for a hang instead of a
// reset.
func TestDialRSTMidHandshake(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{})
	srv.SetDown(fakesmtp.DownAcceptThenRST)

	_, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: testTimeouts()})
	var herr *HandshakeError
	if !errors.As(err, &herr) {
		t.Fatalf("Dial err = %v (%T), want *HandshakeError", err, err)
	}
	if herr.Stage != "greeting" {
		t.Errorf("HandshakeError.Stage = %q, want %q", herr.Stage, "greeting")
	}
	var derr *DialError
	if errors.As(err, &derr) {
		t.Fatalf("Dial err = %v, want NOT *DialError (TCP was accepted; the RST happened post-accept)", err)
	}
}

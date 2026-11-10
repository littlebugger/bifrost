//go:build integration

package proxy

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
)

// dialListener connects to addr and returns the raw connection plus a
// buffered reader over it -- one level below the smtpdrv/testClient
// fixtures built for session-level protocol tests, for asserting Serve's
// own accept-time behavior directly (banner presence/absence, the
// overload reply, the close that follows it).
func dialListener(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, bufio.NewReader(conn)
}

// readLine reads one CRLF-terminated line, terminator stripped.
func readLine(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read line: %v", err)
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
}

// expectClosedConn asserts the connection has nothing further on the
// wire and is, or will imminently be, closed.
func expectClosedConn(t *testing.T, br *bufio.Reader) {
	t.Helper()
	if _, err := br.ReadByte(); err == nil {
		t.Fatalf("connection still open, want closed")
	}
}

// runServe starts Serve over ln in the background and arranges for
// ctx's cancellation, then Serve's return, to be joined by t.Cleanup.
// active is forwarded to Serve unchanged (nil for a private, per-call
// counter) so callers proving the shared-counter behavior can pass one
// shared *atomic.Int64 across two calls.
func runServe(t *testing.T, ctx context.Context, cancel context.CancelFunc, ln net.Listener, cfg *config.Config, active *atomic.Int64) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, cfg, cfg.PrimaryListener(), nil, &stubHandler{}, slog.New(slog.DiscardHandler), active)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not return after ctx cancel")
		}
	})
}

// TestMaxconnBoundary: with global_maxconn = 2, two connections stay
// live; a third is accepted and immediately refused with the contract's
// 421 4.3.2, no banner, connection closed; once one of the first two
// ends (QUIT), a fourth is admitted normally.
func TestMaxconnBoundary(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.GlobalMaxConn = 2

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	runServe(t, ctx, cancel, ln, cfg, nil)

	c1, br1 := dialListener(t, addr)
	if got := readLine(t, br1); got != "220 bifrost.test ESMTP" {
		t.Fatalf("c1 banner = %q", got)
	}
	_, br2 := dialListener(t, addr)
	if got := readLine(t, br2); got != "220 bifrost.test ESMTP" {
		t.Fatalf("c2 banner = %q", got)
	}

	// Third connection: over cap, refused before any banner.
	_, br3 := dialListener(t, addr)
	if got := readLine(t, br3); got != "421 4.3.2 Too many connections, try again later" {
		t.Fatalf("c3 first line = %q, want the overload reply", got)
	}
	expectClosedConn(t, br3)

	// End c1's session; observing its connection fully close guarantees
	// (same-goroutine ordering inside Serve) the freed slot is already
	// accounted for.
	if _, err := c1.Write([]byte("QUIT\r\n")); err != nil {
		t.Fatalf("c1 QUIT: %v", err)
	}
	if got := readLine(t, br1); got != "221 2.0.0 Bye" {
		t.Fatalf("c1 QUIT reply = %q", got)
	}
	expectClosedConn(t, br1)

	// A fourth connection is admitted now that a slot is free. The one
	// residual race (this dial landing a hair before Serve's own
	// goroutine finishes decrementing its counter) is bounded, not
	// slept around: retry briefly instead of asserting on the first
	// attempt.
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, br4 := dialListener(t, addr)
		got := readLine(t, br4)
		if got == "220 bifrost.test ESMTP" {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("c4 first line = %q, want a fresh banner (slot freed)", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSharedMaxconnAcrossListeners: global_maxconn is a PROCESS cap, not a
// per-listener one — the final-review fix for item 1 (cmd/bifrost now
// hoists one *atomic.Int64 out of Serve and hands it to every listener's
// accept loop). Two Serve calls given the SAME counter, GlobalMaxConn=1:
// one session open on listener a must still cap listener b, proving the
// counter is shared rather than each Serve silently keeping its own.
func TestSharedMaxconnAcrossListeners(t *testing.T) {
	cfg := testConfig()
	cfg.Listeners = append(cfg.Listeners, config.Listener{Name: "b", Hostname: "bifrost.test"})
	cfg.Limits.GlobalMaxConn = 1

	lnA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen a: %v", err)
	}
	lnB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen b: %v", err)
	}
	addrA, addrB := lnA.Addr().String(), lnB.Addr().String()

	var shared atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	doneA, doneB := make(chan error, 1), make(chan error, 1)
	go func() {
		doneA <- Serve(ctx, lnA, cfg, &cfg.Listeners[0], nil, &stubHandler{}, slog.New(slog.DiscardHandler), &shared)
	}()
	go func() {
		doneB <- Serve(ctx, lnB, cfg, &cfg.Listeners[1], nil, &stubHandler{}, slog.New(slog.DiscardHandler), &shared)
	}()
	t.Cleanup(func() {
		cancel()
		for _, done := range []chan error{doneA, doneB} {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Serve returned %v, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve did not return after ctx cancel")
			}
		}
	})

	cA, brA := dialListener(t, addrA)
	if got := readLine(t, brA); got != "220 bifrost.test ESMTP" {
		t.Fatalf("a banner = %q", got)
	}

	// a now holds the process's one connection slot: a dial to b -- a
	// DIFFERENT listener's socket -- must still be refused if the cap is
	// truly shared rather than per-listener.
	_, brB := dialListener(t, addrB)
	if got := readLine(t, brB); got != "421 4.3.2 Too many connections, try again later" {
		t.Fatalf("b first line while a holds the slot = %q, want the shared-cap overload reply", got)
	}
	expectClosedConn(t, brB)

	// Freeing a's slot must admit b.
	if _, err := cA.Write([]byte("QUIT\r\n")); err != nil {
		t.Fatalf("a QUIT: %v", err)
	}
	if got := readLine(t, brA); got != "221 2.0.0 Bye" {
		t.Fatalf("a QUIT reply = %q", got)
	}
	expectClosedConn(t, brA)

	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, br := dialListener(t, addrB)
		got := readLine(t, br)
		if got == "220 bifrost.test ESMTP" {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("b first line = %q, want a fresh banner (slot freed by a's close)", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// flakyListener wraps a real listener and fails the first n Accept
// calls with a non-fatal, non-net.ErrClosed error, to exercise Serve's
// accept-error backoff without needing a real EMFILE.
type flakyListener struct {
	net.Listener
	fails int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.fails > 0 {
		l.fails--
		return nil, errors.New("injected accept failure")
	}
	return l.Listener.Accept()
}

// TestAcceptErrorBackoff: an Accept that fails several times in a row
// makes Serve back off and retry -- never exit -- and recover once
// Accept starts succeeding again.
func TestAcceptErrorBackoff(t *testing.T) {
	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := real.Addr().String()
	ln := &flakyListener{Listener: real, fails: 3}

	ctx, cancel := context.WithCancel(context.Background())
	runServe(t, ctx, cancel, ln, testConfig(), nil)

	// The kernel completes the handshake into the listen backlog
	// independently of when Serve's Accept actually runs, so this dial
	// itself doesn't prove recovery -- reading the banner does: it can
	// only arrive once Serve's loop has retried past the three injected
	// failures and reached the real Accept.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	br := bufio.NewReader(conn)
	if got := readLine(t, br); got != "220 bifrost.test ESMTP" {
		t.Fatalf("banner after recovering from accept errors = %q", got)
	}
}

// TestServeStopsOnCtxCancel: canceling ctx makes Serve close its own
// listener and return cleanly -- Accept's resulting net.ErrClosed is a
// shutdown, not a failure to report.
func TestServeStopsOnCtxCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	cfg := testConfig()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, cfg, cfg.PrimaryListener(), nil, &stubHandler{}, slog.New(slog.DiscardHandler), nil)
	}()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}

	if _, err := net.Dial("tcp", addr); err == nil {
		t.Fatal("listener still accepting after Serve returned")
	}
}

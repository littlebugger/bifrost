//go:build chaos

package chaos

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/proxy"
)

// Epic-10 Task 5, client-side scenarios: what a hostile, broken or simply
// dying client does to the sessions around it (nothing, is the claim).

// TestChaosClientDropMidDATA: ten clients abort mid-body at once. Every
// backend leg must be aborted without a terminator — a backend that saw a
// dot would queue a truncated message — and nothing may be left running
// (the package's goleak TestMain is the other half of this assertion).
func TestChaosClientDropMidDATA(t *testing.T) {
	const clients = 10

	f := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	s := newStack(t, poolConfig(chaosTimeouts(), "none", f.Addr()))
	base := stableGoroutineCount()

	eachClient(t, clients, func(i int) {
		c, err := dialSMTP(s.addr)
		if err != nil {
			t.Errorf("client %d: dial: %v", i, err)
			return
		}
		if err := c.greet(fmt.Sprintf("abort%d", i)); err != nil {
			t.Errorf("client %d: greet: %v", i, err)
			c.close()
			return
		}
		if got, err := c.cmd("MAIL FROM:<sender@chaos.test>"); err != nil || !strings.HasPrefix(got, "250") {
			t.Errorf("client %d: MAIL = %q (%v)", i, got, err)
			c.close()
			return
		}
		if got, err := c.cmd("RCPT TO:<rcpt@chaos.test>"); err != nil || !strings.HasPrefix(got, "250") {
			t.Errorf("client %d: RCPT = %q (%v)", i, got, err)
			c.close()
			return
		}
		if got, err := c.cmd("DATA"); err != nil || !strings.HasPrefix(got, "354") {
			t.Errorf("client %d: DATA = %q (%v)", i, got, err)
			c.close()
			return
		}
		if err := c.write("Subject: abandoned\r\n\r\nhalf a message"); err != nil {
			t.Errorf("client %d: partial body: %v", i, err)
		}
		c.rst() // crash, mid-message
	})

	// Every leg was opened and every one was abandoned without a dot.
	waitFor(t, "all aborted legs to be recorded", func() bool { return len(f.Sessions()) >= clients })
	var bodyBytes int64
	for i, sess := range f.Sessions() {
		if got := len(sess.Messages()); got != 0 {
			t.Errorf("backend session %d recorded %d messages, want 0: a dot was synthesized for an abandoned body", i, got)
		}
		bodyBytes += sess.BodyBytes()
	}
	// Some partial body did reach the backend, so these really were
	// mid-DATA aborts. Not every session: an RST discards whatever the
	// client had written but the balancer had not yet read, which is a
	// crashed client's own data loss, not the balancer's.
	if bodyBytes == 0 {
		t.Errorf("no partial body bytes reached the backend at all, want the aborts to have happened mid-DATA")
	}
	if got := settleGoroutines(base); got > base {
		t.Errorf("goroutines after %d aborted sessions = %d, want back to baseline %d", clients, got, base)
	}
}

// TestChaosClientQUITMidDATAWait: the client sends QUIT while the balancer
// is still waiting for the end-of-data verdict. The verdict must arrive
// first (it is the transaction's single reply, and it is already owed),
// and only then the 221 — never the other way round, and never both
// interleaved.
func TestChaosClientQUITMidDATAWait(t *testing.T) {
	f := fakesmtp.Start(t, fakesmtp.Script{
		Caps:  backendCaps(),
		OnEOD: []fakesmtp.Step{{Delay: 200 * time.Millisecond, Reply: "250 2.0.0 OK: queued"}},
	})
	s := newStack(t, poolConfig(chaosTimeouts(), "none", f.Addr()))

	c := openSession(t, s.addr, "quitwait")
	if c == nil {
		return
	}
	if got, err := c.cmd("MAIL FROM:<sender@chaos.test>"); err != nil || !strings.HasPrefix(got, "250") {
		t.Fatalf("MAIL = %q (%v)", got, err)
	}
	if got, err := c.cmd("RCPT TO:<rcpt@chaos.test>"); err != nil || !strings.HasPrefix(got, "250") {
		t.Fatalf("RCPT = %q (%v)", got, err)
	}
	if got, err := c.cmd("DATA"); err != nil || !strings.HasPrefix(got, "354") {
		t.Fatalf("DATA = %q (%v)", got, err)
	}
	// Body, terminator, and QUIT in one write: the QUIT is on the wire
	// before the verdict comes back.
	if err := c.write(bodyFor("quitwait") + "QUIT\r\n"); err != nil {
		t.Fatalf("body+QUIT: %v", err)
	}

	if got, err := c.reply(); err != nil || got != "250 2.0.0 OK: queued" {
		t.Fatalf("first reply = %q (%v), want the end-of-data verdict", got, err)
	}
	if got, err := c.reply(); err != nil || got != synth(proxy.RplBye) {
		t.Errorf("second reply = %q (%v), want %q", got, err, synth(proxy.RplBye))
	}
	if _, err := c.reply(); err == nil {
		t.Errorf("the connection is still open after 221, want it closed")
	}
	if n := recordedBodies(f)[wantBody("quitwait")]; n != 1 {
		t.Errorf("backend recorded the message %d times, want 1", n)
	}
}

// TestChaosSlowLorisClient: one client drips a byte at a time inside DATA,
// slower than the progress watchdog allows, while a neighbour keeps sending
// real messages on its own session. The loris is cut off; the neighbour
// notices nothing.
func TestChaosSlowLorisClient(t *testing.T) {
	const neighbourMessages = 5

	f := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	cfg := poolConfig(timeoutsWith(func(tt *config.Timeouts) {
		tt.DataProgress = 200 * time.Millisecond
	}), "none", f.Addr())
	s := newStack(t, cfg)

	neighbourDone := make(chan struct{})
	go func() {
		defer close(neighbourDone)
		c := openSession(t, s.addr, "neighbour")
		if c == nil {
			return
		}
		for i := 0; i < neighbourMessages; i++ {
			got, err := c.message(fmt.Sprintf("neighbour-%d", i))
			if err != nil {
				t.Errorf("neighbour message %d: %v", i, err)
				return
			}
			if got != "250 2.0.0 OK: queued" {
				t.Errorf("neighbour message %d verdict = %q, want the backend's 250", i, got)
				return
			}
		}
	}()

	loris := openSession(t, s.addr, "loris")
	if loris == nil {
		return
	}
	if got, err := loris.cmd("MAIL FROM:<loris@chaos.test>"); err != nil || !strings.HasPrefix(got, "250") {
		t.Fatalf("MAIL = %q (%v)", got, err)
	}
	if got, err := loris.cmd("RCPT TO:<rcpt@chaos.test>"); err != nil || !strings.HasPrefix(got, "250") {
		t.Fatalf("RCPT = %q (%v)", got, err)
	}
	if got, err := loris.cmd("DATA"); err != nil || !strings.HasPrefix(got, "354") {
		t.Fatalf("DATA = %q (%v)", got, err)
	}

	// One byte per pass, paced by waiting for a reply that has not come
	// yet: the wait IS the drip, so there is no sleep to tune. The gap
	// (300ms) is longer than the watchdog (200ms), which is what makes
	// this a stall rather than slow-but-moving progress.
	var got string
	for i := 0; i < 20 && got == ""; i++ {
		if err := loris.write("x"); err != nil {
			break // the balancer already closed on us; the reply is buffered
		}
		if lines, err := loris.linesWithin(300 * time.Millisecond); err == nil && len(lines) > 0 {
			got = lines[len(lines)-1]
		}
	}
	if got == "" {
		if lines, err := loris.linesWithin(2 * time.Second); err == nil && len(lines) > 0 {
			got = lines[len(lines)-1]
		}
	}
	if want := synth(proxy.RplIdleTimeout); got != want {
		t.Errorf("loris reply = %q, want %q", got, want)
	}
	if _, err := loris.linesWithin(2 * time.Second); err == nil {
		t.Errorf("the loris connection is still open, want it closed after the 421")
	}

	<-neighbourDone
	for i := 0; i < neighbourMessages; i++ {
		if n := recordedBodies(f)[wantBody(fmt.Sprintf("neighbour-%d", i))]; n != 1 {
			t.Errorf("neighbour message %d arrived %d times, want 1", i, n)
		}
	}
}

// TestChaosClientProtocolViolations: eight sessions interleave every
// protocol violation the contract has a reply for, concurrently, while
// sending real mail. Each violation gets its exact synthesized reply, each
// session stays in sync, and each session's own message arrives exactly
// once — the added dimension over epic-03's single-session suite is that
// none of it crosses session boundaries.
func TestChaosClientProtocolViolations(t *testing.T) {
	const clients = 8

	f := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	s := newStack(t, poolConfig(chaosTimeouts(), "none", f.Addr()))

	eachClient(t, clients, func(i int) {
		c := openSession(t, s.addr, fmt.Sprintf("violator%d", i))
		if c == nil {
			return
		}
		check := func(what, sent, want string) {
			got, err := c.cmd(sent)
			if err != nil {
				t.Errorf("client %d: %s: %v", i, what, err)
				return
			}
			if got != want {
				t.Errorf("client %d: %s = %q, want %q", i, what, got, want)
			}
		}

		check("unknown verb", "FROB nicely", synth(proxy.RplUnknownCmd))
		check("RCPT before MAIL", "RCPT TO:<rcpt@chaos.test>", synth(proxy.RplBadSequence))
		check("DATA before MAIL", "DATA", synth(proxy.RplBadSequence))
		check("AUTH", "AUTH PLAIN dGVzdA==", synth(proxy.RplNotImplemented))
		check("BDAT", "BDAT 10", synth(proxy.RplNotImplemented))
		check("over-long line", "NOOP "+strings.Repeat("z", 5000), synth(proxy.RplLineTooLong))

		// A bare LF terminator is never relayed anywhere, and the session
		// stays in sync afterwards.
		if err := c.write("NOOP\n"); err != nil {
			t.Errorf("client %d: bare LF write: %v", i, err)
			return
		}
		if got, err := c.reply(); err != nil || got != synth(proxy.RplBareLf) {
			t.Errorf("client %d: bare LF = %q (%v), want %q", i, got, err, synth(proxy.RplBareLf))
		}

		// Still in sync: a real message goes through on the same session.
		marker := fmt.Sprintf("violator-%d", i)
		got, err := c.message(marker)
		if err != nil {
			t.Errorf("client %d: message after the violations: %v", i, err)
			return
		}
		if got != "250 2.0.0 OK: queued" {
			t.Errorf("client %d: verdict after the violations = %q, want the backend's 250", i, got)
		}

		// An empty EHLO is a syntax error AND resets the session, so it
		// goes last: MAIL is refused until a real greeting arrives again.
		check("empty EHLO", "EHLO", synth(proxy.RplEhloSyntax))
		check("MAIL while ungreeted", "MAIL FROM:<sender@chaos.test>", synth(proxy.RplBadSequence))
	})

	bodies := recordedBodies(f)
	if len(bodies) != clients {
		t.Errorf("backend recorded %d distinct messages, want one per session (%d)", len(bodies), clients)
	}
	for i := 0; i < clients; i++ {
		if n := bodies[wantBody(fmt.Sprintf("violator-%d", i))]; n != 1 {
			t.Errorf("session %d's message arrived %d times, want exactly 1 (cross-session corruption?)", i, n)
		}
	}
}

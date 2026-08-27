//go:build integration

package integration

import (
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/fakesmtp"
	"github.com/revolee/bifrost/internal/proxy"
)

// Epic-10 Task 2: SIGTERM drain. The sequence PROJECT.md specifies is an
// ORDER, not a set of steps, so these tests assert the order: /healthz
// answers 503 while the listener is still accepting, an idle session is
// told to go away while a transaction is still in flight, and the force
// deadline closes the backend leg before the client hears anything.

// drainFixture starts a process with one named backend and the given
// timeout overrides.
func drainFixture(t *testing.T, backend *fakesmtp.Server, timeouts map[string]string) *bifrost {
	t.Helper()
	smtp, adminAddr := freeAddr(t), freeAddr(t)
	cfg := fixture{
		smtp:     smtp,
		admin:    adminAddr,
		timeouts: withTimeouts(timeouts),
		pools:    poolHCL("p", serverHCL("a", backend.Addr(), 1)),
	}.render()
	return startBifrost(t, writeFile(t, t.TempDir(), "bifrost.hcl", cfg), smtp, adminAddr)
}

// TestDrainSequence walks the whole sequence on one live process:
//
//   - /healthz flips to 503 first, and the listener is STILL accepting
//     while it says so (that is what the lame duck is for);
//   - an idle session gets 421 4.3.0 promptly — while a transaction on
//     another connection is still mid-DATA, which is what makes this an
//     ordering claim rather than a timing coincidence;
//   - the mid-DATA transaction finishes, its backend's verdict relayed
//     verbatim, and only then is that session told 421 4.3.0;
//   - the process exits 0.
func TestDrainSequence(t *testing.T) {
	backend := namedFake(t, "A")
	b := drainFixture(t, backend, map[string]string{"lame_duck": "1s"})

	// An idle session: greeted, then sitting between commands.
	idle := dialRaw(t, b.smtp)
	if got := idle.reply(5 * time.Second); !strings.HasPrefix(got, "220 ") {
		t.Fatalf("idle banner = %q, want a 220", got)
	}
	idle.send("EHLO idle.example")
	if got := idle.reply(5 * time.Second); !strings.HasPrefix(got, "250") {
		t.Fatalf("idle EHLO reply = %q, want 250", got)
	}

	// A session parked mid-DATA: 354 relayed, body started, no dot yet.
	inFlight := dialRaw(t, b.smtp)
	inFlight.reply(5 * time.Second)
	inFlight.send("EHLO busy.example")
	inFlight.reply(5 * time.Second)
	inFlight.send("MAIL FROM:<sender@test.example>")
	inFlight.reply(5 * time.Second)
	inFlight.send("RCPT TO:<rcpt@test.example>")
	inFlight.reply(5 * time.Second)
	inFlight.send("DATA")
	if got := inFlight.reply(5 * time.Second); !strings.HasPrefix(got, "354") {
		t.Fatalf("DATA reply = %q, want 354", got)
	}
	inFlight.raw([]byte("Subject: mid-drain\r\n\r\npartial"))

	b.signal(syscall.SIGTERM)

	// 1. healthz says 503 before anything else changes...
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := b.get("/healthz")
		if code == http.StatusServiceUnavailable {
			if !strings.Contains(body, "draining") {
				t.Errorf("/healthz body = %q, want it to say draining", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz never answered 503 after SIGTERM (last: %d %q)", code, body)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// ...and the listener is still accepting while it does: the lame-duck
	// window exists precisely so an upstream balancer can react before
	// connections start being refused.
	lameDuckConn := dialRaw(t, b.smtp)
	if got := lameDuckConn.reply(5 * time.Second); !strings.HasPrefix(got, "220 ") {
		t.Fatalf("banner during the lame duck = %q, want a 220: the listener closed before healthz went 503", got)
	}

	// 2. the idle session is answered and closed — with the mid-DATA
	// transaction still unfinished, so this cannot be "the drain ended".
	wantReply(t, "idle session drain reply", idle.reply(10*time.Second), proxy.RplShuttingDown)
	idle.expectClosed(5 * time.Second)
	wantReply(t, "lame-duck session drain reply", lameDuckConn.reply(10*time.Second), proxy.RplShuttingDown)

	// 3. the in-flight transaction finishes, verdict verbatim...
	inFlight.raw([]byte(" body\r\n.\r\n"))
	if got, want := inFlight.reply(10*time.Second), "250 2.0.0 OK: queued via A"; got != want {
		t.Errorf("in-flight verdict = %q, want %q (relayed verbatim through the drain)", got, want)
	}
	// ...and only then is its session told the process is going away.
	wantReply(t, "in-flight session drain reply", inFlight.reply(10*time.Second), proxy.RplShuttingDown)
	inFlight.expectClosed(5 * time.Second)

	if code := b.waitExit(20 * time.Second); code != 0 {
		t.Errorf("exit code = %d, want 0\nlogs:\n%s", code, b.logText())
	}
	if logs := b.logText(); !strings.Contains(logs, "drain complete: every session ended") {
		t.Errorf("logs do not report a clean drain:\n%s", logs)
	}

	// The message reached the backend exactly once, with its own dot.
	backend.AssertWireBody(t, 0, []byte("Subject: mid-drain\r\n\r\npartial body\r\n"))
}

// TestDrainForceDeadline is the force path: a backend that never answers
// the final dot, and a drain_timeout that runs out while the transaction
// waits on it. PROJECT.md's order is normative here — the backend leg is
// aborted FIRST, so nothing is half-delivered and no dot is ever
// synthesized, and only then does the client hear about it (451 for the
// abandoned transaction, then the session's 421 4.3.0). The process must
// still exit 0, well inside the deadline plus the goodbye grace.
func TestDrainForceDeadline(t *testing.T) {
	backend := fakesmtp.Start(t, fakesmtp.Script{
		Caps:  backendCaps(),
		OnEOD: []fakesmtp.Step{{Action: fakesmtp.ActHang}},
	})
	b := drainFixture(t, backend, map[string]string{
		"lame_duck":         "50ms",
		"drain_timeout":     "1s",
		"backend_final_dot": "60s", // never reached: the force deadline is what ends this
	})

	c := dialRaw(t, b.smtp)
	c.reply(5 * time.Second)
	c.send("EHLO force.example")
	c.reply(5 * time.Second)
	c.send("MAIL FROM:<sender@test.example>")
	c.reply(5 * time.Second)
	c.send("RCPT TO:<rcpt@test.example>")
	c.reply(5 * time.Second)
	c.send("DATA")
	c.reply(5 * time.Second)
	c.raw([]byte("Subject: hang\r\n\r\nbody\r\n.\r\n"))

	// The dot is in; the backend is scripted never to answer it. Wait for
	// the fake to have recorded the whole message before draining, so the
	// force path is provably interrupting a wait for the verdict rather
	// than racing the body.
	waitMessages(t, backend, 1)

	start := time.Now()
	b.signal(syscall.SIGTERM)

	// The 451 can only come from the pump's read failing, i.e. from the
	// leg having been closed: it IS the evidence for "backend legs first".
	wantReply(t, "forced transaction reply", c.reply(20*time.Second), proxy.RplBackendTimeout)
	wantReply(t, "forced session reply", c.reply(10*time.Second), proxy.RplShuttingDown)
	c.expectClosed(5 * time.Second)

	if code := b.waitExit(20 * time.Second); code != 0 {
		t.Errorf("exit code = %d, want 0\nlogs:\n%s", code, b.logText())
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("drain took %s, want it bounded by drain_timeout (1s) + goodbye grace (5s)", elapsed)
	}
	if logs := b.logText(); !strings.Contains(logs, "drain force deadline reached") {
		t.Errorf("logs do not report the force deadline:\n%s", logs)
	}

	if logs := b.logText(); !strings.Contains(logs, "duplicate delivery risk") {
		t.Errorf("logs do not record the duplicate-delivery window (the dot was delivered, the verdict never arrived):\n%s", logs)
	}

	// No dot beyond the client's own, and no polite QUIT on the leg that
	// was aborted: the backend holds exactly one message, byte for byte.
	// (CmdCount would also count the health prober's own QUITs, which is
	// why this asserts on the traffic session's transcript.)
	backend.AssertWireBody(t, 0, []byte("Subject: hang\r\n\r\nbody\r\n"))
	assertNoQuitOnTrafficSession(t, backend)
}

// assertNoQuitOnTrafficSession asserts that the backend session which
// actually carried a message never received a QUIT: an aborted leg gets a
// bare disconnect (RFC 5321 3.8), never a polite teardown that would read
// as a completed conversation.
func assertNoQuitOnTrafficSession(t *testing.T, srv *fakesmtp.Server) {
	t.Helper()
	for _, s := range srv.Sessions() {
		if len(s.Messages()) == 0 {
			continue // a health probe's session
		}
		for _, ev := range s.Transcript() {
			if ev.Verb == "QUIT" {
				t.Errorf("backend session %d received QUIT after its leg was aborted", ev.SessionID)
			}
		}
	}
}

// TestDrainExitBounded is the shutdown's hard guarantee: the process
// exits, full stop. A client that keeps feeding bytes holds its session
// open indefinitely — after the force deadline aborts its backend leg the
// relay discards to the dot, re-arming data_progress per chunk, so the
// session can legitimately outlive every drain deadline all the way to
// session_max. Waiting for it would make "kill -TERM" hang, so the
// goodbye grace bounds the wait and the process leaves anyway.
func TestDrainExitBounded(t *testing.T) {
	backend := namedFake(t, "A")
	b := drainFixture(t, backend, map[string]string{
		"lame_duck":     "50ms",
		"drain_timeout": "1s",
		"data_progress": "30s", // generous: the dribble keeps re-arming it
		"session_max":   "10m", // the timer that would otherwise end this
	})

	c := dialRaw(t, b.smtp)
	c.reply(5 * time.Second)
	c.send("EHLO dribble.example")
	c.reply(5 * time.Second)
	c.send("MAIL FROM:<sender@test.example>")
	c.reply(5 * time.Second)
	c.send("RCPT TO:<rcpt@test.example>")
	c.reply(5 * time.Second)
	c.send("DATA")
	if got := c.reply(5 * time.Second); !strings.HasPrefix(got, "354") {
		t.Fatalf("DATA reply = %q, want 354", got)
	}
	c.raw([]byte("Subject: endless\r\n\r\n"))

	// Keep dribbling body bytes — never the terminator — until the process
	// is gone. The writes are on their own goroutine because they will
	// block once nothing is reading them any more.
	stopDribble := make(chan struct{})
	dribbleDone := make(chan struct{})
	go func() {
		defer close(dribbleDone)
		for {
			select {
			case <-stopDribble:
				return
			default:
			}
			if err := c.conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
				return
			}
			if _, err := c.conn.Write([]byte("still sending\r\n")); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	defer func() {
		close(stopDribble)
		<-dribbleDone
	}()

	start := time.Now()
	b.signal(syscall.SIGTERM)
	code := b.waitExit(20 * time.Second)
	elapsed := time.Since(start)

	if code != 0 {
		t.Errorf("exit code = %d, want 0\nlogs:\n%s", code, b.logText())
	}
	// drain_timeout (1s) + goodbye grace (5s) + slack. Before the wait was
	// bounded this process stayed alive for session_max.
	if elapsed > 12*time.Second {
		t.Errorf("exit took %s while a client kept dribbling, want it bounded by drain_timeout + the 5s goodbye grace", elapsed)
	}
	t.Logf("exited %v after SIGTERM with a session still feeding bytes", elapsed)
	if logs := b.logText(); !strings.Contains(logs, "components still running; exiting anyway") {
		t.Errorf("logs do not report the bounded exit:\n%s", logs)
	}
}

// TestDrainSignalReal is the plain signal contract, with no traffic at
// all: SIGTERM to the real process exits 0.
func TestDrainSignalReal(t *testing.T) {
	b := drainFixture(t, namedFake(t, "A"), nil)
	b.signal(syscall.SIGTERM)
	if code := b.waitExit(20 * time.Second); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0\nlogs:\n%s", code, b.logText())
	}
	if logs := b.logText(); !strings.Contains(logs, "shutdown signal received") {
		t.Errorf("logs do not report the signal:\n%s", logs)
	}
}

// waitMessages polls (bounded) until srv has recorded n complete
// messages across all its sessions.
func waitMessages(t *testing.T, srv *fakesmtp.Server, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		total := 0
		for _, s := range srv.Sessions() {
			total += len(s.Messages())
		}
		if total >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend recorded %d messages, want %d", total, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

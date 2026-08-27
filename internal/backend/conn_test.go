package backend

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/fakesmtp"
)

func TestSendLineVerbatim(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{})
	c, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: testTimeouts()})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	raw := []byte("MAIL FROM:<a@b>   BODY=8BITMIME  SIZE=123\r\n")
	if err := c.SendLine(raw); err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	if _, _, _, err := c.Replies().Next(); err != nil {
		t.Fatalf("Replies().Next(): %v", err)
	}

	sessions := srv.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("Sessions() len = %d, want 1", len(sessions))
	}
	var got []byte
	for _, ev := range sessions[0].Transcript() {
		if ev.Verb == "MAIL" {
			got = ev.Line
		}
	}
	if string(got) != string(raw) {
		t.Errorf("recorded MAIL line = %q, want %q (byte-exact, odd spacing included)", got, raw)
	}
}

// TestCommandClassDeadlines checks both that the zero-value Class
// (MailRcpt, never set via SetCommandClass) is the one that governs a
// MAIL command, and that its deadline (here shortened) actually fires
// against a backend that never replies.
func TestCommandClassDeadlines(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{
		OnMAIL: []fakesmtp.Step{{Action: fakesmtp.ActHang}},
	})

	to := testTimeouts()
	to.BackendMailReply = 150 * time.Millisecond
	c, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: to})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	if err := c.SendLine([]byte("MAIL FROM:<a@b>\r\n")); err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	start := time.Now()
	_, _, _, err = c.Replies().Next()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Replies().Next() err = nil, want a deadline error (fake hangs on MAIL, class defaults to MailRcpt)")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Next() took %v, want close to BackendMailReply (150ms)", elapsed)
	}
}

func TestQuitBestEffort(t *testing.T) {
	t.Run("normal fake records QUIT", func(t *testing.T) {
		srv := fakesmtp.Start(t, fakesmtp.Script{})
		c, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: testTimeouts()})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}

		c.Quit()

		if got := srv.CmdCount("QUIT"); got != 1 {
			t.Errorf("CmdCount(QUIT) = %d, want 1", got)
		}
	})

	t.Run("hanging fake still returns within budget", func(t *testing.T) {
		srv := fakesmtp.Start(t, fakesmtp.Script{
			OnQUIT: []fakesmtp.Step{{Action: fakesmtp.ActHang}},
		})
		c, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: testTimeouts()})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}

		start := time.Now()
		done := make(chan struct{})
		go func() {
			c.Quit()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("Quit() did not return within 3s, want <= %v", quitDeadline)
		}
		if elapsed := time.Since(start); elapsed > quitDeadline+time.Second {
			t.Errorf("Quit() took %v, want roughly <= %v", elapsed, quitDeadline)
		}
	})
}

// TestAbortNoDotNoQuit is the RFC 5321 §3.8 abort proof: mid-DATA, Abort
// must hard-close without ever writing the terminator dot or QUIT.
func TestAbortNoDotNoQuit(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{})
	c, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: testTimeouts()})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	for _, line := range []string{"MAIL FROM:<a@b>\r\n", "RCPT TO:<c@d>\r\n", "DATA\r\n"} {
		if err := c.SendLine([]byte(line)); err != nil {
			t.Fatalf("SendLine(%q): %v", line, err)
		}
		if _, _, _, err := c.Replies().Next(); err != nil {
			t.Fatalf("Replies().Next() after %q: %v", line, err)
		}
	}

	c.SetCommandClass(DataBlock)
	if _, err := c.Writer().Write([]byte("partial body, no terminator\r\n")); err != nil {
		t.Fatalf("Writer().Write: %v", err)
	}

	c.Abort()

	sessions := srv.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("Sessions() len = %d, want 1", len(sessions))
	}
	for _, ev := range sessions[0].Transcript() {
		if string(ev.Line) == ".\r\n" {
			t.Errorf("transcript contains a terminator dot, want none: %+v", ev)
		}
		if ev.Verb == "QUIT" {
			t.Errorf("transcript contains QUIT, want none: %+v", ev)
		}
	}
}

// TestHandshakePhaseDeadline: the fake drips its greeting slowly enough
// (1s between bytes) that a naive per-read deadline shorter than that
// gap (here 200ms) would never see any byte arrive late within a single
// read and would have to be reset to make any forward progress at all —
// the whole-phase budget, armed once for the entire handshake and never
// reset, catches it instead, well before the drip could finish. (The
// gap between the 200ms budget and the elapsed-time assertion below is
// deliberately generous: fakesmtp's own drip-writer goroutine, still
// asleep mid-drip when Dial gives up, keeps this test's t.Cleanup —
// Server.Stop waiting for it to notice the closed socket — running for
// a couple more seconds after Dial itself has already returned; that
// cleanup tail is not part of what's being measured or asserted here.)
func TestHandshakePhaseDeadline(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{
		Banner: fakesmtp.Step{Reply: "220 slow-banner-text", Drip: time.Second},
	})

	to := testTimeouts()
	to.BackendHandshake = 200 * time.Millisecond

	start := time.Now()
	_, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: to})
	elapsed := time.Since(start)

	var herr *HandshakeError
	if !errors.As(err, &herr) {
		t.Fatalf("Dial err = %v (%T), want *HandshakeError", err, err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Dial took %v, want well under the >20s a full drip would take (whole-phase budget should catch it around 200ms)", elapsed)
	}
}

// TestDialCtxCancelMidConnect: the caller's context is cancelled while
// Dial is blocked waiting on a backend that accepted the TCP connection
// and then hung (never sent a banner). Dial must return promptly — not
// wait out BackendHandshake — and must not leak the watcher goroutine
// that makes that possible.
//
// fakesmtp's own DownAcceptThenHang handler is itself one goroutine that
// blocks until the fake server stops (by design — see fakesmtp.Server.
// acceptLoop), so the goroutine count after Dial legitimately sits one
// above the pre-Dial baseline for the rest of this test; the budget
// below is baseline+1, catching a leak in Dial's own watcher goroutine
// without failing on that expected, unrelated one.
func TestDialCtxCancelMidConnect(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{})
	srv.SetDown(fakesmtp.DownAcceptThenHang)

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	to := testTimeouts()
	to.BackendHandshake = 10 * time.Second // long enough that only ctx cancellation explains a prompt return

	start := time.Now()
	c, err := Dial(ctx, testServer(srv.Addr()), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: to})
	elapsed := time.Since(start)
	if c != nil {
		c.Abort()
	}
	if err == nil {
		t.Fatalf("Dial err = nil, want an error (context canceled mid-handshake)")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Dial took %v after cancel at ~50ms, want a prompt return", elapsed)
	}

	budget := before + 1 // +1 for fakesmtp's own hung-connection handler, not Dial's
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > budget && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > budget {
		t.Errorf("NumGoroutine() = %d, want <= %d once Dial's watcher goroutine has settled", got, budget)
	}
}

// TestConcurrentReadWriteClassNoRace mirrors the reviewer's -race probe
// for D4's owner+pump concurrency shape (PROJECT.md: one owner goroutine
// drives SendLine/Writer while a second reply-pump goroutine blocks in
// Replies(), watching for a backend verdict that can arrive early,
// mid-DATA — the "mid-DATA early backend replies" contract row). One
// goroutine (the owner) loops SetCommandClass(DataBlock)+Writer().Write()
// as it streams a body that never actually reaches its terminator in
// this test; concurrently, the other (the pump) does
// SetCommandClass(Dot)+Replies().Next(), a single long block against a
// backend that never replies mid-DATA at all.
//
// Must be race-clean under -race (go.mod's test-race gate runs this),
// and the two directions' deadlines must be observably independent: the
// write side keeps flowing (many successful writes across the whole
// run) while the read side's one blocking call times out on Dot's
// budget specifically — not on DataBlock's much shorter one, which a
// combined, unsynchronized single-class deadline (the pre-fix shape)
// would let the owner's concurrent arming stomp.
func TestConcurrentReadWriteClassNoRace(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{})

	const (
		writeClassBudget = 30 * time.Millisecond
		readClassBudget  = 600 * time.Millisecond
	)
	to := testTimeouts()
	to.DataProgress = writeClassBudget
	to.BackendFinalDot = readClassBudget

	c, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: to})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	for _, line := range []string{"MAIL FROM:<a@b>\r\n", "RCPT TO:<c@d>\r\n"} {
		if err := c.SendLine([]byte(line)); err != nil {
			t.Fatalf("SendLine(%q): %v", line, err)
		}
		if _, _, _, err := c.Replies().Next(); err != nil {
			t.Fatalf("Replies().Next() after %q: %v", line, err)
		}
	}
	c.SetCommandClass(DataInit)
	if err := c.SendLine([]byte("DATA\r\n")); err != nil {
		t.Fatalf("SendLine(DATA): %v", err)
	}
	if _, _, _, err := c.Replies().Next(); err != nil {
		t.Fatalf("Replies().Next() after DATA: %v", err)
	}

	var writeCount atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // owner: keeps streaming a body that never reaches its dot
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.SetCommandClass(DataBlock)
			if _, err := c.Writer().Write([]byte("x\r\n")); err != nil {
				return
			}
			writeCount.Add(1)
		}
	}()

	c.SetCommandClass(Dot)
	start := time.Now()
	_, _, _, readErr := c.Replies().Next() // the pump's one long block
	elapsed := time.Since(start)
	close(stop)
	wg.Wait()

	if readErr == nil {
		t.Fatalf("Replies().Next() err = nil, want a deadline error (fake never replies mid-DATA)")
	}
	if elapsed < readClassBudget/2 || elapsed > readClassBudget*3 {
		t.Errorf("read side elapsed = %v, want close to Dot's budget (%v) - not DataBlock's much shorter one (%v), and not stomped short by the concurrent write-side arming", elapsed, readClassBudget, writeClassBudget)
	}
	if got := writeCount.Load(); got < 20 {
		t.Errorf("write side completed %d writes while the read side waited, want several dozen+ (the write side must keep flowing, unaffected by the concurrent read-side deadline)", got)
	}
}

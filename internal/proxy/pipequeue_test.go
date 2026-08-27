package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPipeQueueFIFO(t *testing.T) {
	var q PipeQueue
	in := []string{
		"MAIL FROM:<a@b> SIZE=100\r\n",
		"RCPT TO:<c@d>\r\n",
		"rcpt to:<E@f> NOTIFY=SUCCESS\r\n",
		"DATA\r\n",
	}
	for _, line := range in {
		if err := q.Push([]byte(line)); err != nil {
			t.Fatalf("Push(%q): %v", line, err)
		}
	}

	got := q.Drain()
	if len(got) != len(in) {
		t.Fatalf("Drain returned %d lines, want %d", len(got), len(in))
	}
	for i, line := range in {
		if string(got[i]) != line {
			t.Errorf("line %d = %q, want verbatim %q", i, got[i], line)
		}
	}
	if left := q.Drain(); len(left) != 0 {
		t.Errorf("second Drain returned %d lines, want 0", len(left))
	}

	// Drained and empty, the queue takes new lines again.
	if err := q.Push([]byte("NOOP\r\n")); err != nil {
		t.Fatalf("Push after Drain: %v", err)
	}
	if got := q.Drain(); len(got) != 1 || string(got[0]) != "NOOP\r\n" {
		t.Errorf("after refill Drain = %q, want [\"NOOP\\r\\n\"]", got)
	}
}

func TestPipeQueueOverflowLines(t *testing.T) {
	var q PipeQueue
	for i := range 32 {
		if err := q.Push([]byte(fmt.Sprintf("RCPT TO:<u%d@x>\r\n", i))); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	if err := q.Push([]byte("RCPT TO:<one-too-many@x>\r\n")); !errors.Is(err, ErrPipelineOverflow) {
		t.Fatalf("33rd Push err = %v, want ErrPipelineOverflow", err)
	}
	// The refused line is not queued.
	if got := q.Drain(); len(got) != 32 {
		t.Errorf("Drain returned %d lines, want 32", len(got))
	}
}

func TestPipeQueueOverflowBytes(t *testing.T) {
	var q PipeQueue
	// 8 lines of 2 KB fills the 16 KB budget exactly.
	line := []byte("X" + strings.Repeat("y", 2045) + "\r\n")
	if len(line) != 2048 {
		t.Fatalf("test line is %d bytes, want 2048", len(line))
	}
	for i := range 8 {
		if err := q.Push(line); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	if err := q.Push([]byte("X\r\n")); !errors.Is(err, ErrPipelineOverflow) {
		t.Fatalf("Push past 16 KB err = %v, want ErrPipelineOverflow", err)
	}
}

// drainingHandler is the stub the batch tests share: it records what the
// session queued and answers one 451 per queued command, in order.
func drainingHandler() *stubHandler {
	h := &stubHandler{}
	h.extra = func(tx *Txn) {
		lines := tx.PipelineQ.Drain()
		h.mu.Lock()
		h.drained = lines
		h.mu.Unlock()
		for range lines {
			_, _ = tx.W.WriteString(RplNoBackend)
		}
		_ = tx.W.Flush()
	}
	return h
}

// wantQueued asserts the session queued exactly these raw lines.
func wantQueued(t *testing.T, h *stubHandler, want ...string) {
	t.Helper()
	got := h.drainedLines()
	if len(got) != len(want) {
		t.Fatalf("queue held %d lines (%q), want %d (%q)", len(got), got, len(want), want)
	}
	for i, line := range want {
		if !bytes.Equal(got[i], []byte(line)) {
			t.Errorf("queued line %d = %q, want verbatim %q", i, got[i], line)
		}
	}
}

func TestSessionPipelinedBatchQueued(t *testing.T) {
	const noBackend = "451 4.4.1 No backend available, try again later"

	t.Run("mail rcpt rcpt data is one batch", func(t *testing.T) {
		h := drainingHandler()
		c := newTestClient(t, testConfig(), nil, h)
		c.expect("220 bifrost.test ESMTP")
		c.send("EHLO client.example")
		c.reply()

		// The whole batch in one write, before any reply comes back.
		c.raw("MAIL FROM:<a@b>\r\nRCPT TO:<c@d>\r\nRCPT TO:<e@f>\r\nDATA\r\n")
		for range 4 {
			c.expect(noBackend)
		}
		wantQueued(t, h, "MAIL FROM:<a@b>\r\n", "RCPT TO:<c@d>\r\n", "RCPT TO:<e@f>\r\n", "DATA\r\n")
	})

	t.Run("batch stops at a sync point", func(t *testing.T) {
		// RFC 2920: NOOP is not part of the MAIL/RCPT/DATA group, so it
		// ends the batch and the session answers it itself — after the
		// transaction's replies, still in command order.
		h := drainingHandler()
		c := newTestClient(t, testConfig(), nil, h)
		c.expect("220 bifrost.test ESMTP")
		c.send("EHLO client.example")
		c.reply()

		c.raw("MAIL FROM:<a@b>\r\nRCPT TO:<c@d>\r\nNOOP\r\nRCPT TO:<e@f>\r\n")
		c.expect(noBackend)
		c.expect(noBackend)
		c.expect("250 2.0.0 OK")
		c.expect("503 5.5.1 Bad sequence of commands") // stray RCPT, no transaction
		wantQueued(t, h, "MAIL FROM:<a@b>\r\n", "RCPT TO:<c@d>\r\n")
	})
}

func TestBatchStopsAtDataBody(t *testing.T) {
	h := drainingHandler()
	c := newTestClient(t, testConfig(), nil, h)
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	// A client that pipelines the body too: DATA ends the batch, so the
	// body bytes are never queued as commands (they belong to the
	// handler's DATA pipe), and neither is the QUIT behind them.
	c.raw("MAIL FROM:<a@b>\r\nRCPT TO:<c@d>\r\nDATA\r\nSubject: x\r\n\r\nbody line\r\n.\r\nQUIT\r\n")

	for range 3 {
		c.expect("451 4.4.1 No backend available, try again later")
	}
	wantQueued(t, h, "MAIL FROM:<a@b>\r\n", "RCPT TO:<c@d>\r\n", "DATA\r\n")

	// The stub never consumed the body, so the main loop sees those
	// lines as commands: unknown, unknown, empty, unknown, then the
	// dot, and finally the real QUIT.
	c.expect("500 5.5.1 Command not recognized") // Subject: x
	c.expect("500 5.5.1 Command not recognized") // (empty line)
	c.expect("500 5.5.1 Command not recognized") // body line
	c.expect("500 5.5.1 Command not recognized") // .
	c.expect("221 2.0.0 Bye")
	c.expectClosed()
}

func TestSessionPipelineOverflow421(t *testing.T) {
	h := &stubHandler{}
	c := newTestClient(t, testConfig(), nil, h)
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	// One MAIL plus 40 RCPTs: past the 32-line queue bound, and small
	// enough to arrive as a single buffered read.
	var b strings.Builder
	b.WriteString("MAIL FROM:<a@b>\r\n")
	for i := range 40 {
		fmt.Fprintf(&b, "RCPT TO:<u%d@x>\r\n", i)
	}
	c.raw(b.String())

	c.expect("421 4.7.0 Too many pipelined commands, closing connection")
	c.expectClosed()
	if got := h.seen(); len(got) != 0 {
		t.Errorf("handler saw %d transactions, want 0 (nothing may be relayed)", len(got))
	}
}

// TestSessionPipelinedBatchOrderWithBareLf covers the interaction of the
// two features: a violation inside a pipelined batch is left in the
// buffer rather than queued, so the transaction's replies still come
// first and the 500 lands in command order (RFC 2920) with the session
// still in sync.
func TestSessionPipelinedBatchOrderWithBareLf(t *testing.T) {
	h := &stubHandler{}
	h.extra = func(tx *Txn) {
		lines := tx.PipelineQ.Drain()
		h.mu.Lock()
		h.drained = lines
		h.mu.Unlock()
		for range lines {
			_, _ = tx.W.WriteString(RplNoBackend)
		}
		_ = tx.W.Flush()
	}

	c := newTestClient(t, testConfig(), nil, h)
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	// MAIL + RCPT, then a bare-LF RCPT, then a clean NOOP.
	c.raw("MAIL FROM:<a@b>\r\nRCPT TO:<c@d>\r\nRCPT TO:<e@f>\nNOOP\r\n")

	c.expect("451 4.4.1 No backend available, try again later")
	c.expect("451 4.4.1 No backend available, try again later")
	c.expect("500 5.5.2 Bare LF is not a valid line terminator")
	c.expect("250 2.0.0 OK")

	if got := h.drainedLines(); len(got) != 2 {
		t.Fatalf("queued %d lines (%q), want 2: the bare-LF line must stay behind", len(got), got)
	}
}

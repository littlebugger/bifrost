package fakesmtp

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestStepSequenceConsumedLastRepeats(t *testing.T) {
	srv := Start(t, Script{OnRCPT: []Step{{Reply: "450 4.1.1 try again"}, {Reply: "250 2.1.5 OK"}}})
	conn, r := dialRaw(t, srv.Addr())

	readReplyLines(t, r) // banner
	_, _ = conn.Write([]byte("EHLO client.example\r\n"))
	readReplyLines(t, r)
	_, _ = conn.Write([]byte("MAIL FROM:<a@b>\r\n"))
	readReplyLines(t, r)

	wantSeq := []string{"450", "250", "250", "250"}
	for i, want := range wantSeq {
		if _, err := conn.Write([]byte("RCPT TO:<c@d>\r\n")); err != nil {
			t.Fatalf("write RCPT #%d: %v", i, err)
		}
		got := readReplyLines(t, r)
		if len(got) != 1 || !strings.HasPrefix(got[0], want) {
			t.Fatalf("RCPT #%d reply = %v, want prefix %s", i, got, want)
		}
	}
}

func TestOnEHLOScripted(t *testing.T) {
	t.Run("scripted rejection", func(t *testing.T) {
		srv := Start(t, Script{
			Caps:   []string{"PIPELINING"},
			OnEHLO: []Step{{Reply: "502 5.5.1 Command not implemented"}},
		})
		conn, r := dialRaw(t, srv.Addr())
		readReplyLines(t, r) // banner
		_, _ = conn.Write([]byte("EHLO client.example\r\n"))
		got := readReplyLines(t, r)
		if len(got) != 1 || got[0] != "502 5.5.1 Command not implemented" {
			t.Fatalf("EHLO reply = %v, want scripted 502 only", got)
		}
	})

	t.Run("empty OnEHLO uses default caps reply", func(t *testing.T) {
		srv := Start(t, Script{Caps: []string{"PIPELINING", "8BITMIME"}})
		conn, r := dialRaw(t, srv.Addr())
		readReplyLines(t, r) // banner
		_, _ = conn.Write([]byte("EHLO client.example\r\n"))
		got := readReplyLines(t, r)
		joined := strings.Join(got, "\n")
		if !strings.Contains(joined, "PIPELINING") || !strings.Contains(joined, "8BITMIME") {
			t.Fatalf("EHLO reply = %v, want default caps reply", got)
		}
	})
}

func TestStepDelay(t *testing.T) {
	delay := 150 * time.Millisecond
	srv := Start(t, Script{OnMAIL: []Step{{Delay: delay}}})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner
	_, _ = conn.Write([]byte("EHLO client.example\r\n"))
	readReplyLines(t, r)

	start := time.Now()
	if _, err := conn.Write([]byte("MAIL FROM:<a@b>\r\n")); err != nil {
		t.Fatalf("write MAIL: %v", err)
	}
	readReplyLines(t, r)
	elapsed := time.Since(start)
	if elapsed < delay {
		t.Fatalf("reply arrived after %v, want at least the scripted delay %v", elapsed, delay)
	}
}

func TestStepDrip(t *testing.T) {
	drip := 3 * time.Millisecond
	reply := "250 dripped reply"
	srv := Start(t, Script{OnMAIL: []Step{{Reply: reply, Drip: drip}}})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner
	_, _ = conn.Write([]byte("EHLO client.example\r\n"))
	readReplyLines(t, r)

	start := time.Now()
	if _, err := conn.Write([]byte("MAIL FROM:<a@b>\r\n")); err != nil {
		t.Fatalf("write MAIL: %v", err)
	}
	got := readReplyLines(t, r)
	elapsed := time.Since(start)

	if len(got) != 1 || got[0] != reply {
		t.Fatalf("MAIL reply = %v, want %q", got, reply)
	}
	nBytes := len(reply) + 2 // + CRLF
	wantMin := time.Duration(nBytes-1) * drip
	if elapsed < wantMin {
		t.Fatalf("dripped reply arrived after %v, want at least %v (paced at %v/byte)", elapsed, wantMin, drip)
	}
}

func TestActDropConn(t *testing.T) {
	srv := Start(t, Script{OnMAIL: []Step{{Action: ActDropConn}}})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner
	_, _ = conn.Write([]byte("EHLO client.example\r\n"))
	readReplyLines(t, r)

	if _, err := conn.Write([]byte("MAIL FROM:<a@b>\r\n")); err != nil {
		t.Fatalf("write MAIL: %v", err)
	}
	_, err := r.ReadByte()
	if err == nil {
		t.Fatalf("expected clean EOF after dropped MAIL, got no error")
	}
	if !isCleanEOF(err) {
		t.Fatalf("expected clean EOF, got %v", err)
	}
}

func TestActRST(t *testing.T) {
	srv := Start(t, Script{OnMAIL: []Step{{Action: ActRST}}})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner
	_, _ = conn.Write([]byte("EHLO client.example\r\n"))
	readReplyLines(t, r)

	if _, err := conn.Write([]byte("MAIL FROM:<a@b>\r\n")); err != nil {
		t.Fatalf("write MAIL: %v", err)
	}
	_, err := r.ReadByte()
	if err == nil {
		t.Fatalf("expected connection reset after RST-dropped MAIL, got no error")
	}
	if isCleanEOF(err) {
		t.Fatalf("expected a reset, got clean EOF")
	}
}

func TestActHang(t *testing.T) {
	srv := Start(t, Script{OnMAIL: []Step{{Action: ActHang}}})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner
	_, _ = conn.Write([]byte("EHLO client.example\r\n"))
	readReplyLines(t, r)

	if _, err := conn.Write([]byte("MAIL FROM:<a@b>\r\n")); err != nil {
		t.Fatalf("write MAIL: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	_, err := r.ReadByte()
	if !isTimeout(err) {
		t.Fatalf("expected read timeout while hung, got %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})

	srv.Stop()

	if _, err := r.ReadByte(); err == nil {
		t.Fatalf("expected connection to end once server stopped, got no error")
	}
}

func isCleanEOF(err error) bool {
	return err != nil && strings.Contains(err.Error(), "EOF")
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// TestMidBodyFiresAfterNLines covers the MidBody hook the relay's
// mid-DATA contract tests depend on: it must fire after exactly
// MidBodyLines body lines, not on the first one, and a step that only
// replies must leave the fake reading the rest of the body.
func TestMidBodyFiresAfterNLines(t *testing.T) {
	srv := Start(t, Script{
		MidBody:      []Step{{Reply: "552 5.3.4 too big"}},
		MidBodyLines: 2,
	})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner

	send := func(s string) {
		t.Helper()
		if _, err := conn.Write([]byte(s)); err != nil {
			t.Fatalf("write %q: %v", s, err)
		}
	}
	for _, c := range []string{"EHLO client.example", "MAIL FROM:<a@b>", "RCPT TO:<c@d>", "DATA"} {
		send(c + "\r\n")
		readReplyLines(t, r)
	}

	// One body line is not enough: nothing may come back yet.
	send("one\r\n")
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := r.ReadString('\n'); !isTimeout(err) {
		t.Fatalf("reply after one body line: err = %v, want a timeout", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}

	send("two\r\n")
	if got, want := readReplyLines(t, r), "552 5.3.4 too big"; len(got) != 1 || got[0] != want {
		t.Fatalf("mid-body reply = %v, want [%q]", got, want)
	}

	// The body kept flowing: terminator accepted, whole body recorded.
	send("three\r\n.\r\n")
	if got := readReplyLines(t, r); len(got) != 1 || got[0][0] != '2' {
		t.Fatalf("end-of-data reply = %v, want 2xx", got)
	}
	srv.AssertWireBody(t, 0, []byte("one\r\ntwo\r\nthree\r\n"))
}

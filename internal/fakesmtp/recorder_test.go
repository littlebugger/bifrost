package fakesmtp

import (
	"bytes"
	"strings"
	"testing"
)

func TestRecorderTranscript(t *testing.T) {
	srv := Start(t, Script{})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner

	cmds := []string{"EHLO client.example", "MAIL FROM:<a@b>", "RCPT TO:<c@d>", "QUIT"}
	for _, c := range cmds {
		if _, err := conn.Write([]byte(c + "\r\n")); err != nil {
			t.Fatalf("write %q: %v", c, err)
		}
		readReplyLines(t, r)
	}

	sessions := srv.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("Sessions() len = %d, want 1", len(sessions))
	}
	events := sessions[0].Transcript()
	if len(events) != len(cmds) {
		t.Fatalf("Transcript() len = %d, want %d: %+v", len(events), len(cmds), events)
	}
	wantVerbs := []string{"EHLO", "MAIL", "RCPT", "QUIT"}
	for i, ev := range events {
		if ev.Verb != wantVerbs[i] {
			t.Errorf("event %d verb = %q, want %q", i, ev.Verb, wantVerbs[i])
		}
		if string(bytes.TrimRight(ev.Line, "\r\n")) != cmds[i] {
			t.Errorf("event %d line = %q, want %q", i, ev.Line, cmds[i])
		}
	}
}

func TestRecorderWireBodyDotStuffed(t *testing.T) {
	srv := Start(t, Script{})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner

	send := func(s string) {
		t.Helper()
		if _, err := conn.Write([]byte(s)); err != nil {
			t.Fatalf("write %q: %v", s, err)
		}
	}
	send("EHLO client.example\r\n")
	readReplyLines(t, r)
	send("MAIL FROM:<a@b>\r\n")
	readReplyLines(t, r)
	send("RCPT TO:<c@d>\r\n")
	readReplyLines(t, r)
	send("DATA\r\n")
	readReplyLines(t, r)

	// Tricky dot-stuffing cases, sent as raw bytes (smtpdrv-style), the
	// stuffed wire form exactly as a real client would produce it:
	//   - a line that started with "." stuffed to ".."
	//   - a lone CR mid-line (not a line terminator: only LF ends a line)
	//   - a line that was originally bare "." stuffed to ".."
	body := "" +
		"first line\r\n" +
		"..leading-dot\r\n" +
		".\rmid-line\r\n" +
		"..\r\n" +
		"last line\r\n"
	send(body)
	send(".\r\n") // terminator, excluded from WireBody
	got := readReplyLines(t, r)
	if len(got) != 1 || got[0][0] != '2' {
		t.Fatalf("EOD reply = %v, want 2xx", got)
	}

	srv.AssertWireBody(t, 0, []byte(body))
}

// TestRecorderRequiresStrictCRLFTerminator is a regression case: a
// bare-LF "." line mid-body must NOT be mistaken for the end-of-data
// terminator, which is the exact 3 bytes ".\r\n" and nothing looser. A
// probe against the pre-fix code sent "before\r\n.\nafter\r\n.\r\n" and
// found the terminator match on the bare-LF line, truncating WireBody to
// "before\r\n" and leaving "after\r\n.\r\n" to be parsed as commands —
// desyncing the session.
func TestRecorderRequiresStrictCRLFTerminator(t *testing.T) {
	srv := Start(t, Script{})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner

	for _, c := range []string{"EHLO client.example", "MAIL FROM:<a@b>", "RCPT TO:<c@d>", "DATA"} {
		if _, err := conn.Write([]byte(c + "\r\n")); err != nil {
			t.Fatalf("write %q: %v", c, err)
		}
		readReplyLines(t, r)
	}

	body := "before\r\n.\nafter\r\n"
	if _, err := conn.Write([]byte(body + ".\r\n")); err != nil {
		t.Fatalf("write body: %v", err)
	}
	eod := readReplyLines(t, r)
	if len(eod) != 1 || eod[0][0] != '2' {
		t.Fatalf("EOD reply = %v, want 2xx", eod)
	}

	srv.AssertWireBody(t, 0, []byte(body))

	// The session must still be in sync: QUIT is read as a command, not
	// misparsed as leftover body content.
	if _, err := conn.Write([]byte("QUIT\r\n")); err != nil {
		t.Fatalf("write QUIT: %v", err)
	}
	quit := readReplyLines(t, r)
	if len(quit) != 1 || !strings.HasPrefix(quit[0], "221") {
		t.Fatalf("QUIT reply = %v, want 221 (session desynced by the bare-LF line)", quit)
	}
}

func TestDialAndCmdCounters(t *testing.T) {
	srv := Start(t, Script{})

	for i := 0; i < 3; i++ {
		conn, r := dialRaw(t, srv.Addr())
		readReplyLines(t, r) // banner
		if _, err := conn.Write([]byte("EHLO client.example\r\n")); err != nil {
			t.Fatalf("write EHLO: %v", err)
		}
		readReplyLines(t, r)
		if _, err := conn.Write([]byte("QUIT\r\n")); err != nil {
			t.Fatalf("write QUIT: %v", err)
		}
		readReplyLines(t, r)
	}

	if got := srv.DialCount(); got != 3 {
		t.Errorf("DialCount() = %d, want 3", got)
	}
	if got := srv.CmdCount("EHLO"); got != 3 {
		t.Errorf("CmdCount(EHLO) = %d, want 3", got)
	}
	if got := srv.CmdCount("QUIT"); got != 3 {
		t.Errorf("CmdCount(QUIT) = %d, want 3", got)
	}
	if got := srv.CmdCount("RCPT"); got != 0 {
		t.Errorf("CmdCount(RCPT) = %d, want 0", got)
	}
}

func TestAssertWireBodyHelper(t *testing.T) {
	srv := Start(t, Script{})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner

	for _, c := range []string{"EHLO client.example", "MAIL FROM:<a@b>", "RCPT TO:<c@d>", "DATA"} {
		if _, err := conn.Write([]byte(c + "\r\n")); err != nil {
			t.Fatalf("write %q: %v", c, err)
		}
		readReplyLines(t, r)
	}
	if _, err := conn.Write([]byte("hello world\r\n.\r\n")); err != nil {
		t.Fatalf("write body: %v", err)
	}
	readReplyLines(t, r)

	srv.AssertWireBody(t, 0, []byte("hello world\r\n"))
}

func TestRecorderDiscardBody(t *testing.T) {
	srv := Start(t, Script{DiscardBody: true})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner

	for _, c := range []string{"EHLO client.example", "MAIL FROM:<a@b>", "RCPT TO:<c@d>", "DATA"} {
		if _, err := conn.Write([]byte(c + "\r\n")); err != nil {
			t.Fatalf("write %q: %v", c, err)
		}
		readReplyLines(t, r)
	}
	if _, err := conn.Write([]byte("line one\r\nline two\r\nline three\r\n.\r\n")); err != nil {
		t.Fatalf("write body: %v", err)
	}
	readReplyLines(t, r)

	sess := srv.Sessions()[0]
	msgs := sess.Messages()
	if len(msgs) != 1 {
		t.Fatalf("Messages() len = %d, want 1", len(msgs))
	}
	if len(msgs[0].WireBody) != 0 {
		t.Errorf("WireBody = %q, want empty (DiscardBody set)", msgs[0].WireBody)
	}

	// DiscardBody must bound transcript memory too, not just WireBody:
	// zero body-line events (Verb == "") should have been recorded.
	for _, ev := range sess.Transcript() {
		if ev.Verb == "" {
			t.Errorf("Transcript() contains a body-line event %+v, want none when DiscardBody is set", ev)
		}
	}
}

// TestBodyBytesCountsUnderDiscardBody pins the counter the 1 GiB
// streaming test relies on: DiscardBody drops the content (that is the
// point — the body does not fit in memory) but the byte count still has
// to prove the whole message arrived.
func TestBodyBytesCountsUnderDiscardBody(t *testing.T) {
	srv := Start(t, Script{DiscardBody: true})
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

	body := "first line\r\n..stuffed\r\n\r\nlast line\r\n"
	send(body)
	send(".\r\n") // terminator, counted in neither
	if got := readReplyLines(t, r); len(got) != 1 || got[0][0] != '2' {
		t.Fatalf("end-of-data reply = %v, want 2xx", got)
	}

	sessions := srv.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if got, want := sessions[0].BodyBytes(), int64(len(body)); got != want {
		t.Errorf("BodyBytes() = %d, want %d", got, want)
	}
	// And the content really was discarded, transcript included.
	msgs := sessions[0].Messages()
	if len(msgs) != 1 || len(msgs[0].WireBody) != 0 {
		t.Errorf("recorded messages = %+v, want one with an empty wire body", msgs)
	}
	for _, ev := range sessions[0].Transcript() {
		if ev.Verb == "" {
			t.Errorf("transcript kept a body line under DiscardBody: %q", ev.Line)
		}
	}
}

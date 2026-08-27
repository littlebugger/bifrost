//go:build integration

package proxy

import (
	"strings"
	"testing"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// DATA is where R4 is won or lost: the 354 is never synthesized, the body
// is a raw pipe (dot-stuffing is terminator-preserving, so unstuffing and
// restuffing would be the identity), and the end-of-data verdict is the
// message's true fate.

func TestNever354Synthesized(t *testing.T) {
	// The predecessors' shared bug: answering 354 yourself and finding out
	// later that the backend would have refused DATA.
	srv := relayFake(t, fakesmtp.Script{OnDATA: []fakesmtp.Step{{Reply: "451 4.3.0 try later"}}})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")
	f.send("DATA")
	f.expect("451 4.3.0 try later")

	// No 354 means no body followed, so the session is still taking
	// commands: the transaction is refused, not the connection.
	f.send("RSET")
	f.expect("250 2.0.0 OK")
	f.send("MAIL FROM:<a2@b.example>")
	f.expect("250 2.1.0 OK")
}

func Test354AndVerdictVerbatim(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{
		OnDATA: []fakesmtp.Step{{Reply: "354 go ahead, I am listening"}},
		OnEOD:  []fakesmtp.Step{{Reply: "452 4.2.2 mailbox full"}},
	})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")
	f.send("DATA")
	f.expect("354 go ahead, I am listening")
	f.raw("Subject: hi\r\n\r\nbody\r\n.\r\n")
	f.expect("452 4.2.2 mailbox full")

	srv.AssertWireBody(t, 0, []byte("Subject: hi\r\n\r\nbody\r\n"))
}

func TestDotStuffingIntegrity(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")
	f.send("DATA")
	f.expect("354 Start mail input; end with <CRLF>.<CRLF>")

	// Hostile shapes: a client-stuffed leading dot, dots mid-line, a bare
	// CR inside a line, a line that is a single stuffed dot — and the
	// terminator itself split across three writes, so the framer has to
	// carry its state across reads.
	pieces := []string{
		"Subject: dots\r\n",
		"..stuffed leading dot\r\n",
		"mid.dle . dots\r\n",
		"bare\rCR inside a line\r\n",
		"..\r\n",
		"\r\n.",
		"\r",
		"\n",
	}
	for _, p := range pieces {
		f.raw(p)
	}
	f.expect("250 2.0.0 OK: queued")

	want := strings.Join(pieces[:5], "") + "\r\n"
	srv.AssertWireBody(t, 0, []byte(want))
}

func TestReadAheadDrainedAtData(t *testing.T) {
	// A client that pipelines the body straight behind DATA (RFC 2920 says
	// wait for the 354, but bulk senders do this): the read-ahead already
	// sitting in the session's buffer must reach the backend after the
	// relayed 354, in order, with nothing lost.
	srv := relayFake(t, fakesmtp.Script{})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.raw("MAIL FROM:<a@b.example>\r\nRCPT TO:<c@d.example>\r\nDATA\r\n" +
		"Subject: ahead\r\n\r\nread-ahead body\r\n.\r\n")

	f.expect("250 2.1.0 OK")
	f.expect("250 2.1.5 OK")
	f.expect("354 Start mail input; end with <CRLF>.<CRLF>")
	f.expect("250 2.0.0 OK: queued")

	srv.AssertWireBody(t, 0, []byte("Subject: ahead\r\n\r\nread-ahead body\r\n"))
}

func TestDetachAfterVerdict(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	sendMsg := func(i string) {
		f.send("MAIL FROM:<" + i + "@b.example>")
		f.expect("250 2.1.0 OK")
		f.send("RCPT TO:<c@d.example>")
		f.expect("250 2.1.5 OK")
		f.send("DATA")
		f.expect("354 Start mail input; end with <CRLF>.<CRLF>")
		f.raw("body " + i + "\r\n.\r\n")
		f.expect("250 2.0.0 OK: queued")
		// The verdict reaches the client before the leg is torn down —
		// that ordering is the point of R4. One round-trip with the
		// session proves the detach has run.
		f.send("NOOP")
		f.expect("250 2.0.0 OK")
	}

	sendMsg("one")
	if got := srv.CmdCount("QUIT"); got != 1 {
		t.Errorf("backend QUIT count = %d, want 1: the leg is detached politely after the verdict", got)
	}
	if got := srv.DialCount(); got != 1 {
		t.Errorf("DialCount after one message = %d, want 1", got)
	}

	// Per-transaction attachment (R3): the next message dials again.
	sendMsg("two")
	if got := srv.DialCount(); got != 2 {
		t.Errorf("DialCount after two messages = %d, want 2", got)
	}
	if open, total := f.leases.state(); open != 0 || total != 2 {
		t.Errorf("leases = (open %d, total %d), want (0, 2)", open, total)
	}
	srv.AssertWireBody(t, 0, []byte("body one\r\n"))
	srv.AssertWireBody(t, 1, []byte("body two\r\n"))
}

//go:build integration

package proxy

import (
	"testing"

	"github.com/revolee/bifrost/internal/fakesmtp"
)

// Two-class latching (decision D12): transaction-fatal errors latch until
// RSET, the next MAIL, or EHLO; per-recipient verdicts never latch at all.
// Mireka latches everything, so one 550 poisons every later RCPT of the
// same message — the bug the first test here exists to prevent.

func TestRcpt550ThenRcpt250ReachesBackend(t *testing.T) {
	// rcpt-550-then-rcpt-250-reaches-backend
	srv := relayFake(t, fakesmtp.Script{
		OnRCPT: []fakesmtp.Step{{Reply: "550 5.1.1 no such user"}, {Reply: "250 2.1.5 second ok"}},
	})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<nobody@d.example>")
	f.expect("550 5.1.1 no such user")

	// A rejected recipient is the backend's verdict on that recipient, not
	// a verdict on the transaction: the next RCPT must reach the backend.
	f.send("RCPT TO:<somebody@d.example>")
	f.expect("250 2.1.5 second ok")
	f.send("DATA")
	f.expect("354 Start mail input; end with <CRLF>.<CRLF>")
	f.raw("body\r\n.\r\n")
	f.expect("250 2.0.0 OK: queued")
	f.send("NOOP") // round-trip: the detach has run by the time this replies
	f.expect("250 2.0.0 OK")

	wantLines(t, srv, 0,
		"MAIL FROM:<a@b.example>\r\n",
		"RCPT TO:<nobody@d.example>\r\n",
		"RCPT TO:<somebody@d.example>\r\n",
		"DATA\r\n",
		"body\r\n",
		"QUIT\r\n",
	)
}

func TestFatalLatchAnswersRestOfTxn(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{OnRCPT: []fakesmtp.Step{{Action: fakesmtp.ActRST}}})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("451 4.4.1 Backend connection lost")

	// The rest of the transaction is answered from the latch, with no
	// backend to answer it and no attempt to find one.
	f.send("RCPT TO:<e@d.example>")
	f.expect("451 4.4.1 Backend connection lost")
	f.send("DATA")
	f.expect("451 4.4.1 Backend connection lost")
	if got := srv.DialCount(); got != 1 {
		t.Errorf("DialCount = %d, want 1: a latched transaction does not re-dial", got)
	}
}

func TestLatchClearsOnRset(t *testing.T) {
	// latch-clears-on-rset
	srv := relayFake(t, fakesmtp.Script{OnRCPT: []fakesmtp.Step{{Action: fakesmtp.ActRST}}})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("451 4.4.1 Backend connection lost")

	// Nothing is attached, so RSET is the synthesized 250 — and it clears
	// the latch.
	f.send("RSET")
	f.expect("250 2.0.0 OK")
	f.send("MAIL FROM:<a2@b.example>")
	f.expect("250 2.1.0 OK")
	if got := srv.DialCount(); got != 2 {
		t.Errorf("DialCount = %d, want 2: the post-RSET MAIL attaches afresh", got)
	}
}

func TestLatchClearsOnNextMail(t *testing.T) {
	// latch-clears-on-next-mail
	srv := relayFake(t, fakesmtp.Script{OnRCPT: []fakesmtp.Step{{Action: fakesmtp.ActRST}}})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("451 4.4.1 Backend connection lost")

	// The next connection gets a backend that behaves (a per-verb step
	// queue has its own cursor per session, so the reset would otherwise
	// repeat on the fresh leg too).
	srv.SetScript(fakesmtp.Script{Caps: backendCaps()})

	// No RSET: MAIL itself is a fresh start, so it re-picks and re-dials.
	f.send("MAIL FROM:<a2@b.example>")
	f.expect("250 2.1.0 OK")
	if got := srv.DialCount(); got != 2 {
		t.Errorf("DialCount = %d, want 2: MAIL clears the latch and picks again", got)
	}

	// And the fresh transaction is a whole transaction, latch-free.
	f.send("RCPT TO:<c2@d.example>")
	f.expect("250 2.1.5 OK")
}

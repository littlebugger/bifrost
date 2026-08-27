//go:build integration

package proxy

import (
	"slices"
	"testing"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// The commands a client can send while a backend is attached, and the
// contract's state table for each: RSET is relayed and detaches, a second
// MAIL is relayed and the backend's verdict drives the state, NOOP never
// touches a backend, and EHLO/QUIT belong to the session.

func TestRsetInTxnRelayedThenDetach(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{OnRSET: []fakesmtp.Step{{Reply: "250 2.0.0 flushed"}}})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")

	// Relayed, not synthesized: the backend's own 250 comes back.
	f.send("RSET")
	f.expect("250 2.0.0 flushed")

	f.send("NOOP") // round-trip: the detach has run by the time this replies
	f.expect("250 2.0.0 OK")
	if got := srv.CmdCount("QUIT"); got != 1 {
		t.Errorf("backend QUIT count = %d, want 1: RSET detaches politely", got)
	}

	// Fresh pick at the next MAIL.
	f.send("MAIL FROM:<a2@b.example>")
	f.expect("250 2.1.0 OK")
	if got := srv.DialCount(); got != 2 {
		t.Errorf("DialCount = %d, want 2: the next MAIL dials again", got)
	}
	wantLines(t, srv, 0,
		"MAIL FROM:<a@b.example>\r\n",
		"RCPT TO:<c@d.example>\r\n",
		"RSET\r\n",
		"QUIT\r\n",
	)
}

func TestSecondMailRelayed(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{
		OnMAIL: []fakesmtp.Step{{}, {Reply: "503 5.5.1 nested MAIL command"}},
	})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("MAIL FROM:<second@b.example>")
	f.expect("503 5.5.1 nested MAIL command")

	// State follows the backend: it refused the second MAIL, so the
	// original transaction is still open on the same leg.
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")
	if got := srv.DialCount(); got != 1 {
		t.Errorf("DialCount = %d, want 1: a relayed second MAIL does not re-pick", got)
	}
	wantLines(t, srv, 0,
		"MAIL FROM:<a@b.example>\r\n",
		"MAIL FROM:<second@b.example>\r\n",
		"RCPT TO:<c@d.example>\r\n",
	)
}

func TestNoopMidTxnNotForwarded(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("NOOP")
	f.expect("250 2.0.0 OK")

	// Still in the transaction, still on the same leg.
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")

	if got := srv.CmdCount("NOOP"); got != 0 {
		t.Errorf("backend NOOP count = %d, want 0: NOOP never touches a backend", got)
	}
	// The other synthesized rows of the contract, mid-transaction.
	f.send("VRFY postmaster")
	f.expect("252 2.5.2 Cannot VRFY user, but will accept message and attempt delivery")
	f.send("HELP")
	f.expect("214 2.0.0 See RFC 5321 for the supported commands")
	f.send("AUTH PLAIN AGFAYgBjCg==")
	f.expect("502 5.5.1 Command not implemented")
	f.send("STARTTLS")
	f.expect("503 5.5.1 Bad sequence of commands")
	f.send("FROBNICATE")
	f.expect("500 5.5.1 Command not recognized")
	f.send("RCPT TO:<e@d.example>")
	f.expect("250 2.1.5 OK")
	if got := srv.CmdCount("FROBNICATE"); got != 0 {
		t.Errorf("backend saw an unknown command %d times, want 0", got)
	}
}

func TestBareLfInTxnNotRelayed(t *testing.T) {
	// The smuggling defense holds inside a transaction too: a bare-LF
	// line is answered, never relayed, and the session stays in sync.
	srv := relayFake(t, fakesmtp.Script{})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.raw("RCPT TO:<smuggled@d.example>\n")
	f.expect("500 5.5.2 Bare LF is not a valid line terminator")
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")

	wantLines(t, srv, 0,
		"MAIL FROM:<a@b.example>\r\n",
		"RCPT TO:<c@d.example>\r\n",
	)
}

func TestQuitInTxn(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")

	// The 221 and the close are the session's, always synthesized.
	f.send("QUIT")
	f.expect("221 2.0.0 Bye")
	f.expectClosed()

	lines := backendLines(t, srv, 0)
	if slices.Contains(lines, ".\r\n") {
		t.Errorf("backend saw a DATA terminator for an abandoned transaction: %q", lines)
	}
	if !slices.Contains(lines, "QUIT\r\n") {
		t.Errorf("backend lines = %q, want the leg closed down", lines)
	}
}

func TestEhloMidSessionActsAsRset(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	f := newRelayClient(t, relayConfig(srv.Addr()))

	wantEhlo := []string{"250-bifrost.test", "250-PIPELINING", "250-8BITMIME", "250 SIZE 10485760"}

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")

	// RFC 5321 4.1.4: EHLO acts as RSET. The backend is dropped and the
	// reply is Bifrost's own capability set, never a backend's.
	f.send("EHLO client.example")
	f.expect(wantEhlo...)

	f.send("MAIL FROM:<a2@b.example>")
	f.expect("250 2.1.0 OK")
	if got := srv.DialCount(); got != 2 {
		t.Errorf("DialCount = %d, want 2: the post-EHLO MAIL attaches afresh", got)
	}

	lines := backendLines(t, srv, 0)
	if slices.Contains(lines, ".\r\n") {
		t.Errorf("backend saw a DATA terminator for an abandoned transaction: %q", lines)
	}
	// A second EHLO between transactions is answered the same way.
	f.send("RSET")
	f.expect("250 2.0.0 OK")
	f.send("EHLO client.example")
	f.expect(wantEhlo...)
}

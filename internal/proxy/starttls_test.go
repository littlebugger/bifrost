package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// authTestConfig is tlsTestConfig plus a listener AUTH block with one
// user, rttskr-team/pw — the credential the AUTH scenarios exercise.
func authTestConfig() *config.Config {
	cfg := tlsTestConfig()
	cfg.Listener.Auth = &config.ListenerAuth{
		Users: []config.AuthUser{testAuthUser("rttskr-team", "s1", "pw")},
	}
	return cfg
}

// authPlainPayload base64-encodes an RFC 4616 PLAIN response with an
// empty authzid.
func authPlainPayload(authcid, password string) string {
	return base64.StdEncoding.EncodeToString([]byte("\x00" + authcid + "\x00" + password))
}

// tlsTestConfig is testConfig plus the advertised STARTTLS capability.
// Listener.StartTLS (the cert *paths*) stays nil on purpose: the session
// takes an already-loaded *tls.Config, because the certificate is read
// once at startup, not once per connection.
func tlsTestConfig() *config.Config {
	cfg := testConfig()
	cfg.Listener.Capabilities = []string{"PIPELINING", "8BITMIME", "SIZE 10485760", "STARTTLS"}
	return cfg
}

// upgrade performs the client half of the STARTTLS handshake and rebuilds
// the client's reader/writer over the TLS connection.
func (c *testClient) upgrade(cfg *tls.Config) {
	c.t.Helper()
	tc := tls.Client(c.conn, cfg)
	if err := tc.Handshake(); err != nil {
		c.t.Fatalf("client handshake: %v", err)
	}
	c.conn = tc
	c.br = bufio.NewReader(tc)
}

func TestStartTLSHandshakeAndReset(t *testing.T) {
	certCfg := fakesmtp.TestCert(t)
	c := newTestClient(t, tlsTestConfig(), certCfg, &stubHandler{})

	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.expect("250-bifrost.test", "250-PIPELINING", "250-8BITMIME", "250-SIZE 10485760", "250 STARTTLS")

	c.send("STARTTLS")
	c.expect("220 2.0.0 Ready to start TLS")
	c.upgrade(certCfg)

	// RFC 3207: full state reset. The EHLO that came before the
	// handshake is gone, so MAIL is out of sequence again.
	c.send("MAIL FROM:<a@b>")
	c.expect("503 5.5.1 Bad sequence of commands")

	// Fresh EHLO works, and STARTTLS is no longer advertised.
	c.send("EHLO client.example")
	c.expect("250-bifrost.test", "250-PIPELINING", "250-8BITMIME", "250 SIZE 10485760")

	c.send("NOOP")
	c.expect("250 2.0.0 OK")
}

func TestStartTLSWithParams501(t *testing.T) {
	c := newTestClient(t, tlsTestConfig(), fakesmtp.TestCert(t), &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	c.send("STARTTLS somearg")
	c.expect("501 5.5.4 Syntax error: STARTTLS takes no parameters")

	// Still usable: the command was refused, not the session.
	c.send("NOOP")
	c.expect("250 2.0.0 OK")
}

func TestSecondStartTLS503(t *testing.T) {
	certCfg := fakesmtp.TestCert(t)
	c := newTestClient(t, tlsTestConfig(), certCfg, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	c.send("STARTTLS")
	c.expect("220 2.0.0 Ready to start TLS")
	c.upgrade(certCfg)

	c.send("STARTTLS")
	c.expect("503 5.5.1 Bad sequence of commands")
}

func TestStartTLSMidTxn503(t *testing.T) {
	c := newTestClient(t, tlsTestConfig(), fakesmtp.TestCert(t), &stubHandler{})
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	c.send("MAIL FROM:<a@b>")
	c.expect("451 4.4.1 No backend available, try again later")

	// A transaction has been attempted on this session, so no TLS
	// upgrade from here on: RFC 3207 forbids STARTTLS inside a
	// transaction, and Bifrost refuses it for the rest of the session
	// (a fresh EHLO clears that, since it resets all session state).
	c.send("STARTTLS")
	c.expect("503 5.5.1 Bad sequence of commands")

	c.send("EHLO client.example")
	c.reply()
	c.send("STARTTLS")
	c.expect("220 2.0.0 Ready to start TLS")
}

func TestStartTLSNoCert(t *testing.T) {
	// No certificate: STARTTLS is filtered out of the advertised set and
	// answers as the unknown extension it is.
	c := newTestClient(t, tlsTestConfig(), nil, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	c.send("EHLO client.example")
	c.expect("250-bifrost.test", "250-PIPELINING", "250-8BITMIME", "250 SIZE 10485760")

	c.send("STARTTLS")
	c.expect("502 5.5.1 Command not implemented")
}

func TestLineTooLong500(t *testing.T) {
	h := &stubHandler{}
	c := newTestClient(t, testConfig(), nil, h)
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	// 4096 bytes of command plus CRLF is one byte over the cap.
	c.raw("MAIL FROM:<" + strings.Repeat("a", 4096-len("MAIL FROM:<>")) + ">\r\n")
	c.expect("500 5.5.2 Command line too long")
	if got := h.seen(); len(got) != 0 {
		t.Fatalf("handler saw %d transactions, want 0", len(got))
	}

	// In sync: the over-long line was consumed through its terminator.
	c.send("NOOP")
	c.expect("250 2.0.0 OK")
}

func TestClientIdle421(t *testing.T) {
	cfg := testConfig()
	cfg.Defaults.Timeouts.ClientIdle = 50 * time.Millisecond
	cfg.Defaults.Timeouts.SessionMax = time.Minute

	c := newTestClient(t, cfg, nil, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	c.expect("421 4.4.2 Idle timeout, closing connection")
	c.expectClosed()
}

func TestFirstCommandWait421(t *testing.T) {
	cfg := testConfig()
	cfg.Defaults.Timeouts.ClientIdle = 50 * time.Millisecond

	c := newTestClient(t, cfg, nil, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	// Same timer guards the wait for the very first command.
	c.expect("421 4.4.2 Idle timeout, closing connection")
	c.expectClosed()
}

func TestSessionLifetime421(t *testing.T) {
	cfg := testConfig()
	cfg.Defaults.Timeouts.ClientIdle = time.Minute
	cfg.Defaults.Timeouts.SessionMax = 50 * time.Millisecond

	c := newTestClient(t, cfg, nil, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	// The lifetime backstop fires first and says so — a different row of
	// the contract table than the idle timeout, same 421 4.4.2 class.
	c.expect("421 4.4.2 Session lifetime exceeded, closing connection")
	c.expectClosed()
}

func TestStartTLSPipelinedPlaintext421(t *testing.T) {
	c := newTestClient(t, tlsTestConfig(), fakesmtp.TestCert(t), &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	// STARTTLS is a pipelining sync point (RFC 3207/2920): plaintext
	// behind it is a command-injection attempt (CVE-2011-0411 class), so
	// it is answered 421 4.7.0 INSTEAD of the 220 — no handshake, and the
	// buffered bytes are discarded uninterpreted.
	c.raw("STARTTLS\r\nEHLO injected.example\r\n")

	c.expect("421 4.7.0 Pipelined data after STARTTLS, closing connection")
	c.expectClosed()
}

// TestSessionAuthRequiresTLS538 covers scenario 2: a certificate is
// configured (so STARTTLS is advertised) but the client never upgrades.
// AUTH stays unadvertised and PLAIN is refused outright — RFC 4954 PLAIN
// never runs in cleartext, no matter what the client asks for.
func TestSessionAuthRequiresTLS538(t *testing.T) {
	c := newTestClient(t, authTestConfig(), fakesmtp.TestCert(t), &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	c.send("EHLO client.example")
	c.expect("250-bifrost.test", "250-PIPELINING", "250-8BITMIME", "250-SIZE 10485760", "250 STARTTLS")

	c.send("AUTH PLAIN " + authPlainPayload("rttskr-team", "pw"))
	c.expect("538 5.7.11 Encryption required for requested authentication mechanism")
}

// TestSessionAuthBeforeEhlo503: AUTH before any EHLO/HELO is a sequence
// violation, same as MAIL or RCPT (TestBadSequence) — checked before the
// TLS-encryption gate, so a configured listener still answers 503, not 538.
func TestSessionAuthBeforeEhlo503(t *testing.T) {
	c := newTestClient(t, authTestConfig(), fakesmtp.TestCert(t), &stubHandler{})
	c.expect("220 bifrost.test ESMTP")

	c.send("AUTH PLAIN " + authPlainPayload("rttskr-team", "pw"))
	c.expect("503 5.5.1 Bad sequence of commands")
}

// TestSessionAuthFullFlowAfterSTARTTLS covers scenario 3 (the whole
// post-STARTTLS lifecycle) and scenario 6 (authed survives the EHLO
// reset): EHLO advertises AUTH PLAIN once TLS is up, MAIL is gated until
// authenticated, a successful AUTH stops advertising AUTH PLAIN and
// blocks a second AUTH, and a fresh EHLO does not undo any of it.
func TestSessionAuthFullFlowAfterSTARTTLS(t *testing.T) {
	certCfg := fakesmtp.TestCert(t)
	h := &stubHandler{}
	c := newTestClient(t, authTestConfig(), certCfg, h)
	c.expect("220 bifrost.test ESMTP")

	c.send("EHLO client.example")
	c.reply() // pre-TLS advertisement is covered by TestSessionAuthRequiresTLS538

	c.send("STARTTLS")
	c.expect("220 2.0.0 Ready to start TLS")
	c.upgrade(certCfg)

	c.send("EHLO client.example")
	c.expect("250-bifrost.test", "250-PIPELINING", "250-8BITMIME", "250-SIZE 10485760", "250 AUTH PLAIN")

	c.send("MAIL FROM:<a@b>")
	c.expect("530 5.7.0 Authentication required")

	c.send("AUTH PLAIN " + authPlainPayload("rttskr-team", "pw"))
	c.expect("235 2.7.0 Authentication succeeded")

	// A fresh EHLO resets greeting state but not authed (RFC 4954: no
	// TLS downgrade path, so no reason to make the client re-auth).
	c.send("EHLO client.example")
	c.expect("250-bifrost.test", "250-PIPELINING", "250-8BITMIME", "250 SIZE 10485760")

	c.send("AUTH PLAIN " + authPlainPayload("rttskr-team", "pw"))
	c.expect("503 5.5.1 Bad sequence of commands")

	c.send("MAIL FROM:<a@b>")
	c.expect("451 4.4.1 No backend available, try again later")
	if got := h.seen(); len(got) != 1 {
		t.Fatalf("handler saw %d transactions, want 1", len(got))
	}
}

// TestSessionAuthContinuationAndCancel covers scenario 4: the 334
// continuation round-trip, and "*" cancelling it.
func TestSessionAuthContinuationAndCancel(t *testing.T) {
	certCfg := fakesmtp.TestCert(t)
	c := newTestClient(t, authTestConfig(), certCfg, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()
	c.send("STARTTLS")
	c.expect("220 2.0.0 Ready to start TLS")
	c.upgrade(certCfg)
	c.send("EHLO client.example")
	c.reply()

	// Cancel: "*" on the continuation line aborts, no payload parsed.
	c.send("AUTH PLAIN")
	c.expect("334 ")
	c.send("*")
	c.expect("501 5.0.0 Authentication cancelled")

	// Still unauthenticated and in sequence: a fresh continuation with a
	// real payload succeeds.
	c.send("AUTH PLAIN")
	c.expect("334 ")
	c.send(authPlainPayload("rttskr-team", "pw"))
	c.expect("235 2.7.0 Authentication succeeded")
}

// TestSessionAuthMechanismAndLockout covers scenario 5: an unsupported
// mechanism, a malformed payload, and three wrong passwords in a row.
func TestSessionAuthMechanismAndLockout(t *testing.T) {
	certCfg := fakesmtp.TestCert(t)
	c := newTestClient(t, authTestConfig(), certCfg, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()
	c.send("STARTTLS")
	c.expect("220 2.0.0 Ready to start TLS")
	c.upgrade(certCfg)
	c.send("EHLO client.example")
	c.reply()

	c.send("AUTH LOGIN")
	c.expect("504 5.5.4 Unrecognized authentication type")

	c.send("AUTH PLAIN !!!invalid-base64!!!")
	c.expect("501 5.5.2 Invalid authentication response")

	wrong := authPlainPayload("rttskr-team", "wrongpw")
	c.send("AUTH PLAIN " + wrong)
	c.expect("535 5.7.8 Authentication credentials invalid")
	c.send("AUTH PLAIN " + wrong)
	c.expect("535 5.7.8 Authentication credentials invalid")
	c.send("AUTH PLAIN " + wrong)
	c.expect("421 4.7.0 Too many failed authentication attempts, closing connection")
	c.expectClosed()
}

// TestSessionAuthContinuationWakesOnDrain covers the drain contract for
// the AUTH continuation read: a client parked at "334 " (sent AUTH PLAIN
// with no initial response, and never sends a payload) must be unblocked
// promptly when the session's context is cancelled — the same wakeup an
// ordinary between-commands read gets (readCommand's wakeOnCancel) — not
// left waiting on the session's own idle timer.
//
// No client-idle timeout is configured here at all (authTestConfig builds
// on testConfig, which leaves Timeouts zero), so the continuation read
// would otherwise block forever: only ctx cancellation can unblock it.
// That makes this test fail closed (it hangs until the harness's 5s
// wait() timeout) if the continuation read ever regresses to a bare
// armDeadline+ReadCommandLine again.
func TestSessionAuthContinuationWakesOnDrain(t *testing.T) {
	certCfg := fakesmtp.TestCert(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newTestClientCtx(ctx, t, authTestConfig(), certCfg, &stubHandler{})
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()
	c.send("STARTTLS")
	c.expect("220 2.0.0 Ready to start TLS")
	c.upgrade(certCfg)
	c.send("EHLO client.example")
	c.reply()

	c.send("AUTH PLAIN")
	c.expect("334 ")

	cancel()

	// The read is interrupted by the drain itself, not an idle timeout:
	// the continuation read now checks ctx.Err() first, same as Run's own
	// loop, so this gets the shutdown reply (421 4.3.0), not 4.4.2.
	c.expect("421 4.3.0 Service shutting down, closing connection")
	c.expectClosed()
}

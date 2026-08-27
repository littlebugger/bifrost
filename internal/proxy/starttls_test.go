package proxy

import (
	"bufio"
	"crypto/tls"
	"strings"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/fakesmtp"
)

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

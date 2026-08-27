package backend

import (
	"crypto/tls"
	"errors"
	"strings"
	"testing"

	"github.com/revolee/bifrost/internal/fakesmtp"
)

// TestBackendStartTLS proves the whole RFC 3207 upgrade: STARTTLS, the
// TLS handshake, and a mandatory re-EHLO whose capabilities (not the
// pre-TLS EHLO's) are what Conn.Caps() reports afterward.
func TestBackendStartTLS(t *testing.T) {
	cfg := fakesmtp.TestCert(t)
	srv := fakesmtp.Start(t, fakesmtp.Script{TLS: cfg, Caps: []string{"PIPELINING", "8BITMIME"}})

	c, err := dialTest(t, srv.Addr(), Opts{
		EhloName:  "client.example",
		TLSMode:   "starttls-verify",
		TLSConfig: cfg,
		Timeouts:  testTimeouts(),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	caps := c.Caps()
	if caps.Has("STARTTLS") {
		t.Errorf("Caps() = %v, want STARTTLS absent (fakesmtp never re-advertises it once TLS is active)", caps)
	}
	if !caps.Has("PIPELINING") || !caps.Has("8BITMIME") {
		t.Errorf("Caps() = %v, want PIPELINING and 8BITMIME (from the post-TLS EHLO)", caps)
	}
}

// TestBackendStartTLSVerifyBadCert: starttls-verify against a cert the
// caller's TLSConfig does not trust must fail the handshake.
func TestBackendStartTLSVerifyBadCert(t *testing.T) {
	serverCfg := fakesmtp.TestCert(t)
	srv := fakesmtp.Start(t, fakesmtp.Script{TLS: serverCfg})

	untrusted := &tls.Config{ServerName: "127.0.0.1"} // no RootCAs trusting the fake's self-signed cert
	_, err := dialTest(t, srv.Addr(), Opts{
		EhloName:  "client.example",
		TLSMode:   "starttls-verify",
		TLSConfig: untrusted,
		Timeouts:  testTimeouts(),
	})
	var herr *HandshakeError
	if !errors.As(err, &herr) {
		t.Fatalf("Dial err = %v (%T), want *HandshakeError (untrusted cert, verify mode)", err, err)
	}
}

// TestBackendStartTLSNoVerify: the exact same untrusted cert/config as
// above, but TLSMode "starttls" (not "-verify") must accept it —
// opportunistic encryption never checks the chain.
func TestBackendStartTLSNoVerify(t *testing.T) {
	serverCfg := fakesmtp.TestCert(t)
	srv := fakesmtp.Start(t, fakesmtp.Script{TLS: serverCfg})

	untrusted := &tls.Config{ServerName: "127.0.0.1"}
	_, err := dialTest(t, srv.Addr(), Opts{
		EhloName:  "client.example",
		TLSMode:   "starttls",
		TLSConfig: untrusted,
		Timeouts:  testTimeouts(),
	})
	if err != nil {
		t.Fatalf("Dial: %v, want success (starttls mode never verifies)", err)
	}
}

// TestBackendTLSRequiredMismatch covers both directions of a TLS-mode
// mismatch between Bifrost and the backend.
func TestBackendTLSRequiredMismatch(t *testing.T) {
	t.Run("TLSMode none never attempts STARTTLS, even against a backend that demands it", func(t *testing.T) {
		cfg := fakesmtp.TestCert(t)
		srv := fakesmtp.Start(t, fakesmtp.Script{
			TLS:    cfg,
			OnMAIL: []fakesmtp.Step{{Reply: "530 5.7.0 Must issue a STARTTLS command first"}},
		})

		c, err := dialTest(t, srv.Addr(), Opts{EhloName: "client.example", TLSMode: "none", Timeouts: testTimeouts()})
		if err != nil {
			t.Fatalf("Dial: %v, want success (TLSMode none skips STARTTLS entirely)", err)
		}

		// Task 4 gives Conn a real SendLine/Replies; here (Task 3) the
		// mismatch is proven directly against the still-plaintext wire.
		if _, err := c.conn.Write([]byte("MAIL FROM:<a@b>\r\n")); err != nil {
			t.Fatalf("write MAIL: %v", err)
		}
		_, code, _, err := c.rr.Next()
		if err != nil {
			t.Fatalf("read MAIL reply: %v", err)
		}
		if code != 530 {
			t.Errorf("MAIL reply code = %d, want 530 (backend's own TLS-required reject, never attempted by Dial)", code)
		}
	})

	t.Run("TLSMode starttls against a backend that never advertises it", func(t *testing.T) {
		srv := fakesmtp.Start(t, fakesmtp.Script{}) // Script.TLS nil: STARTTLS never advertised

		_, err := dialTest(t, srv.Addr(), Opts{
			EhloName:  "client.example",
			TLSMode:   "starttls",
			TLSConfig: &tls.Config{},
			Timeouts:  testTimeouts(),
		})
		var herr *HandshakeError
		if !errors.As(err, &herr) {
			t.Fatalf("Dial err = %v (%T), want *HandshakeError", err, err)
		}
		if !strings.Contains(herr.Error(), "STARTTLS not offered") {
			t.Errorf("HandshakeError = %v, want it to say STARTTLS not offered", err)
		}
	})
}

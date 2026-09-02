package backend

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// authOpts builds the common Opts every AUTH test shares: creds set, a
// TLS-upgraded connection (Dial now refuses to originate AUTH over
// cleartext, see TestDialAuthRefusesCleartext), everything else plain and
// generously timed. TLSMode defaults to "starttls", which never verifies
// the certificate (see backend.Conn.startTLS), so callers need not also
// wire up TLSConfig.
func authOpts(extra Opts) Opts {
	extra.EhloName = "client.example"
	if extra.TLSMode == "" {
		extra.TLSMode = "starttls"
	}
	extra.AuthUsername = "user"
	extra.AuthPassword = "pass"
	extra.Timeouts = testTimeouts()
	return extra
}

func TestDialAuthHappyPath(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{TLS: fakesmtp.TestCert(t), Caps: []string{"PIPELINING", "AUTH PLAIN"}})

	c, err := dialTest(t, srv.Addr(), authOpts(Opts{}))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if c == nil {
		t.Fatal("Dial returned nil Conn with nil error")
	}

	want := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass")) + "\r\n"
	sessions := srv.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("Sessions() len = %d, want 1", len(sessions))
	}
	var got []byte
	for _, ev := range sessions[0].Transcript() {
		if ev.Verb == "AUTH" {
			got = ev.Line
		}
	}
	if string(got) != want {
		t.Errorf("recorded AUTH line = %q, want %q", got, want)
	}
}

// TestDialAuthRefusesCleartext is the belt-and-braces guard itself: creds
// set, TLSMode "none", and a fake that WOULD accept AUTH PLAIN (it's
// advertised). Dial must refuse before a single AUTH byte reaches the
// wire — config.Validate is supposed to prevent this combination from
// ever reaching Dial, but Dial does not trust that every caller got it
// right.
func TestDialAuthRefusesCleartext(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"AUTH PLAIN"}})

	c, err := dialTest(t, srv.Addr(), Opts{
		EhloName:     "client.example",
		TLSMode:      "none",
		AuthUsername: "user",
		AuthPassword: "pass",
		Timeouts:     testTimeouts(),
	})
	if c != nil {
		t.Fatalf("Dial returned non-nil Conn, want nil when creds are set over an un-upgraded connection")
	}
	var herr *HandshakeError
	if !errors.As(err, &herr) {
		t.Fatalf("Dial err = %v (%T), want *HandshakeError", err, err)
	}

	sessions := srv.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("Sessions() len = %d, want 1", len(sessions))
	}
	for _, ev := range sessions[0].Transcript() {
		if ev.Verb == "AUTH" {
			t.Errorf("transcript contains an AUTH line %q, want none", ev.Line)
		}
	}
}

// TestDialAuthAllowsCleartext is TestDialAuthRefusesCleartext's mirror
// with AuthAllowCleartext set: the same TLSMode "none" connection and a
// fake that accepts AUTH PLAIN now succeeds, proving the knob — not just
// the absence of the guard — is what lets the AUTH line reach the wire.
func TestDialAuthAllowsCleartext(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"AUTH PLAIN"}})

	c, err := dialTest(t, srv.Addr(), Opts{
		EhloName:           "client.example",
		TLSMode:            "none",
		AuthUsername:       "user",
		AuthPassword:       "pass",
		AuthAllowCleartext: true,
		Timeouts:           testTimeouts(),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if c == nil {
		t.Fatal("Dial returned nil Conn with nil error")
	}

	want := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass")) + "\r\n"
	sessions := srv.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("Sessions() len = %d, want 1", len(sessions))
	}
	var got []byte
	for _, ev := range sessions[0].Transcript() {
		if ev.Verb == "AUTH" {
			got = ev.Line
		}
	}
	if string(got) != want {
		t.Errorf("recorded AUTH line = %q, want %q", got, want)
	}
}

func TestDialAuthNotAdvertised(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{TLS: fakesmtp.TestCert(t), Caps: []string{"PIPELINING"}})

	c, err := dialTest(t, srv.Addr(), authOpts(Opts{}))
	if c != nil {
		t.Fatalf("Dial returned non-nil Conn, want nil when AUTH PLAIN is missing")
	}
	var ierr *IncompatibleError
	if !errors.As(err, &ierr) {
		t.Fatalf("Dial err = %v (%T), want *IncompatibleError", err, err)
	}
	if len(ierr.Missing) != 1 || ierr.Missing[0] != "AUTH PLAIN" {
		t.Errorf("Missing = %v, want [\"AUTH PLAIN\"]", ierr.Missing)
	}
}

func TestDialAuthPermanentFailure(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{
		TLS:    fakesmtp.TestCert(t),
		Caps:   []string{"AUTH PLAIN"},
		OnAUTH: []fakesmtp.Step{{Reply: "535 5.7.8 nope"}},
	})

	c, err := dialTest(t, srv.Addr(), authOpts(Opts{}))
	if c != nil {
		t.Fatalf("Dial returned non-nil Conn, want nil on a rejected AUTH")
	}
	var aerr *AuthError
	if !errors.As(err, &aerr) {
		t.Fatalf("Dial err = %v (%T), want *AuthError", err, err)
	}
	if aerr.Code != 535 {
		t.Errorf("Code = %d, want 535", aerr.Code)
	}
	if !aerr.Permanent() {
		t.Errorf("Permanent() = false, want true for code 535")
	}
}

func TestDialAuthTransientFailure(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{
		TLS:    fakesmtp.TestCert(t),
		Caps:   []string{"AUTH PLAIN"},
		OnAUTH: []fakesmtp.Step{{Reply: "454 4.7.0 later"}},
	})

	c, err := dialTest(t, srv.Addr(), authOpts(Opts{}))
	if c != nil {
		t.Fatalf("Dial returned non-nil Conn, want nil on a rejected AUTH")
	}
	var aerr *AuthError
	if !errors.As(err, &aerr) {
		t.Fatalf("Dial err = %v (%T), want *AuthError", err, err)
	}
	if aerr.Code != 454 {
		t.Errorf("Code = %d, want 454", aerr.Code)
	}
	if aerr.Permanent() {
		t.Errorf("Permanent() = true, want false for code 454")
	}
}

// TestDialAuthMechanismListPresent is the case-variance scenario: PLAIN
// is one mechanism among several on AUTH's capability line, so the match
// must be a token compare, not a whole-value one.
func TestDialAuthMechanismListPresent(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{TLS: fakesmtp.TestCert(t), Caps: []string{"AUTH LOGIN PLAIN"}})

	if _, err := dialTest(t, srv.Addr(), authOpts(Opts{})); err != nil {
		t.Fatalf("Dial: %v", err)
	}
}

func TestDialAuthMechanismListMissing(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{TLS: fakesmtp.TestCert(t), Caps: []string{"AUTH LOGIN"}})

	_, err := dialTest(t, srv.Addr(), authOpts(Opts{}))
	var ierr *IncompatibleError
	if !errors.As(err, &ierr) {
		t.Fatalf("Dial err = %v (%T), want *IncompatibleError", err, err)
	}
	if len(ierr.Missing) != 1 || ierr.Missing[0] != "AUTH PLAIN" {
		t.Errorf("Missing = %v, want [\"AUTH PLAIN\"]", ierr.Missing)
	}
}

// TestDialAuthIncompatiblePostTLS proves AUTH is checked against the
// POST-TLS capability set, mirroring
// TestDialIncompatibleCapabilitiesPostTLS: the fake advertises AUTH PLAIN
// only on its first (pre-TLS) EHLO and drops it on the second (post-TLS,
// after the mandatory re-EHLO).
func TestDialAuthIncompatiblePostTLS(t *testing.T) {
	cfg := fakesmtp.TestCert(t)
	srv := fakesmtp.Start(t, fakesmtp.Script{
		TLS: cfg,
		OnEHLO: []fakesmtp.Step{
			// Custom Reply text bypasses fakesmtp's auto-appended STARTTLS,
			// so it must be listed explicitly here for the handshake's own
			// STARTTLS to proceed.
			{Reply: "250-fakesmtp\r\n250-AUTH PLAIN\r\n250 STARTTLS"}, // pre-TLS: has AUTH PLAIN
			{Reply: "250 fakesmtp"},                                   // post-TLS: AUTH dropped
		},
	})

	_, err := dialTest(t, srv.Addr(), authOpts(Opts{
		TLSMode:   "starttls",
		TLSConfig: &tls.Config{ServerName: "127.0.0.1"}, // starttls mode never verifies; see startTLS
	}))
	var ierr *IncompatibleError
	if !errors.As(err, &ierr) {
		t.Fatalf("Dial err = %v (%T), want *IncompatibleError (post-TLS EHLO dropped AUTH)", err, err)
	}
	if len(ierr.Missing) != 1 || ierr.Missing[0] != "AUTH PLAIN" {
		t.Errorf("Missing = %v, want [\"AUTH PLAIN\"] (proves the POST-TLS set is what's checked)", ierr.Missing)
	}
}

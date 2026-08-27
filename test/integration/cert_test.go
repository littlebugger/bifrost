//go:build integration

package integration

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

// Epic-10's cert-rotation proof: the routine 90-day rotation must be a
// reload, not a restart (docs/operations.md's "Cert rotation" section).

// TestReloadPicksUpRotatedCert is the routine 90-day rotation: the
// cert/key files are replaced in place and reloaded, and the very next
// STARTTLS handshake presents the new certificate — no restart. A TLS
// session established before the swap keeps the certificate it negotiated
// and goes on working.
func TestReloadPicksUpRotatedCert(t *testing.T) {
	a := namedFake(t, "A")
	smtp, adminAddr := freeAddr(t), freeAddr(t)
	dir := t.TempDir()
	certPath, keyPath, first := writeCertPair(t, dir, "listener")

	cfg := fixture{
		smtp: smtp, admin: adminAddr,
		caps:     `["PIPELINING", "8BITMIME", "STARTTLS"]`,
		starttls: fmt.Sprintf("\n  starttls {\n    cert = %q\n    key  = %q\n  }\n", certPath, keyPath),
		pools:    poolHCL("p", serverHCL("a", a.Addr(), 1)),
	}.render()
	b := startBifrost(t, writeFile(t, dir, "bifrost.hcl", cfg), smtp, adminAddr)

	before, tlsConn := startTLSSession(t, b.smtp)
	if got, want := before.SerialNumber.String(), certSerial(t, first).String(); got != want {
		t.Fatalf("served certificate serial = %s, want the configured one %s", got, want)
	}

	// Same paths, new bytes — exactly what certbot does.
	_, _, second := writeCertPair(t, dir, "listener")
	b.reloadOK("") // the config file itself is unchanged; only the cert files moved

	after, _ := startTLSSession(t, b.smtp)
	wantSerial := certSerial(t, second).String()
	if got := after.SerialNumber.String(); got != wantSerial {
		t.Errorf("certificate after the reload = %s, want the rotated one %s (a restart must not be needed)", got, wantSerial)
	}
	if before.SerialNumber.Cmp(after.SerialNumber) == 0 {
		t.Errorf("certificate serial did not change across the rotation (%s)", before.SerialNumber)
	}

	// The session that handshook before the rotation is untouched.
	if _, err := tlsConn.Write([]byte("NOOP\r\n")); err != nil {
		t.Fatalf("write on the pre-rotation TLS session: %v", err)
	}
	if err := tlsConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := tlsConn.Read(buf)
	if err != nil {
		t.Fatalf("read on the pre-rotation TLS session: %v", err)
	}
	if got := string(buf[:n]); !strings.HasPrefix(got, "250") {
		t.Errorf("pre-rotation session NOOP reply = %q, want a 250", got)
	}
}

// startTLSSession drives banner/EHLO/STARTTLS on a fresh connection and
// returns the certificate the listener presented, plus the live TLS
// connection (so a caller can prove an established session survives a
// rotation).
func startTLSSession(t *testing.T, addr string) (*x509.Certificate, *tls.Conn) {
	t.Helper()
	c := dialRaw(t, addr)
	c.reply(5 * time.Second)
	c.send("EHLO tls.example")
	if got := c.reply(5 * time.Second); !strings.HasPrefix(got, "250") {
		t.Fatalf("EHLO reply = %q, want 250", got)
	}
	c.send("STARTTLS")
	if got := c.reply(5 * time.Second); !strings.HasPrefix(got, "220") {
		t.Fatalf("STARTTLS reply = %q, want 220", got)
	}
	// InsecureSkipVerify: this test is about WHICH certificate is served,
	// not about trusting it (each rotation generates a fresh self-signed
	// one, so there is no stable root to pin).
	tc := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		t.Fatalf("handshake presented no certificate")
	}
	return certs[0], tc
}

// certSerial is the serial number of the certificate inside a
// fakesmtp.TestCert-style config — the handle these tests use to tell one
// generated certificate from another.
func certSerial(t *testing.T, cfg *tls.Config) *big.Int {
	t.Helper()
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse generated certificate: %v", err)
	}
	return leaf.SerialNumber
}

// TestReloadKeepsCertWhenPairIsMidRotation: the cert and the key are read
// as a pair, so a reload that lands between an operator's two writes sees
// a mismatched one. Handshakes must not start failing for that — the
// certificate already loaded keeps being served — and the failure must not
// be cached: finishing the second write is enough, with no second reload.
func TestReloadKeepsCertWhenPairIsMidRotation(t *testing.T) {
	a := namedFake(t, "A")
	smtp, adminAddr := freeAddr(t), freeAddr(t)
	dir := t.TempDir()
	certPath, keyPath, first := writeCertPair(t, dir, "listener")

	cfg := fixture{
		smtp: smtp, admin: adminAddr,
		caps:     `["PIPELINING", "8BITMIME", "STARTTLS"]`,
		starttls: fmt.Sprintf("\n  starttls {\n    cert = %q\n    key  = %q\n  }\n", certPath, keyPath),
		pools:    poolHCL("p", serverHCL("a", a.Addr(), 1)),
	}.render()
	b := startBifrost(t, writeFile(t, dir, "bifrost.hcl", cfg), smtp, adminAddr)

	before, _ := startTLSSession(t, b.smtp)
	if got, want := before.SerialNumber.String(), certSerial(t, first).String(); got != want {
		t.Fatalf("served certificate = %s, want the configured one %s", got, want)
	}

	// Half a rotation: the new certificate is in place, its key is not.
	rotatedCert, rotatedKey, second := writeCertPair(t, dir, "rotating")
	copyFile(t, rotatedCert, certPath)
	b.reloadOK("")

	midRotation, _ := startTLSSession(t, b.smtp)
	if got := midRotation.SerialNumber.String(); got != before.SerialNumber.String() {
		t.Errorf("certificate served with a mismatched pair = %s, want the previously loaded %s", got, before.SerialNumber)
	}
	if logs := b.logText(); !strings.Contains(logs, "keeping the one already loaded") {
		t.Errorf("logs do not report falling back to the loaded certificate:\n%s", logs)
	}

	// Finish the rotation: no reload needed, the next handshake re-reads.
	copyFile(t, rotatedKey, keyPath)
	after, _ := startTLSSession(t, b.smtp)
	if got, want := after.SerialNumber.String(), certSerial(t, second).String(); got != want {
		t.Errorf("certificate after the key caught up = %s, want the rotated one %s (a failed read must not be cached)", got, want)
	}
}

// copyFile copies src over dst — half a rotation, on purpose.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	writeFileAt(t, dst, string(data))
}

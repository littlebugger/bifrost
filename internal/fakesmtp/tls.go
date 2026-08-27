package fakesmtp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// TestCert generates a fresh self-signed certificate for 127.0.0.1 and
// localhost and returns one *tls.Config usable directly on both sides of
// an in-test STARTTLS handshake: as the fake's Script.TLS (it carries the
// certificate) and as the test client's config (it carries a RootCAs pool
// that trusts that same certificate) — tls.Config only reads the fields
// relevant to whichever role it's used in.
//
// ServerName is preset to "127.0.0.1", matching Server.Addr(). A caller
// that dials "localhost" instead should Clone() this config and override
// ServerName to "localhost" (also a SAN on the certificate) before use.
func TestCert(t testing.TB) *tls.Config {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("fakesmtp: generate key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("fakesmtp: generate serial: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "fakesmtp test cert"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("fakesmtp: create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("fakesmtp: parse certificate: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(leaf)

	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
		RootCAs:      roots,
		ServerName:   "127.0.0.1",
		MinVersion:   tls.VersionTLS12,
	}
}

// effectiveCaps returns the capability list to advertise on EHLO: the
// script's configured Caps, plus "STARTTLS" when TLS is configured and
// the session hasn't already upgraded.
func effectiveCaps(script Script, tlsActive bool) []string {
	if script.TLS == nil || tlsActive {
		return script.Caps
	}
	return append(append([]string{}, script.Caps...), "STARTTLS")
}

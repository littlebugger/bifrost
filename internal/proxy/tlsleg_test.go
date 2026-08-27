//go:build integration

package proxy

import (
	"testing"

	"github.com/revolee/bifrost/internal/fakesmtp"
)

// The backend leg's TLS mode is the pool's business (the client leg's TLS
// terminates here), so the splice has to behave identically over an
// upgraded leg — including when that leg dies mid-body.

// startTLSFake starts a fake that offers STARTTLS, and returns it with a
// config whose pool upgrades the backend leg.
func startTLSFake(t *testing.T, s fakesmtp.Script) (*fakesmtp.Server, *relayFixture) {
	t.Helper()
	s.TLS = fakesmtp.TestCert(t)
	srv := relayFake(t, s)
	cfg := relayConfig(srv.Addr())
	cfg.Pools[0].BackendTLS = "starttls"
	return srv, newRelayClient(t, cfg)
}

func TestRelayOverStartTLSBackend(t *testing.T) {
	srv, f := startTLSFake(t, fakesmtp.Script{})

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<c@d.example>")
	f.expect("250 2.1.5 OK")
	f.send("DATA")
	f.expect("354 Start mail input; end with <CRLF>.<CRLF>")
	f.raw("Subject: tls\r\n\r\nbody over an upgraded leg\r\n.\r\n")
	f.expect("250 2.0.0 OK: queued")

	srv.AssertWireBody(t, 0, []byte("Subject: tls\r\n\r\nbody over an upgraded leg\r\n"))
	if got := srv.CmdCount("STARTTLS"); got != 1 {
		t.Errorf("backend STARTTLS count = %d, want 1: the leg was not upgraded", got)
	}
}

func TestBackendDiesMidDataOverTLS(t *testing.T) {
	srv, f := startTLSFake(t, fakesmtp.Script{
		MidBody: []fakesmtp.Step{{Action: fakesmtp.ActRST}},
	})

	f.startBody()
	f.bodyPieces(12)
	f.expect("451 4.4.1 Backend connection lost")

	// Drained to the terminator, so the session is still in command sync.
	f.send("NOOP")
	f.expect("250 2.0.0 OK")
	f.send("MAIL FROM:<a2@b.example>")
	f.expect("250 2.1.0 OK")
	if got := srv.CmdCount("STARTTLS"); got != 2 {
		t.Errorf("backend STARTTLS count = %d, want 2: the retry upgrades too", got)
	}
}

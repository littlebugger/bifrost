//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/proxy"
	"github.com/littlebugger/bifrost/internal/smtpdrv"
)

// Task 11: the double-sided AUTH proof. Every prior task lands one half
// of SMTP AUTH (client-leg PLAIN termination, backend-leg PLAIN
// origination); only an end-to-end run proves the two halves actually
// splice together the way the spec claims: a client authenticates to
// bifrost, and bifrost — independently — authenticates to the backend
// with the pool's own credentials.

const (
	clientAuthUser = "alice"
	clientAuthSalt = "pepper"
	clientAuthPass = "correct-horse"

	poolAuthUser = "relay-app"
	poolAuthPass = "backend-secret"
)

// authTestUser builds a config.AuthUser the same way an operator's config
// loader would: hex(sha256(salt+password)), lowercase.
func authTestUser(name, salt, password string) config.AuthUser {
	sum := sha256.Sum256([]byte(salt + password))
	return config.AuthUser{Name: name, Salt: salt, HashedPassword: hex.EncodeToString(sum[:])}
}

// authPlainPayload base64-encodes an RFC 4616 PLAIN response with an
// empty authzid, the wire format AUTH PLAIN's initial response takes.
func authPlainPayload(authcid, password string) string {
	return base64.StdEncoding.EncodeToString([]byte("\x00" + authcid + "\x00" + password))
}

// poolAuthWireLine is the exact backend-leg AUTH line bifrost must send:
// "AUTH PLAIN "+base64("\x00"+user+"\x00"+pass)+CRLF.
func poolAuthWireLine() string {
	cred := base64.StdEncoding.EncodeToString([]byte("\x00" + poolAuthUser + "\x00" + poolAuthPass))
	return "AUTH PLAIN " + cred + "\r\n"
}

// authBackendCaps is the fake backend's advertised set: enough to satisfy
// the listener's superset check, plus AUTH PLAIN so backend.Dial actually
// originates the backend-leg exchange.
func authBackendCaps() []string { return []string{"PIPELINING", "8BITMIME", "AUTH PLAIN"} }

// authChainConfig is a listener with STARTTLS and one client-leg AUTH
// user, plus a single-server pool that upgrades its own leg to TLS and
// authenticates to the backend with the pool's credentials — the full
// double-sided wiring the brief's three scenarios exercise.
func authChainConfig(backendAddr string) *config.Config {
	pool := config.Pool{
		Name: "p", Balance: "roundrobin", BackendTLS: "starttls", EhloName: "bifrost.test",
		Auth:    &config.PoolAuth{Username: poolAuthUser, Password: poolAuthPass},
		Servers: []config.Server{{Name: "s0", Address: backendAddr, Weight: 1}},
	}
	return &config.Config{
		Defaults: config.Defaults{Timeouts: m1Timeouts()},
		Listener: config.Listener{
			Hostname:     "bifrost.test",
			Capabilities: []string{"PIPELINING", "8BITMIME", "STARTTLS"},
			Auth: &config.ListenerAuth{
				Users: []config.AuthUser{authTestUser(clientAuthUser, clientAuthSalt, clientAuthPass)},
			},
		},
		Pools:   []config.Pool{pool},
		Routing: config.Routing{DefaultPool: "p"},
	}
}

// serveTLS is m1_test.go's serve plus a client-facing tls.Config: the one
// piece that harness lacks, since none of its own tests exercise the
// listener's own STARTTLS. Everything else (accept loop, per-conn
// Session, goleak-visible teardown via t.Cleanup) is identical.
func serveTLS(t *testing.T, cfg *config.Config, tlsCfg *tls.Config, h proxy.TxnHandler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns []net.Conn
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()

			wg.Add(1)
			go func() {
				defer wg.Done()
				s := proxy.NewSession(conn, cfg, tlsCfg, h, slog.New(slog.DiscardHandler))
				if err := s.Run(context.Background()); err != nil {
					t.Errorf("session ended with %v, want nil", err)
				}
			}()
		}
	}()

	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}

// TestAuthFullChainWithBackendCreds is scenario 1: the client authenticates
// to bifrost (wrong password first, then right), and bifrost —
// separately — authenticates to the backend with the pool's own
// credentials before ever relaying MAIL. Both halves are proven from
// outside: the client's own reply codes, and the fake's byte-exact
// transcript of what bifrost put on the wire.
func TestAuthFullChainWithBackendCreds(t *testing.T) {
	fake := fakesmtp.Start(t, fakesmtp.Script{
		Caps: authBackendCaps(),
		TLS:  fakesmtp.TestCert(t),
	})
	cfg := authChainConfig(fake.Addr())

	listenerTLS := fakesmtp.TestCert(t)
	addr := serveTLS(t, cfg, listenerTLS, newRelay(cfg))

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")

	c.Send("EHLO client.example")
	pre := c.Expect("250")
	if joined := strings.Join(pre.Lines, "\n"); strings.Contains(joined, "AUTH") {
		t.Fatalf("pre-STARTTLS EHLO = %v, want no AUTH line advertised in cleartext", pre.Lines)
	}

	c.StartTLS(listenerTLS)

	c.Send("EHLO client.example")
	post := c.Expect("250")
	if joined := strings.Join(post.Lines, "\n"); !strings.Contains(joined, "AUTH PLAIN") {
		t.Fatalf("post-STARTTLS EHLO = %v, want AUTH PLAIN advertised", post.Lines)
	}

	c.Send("AUTH PLAIN " + authPlainPayload(clientAuthUser, "wrong-password"))
	if reply := c.Expect("535"); reply.Lines[0] != "535 5.7.8 Authentication credentials invalid" {
		t.Fatalf("wrong-password AUTH reply = %q, want the exact 535 text", reply.Lines[0])
	}

	c.Send("AUTH PLAIN " + authPlainPayload(clientAuthUser, clientAuthPass))
	if reply := c.Expect("235"); reply.Lines[0] != "235 2.7.0 Authentication succeeded" {
		t.Fatalf("correct AUTH reply = %q, want the exact 235 text", reply.Lines[0])
	}

	verdict := c.SendMsg(0)
	if want := "250 2.0.0 OK: queued"; len(verdict.Lines) != 1 || verdict.Lines[0] != want {
		t.Fatalf("message verdict = %v, want [%q] (the backend's own reply relayed)", verdict.Lines, want)
	}
	c.Send("QUIT")
	c.Expect("221")

	// The backend-leg proof: bifrost's own AUTH PLAIN line, byte-exact,
	// on the wire before MAIL — not merely "authentication happened
	// somewhere", but the pool's credentials replayed verbatim.
	sessions := fake.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("fake backend sessions = %d, want 1", len(sessions))
	}
	transcript := sessions[0].Transcript()
	authIdx, mailIdx := -1, -1
	for i, ev := range transcript {
		switch ev.Verb {
		case "AUTH":
			if authIdx == -1 {
				authIdx = i
			}
		case "MAIL":
			if mailIdx == -1 {
				mailIdx = i
			}
		}
	}
	if authIdx == -1 {
		t.Fatalf("backend transcript has no AUTH line: %v", transcript)
	}
	if mailIdx == -1 {
		t.Fatalf("backend transcript has no MAIL line: %v", transcript)
	}
	if want := poolAuthWireLine(); string(transcript[authIdx].Line) != want {
		t.Errorf("backend AUTH line = %q, want exactly %q", transcript[authIdx].Line, want)
	}
	if authIdx >= mailIdx {
		t.Errorf("backend AUTH line came at transcript index %d, MAIL at %d, want AUTH before MAIL", authIdx, mailIdx)
	}
}

// TestAuthGateRequiresClientAuth is scenario 2: the same double-sided
// config, but the client skips AUTH entirely — MAIL must be refused
// before a backend is ever touched.
func TestAuthGateRequiresClientAuth(t *testing.T) {
	fake := fakesmtp.Start(t, fakesmtp.Script{
		Caps: authBackendCaps(),
		TLS:  fakesmtp.TestCert(t),
	})
	cfg := authChainConfig(fake.Addr())

	listenerTLS := fakesmtp.TestCert(t)
	addr := serveTLS(t, cfg, listenerTLS, newRelay(cfg))

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")
	c.StartTLS(listenerTLS)
	c.Send("EHLO client.example")
	c.Expect("250")

	c.Send("MAIL FROM:<a@test.example>")
	if reply := c.Expect("530"); reply.Lines[0] != "530 5.7.0 Authentication required" {
		t.Fatalf("unauthenticated MAIL reply = %q, want the exact 530 text", reply.Lines[0])
	}

	if got := fake.DialCount(); got != 0 {
		t.Errorf("backend DialCount = %d, want 0: the gate must reject before any backend is touched", got)
	}
}

// TestAuthBackendCredsRejectedYieldsNoBackend is scenario 3: the client
// authenticates fine, but the pool's own credentials are wrong as far as
// the backend is concerned. The backend's 535 must never reach the
// client — attach.go's walk exhausts the one candidate and falls through
// to the closed-enum "no backend available" synth, exactly like a dial
// failure would.
func TestAuthBackendCredsRejectedYieldsNoBackend(t *testing.T) {
	fake := fakesmtp.Start(t, fakesmtp.Script{
		Caps:   authBackendCaps(),
		TLS:    fakesmtp.TestCert(t),
		OnAUTH: []fakesmtp.Step{{Reply: "535 5.7.8 Authentication credentials invalid"}},
	})
	cfg := authChainConfig(fake.Addr())

	listenerTLS := fakesmtp.TestCert(t)
	addr := serveTLS(t, cfg, listenerTLS, newRelay(cfg))

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")
	c.StartTLS(listenerTLS)
	c.Send("EHLO client.example")
	c.Expect("250")

	c.Send("AUTH PLAIN " + authPlainPayload(clientAuthUser, clientAuthPass))
	c.Expect("235")

	c.Send("MAIL FROM:<a@test.example>")
	reply := c.Expect("451")
	if want := "451 4.4.1 No backend available, try again later"; reply.Lines[0] != want {
		t.Fatalf("MAIL after backend auth rejection = %q, want the closed-enum %q (never the backend's own 535)", reply.Lines[0], want)
	}

	// The backend really was tried (and really did reject AUTH) — this
	// is not passing by accident because nothing was dialed at all.
	if got := fake.CmdCount("AUTH"); got == 0 {
		t.Errorf("backend AUTH command count = %d, want at least 1: the backend leg must actually have been attempted", got)
	}
}

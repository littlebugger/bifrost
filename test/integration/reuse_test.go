//go:build integration

package integration

import (
	"testing"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/smtpdrv"
)

// Task 5: the backend connection reuse proof end to end. Tasks 1-4 land
// the pieces (the reuse_envelopes knob, stashing a clean leg, picking it
// back up with a client-invisible RSET, failing over transparently when
// it's dead) as internal/proxy unit tests over net.Pipe; this file is the
// only place they run through a real listener, real TCP, and a real
// fakesmtp backend together.

// wantQueued asserts reply is the single-line "queued" verdict every
// clean SendMsg in this file expects.
func wantQueued(t *testing.T, i int, reply smtpdrv.Reply) {
	t.Helper()
	if want := "250 2.0.0 OK: queued"; len(reply.Lines) != 1 || reply.Lines[0] != want {
		t.Fatalf("message %d verdict = %q, want [%q]", i, reply.Lines, want)
	}
}

// TestReuseSharesOneConnAcrossEnvelopesWithSingleAuth is scenario 1: a
// pool with backend-leg AUTH and reuse_envelopes=3 relays three envelopes
// on one client session over exactly one dialed leg. Pool auth runs at
// Dial time only, so proving it happened once — not once per envelope —
// is the whole point: bifrost's own AUTH PLAIN line must appear exactly
// once in the backend transcript, ahead of every MAIL, with a client-
// invisible RSET between envelopes doing the revalidation instead.
func TestReuseSharesOneConnAcrossEnvelopesWithSingleAuth(t *testing.T) {
	const messages = 3

	fake := fakesmtp.Start(t, fakesmtp.Script{
		Caps: authBackendCaps(), // PIPELINING, 8BITMIME, AUTH PLAIN (auth_test.go)
		TLS:  fakesmtp.TestCert(t),
	})
	cfg := &config.Config{
		Defaults: config.Defaults{Timeouts: m1Timeouts()},
		Listener: config.Listener{
			Hostname:     "bifrost.test",
			Capabilities: []string{"PIPELINING", "8BITMIME"},
		},
		Pools: []config.Pool{{
			Name: "p", Balance: "roundrobin", BackendTLS: "starttls", EhloName: "bifrost.test",
			Auth:           &config.PoolAuth{Username: poolAuthUser, Password: poolAuthPass},
			ReuseEnvelopes: messages,
			Servers:        []config.Server{{Name: "s0", Address: fake.Addr(), Weight: 1}},
		}},
		Routing: config.Routing{DefaultPool: "p"},
	}
	addr := serve(t, cfg, newRelay(cfg))

	// No client-leg AUTH in this config: only the pool's own backend-leg
	// credentials are under test here.
	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	for i := 0; i < messages; i++ {
		wantQueued(t, i, c.SendMsg(i))
	}
	c.Send("QUIT")
	c.Expect("221")

	if got := fake.DialCount(); got != 1 {
		t.Errorf("DialCount = %d, want 1: all three envelopes share one dialed leg", got)
	}
	if got := fake.CmdCount("RSET"); got != messages-1 {
		t.Errorf("backend RSET count = %d, want %d: one revalidation before each later envelope", got, messages-1)
	}

	sessions := fake.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("fake backend sessions = %d, want 1", len(sessions))
	}
	transcript := sessions[0].Transcript()
	authIdx, mailIdx, authCount := -1, -1, 0
	for i, ev := range transcript {
		switch ev.Verb {
		case "AUTH":
			authCount++
			if authIdx == -1 {
				authIdx = i
			}
			if want := poolAuthWireLine(); string(ev.Line) != want {
				t.Errorf("backend AUTH line = %q, want exactly %q", ev.Line, want)
			}
		case "MAIL":
			if mailIdx == -1 {
				mailIdx = i
			}
		}
	}
	if authCount != 1 {
		t.Errorf("backend transcript AUTH line count = %d, want 1 (per connection, not per envelope)", authCount)
	}
	if authIdx == -1 || mailIdx == -1 || authIdx >= mailIdx {
		t.Errorf("backend AUTH line (index %d) must precede the first MAIL (index %d)", authIdx, mailIdx)
	}
}

// TestReuseDeadCachedConnFailsOverToFreshDial is scenario 2: the cached
// leg dies between envelopes. fakesmtp's SetDown only takes effect on
// connections accepted from that point on (see internal/fakesmtp/down.go),
// so it cannot sever a conn already established — this scripts the same
// technique Task 4's own unit tests use (relay_test.go
// TestReuseDeadCachedConnFailsOverTransparently): OnRSET drops the
// connection outright, which is exactly when reuse's own revalidation
// touches the cached leg. With only one server configured, attachAndRelay's
// normal walk (proxy/attach.go) falls back to dialing that same server
// fresh, and the client sees nothing but the ordinary reply sequence for
// both envelopes.
func TestReuseDeadCachedConnFailsOverToFreshDial(t *testing.T) {
	fake := fakesmtp.Start(t, fakesmtp.Script{
		Caps:   backendCaps(),
		OnRSET: []fakesmtp.Step{{Action: fakesmtp.ActDropConn}},
	})
	cfg := m1Config(fake.Addr())
	cfg.Pools[0].ReuseEnvelopes = 3
	addr := serve(t, cfg, newRelay(cfg))

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	wantQueued(t, 0, c.SendMsg(0)) // dials and stashes the one leg
	wantQueued(t, 1, c.SendMsg(1)) // revalidation kills it; transparent fresh dial

	c.Send("QUIT")
	c.Expect("221")

	if got := fake.DialCount(); got != 2 {
		t.Errorf("DialCount = %d, want 2: envelope 2's dead cached leg forces a fresh dial", got)
	}
}

// TestReuseEnvelopesOmittedDialsFreshPerEnvelope is scenario 3: the
// regression pin for the default. m1Config never sets ReuseEnvelopes, so
// it stays the zero value (disabled) — two envelopes on one client
// session must still dial twice, exactly as before this feature existed.
func TestReuseEnvelopesOmittedDialsFreshPerEnvelope(t *testing.T) {
	fake := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	cfg := m1Config(fake.Addr()) // ReuseEnvelopes omitted -> 0, reuse disabled
	addr := serve(t, cfg, newRelay(cfg))

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	wantQueued(t, 0, c.SendMsg(0))
	wantQueued(t, 1, c.SendMsg(1))

	c.Send("QUIT")
	c.Expect("221")

	if got := fake.DialCount(); got != 2 {
		t.Errorf("DialCount = %d, want 2: reuse_envelopes omitted dials fresh per envelope", got)
	}
}

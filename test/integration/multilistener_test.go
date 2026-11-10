//go:build integration

package integration

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/proxy"
	"github.com/littlebugger/bifrost/internal/smtpdrv"
)

// Task 7: the multi-listener proof. Tasks 1-6 land the pieces (listener
// blocks, per-listener Session referent, rule.listener routing, the
// per-transaction RequiredCaps vs. prober-union capability split) as unit
// tests; this file is the only place two listeners run in the same
// process, over real TCP, against real fakesmtp backends, proving the
// three things the design doc's Testing section calls out end to end:
// port-split routing, per-transaction capability checking, and listener-
// scoped auth isolation.

// serveListener runs proxy.Serve (the real accept loop Task 3 wrote, not
// m1_test.go's hand-rolled stand-in) over its own net.Listener for one
// config.Listener, torn down by t.Cleanup exactly like
// internal/proxy/listener_test.go's own runServe: cancel ctx, then wait
// for Serve's return. Multiple calls against the same cfg/h share one
// Relay/config/holder, which is the behavior under test — only the
// *config.Listener each is bound to differs.
func serveListener(t *testing.T, cfg *config.Config, lst *config.Listener, tlsCfg *tls.Config, h proxy.TxnHandler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- proxy.Serve(ctx, ln, cfg, lst, tlsCfg, h, slog.New(slog.DiscardHandler), nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve(listener %q) returned %v, want nil", lst.Name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Serve(listener %q) did not return after ctx cancel", lst.Name)
		}
	})
	return addr
}

// dualListenerConfig is two listeners, "a" and "b", each routed by name
// (rule.listener) to its own single-server pool — the config shape
// scenarios 1 and 2 share, differing only in each listener's advertised
// capability set and which fake backend answers it.
func dualListenerConfig(capsA, capsB []string, addrA, addrB string) *config.Config {
	return &config.Config{
		Defaults: config.Defaults{Timeouts: m1Timeouts()},
		Listeners: []config.Listener{
			{Name: "a", Hostname: "bifrost.test", Capabilities: capsA},
			{Name: "b", Hostname: "bifrost.test", Capabilities: capsB},
		},
		Pools: []config.Pool{
			{
				Name: "pool-a", Balance: "roundrobin", BackendTLS: "none", EhloName: "bifrost.test",
				Servers: []config.Server{{Name: "fa", Address: addrA, Weight: 1}},
			},
			{
				Name: "pool-b", Balance: "roundrobin", BackendTLS: "none", EhloName: "bifrost.test",
				Servers: []config.Server{{Name: "fb", Address: addrB, Weight: 1}},
			},
		},
		Routing: config.Routing{
			Rules: []config.RoutingRule{
				{Listener: []string{"a"}, Pool: "pool-a"},
				{Listener: []string{"b"}, Pool: "pool-b"},
			},
			DefaultPool: "pool-a",
		},
	}
}

// TestMultiListenerPortSplitRouting is scenario 1: two listeners, two
// pools, two fakes, one process. A message sent into listener a's port
// must land only on fake a; a message into listener b's port only on
// fake b — rule.listener's AND-combined match, proven from outside by
// each fake's own DialCount, not by inspecting the router directly.
func TestMultiListenerPortSplitRouting(t *testing.T) {
	fakeA := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	fakeB := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})

	caps := []string{"PIPELINING", "8BITMIME"}
	cfg := dualListenerConfig(caps, caps, fakeA.Addr(), fakeB.Addr())
	relay := newRelay(cfg) // one Relay/config/holder shared by both listeners

	addrA := serveListener(t, cfg, &cfg.Listeners[0], nil, relay)
	addrB := serveListener(t, cfg, &cfg.Listeners[1], nil, relay)

	cA := smtpdrv.Dial(t, addrA)
	cA.Expect("220")
	cA.Send("EHLO client.example")
	cA.Expect("250")
	sendOneMessage(t, cA, "sender@test.example")
	cA.Send("QUIT")
	cA.Expect("221")

	if got := fakeA.DialCount(); got != 1 {
		t.Errorf("fake a DialCount = %d, want 1", got)
	}
	if got := fakeB.DialCount(); got != 0 {
		t.Errorf("fake b DialCount = %d, want 0: listener a's traffic must never reach fake b", got)
	}

	cB := smtpdrv.Dial(t, addrB)
	cB.Expect("220")
	cB.Send("EHLO client.example")
	cB.Expect("250")
	sendOneMessage(t, cB, "sender@test.example")
	cB.Send("QUIT")
	cB.Expect("221")

	if got := fakeA.DialCount(); got != 1 {
		t.Errorf("fake a DialCount = %d, want 1 (unchanged by listener b's traffic)", got)
	}
	if got := fakeB.DialCount(); got != 1 {
		t.Errorf("fake b DialCount = %d, want 1", got)
	}
}

// TestMultiListenerPerTransactionRequiredCaps is scenario 2: the
// capability referent design decision (listener's RequiredCaps checked
// per transaction against the arriving listener, not some global set).
// Both listeners advertise 8BITMIME; fake a has it, fake b does not. A
// message through listener a relays normally; the same message through
// listener b never finds a compatible backend and gets the closed-enum
// 451 synth attach.go falls back to — the exact reply auth_test.go's
// TestAuthBackendCredsRejectedYieldsNoBackend asserts for the same
// reason (a backend was tried and rejected, not simply absent).
func TestMultiListenerPerTransactionRequiredCaps(t *testing.T) {
	fakeA := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})          // PIPELINING, 8BITMIME, SIZE
	fakeB := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING"}}) // missing 8BITMIME

	caps := []string{"PIPELINING", "8BITMIME"} // both listeners advertise it
	cfg := dualListenerConfig(caps, caps, fakeA.Addr(), fakeB.Addr())
	relay := newRelay(cfg)

	addrA := serveListener(t, cfg, &cfg.Listeners[0], nil, relay)
	addrB := serveListener(t, cfg, &cfg.Listeners[1], nil, relay)

	cA := smtpdrv.Dial(t, addrA)
	cA.Expect("220")
	cA.Send("EHLO client.example")
	cA.Expect("250")
	sendOneMessage(t, cA, "sender@test.example") // fake a satisfies listener a's caps
	cA.Send("QUIT")
	cA.Expect("221")

	cB := smtpdrv.Dial(t, addrB)
	cB.Expect("220")
	cB.Send("EHLO client.example")
	cB.Expect("250")
	cB.Send("MAIL FROM:<sender@test.example>")
	reply := cB.Expect("451")
	if want := "451 4.4.1 No backend available, try again later"; reply.Lines[0] != want {
		t.Fatalf("MAIL on listener b (fake b missing 8BITMIME) = %q, want the closed-enum %q", reply.Lines[0], want)
	}
	cB.Send("QUIT")
	cB.Expect("221")

	if got := fakeA.DialCount(); got != 1 {
		t.Errorf("fake a DialCount = %d, want 1: listener a's traffic unaffected by listener b's incompatible backend", got)
	}
	if got := fakeB.DialCount(); got != 1 {
		t.Errorf("fake b DialCount = %d, want 1: the backend really was tried (and rejected), not simply skipped", got)
	}
}

// listenerBAuthUser is scenario 3's "different user": valid on listener
// b, never sent by either client leg in this test, kept only so b's own
// Auth.Users isn't suspiciously empty.
const (
	listenerBAuthUser = "mallory"
	listenerBAuthSalt = "different-salt"
	listenerBAuthPass = "not-alices-password"
)

// TestMultiListenerAuthIsolation is scenario 3: two listeners with
// independent auth stores — a user valid on listener a must fail on
// listener b. Listener b is given allow_cleartext instead of its own
// STARTTLS identity (the brief's two options for getting AUTH onto b's
// wire): simpler than running a second TLS handshake, since this
// scenario is about store isolation, not re-proving STARTTLS itself
// (auth_test.go already covers that in full).
func TestMultiListenerAuthIsolation(t *testing.T) {
	fake := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	cfg := &config.Config{
		Defaults: config.Defaults{Timeouts: m1Timeouts()},
		Listeners: []config.Listener{
			{
				Name:         "a",
				Hostname:     "bifrost.test",
				Capabilities: []string{"PIPELINING", "8BITMIME", "STARTTLS"},
				Auth: &config.ListenerAuth{
					Users: []config.AuthUser{authTestUser(clientAuthUser, clientAuthSalt, clientAuthPass)},
				},
			},
			{
				Name:         "b",
				Hostname:     "bifrost.test",
				Capabilities: []string{"PIPELINING", "8BITMIME"},
				Auth: &config.ListenerAuth{
					AllowCleartext: true,
					Users:          []config.AuthUser{authTestUser(listenerBAuthUser, listenerBAuthSalt, listenerBAuthPass)},
				},
			},
		},
		Pools:   []config.Pool{{Name: "p", Balance: "roundrobin", BackendTLS: "none", EhloName: "bifrost.test", Servers: []config.Server{{Name: "s0", Address: fake.Addr(), Weight: 1}}}},
		Routing: config.Routing{DefaultPool: "p"},
	}
	relay := newRelay(cfg)

	listenerTLS := fakesmtp.TestCert(t)
	addrA := serveListener(t, cfg, &cfg.Listeners[0], listenerTLS, relay)
	addrB := serveListener(t, cfg, &cfg.Listeners[1], nil, relay)

	// Listener a: STARTTLS required before AUTH is even advertised
	// (auth_test.go's own rule), then the client's own user succeeds.
	cA := smtpdrv.Dial(t, addrA)
	cA.Expect("220")
	cA.Send("EHLO client.example")
	cA.Expect("250")
	cA.StartTLS(listenerTLS)
	cA.Send("EHLO client.example")
	cA.Expect("250")
	cA.Send("AUTH PLAIN " + authPlainPayload(clientAuthUser, clientAuthPass))
	if reply := cA.Expect("235"); reply.Lines[0] != "235 2.7.0 Authentication succeeded" {
		t.Fatalf("listener a AUTH reply = %q, want the exact 235 text", reply.Lines[0])
	}
	cA.Send("QUIT")
	cA.Expect("221")

	// Listener b: allow_cleartext, its own (different) user store — the
	// same credentials that just succeeded on a are simply unknown here.
	cB := smtpdrv.Dial(t, addrB)
	cB.Expect("220")
	cB.Send("EHLO client.example")
	cB.Expect("250")
	cB.Send("AUTH PLAIN " + authPlainPayload(clientAuthUser, clientAuthPass))
	if reply := cB.Expect("535"); reply.Lines[0] != "535 5.7.8 Authentication credentials invalid" {
		t.Fatalf("listener b AUTH reply = %q, want the exact 535 text (store isolation)", reply.Lines[0])
	}
	cB.Send("QUIT")
	cB.Expect("221")

	if got := fake.DialCount(); got != 0 {
		t.Errorf("backend DialCount = %d, want 0: neither leg ever reached MAIL", got)
	}
}

//go:build integration

package proxy

import (
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/fakesmtp"
)

// The candidate walk: ordered, silent, and only ever silent while the
// client has seen nothing about the batch being replayed (PROJECT.md's
// failover row and decision D13).

func TestFailoverToSecondCandidate(t *testing.T) {
	first := relayFake(t, fakesmtp.Script{})
	second := relayFake(t, fakesmtp.Script{OnMAIL: []fakesmtp.Step{{Reply: "250 2.1.0 second speaking"}}})
	first.SetDown(fakesmtp.DownAcceptThenRST)

	f := newRelayClient(t, relayConfig(first.Addr(), second.Addr()))
	f.send("MAIL FROM:<a@b.example>")

	// Only the second backend's verdict is ever on the wire: the failover
	// is silent, so the client cannot tell it happened.
	f.expect("250 2.1.0 second speaking")

	if got := first.DialCount(); got != 2 {
		t.Errorf("first candidate DialCount = %d, want 2 attempts before moving on", got)
	}
	if got := second.DialCount(); got != 1 {
		t.Errorf("second candidate DialCount = %d, want 1", got)
	}
	wantLines(t, second, 0, "MAIL FROM:<a@b.example>\r\n")
}

func TestConnectRetriesPerCandidate(t *testing.T) {
	// The "× 2 attempts" half of the connect budget: a backend that
	// accepts and then says nothing burns the handshake budget twice.
	srv := relayFake(t, fakesmtp.Script{})
	srv.SetDown(fakesmtp.DownAcceptThenHang)

	cfg := relayConfig(srv.Addr())
	cfg.Defaults.Timeouts.BackendHandshake = 150 * time.Millisecond
	f := newRelayClient(t, cfg)

	f.send("MAIL FROM:<a@b.example>")
	f.expect("451 4.4.1 No backend available, try again later")

	if got := srv.DialCount(); got != 2 {
		t.Errorf("DialCount = %d, want exactly 2 attempts", got)
	}
}

func TestFailoverExhausted451(t *testing.T) {
	first := relayFake(t, fakesmtp.Script{})
	second := relayFake(t, fakesmtp.Script{})
	first.SetDown(fakesmtp.DownListenerClosed)
	second.SetDown(fakesmtp.DownListenerClosed)

	f := newRelayClient(t, relayConfig(first.Addr(), second.Addr()))
	f.send("MAIL FROM:<a@b.example>")
	f.expect("451 4.4.1 No backend available, try again later")

	// Transaction-scoped, not connection-scoped: the session is still
	// there, and a retry works the moment a backend does.
	second.SetUp()
	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	if got := second.DialCount(); got != 1 {
		t.Errorf("recovered candidate DialCount = %d, want 1", got)
	}
}

func TestFailoverQueueAnswers(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{})
	srv.SetDown(fakesmtp.DownListenerClosed)

	f := newRelayClient(t, relayConfig(srv.Addr()))

	// The queue holds only MAIL + consecutive RCPTs + at most one DATA
	// (RFC 2920 sync-point batching); every one of them is answered, in
	// command order.
	f.raw("MAIL FROM:<a@b.example>\r\nRCPT TO:<one@c.example>\r\nRCPT TO:<two@c.example>\r\nDATA\r\n")
	for i := 0; i < 4; i++ {
		f.expect("451 4.4.1 No backend available, try again later")
	}

	// DATA was refused, so no body follows and the session is still in
	// command sync.
	f.send("NOOP")
	f.expect("250 2.0.0 OK")
}

func TestNoFailoverAfterFirstRelayedByte(t *testing.T) {
	// The invariant: once one byte about this batch has reached the
	// client, the transaction can never be replayed elsewhere.
	first := relayFake(t, fakesmtp.Script{OnRCPT: []fakesmtp.Step{{Action: fakesmtp.ActRST}}})
	second := relayFake(t, fakesmtp.Script{})

	f := newRelayClient(t, relayConfig(first.Addr(), second.Addr()))
	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<one@c.example>")
	f.expect("451 4.4.1 Backend connection lost")

	if got := second.DialCount(); got != 0 {
		t.Errorf("second candidate DialCount = %d, want 0: the transaction was already visible", got)
	}
}

func TestHealthSignalsEmitted(t *testing.T) {
	down := relayFake(t, fakesmtp.Script{})
	up := relayFake(t, fakesmtp.Script{})
	down.SetDown(fakesmtp.DownAcceptThenRST)

	f := newRelayClient(t, relayConfig(down.Addr(), up.Addr()))
	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")
	f.send("QUIT")
	f.expect("221 2.0.0 Bye")
	f.expectClosed()

	dial, trans, succeeds := f.sig.counts()
	if want := []string{"s0", "s0"}; !slices.Equal(dial, want) {
		t.Errorf("DialFailure signals = %v, want %v", dial, want)
	}
	if len(trans) != 0 {
		t.Errorf("TransportError signals = %v, want none", trans)
	}
	if want := []string{"s1"}; !slices.Equal(succeeds, want) {
		t.Errorf("Success signals = %v, want %v", succeeds, want)
	}
	if open, total := f.leases.state(); open != 0 || total != 1 {
		t.Errorf("leases = (open %d, total %d), want (0, 1)", open, total)
	}
}

func TestAmbiguousPoolCandidateSkipped(t *testing.T) {
	// A candidate the loaded config cannot resolve to exactly one pool has
	// no known backend_tls mode. The same address may legally sit in two
	// pools with different modes, so guessing could send mail in the clear
	// that the operator configured to be encrypted: the candidate is
	// skipped and the walk moves on.
	shared := relayFake(t, fakesmtp.Script{})
	healthy := relayFake(t, fakesmtp.Script{})

	cfg := relayConfig(shared.Addr(), healthy.Addr())
	cfg.Pools = append(cfg.Pools, config.Pool{
		Name: "q", BackendTLS: "starttls-verify", EhloName: "bifrost.test",
		Servers: []config.Server{{Name: "q0", Address: shared.Addr(), Weight: 1}},
	})

	// A stale server object: same address, but identity matches nothing in
	// the config any more, exactly as after a reload.
	stale := &config.Server{Name: "stale", Address: shared.Addr(), Weight: 1}
	f := newRelayClientPick(t, cfg, slog.New(slog.DiscardHandler),
		[]*config.Server{stale, &cfg.Pools[0].Servers[1]})

	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")

	if got := shared.DialCount(); got != 0 {
		t.Errorf("ambiguous candidate DialCount = %d, want 0", got)
	}
	if got := healthy.DialCount(); got != 1 {
		t.Errorf("next candidate DialCount = %d, want 1", got)
	}
}

func TestIncompatibleCandidateNotRetried(t *testing.T) {
	// A capability set will not change between two attempts milliseconds
	// apart, so an incompatible backend gets one attempt, not two — the
	// walk moves on instead of doubling the noise on a backend that cannot
	// serve this listener.
	incompatible := relayFake(t, fakesmtp.Script{Caps: []string{"PIPELINING", "SIZE 20971520"}})
	healthy := relayFake(t, fakesmtp.Script{})

	f := newRelayClient(t, relayConfig(incompatible.Addr(), healthy.Addr()))
	f.send("MAIL FROM:<a@b.example>")
	f.expect("250 2.1.0 OK")

	if got := incompatible.DialCount(); got != 1 {
		t.Errorf("incompatible candidate DialCount = %d, want 1", got)
	}
	if got := healthy.DialCount(); got != 1 {
		t.Errorf("next candidate DialCount = %d, want 1", got)
	}
	dial, _, _ := f.sig.counts()
	if want := []string{"s0"}; !slices.Equal(dial, want) {
		t.Errorf("DialFailure signals = %v, want %v", dial, want)
	}
}

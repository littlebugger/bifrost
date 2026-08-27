//go:build integration

package proxy

import (
	"log/slog"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// capPickAt1 is a PickFunc that hands back srv while leases has nothing
// open, and ErrAllSaturated once it does -- the max_transactions == 1
// boundary this file needs to prove the relay maps to the right reply.
// The real filter (InFlight vs. resolved MaxTransactions) is
// internal/balance's own job, proved in its package (maxtxn_test.go):
// internal/proxy must never import internal/balance, so this file's
// double only has to reproduce the signal balance.Router.Pick sends
// across that boundary, not recompute it.
func capPickAt1(leases *leaseCounter, srv *config.Server) PickFunc {
	return func(TxnMeta) ([]*config.Server, error) {
		if open, _ := leases.state(); open >= 1 {
			return nil, ErrAllSaturated
		}
		return []*config.Server{srv}, nil
	}
}

// TestAllSaturated451SessionSurvives is the epic-05 review's design
// resolution: a pool where every eligible server is at its
// max_transactions cap answers the contract's 451 4.3.2 (RplAllBusy)
// rather than the unhealthy-empty 451 4.4.1 -- and the session it
// arrives on survives to retry successfully once the cap frees up.
func TestAllSaturated451SessionSurvives(t *testing.T) {
	srv := relayFake(t, fakesmtp.Script{OnEOD: []fakesmtp.Step{{Action: fakesmtp.ActHang}}})

	cfg := relayConfig(srv.Addr())
	cfg.Defaults.Timeouts.BackendFinalDot = 300 * time.Millisecond
	h := &config.Holder{}
	h.Swap(cfg)

	leases := &leaseCounter{}
	pick := capPickAt1(leases, serversOf(cfg)[0])
	relay := NewRelay(pick, h, slog.New(slog.DiscardHandler), nil, leases.lease)

	c1 := newTestClient(t, cfg, nil, relay)
	c1.expect("220 bifrost.test ESMTP")
	c1.send("EHLO client1.example")
	c1.reply()

	c1.send("MAIL FROM:<a@b.example>")
	c1.expect("250 2.1.0 OK")
	c1.send("RCPT TO:<r@b.example>")
	c1.expect("250 2.1.5 OK")
	c1.send("DATA")
	c1.expect("354 Start mail input; end with <CRLF>.<CRLF>")
	c1.raw("body\r\n.\r\n")

	// The fake is now hanging on the EOD reply, holding the lease open.
	// Read c1's eventual verdict off the main goroutine so this one can
	// drive c2 in the meantime.
	c1Verdict := make(chan []string, 1)
	go func() { c1Verdict <- c1.reply() }()

	c2 := newTestClient(t, cfg, nil, relay)
	c2.expect("220 bifrost.test ESMTP")
	c2.send("EHLO client2.example")
	c2.reply()

	c2.send("MAIL FROM:<x@y.example>")
	c2.expect("451 4.3.2 All backends busy, try again later")

	select {
	case lines := <-c1Verdict:
		if len(lines) != 1 || lines[0] != "451 4.4.2 Backend timeout" {
			t.Fatalf("first transaction's verdict = %v, want the backend-timeout 451 that frees the lease", lines)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first transaction never got a verdict")
	}

	// The lease is free again: the retry lands on the same (still up)
	// backend and succeeds -- the session survived the 451.
	c2.send("MAIL FROM:<x@y.example>")
	c2.expect("250 2.1.0 OK")

	c1.send("QUIT")
	c1.expect("221 2.0.0 Bye")
	c2.send("QUIT")
	c2.expect("221 2.0.0 Bye")
}

// TestLeaseReleasedOnEveryPath proves the lease's release fires on every
// path that can end an attached transaction: a relayed verdict, RSET,
// the backend dying mid-transaction, the client aborting, and QUIT. Each
// subtest attaches exactly one transaction and checks the lease is back
// at 0-open once that path has run its course.
func TestLeaseReleasedOnEveryPath(t *testing.T) {
	newFixture := func(t *testing.T, s fakesmtp.Script) (*testClient, *leaseCounter) {
		t.Helper()
		srv := relayFake(t, s)
		cfg := relayConfig(srv.Addr())
		h := &config.Holder{}
		h.Swap(cfg)
		leases := &leaseCounter{}
		pick := func(TxnMeta) ([]*config.Server, error) { return serversOf(cfg), nil }
		relay := NewRelay(pick, h, slog.New(slog.DiscardHandler), nil, leases.lease)
		c := newTestClient(t, cfg, nil, relay)
		c.expect("220 bifrost.test ESMTP")
		c.send("EHLO client.example")
		c.reply()
		return c, leases
	}

	assertReleased := func(t *testing.T, leases *leaseCounter) {
		t.Helper()
		if open, _ := leases.state(); open != 0 {
			t.Errorf("lease open count = %d, want 0", open)
		}
	}

	t.Run("verdict relayed", func(t *testing.T) {
		c, leases := newFixture(t, fakesmtp.Script{})
		c.send("MAIL FROM:<a@b.example>")
		c.expect("250 2.1.0 OK")
		c.send("RCPT TO:<r@b.example>")
		c.expect("250 2.1.5 OK")
		c.send("DATA")
		c.expect("354 Start mail input; end with <CRLF>.<CRLF>")
		c.raw("body\r\n.\r\n")
		c.expect("250 2.0.0 OK: queued")
		// QUIT round-trip: guarantees the detach a relayed verdict
		// triggers has already run (the client can see its own 250 a
		// moment before the server-side release actually executes).
		c.send("QUIT")
		c.expect("221 2.0.0 Bye")
		assertReleased(t, leases)
	})

	t.Run("RSET detach", func(t *testing.T) {
		c, leases := newFixture(t, fakesmtp.Script{})
		c.send("MAIL FROM:<a@b.example>")
		c.expect("250 2.1.0 OK")
		c.send("RSET")
		c.expect("250 2.0.0 OK")
		c.send("QUIT")
		c.expect("221 2.0.0 Bye")
		assertReleased(t, leases)
	})

	t.Run("backend death", func(t *testing.T) {
		c, leases := newFixture(t, fakesmtp.Script{
			OnRCPT: []fakesmtp.Step{{Action: fakesmtp.ActDropConn}},
		})
		c.send("MAIL FROM:<a@b.example>")
		c.expect("250 2.1.0 OK")
		c.send("RCPT TO:<r@b.example>")
		c.expect("451 4.4.1 Backend connection lost")
		c.send("QUIT")
		c.expect("221 2.0.0 Bye")
		assertReleased(t, leases)
	})

	t.Run("client abort", func(t *testing.T) {
		c, leases := newFixture(t, fakesmtp.Script{})
		c.send("MAIL FROM:<a@b.example>")
		c.expect("250 2.1.0 OK")
		c.send("RCPT TO:<r@b.example>")
		c.expect("250 2.1.5 OK")
		c.send("DATA")
		c.expect("354 Start mail input; end with <CRLF>.<CRLF>")
		c.raw("partial body, no terminator ever sent")
		_ = c.conn.Close() // hard abort mid-DATA (RFC 5321 3.8)
		_ = c.wait()       // joins the session goroutine before the lease is read
		assertReleased(t, leases)
	})

	t.Run("QUIT", func(t *testing.T) {
		c, leases := newFixture(t, fakesmtp.Script{})
		c.send("MAIL FROM:<a@b.example>")
		c.expect("250 2.1.0 OK")
		c.send("QUIT")
		c.expect("221 2.0.0 Bye")
		assertReleased(t, leases)
	})
}

// denyLease returns a LeaseFunc that denies every server named in deny
// (a nil release -- the attach walk's "lost the race for the last
// max_transactions slot" signal, see LeaseFunc's own doc) and grants
// everything else a real, if inert, release. Deterministic coverage for
// attach.go's sawSaturated branch: the chaos scenario is the only other
// place that path gets exercised, and only probabilistically.
func denyLease(deny ...string) LeaseFunc {
	denied := map[string]bool{}
	for _, n := range deny {
		denied[n] = true
	}
	return func(srv *config.Server) func() {
		if denied[srv.Name] {
			return nil
		}
		return func() {}
	}
}

// TestAttachWalksOnAfterLeaseDenial: the first candidate's lease is
// denied right after a successful dial -- its leg is aborted, unused
// (the fake sees only the handshake EHLO, never a MAIL) -- and the walk
// continues to land on the second candidate exactly as it would for a
// dial failure.
func TestAttachWalksOnAfterLeaseDenial(t *testing.T) {
	fakeA := relayFake(t, fakesmtp.Script{})
	fakeB := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(fakeA.Addr(), fakeB.Addr())
	h := &config.Holder{}
	h.Swap(cfg)
	picks := &pickStub{servers: serversOf(cfg)} // [s0, s1] in candidate order
	relay := NewRelay(picks.pick, h, slog.New(slog.DiscardHandler), nil, denyLease("s0"))

	c := newTestClient(t, cfg, nil, relay)
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	c.send("MAIL FROM:<a@b.example>")
	c.expect("250 2.1.0 OK")

	if got := fakeA.CmdCount("EHLO"); got != 1 {
		t.Errorf("fakeA (denied) saw %d EHLO, want 1 (the dial handshake happened)", got)
	}
	if got := fakeA.CmdCount("MAIL"); got != 0 {
		t.Errorf("fakeA (denied) saw %d MAIL, want 0 (aborted before anything was sent)", got)
	}
	if got := fakeB.CmdCount("MAIL"); got != 1 {
		t.Errorf("fakeB saw %d MAIL, want 1 (the walk landed here)", got)
	}

	c.send("QUIT")
	c.expect("221 2.0.0 Bye")
}

// TestAttachAllCandidatesDeniedIsAllBusy: every candidate's lease is
// denied -- nothing is ever sent to either backend -- and the client
// gets the saturation reply (451 4.3.2), not the unhealthy-empty one
// (451 4.4.1), with the session surviving to QUIT.
func TestAttachAllCandidatesDeniedIsAllBusy(t *testing.T) {
	fakeA := relayFake(t, fakesmtp.Script{})
	fakeB := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(fakeA.Addr(), fakeB.Addr())
	h := &config.Holder{}
	h.Swap(cfg)
	picks := &pickStub{servers: serversOf(cfg)}
	relay := NewRelay(picks.pick, h, slog.New(slog.DiscardHandler), nil, denyLease("s0", "s1"))

	c := newTestClient(t, cfg, nil, relay)
	c.expect("220 bifrost.test ESMTP")
	c.send("EHLO client.example")
	c.reply()

	c.send("MAIL FROM:<a@b.example>")
	c.expect("451 4.3.2 All backends busy, try again later")

	for _, fake := range []*fakesmtp.Server{fakeA, fakeB} {
		if got := fake.CmdCount("MAIL"); got != 0 {
			t.Errorf("fake saw %d MAIL, want 0 (every candidate denied before anything was sent)", got)
		}
	}

	c.send("QUIT")
	c.expect("221 2.0.0 Bye")
}

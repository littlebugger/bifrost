//go:build chaos

package chaos

import (
	"strings"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/fakesmtp"
	"github.com/revolee/bifrost/internal/health"
	"github.com/revolee/bifrost/internal/proxy"
)

// Epic-10 Task 5, reply-shaped scenarios: what the client sees when a
// backend answers slowly, partially, rudely, or not at all.

// TestChaosSlowBackendDrip: the backend dribbles its MAIL reply one byte
// at a time, slower than the reply budget. The budget is armed once for the
// whole reply (a backend must not be able to extend it byte by byte), so
// this is a timeout, not an eventual success.
func TestChaosSlowBackendDrip(t *testing.T) {
	f := fakesmtp.Start(t, fakesmtp.Script{
		Caps:   backendCaps(),
		OnMAIL: []fakesmtp.Step{{Reply: "250 2.1.0 OK", Drip: 40 * time.Millisecond}},
	})
	cfg := poolConfig(timeoutsWith(func(tt *config.Timeouts) {
		tt.BackendMailReply = 200 * time.Millisecond
	}), "none", f.Addr())
	s := newStack(t, cfg)

	c := openSession(t, s.addr, "drip")
	if c == nil {
		return
	}
	got, err := c.message("drip")
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	if want := synth(proxy.RplBackendTimeout); got != want {
		t.Errorf("verdict for a dripped reply = %q, want %q", got, want)
	}
	// Not one byte of the half-delivered reply reached the client: a reply
	// is relayed line by line, never character by character.
	if got, err := c.cmd("NOOP"); err != nil || got != synth(proxy.RplOK) {
		t.Errorf("NOOP after the drip = %q (%v), want %q", got, err, synth(proxy.RplOK))
	}
}

// TestChaosPipelinedFailover: the client pipelines MAIL+RCPT+DATA in one
// segment and the backend dies right after accepting the MAIL. The MAIL's
// 250 has already reached the client, so silent failover is off the table
// (that is the invariant) — the remaining two commands are answered 451,
// in command order, and the NEXT transaction fails over to the healthy
// backend.
func TestChaosPipelinedFailover(t *testing.T) {
	dying := fakesmtp.Start(t, fakesmtp.Script{
		Caps:   backendCaps(),
		OnRCPT: []fakesmtp.Step{{Action: fakesmtp.ActDropConn}},
	})
	healthy := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	s := newStack(t, poolConfig(chaosTimeouts(), "none", dying.Addr(), healthy.Addr()))

	c := openSession(t, s.addr, "pipelined")
	if c == nil {
		return
	}
	if err := c.write("MAIL FROM:<sender@chaos.test>\r\nRCPT TO:<rcpt@chaos.test>\r\nDATA\r\n"); err != nil {
		t.Fatalf("pipeline: %v", err)
	}

	first, err := c.reply()
	if err != nil {
		t.Fatalf("MAIL reply: %v", err)
	}
	if !strings.HasPrefix(first, "250") {
		t.Fatalf("MAIL reply = %q, want the backend's 250", first)
	}
	for i, what := range []string{"RCPT", "DATA"} {
		got, err := c.reply()
		if err != nil {
			t.Fatalf("%s reply: %v", what, err)
		}
		if want := synth(proxy.RplBackendLost); got != want {
			t.Errorf("reply %d (%s) = %q, want %q in command order", i+2, what, got, want)
		}
	}

	// The next transaction is a fresh pick, and the healthy backend takes
	// it: failover happens between transactions, never mid-batch once a
	// byte has reached the client.
	got, err := c.message("after-failover")
	if err != nil {
		t.Fatalf("message after the failure: %v", err)
	}
	if !strings.HasPrefix(got, "250") {
		t.Errorf("verdict after failover = %q, want a backend 250", got)
	}
	if n := recordedBodies(healthy)[wantBody("after-failover")]; n != 1 {
		t.Errorf("the healthy backend recorded the retry %d times, want 1", n)
	}
}

// TestChaosAllRCPTRejected: every recipient is rejected 5xx and the client
// sends DATA anyway. Bifrost must relay DATA to the backend and hand back
// the backend's own verdict verbatim — never synthesize a 354 and never
// invent a 503 of its own — and the per-recipient rejections must not latch
// (D12: one 550 may not poison the rest of the session).
func TestChaosAllRCPTRejected(t *testing.T) {
	const (
		rcptReply = "550 5.1.1 <rcpt@chaos.test>: unknown user"
		dataReply = "554 5.5.1 No valid recipients given"
	)
	f := fakesmtp.Start(t, fakesmtp.Script{
		Caps:   backendCaps(),
		OnRCPT: []fakesmtp.Step{{Reply: rcptReply}},
		OnDATA: []fakesmtp.Step{{Reply: dataReply}},
	})
	s := newStack(t, poolConfig(chaosTimeouts(), "none", f.Addr()))

	c := openSession(t, s.addr, "allrejected")
	if c == nil {
		return
	}
	if got, err := c.cmd("MAIL FROM:<sender@chaos.test>"); err != nil || !strings.HasPrefix(got, "250") {
		t.Fatalf("MAIL reply = %q (%v), want 250", got, err)
	}
	for i := 0; i < 2; i++ {
		got, err := c.cmd("RCPT TO:<rcpt@chaos.test>")
		if err != nil {
			t.Fatalf("RCPT %d: %v", i, err)
		}
		if got != rcptReply {
			t.Errorf("RCPT %d reply = %q, want the backend's own %q (verbatim, and no latching)", i, got, rcptReply)
		}
	}
	got, err := c.cmd("DATA")
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	if got != dataReply {
		t.Errorf("DATA reply = %q, want the backend's own %q — never a synthesized 354 or 503", got, dataReply)
	}

	// The session is untouched by all of it.
	if got, err := c.cmd("NOOP"); err != nil || got != synth(proxy.RplOK) {
		t.Errorf("NOOP after the rejections = %q (%v), want %q", got, err, synth(proxy.RplOK))
	}
	if got, err := c.cmd("MAIL FROM:<sender2@chaos.test>"); err != nil || !strings.HasPrefix(got, "250") {
		t.Errorf("MAIL after the rejections = %q (%v), want 250 (per-RCPT verdicts never latch)", got, err)
	}
}

// TestChaosMultilineEverything: multiline banner, multiline EHLO, and
// multiline MAIL/RCPT/end-of-data replies, with the verdict delayed until
// close to the client-idle budget. Every line of every relayed reply must
// reach the client byte for byte, in order.
func TestChaosMultilineEverything(t *testing.T) {
	var (
		mailReply = []string{"250-sender accepted", "250 2.1.0 continue"}
		rcptReply = []string{"250-recipient accepted", "250-will attempt delivery", "250 2.1.5 ok"}
		eodReply  = []string{"250-queued as ABC123", "250-thanks", "250 2.0.0 OK: queued"}
	)
	f := fakesmtp.Start(t, fakesmtp.Script{
		Banner: fakesmtp.Step{Reply: "220-fakesmtp, one moment\r\n220 fakesmtp ESMTP ready"},
		Caps:   backendCaps(),
		OnMAIL: []fakesmtp.Step{{Reply: strings.Join(mailReply, "\r\n")}},
		OnRCPT: []fakesmtp.Step{{Reply: strings.Join(rcptReply, "\r\n")}},
		// 300ms against a 400ms client-idle budget: as late as the verdict
		// can be without the client leg's own timer having a say.
		OnEOD: []fakesmtp.Step{{Reply: strings.Join(eodReply, "\r\n"), Delay: 300 * time.Millisecond}},
	})
	cfg := poolConfig(timeoutsWith(func(tt *config.Timeouts) {
		tt.ClientIdle = 400 * time.Millisecond
	}), "none", f.Addr())
	s := newStack(t, cfg)

	c := openSession(t, s.addr, "multiline")
	if c == nil {
		return
	}
	if err := c.write("MAIL FROM:<sender@chaos.test>\r\n"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	assertLines(t, c, "MAIL", mailReply)
	if err := c.write("RCPT TO:<rcpt@chaos.test>\r\n"); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	assertLines(t, c, "RCPT", rcptReply)
	if got, err := c.cmd("DATA"); err != nil || !strings.HasPrefix(got, "354") {
		t.Fatalf("DATA reply = %q (%v), want 354", got, err)
	}
	if err := c.write(bodyFor("multiline")); err != nil {
		t.Fatalf("body: %v", err)
	}
	assertLines(t, c, "end-of-data", eodReply)

	if n := recordedBodies(f)[wantBody("multiline")]; n != 1 {
		t.Errorf("backend recorded the message %d times, want 1", n)
	}
}

// assertLines reads one whole reply and compares every line with want.
func assertLines(t *testing.T, c *conn, what string, want []string) {
	t.Helper()
	got, err := c.lines()
	if err != nil {
		t.Fatalf("%s reply: %v", what, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s reply = %q, want %q", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s reply line %d = %q, want %q", what, i, got[i], want[i])
		}
	}
}

// TestChaosHalfOpenBackend: the backend accepts TCP and then says nothing
// at all. The handshake budget — not an unbounded wait — ends the attempt,
// the client gets 451 4.4.1, and the active checker reaches the same
// verdict independently.
func TestChaosHalfOpenBackend(t *testing.T) {
	f := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	f.SetDown(fakesmtp.DownAcceptThenHang)

	cfg := poolConfig(timeoutsWith(func(tt *config.Timeouts) {
		tt.BackendConnect = 300 * time.Millisecond
		tt.BackendHandshake = 300 * time.Millisecond
	}), "none", f.Addr())
	s := newHealthStack(t, cfg)

	c := openSession(t, s.addr, "halfopen")
	if c == nil {
		return
	}
	start := time.Now()
	got, err := c.message("halfopen")
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	if want := synth(proxy.RplNoBackend); got != want {
		t.Errorf("verdict against a silent backend = %q, want %q", got, want)
	}
	// Two attempts at 300ms each, plus slack: bounded, not "eventually".
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the attach took %s, want it bounded by the handshake budget", elapsed)
	}

	waitOp(t, s, "s0", health.OpDown)
	if probe := s.checker.Status("p", "s0").LastProbe; probe.Result != "fail" || !strings.Contains(probe.Detail, "handshake") {
		t.Errorf("last probe = %+v, want a failed handshake", probe)
	}
}

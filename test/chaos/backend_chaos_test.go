//go:build chaos

package chaos

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/fakesmtp"
	"github.com/revolee/bifrost/internal/health"
	"github.com/revolee/bifrost/internal/proxy"
)

// Epic-10 Task 5, backend-side scenarios. What each of these adds over the
// single-session tests in epics 04/05 is stated in its own doc comment;
// where the added dimension is concurrency, that concurrency is the
// assertion target, not decoration.

// TestChaosAllBackendsDown: every backend refuses connections while ten
// clients are mid-session. Each gets a transaction-scoped 451 4.4.1 —
// never a closed session — and every one of those same sessions delivers
// again the moment one backend comes back.
func TestChaosAllBackendsDown(t *testing.T) {
	const clients = 10

	a := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	b := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	s := newStack(t, poolConfig(chaosTimeouts(), "none", a.Addr(), b.Addr()))

	a.SetDown(fakesmtp.DownListenerClosed)
	b.SetDown(fakesmtp.DownListenerClosed)

	conns := make([]*conn, clients)
	eachClient(t, clients, func(i int) {
		c := openSession(t, s.addr, fmt.Sprintf("outage%d", i))
		if c == nil {
			return
		}
		conns[i] = c

		got, err := c.message(fmt.Sprintf("outage-%d", i))
		if err != nil {
			t.Errorf("client %d during the outage: %v", i, err)
			return
		}
		if want := synth(proxy.RplNoBackend); got != want {
			t.Errorf("client %d outage verdict = %q, want %q", i, got, want)
		}
		// Transaction-scoped: the session is still there and in sync.
		if got, err := c.cmd("NOOP"); err != nil || got != synth(proxy.RplOK) {
			t.Errorf("client %d NOOP after the outage = %q (%v), want %q", i, got, err, synth(proxy.RplOK))
		}
	})

	a.SetUp()

	eachClient(t, clients, func(i int) {
		c := conns[i]
		if c == nil {
			return
		}
		got, err := c.message(fmt.Sprintf("recovered-%d", i))
		if err != nil {
			t.Errorf("client %d after recovery: %v", i, err)
			return
		}
		if !strings.HasPrefix(got, "250") {
			t.Errorf("client %d recovery verdict = %q, want a backend 250", i, got)
		}
	})

	bodies := recordedBodies(a, b)
	for i := 0; i < clients; i++ {
		if got := bodies[wantBody(fmt.Sprintf("recovered-%d", i))]; got != 1 {
			t.Errorf("recovered message %d arrived %d times, want exactly 1", i, got)
		}
		if got := bodies[wantBody(fmt.Sprintf("outage-%d", i))]; got != 0 {
			t.Errorf("outage message %d arrived %d times, want 0 (it was refused)", i, got)
		}
	}
}

// TestChaosBackendFlapDuringTraffic: one backend's listener flaps
// open/closed, driven by the OTHER backend's traffic events (so the flap is
// paced by real work, not a timer), with the real health checker probing
// throughout. No message may be lost, duplicated, or delivered when the
// client was told 451.
func TestChaosBackendFlapDuringTraffic(t *testing.T) {
	const (
		clients  = 6
		messages = 8
	)

	a := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	b := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	s := newHealthStack(t, poolConfig(chaosTimeouts(), "none", a.Addr(), b.Addr()))

	// The hook runs inline on A's session goroutine and only ever touches
	// B (a different fakesmtp.Server), so it can neither block A nor
	// deadlock against B's own accept loop.
	var mails atomic.Int64
	a.OnEvent(func(ev fakesmtp.Event) {
		if ev.Verb != "MAIL" {
			return
		}
		switch n := mails.Add(1); {
		case n%6 == 0:
			b.SetUp()
		case n%3 == 0:
			b.SetDown(fakesmtp.DownListenerClosed)
		}
	})

	type outcome struct {
		marker  string
		verdict string
	}
	var (
		mu       sync.Mutex
		outcomes []outcome
	)
	eachClient(t, clients, func(i int) {
		c := openSession(t, s.addr, fmt.Sprintf("flap%d", i))
		if c == nil {
			return
		}
		for m := 0; m < messages; m++ {
			marker := fmt.Sprintf("flap-%d-%d", i, m)
			got, err := c.message(marker)
			if err != nil {
				t.Errorf("client %d message %d: %v", i, m, err)
				return
			}
			mu.Lock()
			outcomes = append(outcomes, outcome{marker, got})
			mu.Unlock()
		}
	})

	bodies := recordedBodies(a, b)
	delivered, refused := 0, 0
	for _, o := range outcomes {
		switch {
		case strings.HasPrefix(o.verdict, "250"):
			delivered++
			if got := bodies[wantBody(o.marker)]; got != 1 {
				t.Errorf("message %s was answered %q but arrived %d times, want exactly 1", o.marker, o.verdict, got)
			}
		case strings.HasPrefix(o.verdict, "451"):
			refused++
			if got := bodies[wantBody(o.marker)]; got != 0 {
				t.Errorf("message %s was refused (%q) but arrived %d times, want 0", o.marker, o.verdict, got)
			}
		default:
			t.Errorf("message %s verdict = %q, want a backend 250 or a transient 451", o.marker, o.verdict)
		}
	}
	t.Logf("delivered=%d refused=%d across %d flap toggles", delivered, refused, mails.Load()/3)
	if delivered == 0 {
		t.Errorf("no message got through at all")
	}
}

// TestChaosBackendClosesBetweenTransactions: the backend hangs up rudely
// at the end of every transaction (dropping instead of answering QUIT),
// which is what an idle backend closing looks like from the balancer's
// side. R3/D4: the next MAIL dials a fresh leg, transparently, every time.
func TestChaosBackendClosesBetweenTransactions(t *testing.T) {
	const messages = 5

	f := fakesmtp.Start(t, fakesmtp.Script{
		Caps:   backendCaps(),
		OnQUIT: []fakesmtp.Step{{Action: fakesmtp.ActDropConn}},
	})
	s := newStack(t, poolConfig(chaosTimeouts(), "none", f.Addr()))

	c := openSession(t, s.addr, "reuse")
	if c == nil {
		return
	}
	for i := 0; i < messages; i++ {
		got, err := c.message(fmt.Sprintf("reuse-%d", i))
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if want := "250 2.0.0 OK: queued"; got != want {
			t.Fatalf("message %d verdict = %q, want %q", i, got, want)
		}
	}

	sessions := f.Sessions()
	if len(sessions) != messages {
		t.Errorf("backend sessions = %d, want one per transaction (%d)", len(sessions), messages)
	}
	for i, sess := range sessions {
		if got := len(sess.Messages()); got != 1 {
			t.Errorf("backend session %d carried %d messages, want exactly 1", i, got)
		}
	}
}

// TestChaosBackendDropMidDATA: the backend RSTs mid-body on ten concurrent
// sessions while health probes run against the same server. Every session
// must get its own single 451 and stay in command sync — the added
// dimension over epic-05's single-session case is that nothing crosses
// between sessions.
func TestChaosBackendDropMidDATA(t *testing.T) {
	const clients = 10

	f := fakesmtp.Start(t, fakesmtp.Script{
		Caps:    backendCaps(),
		MidBody: []fakesmtp.Step{{Action: fakesmtp.ActRST}},
	})
	s := newHealthStack(t, poolConfig(chaosTimeouts(), "none", f.Addr()))

	eachClient(t, clients, func(i int) {
		c := openSession(t, s.addr, fmt.Sprintf("dropmid%d", i))
		if c == nil {
			return
		}
		// A megabyte, so the leg is provably still streaming when the RST
		// lands: a body small enough to reach the backend whole (dot
		// included) first would be the duplicate-delivery row instead.
		got, err := c.messageBig(fmt.Sprintf("dropmid-%d", i), 1<<20)
		if err != nil {
			t.Errorf("client %d: %v", i, err)
			return
		}
		if want := synth(proxy.RplBackendLost); got != want {
			t.Errorf("client %d mid-DATA verdict = %q, want %q", i, got, want)
		}
		// Exactly one reply for that transaction: if a second one had been
		// injected (another session's, or a stray after the discard) this
		// read would see it instead of the NOOP's 250.
		if got, err := c.cmd("NOOP"); err != nil || got != synth(proxy.RplOK) {
			t.Errorf("client %d NOOP after the drop = %q (%v), want %q", i, got, err, synth(proxy.RplOK))
		}
	})

	if got := recordedBodies(f); len(got) != 0 {
		t.Errorf("backend recorded %d complete messages, want 0 (every body was cut short)", len(got))
	}
}

// TestChaosBackendHangAtEOD: the backend takes the whole message and never
// answers the dot. The client gets 451 4.4.2 (the duplicate-delivery
// window), the session survives, and the server stays in rotation — a
// passive transport signal accelerates probing, it never decides a server
// is down (recovery and ejection are the active checker's calls; the
// signal accounting itself is unit-tested in internal/health).
func TestChaosBackendHangAtEOD(t *testing.T) {
	f := fakesmtp.Start(t, fakesmtp.Script{
		Caps:  backendCaps(),
		OnEOD: []fakesmtp.Step{{Action: fakesmtp.ActHang}},
	})
	cfg := poolConfig(timeoutsWith(func(tt *config.Timeouts) {
		tt.BackendFinalDot = 300 * time.Millisecond
	}), "none", f.Addr())
	s := newHealthStack(t, cfg)

	c := openSession(t, s.addr, "eodhang")
	if c == nil {
		return
	}
	got, err := c.message("eodhang")
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	if want := synth(proxy.RplBackendTimeout); got != want {
		t.Errorf("verdict = %q, want %q", got, want)
	}
	// The backend really did receive the whole message: that ambiguity IS
	// the duplicate-delivery window.
	if n := recordedBodies(f)[wantBody("eodhang")]; n != 1 {
		t.Errorf("backend recorded the message %d times, want 1", n)
	}
	if got, err := c.cmd("NOOP"); err != nil || got != synth(proxy.RplOK) {
		t.Errorf("NOOP after the hang = %q (%v), want %q", got, err, synth(proxy.RplOK))
	}

	before := probeCount(s, "s0")
	waitFor(t, "probing to continue after a passive failure", func() bool { return probeCount(s, "s0") > before })
	if got := s.checker.Status("p", "s0").Op; got != health.OpUp {
		t.Errorf("Op after one mid-transaction failure = %v, want UP (passive signals never eject)", got)
	}
}

// TestChaosBackend421Between: the backend answers a later transaction's
// MAIL with 421. The client must never see a 421 (that would announce the
// close of ITS connection) — it gets the sanctioned 451 4.4.2 translation
// — and the transaction after that gets a fresh leg and succeeds.
func TestChaosBackend421Between(t *testing.T) {
	normal := fakesmtp.Script{Caps: backendCaps()}
	closing := fakesmtp.Script{
		Caps:   backendCaps(),
		OnMAIL: []fakesmtp.Step{{Reply: "421 4.7.0 Service not available, closing transmission channel"}},
	}
	f := fakesmtp.Start(t, normal)

	// A fresh connection per transaction (D4) snapshots the script as it
	// is accepted, so swapping the script on the Nth MAIL is what makes
	// the (N+1)th transaction the one that meets the 421.
	var mails atomic.Int64
	f.OnEvent(func(ev fakesmtp.Event) {
		if ev.Verb != "MAIL" {
			return
		}
		switch mails.Add(1) {
		case 1:
			f.SetScript(closing)
		case 2:
			f.SetScript(normal)
		}
	})

	s := newStack(t, poolConfig(chaosTimeouts(), "none", f.Addr()))
	c := openSession(t, s.addr, "closing")
	if c == nil {
		return
	}

	if got, err := c.message("first"); err != nil || got != "250 2.0.0 OK: queued" {
		t.Fatalf("first message verdict = %q (%v), want the backend's 250", got, err)
	}

	got, err := c.message("closed")
	if err != nil {
		t.Fatalf("second message: %v", err)
	}
	if want := synth(proxy.RplBackendClosing); got != want {
		t.Errorf("verdict for a backend 421 = %q, want the translation %q", got, want)
	}
	if strings.HasPrefix(got, "421") {
		t.Errorf("a backend 421 was relayed verbatim (%q): that announces the client's own close", got)
	}

	if got, err := c.message("third"); err != nil || got != "250 2.0.0 OK: queued" {
		t.Errorf("third message verdict = %q (%v), want the backend's 250 on a replaced leg", got, err)
	}
}

// TestChaosTLSFailOneBackend: three backends, pool-wide STARTTLS, and the
// middle one does not offer it. The health checker ejects exactly that one
// and the other two carry every message.
func TestChaosTLSFailOneBackend(t *testing.T) {
	const messages = 8

	tlsCfg := fakesmtp.TestCert(t)
	a := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps(), TLS: tlsCfg})
	noTLS := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()}) // never offers STARTTLS
	c3 := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps(), TLS: tlsCfg})

	cfg := poolConfig(chaosTimeouts(), "starttls", a.Addr(), noTLS.Addr(), c3.Addr())
	// The probe upgrades too, so the capability the traffic path requires
	// is the one the checker verifies.
	for i := range cfg.Pools[0].Servers {
		cfg.Pools[0].Servers[i].Check.TLS = "starttls"
	}
	s := newHealthStack(t, cfg)

	waitOp(t, s, "s1", health.OpDown)
	waitOp(t, s, "s0", health.OpUp)
	waitOp(t, s, "s2", health.OpUp)

	client := openSession(t, s.addr, "tlsfail")
	if client == nil {
		return
	}
	for i := 0; i < messages; i++ {
		got, err := client.message(fmt.Sprintf("tls-%d", i))
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !strings.HasPrefix(got, "250") {
			t.Fatalf("message %d verdict = %q, want a backend 250 from a TLS-capable server", i, got)
		}
	}

	if got := len(recordedBodies(noTLS)); got != 0 {
		t.Errorf("the ejected backend recorded %d messages, want 0", got)
	}
	spread := recordedBodies(a, c3)
	if len(spread) != messages {
		t.Errorf("the TLS-capable backends recorded %d distinct messages, want %d", len(spread), messages)
	}
	if a.CmdCount("STARTTLS") == 0 || c3.CmdCount("STARTTLS") == 0 {
		t.Errorf("STARTTLS counts = %d/%d, want both legs upgraded", a.CmdCount("STARTTLS"), c3.CmdCount("STARTTLS"))
	}
}

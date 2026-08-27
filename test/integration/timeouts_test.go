//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/fakesmtp"
	"github.com/revolee/bifrost/internal/proxy"
)

// Epic-10 Task 4: every row of PROJECT.md's timeout budget, driven
// through the REAL binary against a hostile fixture, asserting the exact
// client-visible reply byte for byte against internal/proxy's closed enum.
// This task exists because a timer that is wired to the wrong phase (or to
// nothing) is invisible until a backend misbehaves in production.

// auditTimeouts is the budget with every row shortened to milliseconds.
// The relative ORDER of the rows is preserved (connect <= mail reply,
// data_progress < final_dot), because that ordering is what makes each row
// attributable: whichever timer the test is about must be the one that
// fires first.
const auditTimeouts = `
    client_idle        = "400ms"
    session_max        = "5m"
    backend_connect    = "300ms"
    backend_handshake  = "300ms"
    backend_mail_reply = "300ms"
    backend_354_wait   = "300ms"
    data_progress      = "400ms"
    backend_final_dot  = "500ms"
    lame_duck          = "50ms"
    drain_timeout      = "2s"
`

// lifetimeTimeouts is the session-lifetime row's own budget: the lifetime
// cap has to be the SHORTEST timer for its expiry to be attributable, so
// it gets a process of its own rather than fighting client_idle.
const lifetimeTimeouts = `
    client_idle        = "20s"
    session_max        = "700ms"
    backend_connect    = "2s"
    backend_handshake  = "2s"
    backend_mail_reply = "5s"
    backend_354_wait   = "5s"
    data_progress      = "20s"
    backend_final_dot  = "20s"
    lame_duck          = "50ms"
    drain_timeout      = "2s"
`

// hostilePool is one pool per failure mode, selected by the MAIL FROM
// domain: one process can then host every row of the table, and each row
// is a plain client that picks its own hostile backend by who it claims to
// be from.
func hostilePool(name, addr, extra string) string {
	return fmt.Sprintf("pool %q {\n  balance = \"roundrobin\"\n%s%s}\n",
		name, extra, serverHCL("s", addr, 1))
}

// neverDownCheck keeps a deliberately unreachable backend eligible: these
// two rows are about the dial/handshake TIMER, and a health checker that
// ejected the server first would produce the same 451 for a different
// reason. L0 (plain connect) also keeps probe noise off a fake that is
// scripted to hang.
const neverDownCheck = `  check {
    level = "connect"
    fall  = 1000
  }
`

// TestTimeoutBudgetTable is the audit: one row per timer in PROJECT.md's
// table, each with the hostile fixture that trips it and the exact reply
// the Transparency Contract promises.
func TestTimeoutBudgetTable(t *testing.T) {
	normal := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	mailHang := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps(), OnMAIL: []fakesmtp.Step{{Delay: 3 * time.Second}}})
	dataHang := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps(), OnDATA: []fakesmtp.Step{{Delay: 3 * time.Second}}})
	dotHang := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps(), OnEOD: []fakesmtp.Step{{Delay: 3 * time.Second}}})
	writeHang := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps(), MidBody: []fakesmtp.Step{{Action: fakesmtp.ActHang}}})
	handshakeHang := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	handshakeHang.SetDown(fakesmtp.DownAcceptThenHang)

	// 192.0.2.1 is RFC 5737 TEST-NET-1: guaranteed not to route, so the
	// connect attempt either times out or is refused — both of which are
	// the same row ("next candidate; exhausted -> 451 4.4.1").
	pools := hostilePool("normal", normal.Addr(), "") +
		hostilePool("connectfail", "192.0.2.1:25", neverDownCheck) +
		hostilePool("handshake", handshakeHang.Addr(), neverDownCheck) +
		hostilePool("mailhang", mailHang.Addr(), "") +
		hostilePool("datahang", dataHang.Addr(), "") +
		hostilePool("writehang", writeHang.Addr(), "") +
		hostilePool("dothang", dotHang.Addr(), "")

	var routing strings.Builder
	for _, pool := range []string{"connectfail", "handshake", "mailhang", "datahang", "writehang", "dothang"} {
		fmt.Fprintf(&routing, "  rule {\n    mail_from_domain = [%q]\n    pool             = %q\n  }\n", pool+".test", pool)
	}
	routing.WriteString(`  default_pool = "normal"` + "\n")

	smtp, adminAddr := freeAddr(t), freeAddr(t)
	cfg := fixture{
		smtp: smtp, admin: adminAddr,
		timeouts: auditTimeouts,
		pools:    pools,
		routing:  routing.String(),
	}.render()
	b := startBifrost(t, writeFile(t, t.TempDir(), "bifrost.hcl", cfg), smtp, adminAddr)

	lifetimeSMTP, lifetimeAdmin := freeAddr(t), freeAddr(t)
	lifetimeCfg := fixture{
		smtp: lifetimeSMTP, admin: lifetimeAdmin,
		timeouts: lifetimeTimeouts,
		pools:    poolHCL("p", serverHCL("a", normal.Addr(), 1)),
	}.render()
	lifetime := startBifrost(t, writeFile(t, t.TempDir(), "lifetime.hcl", lifetimeCfg), lifetimeSMTP, lifetimeAdmin)

	rows := []struct {
		name       string
		timer      string // the PROJECT.md row this proves
		want       string // the exact reply from internal/proxy's enum
		wantClosed bool   // 421 rows are always followed by a close
		run        func(t *testing.T) (got string, c *rawClient)
	}{
		{
			name: "client first-command wait", timer: "client first-command / idle",
			want: proxy.RplIdleTimeout, wantClosed: true,
			run: func(t *testing.T) (string, *rawClient) {
				c := dialRaw(t, b.smtp)
				if got := c.reply(5 * time.Second); !strings.HasPrefix(got, "220 ") {
					t.Fatalf("banner = %q", got)
				}
				return c.reply(5 * time.Second), c // never sends a command
			},
		},
		{
			name: "client idle between commands", timer: "client first-command / idle",
			want: proxy.RplIdleTimeout, wantClosed: true,
			run: func(t *testing.T) (string, *rawClient) {
				c := openSession(t, b.smtp)
				return c.reply(5 * time.Second), c // greeted, then goes quiet
			},
		},
		{
			name: "backend connect exhausted", timer: "backend connect (per attempt x 2)",
			want: proxy.RplNoBackend,
			run: func(t *testing.T) (string, *rawClient) {
				c := openSession(t, b.smtp)
				start := time.Now()
				c.send("MAIL FROM:<probe@connectfail.test>")
				got := c.reply(10 * time.Second)
				// Depending on the host's routing, TEST-NET-1 either drops
				// (the connect budget expires, twice) or is refused
				// instantly; both are this row's outcome. What must hold
				// either way is the bound: two attempts' worth of budget,
				// never an unbounded wait. The budget actually expiring is
				// proven by the handshake row below, which always hangs.
				if elapsed := time.Since(start); elapsed > 2*time.Second {
					t.Errorf("MAIL against an unroutable backend answered after %s, want it bounded by 2 x backend_connect (300ms)", elapsed)
				}
				return got, c
			},
		},
		{
			name: "backend handshake hang", timer: "backend handshake (greeting+EHLO+TLS)",
			want: proxy.RplNoBackend,
			run: func(t *testing.T) (string, *rawClient) {
				c := openSession(t, b.smtp)
				c.send("MAIL FROM:<probe@handshake.test>")
				return c.reply(10 * time.Second), c
			},
		},
		{
			name: "backend MAIL reply hang", timer: "backend MAIL/RCPT reply",
			want: proxy.RplBackendTimeout,
			run: func(t *testing.T) (string, *rawClient) {
				c := openSession(t, b.smtp)
				c.send("MAIL FROM:<probe@mailhang.test>")
				return c.reply(10 * time.Second), c
			},
		},
		{
			name: "backend 354 wait hang", timer: "backend 354 wait",
			want: proxy.RplBackendTimeout,
			run: func(t *testing.T) (string, *rawClient) {
				c := openSession(t, b.smtp)
				c.envelope(t, "datahang.test")
				c.send("DATA")
				return c.reply(10 * time.Second), c
			},
		},
		{
			name: "client DATA feed stalls", timer: "DATA progress watchdog (client side)",
			want: proxy.RplIdleTimeout, wantClosed: true,
			run: func(t *testing.T) (string, *rawClient) {
				c := openSession(t, b.smtp)
				c.envelope(t, "normal.test")
				c.send("DATA")
				if got := c.reply(5 * time.Second); !strings.HasPrefix(got, "354") {
					t.Fatalf("DATA reply = %q, want 354", got)
				}
				c.raw([]byte("Subject: stalled\r\n\r\nhalf a "))
				return c.reply(10 * time.Second), c // and then nothing, ever
			},
		},
		{
			name: "backend DATA write stalls", timer: "DATA progress watchdog (backend side)",
			want: proxy.RplBackendTimeout,
			run: func(t *testing.T) (string, *rawClient) {
				c := openSession(t, b.smtp)
				c.envelope(t, "writehang.test")
				c.send("DATA")
				if got := c.reply(5 * time.Second); !strings.HasPrefix(got, "354") {
					t.Fatalf("DATA reply = %q, want 354", got)
				}
				// The backend stops reading after the first body line, so
				// the balancer's writes to it block once the socket buffers
				// fill; the watchdog then fires and the rest of the message
				// is discarded to the dot (which is why the client's own
				// writes still complete).
				errc := writeBigBody(c, 64<<20)
				got := c.reply(30 * time.Second)
				if err := <-errc; err != nil {
					t.Logf("client body write ended with %v (expected once the balancer stops reading)", err)
				}
				return got, c
			},
		},
		{
			name: "backend final-dot reply hang", timer: "backend final-dot reply",
			want: proxy.RplBackendTimeout,
			run: func(t *testing.T) (string, *rawClient) {
				c := openSession(t, b.smtp)
				c.envelope(t, "dothang.test")
				c.send("DATA")
				if got := c.reply(5 * time.Second); !strings.HasPrefix(got, "354") {
					t.Fatalf("DATA reply = %q, want 354", got)
				}
				c.raw([]byte("Subject: no verdict\r\n\r\nbody\r\n.\r\n"))
				return c.reply(10 * time.Second), c
			},
		},
		{
			name: "session lifetime inside a transaction", timer: "session max lifetime",
			want: proxy.RplSessionLifetime, wantClosed: true,
			run: func(t *testing.T) (string, *rawClient) {
				c := openSession(t, lifetime.smtp)
				c.envelope(t, "normal.test")
				c.send("DATA")
				if got := c.reply(5 * time.Second); !strings.HasPrefix(got, "354") {
					t.Fatalf("DATA reply = %q, want 354", got)
				}
				// Bytes keep moving, so no idle or progress timer can fire:
				// only the absolute lifetime cap can end this, and it must
				// end it mid-transaction rather than waiting for the dot.
				return dribbleUntilReply(t, c, 15*time.Second), c
			},
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got, c := row.run(t)
			wantReply(t, row.timer+" reply", got, row.want)
			if row.wantClosed {
				c.expectClosed(5 * time.Second)
				return
			}
			// 451 rows are transaction-scoped: the session must still be
			// there and still in command sync.
			c.send("NOOP")
			if got := c.reply(5 * time.Second); got != strings.TrimSuffix(proxy.RplOK, "\r\n") {
				t.Errorf("NOOP after %s = %q, want the session to survive with %q", row.timer, got, proxy.RplOK)
			}
		})
	}

	// The dot-reply row is also the duplicate-delivery window: the backend
	// took the whole message and never said what it did with it.
	b.waitLog("duplicate delivery risk", 5*time.Second)
}

// openSession dials and greets, leaving the connection ready for a
// transaction.
func openSession(t *testing.T, addr string) *rawClient {
	t.Helper()
	c := dialRaw(t, addr)
	if got := c.reply(5 * time.Second); !strings.HasPrefix(got, "220 ") {
		t.Fatalf("banner = %q, want a 220", got)
	}
	c.send("EHLO audit.example")
	if got := c.reply(5 * time.Second); !strings.HasPrefix(got, "250") {
		t.Fatalf("EHLO reply = %q, want 250", got)
	}
	return c
}

// envelope sends MAIL FROM (in the domain that routes to the pool under
// test) and one RCPT, asserting both are accepted.
func (c *rawClient) envelope(t *testing.T, domain string) {
	t.Helper()
	c.send("MAIL FROM:<probe@" + domain + ">")
	if got := c.reply(10 * time.Second); !strings.HasPrefix(got, "250") {
		t.Fatalf("MAIL reply = %q, want 250", got)
	}
	c.send("RCPT TO:<rcpt@" + domain + ">")
	if got := c.reply(10 * time.Second); !strings.HasPrefix(got, "250") {
		t.Fatalf("RCPT reply = %q, want 250", got)
	}
}

// writeBigBody streams total bytes of body plus the terminator from a
// goroutine (the write blocks once the balancer stops reading, so the
// test's own goroutine has to stay free to read the reply) and reports the
// first write error, if any, on the returned channel.
func writeBigBody(c *rawClient, total int) <-chan error {
	errc := make(chan error, 1)
	line := []byte(strings.Repeat("x", 1022) + "\r\n")
	go func() {
		defer close(errc)
		for sent := 0; sent < total; sent += len(line) {
			if err := c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
				errc <- err
				return
			}
			if _, err := c.conn.Write(line); err != nil {
				errc <- err
				return
			}
		}
		if _, err := c.conn.Write([]byte(".\r\n")); err != nil {
			errc <- err
		}
	}()
	return errc
}

// dribbleUntilReply keeps feeding body bytes (so no progress or idle timer
// can fire) until the server answers, and returns that reply.
func dribbleUntilReply(t *testing.T, c *rawClient, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatalf("set write deadline: %v", err)
		}
		if _, err := c.conn.Write([]byte("still here\r\n")); err != nil {
			break // the server closed on us; the reply is already buffered
		}
		if line, err := c.replyErr(100 * time.Millisecond); err == nil {
			return line
		}
	}
	return c.reply(5 * time.Second)
}

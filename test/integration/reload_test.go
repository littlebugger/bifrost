//go:build integration

package integration

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/smtpdrv"
)

// Epic-10 Task 3: SIGHUP hot reload. The claims are D14's: a new
// generation is live at the next MAIL (not the next connection, and
// certainly not the next process), a transaction already attached finishes
// where it started, a bad file never displaces a good one, and the two
// things that cannot move in v1 — the listener and admin binds — are
// rejected with a diagnostic instead of silently ignored.

// reloadOK rewrites the config file, sends SIGHUP, and returns once the
// process has logged the successful swap. Event-driven on the process's
// own log, so a test never has to guess how long a reload takes.
func (b *bifrost) reloadOK(text string) {
	b.t.Helper()
	b.reloadAndWait(text, "config reloaded")
}

// reloadRejected is reloadOK's mirror: it returns once the process has
// logged that it refused the new file.
func (b *bifrost) reloadRejected(text string) {
	b.t.Helper()
	b.reloadAndWait(text, "reload rejected")
}

func (b *bifrost) reloadAndWait(text, marker string) {
	b.t.Helper()
	before := strings.Count(b.logText(), marker)
	if text != "" {
		writeFileAt(b.t, b.cfgPath, text)
	}
	b.signal(syscall.SIGHUP)

	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(b.logText(), marker) <= before {
		if time.Now().After(deadline) {
			b.t.Fatalf("no %q in the log within 10s after SIGHUP\nlogs:\n%s", marker, b.logText())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// twoPoolFixture is one pool per backend ("pa" holding A, "pb" holding
// B), with defaultPool selecting which one traffic goes to — the smallest
// config shape in which a reload can visibly reroute.
func twoPoolFixture(a, bk *fakesmtp.Server, smtp, adminAddr, defaultPool string) string {
	return fixture{
		smtp:    smtp,
		admin:   adminAddr,
		pools:   poolHCL("pa", serverHCL("a", a.Addr(), 1)) + poolHCL("pb", serverHCL("b", bk.Addr(), 1)),
		routing: fmt.Sprintf("  default_pool = %q\n", defaultPool),
	}.render()
}

// TestReloadNextMailSemantics is D14's central promise on a live
// connection: the client never reconnects, and its next message follows
// the new rules — while a transaction that was already attached when the
// swap landed finishes on the backend it started with.
func TestReloadNextMailSemantics(t *testing.T) {
	a, bk := namedFake(t, "A"), namedFake(t, "B")
	smtp, adminAddr := freeAddr(t), freeAddr(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "bifrost.hcl", twoPoolFixture(a, bk, smtp, adminAddr, "pa"))
	b := startBifrost(t, path, smtp, adminAddr)

	c := smtpdrv.Dial(t, b.smtp)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")
	if got, want := c.SendMsg(0).Lines[0], "250 2.0.0 OK: queued via A"; got != want {
		t.Fatalf("msg1 verdict = %q, want %q", got, want)
	}

	// A second connection parks mid-DATA, attached to A under the OLD
	// config...
	inFlight := dialRaw(t, b.smtp)
	inFlight.reply(5 * time.Second)
	inFlight.send("EHLO inflight.example")
	inFlight.reply(5 * time.Second)
	inFlight.send("MAIL FROM:<sender@test.example>")
	inFlight.reply(5 * time.Second)
	inFlight.send("RCPT TO:<rcpt@test.example>")
	inFlight.reply(5 * time.Second)
	inFlight.send("DATA")
	if got := inFlight.reply(5 * time.Second); !strings.HasPrefix(got, "354") {
		t.Fatalf("DATA reply = %q, want 354", got)
	}
	inFlight.raw([]byte("Subject: across a reload\r\n\r\nbody\r\n"))

	// ...the swap lands while it is still streaming...
	b.reloadOK(twoPoolFixture(a, bk, smtp, adminAddr, "pb"))

	// ...and it still ends on A, verbatim.
	inFlight.raw([]byte(".\r\n"))
	if got, want := inFlight.reply(10*time.Second), "250 2.0.0 OK: queued via A"; got != want {
		t.Errorf("in-flight verdict = %q, want %q (a reload must not move an attached transaction)", got, want)
	}

	// The first connection's NEXT message follows the new rules, with no
	// reconnect: this is the next-MAIL semantics claim.
	if got, want := c.SendMsg(1).Lines[0], "250 2.0.0 OK: queued via B"; got != want {
		t.Errorf("msg2 verdict = %q, want %q (the swap did not take effect at the next MAIL)", got, want)
	}
	c.Send("QUIT")
	c.Expect("221")
}

// TestReloadRemovedServerDrains: a server dropped from the file finishes
// what it is carrying, takes no new transactions, and stops being probed.
func TestReloadRemovedServerDrains(t *testing.T) {
	a, bk := namedFake(t, "A"), namedFake(t, "B")
	smtp, adminAddr := freeAddr(t), freeAddr(t)
	both := fixture{
		smtp: smtp, admin: adminAddr,
		pools: poolHCL("p", serverHCL("a", a.Addr(), 1), serverHCL("b", bk.Addr(), 1)),
	}.render()
	onlyA := fixture{
		smtp: smtp, admin: adminAddr,
		pools: poolHCL("p", serverHCL("a", a.Addr(), 1)),
	}.render()
	b := startBifrost(t, writeFile(t, t.TempDir(), "bifrost.hcl", both), smtp, adminAddr)

	c := smtpdrv.Dial(t, b.smtp)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")
	// Equal weights: WRR's tie-break takes the first-listed server first,
	// so msg1 is A's and the transaction opened next attaches to B.
	if got, want := c.SendMsg(0).Lines[0], "250 2.0.0 OK: queued via A"; got != want {
		t.Fatalf("msg1 verdict = %q, want %q", got, want)
	}

	inFlight := dialRaw(t, b.smtp)
	inFlight.reply(5 * time.Second)
	inFlight.send("EHLO inflight.example")
	inFlight.reply(5 * time.Second)
	inFlight.send("MAIL FROM:<sender@test.example>")
	inFlight.reply(5 * time.Second)
	inFlight.send("RCPT TO:<rcpt@test.example>")
	inFlight.reply(5 * time.Second)
	inFlight.send("DATA")
	inFlight.reply(5 * time.Second)
	inFlight.raw([]byte("Subject: on a removed server\r\n\r\nbody\r\n"))

	b.reloadOK(onlyA)

	inFlight.raw([]byte(".\r\n"))
	if got, want := inFlight.reply(10*time.Second), "250 2.0.0 OK: queued via B"; got != want {
		t.Errorf("in-flight verdict = %q, want %q (a removed server must still finish what it has)", got, want)
	}

	// No new picks, and no more probes: B's connection count freezes while
	// A's keeps climbing (A's own probe cadence is the clock here, not a
	// sleep). The health checker notices a config change on its next
	// polling tick rather than instantly, so the freeze is asserted from
	// after that window, not from the reload itself.
	waitDialsAtLeast(t, a, a.DialCount()+8) // ~1.6s at a 200ms probe interval
	frozen := bk.DialCount()
	waitDialsAtLeast(t, a, a.DialCount()+8)
	if got := bk.DialCount(); got != frozen {
		t.Errorf("removed server saw %d more connections after the reload, want none (probes must stop)", got-frozen)
	}
	for i := 1; i <= 3; i++ {
		if got, want := c.SendMsg(i).Lines[0], "250 2.0.0 OK: queued via A"; got != want {
			t.Errorf("post-reload msg%d verdict = %q, want %q (a removed server must take no new work)", i, got, want)
		}
	}
	if got := bk.DialCount(); got != frozen {
		t.Errorf("removed server saw %d more connections while serving traffic, want none", got-frozen)
	}

	servers := adminServers(t, b)
	if len(servers.Servers) != 1 || servers.Servers[0].Server != "a" {
		t.Errorf("GET /servers = %+v, want only server a", servers.Servers)
	}
}

// TestReloadBadConfigKeepsOld: a file that fails validation is reported
// with its own diagnostics and changes nothing.
func TestReloadBadConfigKeepsOld(t *testing.T) {
	a, bk := namedFake(t, "A"), namedFake(t, "B")
	smtp, adminAddr := freeAddr(t), freeAddr(t)
	good := twoPoolFixture(a, bk, smtp, adminAddr, "pa")
	b := startBifrost(t, writeFile(t, t.TempDir(), "bifrost.hcl", good), smtp, adminAddr)

	broken := strings.Replace(good, `weight  = 1`, `weight  = 999`, 1)
	b.reloadRejected(broken)

	logs := b.logText()
	if !strings.Contains(logs, "Weight out of range") {
		t.Errorf("logs do not carry the rejected config's own diagnostic:\n%s", logs)
	}
	if !strings.Contains(logs, "bifrost.hcl:") {
		t.Errorf("rejection diagnostic has no file:line anchor:\n%s", logs)
	}

	// Traffic keeps flowing on the configuration that was already live.
	c := smtpdrv.Dial(t, b.smtp)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")
	if got, want := c.SendMsg(0).Lines[0], "250 2.0.0 OK: queued via A"; got != want {
		t.Errorf("verdict after a rejected reload = %q, want %q", got, want)
	}
	c.Send("QUIT")
	c.Expect("221")
}

// TestReloadListenerChangeRejected: only the binds are immovable in v1,
// and a file that moves one is rejected WHOLE — the rest of that file's
// changes must not sneak in either.
func TestReloadListenerChangeRejected(t *testing.T) {
	a, bk := namedFake(t, "A"), namedFake(t, "B")
	smtp, adminAddr := freeAddr(t), freeAddr(t)
	live := twoPoolFixture(a, bk, smtp, adminAddr, "pa")
	b := startBifrost(t, writeFile(t, t.TempDir(), "bifrost.hcl", live), smtp, adminAddr)

	for _, tc := range []struct {
		name string
		text string
	}{
		{"listener bind moved", twoPoolFixture(a, bk, freeAddr(t), adminAddr, "pb")},
		{"admin bind moved", twoPoolFixture(a, bk, smtp, freeAddr(t), "pb")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b.reloadRejected(tc.text)
			if logs := b.logText(); !strings.Contains(logs, "restart required") {
				t.Errorf("logs do not say a restart is required:\n%s", logs)
			}

			// The whole file was refused, so the routing change it also
			// carried is not live: traffic still goes to A.
			c := smtpdrv.Dial(t, b.smtp)
			c.Expect("220")
			c.Send("EHLO client.example")
			c.Expect("250")
			if got, want := c.SendMsg(0).Lines[0], "250 2.0.0 OK: queued via A"; got != want {
				t.Errorf("verdict = %q, want %q (a rejected reload must change nothing)", got, want)
			}
			c.Send("QUIT")
			c.Expect("221")
		})
	}
}

// TestReloadRevertsRuntimeWeight is D15's survival matrix, the one row
// that does NOT survive: a runtime weight override is discarded by a
// reload, and the reload says which ones it dropped.
func TestReloadRevertsRuntimeWeight(t *testing.T) {
	a, bk := namedFake(t, "A"), namedFake(t, "B")
	smtp, adminAddr := freeAddr(t), freeAddr(t)
	live := fixture{
		smtp: smtp, admin: adminAddr,
		pools: poolHCL("p", serverHCL("a", a.Addr(), 3), serverHCL("b", bk.Addr(), 1)),
	}.render()
	b := startBifrost(t, writeFile(t, t.TempDir(), "bifrost.hcl", live), smtp, adminAddr)

	if code, body := b.post("/servers/p/a/weight", `{"weight":0}`); code != 200 {
		t.Fatalf("POST weight = %d, body %s", code, body)
	}
	if got := serverWeight(t, b, "a"); got != 0 {
		t.Fatalf("weight after the override = %d, want 0", got)
	}

	b.reloadOK("") // same file, unchanged

	if got := serverWeight(t, b, "a"); got != 3 {
		t.Errorf("weight after the reload = %d, want the file's 3", got)
	}
	if logs := b.logText(); !strings.Contains(logs, "p/a") {
		t.Errorf("reload log does not list the discarded override p/a:\n%s", logs)
	}
}

// serverWeight is (pool p, server name)'s effective weight per GET
// /servers.
func serverWeight(t *testing.T, b *bifrost, name string) int {
	t.Helper()
	for _, s := range adminServers(t, b).Servers {
		if s.Server == name {
			return s.Weight
		}
	}
	t.Fatalf("server %q missing from GET /servers", name)
	return -1
}

// TestReloadListenerFieldWarnsRestartRequired: the listener's own
// identity and the client-leg limits are read once per process, so a
// reload accepts an edit to them and cannot apply it — to new connections
// either. The edit must therefore be NAMED (in the log for SIGHUP, in the
// response for POST /reload); silently reporting "no changes" for a line
// the operator can see in the file is the failure mode this test exists
// to prevent.
func TestReloadListenerFieldWarnsRestartRequired(t *testing.T) {
	a := namedFake(t, "A")
	smtp, adminAddr := freeAddr(t), freeAddr(t)
	live := fixture{smtp: smtp, admin: adminAddr, pools: poolHCL("p", serverHCL("a", a.Addr(), 1))}.render()
	b := startBifrost(t, writeFile(t, t.TempDir(), "bifrost.hcl", live), smtp, adminAddr)

	renamed := strings.Replace(live, `hostname     = "bifrost.test"`, `hostname     = "renamed.test"`, 1)
	if renamed == live {
		t.Fatal("fixture no longer contains the hostname attribute this test edits")
	}
	b.reloadOK(renamed)

	logs := b.logText()
	if !strings.Contains(logs, "reload applied with a limitation") || !strings.Contains(logs, "listener hostname changed") {
		t.Errorf("logs do not name the un-appliable hostname change:\n%s", logs)
	}

	// The truthful current behavior: even a brand-new connection gets the
	// hostname the process started with.
	c := dialRaw(t, b.smtp)
	if got := c.reply(5 * time.Second); got != "220 bifrost.test ESMTP" {
		t.Errorf("banner after the hostname reload = %q, want the startup hostname (restart required)", got)
	}
	c.send("QUIT")
	c.reply(5 * time.Second)

	// POST /reload reports the same thing to the operator who used it.
	capped := strings.Replace(renamed, "global_maxconn = 64", "global_maxconn = 32", 1)
	writeFileAt(t, b.cfgPath, capped)
	code, body := b.post("/reload", "")
	if code != 200 {
		t.Fatalf("POST /reload = %d, body %s", code, body)
	}
	if !strings.Contains(body, "restart_required") || !strings.Contains(body, "global_maxconn changed") {
		t.Errorf("POST /reload body = %s, want it to name the un-appliable global_maxconn change", body)
	}
}

// TestReloadStorm is the load case: 20 reloads inside 10 seconds while 10
// clients keep sending. Every message must still get its backend's real
// verdict — a reload is an atomic pointer swap, so no client may ever see
// a transaction refused because the config was momentarily "between"
// generations — and nothing may be left behind: sessions and in-flight
// counters return to zero and the process drains cleanly (a wedged
// session goroutine would show up as the drain hitting its force
// deadline).
func TestReloadStorm(t *testing.T) {
	const (
		clients           = 10
		messagesPerClient = 8
		reloads           = 20
	)

	a, bk := namedFake(t, "A"), namedFake(t, "B")
	smtp, adminAddr := freeAddr(t), freeAddr(t)
	variants := []string{
		fixture{smtp: smtp, admin: adminAddr, pools: poolHCL("p", serverHCL("a", a.Addr(), 1), serverHCL("b", bk.Addr(), 1))}.render(),
		fixture{smtp: smtp, admin: adminAddr, pools: poolHCL("p", serverHCL("a", a.Addr(), 3), serverHCL("b", bk.Addr(), 1))}.render(),
	}
	b := startBifrost(t, writeFile(t, t.TempDir(), "bifrost.hcl", variants[0]), smtp, adminAddr)

	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := smtpdrv.Dial(t, b.smtp)
			c.SetFail(func(format string, args ...any) { t.Errorf(format, args...) })
			c.Expect("220")
			c.Send(fmt.Sprintf("EHLO client%d.example", i))
			c.Expect("250")
			for m := 0; m < messagesPerClient; m++ {
				reply := c.SendMsg(i*100 + m)
				if len(reply.Lines) != 1 || !strings.HasPrefix(reply.Lines[0], "250 2.0.0 OK: queued via ") {
					t.Errorf("client %d message %d verdict = %q, want a backend 250", i, m, reply.Lines)
					return
				}
			}
			c.Send("QUIT")
			c.Expect("221")
		}(i)
	}

	for r := 0; r < reloads; r++ {
		b.reloadOK(variants[r%len(variants)])
	}
	wg.Wait()

	if got := metricValue(t, b, "bifrost_sessions_active"); got != 0 {
		t.Errorf("bifrost_sessions_active after the storm = %v, want 0", got)
	}
	for _, name := range []string{"a", "b"} {
		if got := metricValue(t, b, fmt.Sprintf("bifrost_in_flight{pool=\"p\",server=%q}", name)); got != 0 {
			t.Errorf("bifrost_in_flight for %s = %v, want 0", name, got)
		}
	}

	b.signal(syscall.SIGTERM)
	if code := b.waitExit(20 * time.Second); code != 0 {
		t.Errorf("exit code = %d, want 0\nlogs:\n%s", code, b.logText())
	}
	if logs := b.logText(); !strings.Contains(logs, "drain complete: every session ended") {
		t.Errorf("drain did not complete cleanly after the storm (a leaked session goroutine?):\n%s", logs)
	}
}

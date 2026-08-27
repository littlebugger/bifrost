//go:build integration

package integration

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/revolee/bifrost/internal/balance"
	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/fakesmtp"
	"github.com/revolee/bifrost/internal/proxy"
	"github.com/revolee/bifrost/internal/smtpdrv"
)

// M1: one client connection, many messages, provably spread across
// backends with byte-exact transparency — requirements R3 and R4 together,
// which is the whole point of the project.

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// serve runs a listener whose accepted connections each become a Session
// with h as their TxnHandler, and returns its address. It is the stand-in
// for epic 08's real accept loop (listener.go, with the maxconn gate);
// everything it does is torn down by t.Cleanup, sessions joined included,
// so goleak can hold the whole package to account.
func serve(t *testing.T, cfg *config.Config, h proxy.TxnHandler) string {
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
				s := proxy.NewSession(conn, cfg, nil, h, slog.New(slog.DiscardHandler))
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

// m1Timeouts is PROJECT.md's budget with the long waits shortened enough
// that a wedged backend fails a test instead of hanging it.
func m1Timeouts() config.Timeouts {
	return config.Timeouts{
		ClientIdle:       30 * time.Second,
		SessionMax:       5 * time.Minute,
		BackendConnect:   2 * time.Second,
		BackendHandshake: 2 * time.Second,
		BackendMailReply: 5 * time.Second,
		Backend354Wait:   5 * time.Second,
		DataProgress:     30 * time.Second,
		BackendFinalDot:  60 * time.Second,
	}
}

// m1Config is a listener plus one pool holding a server per address.
func m1Config(addrs ...string) *config.Config {
	pool := config.Pool{Name: "p", Balance: "roundrobin", BackendTLS: "none", EhloName: "bifrost.test"}
	for i, addr := range addrs {
		pool.Servers = append(pool.Servers, config.Server{
			Name: fmt.Sprintf("s%d", i), Address: addr, Weight: 1,
		})
	}
	return &config.Config{
		Defaults: config.Defaults{Timeouts: m1Timeouts()},
		Listener: config.Listener{
			Hostname:     "bifrost.test",
			Capabilities: []string{"PIPELINING", "8BITMIME", "SIZE 104857600"},
		},
		Pools:   []config.Pool{pool},
		Routing: config.Routing{DefaultPool: "p"},
	}
}

// backendCaps satisfies the superset check against m1Config's advertised
// set.
func backendCaps() []string { return []string{"PIPELINING", "8BITMIME", "SIZE 1073741824"} }

// alwaysEligible is the EligibleFunc these tests use when every server
// is meant to be healthy: epic 06's real Checker is exercised in its own
// package, not here.
func alwaysEligible(string, string) bool { return true }

// newRelay wires a Relay over cfg's servers with the real epic-07
// Router — rule-match -> pool -> algorithm -> ordered candidates, plus
// its in-flight Lease — instead of epic-05's stub. rand is seeded so a
// test's distribution assertions are reproducible.
func newRelay(cfg *config.Config) *proxy.Relay {
	h := &config.Holder{}
	h.Swap(cfg)
	router := balance.NewRouter(h, alwaysEligible, rand.New(rand.NewSource(1)))
	return proxy.NewRelay(router.Pick, h, slog.New(slog.DiscardHandler), nil, router.Lease)
}

// wantBody is the wire body smtpdrv.SendMsg(i) produces: dot-stuffed,
// terminator excluded, exactly as a backend records it.
func wantBody(i int) []byte {
	return []byte(fmt.Sprintf("Subject: message %d\r\n\r\nbody %d\r\n", i, i))
}

// settleGoroutines waits (briefly) for the goroutine count to come back
// down to base, and reports what it ended up at.
func settleGoroutines(base int) int {
	for i := 0; i < 100; i++ {
		if n := runtime.NumGoroutine(); n <= base {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

func TestM1DistributionOneConnection(t *testing.T) {
	const messages = 20

	first := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	second := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	cfg := m1Config(first.Addr(), second.Addr())
	addr := serve(t, cfg, newRelay(cfg))

	base := runtime.NumGoroutine()
	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	// One connection, held open for every message: the R3 claim.
	for i := 0; i < messages; i++ {
		reply := c.SendMsg(i)
		if want := "250 2.0.0 OK: queued"; len(reply.Lines) != 1 || reply.Lines[0] != want {
			t.Fatalf("message %d verdict = %q, want [%q]", i, reply.Lines, want)
		}
	}
	c.Send("QUIT")
	c.Expect("221")

	// Spread across both backends, one attachment per message.
	if got := first.DialCount(); got != messages/2 {
		t.Errorf("first backend DialCount = %d, want %d", got, messages/2)
	}
	if got := second.DialCount(); got != messages/2 {
		t.Errorf("second backend DialCount = %d, want %d", got, messages/2)
	}

	// Every message intact, on whichever backend took it (R4).
	for k := 0; k < messages/2; k++ {
		first.AssertWireBody(t, k, wantBody(2*k))
		second.AssertWireBody(t, k, wantBody(2*k+1))
	}

	if got := settleGoroutines(base); got > base {
		t.Errorf("goroutines after %d transactions = %d, want back to %d", messages, got, base)
	}
}

func TestM1PipelinedThroughput(t *testing.T) {
	const messages = 20

	first := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	second := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	cfg := m1Config(first.Addr(), second.Addr())
	addr := serve(t, cfg, newRelay(cfg))

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	for i := 0; i < messages; i++ {
		// One RFC 2920 group per message: MAIL, RCPT and DATA in a single
		// segment, replies strictly in command order.
		c.Pipeline(
			fmt.Sprintf("MAIL FROM:<sender%d@test.example>", i),
			fmt.Sprintf("RCPT TO:<rcpt%d@test.example>", i),
			"DATA",
		)
		c.ExpectN("250", "250", "354")
		c.Raw(wantBody(i))
		c.Raw([]byte(".\r\n"))
		reply := c.Expect("250")
		if want := "250 2.0.0 OK: queued"; reply.Lines[0] != want {
			t.Fatalf("message %d verdict = %q, want %q", i, reply.Lines[0], want)
		}
	}
	c.Send("QUIT")
	c.Expect("221")

	if got := first.DialCount(); got != messages/2 {
		t.Errorf("first backend DialCount = %d, want %d", got, messages/2)
	}
	if got := second.DialCount(); got != messages/2 {
		t.Errorf("second backend DialCount = %d, want %d", got, messages/2)
	}
	for k := 0; k < messages/2; k++ {
		first.AssertWireBody(t, k, wantBody(2*k))
		second.AssertWireBody(t, k, wantBody(2*k+1))
	}
}

func TestStreamingCeiling1GB(t *testing.T) {
	// Streaming-only is absolute (decision D7): a 1 GiB message must relay
	// under a 64 MiB heap. The body is generated on the fly and discarded
	// at the backend — buffering it anywhere, at either end, is the thing
	// this test exists to catch.
	const (
		lineLen   = 8192                // body line incl. CRLF
		totalSize = 1 << 30             // 1 GiB
		lines     = totalSize / lineLen // body lines to send
		chunk     = 8                   // lines per write
		heapCap   = 64 << 20            // heap ceiling after the transfer
	)

	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps(), DiscardBody: true})
	cfg := m1Config(srv.Addr())
	addr := serve(t, cfg, newRelay(cfg))

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")
	c.Send("MAIL FROM:<big@test.example>")
	c.Expect("250")
	c.Send("RCPT TO:<rcpt@test.example>")
	c.Expect("250")
	c.Send("DATA")
	c.Expect("354")

	line := strings.Repeat("x", lineLen-2) + "\r\n"
	block := []byte(strings.Repeat(line, chunk))
	for sent := 0; sent < lines; sent += chunk {
		c.Raw(block)
	}
	c.Raw([]byte(".\r\n"))
	c.Expect("250")

	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	if stats.HeapAlloc > heapCap {
		t.Errorf("HeapAlloc after streaming %d bytes = %d, want under %d",
			totalSize, stats.HeapAlloc, heapCap)
	}

	// Bounded memory is only half the claim: every byte has to have
	// arrived, too (the body is discarded at the backend, but counted).
	sessions := srv.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("backend sessions = %d, want 1", len(sessions))
	}
	if got := sessions[0].BodyBytes(); got != totalSize {
		t.Errorf("backend received %d body bytes, want %d", got, totalSize)
	}

	c.Send("QUIT")
	c.Expect("221")
}

// withinTolerance reports whether got is within ±tolerance of want.
func withinTolerance(got, want, tolerance int) bool {
	d := got - want
	return d >= -tolerance && d <= tolerance
}

// weightedSplit runs messages transactions over one connection through
// two backends weighted w1:w2 and returns each backend's DialCount.
func weightedSplit(t *testing.T, w1, w2, messages int) (first, second int) {
	t.Helper()

	firstSrv := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	secondSrv := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	cfg := m1Config(firstSrv.Addr(), secondSrv.Addr())
	cfg.Pools[0].Servers[0].Weight = w1
	cfg.Pools[0].Servers[1].Weight = w2
	addr := serve(t, cfg, newRelay(cfg))

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")
	for i := 0; i < messages; i++ {
		reply := c.SendMsg(i)
		if want := "250 2.0.0 OK: queued"; len(reply.Lines) != 1 || reply.Lines[0] != want {
			t.Fatalf("message %d verdict = %q, want [%q]", i, reply.Lines, want)
		}
	}
	c.Send("QUIT")
	c.Expect("221")

	return firstSrv.DialCount(), secondSrv.DialCount()
}

// TestM1WithRealRouter is the M1 distribution proof (epic 05) run again
// through the real Router instead of the epic-05 stub: weighted
// round-robin's own proportional split now comes from balance.WRR, not
// a hand-rolled test double.
func TestM1WithRealRouter(t *testing.T) {
	const messages = 20

	t.Run("equal weights split evenly", func(t *testing.T) {
		first, second := weightedSplit(t, 1, 1, messages)
		if !withinTolerance(first, messages/2, 2) {
			t.Errorf("first backend DialCount = %d, want %d ±2", first, messages/2)
		}
		if !withinTolerance(second, messages/2, 2) {
			t.Errorf("second backend DialCount = %d, want %d ±2", second, messages/2)
		}
	})

	t.Run("3:1 weights split proportionally", func(t *testing.T) {
		first, second := weightedSplit(t, 3, 1, messages)
		if !withinTolerance(first, 15, 2) {
			t.Errorf("first (weight 3) backend DialCount = %d, want 15 ±2", first)
		}
		if !withinTolerance(second, 5, 2) {
			t.Errorf("second (weight 1) backend DialCount = %d, want 5 ±2", second)
		}
	})
}

// domainRoutedConfig is a listener plus two single-server pools: "promo"
// (selected by a mail_from_domain rule) and "dflt" (the fallthrough
// default).
func domainRoutedConfig(promoAddr, defaultAddr string) *config.Config {
	return &config.Config{
		Defaults: config.Defaults{Timeouts: m1Timeouts()},
		Listener: config.Listener{
			Hostname:     "bifrost.test",
			Capabilities: []string{"PIPELINING", "8BITMIME", "SIZE 104857600"},
		},
		Pools: []config.Pool{
			{
				Name: "promo", Balance: "roundrobin", BackendTLS: "none", EhloName: "bifrost.test",
				Servers: []config.Server{{Name: "p0", Address: promoAddr, Weight: 1}},
			},
			{
				Name: "dflt", Balance: "roundrobin", BackendTLS: "none", EhloName: "bifrost.test",
				Servers: []config.Server{{Name: "d0", Address: defaultAddr, Weight: 1}},
			},
		},
		Routing: config.Routing{
			Rules:       []config.RoutingRule{{MailFromDomain: []string{"promo.example.com"}, Pool: "promo"}},
			DefaultPool: "dflt",
		},
	}
}

// sendOneMessage runs one full MAIL/RCPT/DATA transaction from mailFrom
// and asserts the queued verdict.
func sendOneMessage(t *testing.T, c *smtpdrv.Conn, mailFrom string) {
	t.Helper()
	c.Send("MAIL FROM:<" + mailFrom + ">")
	c.Expect("250")
	c.Send("RCPT TO:<rcpt@test.example>")
	c.Expect("250")
	c.Send("DATA")
	c.Expect("354")
	c.Raw([]byte("Subject: test\r\n\r\nbody\r\n"))
	c.Raw([]byte(".\r\n"))
	reply := c.Expect("250")
	if want := "250 2.0.0 OK: queued"; reply.Lines[0] != want {
		t.Fatalf("MAIL FROM:<%s> verdict = %q, want %q", mailFrom, reply.Lines[0], want)
	}
}

// TestMailFromDomainRoutingEndToEnd proves the relay itself populates
// TxnMeta.MailFromDomain from the wire, not just that the rule engine
// can match a domain in a unit test: a real MAIL FROM:<...@promo.
// example.com> sent over the wire lands on the promo pool's backend, and
// a different domain lands on the default pool's. Without the relay
// actually parsing the MAIL line, this feature ships inert.
func TestMailFromDomainRoutingEndToEnd(t *testing.T) {
	promoSrv := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	defaultSrv := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	cfg := domainRoutedConfig(promoSrv.Addr(), defaultSrv.Addr())
	addr := serve(t, cfg, newRelay(cfg))

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	sendOneMessage(t, c, "sender@promo.example.com")
	sendOneMessage(t, c, "sender@other.example.com")

	c.Send("QUIT")
	c.Expect("221")

	if got := promoSrv.DialCount(); got != 1 {
		t.Errorf("promo backend DialCount = %d, want 1", got)
	}
	if got := defaultSrv.DialCount(); got != 1 {
		t.Errorf("default backend DialCount = %d, want 1", got)
	}
}

// TestDeadPrimarySkippedNoDial proves the no-dial half of the
// "dead-primary-skipped-with-no-added-latency" regression: a server the
// EligibleFunc marks down is never dialed at all, not dialed-and-failed.
// (Epic 05's failover tests prove the opposite property — recovery by
// dialing a server that answers again — this epic owns the no-dial
// assertion.)
func TestDeadPrimarySkippedNoDial(t *testing.T) {
	const messages = 5

	down := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	up := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	cfg := m1Config(down.Addr(), up.Addr()) // s0 = down, s1 = up

	h := &config.Holder{}
	h.Swap(cfg)
	elig := func(_, server string) bool { return server != "s0" }
	router := balance.NewRouter(h, elig, rand.New(rand.NewSource(1)))
	relay := proxy.NewRelay(router.Pick, h, slog.New(slog.DiscardHandler), nil, router.Lease)
	addr := serve(t, cfg, relay)

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")
	for i := 0; i < messages; i++ {
		reply := c.SendMsg(i)
		if want := "250 2.0.0 OK: queued"; len(reply.Lines) != 1 || reply.Lines[0] != want {
			t.Fatalf("message %d verdict = %q, want [%q]", i, reply.Lines, want)
		}
	}
	c.Send("QUIT")
	c.Expect("221")

	if got := down.DialCount(); got != 0 {
		t.Errorf("down backend DialCount = %d, want 0 (never dialed)", got)
	}
	if got := up.DialCount(); got != messages {
		t.Errorf("up backend DialCount = %d, want %d (every message)", got, messages)
	}
}

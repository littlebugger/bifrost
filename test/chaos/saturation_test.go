//go:build chaos

// Package chaos holds epic-08's saturation scenario (chaos: "test/chaos"
// scenario 11, PROJECT.md's Test Strategy). It exercises the real
// accept loop (internal/proxy.Serve) and the real Router
// (internal/balance) together under concurrency neither package's own
// tests drive: many clients racing both global_maxconn and
// max_transactions at once.
package chaos

import (
	"bufio"
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

	"github.com/littlebugger/bifrost/internal/balance"
	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/proxy"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// chaosTimeouts is PROJECT.md's timeout budget, shortened enough that a
// genuinely wedged path fails the test instead of hanging it.
func chaosTimeouts() config.Timeouts {
	return config.Timeouts{
		ClientIdle:       10 * time.Second,
		SessionMax:       time.Minute,
		BackendConnect:   2 * time.Second,
		BackendHandshake: 2 * time.Second,
		BackendMailReply: 5 * time.Second,
		Backend354Wait:   5 * time.Second,
		DataProgress:     5 * time.Second,
		BackendFinalDot:  5 * time.Second,
	}
}

// backendCaps satisfies the listener's capability superset check.
func backendCaps() []string { return []string{"PIPELINING", "8BITMIME", "SIZE 1048576"} }

// chaosConfig is a listener plus one leastconn pool of addrs, each
// capped at perServerCap, and a global_maxconn of maxconn.
func chaosConfig(maxconn, perServerCap int, addrs ...string) *config.Config {
	pool := config.Pool{Name: "p", Balance: "leastconn", BackendTLS: "none", EhloName: "bifrost.chaos"}
	for i, addr := range addrs {
		pool.Servers = append(pool.Servers, config.Server{
			Name: fmt.Sprintf("s%d", i), Address: addr, Weight: 1, MaxTransactions: perServerCap,
		})
	}
	return &config.Config{
		Defaults: config.Defaults{Timeouts: chaosTimeouts()},
		Listener: config.Listener{
			Hostname:     "bifrost.chaos",
			Capabilities: backendCaps(),
		},
		Pools:   []config.Pool{pool},
		Routing: config.Routing{DefaultPool: "p"},
		Limits:  config.Limits{GlobalMaxConn: maxconn},
	}
}

// capGuard wraps Router.Lease with an event-driven assertion: at the
// exact instant a lease is granted (never on a poll or a sleep), the
// server's in-flight count must not exceed its resolved
// max_transactions cap. It also records each server's peak observed
// concurrency, so the test can additionally confirm the cap was
// actually reached, not merely never exceeded.
type capGuard struct {
	t      *testing.T
	router *balance.Router
	pool   string

	mu   sync.Mutex
	peak map[string]int
}

func (g *capGuard) lease(srv *config.Server) func() {
	release := g.router.Lease(srv)
	got := g.router.InFlight(g.pool, srv.Name)

	g.mu.Lock()
	if g.peak == nil {
		g.peak = make(map[string]int)
	}
	if got > g.peak[srv.Name] {
		g.peak[srv.Name] = got
	}
	g.mu.Unlock()

	if srv.MaxTransactions > 0 && got > srv.MaxTransactions {
		g.t.Errorf("server %s: InFlight = %d immediately after a Lease, want <= cap %d", srv.Name, got, srv.MaxTransactions)
	}
	return release
}

func (g *capGuard) peakOf(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak[name]
}

// settleGoroutines waits (briefly, bounded) for the goroutine count to
// come back down to base, and reports what it ended up at -- the same
// bounded-poll idiom test/integration/m1_test.go uses for the same need.
func settleGoroutines(base int) int {
	for i := 0; i < 300; i++ {
		if n := runtime.NumGoroutine(); n <= base {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

// stableGoroutineCount waits (briefly, bounded) for the goroutine count
// to stop changing across consecutive checks and returns it -- for
// capturing a clean baseline right after starting background
// goroutines whose own startup (e.g. Serve's internal watcher) has not
// necessarily finished the instant the go statement that launched them
// returns.
func stableGoroutineCount() int {
	n := runtime.NumGoroutine()
	for i, prev := 0, -1; i < 300 && prev != n; i++ {
		prev = n
		time.Sleep(5 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// TestChaosMaxconnExhaustion is scenario 11: 20 clients against a
// global_maxconn of 10, with a per-server max_transactions of 2 spread
// over 2 backends (4 concurrent transaction slots total) and a scripted
// EOD delay that stretches every transaction long enough for the
// contention to actually happen. It asserts: per-server concurrency
// never exceeds its cap (and does reach it), every client's outcome is
// one the contract accounts for, and both the lease counters and the
// goroutine count return to baseline once every client is done.
func TestChaosMaxconnExhaustion(t *testing.T) {
	const (
		numClients   = 20
		maxconn      = 10
		perServerCap = 2
	)

	fakeA := fakesmtp.Start(t, fakesmtp.Script{
		Caps: backendCaps(), OnEOD: []fakesmtp.Step{{Delay: 150 * time.Millisecond}},
	})
	fakeB := fakesmtp.Start(t, fakesmtp.Script{
		Caps: backendCaps(), OnEOD: []fakesmtp.Step{{Delay: 150 * time.Millisecond}},
	})

	cfg := chaosConfig(maxconn, perServerCap, fakeA.Addr(), fakeB.Addr())
	h := &config.Holder{}
	h.Swap(cfg)
	router := balance.NewRouter(h, func(string, string) bool { return true }, rand.New(rand.NewSource(1)))
	guard := &capGuard{t: t, router: router, pool: "p"}
	relay := proxy.NewRelay(router.Pick, h, slog.New(slog.DiscardHandler), nil, guard.lease)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- proxy.Serve(ctx, ln, cfg, nil, relay, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("Serve returned %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not return after ctx cancel")
		}
	})

	// Serve's own watcher goroutine starts a moment after the go
	// statement above actually gets scheduled -- settle first, or that
	// race reads back as a false "leak" of exactly one goroutine.
	base := stableGoroutineCount()

	var wg sync.WaitGroup
	outcomes := make([]string, numClients)
	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i] = runChaosClient(t, addr, i)
		}(i)
	}
	wg.Wait()

	var completed, rejected, busy int
	for i, o := range outcomes {
		switch o {
		case "completed":
			completed++
		case "rejected":
			rejected++
		case "busy":
			busy++
		default:
			t.Errorf("client %d outcome = %q, want completed, rejected, or busy", i, o)
		}
	}
	t.Logf("completed=%d rejected=%d busy=%d", completed, rejected, busy)
	if completed+rejected+busy != numClients {
		t.Errorf("accounted-for outcomes = %d, want %d", completed+rejected+busy, numClients)
	}
	if rejected == 0 {
		t.Errorf("rejected = 0, want at least one client turned away at accept (maxconn=%d < clients=%d)", maxconn, numClients)
	}
	if completed == 0 {
		t.Errorf("completed = 0, want at least one client to get through")
	}

	maxPeak := 0
	for _, name := range []string{"s0", "s1"} {
		if p := guard.peakOf(name); p > maxPeak {
			maxPeak = p
		}
		if p := guard.peakOf(name); p > perServerCap {
			t.Errorf("server %s: peak concurrency = %d, want <= %d", name, p, perServerCap)
		}
	}
	if maxPeak != perServerCap {
		t.Errorf("max observed per-server concurrency = %d, want exactly %d (the cap was never actually reached)", maxPeak, perServerCap)
	}

	for _, name := range []string{"s0", "s1"} {
		if got := router.InFlight("p", name); got != 0 {
			t.Errorf("InFlight(p,%s) after the run = %d, want 0", name, got)
		}
	}
	if got := settleGoroutines(base); got > base {
		t.Errorf("goroutines after the run = %d, want back to baseline %d", got, base)
	}
}

// runChaosClient drives one client through exactly one attempt: dial,
// and either the accept-overload reply (connection-scoped, no session
// ever exists) or a full EHLO/MAIL/RCPT/DATA transaction, whatever its
// verdict. It never retries -- a clean 451 is as valid a terminal
// outcome here as a completed transaction, per the contract. It reports
// "rejected", "busy", "completed", or "error" (already logged via t).
func runChaosClient(t *testing.T, addr string, i int) string {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Errorf("client %d: dial: %v", i, err)
		return "error"
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Errorf("client %d: set deadline: %v", i, err)
		return "error"
	}
	br := bufio.NewReader(conn)

	first, err := readReplyLine(br)
	if err != nil {
		t.Errorf("client %d: read first reply: %v", i, err)
		return "error"
	}
	if strings.HasPrefix(first, "421") {
		if first != "421 4.3.2 Too many connections, try again later" {
			t.Errorf("client %d: overload reply = %q", i, first)
		}
		if _, err := br.ReadByte(); err == nil {
			t.Errorf("client %d: connection still open after 421, want closed", i)
		}
		return "rejected"
	}
	if first != "220 bifrost.chaos ESMTP" {
		t.Errorf("client %d: banner = %q", i, first)
		return "error"
	}

	write := func(line string) bool {
		if _, err := conn.Write([]byte(line + "\r\n")); err != nil {
			t.Errorf("client %d: write %q: %v", i, line, err)
			return false
		}
		return true
	}

	if !write(fmt.Sprintf("EHLO client%d.example", i)) {
		return "error"
	}
	if _, err := readReplyLine(br); err != nil {
		t.Errorf("client %d: EHLO reply: %v", i, err)
		return "error"
	}

	if !write(fmt.Sprintf("MAIL FROM:<sender%d@chaos.example>", i)) {
		return "error"
	}
	mailReply, err := readReplyLine(br)
	if err != nil {
		t.Errorf("client %d: MAIL reply: %v", i, err)
		return "error"
	}
	if strings.HasPrefix(mailReply, "451") {
		if mailReply != "451 4.3.2 All backends busy, try again later" {
			t.Errorf("client %d: saturated reply = %q", i, mailReply)
		}
		write("QUIT")
		_, _ = readReplyLine(br)
		return "busy"
	}
	if mailReply != "250 2.1.0 OK" {
		t.Errorf("client %d: MAIL reply = %q", i, mailReply)
		return "error"
	}

	if !write(fmt.Sprintf("RCPT TO:<rcpt%d@chaos.example>", i)) {
		return "error"
	}
	if reply, err := readReplyLine(br); err != nil || reply != "250 2.1.5 OK" {
		t.Errorf("client %d: RCPT reply = %q, err %v", i, reply, err)
		return "error"
	}
	if !write("DATA") {
		return "error"
	}
	if reply, err := readReplyLine(br); err != nil || reply != "354 Start mail input; end with <CRLF>.<CRLF>" {
		t.Errorf("client %d: DATA reply = %q, err %v", i, reply, err)
		return "error"
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("body %d\r\n.\r\n", i))); err != nil {
		t.Errorf("client %d: write body: %v", i, err)
		return "error"
	}
	verdict, err := readReplyLine(br)
	if err != nil || verdict != "250 2.0.0 OK: queued" {
		t.Errorf("client %d: verdict = %q, err %v", i, verdict, err)
		return "error"
	}
	write("QUIT")
	_, _ = readReplyLine(br)
	return "completed"
}

// readReplyLine reads one full (possibly multiline) SMTP reply, hand
// parsed like fakesmtp/smtpdrv/proxy's own test fixtures (no
// net/textproto), and returns only its final line, CRLF stripped --
// the verdict text this scenario's assertions compare against.
func readReplyLine(br *bufio.Reader) (string, error) {
	for {
		raw, err := br.ReadString('\n')
		if err != nil {
			return "", err
		}
		line := strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
		if len(line) < 4 || line[3] != '-' {
			return line, nil
		}
	}
}

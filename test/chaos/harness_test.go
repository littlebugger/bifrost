//go:build chaos

package chaos

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/balance"
	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/health"
	"github.com/littlebugger/bifrost/internal/proxy"
)

// This file is the shared harness for epic-10's named chaos scenarios:
// a real accept loop over the real router and relay (and, where the
// scenario is about health, a real checker), plus a client that returns
// errors instead of failing a test — because in a chaos scenario a 451 is
// data, not a failure, and many clients run concurrently on goroutines
// where t.Fatalf is illegal.
//
// Synchronization is event-driven throughout: fakesmtp's OnEvent hooks and
// bounded polls on recorded state, never a sleep chosen to be "long
// enough".

// clientDeadline bounds every client read/write in this package: a wedged
// path must fail a scenario, not hang the suite.
const clientDeadline = 20 * time.Second

// stack is a running balancer: listener, router, relay, and optionally the
// health checker. Everything it starts is joined by t.Cleanup, so the
// package's goleak TestMain (saturation_test.go) holds it to account.
type stack struct {
	addr    string
	router  *balance.Router
	relay   *proxy.Relay
	checker *health.Checker // nil unless started by newHealthStack
}

// newStack runs cfg with every server unconditionally eligible: the
// scenario is about the relay's own behavior, not the checker's.
func newStack(t *testing.T, cfg *config.Config) *stack { return startStack(t, cfg, false) }

// newHealthStack runs cfg with the real health checker probing throughout
// — for the scenarios whose subject IS the interaction between probes and
// traffic.
func newHealthStack(t *testing.T, cfg *config.Config) *stack { return startStack(t, cfg, true) }

func startStack(t *testing.T, cfg *config.Config, withHealth bool) *stack {
	t.Helper()
	holder := &config.Holder{}
	holder.Swap(cfg)
	quiet := slog.New(slog.DiscardHandler)

	s := &stack{}
	elig := func(string, string) bool { return true }
	var sig proxy.HealthSignals
	if withHealth {
		s.checker = health.New(holder, nil, quiet)
		elig, sig = s.checker.Eligible, s.checker
	}
	s.router = balance.NewRouter(holder, elig, rand.New(rand.NewSource(1)))
	s.relay = proxy.NewRelay(s.router.Pick, holder, quiet, sig, s.router.Lease)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.addr = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	if s.checker != nil {
		wg.Add(1)
		go func() { defer wg.Done(); s.checker.Run(ctx) }()
	}
	serveErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		serveErr <- proxy.Serve(ctx, ln, cfg, nil, s.relay, quiet)
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		if err := <-serveErr; err != nil {
			t.Errorf("Serve returned %v, want nil", err)
		}
	})
	return s
}

// chaosCheck is a fast, low-threshold probe config: one failure downs a
// server and one success brings it back, so a health transition lands
// inside a scenario's lifetime without any sleeping.
func chaosCheck() config.CheckParams {
	return config.CheckParams{
		Level: "ehlo", Rise: 1, Fall: 1,
		Interval: 30 * time.Millisecond, DownInterval: 30 * time.Millisecond,
		Timeout: 300 * time.Millisecond,
	}
}

// timeoutsWith is chaosTimeouts with per-scenario adjustments — the way a
// scenario about one timer shortens that timer only.
func timeoutsWith(adjust func(*config.Timeouts)) config.Timeouts {
	t := chaosTimeouts()
	if adjust != nil {
		adjust(&t)
	}
	return t
}

// poolConfig is one pool "p" over addrs (servers named s0, s1, ...), each
// weight 1, with the given backend TLS mode and timeouts.
func poolConfig(timeouts config.Timeouts, tlsMode string, addrs ...string) *config.Config {
	pool := config.Pool{Name: "p", Balance: "roundrobin", BackendTLS: tlsMode, EhloName: "bifrost.chaos"}
	for i, addr := range addrs {
		pool.Servers = append(pool.Servers, config.Server{
			Name: fmt.Sprintf("s%d", i), Address: addr, Weight: 1, Check: chaosCheck(),
		})
	}
	return &config.Config{
		Defaults: config.Defaults{Timeouts: timeouts},
		Listener: config.Listener{Hostname: "bifrost.chaos", Capabilities: backendCaps()},
		Pools:    []config.Pool{pool},
		Routing:  config.Routing{DefaultPool: "p"},
		Limits:   config.Limits{GlobalMaxConn: 256},
	}
}

// conn is one client connection. Every method returns an error instead of
// calling t: scenarios run these on many goroutines, and an unexpected
// reply is usually the thing under test.
type conn struct {
	c  net.Conn
	br *bufio.Reader
}

func dialSMTP(addr string) (*conn, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &conn{c: c, br: bufio.NewReader(c)}, nil
}

func (c *conn) close() { _ = c.c.Close() }

// rst closes abortively, the way a client that crashes mid-message does.
func (c *conn) rst() {
	if tc, ok := c.c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.c.Close()
}

func (c *conn) write(s string) error {
	if err := c.c.SetWriteDeadline(time.Now().Add(clientDeadline)); err != nil {
		return err
	}
	_, err := c.c.Write([]byte(s))
	return err
}

// lines reads one whole (possibly multiline) reply and returns every line,
// CRLF stripped — what a byte-transparency assertion needs.
func (c *conn) lines() ([]string, error) {
	return c.linesWithin(clientDeadline)
}

func (c *conn) linesWithin(d time.Duration) ([]string, error) {
	if err := c.c.SetReadDeadline(time.Now().Add(d)); err != nil {
		return nil, err
	}
	var out []string
	for {
		raw, err := c.br.ReadString('\n')
		if err != nil {
			return out, err
		}
		line := strings.TrimRight(raw, "\r\n")
		out = append(out, line)
		if len(line) < 4 || line[3] != '-' {
			return out, nil
		}
	}
}

// reply reads one whole reply and returns its final line.
func (c *conn) reply() (string, error) {
	lines, err := c.lines()
	if err != nil {
		return "", err
	}
	return lines[len(lines)-1], nil
}

// cmd sends one command and reads its reply.
func (c *conn) cmd(line string) (string, error) {
	if err := c.write(line + "\r\n"); err != nil {
		return "", err
	}
	return c.reply()
}

// greet consumes the banner and sends EHLO.
func (c *conn) greet(name string) error {
	if got, err := c.reply(); err != nil {
		return err
	} else if !strings.HasPrefix(got, "220") {
		return fmt.Errorf("banner = %q, want 220", got)
	}
	got, err := c.cmd("EHLO " + name)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(got, "250") {
		return fmt.Errorf("EHLO reply = %q, want 250", got)
	}
	return nil
}

// message runs one whole transaction whose body carries marker (unique per
// message, so a scenario can prove exactly which messages arrived). It
// returns the first reply that ends the transaction — the end-of-data
// verdict on the happy path, or whichever earlier non-2xx reply stopped
// it, which is what makes a 451 an ordinary result here rather than an
// error.
func (c *conn) message(marker string) (string, error) {
	got, err := c.cmd("MAIL FROM:<sender@chaos.test>")
	if err != nil || !strings.HasPrefix(got, "2") {
		return got, err
	}
	got, err = c.cmd("RCPT TO:<rcpt@chaos.test>")
	if err != nil || !strings.HasPrefix(got, "2") {
		return got, err
	}
	got, err = c.cmd("DATA")
	if err != nil || !strings.HasPrefix(got, "3") {
		return got, err
	}
	if err := c.write(bodyFor(marker)); err != nil {
		return "", err
	}
	return c.reply()
}

// messageBig is message with a filler body of at least size bytes, for
// scenarios that need the balancer to still be streaming when the backend
// breaks: a small body can be written to the backend in full (dot
// included) before a mid-body failure is even observed, which is a
// different row of the contract (the duplicate-delivery window) and a
// different reply.
func (c *conn) messageBig(marker string, size int) (string, error) {
	got, err := c.cmd("MAIL FROM:<sender@chaos.test>")
	if err != nil || !strings.HasPrefix(got, "2") {
		return got, err
	}
	got, err = c.cmd("RCPT TO:<rcpt@chaos.test>")
	if err != nil || !strings.HasPrefix(got, "2") {
		return got, err
	}
	got, err = c.cmd("DATA")
	if err != nil || !strings.HasPrefix(got, "3") {
		return got, err
	}
	if err := c.write("Subject: " + marker + "\r\n\r\n"); err != nil {
		return "", err
	}
	line := strings.Repeat("f", 1022) + "\r\n"
	for sent := 0; sent < size; sent += len(line) {
		if err := c.write(line); err != nil {
			return "", err
		}
	}
	if err := c.write(".\r\n"); err != nil {
		return "", err
	}
	return c.reply()
}

// bodyFor is the wire body (terminator included) of the message carrying
// marker; wantBody is the same body as a backend records it.
func bodyFor(marker string) string { return wantBody(marker) + ".\r\n" }

func wantBody(marker string) string {
	return fmt.Sprintf("Subject: %s\r\n\r\n%s\r\n", marker, marker)
}

// recordedBodies collects every message body the fakes have recorded, as a
// marker->count map keyed on the body text, so a scenario can assert
// exactly-once delivery across a whole run.
func recordedBodies(fakes ...*fakesmtp.Server) map[string]int {
	out := map[string]int{}
	for _, f := range fakes {
		for _, sess := range f.Sessions() {
			for _, msg := range sess.Messages() {
				out[string(msg.WireBody)]++
			}
		}
	}
	return out
}

// waitFor polls cond (bounded) and fails the test if it never holds. Every
// wait in this package goes through here: the condition is always a
// recorded event (a probe result, a delivered message), never elapsed time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitOp waits until (pool p, server) reaches want.
func waitOp(t *testing.T, s *stack, server string, want health.OpState) {
	t.Helper()
	waitFor(t, fmt.Sprintf("server %s to be %v", server, want), func() bool {
		return s.checker.Status("p", server).Op == want
	})
}

// probeCount is (pool p, server)'s completed active probes.
func probeCount(s *stack, server string) int64 {
	var total int64
	for _, n := range s.checker.ProbeCounts("p", server) {
		total += n
	}
	return total
}

// eachClient runs fn on n goroutines, waits for all of them, and hands
// each one its own index. Failures are reported with t.Errorf (legal from
// any goroutine); t.Fatalf is not.
func eachClient(t *testing.T, n int, fn func(i int)) {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// openSession dials and greets, reporting through t (safe from a client
// goroutine) and returning nil when it could not get that far.
func openSession(t *testing.T, addr, name string) *conn {
	c, err := dialSMTP(addr)
	if err != nil {
		t.Errorf("%s: dial: %v", name, err)
		return nil
	}
	t.Cleanup(c.close)
	if err := c.greet(name); err != nil {
		t.Errorf("%s: greet: %v", name, err)
		return nil
	}
	return c
}

// synth is one entry of internal/proxy's synthesized-reply enum as it
// appears on the wire (the constant carries its CRLF; a read line does
// not), so a scenario compares against the contract byte for byte.
func synth(reply string) string { return strings.TrimSuffix(reply, "\r\n") }

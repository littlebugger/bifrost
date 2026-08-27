package main

import (
	"net"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/smtpdrv"
)

// TestPercentileMath is the unit half of the plan's percentile claim:
// given a known, already-sorted latency slice, percentile must return
// an exact, predictable sample (nearest-rank, no interpolation) — no
// histogram library, just slices.Sort (by the caller) plus indexing.
func TestPercentileMath(t *testing.T) {
	lat := make([]float64, 100)
	for i := range lat {
		lat[i] = float64(i + 1) // 1..100, already sorted
	}
	slices.Sort(lat)

	cases := []struct {
		p, want float64
	}{
		{50, 50},
		{95, 95},
		{99, 99},
		{100, 100},
	}
	for _, tc := range cases {
		if got := percentile(lat, tc.p); got != tc.want {
			t.Errorf("percentile(1..100, %v) = %v, want %v", tc.p, got, tc.want)
		}
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("percentile(nil, 50) = %v, want 0", got)
	}
}

// TestLoadgenAgainstFake is the headline in-process claim: C=4
// connections, M=10 messages each against a real fakesmtp backend, every
// message accepted, and the percentiles it reports are populated and
// ordered p50<=p95<=p99<=max.
func TestLoadgenAgainstFake(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING", "8BITMIME"}})

	res := Run(Config{Addr: srv.Addr(), Conns: 4, Msgs: 10, Size: 256})

	if res.Sent != 40 {
		t.Errorf("Sent = %d, want 40", res.Sent)
	}
	if res.Errors != 0 {
		t.Errorf("Errors = %d, want 0", res.Errors)
	}
	if res.MaxMs <= 0 {
		t.Errorf("MaxMs = %v, want > 0 (no latency ever recorded)", res.MaxMs)
	}
	if !(res.P50Ms <= res.P95Ms && res.P95Ms <= res.P99Ms && res.P99Ms <= res.MaxMs) {
		t.Errorf("percentiles not ordered: p50=%v p95=%v p99=%v max=%v", res.P50Ms, res.P95Ms, res.P99Ms, res.MaxMs)
	}
}

// TestLoadgenCountsRefusals scripts every MAIL as a hard refusal (451,
// the contract's saturated-backend reply) and proves loadgen reports it
// as a counted error rather than treating the connection as broken or
// hanging waiting for a verdict that will never arrive the way a plain
// SendMsg-style flow would expect.
func TestLoadgenCountsRefusals(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{
		Caps:   []string{"PIPELINING", "8BITMIME"},
		OnMAIL: []fakesmtp.Step{{Reply: "451 4.3.2 Try again later"}},
	})

	done := make(chan Result, 1)
	go func() { done <- Run(Config{Addr: srv.Addr(), Conns: 2, Msgs: 5, Size: 64}) }()

	select {
	case res := <-done:
		if res.Sent != 0 {
			t.Errorf("Sent = %d, want 0 (every MAIL was refused)", res.Sent)
		}
		if res.Errors != 10 {
			t.Errorf("Errors = %d, want 10", res.Errors)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run hung on scripted refusals, want it to report typed errors and return")
	}
}

// TestLoadgenRatePacing proves -rate paces each connection
// independently via its own time.Ticker: M messages at a fixed
// per-connection rate must take close to M/rate to run regardless of
// how many connections run that schedule concurrently, not however
// long an unpaced burst would take.
func TestLoadgenRatePacing(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING", "8BITMIME"}})

	const (
		conns = 4
		msgs  = 20
		rate  = 40.0 // msgs/sec, each connection paced independently
	)
	want := time.Duration(float64(msgs) / rate * float64(time.Second))

	start := time.Now()
	res := Run(Config{Addr: srv.Addr(), Conns: conns, Msgs: msgs, Rate: rate, Size: 64})
	elapsed := time.Since(start)

	if res.Errors != 0 {
		t.Fatalf("Errors = %d, want 0", res.Errors)
	}
	lower := time.Duration(float64(want) * 0.7)
	upper := time.Duration(float64(want) * 1.5)
	if elapsed < lower || elapsed > upper {
		t.Errorf("elapsed = %v, want within [%v, %v] (target %v paced by rate=%v)", elapsed, lower, upper, want, rate)
	}
}

// buildFakesmtpBin builds cmd/fakesmtp once for this test and returns
// its path.
func buildFakesmtpBin(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "fakesmtp")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/fakesmtp")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/fakesmtp: %v\n%s", err, out)
	}
	return bin
}

// waitForDial polls addr (bounded) until a plain TCP connect succeeds,
// closing each attempt immediately -- the standalone binary needs a
// moment to bind after Start.
func waitForDial(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never able to dial %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFakesmtpBinaryFlags drives the actual built cmd/fakesmtp binary
// (not the internal/fakesmtp package directly): -listen, -caps, and
// -delay must all take effect on the wire, which is only provable by
// running the real binary and talking smtpdrv to it.
func TestFakesmtpBinaryFlags(t *testing.T) {
	bin := buildFakesmtpBin(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}

	cmd := exec.Command(bin, "-listen", addr, "-caps", "PIPELINING,8BITMIME,SIZE 1048576", "-delay", "20ms")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cmd/fakesmtp: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	waitForDial(t, addr)

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	before := time.Now()
	reply := c.Expect("250")
	if elapsed := time.Since(before); elapsed < 20*time.Millisecond {
		t.Errorf("EHLO reply arrived after %v, want at least the scripted -delay 20ms", elapsed)
	}
	joined := strings.Join(reply.Lines, "\n")
	if !strings.Contains(joined, "PIPELINING") || !strings.Contains(joined, "SIZE 1048576") {
		t.Fatalf("EHLO reply = %v, want the -caps flag's capabilities", reply.Lines)
	}

	c.Send("QUIT")
	c.Expect("221")
}

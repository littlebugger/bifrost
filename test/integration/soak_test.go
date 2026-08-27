//go:build integration

package integration

import (
	"os"
	"runtime"
	"sync"
	"testing"

	"github.com/revolee/bifrost/internal/fakesmtp"
	"github.com/revolee/bifrost/internal/smtpdrv"
)

// This file is epic-11's soak suite: sustained-volume and R3-at-depth
// runs that a single short test never exercises. TestMain (m1_test.go)
// already wraps this package in goleak, so every goroutine either of
// these tests starts must be fully joined before the test returns —
// runSessionsConcurrently's wg.Wait and serve's own t.Cleanup are what
// make that true here.
//
// Both tests are skipped unless SOAK=1 (and -short is not passed): they
// run thousands of real transactions and are not part of the everyday
// `make integration` gate. Nightly CI sets SOAK=1 (epic-11 Task 4).

// skipUnlessSoak gates a soak test: it only runs with SOAK=1 set and
// -short not passed, so `go test ./...`/`make integration` never
// silently pays for it.
func skipUnlessSoak(t *testing.T) {
	t.Helper()
	if testing.Short() || os.Getenv("SOAK") != "1" {
		t.Skip("soak: set SOAK=1 (without -short) to run")
	}
}

// soakSession dials addr, runs EHLO plus one message plus QUIT, and
// reports any failure via t.Errorf -- goroutine-safe, unlike the default
// Fatalf a testing.TB-backed Conn would use, which many of these run
// concurrently off the test's own goroutine (see smtpdrv.DialAddr's own
// doc: install a non-aborting reporter before driving a Conn from any
// goroutine other than the one that dialed it).
func soakSession(t *testing.T, addr string, i int) {
	c, err := smtpdrv.DialAddr(addr)
	if err != nil {
		t.Errorf("soak session %d: dial: %v", i, err)
		return
	}
	defer func() { _ = c.Close() }()
	c.SetFail(func(format string, args ...any) {
		t.Errorf("soak session %d: "+format, append([]any{i}, args...)...)
	})

	c.Expect("220")
	c.Send("EHLO soak.test")
	c.Expect("250")
	reply := c.SendMsg(i)
	if want := "250 2.0.0 OK: queued"; len(reply.Lines) != 1 || reply.Lines[0] != want {
		t.Errorf("soak session %d: verdict = %v, want [%q]", i, reply.Lines, want)
	}
	c.Send("QUIT")
	c.Expect("221")
}

// runSessionsConcurrently runs n soak sessions, indexed [start,
// start+n), through a bounded worker pool of width concurrency, and
// waits for all of them to finish -- the "sequential+parallel" shape:
// real concurrency within a batch, batches run one after another by the
// caller so it can checkpoint goroutines/heap between them.
func runSessionsConcurrently(t *testing.T, addr string, start, n, concurrency int) {
	t.Helper()
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i := 0; i < n; i++ {
		idx := start + i
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			soakSession(t, addr, idx)
		}()
	}
	wg.Wait()
}

// TestSoak10kSessions runs 10k short sessions (dial/EHLO/one
// message/QUIT) in three concurrent batches against two healthy
// backends, and asserts the M3 memory/goroutine ceiling: goroutines
// settle back to baseline (±2) once every session is done, and
// HeapAlloc after a forced GC is under 64 MiB and does not grow
// (beyond measurement noise) round over round -- the signature of a
// per-session leak would be exactly that steady climb.
func TestSoak10kSessions(t *testing.T) {
	skipUnlessSoak(t)

	first := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	second := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	cfg := m1Config(first.Addr(), second.Addr())
	addr := serve(t, cfg, newRelay(cfg))

	// heapSlack is deliberately generous: first and second's own
	// Sessions() history retains one small record per accepted
	// connection for the test's later inspection (fakesmtp's own
	// documented behavior, not bifrost's), so total heap grows a little
	// with total session count even with zero leak on bifrost's side.
	// Measured in practice: ~4.8 MiB/round of ~3,333 sessions --
	// comfortably inside this slack and the 64 MiB ceiling either way.
	const (
		totalSessions = 10000
		rounds        = 3
		concurrency   = 50
		heapCap       = 64 << 20 // PROJECT.md's ceiling
		heapSlack     = 8 << 20  // tolerance for GC/runtime bookkeeping noise between rounds
	)

	base := settleGoroutines(runtime.NumGoroutine())

	var heapSamples []uint64
	done := 0
	for r := 0; r < rounds; r++ {
		n := totalSessions / rounds
		if r == rounds-1 {
			n = totalSessions - done // last round absorbs the remainder
		}
		runSessionsConcurrently(t, addr, done, n, concurrency)
		done += n

		settleGoroutines(base)
		runtime.GC()
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		heapSamples = append(heapSamples, stats.HeapAlloc)
		t.Logf("round %d: sessions=%d HeapAlloc=%d goroutines=%d", r, done, stats.HeapAlloc, runtime.NumGoroutine())
	}

	if got := settleGoroutines(base); got > base+2 {
		t.Errorf("goroutines after %d sessions = %d, want <= baseline %d + 2", totalSessions, got, base)
	}

	for i, h := range heapSamples {
		if h > heapCap {
			t.Errorf("round %d: HeapAlloc = %d, want < %d (64 MiB ceiling)", i, h, heapCap)
		}
		if i > 0 && h > heapSamples[i-1]+heapSlack {
			t.Errorf("round %d: HeapAlloc %d > round %d's %d (+%d slack): heap growing round over round, possible leak",
				i, h, i-1, heapSamples[i-1], heapSlack)
		}
	}
}

// TestSoakLongConnection is R3 at depth: one client connection, 5,000
// messages, spread across two backends -- the same claim
// TestM1DistributionOneConnection proves at 20 messages, run two orders
// of magnitude deeper. Every message must get a clean verdict (zero
// errors) and the split must stay close to the configured 1:1 weight.
func TestSoakLongConnection(t *testing.T) {
	skipUnlessSoak(t)
	const messages = 5000

	first := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	second := fakesmtp.Start(t, fakesmtp.Script{Caps: backendCaps()})
	cfg := m1Config(first.Addr(), second.Addr())
	addr := serve(t, cfg, newRelay(cfg))

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	errs := 0
	for i := 0; i < messages; i++ {
		reply := c.SendMsg(i)
		if want := "250 2.0.0 OK: queued"; len(reply.Lines) != 1 || reply.Lines[0] != want {
			errs++
			t.Errorf("message %d verdict = %v, want [%q]", i, reply.Lines, want)
		}
	}
	c.Send("QUIT")
	c.Expect("221")

	if errs != 0 {
		t.Fatalf("%d/%d messages did not get a clean verdict", errs, messages)
	}

	firstN, secondN := first.DialCount(), second.DialCount()
	t.Logf("%d messages split first=%d second=%d", messages, firstN, secondN)
	if firstN+secondN != messages {
		t.Errorf("dial counts %d+%d = %d, want %d", firstN, secondN, firstN+secondN, messages)
	}
	tolerance := messages / 20 // +-5%
	if !withinTolerance(firstN, messages/2, tolerance) {
		t.Errorf("first backend DialCount = %d, want %d +- %d", firstN, messages/2, tolerance)
	}
	if !withinTolerance(secondN, messages/2, tolerance) {
		t.Errorf("second backend DialCount = %d, want %d +- %d", secondN, messages/2, tolerance)
	}
}

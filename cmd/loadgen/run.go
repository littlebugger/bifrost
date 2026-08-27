// Command loadgen drives concurrent long-lived SMTP connections against
// a real server (a standalone cmd/fakesmtp, or the bifrost proxy itself)
// and reports throughput/latency as JSON. See
// docs/plans/epic-11-load-bench.md's Produces block for the flag and
// output contract.
package main

import (
	"bytes"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/littlebugger/bifrost/internal/smtpdrv"
)

// Config is one load run's parameters.
type Config struct {
	Addr     string
	Conns    int     // -c: concurrent long-lived connections
	Msgs     int     // -m: messages sent per connection
	Rate     float64 // -rate: overall messages/sec across every connection; <=0 means unpaced
	Size     int     // -size: DATA body bytes
	Pipeline bool    // -pipeline: send MAIL/RCPT/DATA as one pipelined batch per message
}

// Result is the run's JSON report (docs/plans/epic-11-load-bench.md's
// Produces block: exactly these six fields).
type Result struct {
	Sent   int     `json:"sent"`
	Errors int     `json:"errors"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MaxMs  float64 `json:"max_ms"`
}

// Run drives cfg.Conns concurrent connections, each sending cfg.Msgs
// messages, and returns the aggregate result. Every connection is
// independent (its own dial, its own EHLO); a dial failure counts every
// message that connection never got to attempt as an error, so
// Sent+Errors always equals Conns*Msgs. -rate paces each connection
// independently (its own time.Ticker): total demand scales with Conns,
// matching -c's role as the concurrency knob rather than diluting it
// through one shared, aggregate-wide ticker.
func Run(cfg Config) Result {
	body := buildBody(cfg.Size)

	var (
		mu   sync.Mutex
		lat  []float64
		sent int
		errs int
	)
	record := func(ok bool, ms float64) {
		mu.Lock()
		defer mu.Unlock()
		if ok {
			sent++
			lat = append(lat, ms)
			return
		}
		errs++
	}

	var wg sync.WaitGroup
	for w := 0; w < cfg.Conns; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			runConn(cfg, base, body, record)
		}(w * cfg.Msgs)
	}
	wg.Wait()

	slices.Sort(lat)
	maxMs := 0.0
	if n := len(lat); n > 0 {
		maxMs = lat[n-1]
	}
	return Result{
		Sent:   sent,
		Errors: errs,
		P50Ms:  percentile(lat, 50),
		P95Ms:  percentile(lat, 95),
		P99Ms:  percentile(lat, 99),
		MaxMs:  maxMs,
	}
}

// runConn drives one long-lived connection through cfg.Msgs messages,
// reporting each via record. A dial failure counts every message this
// connection never attempted as an error, so the caller's Sent+Errors
// invariant holds regardless of where a failure happens.
func runConn(cfg Config, base int, body []byte, record func(ok bool, ms float64)) {
	c, err := smtpdrv.DialAddr(cfg.Addr)
	if err != nil {
		for i := 0; i < cfg.Msgs; i++ {
			record(false, 0)
		}
		return
	}
	defer func() { _ = c.Close() }()
	c.SetFail(func(string, ...any) {}) // off the goroutine that would otherwise Fatalf; see smtpdrv.DialAddr

	c.Expect("") // consume the 220 banner -- DialAddr does not, same as Dial; every read after this would otherwise be off by one
	c.Send("EHLO loadgen.test")
	c.Expect("") // don't gate on it: a bad greeting surfaces per-message below anyway

	var pace *time.Ticker
	if cfg.Rate > 0 {
		pace = time.NewTicker(time.Duration(float64(time.Second) / cfg.Rate))
		defer pace.Stop()
	}

	for i := 0; i < cfg.Msgs; i++ {
		if pace != nil {
			<-pace.C
		}
		start := time.Now()
		var reply smtpdrv.Reply
		if cfg.Pipeline {
			reply = sendPipelined(c, base+i, body)
		} else {
			reply = sendSequential(c, base+i, body)
		}
		record(strings.HasPrefix(reply.Code, "2"), msFloat(time.Since(start)))
	}

	c.Send("QUIT")
	c.Expect("")
}

// sendSequential runs one MAIL/RCPT/DATA transaction command by command,
// like a non-pipelining client, and returns its outcome: the first
// non-2xx/non-3xx reply that stops it short, or the end-of-data verdict.
// It deliberately does not use smtpdrv.SendMsg: SendMsg always writes
// the body regardless of what DATA answered, and hardcodes a fixed tiny
// body irrespective of -size — both wrong once a backend can refuse
// mid-transaction (a live 451, e.g. a saturated pool) under load.
func sendSequential(c *smtpdrv.Conn, i int, body []byte) smtpdrv.Reply {
	c.Send(fmt.Sprintf("MAIL FROM:<sender%d@loadgen.test>", i))
	r := c.Expect("") // "": tolerate any code, this run classifies success itself
	if !strings.HasPrefix(r.Code, "2") {
		return r
	}
	c.Send(fmt.Sprintf("RCPT TO:<rcpt%d@loadgen.test>", i))
	r = c.Expect("")
	if !strings.HasPrefix(r.Code, "2") {
		return r
	}
	c.Send("DATA")
	r = c.Expect("")
	if !strings.HasPrefix(r.Code, "3") {
		return r // refused before any body byte was owed -- none is sent
	}
	c.Raw(body)
	return c.Expect("")
}

// sendPipelined is sendSequential's -pipeline counterpart: MAIL, RCPT,
// and DATA go out in one write (smtpdrv.Pipeline), and their three
// replies are read back in that order (RFC 2920 constrains order, not
// timing) before deciding whether a body is owed.
func sendPipelined(c *smtpdrv.Conn, i int, body []byte) smtpdrv.Reply {
	c.Pipeline(
		fmt.Sprintf("MAIL FROM:<sender%d@loadgen.test>", i),
		fmt.Sprintf("RCPT TO:<rcpt%d@loadgen.test>", i),
		"DATA",
	)
	replies := c.ExpectN("", "", "")
	last := replies[len(replies)-1]
	if !strings.HasPrefix(last.Code, "3") {
		return last
	}
	c.Raw(body)
	return c.Expect("")
}

// buildBody returns a DATA body of at least size content bytes, CRLF
// lines followed by the ".\r\n" terminator. No line ever starts with
// "." by construction, so it needs no dot-stuffing.
func buildBody(size int) []byte {
	const lineLen = 78 // conventional safe SMTP line length, CRLF excluded
	var b bytes.Buffer
	for b.Len() < size {
		n := lineLen
		if remaining := size - b.Len(); remaining < n {
			n = remaining
		}
		b.WriteString(strings.Repeat("x", n))
		b.WriteString("\r\n")
	}
	b.WriteString(".\r\n")
	return b.Bytes()
}

// msFloat converts a duration to fractional milliseconds.
func msFloat(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// percentile returns the p-th percentile (0<p<=100) of sorted (must
// already be sorted ascending), nearest-rank: no interpolation, so the
// result is always one of the observed samples. Stdlib only (math.Ceil
// plus indexing) -- slices.Sort is the caller's job, once, over the
// whole collected latency slice.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

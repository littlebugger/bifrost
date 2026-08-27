package proxy

import (
	"bufio"
	"sync"
	"time"
)

// clientWriter is the relay's single-writer discipline. Txn.W is
// unsynchronized and two goroutines write replies through it — the
// session goroutine's synthesized replies and the reply pump's relayed
// backend lines — so every byte the client receives passes this lock.
//
// Each write also re-arms the client's write deadline: the wait before a
// reply can be as long as the final-dot budget (600 s), far past whatever
// deadline was armed for the last client read, and a verdict must never
// be lost to an expired write window.
type clientWriter struct {
	mu      sync.Mutex
	tx      *Txn
	idle    time.Duration
	metrics Metrics // never nil; HandleTransaction defaults it via firstMetrics

	touched bool // bytes have reached the client since the last reset
}

// send relays backend bytes verbatim.
func (w *clientWriter) send(line []byte) error {
	err := w.write(func(bw *bufio.Writer) error {
		_, err := bw.Write(line)
		return err
	})
	if err == nil {
		w.metricsOf().RelayBytes(dirToClient, len(line))
	}
	return err
}

// synth writes one reply from the closed enum in replies.go. The
// synthesized-reply counter is bumped before the write, matching
// touched's own "marked before, not after" rule below: a reply that was
// decided and attempted counts, whether or not a half-closed client
// socket actually received every byte.
func (w *clientWriter) synth(reply string) error {
	w.metricsOf().SynthesizedReply(codeEnhanced(reply))
	return w.write(func(bw *bufio.Writer) error {
		_, err := bw.WriteString(reply)
		return err
	})
}

// metricsOf is nil-safe: writer.go is exercised directly by pre-epic-09
// tests that build a clientWriter by hand with no metrics field set.
func (w *clientWriter) metricsOf() Metrics {
	if w.metrics == nil {
		return noMetrics{}
	}
	return w.metrics
}

func (w *clientWriter) write(fn func(*bufio.Writer) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Marked before the write, not after: a partially written reply has
	// still reached the client, which is what the silent-failover
	// invariant turns on.
	w.touched = true
	if w.idle > 0 {
		if err := w.tx.setWriteDeadline(time.Now().Add(w.idle)); err != nil {
			return err
		}
	}
	if err := fn(w.tx.W); err != nil {
		return err
	}
	return w.tx.W.Flush()
}

// reset marks the start of a batch the client has seen nothing of yet.
func (w *clientWriter) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.touched = false
}

// dirty reports whether any byte about the batch being relayed has
// reached the client — the silent-failover invariant.
func (w *clientWriter) dirty() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.touched
}

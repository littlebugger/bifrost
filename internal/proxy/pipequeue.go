package proxy

import "errors"

// Bounds on one transaction's pipelined command batch (decision D9). The
// numbers are /PROJECT.md's: they exist so PIPELINING can be advertised
// without giving up the streaming-only rule — a client must not be able
// to make Bifrost buffer arbitrarily before a backend exists.
const (
	pipeMaxLines = 32
	pipeMaxBytes = 16 << 10
)

// ErrPipelineOverflow reports a batch past either bound. The session
// answers 421 4.7.0 and closes: the overflow is connection-scoped, since
// there is no way to stay in sync with a client that ignores the limit.
var ErrPipelineOverflow = errors.New("proxy: pipelining queue overflow")

// PipeQueue is one transaction's command batch, in wire order.
//
// Element 0 is the MAIL line that opened the transaction; the rest are
// the commands the client pipelined behind it. The relay engine drains
// the queue and replays it to the backend verbatim, and the client's
// replies come back in the same order (RFC 2920 constrains order, not
// timing).
//
// The zero value is an empty queue. A PipeQueue belongs to one session
// goroutine (plus whatever holds the transaction) and does no locking of
// its own.
type PipeQueue struct {
	lines [][]byte
	bytes int
}

// Push appends one raw command line, terminator included. It returns
// ErrPipelineOverflow — leaving the queue untouched — when the line would
// take the batch past 32 lines or 16 KB. Lines are stored, not copied:
// smtpwire.ReadCommandLine hands out freshly allocated slices, so there
// is nothing to alias.
func (q *PipeQueue) Push(raw []byte) error {
	if len(q.lines) >= pipeMaxLines || q.bytes+len(raw) > pipeMaxBytes {
		return ErrPipelineOverflow
	}
	q.lines = append(q.lines, raw)
	q.bytes += len(raw)
	return nil
}

// Drain returns every queued line in order and empties the queue.
func (q *PipeQueue) Drain() [][]byte {
	lines := q.lines
	q.lines, q.bytes = nil, 0
	return lines
}

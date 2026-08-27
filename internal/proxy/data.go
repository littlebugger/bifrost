package proxy

import (
	"errors"
	"os"

	"github.com/littlebugger/bifrost/internal/backend"
	"github.com/littlebugger/bifrost/internal/smtpwire"
)

// This file is DATA: the relayed 354, the raw body pipe, and the single
// verdict. Nothing here rewrites a body byte — dot-stuffing is
// terminator-preserving, so unstuffing and restuffing would be the
// identity function, and the framer only ever reports where the message
// ends (see smtpwire.DataFramer).

// replyResult is the reply pump's one-shot outcome: the final code of the
// reply it relayed, or the failure that means no verdict was relayed at
// all.
type replyResult struct {
	code int
	line string // the reply's verbatim first line, trimmed (txnlog.go)
	err  error
}

// bodyResult is what streaming the body observed.
type bodyResult struct {
	// clientErr is a client leg that failed before the terminator
	// arrived: an abort, a hangup, or a stalled feed.
	clientErr error
	// legErr is the backend leg no longer taking body bytes — a failed
	// write, a write that timed out (the watchdog's backend side), or the
	// pump reporting the leg dead. From then on the rest of the client's
	// message is consumed and discarded (the contract's discard-to-dot
	// rule) so the session stays in command sync.
	legErr error
	// delivered is true only when the whole body, terminator included,
	// reached a backend that was still alive — the one case in which a
	// verdict can still be expected from it, and the one case that opens
	// the duplicate-delivery window.
	delivered bool

	// verdict is the pump's outcome, when it arrived while the body was
	// still streaming: PROJECT.md's mid-DATA row. got says whether it is
	// set, so the caller knows not to wait on the pump again.
	verdict replyResult
	got     bool
}

// pipeBody runs DATA's second half: the client's message body streams
// through to the backend as a raw pipe while a one-shot reply pump waits
// for the verdict. It always ends the transaction.
//
// The pump is a goroutine for one reason: PROJECT.md's mid-DATA row. A
// backend may answer in the middle of the body (552, or a 421 on its way
// out), and that reply is the transaction's single verdict — it has to
// reach the client the moment it arrives, not after the dot.
func (t *txn) pipeBody() {
	t.done = true

	// The dot budget is armed before the pump's first read: once the pump
	// is blocked in Replies(), a class set later would not apply until the
	// read after that one — and the body can take much longer than the
	// 354 wait this leg was last armed with.
	t.c.SetCommandClass(backend.Dot)
	pump := make(chan replyResult, 1)
	go func() {
		code, line, err := t.relayReply()
		pump <- replyResult{code: code, line: line, err: err}
	}()

	body := t.streamBody(pump)
	res := body.verdict
	if !body.got {
		if !body.delivered {
			// A backend that never received the whole message will never
			// answer it: unblock the pump's read now instead of waiting
			// out the dot budget. This is also the client-abort path (RFC
			// 5321 3.8 — a hard close, never a terminator the backend
			// could mistake for a complete message).
			t.c.Abort()
		}
		res = <-pump // the pump is joined here, on every path
	}

	switch {
	case res.err == nil:
		// A final reply was relayed: the transaction's one verdict,
		// whether it came after the dot or in the middle of the body.
		// Nothing is ever emitted after it, and the dot-reply wait is
		// over — that is the single-verdict rule.
		t.record.dataVerdict = res.line
		t.countTxn(t.srv, verdictClass(res.code, false))
		t.detach(body.delivered)
	case errors.Is(res.err, errBackendClosing):
		// A 421 mid-DATA: translated to RplBackendClosing (451 4.4.2, see
		// relayReply) and already relayed by the time we get here. Same
		// single-verdict rule, and the leg is gone.
		t.record.dataVerdict = trimReply(RplBackendClosing)
		t.countTxn(t.srv, verdictClass(codeOf(RplBackendClosing), true))
		t.dropLeg(res.err)
	case errors.Is(res.err, errReplyTorn):
		// Half a verdict reached the client. It cannot be un-sent and
		// nothing may follow it, so the connection is closed.
		t.dropLeg(res.err)
		t.tx.closeClient()
	case body.clientErr != nil:
		t.clientGone(body.clientErr)
	case body.delivered:
		// The duplicate-delivery window: the backend holds the whole
		// message and may have queued it, but never said so.
		t.r.lg.Warn(msgDuplicateRisk, "server", srvName(t.srv),
			"client", t.tx.ClientIP, "error", res.err)
		t.r.metrics.DuplicateRisk()
		t.record.duplicateRisk = true
		t.record.synth = trimReply(RplBackendTimeout)
		t.countTxn(t.srv, verdictClass(codeOf(RplBackendTimeout), true))
		t.dropLeg(res.err)
		_ = t.cw.synth(RplBackendTimeout)
	default:
		// No verdict, and the client's bytes were consumed all the way to
		// the terminator above, so the session is still in command sync.
		reply := fatalReply(firstErr(body.legErr, res.err))
		t.record.synth = trimReply(reply)
		t.countTxn(t.srv, verdictClass(codeOf(reply), true))
		t.dropLeg(res.err)
		_ = t.cw.synth(reply)
	}
}

// streamBody pipes the client's DATA body to the backend byte for byte
// until the CRLF.CRLF terminator, and never past it: the bytes after the
// terminator are the next command and must stay in the session's reader.
//
// There is no copy buffer. The bytes are handed to the backend straight
// out of the session's own read buffer (Peek, then Discard exactly what
// the framer claimed), which is both allocation-free and the reason
// nothing can be over-read: a separate 32 KB buffer would swallow the
// command sitting behind the dot.
func (t *txn) streamBody(pump <-chan replyResult) bodyResult {
	var (
		framer smtpwire.DataFramer
		res    bodyResult
	)
	w := t.c.Writer()

	for {
		// A reply arriving mid-body is either the transaction's verdict —
		// already relayed by the pump, so the body only has to be
		// consumed from here on — or the leg's death, which switches the
		// pipe to discarding. Either way the client's stream is drained
		// to its terminator: anything less desynchronizes the session.
		if !res.got {
			select {
			case v := <-pump:
				res.verdict, res.got = v, true
				if v.err != nil {
					res.legErr = firstErr(res.legErr, v.err)
				}
			default:
			}
		}

		// The progress watchdog is re-armed per chunk: bytes moving, not
		// a single deadline for a whole multi-gigabyte message.
		if err := t.armBodyDeadline(); err != nil {
			res.clientErr = err
			return res
		}
		chunk, err := t.peekBody()
		if err != nil {
			res.clientErr = err
			return res
		}

		n, done := framer.Feed(chunk)
		if res.legErr == nil {
			if _, err := w.Write(chunk[:n]); err != nil {
				res.legErr = err
			} else {
				t.r.metrics.RelayBytes(dirToBackend, n)
				t.record.bytes += int64(n)
			}
		}
		if _, err := t.tx.R.Discard(n); err != nil {
			res.clientErr = err
			return res
		}
		if done {
			res.delivered = res.legErr == nil
			return res
		}
	}
}

// peekBody blocks until the client has sent more body bytes and returns
// everything buffered, without consuming any of it.
func (t *txn) peekBody() ([]byte, error) {
	if _, err := t.tx.R.Peek(1); err != nil {
		return nil, err
	}
	return t.tx.R.Peek(t.tx.R.Buffered())
}

// armBodyDeadline arms the client-side half of the DATA progress
// watchdog, clamped to the session lifetime like every other client read.
//
// Once a verdict has been relayed this watchdog no longer decides
// anything: the drain that follows is still bounded by it, but its expiry
// then ends a transaction that is already answered, and nothing further
// reaches the client either way (the single-verdict rule wins).
func (t *txn) armBodyDeadline() error {
	return t.armClientRead(t.timeouts().DataProgress)
}

// clientGone ends a transaction the client abandoned or stalled in,
// mid-body or between commands. A timeout is connection-scoped — the
// contract's 421 4.4.2 and a close, which the handler performs itself so
// the session does not sit through a second idle wait to say the same
// thing — and it names its cause: an expired session lifetime is its own
// row of the contract, not an idle client. A hangup has nobody left to
// answer.
func (t *txn) clientGone(err error) {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		reply := RplIdleTimeout
		if t.atLifetime {
			reply = RplSessionLifetime
		}
		t.r.lg.Warn("client leg timed out during a transaction",
			"client", t.tx.ClientIP, "reply", reply)
		_ = t.cw.synth(reply)
		t.tx.closeClient()
	}
	t.detach(false)
}

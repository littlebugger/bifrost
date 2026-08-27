package proxy

import (
	"errors"

	"github.com/revolee/bifrost/internal/smtpwire"
)

// Two-class error latching, decision D12 and the Transparency Contract's
// two-class rule.
//
// A transaction-fatal error — no candidate could be attached, or the
// attached leg died, timed out, or spoke nonsense — latches: every
// remaining command of that transaction is answered from the latch, with
// no backend involved and no dial attempted. It clears at RSET, at the
// next MAIL, and at EHLO (which ends the transaction outright).
//
// Per-recipient verdicts never latch. A 550 for one RCPT is the backend's
// verdict on that recipient and nothing more, so the next RCPT is relayed
// as if it had never happened. Mireka latches everything, which lets one
// unknown user poison every valid recipient behind it; that bug is the
// reason this file is a file.

// answerLatched answers a batch from the latch, if this transaction is
// latched at all, and reports whether it did.
func (t *txn) answerLatched(lines [][]byte) bool {
	if t.latch == "" {
		return false
	}
	t.answerAll(lines, t.latch)
	return true
}

// failLeg drops a leg that failed and answers whatever it left
// unanswered — unless the client's reply stream is already torn, in which
// case saying anything more would corrupt it and the connection is closed
// instead (see errReplyTorn).
func (t *txn) failLeg(err error, unanswered [][]byte) {
	t.dropLeg(err)
	if errors.Is(err, errReplyTorn) {
		t.done = true
		t.tx.closeClient()
		return
	}
	t.latchFatal(err, unanswered)
}

// latchFatal latches the reply err calls for (see fatalReply) and answers
// the commands the failed leg left unanswered.
func (t *txn) latchFatal(err error, unanswered [][]byte) {
	t.latchWith(fatalReply(err), unanswered)
}

// latchWith latches an explicit reply from the enum — the router having
// nothing to offer is not a leg failure — and answers the batch with it.
// This is the one place every attach-failure path converges (directly
// from attachAndRelay, or via latchFatal/failLeg), so it is also the one
// place that counts the transaction's failed conclusion
// (bifrost_transactions_total) and records it for the transaction log
// (txnlog.go): t.lastSrv is the leg dropLeg most recently captured, or
// nil if nothing ever attached at all.
func (t *txn) latchWith(reply string, unanswered [][]byte) {
	t.latch = reply
	code, line := codeOf(reply), trimReply(reply)
	t.record.synth = line
	for _, cmd := range unanswered {
		verb, _ := smtpwire.ParseVerb(cmd)
		t.record.observe(verb, code, line)
	}
	t.countTxn(t.lastSrv, verdictClass(code, true))
	t.answerAll(unanswered, t.latch)
}

// clearLatch reopens the transaction to a fresh backend.
func (t *txn) clearLatch() { t.latch = "" }

// answerAll synthesizes one reply per unanswered command, in command
// order: the contract's "451 per queued command" row, and the reason a
// pipelined batch never leaves a command without an answer.
func (t *txn) answerAll(lines [][]byte, reply string) {
	for range lines {
		if err := t.cw.synth(reply); err != nil {
			return
		}
	}
}

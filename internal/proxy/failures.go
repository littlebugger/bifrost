package proxy

import (
	"errors"
	"os"
)

// This file is the failure half of the Transparency Contract: which
// synthesized reply stands in for a backend that could not answer, plus
// the two failures that need more than a reply — the one sanctioned
// rewrite, and the duplicate-risk record.

// closingCode is the reply code the sanctioned translation applies to.
const closingCode = 421

// errBackendClosing marks a backend 421 that has already been answered.
// The reply was swallowed and a transaction-scoped 451 4.4.2 sent in its
// place — relaying 421 verbatim would announce that the *client's*
// connection is closing, which it is not — and the leg is finished.
// Callers must drop the leg without answering that command again.
var errBackendClosing = errors.New("proxy: backend closing the connection")

// errReplyTorn marks a backend reply that went bad after one of its
// continuation lines had already been relayed. PROJECT.md's
// malformed-reply exception applies: a relayed line cannot be un-sent, so
// injecting a synthesized 451 behind it would corrupt the client's reply
// stream. The leg is dropped and the client connection closed instead.
var errReplyTorn = errors.New("proxy: backend reply torn mid-relay")

// msgDuplicateRisk is the structured event for PROJECT.md's
// duplicate-delivery window: the backend took the whole message,
// terminator included, and then died without a verdict. The client gets
// 451 and may resend a message the backend actually queued — inherent to
// cut-through delivery (Exim spools to avoid it; Bifrost does not queue
// by design), so it is recorded rather than hidden.
const msgDuplicateRisk = "duplicate delivery risk: backend died after the final dot"

// fatalReply is the transaction-scoped reply for a backend leg that
// failed: 451 4.4.2 when it ran out of time (the contract's timeout
// rows), 451 4.4.1 when it died, was reset, or spoke nonsense. Never a
// 5xx — a permanent code would bounce mail a healthy backend would have
// taken (Baton's IOException-to-554 mapping is the cautionary tale).
func fatalReply(err error) string {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return RplBackendTimeout
	}
	return RplBackendLost
}

// firstErr returns a if it is set, else b: the failure that stopped the
// body first is the one that describes what happened.
func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

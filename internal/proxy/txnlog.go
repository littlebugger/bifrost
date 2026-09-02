package proxy

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file is epic-09's structured per-transaction log: one slog record
// per transaction, emitted exactly once when HandleTransaction returns
// (see relay.go), whatever path got it there (a delivered message, a
// latched failure, a client abandoning it, RSET). It never logs message
// body bytes — only counts, verdicts, and metadata — the privacy/size
// rule TestTxnLogNeverLogsBody enforces.

// txnRecord accumulates one transaction's log fields as the relay works
// through it. Every field is either a small bounded string (a reply's
// first line, trimmed) or a count/duration — never body content.
type txnRecord struct {
	start time.Time

	authn string // the authcid from Txn.Authn, "" if the session never authenticated (RFC 4954, see auth.go)

	pool, server string // the last-attached candidate, if any

	mailVerdict int // the MAIL command's own reply code; 0 = none yet
	rcptCount   int
	rcpt2xx     int
	rcpt4xx     int
	rcpt5xx     int
	rcptOther   int

	dataVerdict string // verbatim first line of the end-of-data reply (or an immediate non-354 DATA rejection); "" if DATA was never reached
	bytes       int64  // DATA body bytes streamed to the backend -- a count, never the content

	failoverAttempts int // backend.Dial attempts across every candidate tried

	synth         string // the synthesized reply's trimmed text, if the transaction's conclusion was Bifrost's own rather than relayed
	duplicateRisk bool

	// connEnvelope is this attached conn's envelope ordinal: 1 for a
	// fresh dial, k > 1 for the k-th envelope on a reused one (Task 4).
	// 0 means nothing ever attached — emitLog omits the field entirely
	// then, rather than log a misleading 0.
	connEnvelope int
}

// observe classifies one command's own reply (relayed or, via latchWith,
// synthesized) into the record: MAIL/RCPT verdicts and the RCPT class
// tally. A DATA reply is recorded here only when it is an immediate,
// non-go-ahead rejection (never reached a body) — the go-ahead case, and
// the real end-of-data verdict, are set directly by pipeBody instead
// (data.go), since only it knows when the transaction's TRUE final reply
// has arrived.
func (r *txnRecord) observe(verb string, code int, line string) {
	switch verb {
	case "MAIL":
		r.mailVerdict = code
	case "RCPT":
		r.rcptCount++
		switch code / 100 {
		case 2:
			r.rcpt2xx++
		case 4:
			r.rcpt4xx++
		case 5:
			r.rcpt5xx++
		default:
			r.rcptOther++
		}
	case "DATA":
		if code != goAhead {
			r.dataVerdict = line
		}
	}
}

// rcptVerdictClasses renders the RCPT class tally as a compact summary,
// e.g. "2xx=3,5xx=1" — empty when no RCPT was ever answered.
func (r *txnRecord) rcptVerdictClasses() string {
	var parts []string
	if r.rcpt2xx > 0 {
		parts = append(parts, "2xx="+strconv.Itoa(r.rcpt2xx))
	}
	if r.rcpt4xx > 0 {
		parts = append(parts, "4xx="+strconv.Itoa(r.rcpt4xx))
	}
	if r.rcpt5xx > 0 {
		parts = append(parts, "5xx="+strconv.Itoa(r.rcpt5xx))
	}
	if r.rcptOther > 0 {
		parts = append(parts, "other="+strconv.Itoa(r.rcptOther))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// trimReply strips a reply's CRLF terminator for logging.
func trimReply(s string) string {
	return strings.TrimRight(s, "\r\n")
}

// emitLog writes t.record as one structured slog record. Called exactly
// once, from a defer in HandleTransaction (relay.go), regardless of how
// the transaction concluded.
func (t *txn) emitLog() {
	r := &t.record
	args := []any{
		"client", t.tx.ClientIP.String(),
		"helo", t.tx.Helo,
		"authn", r.authn,
		"pool", r.pool,
		"server", r.server,
		"mail_verdict", r.mailVerdict,
		"rcpt_count", r.rcptCount,
		"rcpt_verdicts_class", r.rcptVerdictClasses(),
		"data_verdict", r.dataVerdict,
		"bytes", r.bytes,
		"duration_ms", time.Since(r.start).Milliseconds(),
		"failover_attempts", r.failoverAttempts,
		"synth", r.synth,
		"duplicate_risk", r.duplicateRisk,
	}
	if r.connEnvelope > 0 {
		args = append(args, "conn_envelope", r.connEnvelope)
	}
	t.r.lg.Info("transaction", args...)
}

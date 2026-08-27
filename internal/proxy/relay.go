package proxy

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/revolee/bifrost/internal/backend"
	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/smtpwire"
)

// goAhead is DATA's go-ahead code: the one reply code the relay keys a
// state change off, and one it never speaks itself (the contract's
// "never synthesize 354" row).
const goAhead = 354

// PickFunc returns the ordered candidate backends for one transaction:
// healthy-filtered, best first, the rest in failover order (epic 07's
// router). An empty list (or an error) means nothing is available and the
// transaction is refused with the contract's 451.
//
// It is a closure, not an interface method, for one structural reason:
// internal/proxy must never import internal/balance (the router imports
// TxnMeta from here), so the dependency is inverted at the wiring site.
type PickFunc func(tx TxnMeta) (candidates []*config.Server, err error)

// LeaseFunc registers one in-flight transaction against a server and
// returns the release to call when it ends — epic 07's counters, epic
// 08's max_transactions. A nil LeaseFunc means no accounting.
//
// A nil release (unlike a nil LeaseFunc) is a denial: the server hit
// its cap between the pick's snapshot and this call, so the caller must
// not treat it as attached, and must not call the nil result.
type LeaseFunc func(srv *config.Server) (release func())

// ErrAllSaturated is the sentinel a PickFunc returns when its pool has
// eligible backends but every one is at its max_transactions cap —
// distinct from plain "nothing eligible" (empty, nil-error): the
// contract answers this with 451 4.3.2 (RplAllBusy), not the
// unhealthy-empty 451 4.4.1. Declared here, not in internal/balance, so
// Router can return it without internal/proxy ever importing
// internal/balance (PickFunc's own closure-inversion reason).
var ErrAllSaturated = errors.New("proxy: all eligible backends are at their max_transactions cap")

// TxnMeta is everything the router may route on.
type TxnMeta struct {
	ClientIP netip.Addr
	Helo     string

	// MailFromDomain is the MAIL FROM sender's domain, lowercased, or ""
	// when there is none (a null sender, or an address the relay could
	// not parse — the backend, not Bifrost, is the syntax authority).
	// Populated by mailFromDomain from the transaction's raw MAIL line.
	MailFromDomain string
}

// mailFromDomain extracts and lowercases the domain from a MAIL command
// line: "MAIL FROM:<user@dom> params" → "dom". It returns "" for a null
// sender (MAIL FROM:<>) or anything it cannot parse — Bifrost only
// routes on this, it never enforces MAIL syntax, so an unparseable line
// just fails open to the default pool and is relayed to the backend
// untouched either way.
func mailFromDomain(line []byte) string {
	_, args := smtpwire.ParseVerb(line)
	lt := bytes.IndexByte(args, '<')
	gt := bytes.IndexByte(args, '>')
	if lt < 0 || gt < 0 || gt < lt {
		return ""
	}
	addr := args[lt+1 : gt]
	at := bytes.LastIndexByte(addr, '@')
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return strings.ToLower(string(addr[at+1:]))
}

// HealthSignals receives the relay's passive health observations (epic
// 06's Checker implements it). Passive signals only ever push a server
// down or accelerate its probing; recovery is always an active-check
// decision.
type HealthSignals interface {
	// DialFailure reports an attach attempt that never became a usable
	// backend leg: TCP failure, handshake failure, or a capability set
	// that is not a superset of what the listener advertises.
	DialFailure(srv *config.Server)

	// TransportError reports a leg that was usable and then failed: a
	// mid-transaction drop, a reply timeout, an unparseable reply, or a
	// 421 from a freshly-attached backend.
	TransportError(srv *config.Server)

	// Success reports that the backend leg behaved — it answered
	// everything Bifrost asked and was let go cleanly, whatever verdicts
	// it gave. It is emitted on every clean detach, a client that
	// abandoned its message included, and is NOT a statement that a
	// message was delivered.
	Success(srv *config.Server)
}

// Relay is the transaction splicer: the TxnHandler that attaches a
// backend at MAIL FROM, relays the envelope and the DATA stream in both
// directions byte-for-byte, and detaches after the verdict.
//
// One Relay serves every session; all per-transaction state lives in the
// txn value HandleTransaction builds on the session's own goroutine.
type Relay struct {
	pick    PickFunc
	cfg     *config.Holder
	lg      *slog.Logger
	sig     HealthSignals
	lease   LeaseFunc
	metrics Metrics

	// legs is every backend leg currently attached to a live transaction,
	// registered at attach and removed at detach (attach.go). It exists
	// for exactly one caller: CloseLegs, the drain force deadline's first
	// step, which has no other way to reach a connection owned by a
	// session goroutine.
	legsMu sync.Mutex
	legs   map[*backend.Conn]struct{}
}

// NewRelay builds the relay. sig and lease may be nil (no health
// signalling, no in-flight accounting); pick and cfg may not. metrics is
// a trailing optional argument (variadic, epic-09) — every pre-epic-09
// call site keeps compiling with no metrics wired in at all (noMetrics).
func NewRelay(pick PickFunc, cfg *config.Holder, lg *slog.Logger, sig HealthSignals, lease LeaseFunc, metrics ...Metrics) *Relay {
	if lg == nil {
		lg = slog.Default()
	}
	if sig == nil {
		sig = noSignals{}
	}
	if lease == nil {
		lease = func(*config.Server) func() { return func() {} }
	}
	return &Relay{
		pick: pick, cfg: cfg, lg: lg, sig: sig, lease: lease,
		metrics: firstMetrics(metrics),
		legs:    make(map[*backend.Conn]struct{}),
	}
}

// noSignals is the do-nothing HealthSignals used before epic 06 wires a
// checker in.
type noSignals struct{}

func (noSignals) DialFailure(*config.Server)    {}
func (noSignals) TransportError(*config.Server) {}
func (noSignals) Success(*config.Server)        {}

// HandleTransaction owns the client connection from MAIL FROM until the
// transaction ends, whatever ends it (see txn.run).
func (r *Relay) HandleTransaction(ctx context.Context, tx *Txn) {
	t := &txn{r: r, tx: tx, cfg: r.cfg.Load()}
	t.cw = &clientWriter{tx: tx, idle: t.timeouts().ClientIdle, metrics: r.metrics}
	t.record.start = time.Now()
	defer t.emitLog()
	defer t.detach(false)
	t.run(ctx)
}

// txn is one transaction's state: the client leg, the backend leg while
// one is attached, and the fatal latch. It belongs to the session
// goroutine; the only field a reply pump touches is cw, which locks.
type txn struct {
	r   *Relay
	tx  *Txn
	cfg *config.Config
	cw  *clientWriter

	srv     *config.Server // attached candidate
	c       *backend.Conn  // attached backend leg
	release func()         // lease release for srv
	broken  bool           // the attached leg failed as transport

	// lastSrv is the most recently attached candidate, kept even after
	// detach() clears srv — the metrics/verdict-labeling code (dropLeg,
	// latchWith) needs a failed leg's identity a few calls after detach
	// already nilled srv out.
	lastSrv *config.Server

	// record accumulates the epic-09 structured transaction log's fields
	// (txnlog.go); emitLog reports it exactly once, from HandleTransaction's
	// defer.
	record txnRecord

	// done ends the transaction: set once the DATA verdict has been
	// relayed (or synthesized), which is the only outcome that closes a
	// transaction from the backend's side.
	done bool

	// latch is the transaction-fatal reply, non-empty once this
	// transaction can no longer reach a backend; see latch.go.
	latch string

	// atLifetime records that the deadline currently armed on the client
	// leg is the session-lifetime cap rather than a phase timer, so an
	// expiry can name its own contract row instead of blaming an idle
	// client. Clock comparisons would be a guess at the boundary.
	atLifetime bool
}

// run drives the transaction: replay the queued batch, then keep serving
// the client's commands until something ends the transaction.
func (t *txn) run(ctx context.Context) {
	t.relay(ctx, t.tx.PipelineQ.Drain())
	for !t.done {
		line, err := t.readCommand()
		if err != nil {
			if t.answerViolation(err) {
				continue // answered in sync, still in the transaction
			}
			t.clientGone(err)
			return
		}
		t.command(ctx, line)
	}
}

// command answers one client command sent inside the transaction, per the
// Transparency Contract's state table.
func (t *txn) command(ctx context.Context, line []byte) {
	switch verb, _ := smtpwire.ParseVerb(line); verb {
	case "MAIL", "RCPT", "DATA":
		t.relay(ctx, [][]byte{line})
	case "RSET":
		t.reset(line)
	case "NOOP":
		_ = t.cw.synth(RplOK)
	case "VRFY":
		_ = t.cw.synth(RplVrfy)
	case "HELP":
		_ = t.cw.synth(RplHelp)
	case "EXPN", "AUTH", "BDAT":
		_ = t.cw.synth(RplNotImplemented)
	case "STARTTLS":
		// RFC 3207 forbids an upgrade inside a transaction, and the
		// session refuses it from the first MAIL onwards for the same
		// reason: same reply, wherever the command lands.
		_ = t.cw.synth(RplBadSequence)
	case "EHLO", "HELO", "QUIT":
		// Not the relay's to answer: EHLO/HELO reset session state (RFC
		// 5321 4.1.4, the STARTTLS latch and the routed greeting
		// included) and QUIT ends the connection — state a handler cannot
		// reach. The leg is closed down and the line handed back.
		t.detach(true)
		t.tx.leaveForSession(line)
		t.done = true
	default:
		_ = t.cw.synth(RplUnknownCmd)
	}
}

// reset answers RSET: relayed while a backend is attached (the client
// gets the backend's own 250), synthesized when there is nothing
// attached. Either way the transaction ends, the latch clears, and the
// next MAIL gets a fresh pick.
func (t *txn) reset(line []byte) {
	t.done = true
	t.clearLatch()
	if t.c == nil {
		_ = t.cw.synth(RplOK)
		return
	}
	if left, err := t.relayBatch([][]byte{line}); err != nil {
		t.failLeg(err, left)
		return
	}
	t.detach(true)
}

// answerViolation answers the two malformed-input rows a session stays in
// sync through — a bare-LF terminator (never relayed anywhere: the
// SMTP-smuggling defense) and an over-long command line — and reports
// whether the transaction carries on.
func (t *txn) answerViolation(err error) bool {
	switch {
	case errors.Is(err, smtpwire.ErrBareLF):
		t.r.lg.Warn("bare LF command line rejected in transaction", "client", t.tx.ClientIP)
		return t.cw.synth(RplBareLf) == nil
	case errors.Is(err, smtpwire.ErrLineTooLong):
		return t.cw.synth(RplLineTooLong) == nil
	default:
		return false
	}
}

// relay puts one command batch through the backend: the queued batch
// first, then one command at a time as the client sends them.
//
// A MAIL clears the fatal latch: it is the start of a new attempt, so it
// gets a fresh pick and a fresh dial (decision D12 — the latch holds
// until RSET, the next MAIL, or EHLO).
func (t *txn) relay(ctx context.Context, lines [][]byte) {
	if len(lines) == 0 {
		return
	}
	if verb, _ := smtpwire.ParseVerb(lines[0]); verb == "MAIL" {
		t.clearLatch()
	}
	switch {
	case t.answerLatched(lines):
		// A fatally latched transaction reaches no backend at all.
	case t.c != nil:
		// Already attached: this batch's predecessors were answered from
		// this leg, so there is no failover left to take.
		if left, err := t.relayBatch(lines); err != nil {
			t.failLeg(err, left)
		}
	default:
		t.attachAndRelay(ctx, lines)
	}
}

// readCommand reads the transaction's next client command, arming the
// client-idle deadline first: whatever deadline a backend phase left in
// force (up to the 600 s dot budget) must not become a licence for the
// client to go quiet.
func (t *txn) readCommand() ([]byte, error) {
	if err := t.armClientRead(t.timeouts().ClientIdle); err != nil {
		return nil, err
	}
	return smtpwire.ReadCommandLine(t.tx.R, maxCommandLine)
}

// armClientRead arms the client read deadline d from now, clamped to the
// session lifetime. Every client read the relay makes goes through here:
// the lifetime cap is an absolute bound on a session, so a transaction
// that keeps moving must not be able to push it out one phase at a time.
func (t *txn) armClientRead(d time.Duration) error {
	var dl time.Time
	if d > 0 {
		dl = time.Now().Add(d)
	}
	e := t.tx.Expiry
	t.atLifetime = !e.IsZero() && (dl.IsZero() || e.Before(dl))
	if t.atLifetime {
		dl = e
	}
	return t.tx.setReadDeadline(dl)
}

// relayBatch replays one command batch to the backend and relays a reply
// per command, in command order (RFC 2920 constrains order, not timing).
// The whole batch goes out first — that is the point of the pipelining
// queue — and the replies are read back one per command.
// It returns the commands the leg left unanswered, and the failure that
// stopped it; (nil, nil) means every command in the batch has its reply.
func (t *txn) relayBatch(lines [][]byte) (unanswered [][]byte, err error) {
	for i, line := range lines {
		if err := t.sendCommand(line); err != nil {
			return lines[i:], err
		}
	}
	for i, line := range lines {
		verb, _ := smtpwire.ParseVerb(line)
		t.c.SetCommandClass(classFor(verb))
		code, replyLine, err := t.relayReply()
		switch {
		case errors.Is(err, errBackendClosing):
			// Answered by the translation (RplBackendClosing, already
			// relayed inside relayReply); the leg is finished.
			t.record.observe(verb, codeOf(RplBackendClosing), trimReply(RplBackendClosing))
			return lines[i+1:], err
		case err != nil:
			return lines[i:], err
		}
		t.record.observe(verb, code, replyLine)
		if verb == "DATA" && code == goAhead {
			// The go-ahead was relayed, so the client is sending body
			// bytes: everything after this is the pipe's, and DATA is
			// always the last command of a batch (RFC 2920 makes it a
			// sync point) so there is nothing left to answer.
			t.pipeBody()
			return nil, nil
		}
	}
	return nil, nil
}

// sendCommand writes one client command line to the backend verbatim,
// terminator included.
func (t *txn) sendCommand(line []byte) error {
	verb, _ := smtpwire.ParseVerb(line)
	t.c.SetCommandClass(classFor(verb))
	err := t.c.SendLine(line)
	if err == nil {
		t.r.metrics.RelayBytes(dirToBackend, len(line))
	}
	return err
}

// classFor maps a command verb to the backend deadline class its reply
// wait belongs to (PROJECT.md's timeout budget).
func classFor(verb string) backend.Class {
	if verb == "DATA" {
		return backend.DataInit
	}
	return backend.MailRcpt
}

// relayReply reads one whole backend reply and relays it to the client
// verbatim, line by line as each arrives — never held back waiting for
// the final line (R4). It returns the reply's final code and the
// verbatim text of its first line (trimmed of its CRLF terminator) —
// the transaction log's data_verdict field (txnlog.go); every other
// caller of relayReply ignores it.
//
// The read deadline is armed once for the whole reply, so a backend that
// dribbles continuation lines cannot extend its budget line by line.
func (t *txn) relayReply() (code int, firstLine string, err error) {
	rr := t.c.Replies()
	relayed := false
	var first string
	for {
		line, code, final, err := rr.Next()
		if err != nil {
			if relayed {
				// A continuation line of this reply is already on the
				// client's wire and cannot be un-sent, so no 451 may be
				// injected behind it (the contract's malformed-reply
				// exception): the only honest end is to close.
				return 0, first, errors.Join(errReplyTorn, err)
			}
			return 0, "", err
		}
		if first == "" {
			first = trimReply(string(line))
		}
		if code == closingCode {
			// The one sanctioned rewrite: the whole reply is swallowed,
			// continuation lines included, and answered once with a
			// transaction-scoped 451 4.4.2 (see errBackendClosing).
			if !final {
				continue
			}
			if err := t.cw.synth(RplBackendClosing); err != nil {
				return 0, first, err
			}
			return code, first, errBackendClosing
		}
		if err := t.cw.send(line); err != nil {
			return 0, first, err
		}
		relayed = true
		if final {
			return code, first, nil
		}
	}
}

// timeouts is the effective timeout budget; a hand-built config with no
// timeouts means no deadlines at all.
func (t *txn) timeouts() config.Timeouts {
	if t.cfg == nil {
		return config.Timeouts{}
	}
	return t.cfg.Defaults.Timeouts
}

// countTxn reports one concluded transaction to Metrics.Transaction,
// attributing it to srv's pool (via poolFor) when srv is non-nil, or to
// "" when nothing ever attached at all (see Metrics.Transaction's doc —
// this is a deliberate simplification: a total-attach failure has no
// single server at fault, so labeling one candidate would misattribute
// it, and there is no cheap way from here to name the pool a PickFunc
// resolved without one ever attaching).
func (t *txn) countTxn(srv *config.Server, class string) {
	var poolName string
	if srv != nil {
		if pool := poolFor(t.cfg, srv); pool != nil {
			poolName = pool.Name
		}
	}
	t.r.metrics.Transaction(poolName, srvName(srv), class)
}

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/smtpwire"
)

// maxCommandLine caps a client command line, terminator included. Over
// it the client gets 500 5.5.2 and the session stays in sync (the
// Transparency Contract's "command line over 4 KB" row). RFC 5321's own
// limit is 512 bytes; 4 KB is deliberately generous for long MAIL/RCPT
// parameter lists.
const maxCommandLine = 4096

// fallbackHostname keeps the banner well-formed if a hand-built config
// arrives without a listener hostname (config.Load always resolves one).
const fallbackHostname = "bifrost"

// closeGrace bounds the write of a goodbye reply after a timer expired.
// The deadline that fired on the read applies to writes as well, so a
// 421 would fail instantly without a fresh window — and an unresponsive
// client must not be able to pin the session goroutine either.
const closeGrace = 5 * time.Second

// TxnHandler owns one mail transaction, from MAIL FROM to the end-of-data
// verdict. The relay engine (epic-05) is the real implementation: it
// attaches a backend, replays the pipelined batch verbatim, pumps replies
// straight back to the client, and synthesizes a 451 if the backend dies.
//
// HandleTransaction is called synchronously on the session's own
// goroutine and owns the client connection until it returns: it reads the
// transaction's remaining commands and body from Txn.R and writes every
// reply the client sees for them to Txn.W. When it returns the
// transaction is over, whatever its outcome, and the session resumes the
// command loop.
type TxnHandler interface {
	HandleTransaction(ctx context.Context, tx *Txn)
}

// Txn is one mail transaction handed to a TxnHandler.
type Txn struct {
	// ClientIP is the client's address for routing rules (epic-07); it
	// is the zero Addr when the connection has no IP peer.
	ClientIP netip.Addr

	// Helo is the argument of the EHLO/HELO that opened the session,
	// verbatim.
	Helo string

	// Authn is the authcid the client authenticated as (RFC 4954, see
	// auth.go), or "" if the session never authenticated. Set once, at
	// mail(), from Session.authnID.
	Authn string

	// PipelineQ is this transaction's command batch in wire order:
	// element 0 is the MAIL line that opened it, followed by the
	// consecutive RCPTs and at most one DATA the client had already
	// pipelined behind it (RFC 2920 sync-point batching — nothing else
	// is ever queued, and DATA body bytes never are).
	PipelineQ *PipeQueue

	// R and W are the client connection. R is positioned on the byte
	// after the last queued command; W is buffered, and the session
	// flushes it once the handler returns.
	//
	// W is not synchronized: a handler that writes from more than one
	// goroutine (epic-05's reply pump) must serialize the writes itself.
	R *bufio.Reader
	W *bufio.Writer

	// Expiry is the session-lifetime deadline (the contract's 1 h
	// backstop), or the zero time when the session has no lifetime cap. A
	// handler that re-arms the connection deadline per phase MUST clamp to
	// it: the cap exists to bound a session absolutely, so a transaction
	// that keeps moving must not be able to extend it one phase at a
	// time.
	Expiry time.Time

	conn net.Conn

	// deferred is a command line the handler read but deliberately did
	// not answer; see leaveForSession.
	deferred []byte
}

// leaveForSession hands one already-read command line back to the
// session, which answers it as if it had read the line itself once the
// handler returns.
//
// It exists for the commands that end a transaction but belong to the
// session's own state machine: EHLO/HELO (RFC 5321 4.1.4 resets session
// state, including the STARTTLS latch and the greeting the next
// transaction is routed by) and QUIT (221 and the connection close are
// the session's, not a transaction's). A handler that answered those
// itself would leave the session's state stale.
func (t *Txn) leaveForSession(raw []byte) {
	t.deferred = raw
}

// closeClient closes the client connection. It is the handler's half of
// the contract's connection-scoped rows: a 421 is always followed by a
// close, and leaving that to the session would mean sitting through
// another idle wait only to say the same thing twice.
func (t *Txn) closeClient() {
	_ = t.conn.Close()
}

// setReadDeadline and setWriteDeadline arm the client connection's two
// directions independently.
//
// The session arms the client-idle deadline before the MAIL that opened
// the transaction, and it stays in force until the handler re-arms it; a
// handler with phase timers of its own (backend reply waits, the DATA
// progress watchdog) re-arms per phase, and the session re-arms again on
// its next read. The two directions are separate because a backend reply
// wait can outlast the deadline armed for the last client read (the
// final-dot budget is 600 s): a reply write re-arms its own window
// without extending the read watchdog that is meanwhile policing a
// stalled client mid-DATA.
func (t *Txn) setReadDeadline(deadline time.Time) error {
	return t.conn.SetReadDeadline(deadline)
}

func (t *Txn) setWriteDeadline(deadline time.Time) error {
	return t.conn.SetWriteDeadline(deadline)
}

// Session is one client connection: the SMTP state machine that owns
// everything outside a mail transaction.
//
// One goroutine runs a Session, and it is the only writer to the client
// socket except while a TxnHandler holds it (see TxnHandler).
type Session struct {
	conn   net.Conn
	cfg    *config.Config
	tlsCfg *tls.Config
	h      TxnHandler
	lg     *slog.Logger

	br *bufio.Reader
	bw *bufio.Writer

	clientIP  netip.Addr
	greeted   bool   // a valid EHLO/HELO has been accepted
	helo      string // that greeting's argument, verbatim
	tlsActive bool
	mailSeen  bool      // a transaction has been attempted on this session
	expiry    time.Time // session-lifetime deadline; zero = unlimited

	// AUTH (RFC 4954), client leg only. authed and authnID, once set,
	// live for the rest of the connection (see reset). authFails counts
	// failed attempts on this connection; the third closes it.
	authed    bool
	authnID   string
	authFails int
}

// NewSession wraps an accepted client connection. tlsCfg carries the
// listener's certificate for the client-leg STARTTLS upgrade and is nil
// when no certificate is configured, in which case STARTTLS is neither
// advertised nor accepted; the caller loads it once at startup from
// cfg.Listener.StartTLS rather than per connection.
func NewSession(conn net.Conn, cfg *config.Config, tlsCfg *tls.Config, h TxnHandler, lg *slog.Logger) *Session {
	if lg == nil {
		lg = slog.Default()
	}
	ip, _ := netip.ParseAddrPort(conn.RemoteAddr().String())
	return &Session{
		conn:     conn,
		cfg:      cfg,
		tlsCfg:   tlsCfg,
		h:        h,
		lg:       lg,
		br:       bufio.NewReaderSize(conn, maxCommandLine),
		bw:       bufio.NewWriter(conn),
		clientIP: ip.Addr().Unmap(),
	}
}

// Run drives the session to completion and closes the connection. It
// returns nil for every outcome the protocol accounts for (QUIT, client
// hangup, a synthesized 421 and close) and the underlying error only
// when the connection failed in a way the session could not answer.
func (s *Session) Run(ctx context.Context) error {
	defer func() { _ = s.conn.Close() }()

	if d := s.timeouts().SessionMax; d > 0 {
		s.expiry = time.Now().Add(d)
	}
	// The banner write is bounded too: a client that connects and never
	// reads must not be able to pin this goroutine.
	if err := s.armDeadline(); err != nil {
		return s.finish(err)
	}
	if err := s.write(RplBanner(s.hostname())); err != nil {
		return s.finish(err)
	}

	for {
		// A canceled context is a drain: finish nothing new, say so,
		// close. A read already in flight is interrupted by the caller
		// closing the connection (epic-10's drain path).
		if ctx.Err() != nil {
			return s.finish(s.goodbye(RplShuttingDown))
		}

		raw, err := s.readCommand(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// The read was interrupted BY the drain (readCommand's own
				// ctx watcher), so this is the shutdown row of the contract,
				// not an idle client: 421 4.3.0, not 421 4.4.2.
				return s.finish(s.goodbye(RplShuttingDown))
			}
			stop, rerr := s.handleReadError(err)
			if rerr != nil {
				return s.finish(rerr)
			}
			if stop {
				return nil
			}
			continue // answered in sync, keep going
		}
		stop, err := s.dispatch(ctx, raw)
		if err != nil {
			return s.finish(err)
		}
		if stop {
			return nil
		}
	}
}

// finish is Run's exit filter: a client that hung up (or reset, or was
// closed under us) is a normal end of session, not a failure to report,
// whichever direction noticed it first — a read returning EOF and a
// reply write hitting a closed pipe are the same event.
func (s *Session) finish(err error) error {
	switch {
	case err == nil,
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrClosedPipe),
		errors.Is(err, net.ErrClosed),
		errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ECONNRESET):
		return nil
	default:
		return err
	}
}

// goodbye writes a connection-scoped 421 with a write window of its own:
// the deadline in force may be the one that just expired (it covers
// writes too), and an unresponsive client must not be able to pin the
// goroutine while we say goodbye.
func (s *Session) goodbye(reply string) error {
	if err := s.conn.SetDeadline(time.Now().Add(closeGrace)); err != nil {
		return err
	}
	return s.write(reply)
}

// dispatch answers one command line. stop is true when the session is
// over (QUIT, or a connection-scoped 421 that has already been written).
func (s *Session) dispatch(ctx context.Context, raw []byte) (stop bool, err error) {
	verb, args := smtpwire.ParseVerb(raw)

	switch verb {
	case "EHLO", "HELO":
		// Both reset all session state (RFC 5321 4.1.4), and both
		// require a domain argument (4.1.1.1) — an empty one is a
		// syntax error, and leaves the session ungreeted.
		s.reset()
		if len(args) == 0 {
			return false, s.write(RplEhloSyntax)
		}
		s.greeted, s.helo = true, string(args)
		if verb == "HELO" {
			return false, s.write(RplHelo(s.hostname()))
		}
		return false, s.write(RplEhlo(s.hostname(), s.capabilities()))
	case "STARTTLS":
		return s.startTLS(args)
	case "AUTH":
		return s.auth(ctx, args)
	case "MAIL":
		return s.mail(ctx, raw)
	case "RCPT", "DATA":
		// Outside a transaction by construction: a TxnHandler owns
		// these while one is open.
		return false, s.write(RplBadSequence)
	case "RSET":
		// No backend is attached here by construction, so there is
		// nothing to abort; epic-05 relays RSET while attached.
		return false, s.write(RplOK)
	case "NOOP":
		return false, s.write(RplOK)
	case "VRFY":
		return false, s.write(RplVrfy)
	case "HELP":
		return false, s.write(RplHelp)
	case "QUIT":
		return true, s.write(RplBye)
	case "EXPN", "BDAT":
		// Not advertised, not supported in v1 (decision D6).
		return false, s.write(RplNotImplemented)
	default:
		return false, s.write(RplUnknownCmd)
	}
}

// mail opens a transaction and hands it to the TxnHandler, which owns
// the client connection (and every reply the client sees) until it
// returns.
func (s *Session) mail(ctx context.Context, raw []byte) (stop bool, err error) {
	if !s.greeted {
		return false, s.write(RplBadSequence)
	}
	if s.cfg.Listener.Auth != nil && !s.authed {
		return false, s.write(RplAuthRequired)
	}

	q, err := s.collectBatch(raw)
	if err != nil {
		s.lg.Warn("pipelining queue overflow", "client", s.clientIP)
		return true, s.goodbye(RplPipelineOverflow)
	}

	s.mailSeen = true
	tx := &Txn{
		ClientIP:  s.clientIP,
		Helo:      s.helo,
		Authn:     s.authnID,
		PipelineQ: q,
		Expiry:    s.expiry,
		R:         s.br,
		W:         s.bw,
		conn:      s.conn,
	}
	s.h.HandleTransaction(ctx, tx)
	// The handler is expected to flush its own replies; flushing again
	// is free and keeps a forgetful handler from stalling the client.
	if err := s.bw.Flush(); err != nil {
		return false, err
	}

	// A command the handler read but left for the session (see
	// leaveForSession) is answered here, still in command order, with a
	// deadline of its own: the handler's last phase timer may well have
	// expired.
	if raw := tx.deferred; raw != nil {
		if err := s.armDeadline(); err != nil {
			return false, err
		}
		return s.dispatch(ctx, raw)
	}
	return false, nil
}

// collectBatch builds the transaction's command batch: the MAIL line
// that opened it, plus the consecutive RCPTs and at most one DATA the
// client had already pipelined behind it. Reading those forward now is
// what lets the relay replay them to the backend as one batch (decision
// D9) instead of round-tripping each.
//
// The stopping rules are RFC 2920's sync points, not "whatever is
// buffered". Everything else — RSET, NOOP, QUIT, STARTTLS, a second
// MAIL, an unknown verb — stays in the buffer for the main command loop,
// which answers it after the transaction and therefore still in command
// order. DATA ends the batch too, because what follows it is message
// body, not commands. Violations (bare LF, over-long) are likewise left
// where they are, to be answered in place.
//
// Only already-buffered lines are taken, so this never blocks. The only
// error it can return is ErrPipelineOverflow.
func (s *Session) collectBatch(raw []byte) (*PipeQueue, error) {
	q := &PipeQueue{}
	if err := q.Push(raw); err != nil {
		return nil, err
	}

	for {
		line, ok := s.peekBufferedLine()
		if !ok {
			return q, nil
		}
		switch verb, _ := smtpwire.ParseVerb(line); verb {
		case "RCPT", "DATA":
			got, err := smtpwire.ReadCommandLine(s.br, maxCommandLine)
			if err != nil {
				// peekBufferedLine already vouched for this line, so an
				// error here can only be a torn buffer; stop reading
				// ahead and let the main loop deal with the connection.
				return q, nil
			}
			if err := q.Push(got); err != nil {
				return nil, err
			}
			if verb == "DATA" {
				return q, nil // the next bytes are body
			}
		default:
			return q, nil // sync point
		}
	}
}

// peekBufferedLine returns the next complete, well-formed command line
// already sitting in the read buffer, without consuming it: a line
// ReadCommandLine can take without blocking and without reporting a
// violation. The slice aliases the buffer, so it is only good until the
// next read.
func (s *Session) peekBufferedLine() ([]byte, bool) {
	buffered := s.br.Buffered()
	if buffered == 0 {
		return nil, false
	}
	peek, err := s.br.Peek(buffered)
	if err != nil {
		return nil, false
	}
	i := bytes.IndexByte(peek, '\n')
	switch {
	case i < 0: // no complete line yet
		return nil, false
	case i+1 > maxCommandLine: // over-long: the main loop answers 500
		return nil, false
	case i < 1 || peek[i-1] != '\r': // bare LF: same
		return nil, false
	default:
		return peek[:i+1], true
	}
}

// readCommand arms the client-idle and session-lifetime deadlines, then
// reads one command line.
//
// A drain cancelling ctx while this read is parked interrupts it (see
// wakeOnCancel), which is what lets an idle session be told 421 4.3.0
// promptly instead of sitting there until the force deadline closes the
// connection under it. The watcher covers exactly the between-commands
// window: a transaction in flight belongs to the TxnHandler, and the
// drain's whole promise is that it gets to finish.
func (s *Session) readCommand(ctx context.Context) ([]byte, error) {
	if err := s.armDeadline(); err != nil {
		return nil, err
	}
	defer s.wakeOnCancel(ctx)()
	return smtpwire.ReadCommandLine(s.br, maxCommandLine)
}

// wakeOnCancel expires the client read deadline as soon as ctx is done,
// unblocking a parked read; the returned func stops the watcher and MUST
// be called (it bounds the goroutine to the read it covers — the same
// pattern backend.Conn.handshake and the health prober use).
//
// Expiring the deadline rather than closing the connection is deliberate:
// the session still has to write its goodbye reply, and goodbye() re-arms
// a window of its own for exactly that.
func (s *Session) wakeOnCancel(ctx context.Context) (stop func()) {
	// The connection is captured here, not read from s inside the
	// goroutine: a STARTTLS upgrade reassigns s.conn on the session's own
	// goroutine, and select may still pick ctx.Done() after stop() has
	// been called — reading the field there would be a data race for no
	// gain (net.Conn's own methods are safe from any goroutine).
	conn := s.conn
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-done:
		}
	}()
	return func() { close(done) }
}

// armDeadline sets the connection deadline to whichever comes first, the
// idle timeout or the session lifetime. The same deadline covers the
// reply write that follows the read; a zero value (a hand-built config
// with no timeouts) means no deadline at all.
func (s *Session) armDeadline() error {
	var dl time.Time
	if idle := s.timeouts().ClientIdle; idle > 0 {
		dl = time.Now().Add(idle)
	}
	if !s.expiry.IsZero() && (dl.IsZero() || s.expiry.Before(dl)) {
		dl = s.expiry
	}
	return s.conn.SetDeadline(dl)
}

// handleReadError turns a failed command read into the contract's reply.
// stop is true when the session is over; the two in-sync violations
// (bare LF, over-long line) are answered and the session continues.
func (s *Session) handleReadError(err error) (stop bool, _ error) {
	switch {
	case errors.Is(err, smtpwire.ErrBareLF):
		// Answered, never relayed, never parsed: the smuggling defense.
		s.lg.Warn("bare LF command line rejected", "client", s.clientIP)
		return false, s.write(RplBareLf)
	case errors.Is(err, smtpwire.ErrLineTooLong):
		return false, s.write(RplLineTooLong)
	case errors.Is(err, os.ErrDeadlineExceeded):
		if !s.expiry.IsZero() && !time.Now().Before(s.expiry) {
			return true, s.goodbye(RplSessionLifetime)
		}
		return true, s.goodbye(RplIdleTimeout)
	default:
		// Including the client simply hanging up: Run's finish maps
		// those to a clean end of session.
		return true, err
	}
}

// reset clears everything an EHLO/HELO resets (RFC 5321 4.1.4): the
// greeting and the session's transaction history. STARTTLS resets the
// same state (RFC 3207).
//
// authed (and authnID) deliberately do NOT reset here: RFC 4954 scopes
// AUTH to the connection, not to the EHLO/RSET-bounded "session" within
// it, and there is no TLS downgrade path an EHLO could be used to walk
// back through to re-open a cleartext hole — so there is nothing a
// forced re-auth would buy that clearing it would not just cost the
// client a redundant round trip for.
func (s *Session) reset() {
	s.greeted, s.helo, s.mailSeen = false, "", false
}

// hostname is the name Bifrost answers with in the banner and EHLO
// reply.
func (s *Session) hostname() string {
	if h := s.cfg.Listener.Hostname; h != "" {
		return h
	}
	return fallbackHostname
}

// tlsOffered reports whether a STARTTLS upgrade is possible right now: a
// certificate is configured and the connection is still plaintext.
func (s *Session) tlsOffered() bool {
	return s.tlsCfg != nil && !s.tlsActive
}

// capabilities is the advertised EHLO set: exactly the statically
// configured list (decision D10 — Bifrost never advertises a capability
// the operator did not configure), minus STARTTLS whenever an upgrade is
// impossible (no certificate, or TLS already active per RFC 3207).
func (s *Session) capabilities() []string {
	caps := s.cfg.Listener.Capabilities
	if s.tlsOffered() {
		return caps
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if !strings.EqualFold(strings.TrimSpace(c), "STARTTLS") {
			out = append(out, c)
		}
	}
	// AUTH is post-TLS only (RFC 4954 strict-PLAIN, decision above): it
	// never appears on the tlsOffered() early-return path above, only
	// once TLS is actually active, and only until the client has
	// authenticated.
	if s.cfg.Listener.Auth != nil && s.tlsActive && !s.authed {
		out = append(out, "AUTH PLAIN")
	}
	return out
}

// timeouts is the effective client-leg timeout set.
func (s *Session) timeouts() config.Timeouts {
	return s.cfg.Defaults.Timeouts
}

// write sends one synthesized reply from replies.go and flushes it.
func (s *Session) write(reply string) error {
	if _, err := s.bw.WriteString(reply); err != nil {
		return err
	}
	return s.bw.Flush()
}

// Package backend implements Bifrost's backend leg: everything about one
// balancer->backend connection. Dial performs the full handshake — TCP
// connect, read the 220 greeting (consumed here, never surfaced: the
// leg-splice rule in PROJECT.md means only MAIL/RCPT/DATA replies during
// an attached transaction are ever relayed to a client), EHLO and its
// capability parse, optional backend-leg STARTTLS with a required
// re-EHLO, and a final capability-superset check against the listener's
// advertised set.
//
// Everything after Dial returns is Conn's job: verbatim command send,
// reply access, per-class deadlines, and best-effort/abrupt teardown. The
// error taxonomy (DialError, HandshakeError, IncompatibleError) is
// errors.As-able so the relay (epic 05) and health prober (epic 06) can
// route on it: DialError/HandshakeError mean "try the next candidate and
// signal passive health"; IncompatibleError means the health verdict
// incompatible.
package backend

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/smtpwire"
)

// maxReplyLine and maxReplyTotal bound one handshake reply the same way
// smtpwire.ReplyReader bounds any other: generous limits (a backend's
// greeting/EHLO is never a large payload) that still protect against an
// unbounded read from a hostile or broken backend.
const (
	maxReplyLine  = 4096
	maxReplyTotal = 65536
)

// Opts configures Dial. Values come from the resolved pool config:
// EhloName = pool.EhloName, TLSMode = pool.BackendTLS ("none" |
// "starttls" | "starttls-verify"; ServerName/CA for "starttls-verify" are
// the caller's job to bake into TLSConfig, along with RootCAs — Dial
// only decides, from TLSMode, whether to require STARTTLS at all and
// whether to verify the certificate it presents), RequiredCaps = the
// listener's advertised capability set (each entry "NAME" or "NAME
// value", e.g. "SIZE 10485760" — the same shape as
// config.Listener.Capabilities).
type Opts struct {
	EhloName     string
	TLSMode      string
	TLSConfig    *tls.Config
	RequiredCaps []string
	Timeouts     config.Timeouts

	// AuthUsername/AuthPassword, when either is non-empty, make Dial
	// originate AUTH PLAIN against the backend after the superset check:
	// the pool's own credentials, not the client's (this is a fresh,
	// balancer-owned exchange with the backend, not a splice of anything
	// the client sent).
	AuthUsername string
	AuthPassword string
}

// Conn is one balancer->backend connection, past the handshake.
//
// readClass/writeClass are separate atomics, not one shared field: D4's
// concurrency shape has one owner goroutine driving SendLine/Writer while
// a second reply-pump goroutine blocks in Replies() (watching for an
// early backend verdict during DATA streaming, PROJECT.md's "mid-DATA
// early backend replies" row) — the write side is mid-body (DataBlock)
// exactly while the read side needs to keep waiting on the long Dot
// budget. One field can't hold both at once, and unsynchronized access
// from two goroutines is a data race regardless (see conn_test.go's
// TestConcurrentReadWriteClassNoRace).
type Conn struct {
	conn     net.Conn
	br       *bufio.Reader
	rr       *smtpwire.ReplyReader
	addr     string
	timeouts config.Timeouts
	caps     CapSet

	readClass  atomic.Int32 // Class consulted by Replies (SetReadDeadline)
	writeClass atomic.Int32 // Class consulted by SendLine/Writer (SetWriteDeadline)
}

// Caps returns the backend's parsed capability set from the most recent
// EHLO — the post-TLS EHLO's set when STARTTLS was used (RFC 3207
// requires a fresh EHLO after the upgrade, and its capabilities are the
// ones that actually apply from then on).
func (c *Conn) Caps() CapSet { return c.caps }

// DialError reports a failure to establish the TCP connection itself:
// refused, timed out, or a DNS/resolution failure. The relay tries the
// next candidate; the health prober takes it as a passive down signal.
type DialError struct {
	Addr string
	Err  error
}

func (e *DialError) Error() string { return fmt.Sprintf("backend: dial %s: %v", e.Addr, e.Err) }
func (e *DialError) Unwrap() error { return e.Err }

// HandshakeError reports a failure after the TCP connection was
// established but before it is usable: a bad or missing greeting, EHLO
// rejected, a STARTTLS/TLS failure, or the whole-phase handshake budget
// expiring. Stage names which part of the handshake failed ("greeting",
// "ehlo", "starttls", "tls-handshake", "ehlo-after-tls").
type HandshakeError struct {
	Addr  string
	Stage string
	Err   error
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("backend: handshake with %s failed at %s: %v", e.Addr, e.Stage, e.Err)
}
func (e *HandshakeError) Unwrap() error { return e.Err }

// Dial performs the whole backend handshake and returns a ready-to-use
// Conn: TCP connect (bounded by Timeouts.BackendConnect) -> read the 220
// greeting -> EHLO opts.EhloName and parse its capabilities -> optional
// backend-leg STARTTLS with a re-EHLO -> verify the result is a superset
// of opts.RequiredCaps. The greeting and EHLO replies are consumed here
// and never surfaced to any client.
func Dial(ctx context.Context, srv *config.Server, opts Opts) (*Conn, error) {
	dialCtx := ctx
	if d := opts.Timeouts.BackendConnect; d > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	rawConn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", srv.Address)
	if err != nil {
		return nil, &DialError{Addr: srv.Address, Err: err}
	}

	c := &Conn{addr: srv.Address, timeouts: opts.Timeouts}
	if err := c.handshake(ctx, rawConn, opts); err != nil {
		_ = c.conn.Close()
		return nil, err
	}

	if err := checkSuperset(c.caps, opts.RequiredCaps); err != nil {
		_ = c.conn.Close()
		return nil, err
	}

	if opts.AuthUsername != "" || opts.AuthPassword != "" {
		if !c.caps.HasAuthPlain() {
			_ = c.conn.Close()
			return nil, &IncompatibleError{Missing: []string{"AUTH PLAIN"}}
		}
		if err := c.authenticate(opts.AuthUsername, opts.AuthPassword); err != nil {
			_ = c.conn.Close()
			return nil, err
		}
	}

	return c, nil
}

// handshake runs the connection through greeting, EHLO, and optional
// STARTTLS, populating c.conn/br/rr/caps. Two mechanisms bound it, for
// two different failure shapes:
//
//   - One deadline, computed once up front and never reset before a
//     later read, covers greeting+EHLO+TLS as a whole: a backend that
//     dribbles its reply out slowly must be caught by
//     Timeouts.BackendHandshake in total, not evade it one byte or one
//     stage at a time (see conn_test.go's TestHandshakePhaseDeadline).
//   - A watcher goroutine closes rawConn the moment ctx is done, so a
//     caller-cancelled context interrupts a blocked handshake read
//     promptly instead of waiting out the deadline above (see
//     TestDialCtxCancelMidConnect). done/close(done) bound that
//     goroutine's lifetime to this call, so it never outlives handshake
//     (and never leaks — see the integration goleak test).
func (c *Conn) handshake(ctx context.Context, rawConn net.Conn, opts Opts) error {
	c.conn = rawConn
	c.br = bufio.NewReader(rawConn)
	c.rr = smtpwire.NewReplyReader(c.br, maxReplyLine, maxReplyTotal)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = rawConn.Close()
		case <-done:
		}
	}()

	if d := opts.Timeouts.BackendHandshake; d > 0 {
		if err := rawConn.SetDeadline(time.Now().Add(d)); err != nil {
			return &HandshakeError{Addr: c.addr, Stage: "setup", Err: err}
		}
	}

	if err := readGreeting(c.rr); err != nil {
		return &HandshakeError{Addr: c.addr, Stage: "greeting", Err: err}
	}

	caps, err := c.doEHLO(opts.EhloName)
	if err != nil {
		return &HandshakeError{Addr: c.addr, Stage: "ehlo", Err: err}
	}
	c.caps = caps

	if opts.TLSMode != "" && opts.TLSMode != "none" {
		if err := c.startTLS(opts); err != nil {
			return err
		}
	}

	// Per-class deadlines (Conn.SetCommandClass) take over from here; the
	// handshake's own budget must not leak into whatever the caller does
	// next.
	return c.conn.SetDeadline(time.Time{})
}

// readGreeting reads a (possibly multiline) 220 greeting and reports an
// error for anything else, read failure included.
func readGreeting(rr *smtpwire.ReplyReader) error {
	for {
		_, code, final, err := rr.Next()
		if err != nil {
			return fmt.Errorf("read greeting: %w", err)
		}
		if final {
			if code != 220 {
				return fmt.Errorf("greeting code %d, want 220", code)
			}
			return nil
		}
	}
}

// doEHLO sends "EHLO <ehloName>" and parses the (possibly multiline)
// reply into a CapSet. A non-250 final code is reported as an error;
// the raw lines are handed to parseCaps regardless of how many there
// are, exactly as smtpwire.ReplyReader streamed them.
func (c *Conn) doEHLO(ehloName string) (CapSet, error) {
	if _, err := c.conn.Write([]byte("EHLO " + ehloName + "\r\n")); err != nil {
		return nil, fmt.Errorf("write EHLO: %w", err)
	}

	var lines [][]byte
	for {
		line, code, final, err := c.rr.Next()
		if err != nil {
			return nil, fmt.Errorf("read EHLO reply: %w", err)
		}
		lines = append(lines, line)
		if final {
			if code != 250 {
				return nil, fmt.Errorf("EHLO rejected: code %d", code)
			}
			break
		}
	}
	return parseCaps(lines), nil
}

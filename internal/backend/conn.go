package backend

import (
	"io"
	"time"

	"github.com/revolee/bifrost/internal/smtpwire"
)

// Class identifies which phase of a mail transaction a backend command
// belongs to, so SendLine, Replies, and Writer arm the deadline
// PROJECT.md's timeout budget assigns that phase before their next I/O.
// The zero value, MailRcpt, is deliberate: MAIL/RCPT are the common
// case, and a caller that never touches SetCommandClass still gets the
// right deadline for them.
type Class int

// MailRcpt is the zero value and default class. The other three cover
// DATA's sub-phases, each with its own row in PROJECT.md's timeout table.
const (
	MailRcpt  Class = iota // backend MAIL/RCPT reply (Timeouts.BackendMailReply)
	DataInit               // backend 354 wait (Timeouts.Backend354Wait)
	DataBlock              // DATA progress watchdog, re-armed per write (Timeouts.DataProgress)
	Dot                    // backend final-dot reply (Timeouts.BackendFinalDot)
)

// quitDeadline bounds Quit's best-effort wait for a reply. Fixed, not
// configurable from Timeouts: teardown must never block on a wedged
// backend, regardless of how the pool's own timeouts are tuned.
const quitDeadline = 2 * time.Second

// SetCommandClass sets the class Replies arms its read deadline from —
// "the reply class" epic-05's owner goroutine calls this before waiting
// on a command's response. It also seeds the write class SendLine uses
// next (the two are the same class for every sequential command/reply
// exchange: MAIL, RCPT, the DATA "354" line). Writer is the one
// exception: it always re-arms the write side to DataBlock itself,
// regardless of what SetCommandClass last set (see dataWriter.Write) —
// so calling this from the owner goroutine while a reply-pump goroutine
// concurrently calls Replies() for its own (different) class never
// disturbs Writer's write deadline, and Writer's per-write re-arming
// never disturbs the pump's read deadline. See the Conn doc comment.
func (c *Conn) SetCommandClass(cl Class) {
	c.readClass.Store(int32(cl))
	c.writeClass.Store(int32(cl))
}

// classDeadline returns cl's configured duration, 0 meaning "no
// deadline" (a hand-built config with all-zero Timeouts).
func (c *Conn) classDeadline(cl Class) time.Duration {
	switch cl {
	case DataInit:
		return c.timeouts.Backend354Wait
	case DataBlock:
		return c.timeouts.DataProgress
	case Dot:
		return c.timeouts.BackendFinalDot
	default: // MailRcpt
		return c.timeouts.BackendMailReply
	}
}

// armReadDeadline sets only the connection's read deadline, from
// readClass — SetWriteDeadline is untouched, so a concurrent write-side
// arm (SendLine or Writer, on another goroutine) can never cut this
// short or extend it.
func (c *Conn) armReadDeadline() error {
	d := c.classDeadline(Class(c.readClass.Load()))
	if d <= 0 {
		return c.conn.SetReadDeadline(time.Time{})
	}
	return c.conn.SetReadDeadline(time.Now().Add(d))
}

// armWriteDeadline sets only the connection's write deadline, from
// writeClass — SetReadDeadline is untouched, so this can never stomp a
// reply-pump goroutine's concurrently-armed read deadline.
func (c *Conn) armWriteDeadline() error {
	d := c.classDeadline(Class(c.writeClass.Load()))
	if d <= 0 {
		return c.conn.SetWriteDeadline(time.Time{})
	}
	return c.conn.SetWriteDeadline(time.Now().Add(d))
}

// SendLine writes raw bytes to the backend verbatim, terminator
// included: it never adds, strips, or rewrites a byte (R4). Only the
// write deadline is armed first (from the write class — see
// SetCommandClass), never the read deadline.
func (c *Conn) SendLine(raw []byte) error {
	if err := c.armWriteDeadline(); err != nil {
		return err
	}
	_, err := c.conn.Write(raw)
	return err
}

// Replies returns the connection's shared reply reader, having armed
// only the read deadline (from the read class — see SetCommandClass)
// for the read that follows; the write deadline is untouched. One
// smtpwire.ReplyReader serves the whole connection; callers key their
// state machine off each Next() call's code and final.
func (c *Conn) Replies() *smtpwire.ReplyReader {
	// Best-effort: a failing SetReadDeadline means a dead connection, and
	// the Next() call the caller makes next surfaces that same failure as
	// a read error.
	_ = c.armReadDeadline()
	return c.rr
}

// dataWriter is the io.Writer Conn.Writer returns: every Write re-arms
// the write side to the DataBlock progress deadline first — regardless
// of whatever class SetCommandClass most recently set — so a backend
// that goes quiet mid-body still trips the watchdog on the next chunk
// rather than only once at DATA's start, and so that a concurrent
// reply-pump goroutine's read deadline (Replies, a different class
// entirely during DATA) is never affected: only SetWriteDeadline is
// touched here.
type dataWriter struct{ c *Conn }

func (w dataWriter) Write(p []byte) (int, error) {
	w.c.writeClass.Store(int32(DataBlock))
	if err := w.c.armWriteDeadline(); err != nil {
		return 0, err
	}
	return w.c.conn.Write(p)
}

// Writer returns an io.Writer over the raw backend connection: the DATA
// body's pipe target. See dataWriter.
func (c *Conn) Writer() io.Writer { return dataWriter{c} }

// Quit sends a best-effort QUIT and gives the backend up to quitDeadline
// to reply before the connection is closed either way. Teardown must
// never block on a wedged backend.
func (c *Conn) Quit() {
	defer func() { _ = c.conn.Close() }()

	if err := c.conn.SetDeadline(time.Now().Add(quitDeadline)); err != nil {
		return
	}
	if _, err := c.conn.Write([]byte("QUIT\r\n")); err != nil {
		return
	}
	for {
		_, _, final, err := c.rr.Next()
		if err != nil || final {
			return
		}
	}
}

// Abort hard-closes the connection: no dot, no QUIT (RFC 5321 §3.8).
// This is the client-abort-mid-DATA path — the backend must see a bare
// disconnect, never a terminator that would make it think the message
// completed.
func (c *Conn) Abort() {
	_ = c.conn.Close()
}

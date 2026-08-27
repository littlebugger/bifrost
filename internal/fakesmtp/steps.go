package fakesmtp

import (
	"net"
	"time"
)

// Action overrides how a Step delivers its reply.
type Action int

// ActReply is the default: write the reply normally. The others override
// it to close or hang the connection instead of replying.
const (
	ActReply    Action = iota // write the reply normally (default)
	ActDropConn               // close the connection cleanly, no reply
	ActRST                    // close the connection with RST, no reply
	ActHang                   // never reply; block until the server stops
)

// stepCursors tracks, per session, how many steps have been consumed from
// each verb's queue so far.
type stepCursors struct {
	ehlo, mail, rcpt, data, eod, rset, quit int
}

// nextStep returns the step this call to a per-verb queue should use,
// advancing idx. An empty queue always returns the zero Step (the caller
// falls back to its own default reply). Once idx reaches the end of the
// queue, the last step keeps repeating.
func nextStep(steps []Step, idx *int) Step {
	if len(steps) == 0 {
		return Step{}
	}
	i := *idx
	if i >= len(steps) {
		return steps[len(steps)-1]
	}
	*idx++
	return steps[i]
}

// writeStep executes one scripted step: sleep for Delay, then either
// perform Action or write Reply (falling back to def when Reply is
// empty), paced by Drip if set. It reports whether the connection is
// still usable for further commands.
func writeStep(srv *Server, conn net.Conn, step Step, def string) bool {
	if step.Delay > 0 {
		time.Sleep(step.Delay)
	}

	switch step.Action {
	case ActDropConn:
		closeQuietly(conn)
		return false
	case ActRST:
		rstClose(conn)
		return false
	case ActHang:
		<-srv.done
		return false
	}

	text := step.Reply
	if text == "" {
		text = def
	}
	return writeReplyText(conn, text, step.Drip)
}

// writeReplyText writes text plus a trailing CRLF, optionally pacing the
// write one byte at a time with a sleep of drip between bytes.
func writeReplyText(conn net.Conn, text string, drip time.Duration) bool {
	data := []byte(text + "\r\n")
	if drip <= 0 {
		_, err := conn.Write(data)
		return err == nil
	}
	for i, b := range data {
		if _, err := conn.Write([]byte{b}); err != nil {
			return false
		}
		if i < len(data)-1 {
			time.Sleep(drip)
		}
	}
	return true
}

// rstClose closes conn abortively (TCP RST) when possible, falling back
// to a plain close for connection types that don't support SetLinger
// (e.g. after a STARTTLS upgrade).
func rstClose(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	closeQuietly(conn)
}

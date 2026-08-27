package fakesmtp

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// Event is one raw line the fake received from a client, in the order it
// arrived. Verb is the uppercased command verb for a command line (e.g.
// "MAIL"), or "" for a line received while inside a DATA body.
type Event struct {
	SessionID int
	Verb      string
	Line      []byte // raw bytes exactly as received, terminator included
}

// Msg is one completed mail transaction recorded by a session.
type Msg struct {
	// From is MAIL's raw argument text exactly as received on the wire:
	// the "FROM:" keyword and any ESMTP params included verbatim (e.g.
	// "FROM:<a@b> BODY=8BITMIME"), not just the bare address. The fake
	// records wire truth, not a parsed envelope — see commandArgs.
	From string
	// Rcpts holds each RCPT's raw argument text in the same raw form
	// (e.g. "TO:<a@b>"), one per RCPT command, in order.
	Rcpts    []string
	WireBody []byte // still dot-stuffed, byte-exact, terminator excluded
}

// Session is one accepted connection's recorded activity. Obtain sessions
// via Server.Sessions; the zero value is not meaningful.
type Session struct {
	st *sessionState
}

// Transcript returns every line the session received, in order.
func (s *Session) Transcript() []Event {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	out := make([]Event, len(s.st.transcript))
	copy(out, s.st.transcript)
	return out
}

// BodyBytes returns how many DATA body bytes the session has received,
// terminators excluded, across every transaction on it. It counts even
// when Script.DiscardBody drops the content.
func (s *Session) BodyBytes() int64 {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	return s.st.bodyBytes
}

// Messages returns every mail transaction the session completed.
func (s *Session) Messages() []Msg {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	out := make([]Msg, len(s.st.messages))
	copy(out, s.st.messages)
	return out
}

// sessionState is the mutable state behind a Session. Session itself
// stays a small copyable pointer wrapper (no embedded mutex) so
// Server.Sessions can hand out []Session by value without tripping
// go vet's copylocks check.
type sessionState struct {
	mu          sync.Mutex
	id          int
	discardBody bool

	transcript []Event
	messages   []Msg

	curFrom  string
	curRcpts []string
	curBody  bytes.Buffer

	// bodyBytes counts every DATA body byte received on this session,
	// terminator excluded, whether or not DiscardBody is set: a
	// large-message test discards the content but still has to prove the
	// whole message arrived.
	bodyBytes int64
}

func newSessionState(id int, discardBody bool) *sessionState {
	return &sessionState{id: id, discardBody: discardBody}
}

// recordLine appends a raw line to the transcript and returns the Event
// recorded for it. verb is "" for non-command (DATA body) lines.
func (st *sessionState) recordLine(verb string, raw []byte) Event {
	st.mu.Lock()
	defer st.mu.Unlock()
	line := append([]byte(nil), raw...)
	ev := Event{SessionID: st.id, Verb: verb, Line: line}
	st.transcript = append(st.transcript, ev)
	return ev
}

// startMail begins a new transaction at MAIL FROM.
func (st *sessionState) startMail(from string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.curFrom = from
	st.curRcpts = nil
	st.curBody.Reset()
}

// addRcpt records one recipient of the in-progress transaction.
func (st *sessionState) addRcpt(rcpt string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.curRcpts = append(st.curRcpts, rcpt)
}

// recordBodyLine records one DATA body line — into the transcript and
// into the in-progress transaction's wire body — unless DiscardBody is
// set, in which case it does neither: DiscardBody exists to bound memory
// for large-message tests, which a transcript entry per line would defeat
// just as surely as accumulating the body would.
func (st *sessionState) recordBodyLine(raw []byte) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.bodyBytes += int64(len(raw))
	if st.discardBody {
		return
	}
	line := append([]byte(nil), raw...)
	st.transcript = append(st.transcript, Event{SessionID: st.id, Verb: "", Line: line})
	st.curBody.Write(raw)
}

// finishMessage closes out the in-progress transaction as a completed Msg,
// regardless of what verdict the script sent for it: the recorder's job
// is to capture what arrived on the wire, not to judge it.
func (st *sessionState) finishMessage() {
	st.mu.Lock()
	defer st.mu.Unlock()
	body := append([]byte(nil), st.curBody.Bytes()...)
	st.messages = append(st.messages, Msg{From: st.curFrom, Rcpts: st.curRcpts, WireBody: body})
	st.curFrom = ""
	st.curRcpts = nil
	st.curBody.Reset()
}

// resetTransaction discards an in-progress transaction (RSET) without
// recording a Msg.
func (st *sessionState) resetTransaction() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.curFrom = ""
	st.curRcpts = nil
	st.curBody.Reset()
}

// DialCount returns the number of connections ever accepted.
func (srv *Server) DialCount() int {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.dialCount
}

// CmdCount returns how many times verb (case-insensitive) has been
// received across every session.
func (srv *Server) CmdCount(verb string) int {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.cmdCounts[strings.ToUpper(verb)]
}

// bumpCmdCount records one more occurrence of verb.
func (srv *Server) bumpCmdCount(verb string) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.cmdCounts == nil {
		srv.cmdCounts = make(map[string]int)
	}
	srv.cmdCounts[verb]++
}

// Sessions returns every session accepted so far, in accept order.
func (srv *Server) Sessions() []Session {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	out := make([]Session, len(srv.sessions))
	for i, st := range srv.sessions {
		out[i] = Session{st: st}
	}
	return out
}

// AssertWireBody asserts that the msgIdx-th message recorded across all
// sessions (in accept order) has exactly the given wire body.
func (srv *Server) AssertWireBody(t testing.TB, msgIdx int, want []byte) {
	t.Helper()
	var bodies [][]byte
	for _, sess := range srv.Sessions() {
		for _, m := range sess.Messages() {
			bodies = append(bodies, m.WireBody)
		}
	}
	if msgIdx < 0 || msgIdx >= len(bodies) {
		t.Fatalf("AssertWireBody: msgIdx %d out of range (have %d messages)", msgIdx, len(bodies))
		return
	}
	if !bytes.Equal(bodies[msgIdx], want) {
		t.Errorf("AssertWireBody: message %d wire body mismatch\n got:  %q\nwant: %q", msgIdx, bodies[msgIdx], want)
	}
}

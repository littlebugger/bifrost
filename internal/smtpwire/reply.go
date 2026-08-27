package smtpwire

import (
	"bufio"
	"errors"
)

// Errors reported by ReplyReader. Both mean the same thing to a caller
// relaying a backend reply: the backend is dead at this phase (see
// PROJECT.md's "Malformed backend reply" contract row), the reply is not
// relayable, and the transaction gets a synthesized 451.
var (
	// ErrMalformedReply reports a reply line that is not
	// "<3 digits>[ -]<text>CRLF", carries a code outside 200–599, or
	// continues a multiline reply under a different code than it
	// started with. Bare-LF-terminated reply lines land here too:
	// strict CRLF applies in both directions.
	ErrMalformedReply = errors.New("smtpwire: malformed reply line")

	// ErrReplyTooLong reports a single reply line over maxLine, or a
	// whole multiline reply over maxTotal. Defensive bound: a backend
	// must not be able to make Bifrost buffer without limit.
	//
	// The two causes differ in what is left in the stream: the per-line
	// cap consumed that line through its terminator (the reader is
	// positioned at the next line), while the maxTotal cap leaves the
	// rest of the oversized reply unread. Neither is resynchronizable in
	// practice — the reply is unusable either way and the backend is
	// dropped — so callers must not read on and expect the next reply.
	ErrReplyTooLong = errors.New("smtpwire: reply too long")
)

// ReplyReader reads SMTP replies one line at a time.
//
// Streaming is the point: R4 requires each line to reach the client the
// moment it arrives, so Next never holds continuation lines back waiting
// for the final one. Callers relay the returned bytes verbatim and key
// their state machine off code/final.
//
// One ReplyReader serves a whole connection: after it returns a final
// line it resets, so the next Next starts the next reply (and the
// maxTotal budget starts over).
type ReplyReader struct {
	br       *bufio.Reader
	maxLine  int
	maxTotal int

	total   int  // bytes of the reply currently being read
	code    int  // code the current reply started with
	started bool // a continuation line has already fixed code
}

// NewReplyReader returns a ReplyReader over br. maxLine caps one line
// (terminator included), maxTotal caps one whole multiline reply; both
// must be positive.
func NewReplyReader(br *bufio.Reader, maxLine, maxTotal int) *ReplyReader {
	return &ReplyReader{br: br, maxLine: maxLine, maxTotal: maxTotal}
}

// Next reads the next reply line and returns it verbatim, terminator
// included, along with its code and whether it is the reply's final line
// (space separator, or code-only). A returned error means nothing usable
// came back: line is nil, and the caller drops the backend. The final
// line of a reply leaves the reader ready for the next reply.
func (r *ReplyReader) Next() (line []byte, code int, final bool, err error) {
	// Deliberate reuse of the command reader: same strict-CRLF rules,
	// same "cap the line, resync past its terminator" behavior, same
	// freshly-allocated verbatim slice.
	line, err = ReadCommandLine(r.br, r.maxLine)
	switch {
	case errors.Is(err, ErrLineTooLong):
		r.reset()
		return nil, 0, false, ErrReplyTooLong
	case errors.Is(err, ErrBareLF):
		r.reset()
		return nil, 0, false, ErrMalformedReply
	case err != nil:
		r.reset()
		return nil, 0, false, err
	}

	r.total += len(line)
	if r.total > r.maxTotal {
		r.reset()
		return nil, 0, false, ErrReplyTooLong
	}

	code, final, ok := parseReplyLine(line)
	if !ok || (r.started && code != r.code) {
		r.reset()
		return nil, 0, false, ErrMalformedReply
	}
	r.code, r.started = code, true
	if final {
		r.reset()
	}
	return line, code, final, nil
}

// reset clears the per-reply state so the reader is ready for the next
// reply on the same connection.
func (r *ReplyReader) reset() {
	r.total, r.code, r.started = 0, 0, false
}

// parseReplyLine reads the code and separator off one CRLF-terminated
// reply line. ok is false for anything RFC 5321 §4.2 does not allow:
// fewer than three digits, a non-numeric or out-of-range code, or a
// separator that is neither space nor '-' (a code-only line counts as
// final). Reply text, enhanced status code included, is never inspected.
func parseReplyLine(line []byte) (code int, final, ok bool) {
	body := line[:len(line)-2] // ReadCommandLine guarantees the CRLF
	if len(body) < 3 {
		return 0, false, false
	}
	for _, c := range body[:3] {
		if c < '0' || c > '9' {
			return 0, false, false
		}
	}
	code = int(body[0]-'0')*100 + int(body[1]-'0')*10 + int(body[2]-'0')
	if code < 200 || code > 599 {
		return 0, false, false
	}
	switch {
	case len(body) == 3, body[3] == ' ':
		return code, true, true
	case body[3] == '-':
		return code, false, true
	default:
		return 0, false, false
	}
}

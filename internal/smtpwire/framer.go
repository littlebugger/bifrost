package smtpwire

// dataState is the DataFramer's scanner state: where in the sequence
// CRLF "." CRLF the stream currently sits. Its zero value is
// stateAfterCRLF because a DATA body starts at the beginning of a line —
// which is why a body consisting of nothing but ".CRLF" is a valid,
// empty message.
type dataState uint8

const (
	stateAfterCRLF dataState = iota // at the start of a line
	stateInLine                     // somewhere inside a line
	stateCR                         // saw CR; a LF would end the line
	stateDot                        // at the start of a line, saw "."
	stateDotCR                      // at the start of a line, saw ".CR"
)

// DataFramer finds the end of an SMTP DATA body — and nothing else.
//
// It is the byte-transparency guarantee in code form: the framer only
// tells the caller how many bytes belong to the message, so DATA relays
// as a raw pipe. Dot-stuffing is terminator-preserving, so unstuffing
// and restuffing would be the identity function; Bifrost skips both and
// never rewrites a body byte. There is no line-length limit inside DATA
// and no buffering: the whole scanner is one byte of state, so a 1 GB
// message costs O(1) memory.
//
// Only CRLF "." CRLF ends a body. A bare LF neither closes a line nor
// opens one, so ".LF" and a "." after a bare LF are ordinary body bytes:
// that is the SMTP-smuggling defense (CVE-2023-51764 class), and it is
// the same strict-CRLF rule the command reader applies.
//
// The zero value is ready to use, positioned at the start of the first
// body line. Use one DataFramer per message.
type DataFramer struct {
	state dataState
	done  bool
}

// Feed scans p and reports how many of its bytes belong to the message
// (n, terminator included when done is true) and whether the terminator
// has now been seen. Bytes after the terminator are never claimed — they
// are the next command, and the caller must leave them for its command
// reader. Once done, every later Feed returns (0, true) without looking
// at p.
//
// Feed neither copies nor modifies p: the caller writes p[:n] onward
// verbatim.
func (f *DataFramer) Feed(p []byte) (n int, done bool) {
	if f.done {
		return 0, true
	}

	// ponytail: plain byte-at-a-time switch, ~1 byte/iteration. If the
	// epic-11 load gates ever show DATA framing (rather than the
	// network) as the bottleneck, the upgrade is a bytes.IndexByte('\r')
	// fast path for the stateInLine case — the only state where no other
	// byte can change anything.
	for i, c := range p {
		switch f.state {
		case stateAfterCRLF:
			switch c {
			case '.':
				f.state = stateDot
			case '\r':
				f.state = stateCR
			default:
				f.state = stateInLine
			}
		case stateInLine:
			if c == '\r' {
				f.state = stateCR
			}
		case stateCR:
			switch c {
			case '\n':
				f.state = stateAfterCRLF
			case '\r':
				f.state = stateCR
			default:
				f.state = stateInLine
			}
		case stateDot:
			// ".." (stuffed) and ".x" are body; only ".CR" can still
			// become the terminator.
			if c == '\r' {
				f.state = stateDotCR
			} else {
				f.state = stateInLine
			}
		case stateDotCR:
			switch c {
			case '\n':
				f.done = true
				return i + 1, true
			case '\r':
				f.state = stateCR
			default:
				f.state = stateInLine
			}
		}
	}
	return len(p), false
}

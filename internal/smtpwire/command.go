// Package smtpwire implements Bifrost's raw-preserving SMTP wire
// primitives: the command-line reader, the streaming multiline reply
// reader, and the CRLF.CRLF DATA framer.
//
// Nothing here normalizes anything. No case folding outside a command's
// leading verb token, no whitespace rewriting, no dot-unstuffing, no
// line-ending translation: every byte a caller hands on to the other leg
// is a byte that arrived. That is requirement R4 (see /PROJECT.md), and
// it is why this package reads the wire by hand instead of through
// net/textproto, whose Reader canonicalizes CRLF and whose DotReader
// unstuffs bodies.
//
// The primitives take io/bufio interfaces rather than net.Conn: they are
// net-free, table-driven, and fuzzed (FuzzCommandReader,
// FuzzReplyParser, FuzzDataFramer). Deadlines, TLS, and connection
// lifecycle belong to the callers (internal/proxy, internal/backend,
// internal/health).
//
// Strict CRLF is the rule throughout — commands and framer alike. A
// bare-LF-terminated command line is read to completion but returned
// with ErrBareLF so the caller can answer 500 5.5.2 without ever
// relaying it, and only CRLF.CRLF ends a DATA body. That combination is
// the SMTP-smuggling defense (CVE-2023-51764 class) and matches modern
// Postfix/Exim/Sendmail defaults.
package smtpwire

import (
	"bufio"
	"bytes"
	"errors"
)

// Errors reported by the command reader. Each one maps to exactly one
// row of PROJECT.md's Transparency Contract; internal/proxy translates
// them into synthesized replies.
var (
	// ErrLineTooLong reports a command line longer than the caller's
	// maxLen. The line was consumed through its terminator, so the reader
	// is back in sync and the caller can answer 500 5.5.2 and keep the
	// session going. It is never returned for a line whose terminator
	// never arrived (the read error is returned instead), so its
	// presence always means "still in sync".
	ErrLineTooLong = errors.New("smtpwire: command line too long")

	// ErrBareLF reports a command line terminated by a bare LF instead
	// of CRLF. The line is returned in full — read to completion so the
	// reader stays in sync — but it MUST NOT be relayed: the caller
	// answers 500 5.5.2. See the package comment.
	ErrBareLF = errors.New("smtpwire: bare LF line terminator")
)

// ReadCommandLine reads one SMTP command line from br and returns it
// verbatim, including its terminator. maxLen is the largest acceptable
// line length in bytes, terminator included (callers pass 4096); it must
// be positive.
//
// The returned slice is freshly allocated on every call, so callers may
// hold on to it (queue it, relay it later) without worrying about br's
// buffer being reused.
//
// Errors: ErrBareLF (line returned, must not be relayed), ErrLineTooLong
// (nil line, reader resynced past the terminator), or br's own read
// error. On a read error before any terminator the bytes read so far are
// returned alongside it, so a caller that cares can log the fragment.
func ReadCommandLine(br *bufio.Reader, maxLen int) ([]byte, error) {
	var raw []byte
	tooLong := false

	for {
		// ReadSlice, not ReadBytes: ReadBytes would happily allocate a
		// gigabyte for a client that never sends a terminator. This
		// loop caps the accumulated line at maxLen instead, and keeps
		// draining until the terminator so the caller stays in sync.
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(raw)+len(chunk) > maxLen {
				tooLong = true
				raw = nil
			} else {
				raw = append(raw, chunk...)
			}
		}

		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue // no terminator yet, keep reading
		case err != nil:
			return raw, err
		case tooLong:
			return nil, ErrLineTooLong
		case len(raw) < 2 || raw[len(raw)-2] != '\r':
			return raw, ErrBareLF
		default:
			return raw, nil
		}
	}
}

// ParseVerb splits a raw command line into its verb and the rest.
//
// The verb is the leading token up to the first space or tab, uppercased
// ASCII-only: Unicode case folding maps U+0131 to 'I', which would let
// "maıl" reach the MAIL handler. args is everything after that token
// with the line terminator stripped and surrounding spaces/tabs trimmed;
// its interior bytes are verbatim (case, spacing, and any NULs
// included), and it aliases raw rather than copying it.
func ParseVerb(raw []byte) (verb string, args []byte) {
	line := bytes.TrimSuffix(raw, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))

	i := 0
	for i < len(line) && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	return asciiUpper(line[:i]), bytes.Trim(line[i:], " \t")
}

// asciiUpper uppercases the ASCII letters in b, leaving every other byte
// (including UTF-8 continuation bytes) exactly as it found it.
func asciiUpper(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

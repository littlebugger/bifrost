package smtpwire

import (
	"bufio"
	"bytes"
	"errors"
	"testing"
)

// FuzzCommandReader drives ReadCommandLine over arbitrary bytes with an
// arbitrary line cap and asserts the invariants the whole relay rests
// on: it never panics, it never consumes bytes it does not account for,
// and every line it returns is a verbatim slice of the input at the
// offset it was read from. Over-reading by one byte would silently eat
// the next pipelined command; returning a rewritten line would break R4.
func FuzzCommandReader(f *testing.F) {
	f.Add([]byte("EHLO example.com\r\n"), uint16(4096))
	f.Add([]byte("MAIL FROM:<a@b>\nRCPT TO:<c@d>\r\n"), uint16(4096))
	f.Add([]byte("NOOP\r\nQUIT\r\n"), uint16(8))
	f.Add([]byte("\r\n\n\r\r\n"), uint16(4096))

	f.Fuzz(func(t *testing.T, in []byte, maxSeed uint16) {
		maxLen := min(max(int(maxSeed), 1), 8192)
		src := bytes.NewReader(in)
		br := bufio.NewReaderSize(src, 16)
		consumed := func() int { return len(in) - src.Len() - br.Buffered() }

		for off := 0; ; {
			raw, err := ReadCommandLine(br, maxLen)
			used := consumed() - off
			if used < 0 || consumed() > len(in) {
				t.Fatalf("consumption out of range: off=%d used=%d total=%d", off, used, len(in))
			}

			// Whatever came back must be exactly the bytes just
			// consumed, at the offset they were consumed from. The one
			// case where bytes are consumed without being returned is
			// an over-long line: those are deliberately dropped (never
			// relayable) after the reader resyncs past the terminator,
			// or after the stream ends mid-line.
			transport := err != nil && !errors.Is(err, ErrBareLF) && !errors.Is(err, ErrLineTooLong)
			switch {
			case errors.Is(err, ErrLineTooLong):
				if raw != nil {
					t.Fatalf("ErrLineTooLong with raw=%q, want nil", raw)
				}
				if used <= maxLen {
					t.Fatalf("ErrLineTooLong after %d bytes, maxLen=%d", used, maxLen)
				}
				if in[off+used-1] != '\n' {
					t.Fatalf("ErrLineTooLong did not resync past a terminator: %q", in[off:off+used])
				}
			case transport && raw == nil && used > 0:
				if used <= maxLen {
					t.Fatalf("dropped %d bytes on %v without exceeding maxLen=%d", used, err, maxLen)
				}
			default:
				if used != len(raw) {
					t.Fatalf("consumed %d bytes but returned %d: %q", used, len(raw), raw)
				}
				if !bytes.Equal(raw, in[off:off+used]) {
					t.Fatalf("raw = %q, want verbatim %q", raw, in[off:off+used])
				}
				if len(raw) > maxLen {
					t.Fatalf("returned %d bytes, maxLen=%d", len(raw), maxLen)
				}
			}

			switch {
			case err == nil:
				if !bytes.HasSuffix(raw, []byte("\r\n")) {
					t.Fatalf("nil error for non-CRLF line %q", raw)
				}
			case errors.Is(err, ErrBareLF):
				if !bytes.HasSuffix(raw, []byte("\n")) || bytes.HasSuffix(raw, []byte("\r\n")) {
					t.Fatalf("ErrBareLF for %q", raw)
				}
			case errors.Is(err, ErrLineTooLong):
			default:
				// Transport error: nothing terminated remains.
				if bytes.IndexByte(in[off:], '\n') >= 0 {
					t.Fatalf("stopped with err=%v but %q still holds a terminator", err, in[off:])
				}
				checkParseVerb(t, raw)
				return
			}

			checkParseVerb(t, raw)
			off += used
		}
	})
}

// FuzzReplyParser drives ReplyReader over arbitrary bytes and asserts
// the streaming-relay invariants: every line handed back is a verbatim
// slice of the input at the offset it was read from (R4 — these bytes go
// straight to the client), no line escapes the caps, continuation lines
// never change code mid-reply, and an error never comes with bytes
// attached (a malformed reply must not be relayable).
func FuzzReplyParser(f *testing.F) {
	f.Add([]byte("250 ok\r\n"), uint16(512))
	f.Add([]byte("250-a\r\n250-b\r\n250 c\r\n"), uint16(512))
	f.Add([]byte("250-a\r\n251 b\r\n"), uint16(512))
	f.Add([]byte("354 go\r\n550 5.7.1 no\n"), uint16(512))

	f.Fuzz(func(t *testing.T, in []byte, maxSeed uint16) {
		maxLine := min(max(int(maxSeed), 4), 512)
		maxTotal := 4 * maxLine
		src := bytes.NewReader(in)
		br := bufio.NewReaderSize(src, 16)
		consumed := func() int { return len(in) - src.Len() - br.Buffered() }
		rr := NewReplyReader(br, maxLine, maxTotal)

		replyCode, replyTotal, inReply := 0, 0, false
		for off := 0; ; {
			line, code, final, err := rr.Next()
			used := consumed() - off
			if used < 0 || consumed() > len(in) {
				t.Fatalf("consumption out of range: off=%d used=%d total=%d", off, used, len(in))
			}
			if err != nil {
				if line != nil {
					t.Fatalf("err=%v returned %d bytes: %q", err, len(line), line)
				}
				return
			}

			if used != len(line) {
				t.Fatalf("consumed %d bytes but returned %d: %q", used, len(line), line)
			}
			if !bytes.Equal(line, in[off:off+used]) {
				t.Fatalf("line = %q, want verbatim %q", line, in[off:off+used])
			}
			if len(line) > maxLine {
				t.Fatalf("returned %d bytes, maxLine=%d", len(line), maxLine)
			}
			if code < 200 || code > 599 {
				t.Fatalf("code %d out of range for %q", code, line)
			}
			if inReply && code != replyCode {
				t.Fatalf("continuation code %d after %d: %q", code, replyCode, line)
			}
			replyTotal += len(line)
			if replyTotal > maxTotal {
				t.Fatalf("reply grew to %d bytes, maxTotal=%d", replyTotal, maxTotal)
			}
			replyCode, inReply = code, !final
			if final {
				replyTotal = 0
			}
			off += used
		}
	})
}

// FuzzDataFramer is the flagship target: an arbitrary body plus the
// terminator, split into arbitrary chunks, checked against an
// independent reference for where the body must end. The framer decides
// how much of a client's stream is "the message" — terminate one byte
// early and the rest of the body gets parsed as commands (smuggling);
// terminate late and the session hangs.
func FuzzDataFramer(f *testing.F) {
	f.Add([]byte("hello\r\nworld"), []byte{7})
	f.Add([]byte(""), []byte{1})
	f.Add([]byte("a\r\n..stuffed\r\n.\rnot yet"), []byte{1, 2, 3})
	f.Add([]byte("a\n.\n.\r\n"), []byte{4})
	f.Add([]byte(".\r\n"), []byte{2})

	f.Fuzz(func(t *testing.T, body, chunkSeed []byte) {
		in := append(append([]byte{}, body...), "\r\n.\r\n"...)
		want := terminatorEnd(in)
		if want < 0 {
			t.Fatalf("reference found no terminator in %q", in)
		}
		if len(chunkSeed) == 0 {
			chunkSeed = []byte{1}
		}

		var fr DataFramer
		total, seed := 0, 0
		for total < len(in) {
			size := min(1+int(chunkSeed[seed%len(chunkSeed)]), len(in)-total)
			seed++
			chunk := in[total : total+size]

			n, done := fr.Feed(chunk)
			if n < 0 || n > len(chunk) {
				t.Fatalf("n = %d, out of range for a %d-byte chunk", n, len(chunk))
			}
			total += n
			switch {
			case done && total != want:
				t.Fatalf("terminated after %d bytes, want %d: %q", total, want, in)
			case !done && total > want:
				t.Fatalf("consumed %d bytes without terminating, want %d: %q", total, want, in)
			case !done && n != len(chunk):
				t.Fatalf("consumed %d of a %d-byte chunk without terminating", n, len(chunk))
			}
			if done {
				if n2, done2 := fr.Feed(in); n2 != 0 || !done2 {
					t.Fatalf("Feed after done = (%d, %v), want (0, true)", n2, done2)
				}
				return
			}
		}
		t.Fatalf("consumed all %d bytes without terminating, want terminator at %d: %q", total, want, in)
	})
}

// terminatorEnd is the fuzz target's independent reference for where a
// DATA body ends: the offset just past the first CRLF "." CRLF, or just
// past a leading "." CRLF (the body starts at a line boundary, so a
// stream that opens with ".CRLF" is an empty message). -1 if the stream
// holds no terminator. Deliberately written with bytes.Index rather than
// a second state machine, so it cannot share a bug with the framer.
func terminatorEnd(in []byte) int {
	if bytes.HasPrefix(in, []byte(".\r\n")) {
		return 3
	}
	if i := bytes.Index(in, []byte("\r\n.\r\n")); i >= 0 {
		return i + 5
	}
	return -1
}

// checkParseVerb asserts ParseVerb's own invariants on a line the reader
// just produced: the verb is ASCII-uppercased, and verb plus args never
// claim more bytes than the line held.
func checkParseVerb(t *testing.T, raw []byte) {
	t.Helper()
	verb, args := ParseVerb(raw)
	if asciiUpper([]byte(verb)) != verb {
		t.Fatalf("verb %q is not ASCII-uppercased", verb)
	}
	if len(verb)+len(args) > len(raw) {
		t.Fatalf("ParseVerb over-reported: verb=%q args=%q raw=%q", verb, args, raw)
	}
}

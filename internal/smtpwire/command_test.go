package smtpwire

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// feed is a bufio.Reader over a fixed input plus exact "how many bytes
// has the caller consumed so far" accounting: the input length minus what
// is still unread in the source and what bufio read ahead into its own
// buffer. Every reader-consumption assertion in this package is built on
// it (a raw-preserving primitive that over-reads is as broken as one that
// mis-parses: the next command would be lost).
type feed struct {
	src *bytes.Reader
	br  *bufio.Reader
	n   int
}

func newFeed(in string, bufSize int) *feed {
	src := bytes.NewReader([]byte(in))
	return &feed{src: src, br: bufio.NewReaderSize(src, bufSize), n: len(in)}
}

func (f *feed) consumed() int { return f.n - f.src.Len() - f.br.Buffered() }

func TestReadCommandLine(t *testing.T) {
	const big = 4096

	tests := []struct {
		name     string
		in       string
		max      int
		bufSize  int // 0 => bufio default
		wantRaw  string
		wantErr  error
		wantUsed int
	}{
		{
			name: "crlf line verbatim", in: "EHLO example.com\r\n", max: big,
			wantRaw: "EHLO example.com\r\n", wantUsed: 18,
		},
		{
			name: "empty crlf line", in: "\r\n", max: big,
			wantRaw: "\r\n", wantUsed: 2,
		},
		{
			name: "nul bytes preserved", in: "MAI\x00L FROM:<a\x00b>\r\n", max: big,
			wantRaw: "MAI\x00L FROM:<a\x00b>\r\n", wantUsed: 18,
		},
		{
			// The smuggling defense: read to completion so the session
			// stays in sync, hand it back flagged so it is never relayed.
			name: "bare lf line read fully and flagged", in: "MAIL FROM:<a@b>\n", max: big,
			wantRaw: "MAIL FROM:<a@b>\n", wantErr: ErrBareLF, wantUsed: 16,
		},
		{
			name: "lone lf", in: "\n", max: big,
			wantRaw: "\n", wantErr: ErrBareLF, wantUsed: 1,
		},
		{
			name: "interior cr is not a terminator", in: "EHLO\rx\n", max: big,
			wantRaw: "EHLO\rx\n", wantErr: ErrBareLF, wantUsed: 7,
		},
		{
			name: "exactly max is accepted", in: "AAAAAA\r\n", max: 8,
			wantRaw: "AAAAAA\r\n", wantUsed: 8,
		},
		{
			name: "max plus one discards through end of line", in: "AAAAAAA\r\n", max: 8,
			wantErr: ErrLineTooLong, wantUsed: 9,
		},
		{
			name: "over long line spanning refills stays in sync", in: strings.Repeat("A", 300) + "\r\n", max: 64, bufSize: 16,
			wantErr: ErrLineTooLong, wantUsed: 302,
		},
		{
			name: "line spanning many refills is reassembled verbatim", in: strings.Repeat("A", 300) + "\r\n", max: big, bufSize: 16,
			wantRaw: strings.Repeat("A", 300) + "\r\n", wantUsed: 302,
		},
		{
			name: "eof mid line returns partial and eof", in: "MAIL FROM", max: big,
			wantRaw: "MAIL FROM", wantErr: io.EOF, wantUsed: 9,
		},
		{
			name: "eof on empty stream", in: "", max: big,
			wantErr: io.EOF, wantUsed: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bufSize := tc.bufSize
			if bufSize == 0 {
				bufSize = 4096
			}
			f := newFeed(tc.in, bufSize)

			raw, err := ReadCommandLine(f.br, tc.max)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if string(raw) != tc.wantRaw {
				t.Errorf("raw = %q, want %q", raw, tc.wantRaw)
			}
			if used := f.consumed(); used != tc.wantUsed {
				t.Errorf("consumed = %d, want %d", used, tc.wantUsed)
			}
		})
	}

	t.Run("resync after over long line", func(t *testing.T) {
		f := newFeed(strings.Repeat("A", 20)+"\r\nNOOP\r\n", 16)

		if _, err := ReadCommandLine(f.br, 8); !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("first read err = %v, want ErrLineTooLong", err)
		}
		raw, err := ReadCommandLine(f.br, 8)
		if err != nil {
			t.Fatalf("second read err = %v, want nil", err)
		}
		if string(raw) != "NOOP\r\n" {
			t.Errorf("second raw = %q, want %q", raw, "NOOP\r\n")
		}
	})

	t.Run("bare lf then next line still readable", func(t *testing.T) {
		f := newFeed("RSET\nNOOP\r\n", 4096)

		if _, err := ReadCommandLine(f.br, 4096); !errors.Is(err, ErrBareLF) {
			t.Fatalf("first read err = %v, want ErrBareLF", err)
		}
		raw, err := ReadCommandLine(f.br, 4096)
		if err != nil {
			t.Fatalf("second read err = %v, want nil", err)
		}
		if string(raw) != "NOOP\r\n" {
			t.Errorf("second raw = %q, want %q", raw, "NOOP\r\n")
		}
	})
}

func TestParseVerb(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantVerb string
		wantArgs string
	}{
		{name: "lowercase verb uppercased, args verbatim", raw: "mail from:<a@b>\r\n", wantVerb: "MAIL", wantArgs: "from:<a@b>"},
		{name: "args keep case and spacing", raw: "MAIL FROM:<A@b> SIZE=10\r\n", wantVerb: "MAIL", wantArgs: "FROM:<A@b> SIZE=10"},
		{name: "lone verb", raw: "EHLO\r\n", wantVerb: "EHLO", wantArgs: ""},
		{name: "trailing spaces trimmed", raw: "NOOP   \r\n", wantVerb: "NOOP", wantArgs: ""},
		{name: "tab separator", raw: "rcpt\tTO:<b@c>\r\n", wantVerb: "RCPT", wantArgs: "TO:<b@c>"},
		{name: "multiple separators", raw: "MAIL \t FROM:<a@b>\r\n", wantVerb: "MAIL", wantArgs: "FROM:<a@b>"},
		{name: "bare lf terminator stripped", raw: "quit\n", wantVerb: "QUIT", wantArgs: ""},
		{name: "no terminator at all", raw: "QUIT", wantVerb: "QUIT", wantArgs: ""},
		{name: "empty line", raw: "\r\n", wantVerb: "", wantArgs: ""},
		{name: "leading space means empty verb", raw: " MAIL FROM:<a@b>\r\n", wantVerb: "", wantArgs: "MAIL FROM:<a@b>"},
		{name: "nul in verb preserved", raw: "MA\x00IL x\r\n", wantVerb: "MA\x00IL", wantArgs: "x"},
		// Case folding is ASCII-only on purpose: Unicode folding maps
		// U+0131 to 'I', which would turn this into MAIL and let a
		// non-ASCII line reach a verb handler.
		{name: "non ascii is not folded into a verb", raw: "maıl from:<a@b>\r\n", wantVerb: "MAıL", wantArgs: "from:<a@b>"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			verb, args := ParseVerb([]byte(tc.raw))
			if verb != tc.wantVerb {
				t.Errorf("verb = %q, want %q", verb, tc.wantVerb)
			}
			if string(args) != tc.wantArgs {
				t.Errorf("args = %q, want %q", args, tc.wantArgs)
			}
		})
	}
}

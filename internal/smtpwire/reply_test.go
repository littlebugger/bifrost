package smtpwire

import (
	"errors"
	"io"
	"testing"
)

func TestReplyReader(t *testing.T) {
	// want is one expected Next() result.
	type want struct {
		line  string
		code  int
		final bool
		err   error
	}

	tests := []struct {
		name     string
		in       string
		maxLine  int
		maxTotal int
		want     []want
	}{
		{
			name: "single final line", in: "250 ok\r\n",
			want: []want{{line: "250 ok\r\n", code: 250, final: true}},
		},
		{
			name: "multiline surfaces each line as read", in: "250-a\r\n250-b\r\n250 c\r\n",
			want: []want{
				{line: "250-a\r\n", code: 250},
				{line: "250-b\r\n", code: 250},
				{line: "250 c\r\n", code: 250, final: true},
			},
		},
		{
			name: "enhanced status codes pass through untouched", in: "550-5.7.1 blocked\r\n550 5.7.1 see http://x/y\r\n",
			want: []want{
				{line: "550-5.7.1 blocked\r\n", code: 550},
				{line: "550 5.7.1 see http://x/y\r\n", code: 550, final: true},
			},
		},
		{
			name: "bare code and crlf only", in: "250\r\n",
			want: []want{{line: "250\r\n", code: 250, final: true}},
		},
		{
			name: "continuation with empty text", in: "250-\r\n250 ok\r\n",
			want: []want{
				{line: "250-\r\n", code: 250},
				{line: "250 ok\r\n", code: 250, final: true},
			},
		},
		{
			name: "reader is reused across replies and resets its total", in: "250 a\r\n354 go\r\n", maxTotal: 8,
			want: []want{
				{line: "250 a\r\n", code: 250, final: true},
				{line: "354 go\r\n", code: 354, final: true},
			},
		},
		{
			name: "non digit code", in: "2x0 ok\r\n",
			want: []want{{err: ErrMalformedReply}},
		},
		{
			name: "code below 200", in: "199 nope\r\n",
			want: []want{{err: ErrMalformedReply}},
		},
		{
			name: "code above 599", in: "600 nope\r\n",
			want: []want{{err: ErrMalformedReply}},
		},
		{
			name: "line shorter than a code", in: "25\r\n",
			want: []want{{err: ErrMalformedReply}},
		},
		{
			name: "separator is neither space nor dash", in: "250x ok\r\n",
			want: []want{{err: ErrMalformedReply}},
		},
		{
			name: "mismatched continuation code is backend death", in: "250-a\r\n251 b\r\n",
			want: []want{
				{line: "250-a\r\n", code: 250},
				{err: ErrMalformedReply},
			},
		},
		{
			name: "bare lf terminated reply line", in: "250 ok\n",
			want: []want{{err: ErrMalformedReply}},
		},
		{
			name: "line over maxLine", in: "250 aaaaaaaaaa\r\n250 ok\r\n", maxLine: 8,
			want: []want{{err: ErrReplyTooLong}},
		},
		{
			name: "reply total over maxTotal", in: "250-aaaa\r\n250-bbbb\r\n250 c\r\n", maxTotal: 16,
			want: []want{
				{line: "250-aaaa\r\n", code: 250},
				{err: ErrReplyTooLong},
			},
		},
		{
			name: "eof mid reply", in: "250-a\r\n",
			want: []want{
				{line: "250-a\r\n", code: 250},
				{err: io.EOF},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			maxLine, maxTotal := tc.maxLine, tc.maxTotal
			if maxLine == 0 {
				maxLine = 512
			}
			if maxTotal == 0 {
				maxTotal = 4096
			}
			f := newFeed(tc.in, 16)
			rr := NewReplyReader(f.br, maxLine, maxTotal)

			for i, w := range tc.want {
				line, code, final, err := rr.Next()
				if !errors.Is(err, w.err) {
					t.Fatalf("Next %d: err = %v, want %v", i, err, w.err)
				}
				if string(line) != w.line {
					t.Errorf("Next %d: line = %q, want %q", i, line, w.line)
				}
				if code != w.code {
					t.Errorf("Next %d: code = %d, want %d", i, code, w.code)
				}
				if final != w.final {
					t.Errorf("Next %d: final = %v, want %v", i, final, w.final)
				}
			}
		})
	}

	t.Run("over long line resyncs so the next reply parses", func(t *testing.T) {
		f := newFeed("250 aaaaaaaaaa\r\n250 ok\r\n", 16)
		rr := NewReplyReader(f.br, 8, 4096)

		if _, _, _, err := rr.Next(); !errors.Is(err, ErrReplyTooLong) {
			t.Fatalf("first Next err = %v, want ErrReplyTooLong", err)
		}
		line, code, final, err := rr.Next()
		if err != nil || code != 250 || !final || string(line) != "250 ok\r\n" {
			t.Fatalf("second Next = (%q, %d, %v, %v), want (\"250 ok\\r\\n\", 250, true, nil)", line, code, final, err)
		}
	})
}

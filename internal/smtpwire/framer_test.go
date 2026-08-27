package smtpwire

import (
	"runtime"
	"strings"
	"testing"
)

// feedChunks runs chunks through f and returns the per-chunk consumed
// counts plus the index of the chunk that reported done (-1 if none).
// Feeding stops at done, exactly as the relay stops: the bytes after the
// terminator belong to the next command, not to the message.
func feedChunks(t *testing.T, f *DataFramer, chunks []string) (counts []int, doneAt int) {
	t.Helper()
	doneAt = -1
	for i, c := range chunks {
		n, done := f.Feed([]byte(c))
		if n < 0 || n > len(c) {
			t.Fatalf("chunk %d: n = %d, out of range for %d bytes", i, n, len(c))
		}
		counts = append(counts, n)
		if done {
			doneAt = i
			break
		}
	}
	return counts, doneAt
}

func TestDataFramer(t *testing.T) {
	tests := []struct {
		name      string
		chunks    []string
		wantTotal int // bytes consumed into the message, terminator included
		wantDone  int // index of the chunk that terminated, -1 for none
	}{
		{
			name: "terminator in one chunk", chunks: []string{"hello\r\n.\r\n"},
			wantTotal: 10, wantDone: 0,
		},
		{
			// The relay MUST leave the next command in the reader.
			name: "bytes after the terminator are left alone", chunks: []string{"hi\r\n.\r\nQUIT\r\n"},
			wantTotal: 7, wantDone: 0,
		},
		{
			name: "immediate terminator at stream start", chunks: []string{".\r\n"},
			wantTotal: 3, wantDone: 0,
		},
		{
			name: "dot stuffed line is not a terminator", chunks: []string{"a\r\n..x\r\n.\r\n"},
			wantTotal: 11, wantDone: 0,
		},
		{
			name: "double dot alone on a line is not a terminator", chunks: []string{"a\r\n..\r\n.\r\n"},
			wantTotal: 10, wantDone: 0,
		},
		{
			name: "dot cr then more data", chunks: []string{"a\r\n.\rX\r\n.\r\n"},
			wantTotal: 11, wantDone: 0,
		},
		{
			name: "dot cr cr lf is a line, not a terminator", chunks: []string{"a\r\n.\r\r\n.\r\n"},
			wantTotal: 10, wantDone: 0,
		},
		{
			name: "dot at line start followed by text", chunks: []string{"a\r\n.x\r\n.\r\n"},
			wantTotal: 10, wantDone: 0,
		},
		{
			// Strict CRLF: a bare LF neither ends a line nor opens one,
			// so no dot that follows it can terminate the body. This is
			// the SMTP-smuggling defense.
			name: "bare lf dot bare lf never terminates", chunks: []string{"a\n.\n"},
			wantTotal: 4, wantDone: -1,
		},
		{
			name: "dot crlf after a bare lf does not terminate", chunks: []string{"a\n.\r\n"},
			wantTotal: 5, wantDone: -1,
		},
		{
			name: "bare lf body still ends at a real crlf dot crlf", chunks: []string{"a\n.\nb\r\n.\r\n"},
			wantTotal: 10, wantDone: 0,
		},
		{
			name: "lf crlf dot crlf terminates", chunks: []string{"a\n\r\n.\r\n"},
			wantTotal: 7, wantDone: 0,
		},
		{
			name: "empty chunk makes no progress", chunks: []string{"", ".\r\n"},
			wantTotal: 3, wantDone: 1,
		},
		{
			name: "one byte per chunk", chunks: []string{"a", "\r", "\n", ".", "\r", "\n"},
			wantTotal: 6, wantDone: 5,
		},
		{
			name: "dot survives a chunk boundary mid stuffing", chunks: []string{"a\r\n.", ".x\r\n.", "\r\n"},
			wantTotal: 11, wantDone: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var f DataFramer
			counts, doneAt := feedChunks(t, &f, tc.chunks)

			total := 0
			for _, n := range counts {
				total += n
			}
			if total != tc.wantTotal {
				t.Errorf("consumed %d bytes (%v), want %d", total, counts, tc.wantTotal)
			}
			if doneAt != tc.wantDone {
				t.Errorf("done at chunk %d, want %d", doneAt, tc.wantDone)
			}
			if tc.wantDone >= 0 {
				// Idempotent afterwards: a finished framer never claims
				// another byte.
				if n, done := f.Feed([]byte("more\r\n.\r\n")); n != 0 || !done {
					t.Errorf("Feed after done = (%d, %v), want (0, true)", n, done)
				}
			}
		})
	}

	t.Run("terminator split at every byte boundary", func(t *testing.T) {
		const prefix = "body line one\r\nbody line two"
		const term = "\r\n.\r\n"

		for i := 0; i <= len(term); i++ {
			var f DataFramer
			chunks := []string{prefix + term[:i], term[i:]}
			counts, doneAt := feedChunks(t, &f, chunks)

			total := 0
			for _, n := range counts {
				total += n
			}
			wantDone := 1
			if i == len(term) {
				wantDone = 0
			}
			if total != len(prefix)+len(term) || doneAt != wantDone {
				t.Errorf("split %d: consumed %d (%v) done at %d, want %d bytes done at %d",
					i, total, counts, doneAt, len(prefix)+len(term), wantDone)
			}
		}
	})

	t.Run("byte at a time never terminates early", func(t *testing.T) {
		in := "line\r\n..stuffed\r\n.\rnot yet\r\n.\r\ntail"
		end := strings.Index(in, "\r\n.\r\n") + 5

		var f DataFramer
		for i := range len(in) {
			n, done := f.Feed([]byte(in[i : i+1]))
			if n != 1 {
				t.Fatalf("byte %d: n = %d, want 1", i, n)
			}
			if done != (i == end-1) {
				t.Fatalf("byte %d (%q): done = %v, want %v", i, in[i], done, i == end-1)
			}
			if done {
				break
			}
		}
	})

	t.Run("ten megabyte body in one byte feeds stays O(1)", func(t *testing.T) {
		const size = 10 << 20
		body := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog\r\n..stuffed line\r\n", size/60))
		var f DataFramer
		var buf [1]byte

		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		for _, c := range body {
			buf[0] = c
			n, done := f.Feed(buf[:])
			if n != 1 || done {
				t.Fatalf("body feed: (%d, %v), want (1, false)", n, done)
			}
		}
		for i, c := range []byte("\r\n.\r\n") {
			buf[0] = c
			n, done := f.Feed(buf[:])
			if n != 1 || done != (i == 4) {
				t.Fatalf("terminator byte %d: (%d, %v), want (1, %v)", i, n, done, i == 4)
			}
		}

		runtime.ReadMemStats(&after)
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<10 {
			t.Errorf("framing %d bytes allocated %d bytes, want O(1)", len(body), grew)
		}
	})
}

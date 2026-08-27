package fakesmtp

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDownListenerClosed(t *testing.T) {
	srv := Start(t, Script{})
	srv.SetDown(DownListenerClosed)

	_, err := net.DialTimeout("tcp", srv.Addr(), 500*time.Millisecond)
	if err == nil {
		t.Fatalf("expected dial to be refused while down, got no error")
	}
}

func TestDownAcceptThenRST(t *testing.T) {
	srv := Start(t, Script{})
	srv.SetDown(DownAcceptThenRST)

	conn, r := dialRaw(t, srv.Addr())
	_ = conn
	_, err := r.ReadByte()
	if err == nil {
		t.Fatalf("expected a reset, got no error")
	}
	if isCleanEOF(err) {
		t.Fatalf("expected a reset, got clean EOF")
	}
}

func TestDownAcceptThenHang(t *testing.T) {
	srv := Start(t, Script{})
	srv.SetDown(DownAcceptThenHang)

	conn, r := dialRaw(t, srv.Addr())
	_ = conn.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	_, err := r.ReadByte()
	if !isTimeout(err) {
		t.Fatalf("expected read timeout (no banner while hung), got %v", err)
	}
}

func TestSetUpRestores(t *testing.T) {
	srv := Start(t, Script{})
	srv.SetDown(DownListenerClosed)
	if _, err := net.DialTimeout("tcp", srv.Addr(), 500*time.Millisecond); err == nil {
		t.Fatalf("expected dial to be refused while down")
	}

	srv.SetUp()

	conn, r := dialRaw(t, srv.Addr())
	_ = conn
	banner := readReplyLines(t, r)
	if len(banner) == 0 || banner[0][0] != '2' {
		t.Fatalf("banner after SetUp = %v, want a 2xx banner", banner)
	}
}

// TestStopRacesSetUp is a regression case for a hang found by probing:
// SetUp (via relisten) racing a concurrent Stop could leave Stop closing
// a stale listener while a fresh one — and its acceptLoop — ran on
// unnoticed, hanging Stop's wg.Wait forever (reproduced at iteration
// 2/200 in the original probe). Repeated iterations with fresh servers
// give the race room to happen; a timeout turns a hang into a failure
// instead of stalling the whole suite.
func TestStopRacesSetUp(t *testing.T) {
	for i := 0; i < 20; i++ {
		srv := Start(t, Script{})

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			srv.SetDown(DownListenerClosed)
			srv.SetUp()
		}()
		go func() {
			defer wg.Done()
			srv.Stop()
		}()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: SetDown/SetUp racing Stop hung", i)
		}
	}
}

func TestSetScriptAtomicSwap(t *testing.T) {
	srv := Start(t, Script{Caps: []string{"OLDCAP"}})

	conn1, r1 := dialRaw(t, srv.Addr())
	readReplyLines(t, r1) // banner, script captured at accept time: OLDCAP

	srv.SetScript(Script{Caps: []string{"NEWCAP"}})

	conn2, r2 := dialRaw(t, srv.Addr())
	readReplyLines(t, r2) // banner, script captured at accept time: NEWCAP

	if _, err := conn1.Write([]byte("EHLO a.example\r\n")); err != nil {
		t.Fatalf("conn1 EHLO: %v", err)
	}
	got1 := readReplyLines(t, r1)
	if !strings.Contains(strings.Join(got1, "\n"), "OLDCAP") {
		t.Fatalf("pre-swap session EHLO = %v, want OLDCAP (its script must not change)", got1)
	}

	if _, err := conn2.Write([]byte("EHLO b.example\r\n")); err != nil {
		t.Fatalf("conn2 EHLO: %v", err)
	}
	got2 := readReplyLines(t, r2)
	if !strings.Contains(strings.Join(got2, "\n"), "NEWCAP") {
		t.Fatalf("post-swap session EHLO = %v, want NEWCAP", got2)
	}
}

func TestOnEventHook(t *testing.T) {
	srv := Start(t, Script{})

	var mu sync.Mutex
	var got []Event
	srv.OnEvent(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
	})

	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner
	if _, err := conn.Write([]byte("EHLO client.example\r\n")); err != nil {
		t.Fatalf("write EHLO: %v", err)
	}
	readReplyLines(t, r)
	if _, err := conn.Write([]byte("MAIL FROM:<a@b>\r\n")); err != nil {
		t.Fatalf("write MAIL: %v", err)
	}
	readReplyLines(t, r)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("event count = %d, want 2: %+v", len(got), got)
	}
	if got[0].Verb != "EHLO" {
		t.Errorf("event 0 verb = %q, want EHLO", got[0].Verb)
	}
	if got[1].Verb != "MAIL" {
		t.Errorf("event 1 verb = %q, want MAIL", got[1].Verb)
	}
}

// TestHundredConcurrentSessions is the load axis every later chaos/load
// epic leans on: 100 goroutines each run a full transaction concurrently
// against one fake, and every session must keep an independent, intact
// transcript with no cross-session interference. It runs under -race.
func TestHundredConcurrentSessions(t *testing.T) {
	const n = 100
	srv := Start(t, Script{Caps: []string{"PIPELINING"}})

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, r, err := dialRawOK(srv.Addr())
			if err != nil {
				t.Errorf("session %d: dial: %v", i, err)
				return
			}
			defer closeQuietly(conn)

			if _, err := readReplyLinesOK(r); err != nil {
				t.Errorf("session %d: read banner: %v", i, err)
				return
			}

			cmds := []string{
				fmt.Sprintf("EHLO client%d.example", i),
				fmt.Sprintf("MAIL FROM:<from%d@x.example>", i),
				fmt.Sprintf("RCPT TO:<to%d@x.example>", i),
				"DATA",
			}
			for _, c := range cmds {
				if _, err := conn.Write([]byte(c + "\r\n")); err != nil {
					t.Errorf("session %d: write %q: %v", i, c, err)
					return
				}
				if _, err := readReplyLinesOK(r); err != nil {
					t.Errorf("session %d: read reply to %q: %v", i, c, err)
					return
				}
			}
			if _, err := fmt.Fprintf(conn, "body %d\r\n.\r\n", i); err != nil {
				t.Errorf("session %d: write body: %v", i, err)
				return
			}
			if _, err := readReplyLinesOK(r); err != nil {
				t.Errorf("session %d: read EOD reply: %v", i, err)
				return
			}
			if _, err := conn.Write([]byte("QUIT\r\n")); err != nil {
				t.Errorf("session %d: write QUIT: %v", i, err)
				return
			}
			if _, err := readReplyLinesOK(r); err != nil {
				t.Errorf("session %d: read QUIT reply: %v", i, err)
				return
			}
		}(i)
	}
	wg.Wait()

	if got := srv.DialCount(); got != n {
		t.Fatalf("DialCount() = %d, want %d", got, n)
	}

	sessions := srv.Sessions()
	if len(sessions) != n {
		t.Fatalf("Sessions() len = %d, want %d", len(sessions), n)
	}
	const wantEvents = 6 // EHLO, MAIL, RCPT, DATA, one body line, QUIT
	for i, sess := range sessions {
		events := sess.Transcript()
		if len(events) != wantEvents {
			t.Errorf("session %d transcript len = %d, want %d: %+v", i, len(events), wantEvents, events)
			continue
		}
		wantVerbs := []string{"EHLO", "MAIL", "RCPT", "DATA", "", "QUIT"}
		for j, ev := range events {
			if ev.Verb != wantVerbs[j] {
				t.Errorf("session %d event %d verb = %q, want %q", i, j, ev.Verb, wantVerbs[j])
			}
		}
		msgs := sess.Messages()
		if len(msgs) != 1 {
			t.Errorf("session %d Messages() len = %d, want 1", i, len(msgs))
		}
	}
}

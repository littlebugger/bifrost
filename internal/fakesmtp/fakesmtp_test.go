package fakesmtp

import (
	"bufio"
	"net"
	"net/smtp"
	"strings"
	"testing"
)

// dialRaw opens a plain TCP connection to addr for tests that need to
// speak the wire protocol by hand instead of through smtpdrv or net/smtp.
// Only safe to call from the test's own goroutine (t.Fatalf); concurrent
// helpers use dialRawOK instead.
func dialRaw(t testing.TB, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { closeQuietly(conn) })
	return conn, bufio.NewReader(conn)
}

// dialRawOK is dialRaw without the t.Fatalf, for use from goroutines other
// than the one running the test (testing.T forbids FailNow off-goroutine).
func dialRawOK(addr string) (net.Conn, *bufio.Reader, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	return conn, bufio.NewReader(conn), nil
}

// readReplyLines reads one full (possibly multiline) SMTP reply and
// returns each line verbatim, CRLF stripped. Manual bufio only — no
// net/textproto — matching the rest of the package. Only safe to call
// from the test's own goroutine (t.Fatalf); concurrent helpers use
// readReplyLinesOK instead.
func readReplyLines(t testing.TB, r *bufio.Reader) []string {
	t.Helper()
	lines, err := readReplyLinesOK(r)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return lines
}

// readReplyLinesOK is readReplyLines without the t.Fatalf.
func readReplyLinesOK(r *bufio.Reader) ([]string, error) {
	var lines []string
	for {
		raw, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line := strings.TrimRight(raw, "\r\n")
		lines = append(lines, line)
		if len(line) < 4 || line[3] != '-' {
			break
		}
	}
	return lines, nil
}

func TestFakeBannerEHLOQuit(t *testing.T) {
	srv := Start(t, Script{
		Banner: Step{Reply: "220 test.example banner ready"},
		Caps:   []string{"PIPELINING", "8BITMIME"},
	})

	conn, r := dialRaw(t, srv.Addr())

	banner := readReplyLines(t, r)
	if len(banner) != 1 || banner[0] != "220 test.example banner ready" {
		t.Fatalf("banner = %q, want scripted banner", banner)
	}

	if _, err := conn.Write([]byte("EHLO client.example\r\n")); err != nil {
		t.Fatalf("write EHLO: %v", err)
	}
	ehlo := readReplyLines(t, r)
	joined := strings.Join(ehlo, "\n")
	if !strings.Contains(joined, "PIPELINING") || !strings.Contains(joined, "8BITMIME") {
		t.Fatalf("EHLO reply = %v, want caps PIPELINING and 8BITMIME", ehlo)
	}
	if !strings.HasPrefix(ehlo[0], "250") {
		t.Fatalf("EHLO reply first line = %q, want 250 prefix", ehlo[0])
	}

	if _, err := conn.Write([]byte("QUIT\r\n")); err != nil {
		t.Fatalf("write QUIT: %v", err)
	}
	quit := readReplyLines(t, r)
	if len(quit) != 1 || !strings.HasPrefix(quit[0], "221") {
		t.Fatalf("QUIT reply = %v, want 221", quit)
	}
}

func TestFakeAgainstStdlibClient(t *testing.T) {
	srv := Start(t, Script{})

	body := []byte("Subject: hi\r\n\r\nbody\r\n")
	err := smtp.SendMail(srv.Addr(), nil, "sender@example.com", []string{"rcpt@example.com"}, body)
	if err != nil {
		t.Fatalf("SendMail: %v", err)
	}

	// The fake must be honest about exactly what stdlib put on the wire,
	// not just that SendMail returned nil.
	srv.AssertWireBody(t, 0, body)

	msgs := srv.Sessions()[0].Messages()
	if len(msgs) != 1 {
		t.Fatalf("Messages() len = %d, want 1", len(msgs))
	}
	if want := "FROM:<sender@example.com>"; msgs[0].From != want {
		t.Errorf("From = %q, want %q", msgs[0].From, want)
	}
	if want := []string{"TO:<rcpt@example.com>"}; len(msgs[0].Rcpts) != 1 || msgs[0].Rcpts[0] != want[0] {
		t.Errorf("Rcpts = %v, want %v", msgs[0].Rcpts, want)
	}
}

// TestFakeAgainstStdlibClientCaps exercises a multiline EHLO reply
// (greeting + PIPELINING + 8BITMIME) through stdlib specifically:
// net/smtp only adds "BODY=8BITMIME" to MAIL FROM once it has actually
// parsed "8BITMIME" out of the EHLO reply, so seeing that parameter on
// the recorded MAIL line proves the multiline reply was read correctly,
// not just that a single-line EHLO happens to work.
func TestFakeAgainstStdlibClientCaps(t *testing.T) {
	srv := Start(t, Script{Caps: []string{"PIPELINING", "8BITMIME"}})

	body := []byte("Subject: hi\r\n\r\nbody\r\n")
	if err := smtp.SendMail(srv.Addr(), nil, "sender@example.com", []string{"rcpt@example.com"}, body); err != nil {
		t.Fatalf("SendMail: %v", err)
	}

	srv.AssertWireBody(t, 0, body)

	msgs := srv.Sessions()[0].Messages()
	if len(msgs) != 1 {
		t.Fatalf("Messages() len = %d, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].From, "BODY=8BITMIME") {
		t.Fatalf("MAIL FROM args = %q, want BODY=8BITMIME (proves stdlib parsed the multiline EHLO caps)", msgs[0].From)
	}
}

// TestFakeAgainstStdlibClientStartTLS drives the fake's STARTTLS support
// with stdlib's lower-level Client API. smtp.SendMail can't be used here:
// its own automatic STARTTLS path builds a bare tls.Config{ServerName:
// ...} with no RootCAs, so it can never trust a self-signed TestCert —
// only the manual Dial/StartTLS/Mail/Rcpt/Data/Quit sequence lets the
// caller supply a trusting config.
func TestFakeAgainstStdlibClientStartTLS(t *testing.T) {
	cfg := TestCert(t)
	srv := Start(t, Script{TLS: cfg})

	c, err := smtp.Dial(srv.Addr())
	if err != nil {
		t.Fatalf("smtp.Dial: %v", err)
	}
	defer closeQuietly(c)

	if err := c.Hello("client.example"); err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		t.Fatalf("server did not advertise STARTTLS")
	}
	if err := c.StartTLS(cfg); err != nil {
		t.Fatalf("StartTLS: %v", err)
	}
	if err := c.Mail("sender@example.com"); err != nil {
		t.Fatalf("Mail: %v", err)
	}
	if err := c.Rcpt("rcpt@example.com"); err != nil {
		t.Fatalf("Rcpt: %v", err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("Data: %v", err)
	}
	body := []byte("Subject: hi over tls\r\n\r\nbody\r\n")
	if _, err := w.Write(body); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	if err := c.Quit(); err != nil {
		t.Fatalf("Quit: %v", err)
	}

	srv.AssertWireBody(t, 0, body)
}

// TestNon3yzDataLeavesNextCommandReadable is a regression case: a script
// that refuses DATA must refuse it for real. The fake used to read a body
// regardless of the reply it had just sent, swallowing the client's next
// command — which made a refused DATA look like a client desync in
// whatever was being tested against it.
func TestNon3yzDataLeavesNextCommandReadable(t *testing.T) {
	srv := Start(t, Script{OnDATA: []Step{{Reply: "451 4.3.0 not right now"}}})
	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner

	send := func(s string) {
		t.Helper()
		if _, err := conn.Write([]byte(s + "\r\n")); err != nil {
			t.Fatalf("write %q: %v", s, err)
		}
	}
	for _, c := range []string{"EHLO client.example", "MAIL FROM:<a@b>", "RCPT TO:<c@d>"} {
		send(c)
		readReplyLines(t, r)
	}

	send("DATA")
	if got, want := readReplyLines(t, r), "451 4.3.0 not right now"; len(got) != 1 || got[0] != want {
		t.Fatalf("DATA reply = %v, want [%q]", got, want)
	}

	// No body follows a refused DATA, so this is a command, not content.
	send("RSET")
	if got := readReplyLines(t, r); len(got) != 1 || got[0][0] != '2' {
		t.Fatalf("RSET reply = %v, want 2xx", got)
	}
	if got := srv.CmdCount("RSET"); got != 1 {
		t.Errorf("RSET count = %d, want 1: the command was read as body", got)
	}
}

package fakesmtp

import (
	"bufio"
	"crypto/tls"
	"strings"
	"testing"
)

func TestTestCertRoundTrip(t *testing.T) {
	cfg := TestCert(t)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer closeQuietly(ln)

	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer closeQuietly(conn)
		errCh <- conn.(*tls.Conn).Handshake()
	}()

	clientConn, err := tls.Dial("tcp", ln.Addr().String(), cfg)
	if err != nil {
		t.Fatalf("tls.Dial (client cfg trusting server cfg): %v", err)
	}
	defer closeQuietly(clientConn)

	if err := <-errCh; err != nil {
		t.Fatalf("server-side handshake: %v", err)
	}
}

func TestFakeStartTLS(t *testing.T) {
	cfg := TestCert(t)
	srv := Start(t, Script{Caps: []string{"PIPELINING"}, TLS: cfg})

	conn, r := dialRaw(t, srv.Addr())
	readReplyLines(t, r) // banner

	if _, err := conn.Write([]byte("EHLO client.example\r\n")); err != nil {
		t.Fatalf("write EHLO: %v", err)
	}
	ehlo1 := readReplyLines(t, r)
	if !strings.Contains(strings.Join(ehlo1, "\n"), "STARTTLS") {
		t.Fatalf("EHLO reply (pre-TLS) = %v, want STARTTLS advertised", ehlo1)
	}

	if _, err := conn.Write([]byte("STARTTLS\r\n")); err != nil {
		t.Fatalf("write STARTTLS: %v", err)
	}
	starttls := readReplyLines(t, r)
	if len(starttls) != 1 || !strings.HasPrefix(starttls[0], "220") {
		t.Fatalf("STARTTLS reply = %v, want 220", starttls)
	}

	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	t.Cleanup(func() { closeQuietly(tlsConn) })
	tr := bufio.NewReader(tlsConn)

	if _, err := tlsConn.Write([]byte("EHLO client.example\r\n")); err != nil {
		t.Fatalf("write EHLO over TLS: %v", err)
	}
	ehlo2 := readReplyLines(t, tr)
	joined2 := strings.Join(ehlo2, "\n")
	if strings.Contains(joined2, "STARTTLS") {
		t.Fatalf("EHLO reply (post-TLS) = %v, want STARTTLS no longer advertised", ehlo2)
	}
	if !strings.Contains(joined2, "PIPELINING") {
		t.Fatalf("EHLO reply (post-TLS) = %v, want PIPELINING still advertised", ehlo2)
	}

	if _, err := tlsConn.Write([]byte("QUIT\r\n")); err != nil {
		t.Fatalf("write QUIT over TLS: %v", err)
	}
	quit := readReplyLines(t, tr)
	if len(quit) != 1 || !strings.HasPrefix(quit[0], "221") {
		t.Fatalf("QUIT reply over TLS = %v, want 221", quit)
	}
}

// TestFakeTLSRequiredReject scripts a "must STARTTLS first" backend:
// MAIL is rejected pre-TLS, then must succeed post-TLS. OnMAIL needs two
// steps for that, not one — with only the 530 step, "last step repeats"
// would make the post-TLS MAIL 530 too, which would pass even a fake that
// couldn't tell pre- from post-TLS apart. The second, zero-value step
// falls back to the normal 250 default.
func TestFakeTLSRequiredReject(t *testing.T) {
	cfg := TestCert(t)
	srv := Start(t, Script{
		TLS: cfg,
		OnMAIL: []Step{
			{Reply: "530 5.7.0 Must issue a STARTTLS command first"},
			{},
		},
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
	got := readReplyLines(t, r)
	if len(got) != 1 || !strings.HasPrefix(got[0], "530") {
		t.Fatalf("MAIL reply (pre-TLS, scripted reject) = %v, want 530", got)
	}

	if _, err := conn.Write([]byte("STARTTLS\r\n")); err != nil {
		t.Fatalf("write STARTTLS: %v", err)
	}
	readReplyLines(t, r) // 220 go-ahead

	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	t.Cleanup(func() { closeQuietly(tlsConn) })
	tr := bufio.NewReader(tlsConn)

	if _, err := tlsConn.Write([]byte("EHLO client.example\r\n")); err != nil {
		t.Fatalf("write EHLO over TLS: %v", err)
	}
	readReplyLines(t, tr)

	if _, err := tlsConn.Write([]byte("MAIL FROM:<a@b>\r\n")); err != nil {
		t.Fatalf("write MAIL over TLS: %v", err)
	}
	got2 := readReplyLines(t, tr)
	if len(got2) != 1 || !strings.HasPrefix(got2[0], "250") {
		t.Fatalf("MAIL reply (post-TLS) = %v, want 250 (second OnMAIL step, not the 530 repeating)", got2)
	}
}

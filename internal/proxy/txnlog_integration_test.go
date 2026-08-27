//go:build integration

package proxy

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/revolee/bifrost/internal/fakesmtp"
)

// TestTxnLogNeverLogsBody is the privacy rule, proven for real: an
// actual message carrying a unique marker string is relayed end to end
// through a real fakesmtp backend, with the relay's whole slog output
// captured to a buffer, and the marker must never appear anywhere in
// it. Grepping every captured byte is the only way to actually prove
// "never logs the body" — a hand-built txnRecord (as this package's
// plain-suite TestTxnLogRecordComplete/TestTxnLogSynthAndDuplicateRisk
// use, in txnlog_test.go) only proves "this record's fields don't
// happen to contain it", which is a narrower claim.
//
// This test makes no claim about the log record's size independent of
// message size — for that, see TestStreamingCeiling1GB
// (test/integration/m1_test.go), which relays a 1 GiB message under a
// 64 MiB heap ceiling.
func TestTxnLogNeverLogsBody(t *testing.T) {
	const marker = "UNIQUE-BODY-MARKER-do-not-log-4f8b2c19"

	backend := relayFake(t, fakesmtp.Script{})
	cfg := relayConfig(backend.Addr())

	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, nil))
	f := newRelayClientLog(t, cfg, lg)

	f.send("MAIL FROM:<sender@test.example>")
	f.expect("250 2.1.0 OK")
	f.send("RCPT TO:<rcpt@test.example>")
	f.expect("250 2.1.5 OK")
	f.send("DATA")
	f.expect("354 Start mail input; end with <CRLF>.<CRLF>")
	f.raw("Subject: test\r\n\r\n" + marker + "\r\n.\r\n")
	f.expect("250 2.0.0 OK: queued")

	// The client seeing the final reply does not by itself mean the
	// session goroutine has returned from HandleTransaction (and run
	// its deferred emitLog) yet -- QUIT and waiting for the session to
	// actually exit does: dispatch(QUIT) only runs after mail()'s call
	// to HandleTransaction has returned on that same goroutine, so by
	// the time Run() itself returns, emitLog has already completed.
	// Reading buf before this synchronization is a real data race
	// (caught by -race), not just a flake.
	f.send("QUIT")
	f.expect("221 2.0.0 Bye")
	_ = f.wait()

	if strings.Contains(buf.String(), marker) {
		t.Fatalf("captured slog output contains the message body marker %q:\n--- captured output ---\n%s", marker, buf.String())
	}
}

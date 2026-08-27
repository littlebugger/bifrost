package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/netip"
	"testing"
	"time"
)

// captureLog returns a slog.Logger writing JSON records to buf, so a
// test can assert on emitLog's exact output without a real backend —
// txnlog.go's job is the log record's shape, not the relay behavior
// that populates it (covered elsewhere).
func captureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

// decodeLog parses buf's one JSON log line into a field map.
func decodeLog(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(buf.Bytes(), &fields); err != nil {
		t.Fatalf("log line is not valid JSON: %v (line: %s)", err, buf.String())
	}
	return fields
}

// TestTxnLogRecordComplete: one fully relayed message produces one log
// record carrying every documented field, correctly valued.
func TestTxnLogRecordComplete(t *testing.T) {
	lg, buf := captureLog()
	tx := &txn{
		r:  &Relay{lg: lg},
		tx: &Txn{ClientIP: netip.MustParseAddr("192.0.2.1"), Helo: "client.example"},
	}
	tx.record.start = time.Now().Add(-42 * time.Millisecond)
	tx.record.pool, tx.record.server = "p", "s1"
	tx.record.failoverAttempts = 1
	tx.record.observe("MAIL", 250, "250 2.1.0 OK")
	tx.record.observe("RCPT", 250, "250 2.1.5 OK")
	tx.record.dataVerdict = "250 2.0.0 OK: queued"
	tx.record.bytes = 13

	tx.emitLog()
	fields := decodeLog(t, buf)

	want := map[string]any{
		"client": "192.0.2.1", "helo": "client.example",
		"pool": "p", "server": "s1",
		"mail_verdict": float64(250), "rcpt_count": float64(1), "rcpt_verdicts_class": "2xx=1",
		"data_verdict": "250 2.0.0 OK: queued", "bytes": float64(13),
		"failover_attempts": float64(1), "synth": "", "duplicate_risk": false,
	}
	for k, v := range want {
		if got := fields[k]; got != v {
			t.Errorf("field %q = %v, want %v", k, got, v)
		}
	}
	if _, ok := fields["duration_ms"]; !ok {
		t.Errorf("missing duration_ms field")
	}
}

// TestTxnLogSynthAndDuplicateRisk: a backend that dies after the final
// dot but before a verdict (PROJECT.md's duplicate-delivery window, see
// data.go's pipeBody "body.delivered" case) is recorded with synth set
// to the reply Bifrost sent for it and duplicate_risk true.
func TestTxnLogSynthAndDuplicateRisk(t *testing.T) {
	lg, buf := captureLog()
	tx := &txn{
		r:  &Relay{lg: lg},
		tx: &Txn{ClientIP: netip.MustParseAddr("192.0.2.2"), Helo: "client.example"},
	}
	tx.record.start = time.Now()
	tx.record.pool, tx.record.server = "p", "s1"
	tx.record.observe("MAIL", 250, "250 2.1.0 OK")
	tx.record.observe("RCPT", 250, "250 2.1.5 OK")
	tx.record.bytes = 999 // the whole message reached the backend before it died
	// Mirrors data.go's pipeBody "case body.delivered" exactly:
	tx.record.duplicateRisk = true
	tx.record.synth = trimReply(RplBackendTimeout)

	tx.emitLog()
	fields := decodeLog(t, buf)

	if fields["duplicate_risk"] != true {
		t.Errorf("duplicate_risk = %v, want true", fields["duplicate_risk"])
	}
	if want := "451 4.4.2 Backend timeout"; fields["synth"] != want {
		t.Errorf("synth = %q, want %q", fields["synth"], want)
	}
	// The end-of-data verdict never arrived -- there is nothing to put
	// in data_verdict, and it must not be confused with synth.
	if fields["data_verdict"] != "" {
		t.Errorf("data_verdict = %q, want empty (no verdict was ever relayed)", fields["data_verdict"])
	}
}

// TestTxnLogNeverLogsBody: see txnlog_integration_test.go. It needs a
// real backend and a real message on the wire to actually prove the
// privacy claim (grepping captured output for a body marker), not a
// hand-built record — that version moved to this package's integration
// suite, which already has the fakesmtp/smtpdrv harness this needs.

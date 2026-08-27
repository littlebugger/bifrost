package health

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// compliantCaps is a capability set every plain probe in this file
// (L2/L3) advertises against, satisfied by fakesmtp's own default Caps
// list below unless a test deliberately narrows it.
func compliantCaps() []string { return []string{"PIPELINING", "8BITMIME"} }

// probeParams builds a CheckParams for one probe call: short but
// generous timeouts (real sockets, localhost), the given level.
func probeParams(level string) config.CheckParams {
	return config.CheckParams{Level: level, Timeout: 2 * time.Second, EhloName: "probe.test"}
}

func testSrv(addr string) *config.Server { return &config.Server{Address: addr} }

// deadAddr returns a "host:port" nobody is listening on: a fresh
// ephemeral listener, immediately closed, so the port is real but dead —
// more reliable than a hardcoded low port across test environments.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("deadAddr: listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// portOf extracts the numeric port from a "host:port" address.
func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("portOf(%q): %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("portOf(%q): %v", addr, err)
	}
	return port
}

// waitForSessionCount polls (bounded) until the fake has recorded at
// least n sessions — L0's accept-and-close races the server's acceptLoop
// bookkeeping (nothing was sent for the server to react to), unlike
// L1-L3 where the probe already waited on a reply before returning.
func waitForSessionCount(t *testing.T, srv *fakesmtp.Server, n int) []fakesmtp.Session {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		sessions := srv.Sessions()
		if len(sessions) >= n {
			return sessions
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForSessionCount: only %d sessions recorded, want >= %d", len(sessions), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// transcriptVerbs is the command verbs (DATA-body lines, which carry no
// verb, excluded) a fakesmtp session recorded, in order.
func transcriptVerbs(sess fakesmtp.Session) []string {
	var verbs []string
	for _, ev := range sess.Transcript() {
		if ev.Verb != "" {
			verbs = append(verbs, ev.Verb)
		}
	}
	return verbs
}

// TestProbeLadderLevels: L0 through L3 against one compliant fake, all
// pass; each level's transcript shows exactly the commands that level
// sends, and L1/L2/L3 all end with QUIT.
func TestProbeLadderLevels(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: compliantCaps()})

	cases := []struct {
		level string
		want  []string
	}{
		{"connect", nil},
		{"banner", []string{"QUIT"}},
		{"ehlo", []string{"EHLO", "QUIT"}},
		{"deep", []string{"EHLO", "MAIL", "RCPT", "RSET", "QUIT"}},
	}

	for i, tc := range cases {
		res := runProbe(context.Background(), testSrv(srv.Addr()), probeParams(tc.level), compliantCaps())
		if !res.ok {
			t.Errorf("level %q: ok = false, want true (reason=%q)", tc.level, res.reason)
		}
		if res.incompatible {
			t.Errorf("level %q: incompatible = true, want false", tc.level)
		}

		sessions := waitForSessionCount(t, srv, i+1)
		got := transcriptVerbs(sessions[i])
		if !equalStrings(got, tc.want) {
			t.Errorf("level %q: transcript verbs = %v, want %v", tc.level, got, tc.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestProbeConnectNoSmtpBytes: L0 dials and closes without ever sending
// or reading a byte; DialCount still increments (the fake did accept),
// but the recorded session's transcript is empty, and L0 never marks a
// server Incompatible.
func TestProbeConnectNoSmtpBytes(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: compliantCaps()})

	res := runProbe(context.Background(), testSrv(srv.Addr()), probeParams("connect"), compliantCaps())
	if !res.ok {
		t.Fatalf("ok = false, want true (reason=%q)", res.reason)
	}
	if res.incompatible {
		t.Errorf("incompatible = true, want false (L0 never harvests capabilities)")
	}

	sessions := waitForSessionCount(t, srv, 1)
	if got := srv.DialCount(); got != 1 {
		t.Fatalf("DialCount() = %d, want 1", got)
	}
	if got := transcriptVerbs(sessions[0]); len(got) != 0 {
		t.Errorf("transcript verbs = %v, want empty (zero SMTP bytes sent)", got)
	}
}

// TestProbePortOverride: the server's traffic address is dead, but
// check.port points at the live fake — proves the override redirects
// the probe (traffic-port state is irrelevant), for both L0 and L2.
func TestProbePortOverride(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: compliantCaps()})
	dead := deadAddr(t)
	livePort := portOf(t, srv.Addr())

	for _, level := range []string{"connect", "banner", "ehlo", "deep"} {
		t.Run(level, func(t *testing.T) {
			params := probeParams(level)
			params.Port = livePort
			res := runProbe(context.Background(), testSrv(dead), params, compliantCaps())
			if !res.ok {
				t.Fatalf("ok = false, want true (reason=%q) — override should have redirected to the live fake", res.reason)
			}
		})
	}

	// PROJECT.md: a port-overridden probe never marks a server
	// Incompatible — it's hitting a dedicated health socket, not the
	// traffic listener, so its capability set can't be assumed to speak
	// for the traffic path's.
	t.Run("suppresses incompatible verdict", func(t *testing.T) {
		deficient := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING"}}) // missing 8BITMIME
		params := probeParams("ehlo")
		params.Port = portOf(t, deficient.Addr())
		res := runProbe(context.Background(), testSrv(dead), params, []string{"PIPELINING", "8BITMIME"})
		if !res.ok {
			t.Fatalf("ok = false, want true (reason=%q)", res.reason)
		}
		if res.incompatible {
			t.Errorf("incompatible = true, want false (port override suppresses the superset check)")
		}
	})
}

// TestProbeConnectRefused: a closed listener refuses the TCP connect
// itself; the failure is labeled connect-refused.
func TestProbeConnectRefused(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{})
	srv.SetDown(fakesmtp.DownListenerClosed)

	res := runProbe(context.Background(), testSrv(srv.Addr()), probeParams("connect"), nil)
	if res.ok {
		t.Fatalf("ok = true, want false (listener closed)")
	}
	if res.reason != "connect-refused" {
		t.Errorf("reason = %q, want %q", res.reason, "connect-refused")
	}
}

// TestProbeBannerTimeout: the fake accepts but never sends a banner
// (AcceptThenHang); L1's read times out, labeled banner-timeout —
// distinct from connect-refused (the TCP connect itself succeeded).
func TestProbeBannerTimeout(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{})
	srv.SetDown(fakesmtp.DownAcceptThenHang)

	params := probeParams("banner")
	params.Timeout = 200 * time.Millisecond
	res := runProbe(context.Background(), testSrv(srv.Addr()), params, nil)
	if res.ok {
		t.Fatalf("ok = true, want false (banner never arrives)")
	}
	if res.reason != "banner-timeout" {
		t.Errorf("reason = %q, want %q", res.reason, "banner-timeout")
	}
}

// TestProbeWrongBanner: a 554 greeting fails L1.
func TestProbeWrongBanner(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Banner: fakesmtp.Step{Reply: "554 no"}})

	res := runProbe(context.Background(), testSrv(srv.Addr()), probeParams("banner"), nil)
	if res.ok {
		t.Fatalf("ok = true, want false (554 greeting)")
	}
	if res.reason != "wrong-banner" {
		t.Errorf("reason = %q, want %q", res.reason, "wrong-banner")
	}
}

// TestProbeEhlo502FailsL2PassesL1: the same fake scripts EHLO to reject
// with 502. L1 never sends EHLO, so it passes; L2 sends EHLO, so it
// fails.
func TestProbeEhlo502FailsL2PassesL1(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{
		OnEHLO: []fakesmtp.Step{{Reply: "502 5.5.1 command not implemented"}},
	})

	l1 := runProbe(context.Background(), testSrv(srv.Addr()), probeParams("banner"), nil)
	if !l1.ok {
		t.Errorf("L1: ok = false, want true (reason=%q) — L1 never sends EHLO", l1.reason)
	}

	l2 := runProbe(context.Background(), testSrv(srv.Addr()), probeParams("ehlo"), nil)
	if l2.ok {
		t.Errorf("L2: ok = true, want false (EHLO rejected with 502)")
	}
}

// TestProbeDeepRcpt450Fails: a 450 on RCPT (greylisting a probe sender
// is a common real-world reaction) fails the deep probe — a documented
// caveat, which is exactly why deep checks are off by default
// (PROJECT.md): operators enabling them against a greylisting backend
// should expect and tune around this.
func TestProbeDeepRcpt450Fails(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{
		Caps:   compliantCaps(),
		OnRCPT: []fakesmtp.Step{{Reply: "450 4.2.0 mailbox temporarily unavailable"}},
	})

	res := runProbe(context.Background(), testSrv(srv.Addr()), probeParams("deep"), compliantCaps())
	if res.ok {
		t.Fatalf("ok = true, want false (450 on RCPT)")
	}
	if res.reason != "rcpt-rejected" {
		t.Errorf("reason = %q, want %q", res.reason, "rcpt-rejected")
	}
}

// TestProbeTLSMismatch proves CheckParams.TLS is actually wired into the
// probe's backend.Opts.TLSMode: against a fake that never advertises
// STARTTLS, probe_tls=starttls fails (not offered) while probe_tls=none
// passes (never attempts it) — the same distinction
// internal/backend/tls_test.go's TestBackendTLSRequiredMismatch proves
// at the Dial layer, exercised here through the probe.
func TestProbeTLSMismatch(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: compliantCaps()}) // Script.TLS nil: STARTTLS never advertised

	t.Run("starttls required but not offered", func(t *testing.T) {
		params := probeParams("ehlo")
		params.TLS = "starttls"
		res := runProbe(context.Background(), testSrv(srv.Addr()), params, nil)
		if res.ok {
			t.Fatalf("ok = true, want false (backend never advertises STARTTLS)")
		}
	})

	t.Run("none never attempts it", func(t *testing.T) {
		params := probeParams("ehlo")
		params.TLS = "none"
		res := runProbe(context.Background(), testSrv(srv.Addr()), params, nil)
		if !res.ok {
			t.Fatalf("ok = false, want true (reason=%q)", res.reason)
		}
	})
}

// TestProbeHarvestsCapsAndSupersetVerdict is
// backend-missing-capability-marked-out at the health layer: a backend
// missing 8BITMIME against an advertised set that requires it is marked
// Incompatible, while its op-wise probe result stays a plain success
// (the EHLO handshake itself worked fine).
func TestProbeHarvestsCapsAndSupersetVerdict(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING"}}) // 8BITMIME missing

	res := runProbe(context.Background(), testSrv(srv.Addr()), probeParams("ehlo"), []string{"PIPELINING", "8BITMIME"})
	if !res.ok {
		t.Errorf("ok = false, want true (op-wise the handshake succeeded; reason=%q)", res.reason)
	}
	if !res.incompatible {
		t.Errorf("incompatible = false, want true (8BITMIME missing)")
	}
}

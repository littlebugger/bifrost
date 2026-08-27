//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
	"github.com/littlebugger/bifrost/internal/smtpdrv"
)

// Epic-10 Task 1: the assembled process. These tests drive the real
// binary, so what they prove is the wiring an operator gets — config
// file to listener to router to relay to admin plane — not a
// hand-assembled approximation of it.

// serversResponse mirrors GET /servers' body (internal/admin/servers.go)
// for the fields these tests assert on.
type serversResponse struct {
	Servers []struct {
		Pool     string `json:"pool"`
		Server   string `json:"server"`
		Op       string `json:"op"`
		Admin    string `json:"admin"`
		Weight   int    `json:"weight"`
		InFlight int    `json:"in_flight"`
	} `json:"servers"`
}

// adminServers fetches and decodes GET /servers.
func adminServers(t *testing.T, b *bifrost) serversResponse {
	t.Helper()
	code, body := b.get("/servers")
	if code != 200 {
		t.Fatalf("GET /servers = %d, body %s", code, body)
	}
	var out serversResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode /servers %q: %v", body, err)
	}
	return out
}

// metricValue returns the value of the first /metrics sample line whose
// text starts with prefix (e.g.
// `bifrost_transactions_total{pool="p",server="a",verdict_class="2xx"}`),
// or -1 when no such sample exists.
func metricValue(t *testing.T, b *bifrost, prefix string) float64 {
	t.Helper()
	code, body := b.get("/metrics")
	if code != 200 {
		t.Fatalf("GET /metrics = %d, body %s", code, body)
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parse metric line %q: %v", line, err)
		}
		return v
	}
	return -1
}

// twoBackendFixture is the shared Task-1 shape: one pool, two equally
// weighted backends that name themselves in their end-of-data verdict.
func twoBackendFixture(t *testing.T) (*bifrost, *fakesmtp.Server, *fakesmtp.Server) {
	t.Helper()
	a, bk := namedFake(t, "A"), namedFake(t, "B")
	smtp, adminAddr := freeAddr(t), freeAddr(t)
	cfg := fixture{
		smtp:  smtp,
		admin: adminAddr,
		pools: poolHCL("p", serverHCL("a", a.Addr(), 1), serverHCL("b", bk.Addr(), 1)),
	}.render()
	path := writeFile(t, t.TempDir(), "bifrost.hcl", cfg)
	return startBifrost(t, path, smtp, adminAddr), a, bk
}

// TestBinaryEndToEnd is the epic's headline claim: the built binary, one
// config file, one client connection, four messages — spread across both
// backends per weight, every verdict relayed verbatim, the admin plane
// reporting the same reality, the metrics moving, and a clean SIGTERM
// exit.
func TestBinaryEndToEnd(t *testing.T) {
	b, _, _ := twoBackendFixture(t)

	c := smtpdrv.Dial(t, b.smtp)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")

	verdicts := map[string]int{}
	for i := 0; i < 4; i++ {
		reply := c.SendMsg(i)
		if len(reply.Lines) != 1 {
			t.Fatalf("message %d verdict = %q, want a single line", i, reply.Lines)
		}
		verdicts[reply.Lines[0]]++
	}
	c.Send("QUIT")
	c.Expect("221")

	// Verbatim, and spread: the backends' own reply text, two each.
	for _, name := range []string{"A", "B"} {
		want := "250 2.0.0 OK: queued via " + name
		if got := verdicts[want]; got != 2 {
			t.Errorf("verdict %q seen %d times, want 2 (verdicts: %v)", want, got, verdicts)
		}
	}

	// The admin plane reflects the same reality.
	servers := adminServers(t, b)
	if len(servers.Servers) != 2 {
		t.Fatalf("GET /servers listed %d servers, want 2: %+v", len(servers.Servers), servers.Servers)
	}
	for _, s := range servers.Servers {
		if s.Pool != "p" || s.Op != "UP" || s.Admin != "READY" || s.Weight != 1 {
			t.Errorf("server %q = %+v, want pool p / UP / READY / weight 1", s.Server, s)
		}
		if s.InFlight != 0 {
			t.Errorf("server %q in_flight = %d after every transaction ended, want 0", s.Server, s.InFlight)
		}
	}

	// Metrics moved, per server, through the real registry.
	for _, name := range []string{"a", "b"} {
		prefix := fmt.Sprintf("bifrost_transactions_total{pool=\"p\",server=%q,verdict_class=\"2xx\"}", name)
		if got := metricValue(t, b, prefix); got != 2 {
			t.Errorf("%s = %v, want 2", prefix, got)
		}
	}
	if got := metricValue(t, b, "bifrost_sessions_total"); got < 1 {
		t.Errorf("bifrost_sessions_total = %v, want >= 1", got)
	}

	b.signal(syscall.SIGTERM)
	if code := b.waitExit(20 * time.Second); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0\nlogs:\n%s", code, b.logText())
	}
}

// TestBinaryConfigCheckMode is epic-02's -c mode re-run against a FULL
// wiring config: listener certificate, backend CA, pools, routing, admin
// — everything the server path itself loads. A config the server would
// refuse to start on must fail -c, or -c is not a pre-flight check.
func TestBinaryConfigCheckMode(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := writeCertPair(t, dir, "listener")
	caPath, _, _ := writeCertPair(t, dir, "ca")
	fake := namedFake(t, "A")

	pools := poolHCL("p", serverHCL("a", fake.Addr(), 1)) +
		fmt.Sprintf(`
pool "verified" {
  balance                 = "roundrobin"
  backend_tls             = "starttls-verify"
  backend_tls_server_name = "127.0.0.1"
  backend_tls_ca          = %q
%s}
`, caPath, serverHCL("v", fake.Addr(), 1))

	good := fixture{
		smtp:  "127.0.0.1:0",
		admin: "127.0.0.1:0",
		caps:  `["PIPELINING", "8BITMIME", "STARTTLS"]`,
		starttls: fmt.Sprintf(`
  starttls {
    cert        = %q
    key         = %q
    min_version = "1.2"
  }
`, certPath, keyPath),
		pools: pools,
	}
	goodPath := writeFile(t, dir, "good.hcl", good.render())

	bad := good
	bad.starttls = strings.Replace(bad.starttls, keyPath, filepath.Join(dir, "missing.key"), 1)
	badPath := writeFile(t, dir, "bad.hcl", bad.render())

	badCA := good
	badCA.pools = strings.Replace(badCA.pools, caPath, filepath.Join(dir, "missing-ca.crt"), 1)
	badCAPath := writeFile(t, dir, "bad-ca.hcl", badCA.render())

	cases := []struct {
		name     string
		path     string
		wantCode int
		wantMsg  string
	}{
		{"full wiring config", goodPath, 0, "config OK"},
		{"unreadable listener key", badPath, 1, "starttls key unreadable"},
		{"unreadable backend CA", badCAPath, 1, "backend_tls_ca"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := runCheckMode(t, tc.path)
			if code != tc.wantCode {
				t.Fatalf("bifrost -c -f %s = %d, want %d\noutput:\n%s", tc.path, code, tc.wantCode, out)
			}
			if !strings.Contains(out, tc.wantMsg) {
				t.Errorf("output = %q, want it to contain %q", out, tc.wantMsg)
			}
		})
	}
}

// TestBackendPrivateCAStartTLSVerify closes the ledger's blocking carry:
// a pool with backend_tls = "starttls-verify" and a private
// backend_tls_ca must actually deliver mail (the CA has to reach the
// dialer's RootCAs), and the health checker's own L2 probe — which
// handshakes through the same path — must pass against it too. Before
// this epic the CA was parsed by config and dropped on the floor, so
// starttls-verify was broken closed in both places.
func TestBackendPrivateCAStartTLSVerify(t *testing.T) {
	dir := t.TempDir()
	caPath, _, tlsCfg := writeCertPair(t, dir, "backend-ca")

	fake := fakesmtp.Start(t, fakesmtp.Script{
		Caps:  backendCaps(),
		TLS:   tlsCfg,
		OnEOD: []fakesmtp.Step{{Reply: "250 2.0.0 OK: queued over verified TLS"}},
	})

	smtp, adminAddr := freeAddr(t), freeAddr(t)
	pool := fmt.Sprintf(`
pool "p" {
  balance                 = "roundrobin"
  backend_tls             = "starttls-verify"
  backend_tls_server_name = "127.0.0.1"
  backend_tls_ca          = %q
%s}
`, caPath, serverHCL("v", fake.Addr(), 1))
	path := writeFile(t, dir, "bifrost.hcl", fixture{smtp: smtp, admin: adminAddr, pools: pool}.render())
	b := startBifrost(t, path, smtp, adminAddr)

	c := smtpdrv.Dial(t, b.smtp)
	c.Expect("220")
	c.Send("EHLO client.example")
	c.Expect("250")
	reply := c.SendMsg(0)
	if want := "250 2.0.0 OK: queued over verified TLS"; len(reply.Lines) != 1 || reply.Lines[0] != want {
		t.Fatalf("verdict = %q, want [%q] — starttls-verify with a private CA did not deliver", reply.Lines, want)
	}
	c.Send("QUIT")
	c.Expect("221")

	if got := fake.CmdCount("STARTTLS"); got < 1 {
		t.Errorf("backend STARTTLS count = %d, want >= 1: the leg was not upgraded", got)
	}

	// The L2 probe dials the same backend through the same CA. A probe
	// that could not verify would report "fail", and this poll would
	// never see an ok result.
	waitProbeOK(t, b, "p", "v")
}

// waitProbeOK polls /metrics until (pool, server) has recorded at least
// one successful active probe — the health checker's own verdict on a
// backend, as opposed to the relay's.
func waitProbeOK(t *testing.T, b *bifrost, pool, server string) {
	t.Helper()
	prefix := fmt.Sprintf("bifrost_probe_total{level=\"ehlo\",result=\"ok\",server=%q}", server)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if v := metricValue(t, b, prefix); v >= 1 {
			return
		}
		if time.Now().After(deadline) {
			_, body := b.get("/servers")
			t.Fatalf("no successful ehlo probe for %s/%s within 15s; /servers = %s\nlogs:\n%s",
				pool, server, body, b.logText())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

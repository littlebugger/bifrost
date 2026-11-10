package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/smtpdrv"
)

// requireContains fails t unless reply's joined lines contain want — the
// per-listener hostname check TestTwoListenersServeIndependently uses on
// both the banner and the EHLO reply.
func requireContains(t *testing.T, reply smtpdrv.Reply, want string) {
	t.Helper()
	if !strings.Contains(strings.Join(reply.Lines, "\n"), want) {
		t.Errorf("reply = %v, want it to contain %q", reply.Lines, want)
	}
}

func TestCheckModeOK(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-c", "-f", "../../examples/bifrost.hcl"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(-c examples/bifrost.hcl) = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "config OK") {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), "config OK")
	}
}

func TestCheckModeBad(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fixture := "../../internal/config/testdata/bad-weight.hcl"
	code := run([]string{"-c", "-f", fixture}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(-c %s) = %d, want 1; stdout=%q stderr=%q", fixture, code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "config OK") {
		t.Errorf("stdout = %q, want no \"config OK\" on a failing check", stdout.String())
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "bad-weight.hcl:") {
		t.Errorf("stderr = %q, want a diagnostic naming %s with a line number (file:line)", errOut, fixture)
	}
	if !strings.Contains(errOut, "Weight out of range") {
		t.Errorf("stderr = %q, want it to mention the specific violation", errOut)
	}
}

// TestServeDNSDiscoveryListsMembersAcrossReload drives run()/serve in
// process (self-signaled: serve's own signal.Notify is in THIS process,
// so sending SIGHUP/SIGTERM to os.Getpid() reaches it exactly like an
// operator's kill -HUP would) with a config carrying one server_dns
// template and the package-level defaultLookup overridden to a fake
// resolver. It proves the whole wiring end to end: the Runner is
// actually started (GET /servers lists the discovered node-<ip> members)
// and a SIGHUP reload goes through Rerender rather than a bare
// holder.Swap — an edited file (here, the template's weight) takes
// effect while the discovered membership itself is fail-static across
// the reload (kept from the last resolution, not re-derived from it).
func TestServeDNSDiscoveryListsMembersAcrossReload(t *testing.T) {
	origLookup := defaultLookup
	t.Cleanup(func() { defaultLookup = origLookup })
	defaultLookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.42.0.1"), netip.MustParseAddr("10.42.0.2")}, nil
	}

	smtpAddr, adminAddr := freePort(t), freePort(t)
	path := filepath.Join(t.TempDir(), "bifrost.hcl")
	if err := os.WriteFile(path, []byte(dnsDiscoveryFixture(smtpAddr, adminAddr, 1)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"-f", path}, io.Discard, &stderr) }()

	// serve's own signal.Notify is unregistered (defer signal.Stop) the
	// moment serve returns, so a cleanup-time SIGTERM sent AFTER the
	// normal flow below has already drained done would have no handler
	// left to catch it — default disposition kills the whole test binary
	// instead of just the long-gone serve goroutine. stopped guards
	// against exactly that double-signal race: this cleanup is a safety
	// net for a test that fails before reaching its own clean shutdown,
	// not a second shutdown on top of a successful one.
	var stopped atomic.Bool
	t.Cleanup(func() {
		if stopped.Load() {
			return
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("serve did not exit during cleanup; logs:\n%s", stderr.String())
		}
	})

	adminURL := "http://" + adminAddr
	waitForStatus(t, adminURL+"/healthz", http.StatusOK)
	waitForServersContaining(t, adminURL, "node-10.42.0.1", "node-10.42.0.2")

	// Edit the file (the template's weight, not its membership) and
	// reload: fail-static means the discovered IPs survive untouched,
	// while the edited weight takes effect.
	if err := os.WriteFile(path, []byte(dnsDiscoveryFixture(smtpAddr, adminAddr, 5)), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	before := strings.Count(stderr.String(), "config reloaded")
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP: %v", err)
	}
	waitForLogCount(t, &stderr, "config reloaded", before+1)

	body := waitForServersContaining(t, adminURL, "node-10.42.0.1", "node-10.42.0.2")
	if !strings.Contains(body, `"weight":5`) {
		t.Errorf("GET /servers after reload = %s, want the edited weight 5 applied", body)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case code := <-done:
		stopped.Store(true)
		if code != 0 {
			t.Errorf("run() exit code = %d, want 0; logs:\n%s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("serve did not exit after SIGTERM; logs:\n%s", stderr.String())
	}
}

// TestTwoListenersServeIndependently is Task 3's cmd/bifrost proof: a real
// process, booted from a config with two labeled listener blocks, accepts
// and serves sessions on both ports at once — not just that wire.go builds
// two sockets, but that main.go's N accept loops actually run them. Each
// listener's own hostname shows up in its own banner/EHLO, proving the two
// aren't secretly sharing one Session path.
func TestTwoListenersServeIndependently(t *testing.T) {
	addrA, addrB, adminAddr := freePort(t), freePort(t), freePort(t)
	path := filepath.Join(t.TempDir(), "bifrost.hcl")
	if err := os.WriteFile(path, []byte(twoListenerFixture(addrA, addrB, adminAddr)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"-f", path}, io.Discard, &stderr) }()

	var stopped atomic.Bool
	t.Cleanup(func() {
		if stopped.Load() {
			return
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("serve did not exit during cleanup; logs:\n%s", stderr.String())
		}
	})

	waitForStatus(t, "http://"+adminAddr+"/healthz", http.StatusOK)

	a := smtpdrv.Dial(t, addrA)
	requireContains(t, a.Expect("220"), "a.bifrost.test")
	a.Send("EHLO client.example")
	requireContains(t, a.Expect("250"), "a.bifrost.test")

	b := smtpdrv.Dial(t, addrB)
	requireContains(t, b.Expect("220"), "b.bifrost.test")
	b.Send("EHLO client.example")
	requireContains(t, b.Expect("250"), "b.bifrost.test")

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case code := <-done:
		stopped.Store(true)
		if code != 0 {
			t.Errorf("run() exit code = %d, want 0; logs:\n%s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("serve did not exit after SIGTERM; logs:\n%s", stderr.String())
	}
}

// TestOneListenerStoppingUnexpectedlyFailsProcess proves the per-listener
// fail-safe restored in main.go's smtp-listener closures: with two
// listeners running, killing ONE listener's socket directly (no drain, no
// signal — the way an external fd/fs failure would) must still fail the
// whole process, even though the other listener is perfectly healthy and
// serveDone (now an ALL-listeners-returned barrier) would otherwise never
// fire on its own.
func TestOneListenerStoppingUnexpectedlyFailsProcess(t *testing.T) {
	addrA, addrB, adminAddr := freePort(t), freePort(t), freePort(t)
	path := filepath.Join(t.TempDir(), "bifrost.hcl")
	if err := os.WriteFile(path, []byte(twoListenerFixture(addrA, addrB, adminAddr)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	appc := make(chan *app, 1)
	appHook = func(a *app) { appc <- a }
	t.Cleanup(func() { appHook = nil })

	var stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"-f", path}, io.Discard, &stderr) }()

	var stopped atomic.Bool
	t.Cleanup(func() {
		if stopped.Load() {
			return
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("serve did not exit during cleanup; logs:\n%s", stderr.String())
		}
	})

	waitForStatus(t, "http://"+adminAddr+"/healthz", http.StatusOK)

	var a *app
	select {
	case a = <-appc:
	case <-time.After(5 * time.Second):
		t.Fatalf("app never reached appHook")
	}

	// A healthy session on listener "b", open before "a" dies, to prove
	// shutdown reaches every listener's sessions, not just the one that
	// failed.
	connB, err := net.Dial("tcp", addrB)
	if err != nil {
		t.Fatalf("dial listener b: %v", err)
	}
	defer func() { _ = connB.Close() }()
	rb := bufio.NewReader(connB)
	if banner, err := rb.ReadString('\n'); err != nil || !strings.HasPrefix(banner, "220") {
		t.Fatalf("banner on listener b = %q, err=%v", banner, err)
	}

	var lnA net.Listener
	for _, nl := range a.smtpLns {
		if nl.name == "a" {
			lnA = nl.ln
		}
	}
	if lnA == nil {
		t.Fatalf("listener %q not found in app.smtpLns", "a")
	}
	if err := lnA.Close(); err != nil {
		t.Fatalf("close listener a: %v", err)
	}

	select {
	case code := <-done:
		stopped.Store(true)
		if code != 1 {
			t.Errorf("run() exit code = %d, want 1; logs:\n%s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("serve did not exit after listener a died; logs:\n%s", stderr.String())
	}

	if logs := stderr.String(); !strings.Contains(logs, "smtp listener a:") || !strings.Contains(logs, "stopped unexpectedly") {
		t.Errorf("logs = %s, want a per-listener stopped-unexpectedly error naming %q", logs, "a")
	}

	waitConnClosed(t, connB, 10*time.Second)
}

// oneListenerStartTLSFixture is a single STARTTLS listener, labeled with
// name (empty means unlabeled, which config.Load defaults to "default"),
// at certPath/keyPath -- the final-review item 2 fixture: a reload can
// rename this listener without rebinding or re-pointing its certificate.
func oneListenerStartTLSFixture(name, smtpAddr, adminAddr, certPath, keyPath string) string {
	label := ""
	if name != "" {
		label = fmt.Sprintf("%q ", name)
	}
	return fmt.Sprintf(`
listener %s{
  bind     = %q
  hostname = "bifrost.test"

  starttls {
    cert = %q
    key  = %q
  }
}

pool "p" {
  balance = "roundrobin"
  server "mta1" { address = "192.0.2.1:25" }
}

routing {
  default_pool = "p"
}

admin {
  bind = %q
}
`, label, smtpAddr, certPath, keyPath, adminAddr)
}

// TestReloadRenamedListenerKeepsSTARTTLS is the final-review fix for item
// 2: wire.go's currentCert looks up a listener's certificate by name in
// the LIVE holder, but the GetCertificate closure built for the socket is
// bound to the name that listener had at startup (wire.go's listen/
// listenerTLS never rebuild it on reload). Renaming the listener here
// makes listenerNamed(newCfg, "default") return nil, which must self-heal
// to the cached certificate (with a one-time warning) instead of failing
// every handshake on the still-open socket while plaintext keeps working.
func TestReloadRenamedListenerKeepsSTARTTLS(t *testing.T) {
	certPath, err := filepath.Abs("../../examples/server.crt")
	if err != nil {
		t.Fatalf("abs cert path: %v", err)
	}
	keyPath, err := filepath.Abs("../../examples/server.key")
	if err != nil {
		t.Fatalf("abs key path: %v", err)
	}

	smtpAddr, adminAddr := freePort(t), freePort(t)
	path := filepath.Join(t.TempDir(), "bifrost.hcl")
	if err := os.WriteFile(path, []byte(oneListenerStartTLSFixture("", smtpAddr, adminAddr, certPath, keyPath)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"-f", path}, io.Discard, &stderr) }()

	var stopped atomic.Bool
	t.Cleanup(func() {
		if stopped.Load() {
			return
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("serve did not exit during cleanup; logs:\n%s", stderr.String())
		}
	})

	waitForStatus(t, "http://"+adminAddr+"/healthz", http.StatusOK)

	clientTLS := &tls.Config{InsecureSkipVerify: true}

	// Sanity: STARTTLS works under the listener's original name "default",
	// before any reload.
	before := smtpdrv.Dial(t, smtpAddr)
	before.Expect("220")
	before.Send("EHLO client.example")
	before.Expect("250")
	before.StartTLS(clientTLS)
	before.Send("QUIT")
	before.Expect("221")

	// Rename the listener and reload: BindChange only rejects an edited
	// bind, never a rename (a rename is "removed" + "added", each its own
	// RestartRequired warning, not a rejection).
	if err := os.WriteFile(path, []byte(oneListenerStartTLSFixture("renamed", smtpAddr, adminAddr, certPath, keyPath)), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	reloadsBefore := strings.Count(stderr.String(), "config reloaded")
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP: %v", err)
	}
	waitForLogCount(t, &stderr, "config reloaded", reloadsBefore+1)

	// The still-open socket must keep answering STARTTLS under its stale
	// name, not fail every handshake while plaintext keeps working.
	after := smtpdrv.Dial(t, smtpAddr)
	after.Expect("220")
	after.Send("EHLO client.example")
	after.Expect("250")
	after.StartTLS(clientTLS)
	after.Send("QUIT")
	after.Expect("221")

	if logs := stderr.String(); !strings.Contains(logs, "still-open socket keeps serving the stale certificate") {
		t.Errorf("logs = %s, want the one-time stale-certificate warning", logs)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case code := <-done:
		stopped.Store(true)
		if code != 0 {
			t.Errorf("run() exit code = %d, want 0; logs:\n%s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("serve did not exit after SIGTERM; logs:\n%s", stderr.String())
	}
}

// waitConnClosed bounded-polls conn until a read fails (EOF or otherwise)
// — proof the peer closed it — and fails t if the deadline passes with the
// connection still open (every successful read is discarded; a drained
// session may still have a goodbye line queued ahead of the close).
func waitConnClosed(t *testing.T, conn net.Conn, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	buf := make([]byte, 512)
	for {
		_ = conn.SetReadDeadline(deadline)
		_, err := conn.Read(buf)
		if err == nil {
			continue
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Errorf("connection not closed within %s", d)
			return
		}
		return
	}
}

// twoListenerFixture is two labeled listeners ("a", "b"), each with its own
// hostname, sharing one pool and routing — the smallest config that makes
// main.go actually run two accept loops.
func twoListenerFixture(addrA, addrB, adminAddr string) string {
	return fmt.Sprintf(`
listener "a" {
  bind     = %q
  hostname = "a.bifrost.test"
}

listener "b" {
  bind     = %q
  hostname = "b.bifrost.test"
}

pool "p" {
  balance = "roundrobin"
  server "mta1" { address = "192.0.2.1:25" }
}

routing {
  default_pool = "p"
}

admin {
  bind = %q
}
`, addrA, addrB, adminAddr)
}

// dnsDiscoveryFixture is the smallest config carrying one server_dns
// template (pool "routes", label "node"): a listener, that one pool, a
// default route to it, and an admin plane to observe /servers over.
func dnsDiscoveryFixture(smtpAddr, adminAddr string, weight int) string {
	return fmt.Sprintf(`
listener {
  bind     = %q
  hostname = "bifrost.test"
}

pool "routes" {
  balance = "roundrobin"

  server_dns "node" {
    address          = "mta.discovery.test:587"
    resolve_interval = "1s"
    weight           = %d
  }
}

routing {
  default_pool = "routes"
}

admin {
  bind = %q
}
`, smtpAddr, weight, adminAddr)
}

// freePort reserves a loopback port by binding and immediately releasing
// it: the config file needs a concrete bind address before the process
// starts, so ":0" is not an option (nothing could discover which port it
// got).
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return addr
}

// syncBuffer is a bytes.Buffer safe for serve's slog handler (one
// writer) and the test polling its content (one reader) concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForStatus bounded-polls url until it answers want.
func waitForStatus(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get(url); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s never answered %d", url, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForServersContaining bounded-polls adminURL+"/servers" until its
// body contains every one of want, and returns that body — the
// event-driven way to wait for the Runner's async first resolution (or a
// reload's swap) to land, instead of sleeping a guessed duration.
func waitForServersContaining(t *testing.T, adminURL string, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get(adminURL + "/servers"); err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			text := string(body)
			ok := true
			for _, s := range want {
				if !strings.Contains(text, s) {
					ok = false
					break
				}
			}
			if ok {
				return text
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s/servers never contained %v", adminURL, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForLogCount bounded-polls buf until marker appears at least want
// times.
func waitForLogCount(t *testing.T, buf *syncBuffer, marker string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(buf.String(), marker) < want {
		if time.Now().After(deadline) {
			t.Fatalf("log never reached %d occurrences of %q; logs:\n%s", want, marker, buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

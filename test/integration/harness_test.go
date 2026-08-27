//go:build integration

package integration

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/fakesmtp"
)

// This file is epic-10's harness: it drives the REAL bifrost binary as a
// child process, because that is the only way to test what this epic
// produces — signal handling (SIGTERM drain, SIGHUP reload), process exit
// codes, and the full wiring as an operator gets it.
//
// It is os/exec rather than the epic plan's "in-process run()" for a
// language reason, not a preference: run() lives in package main, and a
// Go test outside that package cannot call it (main packages are not
// importable). The plan's own gates name test/integration as the package,
// so the binary is exercised the way an operator does.

// bifrostBin builds cmd/bifrost once per test binary run and returns the
// executable's path.
//
// The build directory is fixed, not a fresh MkdirTemp: nothing can clean
// it up afterwards (goleak.VerifyTestMain calls os.Exit, so no
// TestMain-level teardown ever runs), and a per-run directory therefore
// leaked a ~15 MB binary on every single `make integration`. One reused
// path, overwritten by each build, costs nothing and grows never.
var bifrostBin = sync.OnceValues(func() (string, error) {
	dir := filepath.Join(os.TempDir(), "bifrost-integration-bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "bifrost")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/bifrost")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build ./cmd/bifrost: %v\n%s", err, out)
	}
	return bin, nil
})

// freeAddr reserves a loopback port by binding and immediately releasing
// it: the child process needs its bind address in the config file before
// it starts, so ":0" is not an option (nothing could then discover which
// port it got).
func freeAddr(t *testing.T) string {
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

// runCheckMode runs "bifrost -c -f path" to completion and returns its
// exit code plus everything it wrote (stdout and stderr together: "config
// OK" goes to one, diagnostics to the other).
func runCheckMode(t *testing.T, path string) (int, string) {
	t.Helper()
	bin, err := bifrostBin()
	if err != nil {
		t.Fatalf("build bifrost: %v", err)
	}
	out, err := exec.Command(bin, "-c", "-f", path).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("bifrost -c -f %s: %v", path, err)
	return -1, string(out)
}

// bifrost is a running child bifrost process.
type bifrost struct {
	t        *testing.T
	cmd      *exec.Cmd
	smtp     string
	adminURL string
	cfgPath  string

	mu     sync.Mutex
	logs   bytes.Buffer
	copied chan struct{}
	waited chan error
}

// startBifrost starts the binary on cfgPath and returns once GET /healthz
// answers 200 — the process is then accepting SMTP too (the SMTP listener
// is bound before the admin plane starts serving).
func startBifrost(t *testing.T, cfgPath, smtpAddr, adminAddr string) *bifrost {
	t.Helper()
	bin, err := bifrostBin()
	if err != nil {
		t.Fatalf("build bifrost: %v", err)
	}

	cmd := exec.Command(bin, "-f", cfgPath)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bifrost: %v", err)
	}

	b := &bifrost{
		t: t, cmd: cmd, smtp: smtpAddr, cfgPath: cfgPath,
		adminURL: "http://" + adminAddr,
		copied:   make(chan struct{}),
		waited:   make(chan error, 1),
	}
	go func() {
		defer close(b.copied)
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				b.mu.Lock()
				b.logs.Write(buf[:n])
				b.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { b.waited <- cmd.Wait() }()

	t.Cleanup(func() {
		if !b.exited() {
			_ = cmd.Process.Kill()
			<-b.waited
		}
		<-b.copied
		if t.Failed() {
			t.Logf("bifrost logs:\n%s", b.logText())
		}
	})

	b.waitReady()
	return b
}

// waitReady polls /healthz until it answers 200, failing the test (with
// the child's logs) if the process dies or never comes up.
func (b *bifrost) waitReady() {
	b.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if code, _ := b.get("/healthz"); code == http.StatusOK {
			return
		}
		select {
		case err := <-b.waited:
			b.waited <- err
			b.t.Fatalf("bifrost exited before becoming ready (%v)\nlogs:\n%s", err, b.logText())
		default:
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("bifrost never answered /healthz 200\nlogs:\n%s", b.logText())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (b *bifrost) exited() bool {
	select {
	case err := <-b.waited:
		b.waited <- err
		return true
	default:
		return false
	}
}

// signal sends sig to the child process.
func (b *bifrost) signal(sig os.Signal) {
	b.t.Helper()
	if err := b.cmd.Process.Signal(sig); err != nil {
		b.t.Fatalf("signal %v: %v", sig, err)
	}
}

// waitExit waits for the process to exit and returns its exit code.
func (b *bifrost) waitExit(timeout time.Duration) int {
	b.t.Helper()
	select {
	case err := <-b.waited:
		b.waited <- err
		return exitCode(b.t, err)
	case <-time.After(timeout):
		b.t.Fatalf("bifrost did not exit within %s\nlogs:\n%s", timeout, b.logText())
		return -1
	}
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("bifrost wait: %v", err)
	return -1
}

// get/post drive the admin plane over HTTP.
func (b *bifrost) get(path string) (int, string) {
	resp, err := http.Get(b.adminURL + path)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (b *bifrost) post(path, body string) (int, string) {
	resp, err := http.Post(b.adminURL+path, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// logText returns everything the child has written to stderr so far.
func (b *bifrost) logText() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.logs.String()
}

// waitLog polls the child's stderr for substr — the event-driven way to
// wait for a reload/drain step the process reports in its own log.
func (b *bifrost) waitLog(substr string, timeout time.Duration) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	for !strings.Contains(b.logText(), substr) {
		if time.Now().After(deadline) {
			b.t.Fatalf("log never contained %q within %s\nlogs:\n%s", substr, timeout, b.logText())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// writeCertPair writes a fresh self-signed cert/key PEM pair (from
// fakesmtp.TestCert, so the same certificate can double as a CA file) and
// returns their paths plus the tls.Config that trusts it.
func writeCertPair(t *testing.T, dir, prefix string) (certPath, keyPath string, cfg *tls.Config) {
	t.Helper()
	cfg = fakesmtp.TestCert(t)
	der := cfg.Certificates[0].Certificate[0]
	key, ok := cfg.Certificates[0].PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("TestCert private key type = %T, want *ecdsa.PrivateKey", cfg.Certificates[0].PrivateKey)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal EC key: %v", err)
	}
	certPath = writeFile(t, dir, prefix+".crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	keyPath = writeFile(t, dir, prefix+".key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	return certPath, keyPath, cfg
}

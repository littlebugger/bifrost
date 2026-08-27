package admin

import (
	"context"
	"encoding/json"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/littlebugger/bifrost/internal/balance"
	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/health"
	"github.com/littlebugger/bifrost/internal/metrics"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// closedAddr returns a loopback address nothing is listening on --
// deterministic and fast (no dial-timeout wait), same trick
// internal/metrics' own integration tests use.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// testCfg is one pool, one server, with a "connect" (L0) health check
// tuned to fail fast and deterministically against a closed port: L0
// never sends SMTP bytes, so a refused TCP connect always reports the
// fixed reason "connect-refused" (probe.go), unlike L2's handshake-error
// text which embeds the OS's own (platform-dependent) error string.
func testCfg(addr string) *config.Config {
	return &config.Config{
		Pools: []config.Pool{{
			Name: "p",
			Servers: []config.Server{{
				Name: "s1", Address: addr, Weight: 3,
				Check: config.CheckParams{
					Level: "connect", Rise: 1, Fall: 1,
					Interval: 15 * time.Millisecond, DownInterval: 15 * time.Millisecond, Timeout: 500 * time.Millisecond,
				},
			}},
		}},
	}
}

// testServer wires an admin.Server over a real (fast-interval)
// health.Checker and a fresh balance.Router -- the checker's scheduler
// is not started; tests that need a live probe call runChecker
// themselves.
func testServer(t *testing.T, cfg *config.Config) (*Server, *health.Checker, *balance.Router) {
	t.Helper()
	holder := &config.Holder{}
	holder.Swap(cfg)
	checker := health.New(holder, nil, nil)
	router := balance.NewRouter(holder, checker.Eligible, rand.New(rand.NewSource(1)))
	m := metrics.New()
	return New("", holder, checker, router, m, nil), checker, router
}

// runChecker starts checker.Run in the background and returns a stop
// func that cancels it and waits for it to actually return -- goleak's
// requirement that nothing outlives the test.
func runChecker(checker *health.Checker) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { checker.Run(ctx); close(done) }()
	return func() {
		cancel()
		<-done
	}
}

// waitFor polls (bounded, real time) until cond is true.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waitFor: condition never became true")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// getJSON performs a GET against h and decodes a JSON body, if any.
func getJSON(t *testing.T, h http.Handler, path string) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	resp := rr.Result()
	var body map[string]any
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("%s: decode JSON: %v", path, err)
		}
	}
	return resp, body
}

// TestServersEndpoint: GET /servers reflects health.Checker's FSM
// (op/last_change/last_probe.detail via one real "connect"-level probe
// against a closed port), the configured weight, and balance.Router's
// in-flight count.
func TestServersEndpoint(t *testing.T) {
	addr := closedAddr(t)
	cfg := testCfg(addr)
	s, checker, router := testServer(t, cfg)

	stop := runChecker(checker)
	waitFor(t, func() bool { return checker.Status("p", "s1").LastProbe.Level != "" })
	stop()

	srv := &cfg.Pools[0].Servers[0]
	release := router.Lease(srv) // simulate one in-flight transaction
	defer release()

	resp, body := getJSON(t, s.Handler(), "/servers")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	servers, ok := body["servers"].([]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("servers = %#v, want exactly one entry", body["servers"])
	}
	row, ok := servers[0].(map[string]any)
	if !ok {
		t.Fatalf("servers[0] is not an object: %#v", servers[0])
	}

	if row["pool"] != "p" || row["server"] != "s1" {
		t.Errorf("pool/server = %v/%v, want p/s1", row["pool"], row["server"])
	}
	if row["op"] != "DOWN" {
		t.Errorf("op = %v, want DOWN (fall=1 against a closed port)", row["op"])
	}
	if row["admin"] != "READY" || row["override"] != "AUTO" {
		t.Errorf("admin/override = %v/%v, want READY/AUTO", row["admin"], row["override"])
	}
	if row["weight"] != float64(3) {
		t.Errorf("weight = %v, want 3", row["weight"])
	}
	if row["in_flight"] != float64(1) {
		t.Errorf("in_flight = %v, want 1", row["in_flight"])
	}
	if row["consec_fail"] != float64(1) {
		t.Errorf("consec_fail = %v, want 1", row["consec_fail"])
	}
	lastChange, ok := row["last_change"].(string)
	if !ok || lastChange == "" {
		t.Fatalf("last_change = %#v, want a non-empty RFC3339 string", row["last_change"])
	}
	if _, err := time.Parse(time.RFC3339, lastChange); err != nil {
		t.Errorf("last_change = %q is not RFC3339: %v", lastChange, err)
	}

	lp, ok := row["last_probe"].(map[string]any)
	if !ok {
		t.Fatalf("last_probe = %#v, want an object", row["last_probe"])
	}
	if lp["level"] != "connect" || lp["result"] != "fail" || lp["detail"] != "connect-refused" {
		t.Errorf("last_probe = %#v, want {level:connect result:fail detail:connect-refused}", lp)
	}
}

// TestStatsEndpoint: GET /stats reports the balancer totals and
// per-server counters the Prometheus registry (also /metrics' source)
// and balance.Router already track.
func TestStatsEndpoint(t *testing.T) {
	cfg := testCfg(closedAddr(t))
	s, _, router := testServer(t, cfg)

	srv := &cfg.Pools[0].Servers[0]
	release := router.Lease(srv)
	defer release()

	// Metrics is the *metrics.Metrics stored on Server; drive it
	// directly the same way internal/proxy would.
	s.m.SessionStarted()
	s.m.Transaction("p", "s1", "2xx")
	s.m.Transaction("p", "s1", "2xx")
	s.m.BackendDial("s1", true)
	s.m.BackendDial("s1", false)

	resp, body := getJSON(t, s.Handler(), "/stats")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	totals, ok := body["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals = %#v, want an object", body["totals"])
	}
	if totals["sessions_active"] != float64(1) {
		t.Errorf("sessions_active = %v, want 1", totals["sessions_active"])
	}
	if totals["sessions_total"] != float64(1) {
		t.Errorf("sessions_total = %v, want 1", totals["sessions_total"])
	}

	servers, ok := body["servers"].([]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("servers = %#v, want exactly one entry", body["servers"])
	}
	row := servers[0].(map[string]any)
	if row["in_flight"] != float64(1) {
		t.Errorf("in_flight = %v, want 1", row["in_flight"])
	}
	if row["transactions_total"] != float64(2) {
		t.Errorf("transactions_total = %v, want 2", row["transactions_total"])
	}
	if row["backend_dials_ok"] != float64(1) || row["backend_dials_fail"] != float64(1) {
		t.Errorf("backend_dials_ok/fail = %v/%v, want 1/1", row["backend_dials_ok"], row["backend_dials_fail"])
	}
}

// TestHealthzDrainAware: 200 while serving, 503 once SetDraining(true)
// flips the lame-duck flag.
func TestHealthzDrainAware(t *testing.T) {
	s, _, _ := testServer(t, testCfg(closedAddr(t)))

	resp, _ := getJSON(t, s.Handler(), "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("before drain: status = %d, want 200", resp.StatusCode)
	}

	s.SetDraining(true)
	resp2, _ := getJSON(t, s.Handler(), "/healthz")
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("after drain: status = %d, want 503", resp2.StatusCode)
	}
}

// TestMetricsServed: GET /metrics serves the Prometheus registry.
// TestMetricsServed is the golden-list contract proven live: every one
// of the 12 documented metric names actually appears in a real scrape
// of the Handler() an operator hits — not merely constructible in
// isolation (see internal/metrics/metrics_test.go's
// TestMetricNamesStable for that, narrower, claim). This is what would
// have caught ServerCollector never being registered on admin.New's own
// registry: server_up/eligible/in_flight/state_changes/probe_total are
// all only reachable through that registration.
func TestMetricsServed(t *testing.T) {
	s, checker, _ := testServer(t, testCfg(closedAddr(t)))

	// bifrost_probe_total only has anything to report once an active
	// probe has actually completed.
	stop := runChecker(checker)
	waitFor(t, func() bool { return checker.Status("p", "s1").LastProbe.Level != "" })
	stop()

	// A CounterVec with zero labeled children is absent from a scrape
	// even when correctly registered -- materialize one series on each
	// of the push-based vecs the way internal/proxy really would.
	s.m.Transaction("p", "s1", "2xx")
	s.m.SynthesizedReply("451 4.4.1")
	s.m.RelayBytes("to_client", 1)
	s.m.BackendDial("s1", true)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	body := rr.Body.String()
	want := []string{
		"bifrost_sessions_active",
		"bifrost_sessions_total",
		"bifrost_transactions_total",
		"bifrost_synthesized_replies_total",
		"bifrost_relay_bytes_total",
		"bifrost_backend_dials_total",
		"bifrost_probe_total",
		"bifrost_server_up",
		"bifrost_server_eligible",
		"bifrost_server_state_changes_total",
		"bifrost_in_flight",
		"bifrost_duplicate_risk_total",
	}
	for _, name := range want {
		if !strings.Contains(body, name) {
			t.Errorf("live /metrics scrape (curl http://<admin-bind>/metrics) missing %q", name)
		}
	}
	if t.Failed() {
		t.Logf("--- full scrape body ---\n%s", body)
	}
}

// TestUnixSocketBind: an admin { bind = "unix://..." } config binds a
// unix socket with 0600 perms, and it actually serves.
func TestUnixSocketBind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.sock")
	cfg := testCfg(closedAddr(t))
	cfg.Admin = &config.Admin{Bind: "unix://" + path}
	s, _, _ := testServer(t, cfg)

	ln, err := s.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket perms = %o, want 0600", perm)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx, ln); close(done) }()
	defer func() {
		cancel()
		<-done
	}()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", path)
		},
	}}
	resp, err := client.Get("http://unix/healthz")
	if err != nil {
		t.Fatalf("GET over unix socket: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

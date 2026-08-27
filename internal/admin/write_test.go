package admin

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/revolee/bifrost/internal/balance"
	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/health"
	"github.com/revolee/bifrost/internal/metrics"
)

// do posts body (a JSON string) to path against h and decodes the
// (always-JSON, on every write endpoint) response body.
func do(t *testing.T, h http.Handler, path, body string) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	resp := rr.Result()
	var fields map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &fields); err != nil {
		t.Fatalf("%s: response is not valid JSON: %v (body: %s)", path, err, rr.Body.String())
	}
	return resp, fields
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// loadedServer builds an admin.Server whose initial config comes from
// an actual config.Load(path) — the fixture Task 4's reload tests need,
// as distinct from testServer's hand-built *config.Config.
func loadedServer(t *testing.T, path string) (*Server, *config.Holder, *balance.Router) {
	t.Helper()
	cfg, diags := config.Load(path)
	if diags.HasErrors() {
		t.Fatalf("initial load of %s: %v", path, diags)
	}
	holder := &config.Holder{}
	holder.Swap(cfg)
	checker := health.New(holder, nil, nil)
	router := balance.NewRouter(holder, checker.Eligible, rand.New(rand.NewSource(1)))
	m := metrics.New()
	return New(path, holder, checker, router, m, nil), holder, router
}

const goodConfigHCL = `
listener {
  bind     = "127.0.0.1:2525"
  hostname = "bifrost.test"
}

pool "p" {
  balance = "roundrobin"
  server "s1" {
    address = "127.0.0.1:9"
    weight  = 1
  }
}

routing {
  default_pool = "p"
}
`

const goodConfigWithSecondPoolHCL = `
listener {
  bind     = "127.0.0.1:2525"
  hostname = "bifrost.test"
}

pool "p" {
  balance = "roundrobin"
  server "s1" {
    address = "127.0.0.1:9"
    weight  = 1
  }
}

pool "p2" {
  balance = "roundrobin"
  server "s2" {
    address = "127.0.0.1:10"
    weight  = 1
  }
}

routing {
  default_pool = "p"
}
`

// badConfigHCL keeps goodConfigHCL's shape but sets an out-of-range
// weight (Validate: 0-256) -- one clear error, file:line-anchored at the
// weight attribute.
const badConfigHCL = `
listener {
  bind     = "127.0.0.1:2525"
  hostname = "bifrost.test"
}

pool "p" {
  balance = "roundrobin"
  server "s1" {
    address = "127.0.0.1:9"
    weight  = 999
  }
}

routing {
  default_pool = "p"
}
`

// TestSetStateDrainMaintReady: POST .../state round-trips through
// health.Checker, visible immediately via Status/Eligible.
func TestSetStateDrainMaintReady(t *testing.T) {
	s, checker, _ := testServer(t, testCfg(closedAddr(t)))
	h := s.Handler()

	for _, tc := range []struct {
		body string
		want health.AdminState
	}{
		{`{"state":"drain"}`, health.AdminDrain},
		{`{"state":"maint"}`, health.AdminMaint},
		{`{"state":"ready"}`, health.AdminReady},
	} {
		resp, body := do(t, h, "/servers/p/s1/state", tc.body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %v", tc.body, resp.StatusCode, body)
		}
		if got := checker.Status("p", "s1").Admin; got != tc.want {
			t.Fatalf("%s: Status.Admin = %v, want %v", tc.body, got, tc.want)
		}
	}

	// DRAIN makes Eligible false immediately, with no probe involved.
	resp, body := do(t, h, "/servers/p/s1/state", `{"state":"drain"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if checker.Eligible("p", "s1") {
		t.Fatalf("Eligible after drain = true, want false")
	}
}

// TestSetOverride: POST .../health round-trips through
// health.Checker.SetOverride, and Eligible reflects FORCE_UP/FORCE_DOWN
// immediately.
func TestSetOverride(t *testing.T) {
	s, checker, _ := testServer(t, testCfg(closedAddr(t)))
	h := s.Handler()

	resp, body := do(t, h, "/servers/p/s1/health", `{"override":"force-down"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if got := checker.Status("p", "s1").Override; got != health.OverrideForceDown {
		t.Fatalf("Override = %v, want FORCE_DOWN", got)
	}
	if checker.Eligible("p", "s1") {
		t.Fatalf("Eligible after force-down = true, want false")
	}

	resp2, body2 := do(t, h, "/servers/p/s1/health", `{"override":"force-up"}`)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp2.StatusCode, body2)
	}
	if !checker.Eligible("p", "s1") {
		t.Fatalf("Eligible after force-up = false, want true")
	}
}

// TestSetWeightRebuildsWRR: POST .../weight is visible immediately
// through the real balance.Router path — no config reload involved.
// (That the WRR/leastconn picking math itself honors an override is
// balance's own concern: internal/balance/weight_test.go's
// TestRouterSetWeightRebuildsWRR drives real Pick() calls and asserts
// the distribution shift; this test is the admin wiring on top of it.)
func TestSetWeightRebuildsWRR(t *testing.T) {
	s, _, router := testServer(t, testCfg(closedAddr(t)))
	h := s.Handler()

	if w, ok := router.Weight("p", "s1"); !ok || w != 3 {
		t.Fatalf("Weight before override = (%d, %v), want (3, true)", w, ok)
	}

	resp, body := do(t, h, "/servers/p/s1/weight", `{"weight":0}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if w, ok := router.Weight("p", "s1"); !ok || w != 0 {
		t.Fatalf("Weight after override = (%d, %v), want (0, true)", w, ok)
	}
}

// TestReloadEndpointGood: POST /reload against a valid config returns
// 200 with a diff summary, the new pool is live in the config
// balance.Router reads on its very next Pick, and a runtime weight
// override set before the reload is discarded by it (PROJECT.md D15's
// survival matrix: admin state/force overrides survive a reload,
// runtime weight reverts to config with a logged discard list).
func TestReloadEndpointGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bifrost.hcl")
	writeFile(t, path, goodConfigHCL)

	s, holder, router := loadedServer(t, path)
	h := s.Handler()

	if resp, body := do(t, h, "/servers/p/s1/weight", `{"weight":42}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("setting a weight override before reload: status = %d, body = %v", resp.StatusCode, body)
	}

	writeFile(t, path, goodConfigWithSecondPoolHCL)
	resp, body := do(t, h, "/reload", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	diff, _ := body["diff"].(string)
	if !strings.Contains(diff, `pool "p2" added`) {
		t.Fatalf(`diff = %q, want it to mention pool "p2" added`, diff)
	}
	discarded, _ := body["discarded_weight_overrides"].([]any)
	if len(discarded) != 1 || discarded[0] != "p/s1" {
		t.Fatalf("discarded_weight_overrides = %v, want [\"p/s1\"]", body["discarded_weight_overrides"])
	}
	if w, ok := router.Weight("p", "s1"); !ok || w != 1 {
		t.Fatalf("Weight after reload = (%d, %v), want (1, true) -- back to the config's own value, override discarded", w, ok)
	}

	cfg := holder.Load()
	found := false
	for _, p := range cfg.Pools {
		if p.Name == "p2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pool p2 not present in the live config after reload")
	}
}

// TestReloadEndpointBindChange: POST /reload against a config that moves
// the listener bind is refused whole (422, "restart required"), and the
// live config is untouched — the same rule SIGHUP applies
// (cmd/bifrost/reload.go, config.BindChange): the socket is already open
// and v1 does not re-bind it (D14).
func TestReloadEndpointBindChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bifrost.hcl")
	writeFile(t, path, goodConfigHCL)

	s, holder, _ := loadedServer(t, path)
	h := s.Handler()
	before := holder.Load()

	writeFile(t, path, strings.Replace(goodConfigHCL, "127.0.0.1:2525", "127.0.0.1:2526", 1))
	resp, body := do(t, h, "/reload", "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
	}
	errs, _ := body["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one", body["errors"])
	}
	if msg, _ := errs[0].(string); !strings.Contains(msg, "restart required") {
		t.Errorf("error = %q, want it to say a restart is required", msg)
	}
	if holder.Load() != before {
		t.Fatalf("the live config changed despite a rejected reload")
	}
}

// TestReloadEndpointBad: POST /reload against a broken config returns
// 422 with file:line diagnostics, and the config still serving is
// unchanged.
func TestReloadEndpointBad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bifrost.hcl")
	writeFile(t, path, goodConfigHCL)

	s, holder, _ := loadedServer(t, path)
	h := s.Handler()
	before := holder.Load()

	writeFile(t, path, badConfigHCL)
	resp, body := do(t, h, "/reload", "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
	}
	// badConfigHCL also trips a couple of RFC-floor warnings (short
	// timeouts) and the "no admin plane" warning; only the one real
	// error (the out-of-range weight) should be reported here.
	errs, _ := body["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one diagnostic (warnings filtered out)", body["errors"])
	}
	first, _ := errs[0].(string)
	if !strings.Contains(first, path+":") {
		t.Errorf("diagnostic %q does not carry the config file:line", first)
	}
	if !strings.Contains(first, "Weight out of range") {
		t.Errorf("diagnostic %q, want it to name the weight error", first)
	}

	if holder.Load() != before {
		t.Fatalf("the live config changed despite a failed reload")
	}
}

// TestValidation404s400s: an unknown pool/server 404s, and a malformed
// body or an out-of-range enum/weight 400s, on every write endpoint.
func TestValidation404s400s(t *testing.T) {
	s, _, _ := testServer(t, testCfg(closedAddr(t)))
	h := s.Handler()

	cases := []struct {
		name string
		path string
		body string
		want int
	}{
		{"unknown server, state", "/servers/p/nope/state", `{"state":"drain"}`, http.StatusNotFound},
		{"unknown pool, state", "/servers/nope/s1/state", `{"state":"drain"}`, http.StatusNotFound},
		{"bad state enum", "/servers/p/s1/state", `{"state":"sleeping"}`, http.StatusBadRequest},
		{"malformed JSON, state", "/servers/p/s1/state", `{`, http.StatusBadRequest},
		{"unknown server, health", "/servers/p/nope/health", `{"override":"auto"}`, http.StatusNotFound},
		{"bad override enum", "/servers/p/s1/health", `{"override":"maybe"}`, http.StatusBadRequest},
		{"unknown server, weight", "/servers/p/nope/weight", `{"weight":1}`, http.StatusNotFound},
		{"weight too high", "/servers/p/s1/weight", `{"weight":257}`, http.StatusBadRequest},
		{"weight negative", "/servers/p/s1/weight", `{"weight":-1}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := do(t, h, tc.path, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d; body = %v", resp.StatusCode, tc.want, body)
			}
		})
	}
}

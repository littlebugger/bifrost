//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// This file renders the config files epic-10's tests hand to the real
// binary, plus the fake backends they point at. Everything here is
// deliberately literal HCL: these tests are about what an operator's own
// file does, so the file is what they build.

// fastTimeouts is PROJECT.md's timeout budget, shortened so a wedged path
// fails a test instead of hanging it. lame_duck/drain_timeout are short
// too: every drain test would otherwise pay the 2s+30s production budget.
const fastTimeouts = `
    client_idle        = "20s"
    session_max        = "5m"
    backend_connect    = "2s"
    backend_handshake  = "2s"
    backend_mail_reply = "5s"
    backend_354_wait   = "5s"
    data_progress      = "10s"
    backend_final_dot  = "10s"
    lame_duck          = "100ms"
    drain_timeout      = "5s"
`

// withTimeouts is fastTimeouts with the named rows replaced — the way a
// test that is about one timer (a drain deadline, a stalled client) tunes
// that timer without restating the other nine.
func withTimeouts(over map[string]string) string {
	var b strings.Builder
	b.WriteString("\n")
	for _, line := range strings.Split(strings.Trim(fastTimeouts, "\n"), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if v, ok := over[name]; ok {
			fmt.Fprintf(&b, "    %s = %q\n", name, v)
			continue
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// fastCheck probes often enough that a health transition lands inside a
// test's lifetime, with rise/fall low for the same reason.
const fastCheck = `
    level         = "ehlo"
    interval      = "200ms"
    down_interval = "200ms"
    timeout       = "150ms"
    rise          = 1
    fall          = 2
`

// fixture is a whole bifrost config file, rendered by render. Zero-valued
// fields fall back to this epic's shared defaults (fastTimeouts,
// fastCheck, PIPELINING+8BITMIME), so a test only writes the part it is
// actually about.
type fixture struct {
	smtp     string // listener bind (required)
	admin    string // admin bind (required)
	timeouts string // body of defaults.timeouts
	check    string // body of defaults.check
	caps     string // listener capabilities list literal
	starttls string // starttls block inside listener
	pools    string // pool blocks (required)
	routing  string // body of routing
	limits   string // body of limits
}

func (f fixture) render() string {
	orDefault := func(v, dflt string) string {
		if v == "" {
			return dflt
		}
		return v
	}
	return fmt.Sprintf(`
defaults {
  timeouts {%s  }
  check {%s  }
}

listener {
  bind         = %q
  hostname     = "bifrost.test"
  capabilities = %s
%s}

%s

routing {
%s}

limits {
%s}

admin {
  bind = %q
}
`,
		orDefault(f.timeouts, fastTimeouts),
		orDefault(f.check, fastCheck),
		f.smtp,
		orDefault(f.caps, `["PIPELINING", "8BITMIME"]`),
		f.starttls,
		f.pools,
		orDefault(f.routing, "  default_pool = \"p\"\n"),
		orDefault(f.limits, "  global_maxconn = 64\n"),
		f.admin,
	)
}

// writeFileAt overwrites the file at path — how a test rotates a
// certificate or edits a config in place, which is what an operator does
// before sending SIGHUP.
func writeFileAt(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// waitDialsAtLeast polls (bounded) until srv has accepted n connections —
// a healthy backend's own probe cadence used as a clock, so a test never sleeps
// to "let some probes happen".
func waitDialsAtLeast(t *testing.T, srv *fakesmtp.Server, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for srv.DialCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("backend accepted %d connections, want at least %d", srv.DialCount(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// writeFile writes text to dir/name and returns the path.
func writeFile(t *testing.T, dir, name, text string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// poolHCL renders one roundrobin pool block around already-rendered
// server blocks (serverHCL).
func poolHCL(name string, servers ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pool %q {\n  balance = \"roundrobin\"\n", name)
	for _, s := range servers {
		b.WriteString(s)
	}
	b.WriteString("}\n")
	return b.String()
}

// serverHCL is one server block: name, address, weight.
func serverHCL(name, addr string, weight int) string {
	return fmt.Sprintf("  server %q {\n    address = %q\n    weight  = %d\n  }\n", name, addr, weight)
}

// namedFake is a fake backend whose end-of-data verdict names itself, so
// a test can tell which backend answered from the client's side alone
// (dial counts also move for health probes, so they cannot).
func namedFake(t *testing.T, name string) *fakesmtp.Server {
	t.Helper()
	return fakesmtp.Start(t, fakesmtp.Script{
		Caps:  backendCaps(),
		OnEOD: []fakesmtp.Step{{Reply: "250 2.0.0 OK: queued via " + name}},
	})
}

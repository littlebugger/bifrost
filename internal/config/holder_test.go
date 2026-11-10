package config

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHolderSwapVisibleToReaders runs several reader goroutines calling
// Load a bounded number of times while a writer goroutine repeatedly
// Swaps between two distinct, fully-formed configs. Under -race this
// proves there is no data race on the swap point, and every observed
// value is exactly one of the two configs ever published — never a nil
// after the first Swap, and never a mix of the two (which atomic.Pointer's
// single-word swap makes structurally impossible, but this is the
// regression test that would catch a future change that broke that
// property).
//
// Both readers and the writer run a fixed number of iterations rather
// than spinning until a stop signal: an unbounded busy-spin reader loop
// starves the writer for CPU under the race detector's instrumentation
// (each access takes a shared lock), which can turn this test into a
// multi-minute stall instead of the sub-second check it should be.
func TestHolderSwapVisibleToReaders(t *testing.T) {
	cfgA := &Config{Defaults: Defaults{EhloName: "A"}}
	cfgB := &Config{Defaults: Defaults{EhloName: "B"}}

	var h Holder
	h.Swap(cfgA)

	const readers = 8
	const readIterations = 2000
	badCh := make(chan string, readers)
	var wg sync.WaitGroup

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < readIterations; j++ {
				cfg := h.Load()
				if cfg == nil {
					badCh <- "Load() returned nil after Swap had already run"
					return
				}
				if name := cfg.Defaults.EhloName; name != "A" && name != "B" {
					badCh <- fmt.Sprintf("observed a config that is neither published value: %+v", cfg)
					return
				}
			}
		}()
	}

	const writeIterations = 500
	for i := 0; i < writeIterations; i++ {
		if i%2 == 0 {
			h.Swap(cfgB)
		} else {
			h.Swap(cfgA)
		}
	}

	wg.Wait()
	close(badCh)
	for msg := range badCh {
		t.Error(msg)
	}
}

// TestHolderDiffSummary checks that Swap returns the previous config and
// that DiffSummary names the added pool, the removed pool, and the
// changed server between two configs.
func TestHolderDiffSummary(t *testing.T) {
	oldCfg := &Config{Pools: []Pool{
		{Name: "p1", Servers: []Server{{Name: "s1", Address: "192.0.2.1:25", Weight: 1}}},
		{Name: "p2", Servers: []Server{{Name: "s2", Address: "192.0.2.2:25", Weight: 1}}},
	}}
	newCfg := &Config{Pools: []Pool{
		{Name: "p1", Servers: []Server{{Name: "s1", Address: "192.0.2.1:25", Weight: 5}}}, // weight changed
		{Name: "p3", Servers: []Server{{Name: "s3", Address: "192.0.2.3:25", Weight: 1}}}, // p2 removed, p3 added
	}}

	var h Holder
	if first := h.Swap(oldCfg); first != nil {
		t.Fatalf("first Swap returned %+v, want nil (nothing published yet)", first)
	}
	got := h.Swap(newCfg)
	if got != oldCfg {
		t.Fatalf("Swap returned %p, want the previous config %p", got, oldCfg)
	}
	if h.Load() != newCfg {
		t.Fatalf("Load() = %p, want the newly swapped-in config %p", h.Load(), newCfg)
	}

	summary := DiffSummary(oldCfg, newCfg)
	for _, want := range []string{
		`pool "p3" added`,
		`pool "p2" removed`,
		`pool "p1": server "s1" changed`,
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("DiffSummary(old, new) = %q, want it to contain %q", summary, want)
		}
	}

	if got := DiffSummary(nil, newCfg); got != "initial config load" {
		t.Errorf("DiffSummary(nil, new) = %q, want %q", got, "initial config load")
	}
	if got := DiffSummary(oldCfg, oldCfg); got != "no changes" {
		t.Errorf("DiffSummary(x, x) = %q, want %q", got, "no changes")
	}
}

// TestRestartRequiredListenerAuth: a listener auth edit (here, a user's
// hash changing — a revoked/rotated credential) must warn "restart
// required", since a Session captures Listener.Auth at accept and a
// reload alone leaves already-open and newly-accepted sessions on the old
// store. Identical auth must warn about nothing, and the warning text
// must never contain the hash itself.
func TestRestartRequiredListenerAuth(t *testing.T) {
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	authWith := func(hash string) *ListenerAuth {
		return &ListenerAuth{Users: []AuthUser{{Name: "alice", Salt: "salt", HashedPassword: hash}}}
	}

	oldCfg := &Config{Listeners: []Listener{{Auth: authWith(hashA)}}}
	changedCfg := &Config{Listeners: []Listener{{Auth: authWith(hashB)}}}

	warnings := RestartRequired(oldCfg, changedCfg)
	var authWarning string
	for _, w := range warnings {
		if strings.Contains(w, "listener auth changed") {
			authWarning = w
		}
	}
	if authWarning == "" {
		t.Fatalf("RestartRequired(old, changed) = %v, want a listener auth warning", warnings)
	}
	if strings.Contains(authWarning, hashA) || strings.Contains(authWarning, hashB) {
		t.Errorf("warning %q echoes credential material", authWarning)
	}

	for _, w := range RestartRequired(oldCfg, oldCfg) {
		if strings.Contains(w, "listener auth changed") {
			t.Errorf("RestartRequired(old, old) = %v, want no listener auth warning for identical auth", w)
		}
	}
}

// TestRestartRequiredListenerAuthCleartext: flipping allow_cleartext alone
// (same users, same hash) must still warn "restart required" — a Session
// captures Listener.Auth, AllowCleartext included, at accept, so a reload
// that only tightens allow_cleartext OFF would otherwise print "reloaded"
// while every already-open (and newly-accepted, pre-restart) session keeps
// accepting plaintext AUTH. Identical auth, AllowCleartext included, must
// warn about nothing.
func TestRestartRequiredListenerAuthCleartext(t *testing.T) {
	authWith := func(allowCleartext bool) *ListenerAuth {
		return &ListenerAuth{
			Users:          []AuthUser{{Name: "alice", Salt: "salt", HashedPassword: strings.Repeat("a", 64)}},
			AllowCleartext: allowCleartext,
		}
	}

	oldCfg := &Config{Listeners: []Listener{{Auth: authWith(true)}}}
	changedCfg := &Config{Listeners: []Listener{{Auth: authWith(false)}}}

	var authWarning string
	for _, w := range RestartRequired(oldCfg, changedCfg) {
		if strings.Contains(w, "listener auth changed") {
			authWarning = w
		}
	}
	if authWarning == "" {
		t.Fatalf("RestartRequired(old, changed) = %v, want a listener auth warning for an AllowCleartext-only change",
			RestartRequired(oldCfg, changedCfg))
	}

	identicalCfg := &Config{Listeners: []Listener{{Auth: authWith(true)}}}
	for _, w := range RestartRequired(oldCfg, identicalCfg) {
		if strings.Contains(w, "listener auth changed") {
			t.Errorf("RestartRequired(old, identical) = %v, want no listener auth warning for identical auth", w)
		}
	}
}

// TestRestartRequiredTemplateSetChanged: internal/discover.Runner starts
// exactly one goroutine per server_dns template at process start and can
// neither start nor stop one afterward (ruling after Task 3's review), so
// a reload that adds (or removes) a server_dns block must warn restart
// required — the same precedent as a listener-identity edit above — and
// an identical template set between two configs must warn about nothing.
// Editing a surviving template's address or resolve_interval warns the
// same way: Run's goroutine captured the old ServerDNS by value at
// startup, so neither edit takes effect without a restart either
// (final-fix-brief.md item 1).
func TestRestartRequiredTemplateSetChanged(t *testing.T) {
	const marker = "server_dns template set or settings changed"

	withoutTemplate := &Config{Pools: []Pool{
		{Name: "routes", Servers: []Server{{Name: "static-extra", Address: "10.9.8.7:587", Weight: 1}}},
	}}
	withTemplate := &Config{Pools: []Pool{
		{
			Name:    "routes",
			Servers: []Server{{Name: "static-extra", Address: "10.9.8.7:587", Weight: 1}},
			DNSTemplates: []ServerDNS{
				{Name: "node", Address: "svc.local:587", ResolveInterval: 30 * time.Second, Weight: 1},
			},
		},
	}}

	if w := RestartRequired(withoutTemplate, withTemplate); !hasWarning(w, marker) {
		t.Fatalf("RestartRequired(without, with) = %v, want a server_dns template set warning", w)
	}
	if w := RestartRequired(withTemplate, withTemplate); hasWarning(w, marker) {
		t.Errorf("RestartRequired(with, with) = %v, want no warning for an identical set", w)
	}

	addressChanged := &Config{Pools: []Pool{
		{
			Name:    "routes",
			Servers: []Server{{Name: "static-extra", Address: "10.9.8.7:587", Weight: 1}},
			DNSTemplates: []ServerDNS{
				{Name: "node", Address: "other.local:587", ResolveInterval: 30 * time.Second, Weight: 1},
			},
		},
	}}
	if w := RestartRequired(withTemplate, addressChanged); !hasWarning(w, marker) {
		t.Errorf("RestartRequired(with, address-changed) = %v, want a warning: same key, new address", w)
	}

	intervalChanged := &Config{Pools: []Pool{
		{
			Name:    "routes",
			Servers: []Server{{Name: "static-extra", Address: "10.9.8.7:587", Weight: 1}},
			DNSTemplates: []ServerDNS{
				{Name: "node", Address: "svc.local:587", ResolveInterval: 60 * time.Second, Weight: 1},
			},
		},
	}}
	if w := RestartRequired(withTemplate, intervalChanged); !hasWarning(w, marker) {
		t.Errorf("RestartRequired(with, interval-changed) = %v, want a warning: same key, new resolve_interval", w)
	}
}

// hasWarning reports whether any warning in warnings contains substr.
func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// TestRestartRequiredSingleListenerWordingUnchanged pins the exact
// unqualified wording a single-listener config has always produced:
// test/integration/reload_test.go greps the running process's log for the
// literal substring "listener hostname changed", so keying RestartRequired
// by listener name internally must not start quoting a name nobody wrote
// in the file for the common one-listener case.
func TestRestartRequiredSingleListenerWordingUnchanged(t *testing.T) {
	oldCfg := &Config{Listeners: []Listener{{Name: "default", Hostname: "bifrost.test", Capabilities: []string{"PIPELINING"}}}}
	newCfg := &Config{Listeners: []Listener{{Name: "default", Hostname: "renamed.test", Capabilities: []string{"PIPELINING", "8BITMIME"}}}}

	warnings := RestartRequired(oldCfg, newCfg)
	if !hasWarning(warnings, "listener hostname changed") {
		t.Errorf("RestartRequired = %v, want the unqualified %q", warnings, "listener hostname changed")
	}
	if !hasWarning(warnings, "listener capabilities changed") {
		t.Errorf("RestartRequired = %v, want the unqualified %q", warnings, "listener capabilities changed")
	}
	for _, w := range warnings {
		if strings.Contains(w, `"default"`) {
			t.Errorf("warning %q names the listener even though it is the only one configured", w)
		}
	}
}

// TestRestartRequiredPerListenerName: with two listeners configured
// ("default" and "bulk"), an edit to "bulk"'s hostname, capabilities,
// auth, or starttls must warn BY NAME, and "default" — untouched — must
// stay silent: the comparison is keyed per listener, not by position 0
// alone (PrimaryListener's Task-1 transitional reads). Identical
// two-listener configs must warn about nothing.
func TestRestartRequiredPerListenerName(t *testing.T) {
	base := func() *Config {
		return &Config{Listeners: []Listener{
			{Name: "default", Bind: "127.0.0.1:2525", Hostname: "bifrost.test", Capabilities: []string{"PIPELINING"}},
			{
				Name: "bulk", Bind: "127.0.0.1:2526", Hostname: "bulk.test", Capabilities: []string{"PIPELINING"},
				Auth:     &ListenerAuth{Users: []AuthUser{{Name: "alice", Salt: "salt", HashedPassword: strings.Repeat("a", 64)}}},
				StartTLS: &StartTLS{Cert: "bulk.crt", Key: "bulk.key", MinVersion: "1.2"},
			},
		}}
	}

	tests := []struct {
		name   string
		modify func(*Config)
		want   string
	}{
		{"hostname", func(c *Config) { c.Listeners[1].Hostname = "renamed.test" }, `listener "bulk" hostname changed`},
		{"capabilities", func(c *Config) { c.Listeners[1].Capabilities = []string{"PIPELINING", "8BITMIME"} }, `listener "bulk" capabilities changed`},
		{"auth", func(c *Config) { c.Listeners[1].Auth.Users[0].HashedPassword = strings.Repeat("b", 64) }, `listener "bulk" auth changed`},
		{"starttls cert", func(c *Config) { c.Listeners[1].StartTLS.Cert = "other.crt" }, `listener "bulk" starttls changed`},
		{"starttls min_version", func(c *Config) { c.Listeners[1].StartTLS.MinVersion = "1.3" }, `listener "bulk" starttls min_version changed (1.2 -> 1.3)`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldCfg := base()
			newCfg := base()
			tc.modify(newCfg)

			warnings := RestartRequired(oldCfg, newCfg)
			if !hasWarning(warnings, tc.want) {
				t.Fatalf("RestartRequired = %v, want a warning containing %q", warnings, tc.want)
			}
			for _, w := range warnings {
				if strings.Contains(w, `"default"`) {
					t.Errorf("warning %q names the untouched listener %q, want only \"bulk\" named", w, "default")
				}
			}
		})
	}

	if w := RestartRequired(base(), base()); len(w) != 0 {
		t.Errorf("RestartRequired on identical two-listener configs = %v, want none", w)
	}
}

// TestRestartRequiredListenerAddedOrRemoved: adding or removing a whole
// listener block needs a restart to open (or stop accepting on) its
// socket — same precedent as every other listener-identity edit — so it
// warns by name rather than silently vanishing from the diff.
func TestRestartRequiredListenerAddedOrRemoved(t *testing.T) {
	oneListener := &Config{Listeners: []Listener{{Name: "default", Bind: "127.0.0.1:2525"}}}
	twoListeners := &Config{Listeners: []Listener{
		{Name: "default", Bind: "127.0.0.1:2525"},
		{Name: "bulk", Bind: "127.0.0.1:2526"},
	}}

	if w := RestartRequired(oneListener, twoListeners); !hasWarning(w, `listener "bulk" added`) {
		t.Errorf("RestartRequired(one, +bulk) = %v, want a warning that listener \"bulk\" was added", w)
	}
	if w := RestartRequired(twoListeners, oneListener); !hasWarning(w, `listener "bulk" removed`) {
		t.Errorf("RestartRequired(two, -bulk) = %v, want a warning that listener \"bulk\" was removed", w)
	}
}

// TestBindChangeNamesTheSurvivingListener: a bind edit on a listener name
// present in both old and newCfg is still the one change a reload cannot
// apply (D14) — unchanged from the single-listener rule — except the
// rejection now names which listener moved.
func TestBindChangeNamesTheSurvivingListener(t *testing.T) {
	oldCfg := &Config{Listeners: []Listener{
		{Name: "default", Bind: "127.0.0.1:2525"},
		{Name: "bulk", Bind: "127.0.0.1:2526"},
	}}
	newCfg := &Config{Listeners: []Listener{
		{Name: "default", Bind: "127.0.0.1:2525"},
		{Name: "bulk", Bind: "127.0.0.1:2527"},
	}}

	err := BindChange(oldCfg, newCfg)
	if err == nil {
		t.Fatalf("BindChange(old, new) = nil, want a rejection naming listener %q", "bulk")
	}
	if !strings.Contains(err.Error(), `"bulk"`) || !strings.Contains(err.Error(), "restart required") {
		t.Errorf("BindChange error = %q, want it to name listener \"bulk\" and say a restart is required", err.Error())
	}
}

// TestBindChangeListenerAddedOrRemovedIsNotARejection: a listener added
// or removed has no surviving counterpart bind to compare — unlike a
// moved bind on a name both sides share, that is a RestartRequired
// warning (a new or departed socket needs a restart to open or to stop
// accepting on), never the BindChange rejection.
func TestBindChangeListenerAddedOrRemovedIsNotARejection(t *testing.T) {
	oneListener := &Config{Listeners: []Listener{{Name: "default", Bind: "127.0.0.1:2525"}}}
	twoListeners := &Config{Listeners: []Listener{
		{Name: "default", Bind: "127.0.0.1:2525"},
		{Name: "bulk", Bind: "127.0.0.1:2526"},
	}}

	if err := BindChange(oneListener, twoListeners); err != nil {
		t.Errorf("BindChange(one, +bulk) = %v, want nil (an added listener is a RestartRequired warning, not a bind rejection)", err)
	}
	if err := BindChange(twoListeners, oneListener); err != nil {
		t.Errorf("BindChange(two, -bulk) = %v, want nil (a removed listener is a RestartRequired warning, not a bind rejection)", err)
	}
}

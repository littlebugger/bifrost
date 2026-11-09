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

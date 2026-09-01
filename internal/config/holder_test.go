package config

import (
	"fmt"
	"strings"
	"sync"
	"testing"
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

	oldCfg := &Config{Listener: Listener{Auth: authWith(hashA)}}
	changedCfg := &Config{Listener: Listener{Auth: authWith(hashB)}}

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

package metrics

import (
	"context"
	"math/rand"
	"net"
	"sort"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/balance"
	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/health"
)

// closedAddr returns a loopback address nothing is listening on —
// deterministic and fast (no dial-timeout wait): listen, then close.
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

// oneServerConfig is the minimal loaded config a ServerCollector needs
// to have something to walk: one pool, one server, with a fast
// "connect"-level check (L0, no SMTP bytes) so a real probe against the
// closed addr completes well within this test's bounded wait.
func oneServerConfig(addr string) *config.Config {
	return &config.Config{
		Pools: []config.Pool{{Name: "p", Servers: []config.Server{{
			Name: "s", Address: addr, Weight: 1,
			Check: config.CheckParams{
				Level: "connect", Rise: 1, Fall: 1,
				Interval: 15 * time.Millisecond, DownInterval: 15 * time.Millisecond, Timeout: time.Second,
			},
		}}}},
	}
}

// waitForProbe polls (bounded) until (pool,server) has completed at
// least one active probe.
func waitForProbe(t *testing.T, c *health.Checker, pool, server string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for c.Status(pool, server).LastProbe.Level == "" {
		if time.Now().After(deadline) {
			t.Fatalf("no probe completed for (%s,%s) in time", pool, server)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestMetricNamesStable is the golden-list contract test: a real
// registry — the push-based Metrics and the pull-based ServerCollector
// registered together on it, exactly the way internal/admin.New wires
// them for a live /metrics scrape — Gather()s exactly the twelve stable
// names PROJECT.md's Produces block documents, no more, no fewer.
//
// Registering ServerCollector here (rather than only checking its
// Describe() output in isolation, as an earlier version of this test
// did) also means a Desc collision between it and Metrics' own vecs —
// two collectors on one registry describing the same fqName — fails
// this test loudly via MustRegister, instead of silently only ever
// being exercised by internal/admin's own suite.
func TestMetricNamesStable(t *testing.T) {
	m := New()
	m.transactions.WithLabelValues("p", "s", "2xx")
	m.synthReplies.WithLabelValues("451 4.4.1")
	m.relayBytes.WithLabelValues("to_client")
	m.backendDials.WithLabelValues("s", "ok")

	holder := &config.Holder{}
	holder.Swap(oneServerConfig(closedAddr(t)))
	checker := health.New(holder, nil, nil)
	router := balance.NewRouter(holder, checker.Eligible, rand.New(rand.NewSource(1)))
	m.Registry.MustRegister(NewServerCollector(holder, checker, router))

	// bifrost_probe_total only has anything to report once an active
	// probe has actually completed.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { checker.Run(ctx); close(done) }()
	waitForProbe(t, checker, "p", "s")
	cancel()
	<-done

	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]bool{}
	for _, mf := range mfs {
		got[mf.GetName()] = true
	}

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
		if !got[name] {
			t.Errorf("missing metric %q", name)
		}
	}

	var gotNames []string
	for name := range got {
		gotNames = append(gotNames, name)
	}
	if len(got) != len(want) {
		sort.Strings(gotNames)
		sort.Strings(want)
		t.Errorf("registry exposes %v, want exactly %v", gotNames, want)
	}
}

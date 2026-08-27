//go:build integration

package metrics

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/fakesmtp"
	"github.com/revolee/bifrost/internal/proxy"
	"github.com/revolee/bifrost/internal/smtpdrv"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// txnTestConfig builds a one-pool, one-server config pointed at
// backendAddr, with short timeouts so a wedged backend fails the test
// instead of hanging it (mirrors internal/proxy/relay_test.go's
// relayTimeouts).
func txnTestConfig(backendAddr string) *config.Config {
	return &config.Config{
		Listener: config.Listener{Hostname: "bifrost.test", Capabilities: []string{"PIPELINING", "8BITMIME"}},
		Defaults: config.Defaults{Timeouts: config.Timeouts{
			ClientIdle: 10 * time.Second, SessionMax: time.Minute,
			BackendConnect: 2 * time.Second, BackendHandshake: 2 * time.Second,
			BackendMailReply: 2 * time.Second, Backend354Wait: 2 * time.Second,
			DataProgress: 5 * time.Second, BackendFinalDot: 5 * time.Second,
		}},
		Pools: []config.Pool{{
			Name: "p", Balance: "roundrobin", BackendTLS: "none", EhloName: "bifrost.test",
			Servers: []config.Server{{Name: "s1", Address: backendAddr, Weight: 1}},
		}},
		Routing: config.Routing{DefaultPool: "p"},
	}
}

// startBifrost wires a real proxy.Serve+proxy.Relay over cfg (whose one
// server is always the PickFunc's only candidate — no internal/balance
// needed for this package's own tests) with m as the Metrics sink, and
// returns the listener address. Everything is torn down via t.Cleanup.
func startBifrost(t *testing.T, cfg *config.Config, m *Metrics) string {
	t.Helper()
	holder := &config.Holder{}
	holder.Swap(cfg)
	srv := &cfg.Pools[0].Servers[0]
	pick := func(proxy.TxnMeta) ([]*config.Server, error) { return []*config.Server{srv}, nil }
	relay := proxy.NewRelay(pick, holder, nil, nil, nil, m)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = proxy.Serve(ctx, ln, cfg, nil, relay, nil, m)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Serve did not return after cancel")
		}
	})
	return ln.Addr().String()
}

// closedPort returns an address nothing is listening on, deterministically
// and without waiting out a dial timeout: listen, then immediately close.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestTransactionCountersMove: one relayed message end to end moves
// bifrost_transactions_total{verdict_class="2xx"} and both directions of
// bifrost_relay_bytes_total.
func TestTransactionCountersMove(t *testing.T) {
	backend := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING", "8BITMIME"}})
	cfg := txnTestConfig(backend.Addr())
	m := New()
	addr := startBifrost(t, cfg, m)

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.test")
	c.Expect("250")
	reply := c.SendMsg(0)
	if reply.Code != "250" {
		t.Fatalf("end-of-data reply = %+v, want 250", reply)
	}
	c.Send("QUIT")
	c.Expect("221")

	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if got := CounterValue(mfs, "bifrost_transactions_total", map[string]string{"pool": "p", "server": "s1", "verdict_class": "2xx"}); got < 1 {
		t.Errorf("bifrost_transactions_total{verdict_class=2xx} = %v, want >= 1", got)
	}
	if got := CounterValue(mfs, "bifrost_relay_bytes_total", map[string]string{"direction": "to_backend"}); got <= 0 {
		t.Errorf("bifrost_relay_bytes_total{direction=to_backend} = %v, want > 0", got)
	}
	if got := CounterValue(mfs, "bifrost_relay_bytes_total", map[string]string{"direction": "to_client"}); got <= 0 {
		t.Errorf("bifrost_relay_bytes_total{direction=to_client} = %v, want > 0", got)
	}
}

// TestSynthesizedReplyCounter: a MAIL FROM against a pool whose only
// server is unreachable synthesizes the contract's 451 4.4.1
// (RplNoBackend) and bumps synthesized_replies_total for it.
func TestSynthesizedReplyCounter(t *testing.T) {
	cfg := txnTestConfig(closedPort(t))
	m := New()
	addr := startBifrost(t, cfg, m)

	c := smtpdrv.Dial(t, addr)
	c.Expect("220")
	c.Send("EHLO client.test")
	c.Expect("250")
	c.Send("MAIL FROM:<sender@test.example>")
	reply := c.Expect("451")
	if len(reply.Lines) != 1 || reply.Lines[0] != "451 4.4.1 No backend available, try again later" {
		t.Fatalf("reply = %v, want the RplNoBackend text", reply.Lines)
	}

	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if got := CounterValue(mfs, "bifrost_synthesized_replies_total", map[string]string{"code_enhanced": "451 4.4.1"}); got < 1 {
		t.Errorf("bifrost_synthesized_replies_total{code_enhanced=\"451 4.4.1\"} = %v, want >= 1", got)
	}
}

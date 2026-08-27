//go:build integration

package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/revolee/bifrost/internal/balance"
	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/fakesmtp"
	"github.com/revolee/bifrost/internal/health"
	"github.com/revolee/bifrost/internal/metrics"
	"github.com/revolee/bifrost/internal/proxy"
	"github.com/revolee/bifrost/internal/smtpdrv"
)

// TestServersEndpointUnderTraffic is the concurrent-read race proof:
// real mail traffic flows through a real relay+backend while several
// goroutines hammer GET /servers and GET /stats in a tight loop, all
// under -race. Every response must be 200 with a parseable JSON body —
// no assertion on the JSON's content, this test is about admin's read
// path never racing balance.Router/health.Checker's writes, not about
// the FSM (covered by internal/health and this package's own
// TestServersEndpoint).
func TestServersEndpointUnderTraffic(t *testing.T) {
	backend := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING", "8BITMIME"}})
	cfg := &config.Config{
		Listener: config.Listener{Hostname: "bifrost.test", Capabilities: []string{"PIPELINING", "8BITMIME"}},
		Defaults: config.Defaults{Timeouts: config.Timeouts{
			ClientIdle: 10 * time.Second, SessionMax: time.Minute,
			BackendConnect: 2 * time.Second, BackendHandshake: 2 * time.Second,
			BackendMailReply: 2 * time.Second, Backend354Wait: 2 * time.Second,
			DataProgress: 5 * time.Second, BackendFinalDot: 5 * time.Second,
		}},
		Pools: []config.Pool{{
			Name: "p", Balance: "roundrobin", BackendTLS: "none", EhloName: "bifrost.test",
			Servers: []config.Server{{
				Name: "s1", Address: backend.Addr(), Weight: 1,
				Check: config.CheckParams{Level: "connect", Rise: 1, Fall: 1, Interval: time.Second, DownInterval: time.Second, Timeout: time.Second},
			}},
		}},
		Routing: config.Routing{DefaultPool: "p"},
	}

	holder := &config.Holder{}
	holder.Swap(cfg)
	checker := health.New(holder, nil, nil)
	router := balance.NewRouter(holder, checker.Eligible, rand.New(rand.NewSource(1)))
	m := metrics.New()
	adminSrv := New("", holder, checker, router, m, nil)

	checkerStop := runChecker(checker)
	defer checkerStop()

	quiet := slog.New(slog.DiscardHandler)
	relay := proxy.NewRelay(router.Pick, holder, quiet, checker, router.Lease, m)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	go func() {
		_ = proxy.Serve(ctx, ln, cfg, nil, relay, quiet, m)
		close(serveDone)
	}()
	defer func() {
		cancel()
		_ = ln.Close()
		<-serveDone
	}()

	stop := make(chan struct{})
	errs := make(chan string, 64)
	var wg sync.WaitGroup

	const readers = 4
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, path := range []string{"/servers", "/stats"} {
					req := httptest.NewRequest(http.MethodGet, path, nil)
					rr := httptest.NewRecorder()
					adminSrv.Handler().ServeHTTP(rr, req)
					if rr.Code != http.StatusOK {
						send(errs, path+": status "+rr.Result().Status)
						continue
					}
					var body map[string]any
					if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
						send(errs, path+": invalid JSON: "+err.Error())
					}
				}
			}
		}()
	}

	// Drive real traffic sequentially on this goroutine (smtpdrv's
	// default failure reporter is t.Fatalf, which the testing package
	// only allows from the goroutine running the test) for long enough
	// that the reader goroutines above race against real Lease/Release
	// and passive health-signal writes.
	deadline := time.Now().Add(400 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		c := smtpdrv.Dial(t, ln.Addr().String())
		c.Expect("220")
		c.Send("EHLO client.test")
		c.Expect("250")
		c.SendMsg(i)
		c.Send("QUIT")
		c.Expect("221")
	}

	close(stop)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func send(errs chan string, msg string) {
	select {
	case errs <- msg:
	default:
	}
}

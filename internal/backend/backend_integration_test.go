//go:build integration

package backend

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

// TestMain wraps the whole package's test run in goleak: every test in
// internal/backend, not just TestHundredConcurrentDials below, must
// leave no goroutine behind once its cleanup has run.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestHundredConcurrentDials is the load axis PROJECT.md's module table
// calls for: 100 goroutines dial one fake concurrently. Every Dial must
// either succeed or fail with one of this package's typed errors — no
// panic, no untyped surprise — and, per TestMain above, none of them may
// leak a goroutine (the handshake's ctx-watcher goroutine in particular).
func TestHundredConcurrentDials(t *testing.T) {
	const n = 100
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING", "8BITMIME"}})

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := Dial(context.Background(), testServer(srv.Addr()), Opts{
				EhloName: fmt.Sprintf("client%d.example", i),
				TLSMode:  "none",
				Timeouts: testTimeouts(),
			})
			errs[i] = err
			if err == nil {
				c.Quit()
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			continue
		}
		var dialErr *DialError
		var handshakeErr *HandshakeError
		var incompatErr *IncompatibleError
		if !errors.As(err, &dialErr) && !errors.As(err, &handshakeErr) && !errors.As(err, &incompatErr) {
			t.Errorf("dial %d: err = %v (%T), want one of the typed errors or nil", i, err, err)
		}
	}
	if got := srv.DialCount(); got != n {
		t.Errorf("DialCount() = %d, want %d", got, n)
	}
}

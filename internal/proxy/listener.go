package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/littlebugger/bifrost/internal/config"
)

// This file is the accept loop: cmd/bifrost's entry point for the one
// configured listener (v1 supports exactly one). It owns the two things
// neither Session nor Relay can see for themselves: the connection-scoped
// global_maxconn gate (the Transparency Contract's "Accept overload" row)
// and resilience against a misbehaving Accept — an EMFILE storm degrades
// the accept rate, it never exits the loop.

// acceptBackoffFloor and acceptBackoffCeil bound the delay between retries
// after an Accept error that is not a clean shutdown: doubling from
// 5 ms, capped at 1 s, and reset the moment Accept succeeds again.
// Mirrors the long-standing net/http Server.Serve pattern.
const (
	acceptBackoffFloor = 5 * time.Millisecond
	acceptBackoffCeil  = 1 * time.Second
)

// Serve runs the accept loop over ln until ctx is canceled or ln is
// closed by anything else — both are a clean shutdown, never an error.
// Every admitted connection becomes a Session; cfg, tlsCfg, h, and lg
// are forwarded to NewSession unchanged, per connection.
//
// global_maxconn (cfg.Limits.GlobalMaxConn; 0 or less means unlimited)
// is enforced here, before a Session exists at all: a connection over
// the cap is accepted and immediately answered with the contract's
// 421 4.3.2 straight on the raw connection, then closed — the one reply
// in this package written outside a Session or a Txn, because at this
// point neither exists yet.
//
// Serve owns the WaitGroup for every session (and rejection) goroutine
// it starts and does not return until all of them have: epic 10's drain
// cancels ctx and then waits on Serve's own return as the signal that
// nothing is left in flight.
func Serve(ctx context.Context, ln net.Listener, cfg *config.Config, tlsCfg *tls.Config, h TxnHandler, lg *slog.Logger, metrics ...Metrics) error {
	if lg == nil {
		lg = slog.Default()
	}
	m := firstMetrics(metrics)

	// Closing ln is what unblocks a call parked in Accept; stop lets this
	// watcher exit on every other return path too; otherwise it would
	// leak, blocked forever on a ctx that will now never fire.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-stop:
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	var active atomic.Int64
	maxconn := int64(cfg.Limits.GlobalMaxConn)

	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			if backoff == 0 {
				backoff = acceptBackoffFloor
			} else {
				backoff *= 2
			}
			if backoff > acceptBackoffCeil {
				backoff = acceptBackoffCeil
			}
			lg.Warn("accept error, backing off", "error", err, "backoff", backoff)

			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil
			}
			continue
		}
		backoff = 0

		if n := active.Add(1); maxconn > 0 && n > maxconn {
			active.Add(-1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				rejectOverload(conn, lg)
			}()
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer active.Add(-1)
			m.SessionStarted()
			defer m.SessionEnded()
			s := NewSession(conn, cfg, tlsCfg, h, lg)
			if err := s.Run(ctx); err != nil {
				lg.Warn("session ended with error", "error", err)
			}
		}()
	}
}

// rejectOverload answers the contract's accept-overload row directly on
// the raw connection — no Session exists for this one — and closes it.
// closeGrace (session.go's own goodbye-write bound) caps the write
// against a client that never reads.
func rejectOverload(conn net.Conn, lg *slog.Logger) {
	defer func() { _ = conn.Close() }()
	if err := conn.SetWriteDeadline(time.Now().Add(closeGrace)); err != nil {
		return
	}
	if _, err := conn.Write([]byte(RplAcceptOverload)); err != nil {
		lg.Warn("accept-overload reply failed", "error", err)
	}
}

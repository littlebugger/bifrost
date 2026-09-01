package proxy

import (
	"context"
	"crypto/tls"
	"errors"

	"github.com/littlebugger/bifrost/internal/backend"
	"github.com/littlebugger/bifrost/internal/config"
)

// This file is the backend leg's lifecycle: walking the candidates,
// dialing one, and letting it go again. All of it is per transaction —
// decision D4 is a fresh connection per message, so there is no pool to
// keep consistent and nothing to leak from one transaction to the next.

// attachAttempts is the per-candidate connect budget from PROJECT.md's
// timeout table: each candidate gets two attempts (each bounded by
// Timeouts.BackendConnect and Timeouts.BackendHandshake) before the walk
// moves on.
const attachAttempts = 2

// attachAndRelay walks the ordered candidates until one of them answers
// the batch, and answers the batch itself when none can.
//
// The walk is silent — the client is never told a candidate was tried and
// dropped — which is only sound while the client has seen nothing about
// this batch. That is the invariant the writer's dirty flag enforces: one
// relayed byte (or one synthesized reply) about these commands, and the
// batch can never be replayed anywhere else.
func (t *txn) attachAndRelay(ctx context.Context, lines [][]byte) {
	// legErr is the last failure of a backend that did answer for a
	// while: it, not "no backend available", is what the client is told
	// when the walk runs out (a reply timeout is 451 4.4.2, a pure dial
	// failure 451 4.4.1).
	var legErr error

	// sawSaturated is set when a lease is denied mid-walk (see the loop
	// below): Pick's own snapshot said this candidate was under cap, but
	// the dial-and-handshake gap let another transaction win the race
	// for its last slot. That is saturation discovered too late for
	// pickErr to carry it, and the walk's final reply must not call it
	// "no backend available" just because ErrAllSaturated itself wasn't
	// the error this particular call happened to get.
	sawSaturated := false

	candidates, pickErr := t.candidates(lines[0])
	for _, srv := range candidates {
		pool := poolFor(t.cfg, srv)
		if pool == nil {
			// A candidate whose pool cannot be identified has no known
			// backend_tls mode, and dialing it anyway could mean sending
			// mail in the clear that the operator configured to be
			// encrypted. Skip it (a reload that removed the server is the
			// realistic cause, and then skipping is also correct).
			t.r.lg.Warn("candidate is not in the loaded configuration; skipping",
				"server", srv.Name, "addr", srv.Address)
			continue
		}
		for attempt := 0; attempt < attachAttempts; attempt++ {
			c, err := backend.Dial(ctx, srv, t.dialOpts(pool))
			t.r.metrics.BackendDial(srv.Name, err == nil)
			t.record.failoverAttempts++
			if err != nil {
				t.r.sig.DialFailure(srv)
				t.r.lg.Warn("backend attach failed", "server", srv.Name,
					"addr", srv.Address, "attempt", attempt+1, "error", err)
				var incompatible *backend.IncompatibleError
				if errors.As(err, &incompatible) {
					// A capability set is not going to change between two
					// attempts milliseconds apart: retrying it only doubles
					// the noise on a backend that cannot serve us.
					break
				}
				continue
			}

			release := t.r.lease(srv)
			if release == nil {
				// Lost the race for srv's last max_transactions slot
				// between Pick's snapshot and this dial completing
				// (LeaseFunc's own doc, epic 08): nothing was ever sent
				// on this leg, so it is discarded exactly like a dial
				// failure onto a full server, and the walk moves to the
				// next candidate.
				sawSaturated = true
				c.Abort()
				break
			}

			t.srv, t.c, t.release = srv, c, release
			t.r.trackLeg(c)
			t.record.pool, t.record.server = pool.Name, srv.Name
			t.cw.reset()
			left, err := t.relayBatch(lines)
			if err == nil {
				return // the batch is answered, whatever the verdicts were
			}
			legErr = err
			if t.cw.dirty() {
				t.failLeg(err, left)
				return
			}
			t.dropLeg(err)
			break // nothing reached the client: replay the batch elsewhere
		}
	}

	if legErr != nil {
		t.latchFatal(legErr, lines)
		return
	}
	if errors.Is(pickErr, ErrAllSaturated) || sawSaturated {
		// Every eligible backend exists and is healthy — it is simply
		// full, whether Pick saw that itself or a lease denial surfaced
		// it mid-walk. Transaction-scoped, not connection-scoped: the
		// contract's 451 4.3.2, distinct from the unhealthy-empty 451
		// 4.4.1 below.
		t.latchWith(RplAllBusy, lines)
		return
	}
	t.latchWith(RplNoBackend, lines)
}

// candidates asks the router for this transaction's ordered candidate
// list. mailLine is the batch's opening MAIL line (attachAndRelay is
// only ever entered with one at lines[0] — a fresh transaction's own
// opening MAIL, or a later one that cleared the fatal latch), the source
// of the routing key's MailFromDomain.
//
// The error it returns is only ever inspected for ErrAllSaturated — any
// other PickFunc error (e.g. an unresolvable pool) gets the same
// unhealthy-empty treatment as a plain empty result, exactly as before
// this method started reporting it at all.
func (t *txn) candidates(mailLine []byte) ([]*config.Server, error) {
	if t.cfg == nil {
		t.r.lg.Error("no configuration loaded; refusing transaction")
		return nil, nil
	}
	candidates, err := t.r.pick(t.meta(mailLine))
	if err != nil || len(candidates) == 0 {
		t.r.lg.Warn("no backend candidates for transaction", "client", t.tx.ClientIP, "error", err)
	}
	return candidates, err
}

// meta is the routing key the PickFunc sees.
func (t *txn) meta(mailLine []byte) TxnMeta {
	return TxnMeta{
		ClientIP:       t.tx.ClientIP,
		Helo:           t.tx.Helo,
		MailFromDomain: mailFromDomain(mailLine),
	}
}

// dialOpts builds the backend-leg options from the candidate's pool: its
// EHLO name and TLS mode, plus the listener's advertised capability set as
// the superset requirement the handshake enforces.
func (t *txn) dialOpts(pool *config.Pool) backend.Opts {
	opts := backend.Opts{
		EhloName:     pool.EhloName,
		TLSMode:      pool.BackendTLS,
		RequiredCaps: t.cfg.Listener.Capabilities,
		Timeouts:     t.timeouts(),
	}
	if opts.EhloName == "" {
		opts.EhloName = t.cfg.Listener.Hostname
	}
	if pool.Auth != nil {
		opts.AuthUsername, opts.AuthPassword = pool.Auth.Username, pool.Auth.Password
	}
	if pool.BackendTLSServerName != "" || pool.CAPool != nil {
		// The pool's backend_tls_ca, parsed once per config load
		// (config.resolveBackendCAs), reaches the handshake here as
		// RootCAs: without it a starttls-verify pool with a private CA
		// could only ever fail closed. Whether the certificate is verified
		// at all is still opts.TLSMode's decision, not this config's
		// (backend.startTLS sets InsecureSkipVerify for plain "starttls").
		opts.TLSConfig = &tls.Config{
			ServerName: pool.BackendTLSServerName,
			RootCAs:    pool.CAPool,
			MinVersion: tls.VersionTLS12,
		}
	}
	return opts
}

// poolFor finds the pool a picked server belongs to — the source of the
// EHLO name and TLS mode for its leg. PickFunc hands back pointers into
// the live config, so identity is the primary key; the address is a
// fallback, because a config reload between the pick and the dial leaves
// the caller holding pointers into the superseded config.
//
// The fallback is only taken when exactly one pool holds that address.
// The same server may legally appear in two pools with different
// backend_tls modes, and guessing between them could send mail in the
// clear that the operator configured to be encrypted; an ambiguous
// address is reported as no pool at all, which skips the candidate.
//
// ponytail: linear scan, once per attach. A pool/server map would be an
// index to invalidate on every reload for tens of servers' worth of work.
func poolFor(cfg *config.Config, srv *config.Server) *config.Pool {
	var byAddr *config.Pool
	pools := 0
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		for j := range pool.Servers {
			s := &pool.Servers[j]
			if s == srv {
				return pool
			}
			if s.Address == srv.Address && byAddr != pool {
				byAddr, pools = pool, pools+1
			}
		}
	}
	if pools != 1 {
		return nil
	}
	return byAddr
}

// trackLeg and untrackLeg maintain the Relay's registry of attached
// backend legs (see Relay.legs). They bracket exactly the window in which
// a leg belongs to a live transaction.
func (r *Relay) trackLeg(c *backend.Conn) {
	r.legsMu.Lock()
	defer r.legsMu.Unlock()
	if r.legs == nil {
		r.legs = make(map[*backend.Conn]struct{})
	}
	r.legs[c] = struct{}{}
}

func (r *Relay) untrackLeg(c *backend.Conn) {
	r.legsMu.Lock()
	defer r.legsMu.Unlock()
	delete(r.legs, c)
}

// CloseLegs aborts every backend leg currently attached to a live
// transaction and returns how many it aborted. It is the drain force
// deadline's first step (PROJECT.md's "backend legs closed FIRST"), and
// the reason that ordering is normative: an abort is a bare disconnect,
// never a terminator a backend could mistake for a completed message, so
// a message still streaming is cleanly abandoned instead of
// half-delivered — and each session then answers its own client per the
// contract (451 for the abandoned transaction, then the session's own
// 421 4.3.0) rather than vanishing mid-verdict.
//
// It is safe to call from any goroutine: net.Conn allows a concurrent
// Close, and the session goroutine that owns the leg observes it as the
// read/write failure it is.
func (r *Relay) CloseLegs() int {
	r.legsMu.Lock()
	legs := make([]*backend.Conn, 0, len(r.legs))
	for c := range r.legs {
		legs = append(legs, c)
	}
	r.legsMu.Unlock()

	for _, c := range legs {
		c.Abort()
	}
	return len(legs)
}

// dropLeg retires a backend leg that failed mid-transaction: a passive
// health signal, and a hard close. The contract never lets a session keep
// talking to a backend that failed inside a transaction.
func (t *txn) dropLeg(err error) {
	t.lastSrv = t.srv // detach() below clears srv; countTxn needs it after
	t.r.sig.TransportError(t.srv)
	t.r.lg.Warn("backend leg failed mid-transaction",
		"server", srvName(t.srv), "client", t.tx.ClientIP, "error", err)
	t.broken = true
	t.detach(false)
}

// srvName is the attached server's name for logging, empty-safe.
func srvName(srv *config.Server) string {
	if srv == nil {
		return ""
	}
	return srv.Name
}

// detach ends the attachment: the backend leg is released and the lease
// with it. clean means the leg is at a command boundary with nothing
// pending, so it gets a polite QUIT; anything else is a hard abort (RFC
// 5321 3.8 — never a terminator a backend could mistake for a completed
// message).
func (t *txn) detach(clean bool) {
	if t.c == nil {
		return
	}
	t.r.untrackLeg(t.c)
	if clean {
		t.c.Quit()
	} else {
		t.c.Abort()
	}
	if !t.broken {
		t.r.sig.Success(t.srv)
	}
	if t.release != nil {
		t.release()
	}
	t.c, t.srv, t.release = nil, nil, nil
}

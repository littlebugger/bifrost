package proxy

import (
	"context"
	"crypto/tls"
	"errors"

	"github.com/littlebugger/bifrost/internal/backend"
	"github.com/littlebugger/bifrost/internal/config"
)

// This file is the backend leg's lifecycle: walking the candidates,
// dialing one, and (usually) letting it go again. Decision D4 keeps
// fresh-per-transaction as the default: a dialed conn answers exactly
// one transaction and is closed, so there is no pool to keep consistent
// and nothing to leak from one transaction to the next.
//
// Above the default, a pool's reuse_envelopes cap turns "letting it go"
// into "handing it back": a conn that finished its envelope cleanly is
// stashed onto backendAffinity, one reuse slot owned by the Session and
// shared by every Txn that runs on it — never shared across sessions,
// never holding more than one conn. Two sites touch the slot: stash
// (below) fills it instead of closing the leg at the two clean detach
// points (a delivered DATA verdict, a relayed RSET); tryReuse drains it
// at the top of attachAndRelay, revalidating the cached conn with RSET
// before handing it back into the walk exactly like a fresh dial. The
// cap and the revalidation are what keep this from becoming the
// connection pool D4 rejected: a conn is never reused past
// reuse_envelopes envelopes, and never reused without first proving
// itself alive.

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

	done, saturated, err := t.tryReuse(candidates, lines)
	if done {
		return
	}
	legErr, sawSaturated = err, saturated

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

			// A fresh dial is always this conn's first envelope; tryReuse
			// above sets a higher ordinal for a reused one.
			done, err := t.attachLeg(srv, c, release, pool, 1, lines)
			if done {
				return
			}
			legErr = err
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

// attachLeg finishes attaching a leg — dialed fresh by the walk above, or
// a cached one tryReuse just consumed — and relays the batch over it:
// the bookkeeping every successful attach shares, and the dirty/dropLeg
// split every failure shares, so a reused leg that fails mid-batch is
// treated as exactly the ordinary leg failure it is (dropLeg's own
// detach(false) already clears t.c, so a broken reused leg is never
// re-stashed).
//
// ordinal is this attach's connEnvelope ordinal: 1 for a fresh dial,
// tryReuse's own running count for a reuse.
//
// It reports whether attachAndRelay is done: true means the batch was
// answered however it went (success, or a dirty failure already
// latched); false means nothing reached the client and the caller may
// still try another candidate, with err the failure to report if
// nothing else attaches either.
func (t *txn) attachLeg(srv *config.Server, c *backend.Conn, release func(), pool *config.Pool, ordinal int, lines [][]byte) (done bool, err error) {
	t.srv, t.c, t.release = srv, c, release
	t.r.trackLeg(c)
	t.record.pool, t.record.server = pool.Name, srv.Name
	t.record.connEnvelope = ordinal
	t.cw.reset()

	left, err := t.relayBatch(lines)
	if err == nil {
		return true, nil // the batch is answered, whatever the verdicts were
	}
	if t.cw.dirty() {
		t.failLeg(err, left)
		return true, err
	}
	t.dropLeg(err)
	return false, err
}

// tryReuse is attachAndRelay's first move: pick up the session's cached
// leg instead of dialing, when the top candidate is still that leg's own
// server and the live pool still allows another envelope on it. See the
// design doc's Mechanics §Reuse for the contract this implements.
//
// done and err mirror attachLeg's own shape — false, _, nil means nothing
// about the cache applied or it was not fit for reuse, and
// attachAndRelay's normal walk should proceed untouched. saturated is set
// when the lease (not the conn) was the obstacle: the walk's own final
// reply must call that RplAllBusy, not RplNoBackend, if nothing else
// attaches either.
func (t *txn) tryReuse(candidates []*config.Server, lines [][]byte) (done bool, saturated bool, err error) {
	a := t.tx.affinity
	if a == nil || a.c == nil {
		return false, false, nil
	}
	if len(candidates) == 0 || candidates[0] != a.srv {
		// poolFor's own pointer-identity rule: the router moved on to a
		// different server (a reload, a weight/round-robin pick), so this
		// leg is no longer the one to reuse. It is left in the cache
		// exactly as is — stash()'s own closeIfAny cleans up a stale
		// entry like this the next time this session stashes.
		return false, false, nil
	}
	pool := poolFor(t.cfg, a.srv)
	if pool == nil || pool.ReuseEnvelopes <= 1 || a.envelopes >= pool.ReuseEnvelopes {
		return false, false, nil
	}

	if !t.revalidate(a.c) {
		t.r.lg.Debug("cached backend leg failed RSET revalidation; falling back to a fresh dial",
			"server", a.srv.Name)
		a.closeIfAny() // Abort + clear the slot; no health signal (see closeIfAny's own doc)
		return false, false, nil
	}

	release := t.r.lease(a.srv)
	if release == nil {
		// Lost the race for the pool's last max_transactions slot. The
		// spec prefers the cache retained over discarded here: leave it
		// exactly as is for a later envelope, once whatever is holding
		// the slot lets go.
		return false, true, nil
	}

	c, srv := a.c, a.srv
	ordinal := a.envelopes + 1
	// Binding contract from Task 3's review: clear the slot's fields now,
	// at the moment the conn is consumed — not merely read them. attachLeg
	// below may run straight into a clean stash of this same leg, and
	// stash()'s own closeIfAny would otherwise Abort the very connection
	// this envelope just attached.
	a.c, a.srv, a.envelopes = nil, nil, 0

	t.r.metrics.BackendReuse(srv.Name, "reused")
	done, err = t.attachLeg(srv, c, release, pool, ordinal, lines)
	return done, false, err
}

// revalidate sends RSET on a cached leg and reads its whole reply —
// multiline-safe, relayReply's own technique — without relaying a byte
// of it to the client: the client never knew this leg existed before
// this envelope, so a stale conn's failure here must be exactly as
// invisible as one dying between envelopes always is. It reports
// whether the leg answered 2xx and is fit for another envelope.
func (t *txn) revalidate(c *backend.Conn) bool {
	c.SetCommandClass(backend.MailRcpt)
	if err := c.SendLine([]byte("RSET\r\n")); err != nil {
		return false
	}
	rr := c.Replies()
	for {
		_, code, final, err := rr.Next()
		if err != nil {
			return false
		}
		if final {
			return code/100 == 2
		}
	}
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

// backendAffinity is a session's one reuse slot: a backend leg a prior
// transaction on the session finished cleanly and handed back instead of
// closing, held at a command boundary for the session's next envelope to
// pick back up (Task 4 reads it; this task only ever writes it). One
// slot, one session, and every access happens on the session goroutine —
// the same goroutine that runs every transaction on it — so it needs no
// lock of its own.
type backendAffinity struct {
	c         *backend.Conn
	srv       *config.Server // pointer identity is the reuse key, poolFor's own rule
	envelopes int            // envelopes this conn has carried so far
}

// closeIfAny closes a stashed conn, if there is one, and clears the slot.
// Nil-safe: a session that never stashed anything pays nothing for this.
//
// It is always an Abort, never a Quit: RFC 5321 3.8 governs a mid-message
// disconnect, and a stashed conn is never that — it is only ever stashed
// at a command boundary (a clean DATA verdict or a relayed RSET), so a
// bare close is exactly as safe here as at any other command-boundary
// teardown, and there is no backend left to wait out a polite QUIT
// round-trip for.
func (a *backendAffinity) closeIfAny() {
	if a.c == nil {
		return
	}
	a.c.Abort()
	a.c, a.srv, a.envelopes = nil, nil, 0
}

// detachOrStash is the clean-detach decision at the two sites where a leg
// finished its envelope with nothing pending (the delivered DATA verdict,
// a relayed RSET): stash it onto the session's affinity slot instead of
// closing it, when the live pool still allows another envelope on this
// conn. Every other case — no conn, a broken leg, reuse disabled or
// unresolvable, or the cap reached — falls back to today's plain
// detach(true), the cap additionally counting a "capped" reuse event.
func (t *txn) detachOrStash() {
	pool := poolFor(t.cfg, t.srv)
	if t.c == nil || t.broken || pool == nil || pool.ReuseEnvelopes <= 1 {
		t.detach(true)
		return
	}
	k := t.record.connEnvelope
	if k < pool.ReuseEnvelopes {
		t.stash()
		return
	}
	if k == pool.ReuseEnvelopes {
		t.r.metrics.BackendReuse(srvName(t.srv), "capped")
	}
	t.detach(true)
}

// stash disowns the leg from this transaction and hands it to the
// session's affinity slot instead of closing it: untracked from the
// Relay's leg registry, its lease released, and a Success signal exactly
// as a clean detach reports (the leg behaved, whatever this envelope's
// verdict was) — but the connection itself lives on for the session's
// next envelope instead of being QUIT. t.c/srv/release are cleared same
// as detach, so HandleTransaction's deferred detach(false) no-ops after
// this runs.
//
// closeIfAny first: a slot tryReuse already drained before this envelope
// attached is empty, so that call is then a no-op; a slot still holding
// an unrelated leg (a mismatched-candidate case tryReuse left untouched,
// or a session that never reused at all) gets closed here instead of
// leaked.
//
// The nil check on t.tx.affinity is defensive, not reachable today: every
// real Txn carries one (session.go); it only guards a hand-built Txn in a
// test that also configures a reusable pool, the same defensive parity
// detach() already has by never needing an affinity slot at all.
// detach(true) does the same clean close that would have run had reuse
// never applied to this leg.
func (t *txn) stash() {
	if t.tx.affinity == nil {
		t.detach(true)
		return
	}
	t.r.untrackLeg(t.c)
	t.r.sig.Success(t.srv)
	if t.release != nil {
		t.release()
	}
	t.tx.affinity.closeIfAny()
	t.tx.affinity.c = t.c
	t.tx.affinity.srv = t.srv
	t.tx.affinity.envelopes = t.record.connEnvelope
	t.c, t.srv, t.release = nil, nil, nil
}

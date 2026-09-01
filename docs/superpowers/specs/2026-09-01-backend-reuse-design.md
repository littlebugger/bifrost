# Session-Affine Backend Connection Reuse — Design

Approved in chat 2026-09-01. User goal: destination-MTA connections get
dropped after N sent envelopes so they stay fresh — which first requires
reuse to exist (today every envelope dials, uses, and closes its own
backend connection; decision D4).

## Goal

Opt-in, per-pool: a backend connection that finishes an envelope cleanly
is kept on the client session and reused for that session's next envelope
when the balancer picks the same server again — up to `reuse_envelopes`
envelopes per connection, then closed and re-dialed fresh. Skipping the
per-envelope TCP+EHLO+STARTTLS+AUTH handshake is the payoff; the cap is
the freshness guarantee.

## Non-goals

- Cross-session connection pooling, idle reapers, per-destination keyed
  pools (kumo/bounce-master style). Single-backend pools get nearly all
  the win from session affinity alone.
- Any change to per-transaction balancing (R3): every MAIL still gets a
  fresh pick; reuse happens only when the pick lands on the server we
  already hold.
- Reuse across client sessions: the cache lives and dies with one client
  session.

## Config

```hcl
pool "outgoing" {
  reuse_envelopes = 50   # 0 or 1 (or omitted) = today's fresh-per-envelope
}
```

One knob is both the enabler and the cap: N > 1 enables reuse bounded at
N envelopes per backend connection. Negative → validation error. No
defaults-tier inheritance (pool attribute only).

## Mechanics

**Cache.** One slot per client session (`backendAffinity`: conn, server
pointer, envelope count), owned by the Session, handed to each Txn. The
session's Run defer closes any cached conn on session end (drain
included; a cached conn is idle at a command boundary, so a plain Abort
is safe — it is never mid-message).

**Stash (instead of close).** At the two clean end-of-envelope detach
sites — the DATA-verdict detach (`data.go` `t.detach(body.delivered)`
when delivered) and the relayed-RSET detach (`relay.go reset()`) — when
the live pool has `reuse_envelopes > 1`, the leg is unbroken, and the
connection has carried fewer than N envelopes: release the lease, signal
Success, untrack the leg, and stash the conn + server + count instead of
QUITing. At the cap: close as today and count a `capped` metric event.
The EHLO/HELO/QUIT hand-back path keeps today's polite close (QUIT must
close; EHLO/HELO take the conservative path).

**Reuse (instead of dial).** In `attachAndRelay`, before the candidate
walk: if the cache holds a conn AND the pick's first candidate is the
same `*config.Server` (pointer identity — the same rule poolFor relies
on; a reload naturally invalidates the cache) AND the live pool still has
`reuse_envelopes > 1` AND count < N: revalidate with RSET (MailRcpt
class; any error or non-2xx → close the cached conn silently and fall
through to the normal walk — staleness is expected, no health signal),
acquire the lease (denied → return conn to cache, mark saturated, normal
walk), then attach the cached conn exactly like a dialed one (trackLeg,
record fields, envelope count++, `reused` metric event). AUTH state
survives on the conn — that is the point.

**Observability.** Metrics: one counter with an outcome label,
`bifrost_backend_conn_reuse_total{server, outcome}` where outcome ∈
`reused` | `capped`. Transaction log gains `conn_envelope=<k>` (1 =
fresh connection, k > 1 = k-th envelope on a reused one).

**Interactions.**
- Leases: every envelope takes and releases a lease as today; a cached
  idle conn holds no lease and is invisible to leastconn/in-flight
  accounting (documented).
- Health: probes unchanged (always fresh dials). A cached conn that died
  is discovered by the RSET revalidation and replaced silently.
- Drain: cached conns are not in the Relay legs map (not mid-message);
  session teardown closes them.
- max_transactions: unchanged — leases gate concurrency, reuse gates
  connection lifetime.

## Testing

Unit (fakesmtp): stash keeps the fake's session open across envelopes
(DialCount stays 1, RSET between envelopes on the wire); cap closes and
re-dials (DialCount 2, capped counted); server mismatch and dead cached
conn fall back to fresh dials silently; lease denial retains the cache;
session end closes the cached conn. Integration: full chain with pool
auth — AUTH appears once per connection, not once per envelope; kill the
fake between envelopes and the next envelope transparently re-dials;
reuse_envelopes=0 regression pins today's behavior.

## Docs

PROJECT.md: amend decision D4 (fresh-per-transaction stays the default;
session-affine reuse is the documented opt-in) and the attach.go header
comment that cites it; note the leastconn/idle-conn caveat. operations.md:
knob semantics, the no-idle-reaper ceiling (backend idle timeouts
self-heal via RSET revalidation), metrics. examples/bifrost.hcl: commented
`reuse_envelopes` line.

## Amendment (2026-09-01, user request): allow_cleartext on both auth legs

`allow_cleartext = true` (default false) is accepted inside BOTH auth
blocks, for links secured at the network layer (in-cluster k8s):

- `pool.auth`: lifts the three backend-leg guards for that pool — the
  "pool auth requires backend TLS" validation error, the "pool auth
  requires TLS probes" validation error, and backend.Dial's refusal to
  send AUTH PLAIN on a non-TLS-upgraded connection.
- `listener.auth`: lifts the "client auth requires starttls" validation
  error; AUTH PLAIN is then advertised and accepted on plaintext
  sessions (the 538 gate applies only when the knob is off).

The default stays strict: backend_tls defaults to "none", so a silent
allow would leak credentials on a forgotten backend_tls line. The knob
must appear in the config file where an auditor will see it.

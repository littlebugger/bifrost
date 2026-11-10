# Multiple Listeners in One Process — Design

User request 2026-11-09: one bifrost process serving two (or more) ports,
each stream routed to its own destination pool — the upstream MTA splits
traffic by port, bifrost must honor the split without running two
instances.

## Goal

N `listener` blocks in one config/process, each with its own bind,
hostname, STARTTLS, auth store, and advertised capability set; routing
rules gain a `listener` match key so a transaction's arrival port can
select its pool. Lifts v1's "exactly one listener" restriction
(PROJECT.md's named post-v1 mode).

## Non-goals

- Per-listener pools/limits ownership: pools, routing, limits, admin,
  and defaults stay config-global. `global_maxconn` stays global.
- Live listener add/remove on reload (bind-set changes stay
  restart-required, as single-listener bind changes are today).
- Computed live capability intersection (the other post-v1 mode named in
  PROJECT.md) — the advertised set stays statically configured per
  listener.

## Config

```hcl
listener "transactional" {
  bind         = "0.0.0.0:2525"
  hostname     = "tx.mail.example.com"
  capabilities = ["PIPELINING", "8BITMIME"]
  auth { ... }                       # per-listener user store
}

listener "bulk" {
  bind     = "0.0.0.0:2526"
  hostname = "bulk.mail.example.com"
  starttls { ... }                   # per-listener TLS identity
}

routing {
  rule { listener = ["transactional"] pool = "stream-a" }
  rule { listener = ["bulk"]          pool = "stream-b" }
  default_pool = "stream-a"
}
```

**Backward compatibility:** exactly one UNLABELED block stays valid and
gets the implicit name `"default"` — every existing config loads
unchanged. Labels are required the moment there is more than one block.
Validation: unique labels, unique binds, `rule.listener` entries must
name declared listeners; the SMTPUTF8/8BITMIME pairing, auth/starttls,
and capability rules apply per listener.

`rule.listener` is a list, AND-combined with the existing keys
(client_cidr, mail_from_domain); a rule without it matches any listener.
First match wins, unchanged.

## Mechanics

**Serve.** One accept loop per listener (today's loop, multiplied); each
Session carries its originating Listener (replacing every
`cfg.Listener` read in the session path: banner/EHLO hostname,
capability filtering, STARTTLS config, auth store, the 530 gate).
Drain covers all listeners together, as one process-level event.

**Routing.** TxnMeta gains the listener name; the router matches
`rule.listener` against it.

**Backend capability referent — the real design decision, settled as:**

- *Per transaction:* `RequiredCaps` = the ARRIVING listener's capability
  set (dialOpts is already per-transaction; the Txn's listener provides
  the set). A backend serving a transaction is checked against exactly
  the promises made to that client. SIZE/STARTTLS carve-outs unchanged.
- *Health prober:* probes are per-server, not per-transaction, so they
  need one referent: the UNION of all listeners' capability sets, with
  SIZE compared at the maximum required value. Conservative by design —
  a backend must be able to serve any listener that could route to it.
  (A per-pool `required_capabilities` override is the named escape hatch
  if union proves too strict for a deployment; deferred until someone
  hits it.)

**Reload.** RestartRequired compares per listener by name: hostname,
capabilities, auth (including allow_cleartext), and the bind set
(added/removed/changed binds → restart-required warning). Pool/routing
edits stay reload-live.

**Observability.** The transaction log gains `listener=<name>`; session
and transaction metrics gain a `listener` label where a label already
exists for pool (no new metric families). Admin `/servers` is unchanged
(servers are pool-scoped); `/stats` gains per-listener session counts.

## Testing

Unit: config decode/validation matrix (unlabeled-single compat, dup
labels/binds, unknown rule.listener); session uses its own listener's
hostname/caps/auth (two listeners with different stores — a user valid
on A must fail on B); router listener-key matching incl. AND with
existing keys. Integration: two listeners, two fakes, one process —
port-split routing proven end to end; per-transaction RequiredCaps
proven by giving the two listeners different capability sets and a
backend satisfying only one (transactions from the other listener get
the no-backend synth); prober union proven by a backend missing a cap
only listener B advertises → incompatible.

## Docs

PROJECT.md: replace the v1 single-listener statements (goals R1 note,
capability policy's "single referent", decision table) with the
per-transaction/union referent model; operations.md: multi-listener
section + routing key; examples/bifrost.hcl: stays single-listener
(fixture), with a commented second-listener sketch.

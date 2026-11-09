# DNS-Discovered Pool Members ("headless" destinations) — Design

User request 2026-11-09: one DNS name whose A/AAAA answer is N servers
(k8s headless service, DNS-RR farm) usable as a pool destination, with
real per-member balancing, health, and failover — not the single-entry
degradation that pointing `server.address` at a multi-A name gives today
(one health status for N machines, no SMTP-level failover across IPs,
resolver-ordered "balancing"). This is PROJECT.md's skipped-in-v1 "DNS
service discovery", scoped to the HAProxy server-template essence.

## Goal

```hcl
pool "routes" {
  balance = "leastconn"

  server_dns "node" {
    address          = "mta.headless.svc.cluster.local:587"
    resolve_interval = "30s"   # default; min 1s
    # weight / backup / max_transactions / check { } — inherited by
    # every discovered member, exactly like a static server's fields
  }

  server "static-extra" { address = "10.9.8.7:587" }  # mixing allowed
}
```

A `server_dns` template expands at runtime into concrete members named
`node-<ip>`, each a full pool citizen: own health FSM and probes, own
lease/in-flight accounting, own metrics labels, normal candidate in the
failover walk, normal reuse-affinity target.

## Non-goals

- SRV records (port always from the template address), TTL-driven
  refresh (Go's resolver does not expose TTLs — fixed interval instead;
  documented), per-member weight (uniform from the template), DNS-based
  listener binds, cross-pool dedup of identical names.

## Mechanics — discovery is an internal reload

The architecture treats config as immutable snapshots behind
`config.Holder`; every consumer (relay per transaction, health checker
per swap — scheduler.go already re-syncs when `cfgH.Load()` identity
changes, admin, metrics) re-reads it. Discovery rides exactly that:

- One resolver goroutine per `server_dns` template (started by the same
  runtime that owns the health checker): `LookupIP` on
  `resolve_interval` with ±10% jitter.
- On a MEMBERSHIP DELTA only (set of IPs changed vs the current
  snapshot): build a derived Config — the template expanded into
  concrete `Server` entries — and `holder.Swap` it. No delta, no swap:
  a stable farm costs zero churn.
- **Survivor identity is NAME-based, not pointer-based** (ruling
  2026-11-09, after verifying internal/health: the checker keys
  serverEntry/serverHealth by svrKey and persists them across holder
  swaps; its *Server pointer map is a secondary index rebuilt per
  sync). ExpandDNS therefore does a plain deterministic rebuild on
  every delta — fresh Server objects — and survivor continuity comes
  from stable member NAMES (`<template>-<ip>`): health-FSM state,
  probe counters, and admin overrides all persist by key. The one
  thing fresh pointers cost is session reuse-affinity to that pool for
  that single transition — the cached conn fails the pointer match and
  the session silently re-dials, the feature's own documented fallback.
  Added members enter as Unknown → `rise` probes, the normal FSM path.
  Removed members vanish from the next snapshot: in-flight
  transactions finish on their leases, cached conns die on mismatch.
- SIGHUP interplay: an operator reload re-renders templates from the
  resolver's last-known answer (the resolver owns membership; the file
  owns everything else). The derived-snapshot builder is the single
  place both paths meet.

**Failure policy — fail-static (HAProxy "hold" semantics):** a
resolution error or an EMPTY answer keeps the last-known-good
membership and logs/counts the failure. DNS blips must never empty a
pool. First-ever resolution failing at startup: the pool starts with
the template unexpanded (no members) and a loud log; the resolver keeps
trying.

## Config & validation

`server_dns` is a labeled block beside `server`, sharing its attribute
set minus none; `address` must be host:port with a non-IP host (an IP
literal → validation error pointing at plain `server`);
`resolve_interval >= 1s`; expanded-member cap 256 per template
(validation cannot see runtime count — enforced by the resolver,
excess logged and truncated deterministically by sorted IP). A pool of
only `server_dns` templates is valid even though it may be empty at
runtime (the existing empty-pool validation applies to declared blocks,
not runtime membership).

## Observability

Metrics: `bifrost_dns_discovery_total{template, result=ok|error|empty}`,
`bifrost_dns_discovery_changes_total{template}`,
`bifrost_pool_discovered_members{pool, template}` gauge. Member-level
metrics use the `node-<ip>` server name, exactly like static servers.
Admin `/servers` rows for discovered members carry
`"discovered": true` and `"template": "node"`. Log line on every
membership change naming added/removed IPs.

## Testing

Unit: template decode/validation (IP-literal rejection, interval floor,
mixing with static servers); derived-snapshot builder (survivor pointer
reuse, add/remove, deterministic member naming/truncation); fail-static
on error and on empty answer. Resolver injected as a func for tests
(no real DNS in unit tests). Integration: fake resolver flipping
between {A}, {A,B}, {B} — balancing spreads over both when present,
probes attach per member, transactions in flight on a removed member
finish, affinity conn to a removed member re-dials silently, empty
answer changes nothing. Reuse + discovery: survivor keeps its cached
conn across a delta that only adds a member.

## Docs

PROJECT.md: move "DNS service discovery" from the skipped list to a
documented mode with the fail-static policy; operations.md: section
with the k8s headless example, the no-TTL/interval constraint, and the
member-naming scheme; examples/bifrost.hcl: commented `server_dns`
sketch.

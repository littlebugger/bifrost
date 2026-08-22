# SMTP Balancer — Open-Source Options Research

*Researched 2026-08-18. Verified against current docs and source code (Exim spec, Haraka plugin docs, Mireka source).*

## Requirements

1. **Custom balancing rules** like HAProxy (algorithms, ACL-style routing).
2. **Liveness checks** for destination service availability.
3. **Cross-connect balancing** — one long-lived client connection is still balanced per message across available destinations.
4. **Transparency** — no status termination: server responses relayed verbatim to the client immediately, and vice versa.

## Verdict: no open-source project fits all four 100%

The killer is the combination of **#3 + #4**:

- Balancing individual messages inside one long-lived connection forces the balancer to *speak SMTP* — it must own the banner/EHLO exchange (no single backend exists yet at greeting time). Every L4/TCP balancer fails this: they pin a connection to one backend for its lifetime.
- Full status transparency rules out every store-and-forward MTA: they answer "250 queued" themselves and turn backend verdicts into async bounces.

What's needed is a **cut-through, transaction-aware proxy with a health-checked backend pool** — that exact combination doesn't exist as a finished OSS product. GitHub sweeps confirm: nothing beyond HAProxy configs, client-side sending pools, and generic TCP balancers.

**Physical limit:** even a "100%" solution is transparent *per transaction* (MAIL/RCPT/DATA statuses relayed verbatim both ways), not per session. Banner/EHLO are necessarily the balancer's own; it must advertise the intersection of backend capabilities and terminate STARTTLS on each leg. Source-IP transparency to backends is a separate concern, solvable via XCLIENT or PROXY protocol.

## Comparison

| Solution | 1. Custom LB rules | 2. Health checks | 3. Per-message balancing in one connection | 4. Status pass-through |
|---|---|---|---|---|
| **HAProxy** (tcp mode) | ✅ full (ACLs, algos) | ✅ active, SMTP-aware (`option smtpchk`, `tcp-check` scripts) | ❌ pinned per connection | ✅ byte-transparent |
| **nginx stream** / LVS+keepalived / Pen / gobetween / Envoy·Traefik TCP | ✅ decent | mixed (nginx OSS = passive only; keepalived has active `SMTP_CHECK`) | ❌ per connection | ✅ |
| **nginx mail module** | ✅ you write the `auth_http` chooser service | DIY inside that service | ❌ backend picked once per connection | ⚠️ handshake terminated by nginx, then piped |
| **Postfix / Exim(default) / OpenSMTPD / KumoMTA / maddy / Stalwart** (store-and-forward relay to a pool) | ✅ rich routing | passive (retry/dead-host logic) | ✅ per message | ❌ terminates: client gets "250 queued", rejections become bounces |
| **Exim + `cutthrough_delivery`** | ⚠️ routers are rule-rich, but LB = random/failover (`hosts_randomize`; weights only by duplicating hosts, no least-conn) | ❌ passive only (retry DB) | ✅ per message | ✅ backend MAIL/RCPT/end-of-DATA responses relayed; `defer=pass` keeps temp-fails transparent |
| **Haraka + `queue/smtp_proxy`** | ✅ arbitrary JS plugin logic | ❌ none built-in | ✅ shape is right: connects at MAIL FROM per transaction, pooled | ✅ MAIL/RCPT/DATA relayed |
| **Mireka** (Java) | ⚠️ per-recipient routing; weighted `Upstream` exists but **not wired into proxy mode** | ❌ | ✅ backend connected at first RCPT per transaction | ✅ backend rejections relayed in-session |

## The cut-through candidates in detail

The only class of software that can satisfy #3 and #4 together. Three exist:

### Exim with `control = cutthrough_delivery` — most complete as-shipped (~3.5/4)

Verified against the spec:

- Backend connection opens at first RCPT; MAIL/RCPT/end-of-DATA responses pass to the client in real time.
- Each message routes independently → one long client connection spreads across hosts (`hosts_randomize`).

Caveats:

- Incompatible with CHUNKING/PRDR — don't advertise BDAT to clients.
- Incompatible with transport filters and content-modifying scans.
- All recipients of one message must route to the same host.
- Default `defer=spool` silently falls back to queueing on temp failure — **set `defer=pass`** to stay transparent.

Loses on: no active health checks (passive retry DB only), no HAProxy-grade algorithms (random/failover; weights only by duplicating host entries; no least-conn).

### Haraka `queue/smtp_proxy` — smallest distance to 100%

Verified in the docs:

- Exactly the right architecture: per-transaction backend checkout from a connection pool at MAIL FROM time, real-time relay of MAIL/RCPT/DATA responses.
- **But** config takes one `host=` — no multi-backend pool, no probes.
- Sibling `queue/smtp_forward` does per-domain routes but connects only at queue time, so RCPT answers are generated locally (breaks strict #4).

Haraka is a plugin framework in Node.js: extending `smtp_proxy` with a host pool + active liveness probes is a contained, well-supported patch. Rules become plain JS — more expressive than HAProxy ACLs.

### Mireka — proof the pattern works, but dormant

Verified in source (`RelayDestination.java`, `Upstream.java`):

- `RelayDestination` relays each transaction step in real time (design copied from Google's dead Baton proxy); backend connected at first RCPT per transaction; backend rejections relayed in-session.
- A weighted+backup `Upstream` class exists — but only the store-and-forward path uses it; **proxy mode takes a single backend**.
- No active health checks. Java/SubEthaSMTP, ~44 stars, essentially dormant (latest commits are Java 11 compatibility). Not a production bet.

## Dismissed

- **maddy** synchronous mode — multiple targets are failover-only, not balanced.
- **decke/smtprelay** — synchronous but failover-only, minimal rules.
- **ASSP / proxsmtp / smtpprox** — filtering proxies, single backend.
- **Envoy** — no usable SMTP filter; TCP proxy only, per-connection.
- **aiosmtpd Proxy / go-smtp** — libraries/building blocks, not products.
- **F5 / Halon / PowerMTA** — commercial, out of scope.

## Realistic paths, in order

1. **Exim + cutthrough + `defer=pass`** — pure config, hits 3 of 4 fully. Fastest to production if passive health checking and random/weighted-by-duplication balancing are acceptable.
2. **Haraka + one custom plugin** (pool + active liveness probes grafted onto `smtp_proxy`) — hits all four. Moderate, low-risk work on a maintained platform.
3. **Thin custom transaction-splice proxy** — only if neither compromise flies. Core logic is small (EHLO capability intersection, per-MAIL healthy-backend pick, verbatim relay), but it's a new mail-path component to own.

Also worth checking first: if clients can switch to short-lived connections, plain **HAProxy** covers everything — the zero-code answer.

## Sources

- [Exim spec — ACLs / cutthrough delivery](https://www.exim.org/exim-html-current/doc/html/spec_html/ch-access_control_lists.html)
- [Haraka queue/smtp_proxy docs](https://github.com/haraka/Haraka/blob/master/docs/plugins/queue/smtp_proxy.md)
- [Haraka queue/smtp_forward docs](https://github.com/haraka/Haraka/blob/master/docs/plugins/queue/smtp_forward.md)
- [Mireka — proxy mode](http://mireka.org/doc/basic-configuration/proxy.html)
- [Mireka — upstream/relayhost](https://mireka.org/doc/basic-configuration/relayhost.html)
- [Mireka source repo](https://github.com/hontvari/mireka)
- [maddy — target.smtp](https://maddy.email/reference/targets/smtp/)
- [HAProxy SMTP relay infrastructure guide](https://www.haproxy.com/blog/efficient-smtp-relay-infrastructure-with-postfix-and-load-balancers)

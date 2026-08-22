# smtp-balancer — Project Description

An SMTP-aware, cut-through load balancer. It sits between SMTP clients and a pool of destination SMTP servers, balances **each mail transaction independently** — even when a client sends thousands of messages over one long-lived connection — and relays server verdicts to the client **verbatim and immediately**, in both directions. Think *HAProxy's operability and configurability, applied at the SMTP-transaction layer instead of the TCP-connection layer*.

Prior research (`smtp-balancer-options.md`) verified that no open-source project does this: L4 balancers pin whole connections to one backend; store-and-forward MTAs answer "250 queued" themselves and hide backend verdicts. The three partial precedents — Exim `cutthrough_delivery`, Haraka `queue/smtp_proxy`, Mireka `RelayDestination` — prove the transaction-splicing pattern works but each lacks pooling + rules + active checks. This project builds the missing tool. A 7-track research pass (protocol, HAProxy distillation, health checking, Go stack, concurrency, testing, prior-art mining) fixed the design below; primary sources are cited in the epics.

## Goals

1. **R1 — HAProxy-grade rules, HAProxy-grade ease.** Named pools of weighted servers; balance algorithms per pool (smooth weighted round-robin, least-conn); ordered routing rules (client CIDR, MAIL FROM domain → pool) with a default pool; one HCL config file with strict, line-precise validation; `-c` check mode; hot reload; runtime admin API (stats, drain, force state, weight).
2. **R2 — Active health checks.** HAProxy's checker semantics applied to SMTP: probe ladder with selectable depth per pool/server — **`connect` (plain TCP, no SMTP bytes — k8s `tcpSocket`-style)** → 220 banner → EHLO → optional deep MAIL/RCPT probe — plus an optional **probe `port` override** (probe a different port than the traffic address, k8s-style), `interval`/`down_interval`/`rise`/`fall`, jittered scheduling, passive transport-error signals with active-only recovery, admin overrides.
3. **R3 — Per-transaction balancing inside one connection.** A backend is attached per mail transaction at MAIL FROM and detached after the end-of-data verdict. A client holding one connection open all day still spreads its messages across every healthy backend.
4. **R4 — Maximum byte transparency.** Every reply the backend produces for MAIL, RCPT, DATA (354 and the final verdict) reaches the client byte-for-byte and immediately — codes, enhanced status codes, multiline text. The client's envelope commands and message bytes reach the backend equally untouched. The balancer speaks for itself only where the protocol makes it unavoidable, and that set is a closed, tested enum (see Transparency Contract).

## Non-Goals

- **Not an MTA.** No queue, no retries, no bounces, no DKIM, no content filtering. If the backend says 451, the *client* owns the retry — that is the point of R4.
- **Not a policy engine.** Rules select backends; acceptance/rejection verdicts come from backends.
- **No CHUNKING/BDAT in v1.** Not advertised, so compliant clients never send it (`BDAT` anyway → 502). Three protocol reasons documented in the contract research; Exim's cutthrough made the same call.
- **No AUTH in v1.** Per-transaction backend binding makes relayed AUTH incoherent (auth is connection-scoped; the session spans many backends). `AUTH` → 502. Target deployment is a trusted internal segment. Local AUTH termination is a possible post-v1 feature.
- **No Received: header insertion in v1.** RFC 5321 expects relays to add one; we deliberately deviate to preserve byte transparency (the balancer behaves as a middlebox, not an MTA). Documented deviation; optional prepend-only insertion is a possible post-v1 feature.

## Requirements Analysis

### The central tension (R3 vs R4) and its resolution

Full *session* transparency and per-transaction balancing are mutually exclusive: at connect time there is no chosen backend whose banner/EHLO could be mirrored, and one client session may touch N backend sessions. Therefore the balancer owns the *session framing* (banner, EHLO, STARTTLS) and is perfectly transparent at the *transaction* level. The bright-line rule (verified against RFC 5321/2920/3207): **any reply generated while a backend transaction is live is relayed verbatim; everything before backend attachment, and everything about connection lifecycle, is synthesized.** The Transparency Contract below enumerates every synthesized reply; integration tests assert the contract reply-by-reply.

### "As in HAProxy" — what is replicated, what is skipped

Replicated (the operational model): defaults→pool→server config inheritance (one level); one-line-ish server declarations with weight, backup, max_transactions, check parameters; `balance roundrobin|leastconn`; ordered routing rules with fallthrough default; `-c` config check; runtime admin (show state/stats, set state ready/drain/maint, force health, set weight, reload); drain semantics; `option smtpchk`-style checks extended with a deeper ladder.

Skipped deliberately (v1): general ACL expression language (two match keys cover the stated use cases; additive later), hash/source balancing (session affinity contradicts R3), stick tables, agent-check, slowstart (flagged as the natural post-v1 feature for MTA IP-warmup), PROXY protocol/XCLIENT (post-v1 epic), DNS service discovery, Lua/maps, multi-level defaults.

### "As in Mireka" — the transaction lifecycle, corrected

Adopted from Mireka/Baton: per-transaction backend attachment, real-time step-by-step relay, weighted-shuffle ordered failover candidates (its `Upstream`). Corrected using its source as a cautionary tale: attach at **MAIL FROM**, not first-RCPT (Mireka's lazy attach forces a synthesized 250 to MAIL, distorting backend sender-rejects; its rationale — skipping connects for spam — is irrelevant for trusted clients); error latching is **two-class** (transaction-fatal errors latch until RSET/next-MAIL; per-RCPT rejections never latch — Mireka latches everything, so one 550 poisons subsequent valid RCPTs); never append text to backend replies (Mireka does); never map transient IO errors to permanent codes (Baton maps IOException→554, turning outages into bounces); never fan one message out to multiple backends (Baton multicasts DATA and drops per-target failures silently).

### Protocol-forced constraints (verified)

- **One backend per message** — all RCPTs of a transaction go to one backend (N verdicts cannot map to one client reply; Exim imposes the same).
- **STARTTLS terminates per leg** — client-leg TLS is the balancer's; backend-leg TLS is configured per pool (none | STARTTLS with optional verification). After the client-leg handshake: full state reset, fresh EHLO required (RFC 3207).
- **DATA is a raw pipe** — dot-stuffing is terminator-preserving, so unstuff+restuff is the identity; the relay only scans for `CRLF.CRLF` (5-state scanner) and never rewrites bytes. No line-length enforcement inside DATA.
- **Mid-transaction backend death needs synthesis** — and if it happens mid-DATA, the balancer **must keep consuming the client's stream to `CRLF.CRLF`** before answering 451, or the session desynchronizes (message bytes would parse as commands).

## Transparency Contract (normative)

**Rule: replies to MAIL, RCPT, DATA (354 and final verdict), and RSET-while-attached are relayed byte-verbatim, immediately, including multiline lines and enhanced status codes. The balancer's own state machine keys only off the first digit of the final reply line. Everything else is synthesized from the closed enum below (single source: `internal/proxy/replies.go`).**

| Client-visible event | Reply | Notes |
|---|---|---|
| Connection banner | **Synth** `220 <hostname> ESMTP` | configurable hostname/text |
| `EHLO` | **Synth** `250-` multiline | static configured capability set (see policy below) |
| `HELO` | **Synth** `250` | no extensions |
| `STARTTLS` | **Synth** `220` → TLS handshake; `501` params present; `503` mid-transaction or already-TLS; `502` when not advertised (no cert configured) | RFC 3207; post-handshake state reset; no 454 trigger exists in v1, so 454 is not in the enum |
| `NOOP` | **Synth** `250` | never touches a backend |
| `QUIT` | **Synth** `221` + close | attached backend leg aborted/quit |
| `RSET` (no backend attached) | **Synth** `250` | |
| `RSET` (attached) | **Relay** to backend, relay its `250`, then detach | fresh pick at next MAIL |
| `VRFY` / `EXPN` / `HELP` | **Synth** `252` / `502` / `214` | no backend to ask pre-selection |
| Unknown command | **Synth** `500` — connection stays open | RFC 5321 4.2.4; never forwarded |
| Command line terminated by bare LF | **Synth** `500 5.5.2` — never relayed | SMTP-smuggling defense (CVE-2023-51764 class); strict CRLF on commands AND in the DATA framer; matches modern Postfix/Exim/Sendmail defaults |
| Command line over 4 KB | **Synth** `500 5.5.2` — session continues in sync | |
| Pipelining queue overflow (>32 lines / 16 KB pre-attach) | **Synth** `421 4.7.0` + close | protects the streaming-only rule |
| `EHLO` while a backend is attached | backend aborted, **Synth** fresh `250-` capability reply, latch cleared | RFC 5321 4.1.4: EHLO acts as RSET |
| Session lifetime exceeded (1 h default) | **Synth** `421 4.4.2` + close | anti-slow-loris backstop |
| `RCPT`/`DATA` before `MAIL` | **Synth** `503` | |
| `AUTH` / `BDAT` | **Synth** `502` | not advertised, not supported v1 |
| `MAIL FROM` | **Relay** — backend attached now; its actual verdict | second MAIL mid-txn: relayed; backend's 503/2yz drives state |
| `RCPT TO` (each) | **Relay** | |
| `DATA` go-ahead | **Relay** backend's `354` or error — **never synthesize 354** | predecessors' shared bug |
| End-of-data verdict | **Relay** | the message's true fate, always |
| Backend `421` while attached (pre-DATA) | **Translate → `451 4.4.2`**, drop backend, keep session | the one sanctioned rewrite: relaying 421 would falsely announce client-channel close |
| Backend final reply mid-DATA (any non-354 final reply after relayed 354, before the dot) | **Relay it — it IS the transaction's single verdict** (a 421 here is translated → `451 4.4.2`). DATA is then answered: keep consuming client bytes to `CRLF.CRLF` (piping if the backend is still alive, discarding if dead), emit **nothing** after the dot, disarm the dot-reply timer, detach | exactly one reply per DATA, ever; the failure-synthesis rows below fire ONLY when no verdict was relayed |
| Malformed backend reply (unparseable line, code outside 2xx–5xx, oversized) | treated as backend death at the current phase — the matching `451` row below applies | defensive bound, backend dropped |
| All attach attempts fail at MAIL | **Synth** `451 4.4.1` (per queued command; RSET/NOOP in queue get `250`; a queued QUIT gets `221` + close) | silent retry across healthy backends first — safe only while zero backend bytes reached the client |
| Backend dies pre-354 | **Synth** `451 4.4.1` for pending commands | session survives |
| Backend dies mid-DATA (no verdict relayed) | discard client bytes to `CRLF.CRLF`, then **Synth** `451 4.4.1` | session survives |
| Backend dies after dot, before verdict | **Synth** `451 4.4.2` + duplicate-risk log event | inherent cut-through hazard, documented |
| All healthy backends saturated (`max_transactions`) | **Synth** `451 4.3.2` | transaction-scoped; session survives |
| Accept overload (`global_maxconn`) | **Synth** `421 4.3.2` + close | connection-scoped |
| Client idle / stalled DATA feed | **Synth** `421 4.4.2` + close | |
| Drain/shutdown | in-flight transaction finishes, then **Synth** `421 4.3.0` + close | |
| Pipelined batch | replies emitted strictly in command order | RFC 2920 constrains order, not timing |

**421 vs 451 rule:** `451 4.4.x/4.3.2` for transaction-scoped failures (session stays open, client may retry on the same connection); `421` only for connection-scoped events (overload at accept, idle/stall, drain) and always followed by close. Never `554` for backend failure — permanent codes would bounce mail a healthy backend would take.

**EHLO capability policy:** the advertised set is **statically configured** with safe defaults (`PIPELINING`, `8BITMIME`, `SIZE <configured>`, `STARTTLS` iff cert configured; operators may add `SMTPUTF8`, `DSN`, `ENHANCEDSTATUSCODES`). Enforcement is delegated: the health checker's EHLO probe marks any backend whose capability set is **not a superset** of the advertised set as *incompatible* and removes it from rotation, with two carve-outs: (1) **`STARTTLS` is excluded from the superset comparison** — it is client-leg-terminated; backend TLS is governed solely by the pool's `backend_tls` mode; (2) **`SIZE` compares numerically, and a bare `SIZE` or `SIZE 0` means unlimited (RFC 1870) and satisfies any advertised value.** MAIL/RCPT parameters always relay verbatim — the backend is the enforcer. `SMTPUTF8` requires `8BITMIME` (RFC 6531) — config validation enforces the pairing. **v1 supports exactly one listener** (config-validated), so "the advertised set" has a single referent for the health verdict. Computed live intersection and multi-listener are possible post-v1 modes.

## Architecture

Single Go binary. Per session: **one owner goroutine** (client reader, state machine, DATA pipe) plus, only while a backend is attached, **one reply-pump goroutine** (tees backend reply bytes verbatim to the client while parsing codes for state). Goroutine total is bounded: `1 + 2×maxconn + n_backends`. Streaming-only is absolute — the only data-path buffers are the 4 KB command line, the 16 KB pipelining queue, and the 32 KB copy buffer; a 1 GB message must relay under a 64 MB heap ceiling.

```
client ──TLS?──▶ session engine ──MAIL──▶ router (rules→pool→algorithm→ordered candidates)
                   │      ▲                          │
                   │      └── verbatim reply pump ◀──┤ backend dial+handshake ──TLS?──▶ backend
                   ▼                                 ▼
             config (HCL, atomic swap)      health checker (active ladder + passive signals)
                   ▼                                 ▼
             admin HTTP/unix API · Prometheus metrics · structured transaction logs
```

### Module map, usage, and test strategy

Full per-module test matrices (conditions × load × failure) live in the epics; this table is the index.

| Module | Role in the system | Used by | Tested by (epic) |
|---|---|---|---|
| `internal/smtpwire` | Raw-preserving wire primitives: command-line reader (4 KB cap), streaming multiline reply reader, `CRLF.CRLF` DataFramer. Zero normalization — the R4 foundation. | session, relay, backend, health | 03: table-driven units + **three fuzz targets** (framer is flagship: dot-boundary splits across reads); torture corpus in `testdata/fuzz` |
| `internal/config` | HCL parse, strict decode, semantic validation (cross-refs, timeout-hierarchy ordering, capability pairing), `-c` mode, atomic runtime swap | all | 02: fixture configs ↔ expected line-precise diagnostics; race-tested swap |
| `internal/fakesmtp` | Scriptable fake backend: per-verb reply scripts, Drop/RST/Hang/Drip actions, down-modes, event hooks, byte-exact Recorder. **Foundation for every integration test.** | all integration/chaos/load tests | 01: self-tested against stdlib `net/smtp` as reference client |
| `internal/smtpdrv` | Scripted test client: Expect/Send/Pipeline/Raw/AbortMidData/StartTLS | integration tests, loadgen | 01: against fakesmtp |
| `internal/proxy` (session) | Client leg: banner, EHLO/HELO, STARTTLS, synthesized replies (`replies.go` closed enum), pre-attach states, pipelining queue, limits | main | 03: state-machine tables; protocol-violation suite; idle/overlong-line expiry ↔ exact replies |
| `internal/backend` | Backend leg: dial, greeting+EHLO, capability-superset check, STARTTLS, verbatim send, best-effort QUIT, error taxonomy | relay, health | 04: against fakesmtp scripts (slow banner, wrong caps, TLS mismatch, hang) |
| `internal/proxy` (relay) | Transaction splice: attach-at-MAIL with silent failover, queue replay, verbatim pump, 421→451 translation, DATA pipe + watchdog, failure synthesis, two-class error latching | main | 05: contract tests reply-by-reply; named prior-art regression tests; M1 distribution proof; 1 GB streaming ceiling |
| `internal/health` | Probe ladder L0–L3 (L0 = plain TCP without SMTP, k8s-tcpSocket-style; all levels honor an optional probe-port override), rise/fall FSM, jittered scheduler, passive signals (transport-only), admin states | router (gating), admin | 06: fake-clock FSM tables; flap scripts vs predicted transitions; 100-backend probe load with goleak |
| `internal/balance` | Smooth WRR, leastconn, ordered failover candidates (healthy-filter → pick → shuffle rest → backups), rule engine (CIDR, MAIL FROM domain), in-flight counters, max_transactions | relay | 07/08: seeded-rand distribution properties; saturation chaos |
| `internal/admin` + `internal/metrics` | HTTP/unix admin API (state/stats/weight/reload), Prometheus metrics, slog transaction records | operators | 09: API round-trips; drain-visibility integration |
| `cmd/smtp-balancer` | wiring, signals (SIGTERM drain, SIGHUP reload) | — | 10: drain/reload-under-load chaos; timeout budget audit |
| `cmd/loadgen`, `cmd/fakesmtp` | load generator (C×M, rate-paced, stdlib percentiles, `-direct` baseline), standalone fake | CI load gates | 11: ratio gates (p99 ≤ 2×direct+2ms), goroutine/RSS ceilings, soak |

### Timeout budget (defaults; all configurable; validation rejects inverted hierarchies)

| Timer | Default | On expiry (exact reply) |
|---|---|---|
| client first-command / idle | 300 s | `421 4.4.2` + close |
| session max lifetime | 1 h | `421 4.4.2` + close |
| backend connect (per attempt × 2 attempts) | 5 s | next candidate; exhausted → `451 4.4.1` |
| backend handshake (greeting+EHLO+TLS) | 15 s | same as connect failure |
| backend MAIL/RCPT reply | 30 s | `451 4.4.2`, drop backend, session survives |
| backend 354 wait | 60 s | `451 4.4.2`, drop backend |
| DATA progress watchdog (bytes-moving, per direction) | 60 s | client-side stall `421 4.4.2`+close; backend-side stall → discard-to-dot → `451 4.4.2` |
| backend final-dot reply | **600 s (RFC minimum, kept deliberately)** | `451 4.4.2` + duplicate-risk log |
| drain: lame duck (healthz 503 before listener close) | 2 s | — |
| drain: force deadline | 30 s | backend legs closed first (clean abort), then `421` + close |

The RFC 5321 §4.5.3.2 values are **floors** (client-side minimum waits), not ceilings: config validation rejects only non-positive or internally inverted values, emits a **warning** (never an error) for values below the RFC floors, and imposes no upper caps. Defaults sit below the floors deliberately (documented deviation for a balancer between cooperating parties) except the dot-reply timer, which stays at the RFC floor because expiring early after a delivered dot manufactures duplicate mail.

## Key Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Go** (go.mod ≥1.25, CI 1.26) | I/O-bound line protocol; goroutine-per-session maps 1:1; stdlib TLS with first-class STARTTLS wrap; static binary; fastest agent iteration. Rust's wins irrelevant at SMTP rates. |
| D2 | **HCL config** (`hashicorp/hcl/v2` + gohcl) | R1 is the prime requirement: haproxy-feel blocks, native ordered rule blocks, strict decoding, file:line:column diagnostics for free. yaml.v3 is archived and needs custom work to match diagnostics. HCL types stay inside `internal/config`. |
| D3 | **Backend attach at MAIL FROM** | Earliest moment full verbatim relay is possible (MAIL verdict is real). Mireka's first-RCPT laziness exists for spam economics that don't apply to trusted clients. |
| D4 | **Fresh backend connection per transaction (v1)** | Haraka removed pooling wholesale after years of bugs (#2788 key leaks, RSET-reuse races). Pooling is post-v1 epic 12 with four named hazards pre-specified. Cost: one TCP+EHLO(+TLS) per message + backend conn-rate pressure (documented; `max_transactions` caps it). |
| D5 | **One backend per message** | Protocol-forced (Baton's fan-out silently lost mail). |
| D6 | **No CHUNKING, no AUTH, no Received insertion (v1)** | Each removes a failure class; all documented deviations/deferrals. |
| D7 | **Streaming-only, bounded buffers** | 1 GB message under 64 MB heap, enforced by test. |
| D8 | **Closed synthesized-reply enum** (`replies.go`) + contract tests | R4 audit surface: everything not in the enum is provably verbatim. |
| D9 | **PIPELINING advertised in v1** | Core to R3 value (bulk senders pipeline). Mechanics fully specified: bounded 32-line/16 KB pre-attach queue, verbatim replay, in-order replies, overflow `421 4.7.0`; read-ahead drained forward at DATA. |
| D10 | **Static advertised EHLO set + health-enforced backend superset** | Deterministic client view; capability drift becomes a health verdict (`incompatible`), not a mid-transaction surprise. |
| D11 | **451 for transaction-scoped, 421 for connection-scoped, never 554 for infra failures; backend 421 → 451 translation** | RFC semantics; keeps sessions alive (R3) and mail unbounced. |
| D12 | **Two-class error latching** | Transaction-fatal latches until RSET/next-MAIL; per-RCPT verdicts never latch (Mireka's over-latching bug). |
| D13 | **Selection = healthy-filter → algorithm → ordered failover walk (primaries, then backups)** | Mireka `Upstream` shape + health gating; silent failover valid only before any backend byte reached the client. |
| D14 | **In-process hot reload** (validate → `atomic.Pointer` swap; effective at next MAIL; listener changes require restart in v1) | Long-lived sessions must survive reloads (R3); HAProxy's restart-reload model would orphan them. |
| D15 | **Admin = HTTP+JSON, loopback/unix-socket only** (unauthenticated by design; TCP binds must be loopback unless `allow_remote = true` is set explicitly) | curl-scriptable; stdlib-served; states ready/drain/maint × auto/force-up/force-down. Reload-survival matrix: admin state (ready/drain/maint) **survives** reload; force overrides **survive** reload; runtime weight **reverts** to config on reload with a logged list of discarded overrides. Everything is lost on process restart (like HAProxy without a state file). |

## Accepted Semantics & Risks (documented, not hidden)

- **Duplicate-delivery window:** if a backend dies after receiving the final dot but before replying, the client gets 451 and may resend a message the backend actually queued. Inherent to cut-through (Exim spools to avoid it; we don't queue by design). Mitigation: 600 s dot timer, duplicate-risk log event, metric.
- **Backend connection rate:** fresh-per-transaction multiplies backend connect rate; backends with postscreen/anvil-style limits must whitelist the balancer. `max_transactions` caps concurrency per server.
- **Probe log noise on backends:** probes always QUIT politely; `down_interval` reduces dead-server churn; deep probes are off by default and documented (greylisting interaction).
- **Discard-to-dot bandwidth:** after mid-DATA backend death the balancer pays for the rest of the client's message; bounded by the progress watchdog and session lifetime.
- **Mid-DATA early backend replies** (e.g. 552, with or without close) follow the single-verdict contract row: the early reply is the transaction's only verdict, the remaining client bytes are consumed to the dot, and nothing is emitted after it. Clients that only read after the dot see the verdict then — interop covered by named tests.

## References

- `smtp-balancer-options.md` — options research that motivated the project
- `docs/plans/ROADMAP.md` — epic ordering; `docs/plans/epic-*.md` — executable plans
- RFC 5321, 2920 (PIPELINING), 3207 (STARTTLS), 1870 (SIZE), 6152 (8BITMIME), 6531 (SMTPUTF8), 3461 (DSN), 3463/2034 (enhanced codes), 3030 (CHUNKING)
- HAProxy configuration & management manuals (checker semantics, drain model, admin verbs)
- Mireka source (`filter/proxy`, `Upstream`), Haraka `queue/smtp_proxy` docs & issues #2788/#1842, nginx mail module, Exim cutthrough spec — precedent designs and their bugs

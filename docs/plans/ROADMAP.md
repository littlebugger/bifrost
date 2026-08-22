# smtp-balancer — Implementation Roadmap

> No checkboxes here by design — ralphex-executable task checklists live only in the epic files.
> Spec: `/PROJECT.md`. Epic files: `docs/plans/epic-NN-*.md` (62 tasks total across epics 00–11).

Each epic is one ralphex-executable plan (`epic-NN-<name>.md`) sized so a fresh-context agent can
implement it task by task. An epic is *done* when its Validation Commands pass and its module's
condition/load/failure test matrix (defined in the epic) is green.

## Epic order and dependencies

| Epic | Name | Delivers | Depends on |
|---|---|---|---|
| 00 | `epic-00-scaffold` | Go module, repo layout, Makefile (`lint test test-race integration fuzz-short load-smoke`), CI config, versioned no-op binary | — |
| 01 | `epic-01-smtptest-harness` | Scriptable fake SMTP backend (in-process lib) + scripted client driver. **Foundation: every later epic's integration tests run on this.** | 00 |
| 02 | `epic-02-config` | Config schema, strict parser/validator, `-c` check mode, defaults inheritance | 00 |
| 03 | `epic-03-smtp-session` | Wire primitives (`smtpwire`: command reader, reply reader, DataFramer — fuzz targets) + client-leg engine: banner, EHLO/HELO, command reader, session state machine, PIPELINING queue, STARTTLS, per-session limits (accept loop lands in epic 08) | 01, 02 |
| 04 | `epic-04-backend-client` | Backend-leg client: dial, EHLO + capability cache, STARTTLS, raw-preserving reply reader | 01, 02, 03 (`smtpwire`) |
| 05 | `epic-05-relay` | Transaction splicer: envelope relay, raw DATA pipe, verbatim reply passthrough, failure synthesis. **Transparency Contract tests live here.** | 03, 04 |
| 06 | `epic-06-health` | Active probe ladder (connect = plain TCP without SMTP, k8s-style / banner / EHLO / deep; optional probe-port override), rise/fall state machine, passive signals, flap damping | 04 |
| 07 | `epic-07-router` | Balance algorithms (smooth WRR, leastconn) + rule engine (client CIDR, MAIL FROM domain → pool) | 02, 05 (`TxnMeta`, M1 test), 06 |
| 08 | `epic-08-pool-limits` | Per-server and global maxconn, transaction leasing/counters (leastconn input), exhaustion replies per contract | 05, 07 |
| 09 | `epic-09-observability` | Prometheus metrics, structured per-transaction logs, admin API (stats, force up/down/drain, reload trigger), operator docs (`docs/operations.md`) | 05–08 |
| 10 | `epic-10-resilience` | Graceful drain (SIGTERM), hot reload (SIGHUP/validate-swap), end-to-end timeout budget, chaos scenario suite | 08, 09 |
| 11 | `epic-11-load-bench` | Load generator completion, load gates (p99 added-latency budget, goroutine-leak-free, bounded RSS), soak test | 10 |

Parallelizable pairs: 01∥02 (after 00). Epics 03→04→05→06/07 are sequential (04 needs 03's `smtpwire`; 07 needs 05's `TxnMeta`); 06 can run parallel to 05.

## Post-v1 (specified, unscheduled)

| Epic | Name | Why deferred |
|---|---|---|
| 12 | `epic-12-backend-reuse` | Backend connection pooling/reuse via RSET — perf optimization with distinct hazards (stale conns, capability drift, health-race); v1 ships fresh-connection-per-transaction |
| 13 | `epic-13-rcpt-routing` | RCPT-domain rules require answering MAIL locally + envelope replay — an explicit, opt-in transparency trade |
| 14 | `epic-14-source-transparency` | PROXY protocol / XCLIENT toward backends; needs backend-side trust config; carries named regression `xclient-mode-double-ehlo` (XCLIENT forces re-EHLO; capabilities must be re-read from the second EHLO) |

## Milestones

- **M1 (after 05):** balances a pipelined multi-message connection across 2 fake backends with verbatim replies — the R3+R4 proof.
- **M2 (after 09):** ops-complete: health-checked pools, rules, metrics, admin — the R1+R2 proof.
- **M3 (after 11):** production candidate: drains cleanly, reloads under load, survives the chaos suite within performance gates.

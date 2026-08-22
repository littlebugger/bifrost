# Epic 09: Observability & Admin API

> Part of **smtp-balancer** — read `/PROJECT.md` first. Depends on epics 05–08.
> One task per fresh-context agent session, in order. TDD; commit after each task.

## Overview

**Goal:** the R1/R2 ops plane: Prometheus metrics, structured per-transaction logs, the HTTP/unix admin API (HAProxy stats-socket analog): show state/stats, set server state (ready/drain/maint), force health, set weight, trigger reload, healthz — and the operator documentation that carries the HAProxy deltas out of the planning files.

**Security model (normative):** the admin API is **unauthenticated by design** and therefore loopback/unix-socket only — config validation (epic-02) rejects non-loopback TCP binds without an explicit `allow_remote = true`. Deployments that need the `/healthz` lame-duck signal reachable by an upstream L4 balancer must use a TCP loopback/host bind (documented in operations.md), since a unix-only bind makes lame-duck invisible to the LB.

**Produces:**
```
GET  /servers                 → JSON: pool/server → {op, admin, override, incompatible, weight, in_flight, consec_fail, last_change (RFC3339 — "DOWN since when" is the first 3am question), last_probe{level,result,latency,detail (error/reply text, e.g. the 554 banner received)}}
GET  /stats                   → JSON: per pool/server counters + balancer totals
GET  /healthz                 → 200 when serving; 503 while draining (drain flips this FIRST — lame duck)
GET  /metrics                 → Prometheus
POST /servers/{pool}/{srv}/state    {"state":"ready|drain|maint"}
POST /servers/{pool}/{srv}/health   {"override":"auto|force-up|force-down"}
POST /servers/{pool}/{srv}/weight   {"weight":N}        // runtime-only, documented ephemeral
POST /reload                  → run config Load+Validate; 200 + diff summary | 422 + diagnostics
```
Metric names (stable contract): `smtpbal_sessions_active`, `smtpbal_sessions_total`, `smtpbal_transactions_total{pool,server,verdict_class}` (verdict_class ∈ 2xx|4xx|5xx|synth_451|synth_421), `smtpbal_synthesized_replies_total{code_enhanced}`, `smtpbal_relay_bytes_total{direction}`, `smtpbal_backend_dials_total{server,result}`, `smtpbal_probe_total{server,level,result}`, `smtpbal_server_up{pool,server}` gauge, `smtpbal_server_eligible{pool,server}` gauge (up AND ready AND compatible — "deliberately not receiving traffic" must be visible to Prometheus), `smtpbal_server_state_changes_total{pool,server}` (flap alerting), `smtpbal_in_flight{pool,server}` gauge, `smtpbal_duplicate_risk_total`.

Transaction log (slog, one record per transaction): `client`, `helo`, `pool`, `server`, `mail_verdict`, `rcpt_count`, `rcpt_verdicts_class`, `data_verdict` (verbatim first line), `bytes`, `duration_ms`, `failover_attempts`, `synth` (which enum reply, if any).

## Module Usage

Weight changes go through `balance` (WRR state rebuild for that server), state/override through `health.Checker`, reload through `config.Holder` + the swap plumbing (full sequence in epic 10; here the endpoint validates and swaps). Ops runbooks: drain a server → watch `in_flight` hit zero → maint it.

## Test Strategy

Conditions: every endpoint round-trips; metrics move when traffic flows. Failure: bad pool/server → 404; invalid body → 400; reload with broken config → 422 + line-precise diagnostics, old config stays live. Load: /servers under concurrent traffic race-clean. Integration: drain visibility end-to-end (drained server stops receiving new transactions mid-client-connection — the R3-specific drain proof).

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`
3. `make integration`

### Task 1: Metrics registry + instrumentation

**Files:**
- Create: `internal/metrics/metrics.go`; Modify: relay/health/balance/listener call sites; tests `internal/metrics/metrics_test.go`

- [ ] **Step 1:** failing tests: `TestMetricNamesStable` (registry exposes exactly the documented names — golden list), `TestTransactionCountersMove` (integration: one relayed message → transactions_total{verdict_class="2xx"} +1, relay_bytes both directions >0), `TestSynthesizedReplyCounter` (all-backends-down MAIL → synthesized_replies_total{code_enhanced="451 4.4.1"} +1)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement with promauto on a private registry (no global default registry — testability); add `github.com/prometheus/client_golang`
- [ ] **Step 4:** `make verify-new PKG=./internal/metrics TESTS='TestMetricNamesStable'` and `make verify-new PKG=./internal/metrics TESTS='TestTransactionCountersMove TestSynthesizedReplyCounter' TAGS=integration
- [ ] **Step 5:** commit `feat(metrics): registry + relay/health instrumentation`

### Task 2: Transaction logging

**Files:**
- Create: `internal/proxy/txnlog.go`; tests `internal/proxy/txnlog_test.go`

- [ ] **Step 1:** failing tests: `TestTxnLogRecordComplete` (capture slog to buffer; one message → one record with all documented fields), `TestTxnLogSynthAndDuplicateRisk` (post-dot death → record carries synth reply + duplicate_risk=true), `TestTxnLogNeverLogsBody` (grep captured output for body marker bytes — absent; privacy/size rule)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement
- [ ] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestTxnLogRecordComplete TestTxnLogSynthAndDuplicateRisk TestTxnLogNeverLogsBody'`
- [ ] **Step 5:** commit `feat(obs): structured transaction log`

### Task 3: Admin read endpoints

**Files:**
- Create: `internal/admin/admin.go`, `internal/admin/admin_test.go`

- [ ] **Step 1:** failing tests: `TestServersEndpoint` (reflects health FSM + weights + in-flight + last_change + last_probe.detail), `TestStatsEndpoint`, `TestHealthzDrainAware` (200 → 503 after drain flag), `TestMetricsServed`, `TestUnixSocketBind` (`unix:///` admin bind works; file perms 0600), `TestServersEndpointUnderTraffic` (integration: relay traffic against fakes while hammering GET /servers + /stats in a loop under `-race`; all responses 200 with parseable JSON — the concurrent-read race proof)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement (stdlib net/http, no framework; JSON via encoding/json)
- [ ] **Step 4:** `make verify-new PKG=./internal/admin TESTS='TestServersEndpoint TestStatsEndpoint TestHealthzDrainAware TestMetricsServed TestUnixSocketBind'` and `make verify-new PKG=./internal/admin TESTS='TestServersEndpointUnderTraffic' TAGS=integration
- [ ] **Step 5:** commit `feat(admin): read endpoints over http/unix`

### Task 4: Admin write endpoints

**Files:**
- Modify: `internal/admin/admin.go`; tests `internal/admin/write_test.go`

- [ ] **Step 1:** failing tests: `TestSetStateDrainMaintReady` (round-trip through health.Checker), `TestSetOverride`, `TestSetWeightRebuildsWRR` (weight change visible in next picks), `TestReloadEndpointGood` (200 + diff summary; new pool live), `TestReloadEndpointBad` (422 + diagnostics with file:line; old config still serving), `TestValidation404s400s`
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement
- [ ] **Step 4:** `make verify-new PKG=./internal/admin TESTS='TestSetStateDrainMaintReady TestSetOverride TestSetWeightRebuildsWRR TestReloadEndpointGood TestReloadEndpointBad TestValidation404s400s'`
- [ ] **Step 5:** commit `feat(admin): write endpoints (state/override/weight/reload)`

### Task 5: Drain visibility end-to-end (M2 slice)

**Files:**
- Create: `test/integration/admin_drain_test.go` (integration tag, goleak)

- [ ] **Step 1:** failing test `TestDrainMidClientConnection` (client sends msg1 → server A (only pick); admin drains A mid-connection; msg2 on the SAME client connection routes to B; A's probes still recorded; un-drain → A eligible again — the R3 drain-converges-in-one-transaction proof)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** fix what it surfaces
- [ ] **Step 4:** `make verify-new PKG=./test/integration TESTS='TestDrainMidClientConnection' TAGS=integration` and `make integration`
- [ ] **Step 5:** commit `feat(admin): drain visibility proven end-to-end`

### Task 6: Operator documentation

**Files:**
- Create: `docs/operations.md`

- [ ] **Step 1:** write `docs/operations.md` from the shipped behavior (not aspirations): admin endpoint reference with curl examples for every verb; the drain runbook (`drain → watch smtpbal_in_flight → maint`, converges within one transaction per client connection); reload semantics and the survival matrix (admin state and force overrides survive, runtime weights revert with a logged discard list, everything dies on restart); cert rotation via reload (same path, new content — no restart); the **"coming from HAProxy" delta list**: no stats-socket line protocol (HTTP+JSON), runtime changes ephemeral (no state file), no slowstart yet, drain converges per-transaction not per-connection, listener bind changes require restart, admin is unauthenticated → loopback/unix only, healthz-behind-L4 bind guidance, probe-level selection guide (`connect` = plain TCP/k8s-tcpSocket-style — zero SMTP but logs "lost connection after CONNECT" on MTAs; `banner`/`ehlo` QUIT politely; `port` override for dedicated health sockets), probe log noise on backends + whitelisting advice, duplicate-delivery window semantics
- [ ] **Step 2:** cross-check every claim in the doc against the epics/tests that implement it (each section links the named test proving it)
- [ ] **Step 3:** `make lint` clean (markdown not linted, but repo stays green)
- [ ] **Step 4:** commit `docs: operator guide with haproxy deltas`

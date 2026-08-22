# Epic 10: Resilience — Drain, Reload, Timeout Audit, Chaos Suite

> Part of **smtp-balancer** — read `/PROJECT.md` first. Depends on epics 00–09 (this epic hardens the assembled binary).
> One task per fresh-context agent session, in order. TDD; commit after each task.

## Overview

**Goal:** `cmd/smtp-balancer` becomes a production process: full wiring, SIGTERM graceful drain, SIGHUP hot reload, an audited end-to-end timeout budget, and the named chaos suite (scenarios not already landed in earlier epics).

**Produces:** runnable binary (`smtp-balancer -f config.hcl`); drain sequence: healthz→503 → 2s lame duck → listener close → idle sessions `421 4.3.0` → in-flight transactions finish → 30s force deadline (backend legs closed FIRST — clean abort, no partial delivery) → exit 0. Reload: SIGHUP or POST /reload → validate → atomic swap → next-MAIL semantics; removed servers drain; **listener/admin bind changes are rejected with a clear diagnostic (restart required, v1)**.

## Module Usage

This is the epic where operators meet the product. Every prior module is wired in `main.go` (still thin: construct config holder, health checker, router, relay — passing `router.Pick`/`router.Lease` as the relay's PickFunc/LeaseFunc closures — listeners, admin; run under one WaitGroup + error channel (stdlib, no errgroup dep); signal handling).

## Test Strategy

Integration/chaos on the real binary path (in-process `run()` where possible, `os/exec` for signal tests). Conditions: clean start/stop. Failure: the remaining chaos scenarios 1–10 + 12–22 (11 landed in epic 08). Load: reload storm under traffic. The timeout audit is a table: every timer row from PROJECT.md × a hostile fixture proving expiry produces the exact contract reply.

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`
3. `make integration`
4. `make chaos`

### Task 1: Full wiring in main

**Files:**
- Modify: `cmd/smtp-balancer/main.go`; Create: `cmd/smtp-balancer/wire.go`, `test/integration/binary_test.go`

- [ ] **Step 1:** failing tests: `TestBinaryEndToEnd` (in-process run() with example-config-adapted fixture: 2 fakes as servers → smtpdrv sends 4 messages on one connection → spread per weights, verdicts verbatim, admin /servers reflects reality, metrics move), `TestBinaryConfigCheckMode` (already exists from 02 — extend: `-c` validates the FULL wiring config incl. listener certs)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement wiring with **stdlib only** — `sync.WaitGroup` + a small error-collecting channel over the three long-runners (listeners, health.Run, admin server), one root ctx tree; do NOT add `golang.org/x/sync/errgroup` (outside the deps budget in epic-00 / PROJECT.md). Wire `router.Lease` into `NewRelay` as its `LeaseFunc` and `router.Pick` as its `PickFunc` here (the closures keep `internal/proxy` import-free of `internal/balance`)
- [ ] **Step 4:** `make verify-new PKG=./test/integration TESTS='TestBinaryEndToEnd' TAGS=integration` and `make integration`
- [ ] **Step 5:** commit `feat(main): full wiring`

### Task 2: SIGTERM drain

**Files:**
- Create: `cmd/smtp-balancer/drain.go`, `test/integration/drain_test.go`

- [ ] **Step 1:** failing tests: `TestDrainSequence` (in-process: start, open idle session + mid-DATA session; trigger drain; assert order: healthz 503 first, idle session gets 421 4.3.0, mid-DATA finishes and its verdict is verbatim, THEN its session gets 421 on next command; process exits 0; goleak), `TestDrainForceDeadline` (session with fake hanging at EOD + drain_timeout 1s → backend leg closed first (fake sees EOF, no dot beyond what client sent... assert no synthesized dot), client gets 421, exit 0 within deadline), `TestDrainSignalReal` (os/exec: SIGTERM → exit 0)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement per the produced sequence above
- [ ] **Step 4:** `make verify-new PKG=./test/integration TESTS='TestDrainSequence TestDrainForceDeadline TestDrainSignalReal' TAGS=integration
- [ ] **Step 5:** commit `feat(main): graceful drain`

### Task 3: SIGHUP reload

**Files:**
- Create: `cmd/smtp-balancer/reload.go`, `test/integration/reload_test.go`

- [ ] **Step 1:** failing tests: `TestReloadNextMailSemantics` (client mid-connection: msg1 under old config → pool A; swap config (weights/pools changed); msg2 same connection → new rules; in-flight transaction during swap finishes on its old backend), `TestReloadRemovedServerDrains` (server removed from config: in-flight finishes, no new picks, probes stop), `TestReloadBadConfigKeepsOld` (SIGHUP with broken file → error logged with diagnostics, traffic continues on old config), `TestReloadListenerChangeRejected` (**only bind/address changes are rejected** — explicit "restart required" diagnostic, old config live), `TestReloadPicksUpRotatedCert` (cert/key file content replaced at the SAME path → reload accepted; next client STARTTLS handshake presents the new cert while existing TLS sessions keep theirs — the routine 90-day rotation MUST NOT need a restart), `TestReloadRevertsRuntimeWeight` (admin-set weight, then SIGHUP → weight back to config value, log line lists the discarded override — the D15 survival matrix), `TestReloadStorm` (chaos: 20 reloads in 10s under C=10 traffic → zero client errors, zero leaks)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement (reuse Holder.Swap + health generation tokens + admin-state/override carryover from 06; listener cert loading via `tls.Config.GetCertificate` reading the swapped config)
- [ ] **Step 4:** `make verify-new PKG=./test/integration TESTS='TestReloadNextMailSemantics TestReloadRemovedServerDrains TestReloadBadConfigKeepsOld TestReloadListenerChangeRejected TestReloadPicksUpRotatedCert TestReloadRevertsRuntimeWeight TestReloadStorm' TAGS=integration` and `make integration`
- [ ] **Step 5:** commit `feat(main): hot reload with next-MAIL semantics`

### Task 4: Timeout budget audit

**Files:**
- Create: `test/integration/timeouts_test.go`

- [ ] **Step 1:** failing table test `TestTimeoutBudgetTable` — one row per PROJECT.md timer, each with a hostile fixture and the EXACT expected client-visible outcome (shortened test durations via config): client idle → `421 4.4.2`+close; first-command wait → 421; backend connect exhausted → `451 4.4.1`; handshake hang → 451 4.4.1; MAIL reply hang → `451 4.4.2`; 354 hang → 451 4.4.2; client DATA stall → `421 4.4.2`+close; backend DATA-write stall → discard-to-dot → `451 4.4.2`; dot-reply hang → `451 4.4.2` + duplicate-risk log; session lifetime → 421. Assert reply text comes from replies.go enum (byte-compare)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** fix every gap the table finds (this task exists because at least one timer is always mis-wired)
- [ ] **Step 4:** `make verify-new PKG=./test/integration TESTS='TestTimeoutBudgetTable' TAGS=integration
- [ ] **Step 5:** commit `test: full timeout budget audit`

### Task 5: Chaos suite completion

**Files:**
- Create: `test/chaos/chaos_test.go` (tag chaos, goleak TestMain; event-hook-driven, NO sleeps)

Scenarios already covered elsewhere are CUT, not re-tested: reload-under-load = Task 3's `TestReloadStorm`; graceful-shutdown = Task 2's drain tests; dot-stuffing = epic-05's `TestDotStuffingIntegrity`. Single-session behaviors from epics 03/05 reappear below ONLY with a stated added dimension (concurrency/interleaving) — that dimension is the test's assertion target.

- [ ] **Step 1:** failing named tests: `TestChaosAllBackendsDown` (dimension over epic-05: 10 concurrent client connections during the outage, all get per-transaction 451s, ALL survive and recover when one fake returns), `TestChaosBackendFlapDuringTraffic` (health flaps via SetDown/SetUp under load; zero lost/misrouted messages), `TestChaosBackendClosesBetweenTransactions` (idle backend closed after msg N; next MAIL transparently re-dials — R3), `TestChaosBackendDropMidDATA` (dimension: RST mid-body on 10 concurrent sessions while probes run; every session gets its own single 451, no cross-session interference), `TestChaosBackendHangAtEOD` (no reply after dot → timeout, 451, backend marked suspect), `TestChaosBackend421Between` (backend 421s between transactions; not relayed, leg replaced), `TestChaosTLSFailOneBackend` (STARTTLS fails on B only; B ejected, A/C carry traffic), `TestChaosClientDropMidDATA` (dimension: 10 concurrent client aborts; every backend leg aborted cleanly without dots, goleak), `TestChaosClientQUITMidDATAWait` (QUIT bytes while awaiting EOD reply), `TestChaosHealthProbeRacesTraffic` (probe + transaction on a backend with concurrency 1), `TestChaosSlowLorisClient` (1 B/s in DATA; timeout enforced, neighbors unaffected), `TestChaosSlowBackendDrip` (reply dripped byte-wise; reply timeout), `TestChaosPipelinedFailover` (client pipelines MAIL/RCPT/DATA; backend dies after accepting MAIL), `TestChaosAllRCPTRejected` (every RCPT 5xx → DATA relayed, backend's own 503/554 verdict verbatim — covered nowhere else), `TestChaosMultilineEverything` (multiline greeting/EHLO/replies + delay near client timeout), `TestChaosHalfOpenBackend` (accepts TCP, never sends banner; banner timeout + health verdict), `TestChaosRecoveryStorm` (all backends recover simultaneously; probes jittered, traffic redistributes), `TestChaosClientProtocolViolations` (dimension over epic-03: violations interleaved across concurrent sessions under load; correct 5xx each, no crash, no cross-session corruption)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement scenario by scenario; every failure found upstream gets fixed in its module with a unit regression test there
- [ ] **Step 4:** `make verify-new PKG=./test/chaos TESTS='TestChaosAllBackendsDown TestChaosBackendFlapDuringTraffic TestChaosBackendClosesBetweenTransactions TestChaosBackendDropMidDATA TestChaosBackendHangAtEOD TestChaosBackend421Between TestChaosTLSFailOneBackend TestChaosClientDropMidDATA TestChaosClientQUITMidDATAWait TestChaosHealthProbeRacesTraffic TestChaosSlowLorisClient TestChaosSlowBackendDrip TestChaosPipelinedFailover TestChaosAllRCPTRejected TestChaosMultilineEverything TestChaosHalfOpenBackend TestChaosRecoveryStorm TestChaosClientProtocolViolations' TAGS=chaos` (all 18 scenario names checked — a bare `make chaos` exits 0 on missing tests), then `make chaos` green ×3 consecutive runs
- [ ] **Step 5:** commit `test(chaos): full named scenario suite`

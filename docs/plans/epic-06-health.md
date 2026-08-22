# Epic 06: Health Checker

> Part of **smtp-balancer** — read `/PROJECT.md` first. Depends on epics 00–02, 04 (prober reuses `internal/backend`), 01 (fakesmtp).
> One task per fresh-context agent session, in order. TDD; commit after each task.

## Overview

**Goal:** `internal/health` — HAProxy-semantics active checking for SMTP backends: probe ladder, rise/fall state machine with interval table, jittered per-server scheduler, passive transport signals with active-only recovery, admin states, and the capability-superset verdict feeding the router.

**Produces:**
```go
// internal/health
type Checker struct{}
func New(cfg *config.Holder, clk Clock, lg *slog.Logger) *Checker   // Clock injectable for tests
func (c *Checker) Run(ctx context.Context)                           // schedulers; returns after ctx cancel + drain
func (c *Checker) Status(pool, server string) Status                 // {Op: UP|DOWN, Admin: READY|DRAIN|MAINT, Override: AUTO|FORCE_UP|FORCE_DOWN, Incompatible bool, ConsecFail, ConsecOK int}
func (c *Checker) Eligible(pool, server string) bool                 // the router's single question: UP && READY && !Incompatible (or FORCE_UP)
// admin plane (epic 09 exposes over HTTP):
func (c *Checker) SetAdminState(pool, server string, st AdminState) error
func (c *Checker) SetOverride(pool, server string, ov Override) error
// passive signals (relay calls these — implements proxy.HealthSignals):
func (c *Checker) DialFailure(srv *config.Server)
func (c *Checker) TransportError(srv *config.Server)   // mid-txn drop, reply timeout, fresh-conn 421, backend TLS failure
func (c *Checker) Success(srv *config.Server)
```
**FSM parameters:** configurable via `CheckParams`: interval 5s, down_interval 15s, rise 2, fall 3, timeout 5s (per-step; whole-probe budget = 3×timeout). **Package constants, NOT config knobs** (documented in code): `fastinter = 1s` (probe cadence while transitioning/unchecked), `errorLimit = 10` (passive events → one synthetic failed check + fastinter), `initState = UP` (servers start selectable; the first failed probe downs them — no `fully_down` option in v1). Scheduling: initial offset spread over min(interval, 5s), ±5% per-interval jitter. **Invariant: passive events only push DOWN-ward/accelerate; recovery requires `rise` consecutive ACTIVE successes.**

**Probe ladder:** **L0 connect — plain TCP, ZERO SMTP bytes (k8s `tcpSocket`-style): dial succeeds → healthy, socket closed immediately without reading the banner.** Trade-off documented in code + operations.md: an L0 close on an SMTP port makes MTAs log "lost connection after CONNECT" — that noise is why L1+ exist and why L0 pairs naturally with the port override below (probe a dedicated health socket instead of port 25) · L1 banner (expect 220, QUIT politely) · **L2 ehlo (default; harvests capability set → superset verdict vs the single v1 listener's advertised set — one referent by config validation, with the STARTTLS and SIZE-0 carve-outs from PROJECT.md's capability policy)** · L3 deep (`MAIL FROM:<>` → `RCPT TO:<probe_rcpt|postmaster>` expect 250/251 → RSET → QUIT; never DATA; off by default). **All levels honor `check.port` (k8s-style probe-port override; 0/unset = the server's traffic port)** — the probe dials `host-of(address):check.port`. Probe TLS mode and EHLO name default from the pool's `backend_tls`/`ehlo_name` (epic-02), so probes validate the real traffic path; capability harvesting/superset verdicts apply only when the probe actually speaks SMTP (L2+) — an L0/L1 or port-overridden probe never marks a server `Incompatible`.

## Module Usage

The router asks `Eligible` at every MAIL-time pick — health gates all traffic. The relay feeds passive signals from live transactions. The L2 probe doubles as the capability-superset enforcer (D10): a backend not advertising a superset of the listener's capability set is marked `Incompatible` and leaves rotation without a config change. Admin drain/maint (via epic 09's API) map to eligibility, not connection kills.

## Test Strategy

Units run on a fake clock (no sockets, no sleeps): counter math, interval table, jitter bounds, passive/active interplay, admin transitions. Integration runs against fakesmtp down-modes and scripts. Load: 100 fakes probed concurrently, goleak, intervals honored. Failure: DNS failure, probe-vs-reload race (generation token), half-open (accept-then-silence) distinct from refused.

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`
3. `make integration`

### Task 1: FSM core (fake clock)

**Files:**
- Create: `internal/health/fsm.go`, `internal/health/clock.go`, `internal/health/fsm_test.go`

- [ ] **Step 1:** failing tests: `TestFallMath` (fall-1 failures stay UP; fall-th → DOWN; interleaved success resets), `TestRiseMath` (symmetric), `TestIntervalTable` (exhaustive: steady-UP→interval, transitional-up/down→fastinter, unchecked→fastinter, steady-DOWN→down_interval), `TestInitStateUp` (servers start selectable; first failed probe downs them), `TestCountersNeverCorruptedByAdminChanges`
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement pure FSM (no goroutines) + injectable Clock
- [ ] **Step 4:** `make verify-new PKG=./internal/health TESTS='TestFallMath TestRiseMath TestIntervalTable TestInitStateUp TestCountersNeverCorruptedByAdminChanges'`
- [ ] **Step 5:** commit `feat(health): rise/fall fsm + interval table`

### Task 2: Prober ladder

**Files:**
- Create: `internal/health/probe.go`, `internal/health/probe_test.go` (integration tag for fake-backed ones)

- [ ] **Step 1:** failing tests (all integration-tagged, fake-backed): `TestProbeLadderLevels` (L0 vs L1 vs L2 vs L3 against compliant fake — all pass; each level's fake transcript shows exactly the expected commands, L1/L2/L3 end with QUIT), `TestProbeConnectNoSmtpBytes` (L0: DialCount increments, session transcript is EMPTY — zero bytes sent, closed after connect; and L0 never sets Incompatible), `TestProbePortOverride` (server address points at a dead port, `check.port` points at a live fake → probe passes; proves the override is honored and that traffic-port state is irrelevant to the probe — works for L0 and L2), `TestProbeConnectRefused` (SetDown ListenerClosed → failure labeled connect-refused), `TestProbeBannerTimeout` (AcceptThenHang → failure labeled banner-timeout, distinct from connect-refused), `TestProbeWrongBanner` (554 → fail), `TestProbeEhlo502FailsL2PassesL1` (via `Script.OnEHLO`), `TestProbeDeepRcpt450Fails` (greylist caveat documented in code comment), `TestProbeTLSMismatch` (fake requires TLS, probe_tls none → fail; probe_tls starttls → pass), `TestProbeHarvestsCapsAndSupersetVerdict` (`backend-missing-capability-marked-out`: fake missing 8BITMIME vs advertised set → Incompatible=true while op state stays per-probe-success)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement: **L0/L1 do NOT use `backend.Dial`** (Dial is an unconditional full handshake through EHLO + superset check) — L0 is a bare `net.Dialer.DialContext`; L1 dials, reads the greeting with `smtpwire.ReplyReader`, sends QUIT, closes (~20 lines, no new backend API). **L2/L3 reuse `backend.Dial`** (whose handshake IS the L2 probe; L3 continues with MAIL/RCPT/RSET on the returned Conn). Per-step + whole-probe budgets wrap all levels
- [ ] **Step 4:** `make verify-new PKG=./internal/health TESTS='TestProbeLadderLevels TestProbeConnectNoSmtpBytes TestProbePortOverride TestProbeConnectRefused TestProbeBannerTimeout TestProbeWrongBanner TestProbeEhlo502FailsL2PassesL1 TestProbeDeepRcpt450Fails TestProbeTLSMismatch TestProbeHarvestsCapsAndSupersetVerdict' TAGS=integration
- [ ] **Step 5:** commit `feat(health): probe ladder with capability harvest`

### Task 3: Scheduler (jitter, spread, generations)

**Files:**
- Create: `internal/health/scheduler.go`, `internal/health/scheduler_test.go`

- [ ] **Step 1:** failing tests: `TestJitterBounds` (10k scheduled intervals within ±5%; seeded rand), `TestInitialSpread` (N servers' first probes spread over min(interval,5s), not synchronized), `TestGenerationTokenDiscardsStaleResult` (probe in flight for a server removed by reload → result discarded, no state mutation, no panic), `TestSchedulerStopsOnCtxCancel` (all goroutines exit; goleak)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement goroutine-per-server with `time.Timer` (ponytail: central heap only if thousands of servers), config-generation token compare on completion
- [ ] **Step 4:** `make verify-new PKG=./internal/health TESTS='TestJitterBounds TestInitialSpread TestGenerationTokenDiscardsStaleResult TestSchedulerStopsOnCtxCancel'`
- [ ] **Step 5:** commit `feat(health): jittered per-server scheduler with reload generations`

### Task 4: Passive signals

**Files:**
- Create: `internal/health/passive.go`, `internal/health/passive_test.go`

- [ ] **Step 1:** failing tests: `TestPassiveEventsCount` (DialFailure/TransportError increment; Success resets), `TestPassiveNeverCounts4xx5xxVerdicts` (API shape makes it impossible — test documents: only the three methods exist), `TestErrorLimitFiresFailCheck` (10 consecutive → exactly one synthetic failed check + fastinter engaged), `TestPassiveCannotRecover` (DOWN server + 100 Success calls → still DOWN; rise active probes → UP — the anti-flap invariant), `TestPassiveIntegration` (integration: relay traffic against fake that starts refusing → server DOWN within computable bound without waiting for slow active cadence)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement
- [ ] **Step 4:** `make verify-new PKG=./internal/health TESTS='TestPassiveEventsCount TestPassiveNeverCounts4xx5xxVerdicts TestErrorLimitFiresFailCheck TestPassiveCannotRecover'` and `make verify-new PKG=./internal/health TESTS='TestPassiveIntegration' TAGS=integration
- [ ] **Step 5:** commit `feat(health): passive signals with active-only recovery`

### Task 5: Admin states + overrides

**Files:**
- Create: `internal/health/admin.go`, `internal/health/admin_test.go`

- [ ] **Step 1:** failing tests: `TestDrainExcludesFromEligibleProbesContinue`, `TestMaintStopsProbes` (scheduler pauses; idle probe conns closed), `TestForceDownBeatsProbeSuccess` (probes keep recording but verdict frozen), `TestForceUpBeatsProbeFailure`, `TestAutoResumes`, `TestAdminStateSurvivesReload` (same server identity keeps admin state through config swap; removed server's state dropped), `TestOverrideSurvivesReload` (force-up/force-down likewise survives config swap — the D15 reload-survival matrix)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement orthogonal {READY,DRAIN,MAINT} × {AUTO,FORCE_UP,FORCE_DOWN}; Eligible = (FORCE_UP or (UP and AUTO)) && READY && !Incompatible — write the truth table in a doc comment and test it exhaustively
- [ ] **Step 4:** `make verify-new PKG=./internal/health TESTS='TestDrainExcludesFromEligibleProbesContinue TestMaintStopsProbes TestForceDownBeatsProbeSuccess TestForceUpBeatsProbeFailure TestAutoResumes TestAdminStateSurvivesReload TestOverrideSurvivesReload'`
- [ ] **Step 5:** commit `feat(health): admin states and overrides`

### Task 6: Flap + load integration

**Files:**
- Create: `test/integration/health_test.go` (integration tag, goleak TestMain)

- [ ] **Step 1:** failing tests: `TestFlapScriptMatchesPredictedTransitions` (fake flips SetDown/SetUp on a script; with rise=2/fall=3 the recorded transition log equals the counter-math prediction exactly), `TestHundredBackendsProbeLoad` (100 fakes, interval 200ms, 30s run: goroutine count flat, probe cadence honored within jitter tolerance, zero cross-server interference from 20 hanging fakes — no head-of-line blocking), `TestDNSFailureCountsAsProbeFailure` (server address with unresolvable host → probe failure within budget; recovers when... use host that resolves after /etc/hosts-style injection — simulate via config swap to valid address)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement fixes the tests surface
- [ ] **Step 4:** `make verify-new PKG=./test/integration TESTS='TestFlapScriptMatchesPredictedTransitions TestHundredBackendsProbeLoad TestDNSFailureCountsAsProbeFailure' TAGS=integration` and `make integration`
- [ ] **Step 5:** commit `feat(health): flap determinism + 100-backend load proof`

# Epic 07: Router — Balance Algorithms + Rules

> Part of **Bifrost** — read `/PROJECT.md` first. Depends on epics 00–02, **05** (`proxy.TxnMeta` and the M1 integration test this epic rewires), 06 (Eligible gating).
> Import direction (no cycle): `internal/balance` imports `internal/proxy` for `TxnMeta`; `internal/proxy` never imports `internal/balance` — the relay receives a `PickFunc`, wired in `cmd/bifrost`.
> One task per fresh-context agent session, in order. TDD; commit after each task.

## Overview

**Goal:** `internal/balance` — the R1 core: rule evaluation (which pool) and server selection (which servers, in what order) producing the ordered candidate list the relay walks.

**Produces:**
```go
// internal/balance
type Router struct{}
func NewRouter(cfg *config.Holder, elig EligibleFunc, rnd *rand.Rand /*seedable*/) *Router
type EligibleFunc func(pool, server string) bool          // health.Checker.Eligible
func (r *Router) Pick(tx proxy.TxnMeta) ([]*config.Server, error)
// Pick = rule-match → pool → eligible-filter → algorithm primary pick → weighted-shuffled rest → backups tier.
// error only when pool empty/no default (config prevents); empty list when nothing eligible → relay synthesizes 451 4.4.1.

// in-flight accounting (leastconn input + max_transactions in epic 08):
func (r *Router) Lease(srv *config.Server) func()          // returns release; increments in-flight
func (r *Router) InFlight(pool, server string) int
```
Algorithms: **smooth weighted round-robin** (nginx algorithm: current += weight; pick max; current -= total) and **leastconn** (fewest in-flight transactions, weight-adjusted: inflight/weight, ties by WRR). `weight = 0` = config-level drain (never picked while >0-weight eligible servers exist).

## Module Usage

The relay calls `Pick` once per transaction at MAIL FROM with `TxnMeta{ClientIP, Helo, MailFromDomain}` and walks the list on dial failure (epic 05 Task 2). Rules are evaluated in config file order; first match wins; both keys in one rule = AND; no match → default_pool. Counters from `Lease` feed leastconn now and `max_transactions` in epic 08.

## Test Strategy

Pure functions with seeded rand — fully table-driven. Conditions: exact rule-precedence tables; wildcard domains; WRR interleaving sequences. Load: distribution property over 10k picks ≈ weights ±2%. Failure: all-ineligible → empty; backup tier engaged only when zero primaries eligible; and the named regression `dead-primary-skipped-with-no-added-latency` is proven HERE (Task 5's `TestDeadPrimarySkippedNoDial`: a DOWN server receives **zero dial attempts** during traffic — note epic-05's failover tests prove the opposite property, recovery-by-dialing, so this epic owns the no-dial assertion).

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`

### Task 1: Smooth weighted round-robin

**Files:**
- Create: `internal/balance/wrr.go`, `internal/balance/wrr_test.go`

- [x] **Step 1:** failing tests: `TestWRRInterleaving` (weights 5/1/1 produce the canonical smooth sequence AABABAA-style — assert exact nginx-algorithm output for 3 documented weight sets), `TestWRRSingleServer`, `TestWRRZeroWeightSkipped` (`weight=0` never picked while others exist; picked alone? no — zero-weight-only pool → empty pick), `TestWRRDistribution` (10k picks, weights 3/2/1 → counts within ±2%)
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement (per-pool state, mutex-guarded; state survives config-swap for unchanged servers — key by pool+server name)
- [x] **Step 4:** `make verify-new PKG=./internal/balance TESTS='TestWRRInterleaving TestWRRSingleServer TestWRRZeroWeightSkipped TestWRRDistribution'`
- [x] **Step 5:** commit `feat(balance): smooth weighted round-robin`

### Task 2: Leastconn + in-flight accounting

**Files:**
- Create: `internal/balance/leastconn.go`, `internal/balance/lease.go`, tests in `internal/balance/leastconn_test.go`

- [x] **Step 1:** failing tests: `TestLeaseRelease` (in-flight counters correct under 100 concurrent goroutines — race-clean), `TestLeastconnPicksFewest` (weight-adjusted: inflight 2/w2 beats 1/w1... assert documented formula inflight/weight with WRR tiebreak), `TestLeastconnDegradesToWRRWhenIdle` (all zero in-flight → WRR order — documented behavior)
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement
- [x] **Step 4:** `make verify-new PKG=./internal/balance TESTS='TestLeaseRelease TestLeastconnPicksFewest TestLeastconnDegradesToWRRWhenIdle'`
- [x] **Step 5:** commit `feat(balance): leastconn on in-flight transactions`

### Task 3: Ordered failover candidates

**Files:**
- Create: `internal/balance/candidates.go`, tests in `internal/balance/candidates_test.go`

- [x] **Step 1:** failing tests: `TestCandidatesOrder` (primary pick first, remaining primaries weighted-shuffled after, backups last; seeded rand → deterministic assertion), `TestCandidatesFilterIneligible` (ineligible excluded entirely), `TestBackupTierOnlyWhenNoPrimaries` (backups appear in tail always but lead ONLY when zero eligible primaries), `TestCandidatesEmptyWhenAllIneligible`
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement (Mireka Upstream semantics + health gating)
- [x] **Step 4:** `make verify-new PKG=./internal/balance TESTS='TestCandidatesOrder TestCandidatesFilterIneligible TestBackupTierOnlyWhenNoPrimaries TestCandidatesEmptyWhenAllIneligible'`
- [x] **Step 5:** commit `feat(balance): ordered failover candidate list`

### Task 4: Rule engine

**Files:**
- Create: `internal/balance/rules.go`, tests in `internal/balance/rules_test.go`

- [x] **Step 1:** failing tests: `TestRuleFirstMatchWins` (ordered rules; earlier match shadows later), `TestRuleClientCIDR` (v4+v6 CIDRs; netip-based), `TestRuleMailFromDomain` (exact match; case-insensitive), `TestRuleWildcardDomain` (`*.news.example.com` matches `a.news.example.com` and `a.b.news.example.com`, NOT `news.example.com` — document choice), `TestRuleANDSemantics` (both keys present → both must match), `TestRuleDefaultPool` (no match → default), `TestRuleEmptyMailFrom` (`MAIL FROM:<>` → domain "" matches only default unless a rule lists `""`? decision: null sender never matches domain rules — document + test)
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement (domain extraction from raw MAIL args happens in relay; here pure matching on TxnMeta)
- [x] **Step 4:** `make verify-new PKG=./internal/balance TESTS='TestRuleFirstMatchWins TestRuleClientCIDR TestRuleMailFromDomain TestRuleWildcardDomain TestRuleANDSemantics TestRuleDefaultPool TestRuleEmptyMailFrom'`
- [x] **Step 5:** commit `feat(balance): ordered rule engine`

### Task 5: Router facade + relay wiring

**Files:**
- Create: `internal/balance/router.go`, tests in `internal/balance/router_test.go`; Modify: `test/integration/m1_test.go` (swap stub PickFunc for real Router); Modify: `internal/proxy/relay.go` (populate `TxnMeta.MailFromDomain` from the raw MAIL line — this is where the real parsing lives)

- [x] **Step 1:** failing tests: `TestRouterPickEndToEnd` (config with 2 pools + rules → right pool, right ordering, config-swap picks up new weights at next Pick), `TestM1WithRealRouter` (M1 distribution test now through Router with weights 1/1 → ~10/10 of 20 messages; then weights 3/1 → ~15/5 ±2), `TestMailFromDomainRoutingEndToEnd` (a REAL `MAIL FROM:<user@promo.example.com>` sent through session+relay+router lands in the domain-rule pool, a different domain lands in default_pool — proves the relay actually populates TxnMeta.MailFromDomain; without this the R1 domain-routing feature can ship inert), `TestDeadPrimarySkippedNoDial` (`dead-primary-skipped-with-no-added-latency`: two fakes, A marked ineligible via the EligibleFunc; N transactions → `fakeA.DialCount()==0`, all N landed on B — the no-dial half of the named regression)
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement facade; extract MailFromDomain parsing in relay (`MAIL FROM:<user@dom> params` → `dom`, lowercased; malformed → "" and relay untouched — backend enforces syntax)
- [x] **Step 4:** `make verify-new PKG=./internal/balance TESTS='TestRouterPickEndToEnd'` and `make verify-new PKG=./test/integration TESTS='TestM1WithRealRouter TestMailFromDomainRoutingEndToEnd TestDeadPrimarySkippedNoDial' TAGS=integration` and `make integration`
- [x] **Step 5:** commit `feat(balance): router facade wired into relay (M1 on real router)`

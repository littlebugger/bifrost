# Epic 08: Limits & Backpressure

> Part of **Bifrost** — read `/PROJECT.md` first. Depends on epics 05, 07.
> One task per fresh-context agent session, in order. TDD; commit after each task.

## Overview

**Goal:** enforce the two capacity limits with the contract's exact replies: per-server `max_transactions` (skip-at-selection; all-saturated → `451 4.3.2`, session survives) and `global_maxconn` (accept-time `421 4.3.2` + close), plus accept-loop resilience.

**Consumes:** `balance.Lease/InFlight` (07) — reaching the relay ONLY through the `proxy.LeaseFunc` closure defined in epic-05 (`internal/proxy` must never import `internal/balance`; `router.Lease` is wired into `NewRelay` as the LeaseFunc in `cmd/bifrost`, epic-10); `proxy.Relay` (05); `replies.go` enum.
**Produces:** `internal/proxy/listener.go` — `Serve(ctx, ln, cfg, relay)` accept loop with maxconn gate and error backoff; `balance.Router.Pick` respecting `max_transactions`.

## Module Usage

`cmd/bifrost` calls `Serve` per configured listener. The 451-vs-421 split here is load-bearing for R3: saturation must NOT kill long-lived client connections — only refuse the current transaction.

## Test Strategy

Conditions: below-cap traffic unaffected. Load: cap boundary exact (cap, cap+1); counters converge to zero after drain. Failure: accept-loop EMFILE simulation (inject failing listener), saturated-then-released flow, MaxconnExhaustion chaos scenario.

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`
3. `make integration`

### Task 1: max_transactions at selection

**Files:**
- Modify: `internal/balance/candidates.go`; tests in `internal/balance/maxtxn_test.go`

- [x] **Step 1:** failing tests: `TestMaxTransactionsSkipsSaturated` (server at cap excluded from candidates like unhealthy), `TestMaxTransactionsZeroMeansUnlimited`, `TestSaturationReleasedRestoresEligibility`
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement in candidate filter using `InFlight` vs resolved `MaxTransactions`
- [x] **Step 4:** `make verify-new PKG=./internal/balance TESTS='TestMaxTransactionsSkipsSaturated TestMaxTransactionsZeroMeansUnlimited TestSaturationReleasedRestoresEligibility'`
- [x] **Step 5:** commit `feat(limits): per-server max_transactions gate`

### Task 2: All-saturated → 451, lease lifecycle in relay

**Files:**
- Modify: `internal/proxy/relay.go`; tests in `internal/proxy/saturation_test.go` (integration)

- [x] **Step 1:** failing tests: `TestAllSaturated451SessionSurvives` (pool of one server cap=1; hold one transaction open (fake Hang on EOD); second connection's MAIL → `451 4.3.2`; first completes; second retries MAIL → succeeds), `TestLeaseReleasedOnEveryPath` (release fires on: verdict relayed, RSET detach, backend death, client abort, QUIT — assert InFlight==0 after each; goleak)
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement: relay calls its injected `LeaseFunc` at successful attach and defers the returned release on every detach path (NO direct `internal/balance` import in relay.go — the closure is the boundary); empty-because-saturated distinguished from empty-because-unhealthy only in metrics label (same 451 4.3.2 vs 4.4.1 per contract — saturated uses 4.3.2)
- [x] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestAllSaturated451SessionSurvives TestLeaseReleasedOnEveryPath' TAGS=integration
- [x] **Step 5:** commit `feat(limits): saturation 451 + airtight lease lifecycle`

### Task 3: global_maxconn accept gate

**Files:**
- Create: `internal/proxy/listener.go`, tests in `internal/proxy/listener_test.go` (integration)

- [x] **Step 1:** failing tests: `TestMaxconnBoundary` (maxconn=2: two sessions live; third accepted then immediately `421 4.3.2` + close; after one QUIT, fourth connects fine), `TestAcceptErrorBackoff` (injected listener whose Accept fails N times → loop retries with 5ms..1s backoff, never exits, recovers), `TestServeStopsOnCtxCancel` (listener closed, Accept's net.ErrClosed treated clean; goleak)
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement `Serve` (session WaitGroup owned here — epic 10's drain uses it)
- [x] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestMaxconnBoundary TestAcceptErrorBackoff TestServeStopsOnCtxCancel' TAGS=integration
- [x] **Step 5:** commit `feat(limits): global maxconn accept gate + backoff`

### Task 4: Saturation chaos

**Files:**
- Create: `test/chaos/saturation_test.go` (tag chaos, goleak TestMain)

- [x] **Step 1:** failing test `TestChaosMaxconnExhaustion` (scenario 11: C=20 clients vs maxconn=10, per-server cap 2 on 2 fakes with scripted EOD delays; assert: exactly-at-cap concurrency observed by fakes (never above), every client either completes or gets clean 421/451 per contract, counters zero and goroutines baseline after)
- [x] **Step 2:** run: fail
- [x] **Step 3:** fix what it surfaces
- [x] **Step 4:** `make verify-new PKG=./test/chaos TESTS='TestChaosMaxconnExhaustion' TAGS=chaos`
- [x] **Step 5:** commit `test(chaos): maxconn exhaustion scenario`

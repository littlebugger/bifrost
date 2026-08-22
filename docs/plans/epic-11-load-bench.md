# Epic 11: Load Generator & Performance Gates

> Part of **smtp-balancer** — read `/PROJECT.md` first. Depends on epics 01, 10.
> One task per fresh-context agent session, in order. TDD; commit after each task.

## Overview

**Goal:** `cmd/loadgen` (+ `cmd/fakesmtp`), the `make load-smoke` gate, and the soak/nightly load runs with ratio-based pass thresholds (M3 exit).

**Produces:**
- `cmd/loadgen`: `-addr`, `-c` connections, `-m` messages per long-lived connection, `-rate` msgs/s (paced by `time.Ticker`), `-size` body bytes, `-pipeline`, `-direct` (baseline mode against a fake directly), JSON out: `{sent, errors, p50_ms, p95_ms, p99_ms, max_ms}` — percentiles are stdlib (`slices.Sort` + index over the collected latency slice; ≤100k samples ≈ 800 KB, no histogram library needed).
- `cmd/fakesmtp`: flags `-listen`, `-caps` (comma list), `-delay` (per-reply) — a default-250 standalone fake; anything needing a real `Script` stays an in-process Go test against `internal/fakesmtp` (a JSON Script format is unimplementable anyway: `Script.TLS *tls.Config` doesn't marshal).
- `make load-smoke`: starts balancer + 2 fakes, runs loadgen direct then through-proxy, asserts gates, <30 s total.

**Gates (from research; constants pinned after first CI benchmark):** proxy p99 ≤ 2 × direct p99 + 2 ms; zero errors at C=50, M=200, rate=500/s; goroutines return to baseline ±2; HeapAlloc after GC < 64 MiB and non-increasing across 3 rounds. Load-smoke retries once before failing (CI noise policy).

## Module Usage

R3's throughput story is proven here: long connections × many messages through per-transaction balancing at load. The ratio-vs-direct methodology keeps gates meaningful on shared runners.

## Test Strategy

The loadgen itself is table-tested (histogram math, rate pacing). Load: the gates ARE the tests. Failure: loadgen must report (not hang on) balancer refusals (421/451 counted as typed errors). Soak: 10k sessions; nightly C=100×M=1000.

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`
3. `make load-smoke`

### Task 1: loadgen + standalone fake

**Files:**
- Create: `cmd/loadgen/main.go`, `cmd/loadgen/run.go`, `cmd/loadgen/run_test.go`, `cmd/fakesmtp/main.go`

- [ ] **Step 1:** failing tests: `TestLoadgenAgainstFake` (in-process: C=4, M=10 against fakesmtp → sent=40, errors=0, percentiles populated and ordered p50≤p95≤p99≤max), `TestLoadgenCountsRefusals` (fake scripted 451s → typed error counts, no hang), `TestLoadgenRatePacing` (rate=100 → duration ≈ expected ±20%), `TestPercentileMath` (unit: known latency slice → exact p50/p95/p99), `TestFakesmtpBinaryFlags` (os/exec cmd/fakesmtp with -caps/-delay; smtpdrv talks to it)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement (reuse `internal/smtpdrv`; stdlib only — no new deps)
- [ ] **Step 4:** `make verify-new PKG=./cmd/loadgen TESTS='TestLoadgenAgainstFake TestLoadgenCountsRefusals TestLoadgenRatePacing TestPercentileMath TestFakesmtpBinaryFlags'`
- [ ] **Step 5:** commit `feat(loadgen): client load generator + standalone fake`

### Task 2: load-smoke gate

**Files:**
- Create: `scripts/load_smoke.sh`; Modify: `Makefile` (replace placeholder)

- [ ] **Step 1:** write `load_smoke.sh`: build; start 2 `cmd/fakesmtp`; run loadgen `-direct` for baseline JSON; start balancer with generated minimal config; run loadgen through proxy (C=50 M=200 rate=500 size=4096); assert gates with jq-free awk/python3 one-liner; retry whole run once on gate failure; kill everything on exit trap
- [ ] **Step 2:** `make load-smoke` exits 0 locally; deliberately break a gate (set budget to 0ms) → exits 1 → restore
- [ ] **Step 3:** wire into CI nightly job + PR-label-gated run
- [ ] **Step 4:** commit `feat(ci): load-smoke ratio gate`

### Task 3: Soak + memory ceiling

**Files:**
- Create: `test/integration/soak_test.go` (tag integration; skipped unless `-short=false` && env SOAK=1)

- [ ] **Step 1:** failing tests: `TestSoak10kSessions` (10k sequential+parallel short sessions; NumGoroutine flat ±2 vs baseline; HeapAlloc non-increasing across 3 GC'd checkpoints), `TestSoakLongConnection` (1 connection × 5k messages across 2 fakes — R3 at depth; distribution within tolerance; zero errors)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** fix leaks/retention it surfaces
- [ ] **Step 4:** `SOAK=1 make verify-new PKG=./test/integration TESTS='TestSoak10kSessions TestSoakLongConnection' TAGS=integration`
- [ ] **Step 5:** commit `test: soak + memory ceilings`

### Task 4: Nightly full load + docs

**Files:**
- Modify: `.github/workflows/ci.yml`; Create: `docs/performance.md`

- [ ] **Step 1:** nightly job: `load_smoke.sh` with C=100 M=1000 + soak env; upload JSON artifacts
- [ ] **Step 2:** `docs/performance.md`: methodology (ratio gates, why), current numbers table (filled by first nightly), tuning knobs (maxconn, timeouts, GOMAXPROCS note)
- [ ] **Step 3:** `make lint` clean; CI config valid (actionlint if available)
- [ ] **Step 4:** commit `feat(ci): nightly load + performance docs`

# Session-Affine Backend Reuse Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Opt-in per-pool backend connection reuse across a client session's envelopes, capped at `reuse_envelopes` per connection, with RSET revalidation, reuse/cap metrics, and a `conn_envelope` transaction-log field.

**Architecture:** A one-slot cache (`backendAffinity`) owned by each Session and threaded through Txn. Clean end-of-envelope detaches stash the conn instead of QUITing (below the cap); `attachAndRelay` reuses the cached conn when the pick's first candidate is the same server pointer, after an RSET revalidation. Everything else (balancing, leases, health, drain) unchanged.

**Tech Stack:** Go stdlib only. All in existing packages: internal/config, internal/proxy, internal/metrics.

**Spec:** `docs/superpowers/specs/2026-09-01-backend-reuse-design.md`

## Global Constraints

- No new module dependencies; stdlib only.
- Synthesized client replies come ONLY from `internal/proxy/replies.go` (ast audit) — this feature must NOT add any new client-facing reply.
- Commit messages: plain `type: subject` (hook rejects `type(scope):`).
- All work on branch `feat/backend-reuse`.
- After each task the named package tests pass; Tasks 5–6 run `go test ./...`.
- `gofmt` clean; comments match surrounding style; cite RFC 5321 3.8 where abort semantics matter.
- Per-transaction balancing (R3) must be untouched: a pick happens for every MAIL; reuse only when it lands on the cached server.

---

### Task 1: Config knob `reuse_envelopes`

**Files:**
- Modify: `internal/config/config.go` (Pool), `internal/config/rawschema.go`, `internal/config/load.go`, `internal/config/validate.go`
- Create: `internal/config/testdata/bad-reuse-negative.hcl`
- Test: `internal/config/load_test.go`, `internal/config/validate_test.go`

**Interfaces:**
- Produces: `Pool.ReuseEnvelopes int` (HCL `reuse_envelopes`, optional, default 0). Validation: negative → error summary `"reuse_envelopes out of range"`. 0 and 1 are valid (= disabled).

- [ ] **Step 1: Failing tests** — extend an existing auth/pool fixture load test (add `reuse_envelopes = 50` to `testdata/auth.hcl`'s pool and assert `Pool.ReuseEnvelopes == 50`); validate_test row for `bad-reuse-negative.hcl` (a minimal valid pool + `reuse_envelopes = -1`) expecting `"reuse_envelopes out of range"`.
- [ ] **Step 2: Verify RED** — `go test ./internal/config/ -run 'TestLoadAuth|TestValidateDiagnostics' -v` (unknown attribute → strict-decode failure).
- [ ] **Step 3: Implement** — rawPool gains `ReuseEnvelopes *int \`hcl:"reuse_envelopes,optional"\`` (mirror how other optional pool ints are declared — read how `max_transactions` flows raw→resolved and copy that idiom, range capture included); validate rule beside the weight/max_transactions numeric checks.
- [ ] **Step 4: GREEN** — `go test ./internal/config/ -v`.
- [ ] **Step 5: Commit** — `git commit -am "feat: add reuse_envelopes pool knob"`

### Task 2: Reuse metrics

**Files:**
- Modify: the proxy `Metrics` interface (find it: `grep -n "BackendDial" internal/proxy/*.go` — the interface, `noMetrics`, and `firstMetrics` live together), `internal/metrics` (prometheus implementation + registry)
- Test: the existing metrics package test file (mirror how `BackendDial`'s counter is asserted)

**Interfaces:**
- Produces: interface method `BackendReuse(server string, outcome string)`; prometheus counter `bifrost_backend_conn_reuse_total{server, outcome}`, outcome ∈ `"reused"` | `"capped"`; `noMetrics` no-op.

- [ ] **Step 1: Failing test** — metrics package: call `BackendReuse("s1", "reused")` twice and `("s1", "capped")` once, assert counter values via the registry the way the existing BackendDial test does.
- [ ] **Step 2: Verify RED** — method undefined.
- [ ] **Step 3: Implement** — one CounterVec, two label values; wire into the interface + noMetrics.
- [ ] **Step 4: GREEN** — `go test ./internal/metrics/ ./internal/proxy/ -v` (interface change must not break proxy).
- [ ] **Step 5: Commit** — `git commit -am "feat: add backend connection reuse metrics"`

### Task 3: Affinity cache + stash-on-clean-detach

**Files:**
- Modify: `internal/proxy/session.go` (Session field, Txn field, mail() handoff, Run defer), `internal/proxy/attach.go` (stash logic beside detach), `internal/proxy/relay.go` (reset()'s detach site), `internal/proxy/data.go` (verdict detach site), `internal/proxy/txnlog.go` (`conn_envelope` field)
- Test: `internal/proxy/relay_test.go`

**Interfaces:**
- Consumes: Task 1 `Pool.ReuseEnvelopes`, Task 2 `BackendReuse`.
- Produces:

```go
// session-owned, one slot; all access on the session goroutine.
type backendAffinity struct {
	c         *backend.Conn
	srv       *config.Server // pointer identity is the reuse key
	envelopes int            // envelopes this conn has carried
}

func (a *backendAffinity) closeIfAny() // Abort + clear; nil-safe

// Txn gains: affinity *backendAffinity (unexported; same package)
// txn gains a method used at the two clean detach sites:
func (t *txn) detachOrStash() // stash when eligible, else detach(true)
```

- [ ] **Step 1: Failing tests** (fakesmtp-driven, mirror relay_test.go's harness):
  1. `reuse_envelopes = 2`, one envelope completes → the fake's session stays open (no QUIT: assert `srv.DialCount() == 1` and the transcript has no QUIT event), and the session's next... (reuse itself is Task 4 — here assert only that the conn was NOT closed after the envelope).
  2. Session ends (client QUITs) → the cached conn is closed (fake observes disconnect; use the fake's session-closed observability the way existing tests detect drops).
  3. `reuse_envelopes = 0` regression: envelope completes → polite QUIT exactly as today (existing tests must not change).
- [ ] **Step 2: Verify RED** — `go test ./internal/proxy/ -run TestReuse -v`.
- [ ] **Step 3: Implement** — `detachOrStash()`: let k = the number of envelopes this conn has now carried (fresh dial's first envelope → k=1; tracked in the affinity slot and set on attach). Eligible to stash when `t.c != nil && !t.broken && pool.ReuseEnvelopes > 1 && k < pool.ReuseEnvelopes` (pool = live `poolFor(t.cfg, t.srv)`); at `k == pool.ReuseEnvelopes` → `t.r.metrics.BackendReuse(srvName(t.srv), "capped")` then `detach(true)`; ineligible for any other reason → plain `detach(true)`. Stashing = `untrackLeg`, `sig.Success`, `release()`, store into `tx.affinity`, clear `t.c/t.srv/t.release`. Call sites: `data.go:94` (`t.detach(body.delivered)` → `if body.delivered { t.detachOrStash() } else { t.detach(false) }`) and `reset()`'s `t.detach(true)`. The EHLO/HELO/QUIT site in `command()` stays `t.detach(true)`. Envelope counting: increment when a conn attaches for an envelope (fresh dial = 1; set `t.record.connEnvelope`). Session: `affinity backendAffinity` field, `tx.affinity = &s.affinity` in mail(), `defer s.affinity.closeIfAny()` in Run. txnlog: emit `conn_envelope` when > 0.
- [ ] **Step 4: GREEN** — `go test ./internal/proxy/ -v` (whole package: no existing detach-behavior regressions).
- [ ] **Step 5: Commit** — `git commit -am "feat: stash clean backend legs for session-affine reuse"`

### Task 4: Reuse path in attachAndRelay

**Files:**
- Modify: `internal/proxy/attach.go`
- Test: `internal/proxy/relay_test.go`

**Interfaces:**
- Consumes: Task 3's `tx.affinity`, Task 2's metrics.
- Produces: reuse attempt before the candidate walk in `attachAndRelay`.

- [ ] **Step 1: Failing tests:**
  1. Happy path: `reuse_envelopes = 3`, three envelopes on one client session → `DialCount() == 1`, wire shows `RSET` before envelopes 2 and 3, all three MAIL verdicts relayed, `conn_envelope` reaches 3 (assert via the log record if the harness exposes it, else via metrics: `reused` counted twice).
  2. Cap rollover: `reuse_envelopes = 2`, three envelopes → `DialCount() == 2`, `capped` counted once, `reused` counted once.
  3. Dead cached conn: complete envelope 1, `srv.SetDown(...)`/stop the fake between envelopes, envelope 2 → transparent fresh dial attempt (client sees the normal outcome for a down backend — with a second healthy candidate configured, failover works; no extra client-visible error from the stale conn).
  4. Server mismatch: two-server pool where the pick moves (weight/roundrobin) → cached conn closed, fresh dial to the new server, no reuse metric.
  5. Lease denial on reuse: max_transactions=1 with a concurrent holder → cache retained (next envelope after release can still reuse) OR — if retaining proves awkward — closed; assert whichever the implementation does explicitly (spec prefers retained).
- [ ] **Step 2: Verify RED** — `go test ./internal/proxy/ -run TestReuse -v`.
- [ ] **Step 3: Implement** — at the top of `attachAndRelay`, after `t.candidates(...)`: if `tx.affinity` holds a conn and `candidates[0] == tx.affinity.srv` (pointer identity) and live `poolFor` gives `ReuseEnvelopes > 1` and `tx.affinity.envelopes < N`: send `RSET` on the cached conn (SetCommandClass MailRcpt, SendLine, read one reply — reuse the conn's reply reader the way relayBatch does but WITHOUT relaying to the client; any error or final code outside 2xx → `Abort` + clear cache + fall through, log at Debug, no health signal); then `t.r.lease(srv)` (nil → return conn to cache untouched, `sawSaturated = true`, fall through); then attach: `t.srv, t.c, t.release = ...`, `trackLeg`, record pool/server, `tx.affinity.envelopes++`, `t.record.connEnvelope = tx.affinity.envelopes`, clear the cache slot (the conn is now attached, not cached), `t.r.metrics.BackendReuse(srv.Name, "reused")`, `t.cw.reset()`, `relayBatch` — sharing the post-dial code path with the walk rather than duplicating it (extract a small helper if that keeps it DRY).
- [ ] **Step 4: GREEN** — `go test ./internal/proxy/ -v` and `go test -race ./internal/proxy/ -run TestReuse`.
- [ ] **Step 5: Commit** — `git commit -am "feat: reuse cached backend legs across envelopes"`

### Task 5: Integration test

**Files:**
- Create: `test/integration/reuse_test.go`

- [ ] **Step 1: Write tests** (reuse test/integration harness incl. auth_test.go's serveTLS):
  1. Full chain with pool auth + `reuse_envelopes = 3`: client sends three messages on one session → fake transcript shows exactly ONE `AUTH PLAIN` line (per connection, not per envelope), `RSET` between envelopes, `DialCount() == 1`.
  2. Kill the fake after envelope 1 (`SetDown`), restart, envelope 2 → delivered on a fresh dial, client saw only normal replies.
  3. `reuse_envelopes` omitted: two envelopes → `DialCount() == 2` (regression pin for the default).
- [ ] **Step 2: Run** — `go test -tags=integration ./test/integration/ -run TestReuse -v` → PASS (fix forward).
- [ ] **Step 3: Full suite** — `go test ./...` green.
- [ ] **Step 4: Commit** — `git commit -am "test: backend reuse integration coverage"`

### Task 6: Documentation

**Files:**
- Modify: `PROJECT.md`, `docs/operations.md`, `examples/bifrost.hcl`, `internal/proxy/attach.go` (header comment)

- [ ] **Step 1: PROJECT.md** — amend decision D4's row (fresh-per-transaction remains the default; session-affine reuse opt-in via `reuse_envelopes`, cap = freshness bound) and any prose asserting "fresh connection per message" (grep `fresh connection` / `D4`); note the leastconn caveat (cached idle conns hold no lease and are invisible to in-flight counts).
- [ ] **Step 2: attach.go header** — the file comment currently says "decision D4 is a fresh connection per message, so there is no pool to keep consistent and nothing to leak from one transaction to the next" — rewrite to describe the affinity slot and its ownership.
- [ ] **Step 3: operations.md** — a "Backend connection reuse" section: knob semantics (0/1 off, N>1 cap), RSET revalidation self-healing (no idle reaper — a ponytail-style ceiling note: backend idle timeouts surface as one silent re-dial), metrics names, `conn_envelope` log field, interaction with pool auth (AUTH once per connection).
- [ ] **Step 4: examples/bifrost.hcl** — commented `# reuse_envelopes = 50` with a one-line comment in the pool showcase; `go test ./internal/config/ -v` stays green.
- [ ] **Step 5: Gates** — `go test ./...` green; `gofmt -l internal cmd test` empty.
- [ ] **Step 6: Commit** — `git commit -am "docs: backend connection reuse"`

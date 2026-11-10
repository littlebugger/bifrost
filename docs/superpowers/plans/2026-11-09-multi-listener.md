# Multiple Listeners Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** N labeled `listener` blocks in one process — per-listener bind/hostname/STARTTLS/auth/capabilities, a `listener` routing key, per-transaction backend capability requirements from the arriving listener, prober requirements from the union of all listeners.

**Architecture:** Decode already handles N blocks (`rawFile.Listeners` is a slice); the work is the resolved model (`Config.Listeners` + implicit `"default"` name for a single unlabeled block), threading the arriving `*config.Listener` through Serve→Session→Txn→relay, one accept loop per listener in cmd/bifrost, and splitting the capability referent (per-txn = arriving listener; prober = union).

**Tech Stack:** Go stdlib only. Existing packages: internal/config, internal/proxy, internal/balance, internal/health, cmd/bifrost.

**Spec:** `docs/superpowers/specs/2026-11-09-multi-listener-design.md`

## Global Constraints

- No new module dependencies; stdlib only.
- Synthesized client replies come ONLY from `internal/proxy/replies.go` (ast audit) — this feature adds none.
- Commit messages: plain `type: subject` (hook rejects `type(scope):`).
- Branch: `feat/multi-listener`.
- Backward compat is a hard requirement: every existing config (one unlabeled listener) must load and behave byte-identically; the whole existing test suite is the regression gate and must stay green UNMODIFIED except where a test constructs `config.Config{Listener: ...}` literals (mechanical field rename only).
- `gofumpt -l .` clean (CI's make lint uses gofumpt); `go vet ./...` clean.
- After each task: the named packages' tests green; Tasks 6–8 run full `go test ./...`.

---

### Task 1: Config model — `Listeners []Listener` with names

**Files:**
- Modify: `internal/config/config.go`, `internal/config/rawschema.go`, `internal/config/load.go`, `internal/config/validate.go`, `internal/config/defaults.go` (capability resolution per listener), `internal/config/holder.go` (compile-only: field access), all config consumers' *field access* (`grep -rln 'cfg\.Listener\b\|\.Listener\.' internal cmd test` — mechanical swap, no behavior change)
- Create: `internal/config/testdata/multi-listener.hcl`, `internal/config/testdata/bad-dup-listener-name.hcl`, `internal/config/testdata/bad-dup-listener-bind.hcl`, `internal/config/testdata/bad-unlabeled-multi.hcl`
- Test: `internal/config/load_test.go`, `internal/config/validate_test.go`

**Interfaces:**
- Produces:

```go
// Listener gains: Name string (block label; "default" when the single
// block is unlabeled). Config.Listener (singular) is REPLACED by:
Listeners []Listener
// plus the transitional accessor every current consumer migrates to in
// this task (they all assume one listener until Tasks 3-4 thread the
// real one through):
func (c *Config) PrimaryListener() *Listener // &c.Listeners[0]
// RoutingRule gains: Listener []string `hcl:"listener,optional"` (decode
// only here; matching is Task 2).
```

- [x] **Step 1: Failing tests** — `multi-listener.hcl`: two labeled listeners (different binds/hostnames/caps; one with auth+starttls, one without), a rule with `listener = ["bulk"]`; assert `len(cfg.Listeners)==2`, names, per-listener fields, `cfg.Routing.Rules[0].Listener`. Compat: existing fixtures (auth.hcl etc.) assert `PrimaryListener().Name == "default"`. Validation rows: dup names → `"duplicate listener name"`; dup binds → `"duplicate listener bind"`; two unlabeled blocks → `"listener blocks require labels"`; rule referencing unknown listener → `"routing rule references unknown listener"`.
- [x] **Step 2: Verify RED** — `go test ./internal/config/ -run 'TestLoad|TestValidate' -v` (label decode fails first: rawListener needs an optional label — gohcl requires a label field for labeled blocks; decode BOTH shapes by trying labeled schema first, falling back — read how hclsimple handles optional labels; if HCL cannot express optional labels on one block type, decode the label via the block header ranges the way rawschema captures `attr_range`s, and note the chosen mechanism in the task report).
- [x] **Step 3: Implement** — resolved model + the mechanical consumer swap (`cfg.Listener.X` → `cfg.PrimaryListener().X`); validateListenerCount becomes the new rules (≥1 block always required); per-listener application of the existing listener-scoped validations (capabilities, starttls/auth pairing, SMTPUTF8, hostname control chars) — loop them, keep messages identical so existing validate_test rows pass unchanged.
- [x] **Step 4: GREEN** — `go test ./internal/config/ -v`, then compile-wide `go build ./...` and `go test ./internal/... ./cmd/...` (consumer swap must not change behavior anywhere).
- [x] **Step 5: Commit** — `git commit -am "feat: resolve multiple named listeners in config"`

### Task 2: Routing by listener

**Files:**
- Modify: `internal/proxy/relay.go` (TxnMeta), `internal/balance/rules.go`
- Test: `internal/balance/rules_test.go`

**Interfaces:**
- Consumes: Task 1 `RoutingRule.Listener []string`.
- Produces: `TxnMeta.Listener string`; `ruleMatches` gains the clause (AND semantics, empty key matches all):

```go
if len(rule.Listener) > 0 && !slices.Contains(rule.Listener, tx.Listener) {
	return false
}
```

- [x] **Step 1: Failing tests** — rules_test rows: listener-only rule matches/skips by name; listener+CIDR AND-combined; empty listener key matches any; first-match ordering with listener rules.
- [x] **Step 2: RED** — `go test ./internal/balance/ -run TestMatchPool -v`.
- [x] **Step 3: Implement** (the clause above, mirroring the two existing key checks at rules.go:28-33).
- [x] **Step 4: GREEN** — `go test ./internal/balance/ ./internal/proxy/ -v`.
- [x] **Step 5: Commit** — `git commit -am "feat: route transactions by arrival listener"`

### Task 3: Serve/Session/Txn threading + N accept loops

**Files:**
- Modify: `internal/proxy/listener.go` (Serve signature), `internal/proxy/session.go`, `internal/proxy/relay.go` (meta), `internal/proxy/attach.go` (dialOpts RequiredCaps), `internal/proxy/txnlog.go` (+`listener` field), `cmd/bifrost/wire.go`, `cmd/bifrost/main.go`, `cmd/bifrost/drain.go`
- Test: `internal/proxy/session_test.go`, `internal/proxy/relay_test.go`, `cmd/bifrost/main_test.go`

**Interfaces:**
- Consumes: Tasks 1–2.
- Produces:

```go
// proxy.Serve gains the listener it serves (cfg stays for defaults/limits):
func Serve(ctx context.Context, ln net.Listener, cfg *config.Config,
	lst *config.Listener, tlsCfg *tls.Config, h TxnHandler,
	lg *slog.Logger, metrics ...Metrics) error
// Session stores lst *config.Listener; every s.cfg.Listener-derived read
// (hostname, capabilities, auth store, 530 gate, AUTH advertisement)
// moves to s.lst. Txn gains lst *config.Listener (unexported, same pkg);
// relay: TxnMeta.Listener = tx.lst.Name; dialOpts RequiredCaps =
// t.tx.lst.Capabilities; txnlog gains listener=<name>.
// cmd/bifrost: app holds smtpLns []namedListener (name, net.Listener,
// *tls.Config, *config.Listener); main starts one accept loop per entry
// (start("smtp listener "+name, ...)); serveDone closes when ALL return
// (WaitGroup + one closer goroutine); drain closes every ln.
```

- [x] **Step 1: Failing tests** — proxy: a Session built on listener A uses A's hostname/caps/auth while B differs (two-listener config; drive two sessions; assert banners, EHLO sets, and that a user valid on A gets 535 on B); relay test asserting txn log carries `listener=` and that RequiredCaps enforcement is per-listener (fake missing a cap only listener B advertises: txn from A relays, txn from B gets the no-backend synth). cmd: main_test boots two listeners on :0 and SMTPs both ports.
- [x] **Step 2: RED** — `go test ./internal/proxy/ -run 'TestListener|TestSession' -v`.
- [x] **Step 3: Implement.** Session threading first (compile ripples through NewSession call sites incl. tests — update them mechanically); then cmd wiring: wire.go builds per-listener `net.Listener` + per-listener TLS cert load (today's single-cert load loops); drain.go closes all; the "bifrost listening" log prints each name=bind pair.
- [x] **Step 4: GREEN** — `go test ./internal/proxy/ ./cmd/... -v` and `go test ./...` (integration-untagged).
- [x] **Step 5: Commit** — `git commit -am "feat: serve sessions per named listener"`

### Task 4: Prober capability union

**Files:**
- Modify: `internal/config/config.go` (union helper), the prober's requiredCaps origin (`grep -rn "Capabilities" internal/health cmd/bifrost/wire.go` — update where the probe's required set is sourced)
- Test: `internal/config/config_test.go` (or the file the helper lands beside), `internal/health/probe_test.go`

**Interfaces:**
- Produces:

```go
// UnionCapabilities returns every capability advertised by any listener,
// deduped case-insensitively, with SIZE collapsed to the maximum
// required value (absent if no listener advertises it). STARTTLS passes
// through; checkSuperset already ignores it.
func (c *Config) UnionCapabilities() []string
```

- [x] **Step 1: Failing tests** — union helper: dedupe across listeners; SIZE max of {"SIZE 100","SIZE 900"} → "SIZE 900"; bare SIZE + numeric → bare wins? NO — bare SIZE means unlimited required = satisfied by anything, so numeric max wins for strictness; assert exactly `SIZE 900` given {"SIZE","SIZE 900"}. Probe test: two listeners where only B advertises 8BITMIME; backend without it → incompatible verdict.
- [x] **Step 2: RED**.
- [x] **Step 3: Implement** helper + swap the prober's source from the single listener's caps to the union (ONE call site change — find it in wire.go/health scheduling and name it in the report).
- [x] **Step 4: GREEN** — `go test ./internal/config/ ./internal/health/ -v`.
- [x] **Step 5: Commit** — `git commit -am "feat: health probes require the union of listener capabilities"`

### Task 5: Reload semantics per listener

**Files:**
- Modify: `internal/config/holder.go` (RestartRequired, BindChange if present)
- Test: `internal/config/holder_test.go`

- [x] **Step 1: Failing tests** — compare by listener NAME: changed hostname/capabilities/auth(incl. AllowCleartext)/starttls on listener "bulk" → warning naming "bulk"; listener added or removed → warning; bind changed on one listener → the existing bind rejection/warning path per current single-listener behavior (read holder.go's BindChange handling first and mirror it per listener).
- [x] **Step 2: RED** — `go test ./internal/config/ -run 'TestRestartRequired|TestBind' -v`.
- [x] **Step 3: Implement** — key both sets by name; reuse the existing field comparisons per pair; messages name the listener.
- [x] **Step 4: GREEN** — `go test ./internal/config/ -v`.
- [x] **Step 5: Commit** — `git commit -am "feat: per-listener reload warnings"`

### Task 6: Observability

**Files:**
- Modify: `internal/proxy/metrics.go` + `internal/metrics/metrics.go` (+test) — `Transaction(pool, server, class)` gains a leading `listener` label; `internal/admin/admin.go` `/stats` gains per-listener session counts (source: proxy session gauge keyed by listener — read how sessions are counted today and extend the same mechanism)
- Test: `internal/metrics/metrics_test.go`, `internal/admin/admin_test.go`

- [x] **Step 1: Failing tests** — metrics golden-list labels updated; Transaction records under the right listener label; /stats JSON shape with two listeners.
- [x] **Step 2: RED**. **Step 3: Implement** (noMetrics + prometheus + call sites in relay). **Step 4: GREEN** — `go test ./...`.
- [x] **Step 5: Commit** — `git commit -am "feat: label transactions and stats by listener"`

### Task 7: Integration test

**Files:**
- Create: `test/integration/multilistener_test.go`

- [x] **Step 1: Write tests** (reuse the m1/auth harness; Serve's new signature): (1) two listeners, two pools, two fakes — mail into port A lands only on fake A, port B only on fake B (listener routing end to end); (2) per-txn RequiredCaps: fake B missing a cap only listener B advertises → port-B mail gets 451 synth, port-A mail unaffected; (3) auth isolation: user valid on A → 235 on A, 535 on B.
- [x] **Step 2: Run** — `go test -tags=integration ./test/integration/ -run TestMultiListener -v` (note: check whether this harness file needs the integration tag by looking at its siblings).
- [x] **Step 3: Full suite** — `go test ./...` and `go test -race -count=1 -tags=integration ./internal/proxy/ ./test/integration/`.
- [x] **Step 4: Commit** — `git commit -am "test: multi-listener integration coverage"`

### Task 8: Documentation

**Files:**
- Modify: `PROJECT.md` (R1 note, capability policy's "single referent" paragraph → per-txn/union model, decision table v1-single-listener row), `docs/operations.md` (multi-listener section + routing key + reload caveats), `examples/bifrost.hcl` (stays single-listener as the fixture; commented second-listener sketch)

- [x] **Step 1–3: Edit the three docs**; verify `go test ./internal/config/ -v` (example fixture) stays green.
- [x] **Step 4: Gates** — `go test ./...`; `gofumpt -l internal cmd test` empty.
- [x] **Step 5: Commit** — `git commit -am "docs: multiple listeners"`

### Task 9: Feature config examples (user request 2026-11-10)

**Files:**
- Create: `examples/auth.hcl`, `examples/reuse.hcl`, `examples/dns-discovery.hcl`, `examples/multi-listener.hcl` — one UNCOMMENTED, minimal, load-valid config per feature (not sketches: every attribute live), each headed by a comment block naming the feature, what each knob does, and the operations.md section to read
- Create: `internal/config/examples_test.go` — `TestExamplesLoad`: globs `../../examples/*.hcl`, asserts each loads with zero error diagnostics (table over files; `examples/bifrost.hcl` keeps its own detailed test)
- Modify: `README.md` — a "Configuration" section after Install: the minimal config shape, then a table linking each `examples/*.hcl` to its feature and operations.md anchor
- Modify: `docs/operations.md` — each feature section gains one line "Working example: `examples/<file>.hcl`"

**Interfaces:**
- Consumes: everything shipped on the auth, reuse, dns-discovery, and multi-listener branches (multi-listener example must use the Tasks 1–3 syntax: labeled `listener "name" {}` blocks + `rule { listener = [...] }`).
- Constraint: example files reference certs as `server.crt`/`server.key` (relative to the file, like bifrost.hcl) and must pass validation INCLUDING the auth-requires-starttls and pool-auth-requires-TLS rules — so the auth example carries starttls + `backend_tls = "starttls"`; dns-discovery's `server_dns` address is a hostname (any; never resolved at load).

- [x] **Step 1: Failing test** — `TestExamplesLoad` written first; it fails on the not-yet-created files only if the glob is asserted non-empty (assert ≥ 5 files so missing examples fail loudly).
- [x] **Step 2: Write the four example files**; `go test ./internal/config/ -run TestExamplesLoad -v` to green — any validation error means the example is wrong, fix the example (never the validator).
- [x] **Step 3: README Configuration section + operations.md pointers.**
- [x] **Step 4: Gates** — `go test ./...`; `gofumpt -l internal` empty.
- [x] **Step 5: Commit** — `git commit -am "docs: per-feature config examples"`

# Epic 02: Config Engine (HCL)

> Part of **smtp-balancer** — read `/PROJECT.md` first. Depends on epic-00.
> One task per fresh-context agent session, in order. TDD; commit after each task.

## Overview

**Goal:** `internal/config` — HCL parsing with strict schema, semantic validation with line-precise diagnostics, `-c` check mode, and the atomic runtime holder used by hot reload (D2, D14).

**Produces:**
```go
// internal/config
func Load(path string) (*Config, hcl.Diagnostics)        // parse + decode + Validate
func (c *Config) Validate() hcl.Diagnostics               // semantic pass, range-anchored
type Holder struct{ p atomic.Pointer[Config] }            // runtime swap point
func (h *Holder) Load() *Config
func (h *Holder) Swap(c *Config) (old *Config)

type Config struct {
    Defaults Defaults    // timeouts + check defaults + default ehlo_name
    Listener Listener    // EXACTLY ONE in v1 (validation rejects more): bind, hostname, starttls{cert,key,min_version}, advertised capabilities
    Pools    []Pool
    Routing  Routing     // ordered rules (client_cidr[], mail_from_domain[]) → pool; default_pool
    Admin    Admin       // bind (tcp host:port or unix:///path); optional — absent = no admin plane, logged warning; AllowRemote bool (non-loopback TCP bind rejected unless true)
    Limits   Limits      // global_maxconn, per-defaults max_transactions override
}
type Pool struct {
    Name    string
    Balance string          // "roundrobin"|"leastconn"
    // backend-leg TRAFFIC settings (epic-04 Opts and epic-06 probes read these):
    BackendTLS string       // "none"|"starttls"|"starttls-verify"; default none
    BackendTLSServerName string; BackendTLSCA string // for starttls-verify
    EhloName string         // traffic EHLO identity; defaults: pool → defaults.ehlo_name → listener hostname
    Check   CheckParams     // overrides; Check.TLS/EhloName default from the pool's BackendTLS/EhloName
    Servers []Server
}
type Server struct { Name, Address string; Weight int; Backup bool; MaxTransactions int; Check CheckParams }
type CheckParams struct { Level string /*connect|banner|ehlo|deep*/; Port int /*probe port override, k8s-style; 0 = the server's traffic port*/; Interval, DownInterval, Timeout time.Duration; Rise, Fall int; EhloName, ProbeRcpt string; TLS string /*none|starttls|starttls-verify*/ }
// fastinter (1s), error_limit (10), init_state (up) are health-package CONSTANTS, not config — no fields here.
type Timeouts struct { /* every row of the PROJECT.md timeout table, same names — including lame_duck (2s) and drain_timeout (30s) */ }
```
HCL types (`hcl.Range`, cty) must NOT leak outside `internal/config` — public structs are plain Go.

**Reference config:** the annotated example from PROJECT.md research lives at `examples/smtp-balancer.hcl` and doubles as the primary test fixture.

## Module Usage

Every subsystem reads its knobs from `*config.Config` snapshots taken via `Holder.Load()` at well-defined points (session start; each MAIL FROM for routing). `-c` and SIGHUP reload run the identical `Load` path (write once). Line-precise diagnostics are an R1 deliverable: operators and agents both get `file:line:col` errors.

## Test Strategy

Conditions: full example decodes; defaults inheritance (defaults→pool→server) resolves documented precedence; zero-value defaults match PROJECT.md tables. Failure: fixture-per-error-class with EXPECTED DIAGNOSTIC (message substring + line number) — unknown attribute, missing pool ref, duplicate server name, bad CIDR, bad wildcard, inverted timeout hierarchy, SMTPUTF8-without-8BITMIME, weight out of range, unreadable cert. Load: `Holder` swap under `-race` with concurrent readers.

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`

### Task 1: Schema + strict decode

**Files:**
- Create: `internal/config/config.go`, `internal/config/load.go`, `internal/config/load_test.go`, `examples/smtp-balancer.hcl`, `internal/config/testdata/*.hcl`

- [ ] **Step 1:** write `examples/smtp-balancer.hcl` — the full annotated example: defaults (timeouts incl. lame_duck/drain_timeout + check + ehlo_name), ONE listener with starttls + capabilities `["PIPELINING","8BITMIME","SIZE 10485760","STARTTLS"]`, pools "internal" (roundrobin; mta1 w=3, mta2 w=1, spare backup with `check { level = "connect" port = 9025 }` — the plain-TCP / probe-port-override showcase; backend_tls = "none") and "bulk" (leastconn; max_transactions; `backend_tls = "starttls-verify"` with ca + server_name — the backend-TLS showcase), routing rules (client_cidr → internal; mail_from_domain incl. `*.news.example.com` → bulk; default_pool), limits block (global_maxconn), admin bind (loopback)
- [ ] **Step 2:** failing tests: `TestLoadExample` (decodes; spot-check every section's values INCLUDING pool backend_tls/ehlo_name resolution and the limits block), `TestLoadUnknownAttribute` (fixture with `wieght = 3` → diagnostic contains "wieght" and its line), `TestLoadMissingRequired` (server without address)
- [ ] **Step 3:** run: fail
- [ ] **Step 4:** implement structs + `gohcl.DecodeBody` (strict: unknown attrs/blocks are errors); add `github.com/hashicorp/hcl/v2` to go.mod
- [ ] **Step 5:** `make verify-new PKG=./internal/config TESTS='TestLoadExample TestLoadUnknownAttribute TestLoadMissingRequired'`
- [ ] **Step 6:** commit `feat(config): hcl schema + strict decode`

### Task 2: Semantic validation

**Files:**
- Create: `internal/config/validate.go`, `internal/config/validate_test.go`, fixtures `internal/config/testdata/bad-*.hcl`

- [ ] **Step 1:** failing table test `TestValidateDiagnostics` — one fixture per rule. Every rule below is stated as **reject when <condition>** (errors) or **warn when <condition>** (warnings; config still loads):
  - reject when: a rule references a nonexistent pool; duplicate pool names; duplicate server names within a pool; default_pool missing/nonexistent; bad CIDR; wildcard not of form `*.suffix`; weight <0 or >256; SIZE capability without a value; `SMTPUTF8` advertised without `8BITMIME` (RFC 6531); check level not in enum; check port set but outside 1–65535; rise/fall <1; backend_tls not in enum; `starttls-verify` without ca/server_name; starttls cert/key unreadable; admin bind malformed; **admin TCP bind not loopback and `allow_remote` unset** (`TestAdminBindNonLoopbackRejected`); **more than one `listener` block** ("one listener supported in v1"); empty pool (no servers); all-backup pool
  - reject when (timeout sanity, `config-rejects-inverted-timeouts`): any timeout ≤ 0; `check.timeout > check.interval`; `backend_connect > backend_mail_reply`. **No upper caps** — RFC 5321 §4.5.3.2 values are floors, not ceilings
  - warn when: any backend reply timeout is below its RFC 5321 §4.5.3.2 floor (documented deviation, default config intentionally triggers these warnings except the dot timer); admin block absent
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement `Validate()` emitting `hcl.Diagnostics` anchored to the original ranges (keep per-block `hcl.Range` during decode for anchoring)
- [ ] **Step 4:** `make verify-new PKG=./internal/config TESTS='TestValidateDiagnostics'`
- [ ] **Step 5:** commit `feat(config): semantic validation with range-anchored diagnostics`

### Task 3: Defaults inheritance

**Files:**
- Create: `internal/config/defaults.go`, `internal/config/defaults_test.go`

- [ ] **Step 1:** failing tests: `TestDefaultsToPoolToServer` (check params: server override > pool override > defaults > built-in; Check.TLS/EhloName default from pool BackendTLS/EhloName; pool EhloName defaults to defaults.ehlo_name, then listener hostname), `TestBuiltinDefaults` (omit everything optional → PROJECT.md defaults verbatim: interval 5s, down_interval 15s, rise 2, fall 3, check timeout 5s — valid because the invariant is `timeout ≤ interval` — level ehlo, check port 0 = traffic port, backend_tls none, timeouts table values incl. lame_duck 2s and drain_timeout 30s, global_maxconn 1024, max_transactions unlimited=0, weight 1), `TestBuiltinDefaultsPassValidation` (the resolved built-in defaults run through Validate() with zero error diagnostics — the defaults must never fail their own validator)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement resolution at Load time — resolved values stored in the returned structs (consumers never re-derive)
- [ ] **Step 4:** `make verify-new PKG=./internal/config TESTS='TestDefaultsToPoolToServer TestBuiltinDefaults TestBuiltinDefaultsPassValidation'`
- [ ] **Step 5:** commit `feat(config): inheritance resolution`

### Task 4: `-c` check mode

**Files:**
- Modify: `cmd/smtp-balancer/main.go`; Create: `cmd/smtp-balancer/main_test.go`

- [ ] **Step 1:** failing tests: `TestCheckModeOK` (`-c -f examples/smtp-balancer.hcl` → exit 0, prints "config OK"), `TestCheckModeBad` (bad fixture → exit 1, diagnostic on stderr with file:line) — use `os/exec` on the built binary or refactor main into testable `run(args) int`
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement (refactor main → `run()`)
- [ ] **Step 4:** `make verify-new PKG=./cmd/smtp-balancer TESTS='TestCheckModeOK TestCheckModeBad'`
- [ ] **Step 5:** commit `feat(config): -c check mode`

### Task 5: Runtime Holder (reload swap point)

**Files:**
- Create: `internal/config/holder.go`, `internal/config/holder_test.go`

- [ ] **Step 1:** failing tests: `TestHolderSwapVisibleToReaders` (N reader goroutines Load() in a loop; writer Swaps; readers only ever see complete old or complete new — run under `-race`), `TestHolderDiffSummary` (Swap returns old; a `DiffSummary(old,new) string` names added/removed/changed pools+servers for the reload log)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement with `atomic.Pointer[Config]`
- [ ] **Step 4:** `make verify-new PKG=./internal/config TESTS='TestHolderSwapVisibleToReaders TestHolderDiffSummary'`
- [ ] **Step 5:** commit `feat(config): atomic runtime holder + diff summary`

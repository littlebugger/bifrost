# DNS-Discovered Pool Members Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `server_dns "name" { address = "host:port", resolve_interval = "30s", ... }` pool templates that expand at runtime into per-IP members (`name-<ip>`) with full per-member health, balancing, metrics, and reuse — fail-static on resolver errors/empty answers.

**Architecture:** Discovery is an internal reload. A resolver loop per template; on a membership delta it builds a derived config snapshot (`config.ExpandDNS`) and swaps the holder — the relay, health checker (re-syncs on snapshot identity), and admin already honor swaps. Survivor members keep their `*config.Server` pointers across swaps (health-FSM and reuse-affinity continuity); the SIGHUP path re-renders through the same builder so file edits and membership compose.

**Tech Stack:** Go stdlib only (`net.Resolver`, `netip`). New package `internal/discover` (runtime loop); builder lives in `internal/config` (needs unexported range fields for cloning).

**Spec:** `docs/superpowers/specs/2026-11-09-dns-discovery-design.md`

## Global Constraints

- No new module dependencies; stdlib only. No real DNS in unit tests — the lookup is an injected `func(ctx, host) ([]netip.Addr, error)`.
- Fail-static is a hard rule: a lookup error or empty answer NEVER shrinks membership; it logs and counts.
- Survivor continuity is NAME-based (ruling 2026-11-09): health state persists across swaps via the checker's svrKey sync; ExpandDNS does a plain rebuild with fresh pointers, and member names must be deterministic and stable.
- Commit messages: plain `type: subject`. Branch: `feat/dns-discovery`.
- `gofumpt -l .` clean; `go vet ./...` clean. Tasks 5–7 run full `go test ./...`.

---

### Task 1: Config — `server_dns` templates

**Files:**
- Modify: `internal/config/config.go`, `internal/config/rawschema.go`, `internal/config/load.go`, `internal/config/validate.go`, `internal/config/defaults.go` (templates resolve Check/auth exactly like servers)
- Create: `internal/config/testdata/dns-template.hcl`, `internal/config/testdata/bad-dns-ip-literal.hcl`, `internal/config/testdata/bad-dns-interval.hcl`, `internal/config/testdata/bad-dns-dup-name.hcl`
- Test: `internal/config/load_test.go`, `internal/config/validate_test.go`, `internal/config/defaults_test.go`

**Interfaces:**
- Produces:

```go
// ServerDNS is a server template: one DNS name expanded at runtime into
// per-IP members. Field set mirrors Server minus Address semantics.
type ServerDNS struct {
	Name            string // block label
	Address         string // host:port; host must NOT be an IP literal
	ResolveInterval time.Duration // default 30s, min 1s
	Weight          int
	Backup          bool
	MaxTransactions int
	Check           CheckParams // resolved through the same defaults cascade as Server
	rng             hcl.Range
}
// Pool gains: DNSTemplates []ServerDNS
```

- [x] **Step 1: Failing tests** — `dns-template.hcl` (pool with one `server_dns` + one static `server`; pool with ONLY a template — must validate despite zero static servers: adjust the existing empty-pool rule to count templates); load assertions for every field incl. resolved Check inheriting pool tier + auth creds in Check (pool auth + template → `Check.AuthUsername` populated, mirroring servers). Validation rows: IP-literal host → `"server_dns address must be a hostname"`; `resolve_interval = "500ms"` → `"resolve_interval out of range"`; template label colliding with a server name in the same pool → `"duplicate server name"` (same summary as the existing dup rule — members share the server namespace).
- [x] **Step 2: RED** — `go test ./internal/config/ -run 'TestLoad|TestValidate|TestDefaults' -v`.
- [x] **Step 3: Implement** — decode mirrors `rawServer` (labeled block, attr ranges); defaults.go resolves template Check through the same chain servers use (read `resolveCheck`'s server loop and add the template loop beside it, including the pool-auth → CheckParams copy from `resolvePoolAuth`); IP-literal detection via `netip.ParseAddr` on the split host.
- [x] **Step 4: GREEN** — `go test ./internal/config/ -v`.
- [x] **Step 5: Commit** — `git commit -am "feat: decode server_dns pool templates"`

### Task 2: Derived-snapshot builder — `config.ExpandDNS`

**Files:**
- Create: `internal/config/dns.go`
- Test: `internal/config/dns_test.go`

**Interfaces:**
- Produces:

```go
// Membership keys a template by pool and template name.
type TemplateKey struct{ Pool, Template string }

// ExpandDNS returns a snapshot derived from base (the operator's loaded
// config): every pool's DNSTemplates expanded into concrete Server
// entries named "<template>-<ip>", appended after the pool's static
// servers in sorted-IP order, truncated at 256 per template. A plain
// deterministic rebuild: fresh Server objects every call — continuity
// across swaps is name-based (the health checker's key sync), never
// pointer-based. base is never mutated; pools without templates share
// base's Pool value.
func ExpandDNS(base *Config, members map[TemplateKey][]netip.Addr) *Config
```

- [ ] **Step 1: Failing tests** (pure, no DNS): expansion naming/order (`node-192.0.2.2` before `node-192.0.2.11`? assert SORTED BY netip.Addr.Compare, and state the expectation in the test name); member fields cloned from the template (weight/backup/check/auth creds); static servers precede members; stable member names/addresses across deltas (continuity key); truncation at 256 with deterministic drop (sorted tail); nil prev; template with zero members expands to nothing; base untouched (deep-compare before/after).
- [ ] **Step 2: RED** — `go test ./internal/config/ -run TestExpandDNS -v`.
- [ ] **Step 3: Implement** (~80 lines; shallow-copy Config and Pools slice, rebuild Servers slices only for pools with templates).
- [ ] **Step 4: GREEN**.
- [ ] **Step 5: Commit** — `git commit -am "feat: expand dns templates into derived config snapshots"`

### Task 3: Resolver runtime — `internal/discover`

**Files:**
- Create: `internal/discover/discover.go`
- Test: `internal/discover/discover_test.go`

**Interfaces:**
- Consumes: Tasks 1–2; `config.Holder` (Load/Swap).
- Produces:

```go
// LookupFunc resolves a hostname to its A/AAAA set. Injected; production
// wiring passes net.DefaultResolver.LookupNetIP (ip4+ip6).
type LookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// Runner owns DNS membership and every holder swap derived from it.
// All swaps (its own deltas AND operator reloads, via Rerender) go
// through one mutex — two writers would race read-modify-write.
type Runner struct { /* holder, lookup, lg, metrics, mu, members, base */ }

func New(h *config.Holder, lookup LookupFunc, lg *slog.Logger, m Metrics) *Runner
// Run starts one loop per template in the CURRENT base config and blocks
// until ctx ends. Interval per template = ResolveInterval ±10% jitter.
func (r *Runner) Run(ctx context.Context)
// Rerender installs a newly loaded operator config: expands it with the
// last-known membership and swaps, atomically with the loops' own swaps.
// The reload path calls this INSTEAD of holder.Swap when templates exist.
func (r *Runner) Rerender(base *config.Config)
```

Semantics (binding): lookup error OR empty answer → keep last-known-good, count `result=error|empty`, log at Warn with the template key — no swap. Delta (set comparison on sorted addrs) → `ExpandDNS(base, members)` + Swap + Info log naming added/removed IPs + `changes` counter. No delta → nothing (a stable farm never swaps). First resolution at startup happens inline in Run before the ticker (members available ~immediately), still fail-static.

- [ ] **Step 1: Failing tests** (fake lookup, intervals of 10–20ms): initial resolve populates members (holder snapshot has them); flip {A}→{A,B}→{B} drives adds/removes with stable member names across snapshots; error keeps membership; empty keeps membership; Rerender with an edited base keeps membership while applying the edit; concurrent Rerender + delta swaps race-clean (`-race`).
- [ ] **Step 2: RED** — `go test ./internal/discover/ -v`.
- [ ] **Step 3: Implement** (~150 lines).
- [ ] **Step 4: GREEN** — `go test -race ./internal/discover/ -v`.
- [ ] **Step 5: Commit** — `git commit -am "feat: dns discovery runner with fail-static membership"`

### Task 4: Wiring — cmd/bifrost + reload meeting point

**Files:**
- Modify: `cmd/bifrost/wire.go` (construct Runner when any pool has templates; initial holder content = `ExpandDNS(base, nil, nil)` so templates never leak unexpanded), `cmd/bifrost/main.go` (`start("dns discovery", ...)` beside the checker), `cmd/bifrost/reload.go` (when the runner exists, hand the validated new config to `runner.Rerender` instead of `a.holder.Swap`; the RestartRequired/BindChange checks run unchanged before it)
- Test: `cmd/bifrost/main_test.go`

- [ ] **Step 1: Failing test** — main_test boots a config with a template + injectable lookup (add a test seam the way main_test already injects things — read how it boots; if it execs the real serve path, wire the LookupFunc through newApp with a package-level default that tests override) and asserts `/servers` lists `node-<ip>` members; SIGHUP with an edited file keeps members (fail-static across reload).
- [ ] **Step 2: RED**. **Step 3: Implement.** **Step 4: GREEN** — `go test ./cmd/... -v` and full `go test ./...`.
- [ ] **Step 5: Commit** — `git commit -am "feat: wire dns discovery into serve and reload"`

### Task 5: Observability

**Files:**
- Modify: `internal/discover/discover.go` (Metrics interface it consumes), `internal/metrics/metrics.go` (+test), `internal/admin/admin.go` (+test), `internal/config/config.go` + `internal/config/dns.go` (the `Server.DNSTemplate` marker ExpandDNS sets)

**Interfaces:**
- Produces: counters `bifrost_dns_discovery_total{template, result="ok"|"error"|"empty"}`, `bifrost_dns_discovery_changes_total{template}`; gauge `bifrost_pool_discovered_members{pool, template}`; admin `/servers` rows for members gain `"discovered": true, "template": "<name>"` (derive from a marker on the expanded Server — add `DNSTemplate string` to `config.Server`, set by ExpandDNS, empty for static servers; admin reads it).

- [ ] **Step 1: Failing tests** — metrics golden list +3; admin JSON shape for a discovered vs static server.
- [ ] **Step 2: RED**. **Step 3: Implement** (template label = "pool/name" to keep it unique). **Step 4: GREEN** — `go test ./...`.
- [ ] **Step 5: Commit** — `git commit -am "feat: dns discovery metrics and admin fields"`

### Task 6: Integration test

**Files:**
- Create: `test/integration/dnsdiscovery_test.go`

- [ ] **Step 1: Write tests** (library-level harness like m1/reuse tests, injected LookupFunc, two fakesmtp backends as the "IPs" — the lookup returns their listen addrs' IPs; template port must match, so start the fakes on one fixed port each via the harness's addr plumbing — read how fakesmtp.StartAddr works): (1) membership {A,B}: transactions spread over both (DialCount>0 on each), per-member probes visible in `/servers`; (2) remove B mid-run: in-flight finishes, next transactions all land on A, cached affinity conn to B silently re-dials (pool with reuse_envelopes=3 to prove the interplay); (3) empty answer: membership unchanged, mail keeps flowing.
- [ ] **Step 2: Run** — `go test -tags=integration ./test/integration/ -run TestDNSDiscovery -v`.
- [ ] **Step 3: Full suite** — `go test ./...` and `go test -race -count=1 -tags=integration ./internal/discover/ ./test/integration/`.
- [ ] **Step 4: Commit** — `git commit -am "test: dns discovery integration coverage"`

### Task 7: Documentation

**Files:**
- Modify: `PROJECT.md` (move "DNS service discovery" from the skipped list to a documented mode; fail-static policy sentence), `docs/operations.md` (section: k8s headless example, no-TTL/interval constraint, member naming, fail-static, metrics), `examples/bifrost.hcl` (commented `server_dns` sketch)

- [ ] **Step 1–2: Edit docs**; `go test ./internal/config/ -v` green (fixture).
- [ ] **Step 3: Gates** — `go test ./...`; `gofumpt -l internal cmd test` empty.
- [ ] **Step 4: Commit** — `git commit -am "docs: dns-discovered pool members"`

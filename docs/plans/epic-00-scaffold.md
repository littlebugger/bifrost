# Epic 00: Scaffold

> Part of **Bifrost** — read `/PROJECT.md` first (it is the spec; this epic builds the repo it lives in).
> One task per fresh-context agent session, in order. Commit after each task.

## Overview

**Goal:** a buildable, lintable, CI-wired Go repo skeleton with the Make targets every later epic's Validation Commands depend on.

**Produces (later epics consume):**
- Module name `bifrost` (binary-only repo; internal packages import as `bifrost/internal/<pkg>`).
- Make targets: `build lint test test-race integration fuzz-short chaos load-smoke verify-new`.
- CI: per-commit gate = lint + test-race + integration; nightly = fuzz-long + chaos + load.

## Module Usage

Everything else is built inside this skeleton. `make verify-new` is the mechanism every task in every epic uses to prove its named tests exist and pass (plain `go test -run` exits 0 on zero matches — the silent-false-pass hole this closes).

## Test Strategy

Conditions: fresh clone builds; every Make target exits correctly on the empty skeleton. Failure: `verify-new` MUST fail when a named test is absent (self-test in Task 3). Load: n/a.

## Global Constraints (all epics)

- Go: `go 1.25` directive; CI toolchain 1.26.x. `CGO_ENABLED=0`.
- Allowed external deps (whole v1): `github.com/hashicorp/hcl/v2`, `github.com/prometheus/client_golang`, test-only `go.uber.org/goleak`. Nothing else — loadgen percentiles are stdlib `slices.Sort` + index, pacing is `time.Ticker`. Anything else needs a PROJECT.md decision first.
- Forbidden on the proxy data path: `github.com/emersion/go-smtp`, `net/textproto` (both normalize/unstuff — breaks R4).
- Files ≤ ~400 lines, one concept per file, table-driven tests beside code.

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`
3. `make build`

### Task 1: Repo + Go module + entrypoint

**Files:**
- Create: `go.mod`, `.gitignore`, `cmd/bifrost/main.go`, directory stubs `internal/`, `docs/` (plans already exist), `examples/`, `test/integration/`, `test/chaos/`, `testdata/`

**Interfaces:**
- Produces: module `bifrost`; `main.go` flags: `-version` (print version+exit 0), `-f <path>` (config path, unused yet), `-c` (check mode, unused yet — exits 2 "not implemented").

- [x] **Step 1:** `git init`; write `.gitignore` (binaries, `/tmp`, coverage, `*.out`, fuzz corpus crashers stay tracked though: ignore only `testdata/fuzz/**/pending`)
- [x] **Step 2:** `go mod init bifrost`; set `go 1.25`
- [x] **Step 3:** write `cmd/bifrost/main.go` using stdlib `flag`: `-version` prints `bifrost <version>` where version is a `var version = "dev"` ldflags hook; `-c`/`-f` parsed but `-c` exits 2 with "config check not implemented yet"
- [x] **Step 4:** `go build ./...` and `go run ./cmd/bifrost -version` both succeed
- [x] **Step 5:** commit `feat: repo skeleton, go module, entrypoint`

### Task 2: Makefile + lint config

**Files:**
- Create: `Makefile`, `.golangci.yml`

**Interfaces:**
- Produces: the Make targets listed in Overview, exactly these names — every epic's Validation Commands call them.

- [x] **Step 1:** write `.golangci.yml` enabling exactly: `govet, errcheck, staticcheck, revive, gofumpt, unused` (short list; no `depguard` yet)
- [x] **Step 2:** write `Makefile`:
  - `build`: `CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/...`
  - `lint`: `gofumpt -l . | (! grep .)` + `go vet ./...` + `golangci-lint run` (skip golangci-lint with a warning if the binary is absent — CI installs it, laptops may not)
  - `test`: `go test -count=1 ./...`
  - `test-race`: `go test -race -count=1 ./...`
  - `integration`: `go test -race -count=1 -tags=integration ./...` — the `./...` is load-bearing: several epics put integration-tagged tests inside `internal/` packages, and a `./test/integration/...`-only path would silently stop running them after their authoring task (exactly the false-pass hole verify-new exists to close)
  - `fuzz-short`: loop `FuzzCommandReader FuzzReplyParser FuzzDataFramer` → `go test -run='^$$' -fuzz=$$T -fuzztime=30s ./internal/smtpwire` (guard: skip until package exists)
  - `fuzz-long`: same loop with `-fuzztime=10m` (nightly)
  - `chaos`: `go test -race -count=1 -tags=chaos -timeout 10m ./test/chaos/...` (same guard)
  - `load-smoke`: placeholder `@echo "load-smoke: wired in epic-11"; exit 0` until epic 11 replaces it
  - `verify-new`: see Task 3
- [x] **Step 3:** `make lint test-race build` all exit 0
- [x] **Step 4:** commit `feat: makefile targets + lint config`

### Task 3: verify-new (the anti-silent-pass gate)

**Files:**
- Modify: `Makefile`
- Create: `scripts/verify_new_selftest.sh`

**Interfaces:**
- Produces: `make verify-new PKG=<./pkg> TESTS='TestA TestB' [TAGS=integration|chaos]` — fails unless EVERY named test ran and printed `--- PASS: <name>`. TAGS is required whenever the named tests sit behind a build tag; without it `go test` matches nothing and the gate must fail loudly, not pass silently.

- [x] **Step 1:** implement in `Makefile` (note: `$$` in a Make recipe reaches the shell as `$` — do NOT quadruple them; a `$$$$` would become the shell PID):
  ```make
  verify-new:
	@test -n "$(PKG)" && test -n "$(TESTS)" || { echo "usage: make verify-new PKG=./internal/x TESTS='TestA TestB' [TAGS=integration]"; exit 2; }
	@go test -race -count=1 $(if $(TAGS),-tags=$(TAGS)) -run "^($(shell echo $(TESTS) | tr ' ' '|'))$$" -v $(PKG) | tee /tmp/verify-new.out
	@for t in $(TESTS); do grep -q -- "--- PASS: $$t" /tmp/verify-new.out || { echo "FAIL: test $$t missing or not passing"; exit 1; }; done
  ```
- [x] **Step 2:** write `scripts/verify_new_selftest.sh`: creates a temp package with one passing test `TestPresent` and one integration-tagged passing test `TestTagged`; asserts (a) `make verify-new PKG=<tmp> TESTS='TestPresent'` exits 0, (b) `make verify-new PKG=<tmp> TESTS='TestAbsent'` exits **non-zero** — the whole point, (c) `TESTS='TestTagged'` without TAGS exits non-zero, (d) with `TAGS=integration` exits 0
- [x] **Step 3:** run the selftest; both assertions hold
- [x] **Step 4:** commit `feat: verify-new target with self-test`

### Task 4: CI + README

**Files:**
- Create: `.github/workflows/ci.yml`, `README.md`

- [x] **Step 1:** `ci.yml`: job `gate` on push/PR — setup Go 1.26.x, install golangci-lint + gofumpt, run `make lint test-race integration`; job `nightly` on `schedule` (cron daily) — `make fuzz-long chaos` plus `go test -race -count=3 ./...` (full-load wiring lands with epic 11)
- [x] **Step 2:** `README.md`: one-paragraph description (from PROJECT.md intro), status badge, "read PROJECT.md; plans in docs/plans/"
- [x] **Step 3:** `make lint test-race` still green
- [x] **Step 4:** commit `feat: CI workflow + README`

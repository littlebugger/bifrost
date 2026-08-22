# Epic 04: Backend-Leg Client

> Part of **smtp-balancer** — read `/PROJECT.md` first. Depends on epics 00, 01, 02, 03 (uses `internal/smtpwire`).
> One task per fresh-context agent session, in order. TDD; commit after each task.

## Overview

**Goal:** `internal/backend` — everything about one balancer→backend connection: dial, greeting+EHLO handshake, capability-superset verification, optional STARTTLS, verbatim command send, reply access, polite teardown, and a typed error taxonomy that the relay (epic 05) and health prober (epic 06) both consume.

**Produces:**
```go
// internal/backend
func Dial(ctx context.Context, srv *config.Server, opts Opts) (*Conn, error)
// Full handshake: TCP dial → read greeting (expect 220, multiline ok) → EHLO opts.EhloName →
// parse capability set → optional STARTTLS (per opts.TLSMode: none|starttls|starttls-verify) →
// re-EHLO after TLS → verify superset of opts.RequiredCaps (incl. SIZE >= required).
// The greeting and EHLO replies are CONSUMED here — never surfaced to any client (leg-splice rule).

type Opts struct { EhloName string; TLSMode string; TLSConfig *tls.Config; RequiredCaps []string; Timeouts config.Timeouts }
// Opts values come from the resolved pool config: EhloName = pool.EhloName, TLSMode = pool.BackendTLS
// (+ ServerName/CA for starttls-verify), RequiredCaps = the listener's advertised set.
// Superset carve-outs (normative, see PROJECT.md capability policy): STARTTLS is EXCLUDED from the
// comparison (client-leg-owned; backend TLS is the pool's business), and SIZE compares numerically
// with bare `SIZE` / `SIZE 0` meaning unlimited (RFC 1870) — satisfies any required value.
type Conn struct{}
func (c *Conn) SendLine(raw []byte) error                 // verbatim bytes, deadline-armed
func (c *Conn) Replies() *smtpwire.ReplyReader            // deadline management via SetCommandClass
func (c *Conn) SetCommandClass(cl Class)                  // Class ∈ {MailRcpt, DataInit, DataBlock, Dot} → arms the per-class reply deadline from Timeouts
func (c *Conn) Writer() io.Writer                          // raw DATA pipe target
func (c *Conn) Caps() CapSet                               // parsed post-handshake (post-TLS if upgraded)
func (c *Conn) Quit()                                      // best-effort QUIT, 2s deadline, then close
func (c *Conn) Abort()                                     // hard close, no dot, no QUIT (RFC 5321 3.8 abort)

// error taxonomy (errors.As-able):
type DialError struct{...}        // TCP-level: refused, timeout, DNS
type HandshakeError struct{...}   // bad greeting / EHLO rejected / TLS failure / handshake timeout
type IncompatibleError struct{ Missing []string } // capability superset violation
```

## Module Usage

The relay dials a fresh `backend.Conn` per transaction (D4) walking the router's ordered candidate list; the health prober (epic 06) reuses `Dial` for its L2/L3 probes so probes and traffic exercise the same code (L0/L1 probes are shallower than Dial's full handshake and use net.Dialer + smtpwire directly — see epic-06). `Abort()` is the client-abort-mid-DATA path (never send a dot); `Quit()` is normal detach. The error taxonomy routes: DialError/HandshakeError → try next candidate + passive health signal; IncompatibleError → health verdict `incompatible`.

## Test Strategy

All against `internal/fakesmtp`. Conditions: compliant handshake; multiline greeting; capability parse incl. `SIZE 10485760`. Failure: slow banner (timeout), wrong greeting code, EHLO 502, TLS-required backend vs TLSMode=none (HandshakeError), bad cert with verify (fails) vs without verify (passes), missing capability (IncompatibleError names it), hang at each stage (whole-phase 15 s deadline), RST mid-handshake. Load: 100 concurrent Dials race-clean, goleak.

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`

### Task 1: Dial + greeting + EHLO + capability parse

**Files:**
- Create: `internal/backend/backend.go`, `internal/backend/caps.go`, `internal/backend/backend_test.go`

- [ ] **Step 1:** failing tests: `TestDialHappyPath` (fake with caps → Conn.Caps() has PIPELINING, SIZE parsed as int, 8BITMIME), `TestDialMultilineGreeting` (220-a/220 b), `TestDialBadGreeting` (fake banner `554 no` → HandshakeError), `TestDialRefused` (SetDown ListenerClosed → DialError), `TestDialGreetingTimeout` (AcceptThenHang + short timeout → HandshakeError, distinct from DialError), `TestEhloRejected` (Script.OnEHLO `[502]` → HandshakeError)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement with `net.Dialer.DialContext`, smtpwire.ReplyReader for greeting/EHLO, CapSet parse (`NAME` or `NAME value` per line, case-insensitive names)
- [ ] **Step 4:** `make verify-new PKG=./internal/backend TESTS='TestDialHappyPath TestDialMultilineGreeting TestDialBadGreeting TestDialRefused TestDialGreetingTimeout TestEhloRejected'`
- [ ] **Step 5:** commit `feat(backend): dial + handshake + capability parse`

### Task 2: Capability superset check

**Files:**
- Create: `internal/backend/superset.go`, tests in `internal/backend/superset_test.go`

- [ ] **Step 1:** failing tests: `TestSupersetOK` (backend caps ⊇ required incl SIZE 20M ≥ required 10M), `TestSupersetMissingCapability` (`backend-missing-capability-marked-out`: required 8BITMIME absent → IncompatibleError{Missing:["8BITMIME"]}), `TestSupersetSizeTooSmall` (`advertised-size-le-min-backend-size`: SIZE 5M < required 10M → IncompatibleError naming SIZE), `TestSupersetSizeZeroUnlimited` (`SIZE 0` satisfies required 10M — RFC 1870 unlimited), `TestSupersetSizeBareKeyword` (bare `SIZE` likewise unlimited), `TestSupersetIgnoresStarttls` (STARTTLS in required set is ignored entirely: pool backend_tls=none + plaintext backend without STARTTLS → compatible; pool backend_tls=starttls → TLS enforced by the handshake itself, not by the capability comparison)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement; wire into Dial as final step
- [ ] **Step 4:** `make verify-new PKG=./internal/backend TESTS='TestSupersetOK TestSupersetMissingCapability TestSupersetSizeTooSmall TestSupersetSizeZeroUnlimited TestSupersetSizeBareKeyword TestSupersetIgnoresStarttls'`
- [ ] **Step 5:** commit `feat(backend): capability superset verification`

### Task 3: Backend-leg STARTTLS

**Files:**
- Create: `internal/backend/tls.go`, tests in `internal/backend/tls_test.go`

- [ ] **Step 1:** failing tests: `TestBackendStartTLS` (fake with TLS → handshake, re-EHLO, Caps() from SECOND EHLO), `TestBackendStartTLSVerifyBadCert` (verify mode + untrusted cert → HandshakeError), `TestBackendStartTLSNoVerify` (same cert, mode starttls → ok), `TestBackendTLSRequiredMismatch` (fake requires TLS via 530-before-MAIL script, TLSMode none → later commands fail; and: fake NOT advertising STARTTLS while TLSMode starttls → HandshakeError "STARTTLS not offered")
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement (`tls.Client` wrap, rebuild bufio, second EHLO replaces CapSet)
- [ ] **Step 4:** `make verify-new PKG=./internal/backend TESTS='TestBackendStartTLS TestBackendStartTLSVerifyBadCert TestBackendStartTLSNoVerify TestBackendTLSRequiredMismatch'`
- [ ] **Step 5:** commit `feat(backend): backend-leg starttls with re-EHLO`

### Task 4: Command send, deadlines, teardown, concurrency hygiene

**Files:**
- Create: `internal/backend/conn.go` (SendLine/Replies/SetCommandClass/Writer/Quit/Abort), tests in `internal/backend/conn_test.go`; Create: `internal/backend/backend_integration_test.go` (tag `integration`, goleak TestMain)
- Modify: `internal/backend/backend.go` (whole-phase handshake budget: one `context.WithTimeout` around greeting+EHLO+TLS)

- [ ] **Step 1:** failing tests: `TestSendLineVerbatim` (raw bytes with odd spacing/params arrive byte-exact at fake — Recorder assert), `TestCommandClassDeadlines` (MailRcpt class 30s default; with test-shortened timeouts a hanging fake trips the deadline error), `TestQuitBestEffort` (Quit returns ≤2s even against a hang; fake with normal script records QUIT), `TestAbortNoDotNoQuit` (Abort mid-DATA: fake session sees EOF, transcript contains NO terminator dot and NO QUIT — the RFC 3.8 abort proof), `TestHandshakePhaseDeadline` (fake DRIPS the greeting at 1 byte/s — each byte resets a naive per-read deadline, only the whole-phase budget catches it; shortened test budget 1s), `TestDialCtxCancelMidConnect` (cancel during hang → returns promptly, no goroutine leak), `TestHundredConcurrentDials` (integration: 100 goroutines dial one fake; all succeed or fail typed; goleak clean)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement; deadlines armed per class before each read; Writer() re-arms DataBlock deadline per write; phase budget wraps the whole handshake; `go get go.uber.org/goleak` here (test-only — its first use in the plan)
- [ ] **Step 4:** `make verify-new PKG=./internal/backend TESTS='TestSendLineVerbatim TestCommandClassDeadlines TestQuitBestEffort TestAbortNoDotNoQuit TestHandshakePhaseDeadline TestDialCtxCancelMidConnect'` and `make verify-new PKG=./internal/backend TESTS='TestHundredConcurrentDials' TAGS=integration` and `make integration`
- [ ] **Step 5:** commit `feat(backend): verbatim send, class deadlines, quit/abort, handshake budget`

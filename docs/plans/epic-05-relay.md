# Epic 05: Transaction Relay (the splice)

> Part of **smtp-balancer** — read `/PROJECT.md` first; the Transparency Contract is THIS epic's spec. Depends on epics 00–04.
> One task per fresh-context agent session, in order. TDD; commit after each task.

## Overview

**Goal:** the heart of the project — `internal/proxy` relay engine implementing `TxnHandler`: attach a backend at MAIL FROM (ordered-candidate failover), replay the pipelining queue verbatim, pump backend replies verbatim (with the single sanctioned 421→451 translation), pipe DATA raw through the DataFramer, synthesize failures per the contract, and detach after the verdict. Ends with the **M1 milestone test**: one client connection, many messages, provably spread across backends with byte-exact transparency.

**Consumes:** `session.TxnHandler` slot (03), `backend.Dial/Conn` (04), `smtpwire` (03), `fakesmtp/smtpdrv` (01).
**Produces:**
```go
// internal/proxy
type Relay struct{}
func NewRelay(pick PickFunc, cfg *config.Holder, lg *slog.Logger, sig HealthSignals, lease LeaseFunc) *Relay
// PickFunc/LeaseFunc — epic 07 provides the real ones; this epic tests with stubs.
// These closure types exist so internal/proxy NEVER imports internal/balance (no import cycle):
type PickFunc func(tx TxnMeta) (candidates []*config.Server, err error) // ordered, healthy-filtered
type LeaseFunc func(srv *config.Server) (release func())               // in-flight accounting; epic-05 stub: return func(){}
type TxnMeta struct { ClientIP netip.Addr; Helo string; MailFromDomain string }
type HealthSignals interface { DialFailure(srv); TransportError(srv); Success(srv) } // epic 06 implements; stub here
func (r *Relay) HandleTransaction(ctx context.Context, tx *Txn)
```
**Goroutine shape (from PROJECT.md):** the session goroutine owns the client leg and the DATA pipe; one reply-pump goroutine per attachment tees backend reply lines to the client (single-writer discipline via the session's writer mutex) while feeding parsed codes to the state machine. Pump joined via WaitGroup before detach returns.

## Module Usage

`cmd/smtp-balancer` wires `session → relay → balance/health`. Every message a client sends enters `HandleTransaction`; everything the client learns about its message exits through the pump. This epic is where R3 and R4 become true.

## Test Strategy

Integration-first against fakesmtp (unit tests for latching/replay pieces). Conditions: happy single message; multi-RCPT; multiline verdicts; pipelined batch end-to-end. Load: M1 test (1 conn × 20 msgs × 2 backends), 1 GB streaming ceiling. Failure: the full contract failure matrix — dial exhaustion, death at each stage (pre-354 / mid-DATA / post-dot), backend 421 mid-session, client abort mid-DATA, per-RCPT 550 vs fatal latching. Every reply asserted byte-exact; goleak everywhere; named prior-art regression tests included verbatim.

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`
3. `make integration`

### Task 1: Attach at MAIL + queue replay + verbatim envelope relay

**Files:**
- Create: `internal/proxy/relay.go`, `internal/proxy/relay_test.go` (tag integration, goleak)

- [ ] **Step 1:** failing tests: `TestMailVerdictVerbatim` (`backend-rejects-mailfrom-552-seen-at-mail-not-rcpt`: fake OnMAIL `552 5.3.4 too big` → client sees exactly that at MAIL), `TestEnvelopeRelayHappy` (MAIL/RCPT/RCPT relayed; fake Recorder shows raw client bytes verbatim incl. params like `BODY=8BITMIME`), `TestReplyVerbatimMultiline` (`reply-verbatim-multiline`: fake replies 3-line 250 with enhanced codes → client receives identical bytes), `TestQueueReplayPipelined` (pipelined MAIL+RCPT+RCPT queued during dial, replayed in order, replies in order), `TestBackendBannerNotLeaked` (`backend-banner-and-ehlo-not-leaked`: client transcript contains no 220 greeting/EHLO lines from backend)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement attach flow (PickFunc stub returns one fake), pump goroutine (tee lines verbatim under writer lock; state from final-line first digit only), queue replay via `Conn.SendLine`
- [ ] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestMailVerdictVerbatim TestEnvelopeRelayHappy TestReplyVerbatimMultiline TestQueueReplayPipelined TestBackendBannerNotLeaked' TAGS=integration
- [ ] **Step 5:** commit `feat(relay): attach at MAIL, verbatim envelope + pump`

### Task 2: Candidate failover (silent, pre-reply only)

**Files:**
- Modify: `internal/proxy/relay.go`; tests in `internal/proxy/failover_test.go`

- [ ] **Step 1:** failing tests: `TestFailoverToSecondCandidate` (first fake SetDown → dial fails → second fake gets the transaction; client sees ONLY the second's verdicts; DialCount proves order), `TestConnectRetriesPerCandidate` (one hanging fake as sole candidate → exactly 2 dial attempts observed via DialCount — the "×2 attempts" contract), `TestFailoverExhausted451` (both down → MAIL answered `451 4.4.1`; **session stays open**; next MAIL works when a fake returns), `TestFailoverQueueAnswers` (pipelined MAIL+RCPT+RSET+NOOP+DATA, all candidates down → MAIL 451 4.4.1, RCPT 451 4.4.1, RSET 250, NOOP 250, DATA 451 4.4.1 — per-class synthesis from replies.go), `TestFailoverQueuedQuit` (queued QUIT on exhaustion → 221 + clean close), `TestNoFailoverAfterFirstRelayedByte` (first fake accepts MAIL then dies at RCPT → NO silent retry; RCPT gets 451 4.4.1 — the invariant test), `TestHealthSignalsEmitted` (DialFailure on down candidate, Success on completed txn)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement walk over candidates with per-attempt connect budget; invariant flag `relayedBytes bool` guards retry
- [ ] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestFailoverToSecondCandidate TestConnectRetriesPerCandidate TestFailoverExhausted451 TestFailoverQueueAnswers TestFailoverQueuedQuit TestNoFailoverAfterFirstRelayedByte TestHealthSignalsEmitted' TAGS=integration
- [ ] **Step 5:** commit `feat(relay): ordered silent failover with relayed-byte invariant`

### Task 3: DATA — 354 relay + raw pipe + detach

**Files:**
- Create: `internal/proxy/data.go`; tests in `internal/proxy/data_test.go`

- [ ] **Step 1:** failing tests: `TestNever354Synthesized` (`backend-rejects-data-command`: fake OnDATA `451 4.3.0` → client sees it; state back to in-transaction, RSET works), `Test354AndVerdictVerbatim` (`backend-4xx-after-dot-relayed`: OnEOD `452 4.2.2` relayed exactly), `TestDotStuffingIntegrity` (bodies with `..`-stuffed lines, `.\r` mid-line, split writes at hostile boundaries → fake `AssertWireBody` byte-exact — R4 proof), `TestReadAheadDrainedAtData` (client pipelines body bytes immediately after DATA in same segment; bufio read-ahead reaches backend after relayed 354, nothing lost/reordered), `TestDetachAfterVerdict` (after final-dot verdict, backend gets QUIT (Recorder), next MAIL dials fresh — DialCount+1)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement: forward DATA, relay backend's 354-or-error; on 354 relayed → session goroutine pipes client→backend via 32 KB buffer + DataFramer; per-direction bytes-moving watchdog via deadlines re-armed per chunk; detach after verdict relayed
- [ ] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestNever354Synthesized Test354AndVerdictVerbatim TestDotStuffingIntegrity TestReadAheadDrainedAtData TestDetachAfterVerdict' TAGS=integration
- [ ] **Step 5:** commit `feat(relay): data pipe with framer + watchdog + detach`

### Task 4: In-transaction commands: RSET, second MAIL, QUIT

**Files:**
- Modify: `internal/proxy/relay.go`; tests in `internal/proxy/txncmds_test.go`

- [ ] **Step 1:** failing tests: `TestRsetInTxnRelayedThenDetach` (RSET relayed, backend 250 relayed, backend closed (Quit), next MAIL fresh dial), `TestSecondMailRelayed` (MAIL while in-txn → relayed; fake scripts 503 → relayed; state follows backend: still in original txn), `TestNoopMidTxnNotForwarded` (NOOP while attached → synthesized 250 AND `fake.CmdCount("NOOP")==0` — the contract's "never touches a backend" claim), `TestQuitInTxn` (client QUIT mid-envelope → relay QUIT? NO — contract: QUIT is synthesized 221 + both legs closed, backend gets Abort/Quit; assert fake saw no dot), `TestEhloMidSessionActsAsRset` (EHLO between transactions re-synthesized; EHLO mid-txn → backend aborted, fresh EHLO reply, next MAIL fresh)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement per contract state table
- [ ] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestRsetInTxnRelayedThenDetach TestSecondMailRelayed TestNoopMidTxnNotForwarded TestQuitInTxn TestEhloMidSessionActsAsRset' TAGS=integration
- [ ] **Step 5:** commit `feat(relay): in-transaction rset/mail/quit/ehlo semantics`

### Task 5: Failure synthesis — death at every stage, and the mid-DATA single-verdict rule

**Files:**
- Create: `internal/proxy/failures.go`; tests in `internal/proxy/failures_test.go`

**The single-verdict rule (normative, from the contract):** any final (non-354) backend reply arriving between the relayed 354 and the dot IS the transaction's only verdict — relay it (421 translated → 451 4.4.2), mark DATA answered, disarm the dot-reply timer, keep consuming client bytes to `CRLF.CRLF` (piping if the backend is still alive, discarding if dead), and emit **nothing** after the dot. The synthesis paths below fire ONLY when no verdict was relayed.

- [ ] **Step 1:** failing tests: `TestBackendDiesPre354` (`io-error-is-4xx-never-5xx`: RST after RCPT accepted → pending command 451 4.4.1 — never 5xx; session alive; next MAIL ok), `TestBackendDiesMidDataDiscardToDot` (`backend-dies-mid-data-client-gets-451`: RST mid-body, no verdict relayed → balancer keeps consuming client body to terminator, then 451 4.4.1; **next command parses correctly** — the desync guard), `TestBackendDiesAfterDot` (ActDropConn on EOD → 451 4.4.2 + duplicate-risk log record present), `TestBackend421Translated` (fake OnRCPT `421 4.7.0 closing` → client sees `451 4.4.2`, session alive, backend dropped — the one sanctioned rewrite), `TestBackendEarlyReplyMidData` (fake scripts `552` via OnEvent mid-body then ActDropConn → client receives the 552 bytes verbatim, **exactly one reply for DATA**, remaining body consumed to dot, nothing emitted after it, next command in sync), `TestBackend421MidData` (421 mid-body → single `451 4.4.2`, same single-reply and drain-to-dot assertions), `TestMalformedBackendReplyTreatedAsDeath` (fake replies garbage/oversized line → treated as backend death at current phase per contract), `TestClientAbortMidData` (smtpdrv AbortMidData → backend leg Abort()ed: fake transcript has NO dot, NO QUIT; goleak clean), `TestBackendReplyTimeout451` (Hang on MAIL with shortened timeout → 451 4.4.2, TransportError signal)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement discard-to-dot mode (framer-driven, counts bytes, no writes), the single-verdict rule (verdict-relayed flag owned by the pump, checked by every synthesis path; dot-timer disarm), 421 interception in pump, duplicate-risk structured log event
- [ ] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestBackendDiesPre354 TestBackendDiesMidDataDiscardToDot TestBackendDiesAfterDot TestBackend421Translated TestBackendEarlyReplyMidData TestBackend421MidData TestMalformedBackendReplyTreatedAsDeath TestClientAbortMidData TestBackendReplyTimeout451' TAGS=integration
- [ ] **Step 5:** commit `feat(relay): contract failure synthesis, single-verdict rule, 421 translation`

### Task 6: Two-class error latching

**Files:**
- Create: `internal/proxy/latch.go`; tests in `internal/proxy/latch_test.go`

- [ ] **Step 1:** failing tests: `TestRcpt550ThenRcpt250ReachesBackend` (`rcpt-550-then-rcpt-250-reaches-backend`: fake OnRCPT [550, 250] → second RCPT relayed and accepted — per-RCPT verdicts never latch; Mireka's bug regression), `TestFatalLatchAnswersRestOfTxn` (backend dead → subsequent RCPT/DATA in same txn answered 451 4.4.1 from latch without dial attempts), `TestLatchClearsOnRset` (`latch-clears-on-rset`), `TestLatchClearsOnNextMail` (`latch-clears-on-next-mail`: fresh MAIL re-picks; DialCount proves new dial)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement latch scoped to transaction, cleared at RSET/MAIL/EHLO
- [ ] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestRcpt550ThenRcpt250ReachesBackend TestFatalLatchAnswersRestOfTxn TestLatchClearsOnRset TestLatchClearsOnNextMail' TAGS=integration
- [ ] **Step 5:** commit `feat(relay): two-class error latching`

### Task 7: M1 milestone — R3+R4 proof + streaming ceiling

**Files:**
- Create: `test/integration/m1_test.go` (tag integration, goleak TestMain)

- [ ] **Step 1:** failing tests: `TestM1DistributionOneConnection` (wire session+relay with a round-robin stub PickFunc over TWO fakes; one smtpdrv connection sends 20 messages; assert: every message verdict verbatim, both fakes got ~10 (exact for round-robin stub), all WireBodies byte-exact, goroutines back to baseline), `TestM1PipelinedThroughput` (same but each message sent as one pipelined batch), `TestStreamingCeiling1GB` (one 1 GiB generated body streamed through; `runtime.ReadMemStats` HeapAlloc stays < 64 MiB after GC; fake discards body via `Script.DiscardBody = true` — the field exists in epic-01's Script)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** wire the pieces in the test (no cmd/ changes yet)
- [ ] **Step 4:** `make verify-new PKG=./test/integration TESTS='TestM1DistributionOneConnection TestM1PipelinedThroughput TestStreamingCeiling1GB' TAGS=integration` and `make integration`
- [ ] **Step 5:** commit `feat(relay): M1 — per-transaction balancing with byte transparency proven`

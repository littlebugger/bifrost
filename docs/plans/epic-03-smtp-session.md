# Epic 03: Wire Primitives + Client-Leg Session

> Part of **Bifrost** — read `/PROJECT.md` first (especially the Transparency Contract). Depends on epics 00, 01, 02.
> One task per fresh-context agent session, in order. TDD; commit after each task.

## Overview

**Goal:** `internal/smtpwire` (raw-preserving wire primitives — the R4 foundation and the fuzz surface) and the pre-attach half of `internal/proxy` (banner, EHLO/HELO, STARTTLS, synthesized replies, pipelining queue, limits). After this epic a client can connect, negotiate, and be correctly refused at MAIL (no backends wired yet).

**Produces:**
```go
// internal/smtpwire — NO normalization anywhere; forbidden: net/textproto
func ReadCommandLine(br *bufio.Reader, max int) (raw []byte, err error) // raw includes CRLF; ErrLineTooLong; ErrBareLF: a bare-LF-terminated line is READ TO COMPLETION but returned with ErrBareLF — the session synthesizes 500 5.5.2 and NEVER relays it (SMTP-smuggling defense, CVE-2023-51764 class; matches modern Postfix/Exim/Sendmail defaults). Strict CRLF everywhere, commands and framer alike.
func ParseVerb(raw []byte) (verb string, args []byte)                   // verb uppercased; args raw

type ReplyReader struct{ /* bufio-backed */ }
func NewReplyReader(br *bufio.Reader, maxLine, maxTotal int) *ReplyReader
func (r *ReplyReader) Next() (line []byte, code int, final bool, err error) // line verbatim incl CRLF; code from digits; final = space separator; ErrMalformedReply, ErrReplyTooLong

type DataFramer struct{ /* 5-state CRLF.CRLF scanner; initial state = afterCRLF */ }
func (f *DataFramer) Feed(p []byte) (n int, done bool) // n bytes consumed into the message (incl. terminator when done)

// internal/proxy (pre-attach)
type Session struct{ /* one per client conn */ }
func NewSession(conn net.Conn, cfg *config.Config, h TxnHandler, lg *slog.Logger) *Session
func (s *Session) Run(ctx context.Context) error
type TxnHandler interface { // relay engine plugs in here (epic-05); epic-03 tests use a stub
    HandleTransaction(ctx context.Context, tx *Txn) // owns MAIL..dot lifecycle
}
type Txn struct { ClientIP netip.Addr; Helo string; PipelineQ *PipeQueue; /* reader/writer handles */ }
type PipeQueue struct{} // bounded 32 lines / 16 KB; Push(raw []byte) error(ErrPipelineOverflow); Drain() [][]byte

// internal/proxy/replies.go — THE closed enum (Transparency Contract table); no SMTP reply literal
// may appear anywhere else in internal/proxy: e.g. RplBanner(host), RplEhlo(caps), RplBadSequence,
// RplUnknownCmd, RplNotImplemented, RplIdleTimeout, RplShuttingDown, RplNoBackend, RplBackendLost,
// RplBackendTimeout, RplAllBusy, RplPipelineOverflow, RplLineTooLong, RplBareLf, RplSessionLifetime,
// RplVrfy, RplHelp, RplOK, RplBye
```

## Module Usage

`smtpwire` is shared by session (client leg), relay pump, backend client, and health prober — one wire implementation, one set of bugs, fuzzed once. The session engine owns everything before a backend exists and hands each transaction to `TxnHandler` at MAIL; the pipelining queue is filled here and drained by the relay in epic-05.

## Test Strategy

Conditions: full EHLO negotiation, HELO (no extensions), capability list exactly from config, STARTTLS state reset. Load: fuzz all three primitives (`FuzzCommandReader`, `FuzzReplyParser`, `FuzzDataFramer` — flagship, with dot-boundary-split seed corpus). Failure: every protocol violation from the contract table → exact synthesized reply, connection stays open where mandated; limits (4 KB line → 500; idle → 421+close; pipeline overflow → 421 4.7.0+close).

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`
3. `make fuzz-short`

### Task 1: Command-line reader

**Files:**
- Create: `internal/smtpwire/command.go`, `internal/smtpwire/command_test.go`, `internal/smtpwire/fuzz_test.go` (FuzzCommandReader), seed corpus `internal/smtpwire/testdata/fuzz/FuzzCommandReader/*`

- [x] **Step 1:** failing table test `TestReadCommandLine`: CRLF line verbatim (incl. CRLF in raw); **bare-LF line → read fully, returned with `ErrBareLF` (never relayed; caller synthesizes 500)**; exactly-max line ok; max+1 → `ErrLineTooLong` after discarding through end-of-line (session must be able to answer 500 and continue in sync); EOF mid-line; NUL bytes preserved. `TestParseVerb`: `"mail from:<a@b>"` → `MAIL`, args verbatim; lone verb; trailing spaces
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement; add `FuzzCommandReader` (no panic, no over-read, consumed+returned bytes reconstruct input)
- [x] **Step 4:** `make verify-new PKG=./internal/smtpwire TESTS='TestReadCommandLine TestParseVerb'` and `go test -run='^$' -fuzz=FuzzCommandReader -fuzztime=15s ./internal/smtpwire`
- [x] **Step 5:** commit `feat(smtpwire): raw command reader + fuzz`

### Task 2: Streaming reply reader

**Files:**
- Create: `internal/smtpwire/reply.go`, `internal/smtpwire/reply_test.go`, FuzzReplyParser + corpus

- [x] **Step 1:** failing table test `TestReplyReader`: single `250 ok`; multiline `250-a/250-b/250 c` (three Next() calls, lines verbatim, final only on last); enhanced codes passed through untouched; bare `250` line; code-only+CRLF; malformed (non-digit, code <200 or >599, mismatched continuation codes tolerated but flagged? decision: mismatched continuation code → ErrMalformedReply — treat as backend death per contract); line > maxLine and total > maxTotal → errors
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement streaming (each line surfaced as read — R4 forbids holding continuation lines); add `FuzzReplyParser`
- [x] **Step 4:** `make verify-new PKG=./internal/smtpwire TESTS='TestReplyReader'` + 15s fuzz
- [x] **Step 5:** commit `feat(smtpwire): streaming verbatim reply reader + fuzz`

### Task 3: DataFramer (the R4 flagship)

**Files:**
- Create: `internal/smtpwire/framer.go`, `internal/smtpwire/framer_test.go`, FuzzDataFramer + torture corpus

- [x] **Step 1:** failing table test `TestDataFramer` over hostile split points: terminator in one chunk; split at every byte boundary of `\r\n.\r\n` (5 positions × arbitrary prefix); leading-dot-stuffed lines (`..x`) NOT treated as terminator; `\r\n.\r` then more data; `.\r\n` at stream start (initial state afterCRLF → immediate done — empty message); bare-LF lines inside body (never terminate; only `\r\n.\r\n` does); 10 MB body streamed in 1-byte feeds (state machine only, O(1) memory)
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement 5-state scanner; add `FuzzDataFramer` (property: for random body+terminator randomly chunked, sum of consumed == len, done exactly once, never early); seed corpus = the table cases as files
- [x] **Step 4:** `make verify-new PKG=./internal/smtpwire TESTS='TestDataFramer'` + 30s fuzz
- [x] **Step 5:** commit `feat(smtpwire): crlf-dot-crlf framer + torture corpus`

### Task 4: replies.go + pre-attach session states

**Files:**
- Create: `internal/proxy/replies.go`, `internal/proxy/session.go`, `internal/proxy/session_test.go` (over `net.Pipe()`; no listener yet)

- [x] **Step 1:** failing tests (stub TxnHandler answering RplNoBackend): `TestBannerAndEhlo` (banner from config hostname; EHLO → exact configured capability lines; EHLO again → same, resets state per RFC), `TestHelo` (250, no extensions), `TestPreAttachTrivia` (NOOP 250; RSET 250; VRFY 252; EXPN 502; HELP 214; unknown → 500 **and next command still works**), `TestBareLfCommand500` (bare-LF-terminated command → RplBareLf 500 5.5.2, nothing relayed, session continues in sync), `TestBadSequence` (RCPT/DATA before MAIL → 503), `TestQuit` (221 + close), `TestAuthAndBdatRejected` (502), `TestMailReachesHandler` (MAIL hands Txn to TxnHandler with ClientIP+Helo; stub's 451 written)
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement session loop (S0 banner-sent → S1 ready), all synthesized replies ONLY via replies.go constants; single-writer discipline (one writer owns the client socket)
- [x] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestBannerAndEhlo TestHelo TestPreAttachTrivia TestBareLfCommand500 TestBadSequence TestQuit TestAuthAndBdatRejected TestMailReachesHandler'`
- [x] **Step 5:** commit `feat(proxy): pre-attach session + closed reply enum`

### Task 5: STARTTLS (client leg) + limits

**Files:**
- Create: `internal/proxy/starttls.go`; Modify: `internal/proxy/session.go`; tests `internal/proxy/starttls_test.go`

- [x] **Step 1:** failing tests: `TestStartTLSHandshakeAndReset` (220 → tls handshake with `fakesmtp.TestCert`; session state reset; EHLO required again; STARTTLS absent from post-TLS EHLO; MAIL before re-EHLO → 503), `TestStartTLSWithParams501`, `TestSecondStartTLS503`, `TestStartTLSMidTxn503` (after MAIL), `TestStartTLSNoCert` (not advertised; command → 502 — decision: unadvertised STARTTLS without cert behaves as unknown-extension 502), `TestLineTooLong500` (4 KB+1 command → 500 5.5.2, session continues in sync), `TestClientIdle421` (short idle timeout config → 421 4.4.2 + close), `TestFirstCommandWait421`
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement (`tls.Server` wrap; rebuild bufio wrappers after upgrade; deadlines re-armed before every read per config timeouts)
- [x] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestStartTLSHandshakeAndReset TestStartTLSWithParams501 TestSecondStartTLS503 TestStartTLSMidTxn503 TestStartTLSNoCert TestLineTooLong500 TestClientIdle421 TestFirstCommandWait421'`
- [x] **Step 5:** commit `feat(proxy): client-leg starttls + session limits`

### Task 6: Pipelining queue

**Files:**
- Create: `internal/proxy/pipequeue.go`, `internal/proxy/pipequeue_test.go`

- [x] **Step 1:** failing tests: `TestPipeQueueFIFO` (raw lines out in order, verbatim), `TestPipeQueueOverflowLines` (33rd line → ErrPipelineOverflow), `TestPipeQueueOverflowBytes` (16 KB+1), `TestSessionPipelinedBatchQueued` (client pipelines MAIL+RCPT+RCPT+DATA in one write before handler responds; session pushes all four to the Txn's queue; stub handler drains and answers; replies arrive in command order), `TestSessionPipelineOverflow421` (oversized batch → RplPipelineOverflow 421 4.7.0 + close)
- [x] **Step 2:** run: fail
- [x] **Step 3:** implement bounded queue; session reads-ahead available buffered lines into the queue while a transaction is pending
- [x] **Step 4:** `make verify-new PKG=./internal/proxy TESTS='TestPipeQueueFIFO TestPipeQueueOverflowLines TestPipeQueueOverflowBytes TestSessionPipelinedBatchQueued TestSessionPipelineOverflow421'`
- [x] **Step 5:** commit `feat(proxy): bounded pipelining queue`

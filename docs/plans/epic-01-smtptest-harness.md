# Epic 01: SMTP Test Harness (fakesmtp + smtpdrv)

> Part of **smtp-balancer** — read `/PROJECT.md` first. Depends on epic-00.
> One task per fresh-context agent session, in order. TDD; commit after each task.

## Overview

**Goal:** the two test assets every later epic runs on: `internal/fakesmtp` (scriptable fake SMTP backend with byte-exact recording) and `internal/smtpdrv` (scripted SMTP client driver). No production code in this epic.

**Produces (consumed by every integration/chaos/load test):**
```go
// internal/fakesmtp
func Start(t testing.TB, s Script) *Server          // net.Listen on 127.0.0.1:0
func (s *Server) Addr() string
func (s *Server) Stop()
func (s *Server) SetScript(Script)                   // atomic swap (flap tests)
func (s *Server) SetDown(mode DownMode)              // ListenerClosed | AcceptThenRST | AcceptThenHang
func (s *Server) SetUp()
func (s *Server) OnEvent(func(Event))                // deterministic chaos triggers; Event{Verb, SessionID}
func (s *Server) Sessions() []Session
func (s *Server) DialCount() int
func (s *Server) CmdCount(verb string) int
func (s *Server) AssertWireBody(t testing.TB, msgIdx int, want []byte)

type Script struct {
    Banner Step
    Caps   []string      // EHLO 250- lines, e.g. "PIPELINING", "SIZE 10485760", "8BITMIME"
    TLS    *tls.Config   // nil => STARTTLS not advertised
    OnEHLO []Step        // empty => default 250- reply built from Caps (epics 04/06 script EHLO rejections)
    OnMAIL, OnRCPT, OnDATA, OnEOD, OnRSET, OnQUIT []Step // consumed per call; LAST STEP REPEATS
    DiscardBody bool     // recorder skips body accumulation (1 GB streaming tests)
}
type Step struct {
    Reply  string        // full text incl. code; "\r\n"-joined multiline; "" => default 250
    Delay  time.Duration // before replying
    Drip   time.Duration // per-byte write delay
    Action Action        // ActReply (default) | ActDropConn | ActRST | ActHang
}
type Session struct{}                    // concrete struct, one implementation — no interface ceremony
func (s *Session) Transcript() []Event   // every received line, raw bytes
func (s *Session) Messages() []Msg       // Msg{From string; Rcpts []string; WireBody []byte} — WireBody still dot-stuffed, byte-exact

// internal/smtpdrv
func Dial(t testing.TB, addr string) *Conn
func (c *Conn) Expect(codePrefix string) Reply       // asserts; returns full multiline reply
func (c *Conn) Send(line string)
func (c *Conn) Pipeline(lines ...string)             // one write
func (c *Conn) ExpectN(codePrefixes ...string) []Reply // in-order replies for a pipelined batch
func (c *Conn) Raw(b []byte)                          // protocol violations
func (c *Conn) SendMsg(i int) Reply                   // MAIL/RCPT/DATA/body/. convenience; returns final verdict
func (c *Conn) AbortMidData(afterBytes int)           // hard close mid-body
func (c *Conn) StartTLS(cfg *tls.Config)
func TestCert(t testing.TB) *tls.Config               // self-signed in-test cert (crypto/x509), server+client configs
```

## Module Usage

fakesmtp plays every backend role in integration, chaos, health, and load tests: scripted verdict sequences (`4xx,4xx,250`), failure injection at any stage, capability variation, TLS on/off, and byte-exact assertion that the balancer relayed the client's message unmodified (R4's DotStuffingIntegrity proof). smtpdrv plays every client role including protocol violations and pipelining; epic-11's loadgen reuses it.

## Test Strategy

The harness itself must be trustworthy: **fakesmtp is validated against stdlib `net/smtp` as a known-good reference client** (if stdlib can send mail through it and the Recorder captures exactly what stdlib sent, the fake is honest). Conditions: happy scripts, multiline replies, TLS. Failure: every Action (Drop/RST/Hang) observable from the client side; down-modes distinguishable. Load: 100 concurrent sessions against one fake without lost transcripts (race-clean).

**CRITICAL correctness rule:** the Recorder must capture the DATA body **raw and still dot-stuffed** — do NOT use `net/textproto.DotReader` (it unstuffs). Read lines manually; body accumulation stops at the `CRLF.CRLF` terminator line.

## Validation Commands

<!-- all must exit 0; run from repo root -->
1. `make lint`
2. `make test-race`

### Task 1: fakesmtp core loop

**Files:**
- Create: `internal/fakesmtp/fakesmtp.go`, `internal/fakesmtp/fakesmtp_test.go`

- [ ] **Step 1:** write failing tests `TestFakeBannerEHLOQuit` (dial raw, read scripted banner, EHLO → 250- caps lines, QUIT → 221) and `TestFakeAgainstStdlibClient` (`net/smtp.SendMail` through the fake with default Script succeeds)
- [ ] **Step 2:** run: both fail (package empty)
- [ ] **Step 3:** implement `Start/Addr/Stop`, session loop: banner Step, EHLO/HELO → `250-`caps reply, MAIL/RCPT/DATA/EOD/RSET/QUIT default 250/354/250/221 replies, per-session goroutine, `t.Cleanup(Stop)`
- [ ] **Step 4:** `make verify-new PKG=./internal/fakesmtp TESTS='TestFakeBannerEHLOQuit TestFakeAgainstStdlibClient'`
- [ ] **Step 5:** commit `feat(fakesmtp): core scripted server`

### Task 2: Step sequences + actions

**Files:**
- Modify: `internal/fakesmtp/fakesmtp.go`; Create: `internal/fakesmtp/steps.go`, tests in `internal/fakesmtp/steps_test.go`

- [ ] **Step 1:** failing tests: `TestStepSequenceConsumedLastRepeats` (OnRCPT `[450, 250]` → first RCPT 450, all later 250), `TestOnEHLOScripted` (OnEHLO `[502]` → EHLO rejected; empty OnEHLO → default 250- from Caps), `TestStepDelay` (reply arrives after Delay), `TestStepDrip` (bytes paced), `TestActDropConn` (clean EOF after MAIL), `TestActRST` (`SetLinger(0)` → connection reset observed), `TestActHang` (no reply until server Stop)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement per-verb Step queues (mutex-guarded; last repeats), Delay/Drip in the writer, Actions (RST via `TCPConn.SetLinger(0)` + Close; Hang blocks on server ctx)
- [ ] **Step 4:** `make verify-new PKG=./internal/fakesmtp TESTS='TestStepSequenceConsumedLastRepeats TestOnEHLOScripted TestStepDelay TestStepDrip TestActDropConn TestActRST TestActHang'`
- [ ] **Step 5:** commit `feat(fakesmtp): step sequences, delay/drip, drop/rst/hang actions`

### Task 3: Recorder — byte-exact transcripts

**Files:**
- Create: `internal/fakesmtp/recorder.go`, `internal/fakesmtp/recorder_test.go`

- [ ] **Step 1:** failing tests: `TestRecorderTranscript` (verbs+raw lines in order), `TestRecorderWireBodyDotStuffed` — send via smtpdrv-style raw writes a body containing `\r\n..leading-dot\r\n`, `\r\n.\r` mid-line, bare `.` line escaped as `..`; assert `Messages()[0].WireBody` equals the stuffed bytes EXACTLY as sent (terminator excluded), `TestDialAndCmdCounters`, `TestAssertWireBodyHelper`
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement raw line reader (bufio, manual CRLF handling — **no textproto**), body capture until `CRLF.CRLF`, counters, helpers
- [ ] **Step 4:** `make verify-new PKG=./internal/fakesmtp TESTS='TestRecorderTranscript TestRecorderWireBodyDotStuffed TestDialAndCmdCounters TestAssertWireBodyHelper'`
- [ ] **Step 5:** commit `feat(fakesmtp): byte-exact recorder`

### Task 4: Down-modes, script swap, event hooks

**Files:**
- Create: `internal/fakesmtp/down.go`, `internal/fakesmtp/down_test.go`

- [ ] **Step 1:** failing tests: `TestDownListenerClosed` (dial refused), `TestDownAcceptThenRST` (accept then immediate reset), `TestDownAcceptThenHang` (accept, no banner), `TestSetUpRestores`, `TestSetScriptAtomicSwap` (mid-traffic swap; next session uses new script; no race under `-race`), `TestOnEventHook` (event fires on MAIL with correct verb, usable as chaos trigger channel), `TestHundredConcurrentSessions` (the load axis: 100 goroutines each run banner/EHLO/MAIL/RCPT/DATA/QUIT concurrently; assert `DialCount()==100`, 100 sessions each with an intact, complete transcript — the locking every later chaos/load epic leans on)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement; down-modes must be switchable while sessions are live
- [ ] **Step 4:** `make verify-new PKG=./internal/fakesmtp TESTS='TestDownListenerClosed TestDownAcceptThenRST TestDownAcceptThenHang TestSetUpRestores TestSetScriptAtomicSwap TestOnEventHook TestHundredConcurrentSessions'`
- [ ] **Step 5:** commit `feat(fakesmtp): down modes, script swap, event hooks`

### Task 5: STARTTLS in the fake + TestCert

**Files:**
- Create: `internal/fakesmtp/tls.go`, `internal/fakesmtp/tls_test.go`

- [ ] **Step 1:** failing tests: `TestTestCertRoundTrip` (client cfg trusts server cfg), `TestFakeStartTLS` (Caps include STARTTLS iff Script.TLS != nil; STARTTLS → 220 → handshake → session continues; EHLO after TLS no longer lists STARTTLS), `TestFakeTLSRequiredReject` (script mode: 530 to MAIL before TLS)
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement `TestCert` (self-signed via crypto/x509, SANs `127.0.0.1`,`localhost`) and STARTTLS upgrade via `tls.Server`
- [ ] **Step 4:** `make verify-new PKG=./internal/fakesmtp TESTS='TestTestCertRoundTrip TestFakeStartTLS TestFakeTLSRequiredReject'`
- [ ] **Step 5:** commit `feat(fakesmtp): starttls + in-test certs`

### Task 6: smtpdrv client driver

**Files:**
- Create: `internal/smtpdrv/smtpdrv.go`, `internal/smtpdrv/smtpdrv_test.go`

- [ ] **Step 1:** failing tests (all against fakesmtp): `TestDrvExpectMultiline` (EHLO caps captured fully), `TestDrvPipelineExpectN` (one write MAIL/RCPT/RCPT/DATA, replies read in order), `TestDrvRawViolation` (bare-LF line delivered as-is; fake records raw), `TestDrvSendMsg` (returns final verdict; fake's WireBody matches), `TestDrvAbortMidData` (fake session sees EOF before terminator), `TestDrvStartTLS`
- [ ] **Step 2:** run: fail
- [ ] **Step 3:** implement over `net.Conn` + bufio (raw-preserving; textproto acceptable here ONLY for reply reading convenience — but prefer the same manual reader to avoid normalization surprises; decision: manual)
- [ ] **Step 4:** `make verify-new PKG=./internal/smtpdrv TESTS='TestDrvExpectMultiline TestDrvPipelineExpectN TestDrvRawViolation TestDrvSendMsg TestDrvAbortMidData TestDrvStartTLS'`
- [ ] **Step 5:** commit `feat(smtpdrv): scripted test client`

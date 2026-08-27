// Package fakesmtp implements a scriptable fake SMTP backend for tests.
//
// It is the shared test double every later Bifrost epic drives its
// integration, chaos, and load tests against: scripted verdict sequences,
// failure injection, capability variation, TLS on/off, and byte-exact
// recording of what a client actually sent.
//
// The wire is read by hand throughout (bufio only) and never through
// net/textproto: textproto.Reader.DotReader unstuffs DATA bodies, which
// would break the byte-exact recording later tasks in this package build
// on top of this file.
package fakesmtp

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// closeQuietly closes c, discarding the error. Used for best-effort
// cleanup (deferred conn/listener closes) where there is nothing
// sensible to do with a failure.
func closeQuietly(c io.Closer) {
	_ = c.Close()
}

// Step scripts a single reply.
type Step struct {
	Reply  string        // full reply text incl. code; "\r\n"-joined for multiline; "" => verb's default
	Delay  time.Duration // sleep before replying (or acting)
	Drip   time.Duration // pace the reply write one byte at a time, sleeping this long between bytes
	Action Action        // ActReply (default) | ActDropConn | ActRST | ActHang
}

// Script configures one fake backend session. The zero value is a fully
// usable "accept everything" script: default banner, no capabilities.
//
// OnEHLO, OnMAIL, OnRCPT, OnDATA, OnEOD, OnRSET, OnQUIT are per-verb step
// queues: each call to that verb within a session consumes the next step;
// once exhausted, the last step repeats forever. An empty queue uses the
// verb's built-in default reply (OnEHLO's default is the 250- capability
// block built from Caps).
//
// The fake never enforces SMTP's own command ordering (e.g. RCPT/DATA
// before MAIL, or two MAILs in a row): each verb replies independently of
// session state, by design. Script the sequence — including the
// out-of-order case — you want to observe; the fake won't reject it for you.
type Script struct {
	Banner Step
	Caps   []string    // EHLO 250- capability lines, e.g. "PIPELINING", "8BITMIME"
	TLS    *tls.Config // nil => STARTTLS not advertised; see TestCert

	OnEHLO                                        []Step
	OnMAIL, OnRCPT, OnDATA, OnEOD, OnRSET, OnQUIT []Step

	// MidBody runs in the *middle* of a DATA body, once MidBodyLines body
	// lines have been read (default: after the first line): the steps are
	// executed in order, so a script can send an early final reply and
	// then drop or reset the connection. It is the only way to script
	// PROJECT.md's mid-DATA rows — a reply that arrives after the
	// relayed 354 and before the dot — since a body line is not a command
	// and therefore fires no OnEvent hook. When no step closes the
	// connection the fake goes on reading the body to its terminator.
	MidBody      []Step
	MidBodyLines int

	// DiscardBody skips body accumulation in the recorder for
	// large-message tests: the transcript and wire body are not kept, but
	// Session.BodyBytes still counts every byte that arrived.
	DiscardBody bool
}

// Server is a running fake SMTP backend.
type Server struct {
	t testing.TB

	mu         sync.Mutex
	ln         net.Listener
	addr       string // fixed host:port, stable across SetDown(DownListenerClosed)/SetUp cycles
	script     Script
	downMode   DownMode
	dialCount  int
	cmdCounts  map[string]int
	sessions   []*sessionState
	eventHooks []func(Event)
	stopped    bool

	done     chan struct{} // closed by Stop; unblocks ActHang steps
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// Start launches a fake SMTP backend listening on 127.0.0.1:0 and returns
// once it is accepting connections. It is stopped automatically via
// t.Cleanup, so tests need not call Stop themselves.
func Start(t testing.TB, s Script) *Server {
	t.Helper()
	srv, err := startOn(t, "127.0.0.1:0", s)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(srv.Stop)
	return srv
}

// nopTB is a minimal, private testing.TB stand-in for StartAddr. Start's
// own Helper/Fatalf calls always run against the caller's real t, never
// against the value stored on Server — the only stored-t method any
// other production code path calls is relisten's srv.t.Errorf (down.go),
// reachable only via SetDown(DownListenerClosed)+SetUp, which
// cmd/fakesmtp (a listen-once standalone fake) never calls. Errorf is
// overridden defensively anyway, as a no-op, so that dead path stays
// safe rather than panicking if it is ever exercised. Embedding the
// interface, rather than implementing it outright, is required to
// satisfy testing.TB's sealed private method from outside package
// testing; every other promoted method stays nil and unreachable.
type nopTB struct{ testing.TB }

func (nopTB) Errorf(string, ...any) {}

// startOn is Start's shared core: listen on addr, launch the accept
// loop, and return the running Server (or the listen error). Both Start
// (always 127.0.0.1:0, ephemeral, for tests) and StartAddr (an
// operator-chosen bind, for cmd/fakesmtp) build on it.
func startOn(t testing.TB, addr string, s Script) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("fakesmtp: listen %s: %w", addr, err)
	}
	srv := &Server{t: t, ln: ln, addr: ln.Addr().String(), script: s, done: make(chan struct{})}
	srv.wg.Add(1)
	go srv.acceptLoop(ln)
	return srv, nil
}

// StartAddr is Start's non-testing counterpart, for callers with no
// testing.TB (cmd/fakesmtp, a standalone binary): it binds addr — not
// necessarily ephemeral, an operator-chosen port — and returns a listen
// error instead of calling Fatalf. There is no t.Cleanup, so the caller
// must call Stop itself.
//
// Deviation (recorded, epic-11): fakesmtp's whole API assumes a
// testing.TB. StartAddr is the minimal non-testing entry point
// cmd/fakesmtp needs; the testing.TB path (Start, above) is unchanged.
func StartAddr(addr string, s Script) (*Server, error) {
	return startOn(nopTB{}, addr, s)
}

// Addr returns the "host:port" the server is listening on. It stays
// stable across SetDown(DownListenerClosed)/SetUp cycles.
func (srv *Server) Addr() string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.addr
}

// Stop shuts the server down: closes the listener and waits for every
// session goroutine to finish. Safe to call more than once, including
// concurrently with SetDown/SetUp (relisten re-checks stopped under the
// same lock before committing a fresh listener, so a Stop racing a
// SetUp can never miss the listener it needs to close, or leave one
// running unattended).
func (srv *Server) Stop() {
	srv.stopOnce.Do(func() {
		// ln MUST be read inside this same critical section, not before
		// stopOnce.Do runs: reading it earlier leaves a window where a
		// concurrent SetUp's relisten can swap in a fresh listener that
		// this Stop would never learn about, hanging wg.Wait on an
		// acceptLoop nothing ever tells to exit.
		srv.mu.Lock()
		srv.stopped = true
		ln := srv.ln
		srv.mu.Unlock()
		closeQuietly(ln)
		close(srv.done)
	})
	srv.wg.Wait()
}

func (srv *Server) acceptLoop(ln net.Listener) {
	defer srv.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		srv.mu.Lock()
		srv.dialCount++
		dialN := srv.dialCount
		mode := srv.downMode
		script := srv.script
		srv.mu.Unlock()

		switch mode {
		case DownAcceptThenRST:
			srv.wg.Add(1)
			go func() {
				defer srv.wg.Done()
				rstClose(conn)
			}()
			continue
		case DownAcceptThenHang:
			srv.wg.Add(1)
			go func() {
				defer srv.wg.Done()
				<-srv.done
				closeQuietly(conn)
			}()
			continue
		}

		sess := newSessionState(dialN, script.DiscardBody)
		srv.mu.Lock()
		srv.sessions = append(srv.sessions, sess)
		srv.mu.Unlock()

		srv.wg.Add(1)
		go func() {
			defer srv.wg.Done()
			srv.handleConn(conn, script, sess)
		}()
	}
}

// handleConn runs one session end to end: banner, then a command loop
// dispatching on the verb until QUIT or a read/write error ends it. sess
// records every line and completed transaction for later inspection via
// Server.Sessions.
func (srv *Server) handleConn(conn net.Conn, script Script, sess *sessionState) {
	// A closure, not a plain defer closeQuietly(conn): STARTTLS reassigns
	// conn to the tls.Conn wrapper, and the deferred close must close
	// whatever conn currently holds when the function returns.
	defer func() { closeQuietly(conn) }()
	r := bufio.NewReader(conn)

	if !writeStep(srv, conn, script.Banner, "220 fakesmtp ESMTP ready") {
		return
	}

	var cur stepCursors
	tlsActive := false
	for {
		raw, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		verb, line := parseCommand(raw)
		ev := sess.recordLine(verb, raw)
		if verb != "" {
			srv.bumpCmdCount(verb)
			srv.fireEvent(ev)
		}
		args := commandArgs(line, verb)

		switch verb {
		case "EHLO", "HELO":
			step := nextStep(script.OnEHLO, &cur.ehlo)
			if !writeStep(srv, conn, step, defaultEHLOReply(effectiveCaps(script, tlsActive))) {
				return
			}
		case "STARTTLS":
			if script.TLS == nil {
				if !writeStep(srv, conn, Step{}, "502 5.5.1 Command not implemented") {
					return
				}
				continue
			}
			if tlsActive {
				if !writeStep(srv, conn, Step{}, "503 5.5.1 already using TLS") {
					return
				}
				continue
			}
			if !writeStep(srv, conn, Step{}, "220 2.0.0 Ready to start TLS") {
				return
			}
			tconn := tls.Server(conn, script.TLS)
			if err := tconn.Handshake(); err != nil {
				return
			}
			conn = tconn
			r = bufio.NewReader(conn)
			tlsActive = true
		case "MAIL":
			sess.startMail(args)
			step := nextStep(script.OnMAIL, &cur.mail)
			if !writeStep(srv, conn, step, "250 2.1.0 OK") {
				return
			}
		case "RCPT":
			sess.addRcpt(args)
			step := nextStep(script.OnRCPT, &cur.rcpt)
			if !writeStep(srv, conn, step, "250 2.1.5 OK") {
				return
			}
		case "DATA":
			step := nextStep(script.OnDATA, &cur.data)
			reply := step.Reply
			if reply == "" {
				reply = "354 Start mail input; end with <CRLF>.<CRLF>"
			}
			if !writeStep(srv, conn, step, reply) {
				return
			}
			// A DATA the script refused is refused for real: no body
			// follows a non-3yz reply, so the next line the client sends
			// is a command, exactly as on a real server. Reading a body
			// anyway would swallow it.
			if !strings.HasPrefix(reply, "3") {
				continue
			}
			if !consumeDataBody(srv, conn, r, sess, script) {
				return
			}
			sess.finishMessage()
			eod := nextStep(script.OnEOD, &cur.eod)
			if !writeStep(srv, conn, eod, "250 2.0.0 OK: queued") {
				return
			}
		case "RSET":
			sess.resetTransaction()
			step := nextStep(script.OnRSET, &cur.rset)
			if !writeStep(srv, conn, step, "250 2.0.0 OK") {
				return
			}
		case "QUIT":
			step := nextStep(script.OnQUIT, &cur.quit)
			writeStep(srv, conn, step, "221 2.0.0 Bye")
			return
		case "NOOP":
			if !writeStep(srv, conn, Step{}, "250 2.0.0 OK") {
				return
			}
		default:
			if !writeStep(srv, conn, Step{}, "500 5.5.1 unrecognized command") {
				return
			}
		}
	}
}

// commandArgs returns the part of line after its leading verb token,
// whitespace-trimmed, e.g. commandArgs("MAIL FROM:<a@b>", "MAIL") ==
// "FROM:<a@b>".
func commandArgs(line, verb string) string {
	if len(line) < len(verb) {
		return ""
	}
	return strings.TrimSpace(line[len(verb):])
}

// defaultEHLOReply builds the standard 250- capability block for an
// EHLO/HELO reply out of the script's advertised capabilities.
func defaultEHLOReply(caps []string) string {
	lines := make([]string, 0, len(caps)+1)
	lines = append(lines, "250-fakesmtp")
	for _, c := range caps {
		lines = append(lines, "250-"+c)
	}
	last := len(lines) - 1
	lines[last] = "250 " + strings.TrimPrefix(lines[last], "250-")
	return strings.Join(lines, "\r\n")
}

// parseCommand extracts the uppercased verb (first whitespace-delimited
// token) from a raw command line, along with the line with its terminator
// stripped.
func parseCommand(raw []byte) (verb string, line string) {
	line = strings.TrimSuffix(string(raw), "\n")
	line = strings.TrimSuffix(line, "\r")
	verb = line
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		verb = line[:i]
	}
	return strings.ToUpper(verb), line
}

// consumeDataBody reads lines until the DATA terminator ("." alone on a
// line), recording each body line (transcript + wire body) via sess, and
// firing the script's MidBody steps once it has read that many lines. It
// reports whether the terminator was reached without a read error — a
// MidBody step that closes the connection ends the body early, exactly
// like a read error.
func consumeDataBody(srv *Server, conn net.Conn, r *bufio.Reader, sess *sessionState, script Script) bool {
	midAt := script.MidBodyLines
	if midAt <= 0 {
		midAt = 1
	}

	lines := 0
	for {
		raw, err := r.ReadBytes('\n')
		if err != nil {
			return false
		}
		if isDataTerminator(raw) {
			return true
		}
		sess.recordBodyLine(raw)

		lines++
		if lines != midAt {
			continue
		}
		for _, step := range script.MidBody {
			if step.Action == ActReply && step.Reply == "" {
				continue // nothing scripted to say
			}
			if !writeStep(srv, conn, step, step.Reply) {
				return false
			}
		}
	}
}

// isDataTerminator reports whether a raw line is exactly the DATA
// end-of-body marker: the 3 bytes ".\r\n", strict CRLF only. A bare-LF
// ".\n" is NOT a terminator — it's malformed body content, not the end
// of the message (accepting it would truncate the body and desync the
// session: whatever the client sends next gets parsed as commands).
func isDataTerminator(raw []byte) bool {
	return string(raw) == ".\r\n"
}

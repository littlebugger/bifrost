package fakesmtp

import "net"

// DownMode simulates a backend outage at the accept boundary. It only
// affects connections accepted from the moment SetDown is called onward;
// sessions already established keep running normally.
type DownMode int

// DownUp is the default: normal operation. The others simulate a backend
// outage in a different, client-observable way.
const (
	DownUp             DownMode = iota // normal operation (default)
	DownListenerClosed                 // listener closed: dialing is refused
	DownAcceptThenRST                  // TCP accepted, then immediately reset
	DownAcceptThenHang                 // TCP accepted, then nothing (no banner) until Stop
)

// SetDown puts the server into mode for every connection accepted from
// now on.
func (srv *Server) SetDown(mode DownMode) {
	srv.mu.Lock()
	srv.downMode = mode
	ln := srv.ln
	srv.mu.Unlock()

	if mode == DownListenerClosed {
		closeQuietly(ln)
	}
}

// SetUp restores normal operation, undoing any SetDown.
func (srv *Server) SetUp() {
	srv.mu.Lock()
	was := srv.downMode
	srv.downMode = DownUp
	srv.mu.Unlock()

	if was == DownListenerClosed {
		srv.relisten()
	}
}

// SetScript replaces the script used for connections accepted from now
// on. Sessions already established keep running against the script they
// started with (see acceptLoop's per-accept snapshot).
func (srv *Server) SetScript(s Script) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.script = s
}

// OnEvent registers fn to be called synchronously, in registration order,
// for every command-verb event across every session. Intended as a
// deterministic chaos trigger, e.g. "fire on the third MAIL".
//
// fn runs inline on the session's own goroutine, between reading a
// command and dispatching it: it must not block, and it must never call
// Stop — Stop waits (wg.Wait) for every session goroutine to finish,
// including the one currently blocked running fn, so calling it from
// inside fn deadlocks. SetDown and SetScript are both safe to call from
// fn; to do more, hand the Event to a buffered channel and act on it from
// a different goroutine.
func (srv *Server) OnEvent(fn func(Event)) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.eventHooks = append(srv.eventHooks, fn)
}

// fireEvent invokes every registered hook with ev, in registration order.
func (srv *Server) fireEvent(ev Event) {
	srv.mu.Lock()
	hooks := make([]func(Event), len(srv.eventHooks))
	copy(hooks, srv.eventHooks)
	srv.mu.Unlock()

	for _, h := range hooks {
		h(ev)
	}
}

// relisten opens a fresh listener on the server's original address and
// restarts the accept loop. It recovers from DownListenerClosed.
func (srv *Server) relisten() {
	srv.mu.Lock()
	if srv.stopped {
		srv.mu.Unlock()
		return
	}
	addr := srv.addr
	srv.mu.Unlock()

	// net.Listen is a syscall: srv.mu is deliberately not held across it.
	// That leaves a window where a concurrent Stop can run to completion
	// while we're dialing, so stopped is re-checked below before this
	// listener (and the goroutine/wg.Add that go with it) is committed.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		srv.t.Errorf("fakesmtp: relisten %s: %v", addr, err)
		return
	}

	srv.mu.Lock()
	if srv.stopped {
		srv.mu.Unlock()
		// Stop ran while we were dialing: it already closed the listener
		// it knew about and returned. Resurrecting this one — or adding
		// to wg after Stop's Wait may have already returned — would leak
		// a listener and hang or misuse the WaitGroup. Close it and bail.
		closeQuietly(ln)
		return
	}
	srv.ln = ln
	srv.wg.Add(1)
	srv.mu.Unlock()

	go srv.acceptLoop(ln)
}

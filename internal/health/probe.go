package health

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/revolee/bifrost/internal/backend"
	"github.com/revolee/bifrost/internal/config"
	"github.com/revolee/bifrost/internal/smtpwire"
)

// maxProbeLine and maxProbeTotal bound a probe's own hand-rolled reply
// reads (L0/L1), the same generous-but-defensive limits internal/backend
// uses for its handshake reads.
const (
	maxProbeLine  = 4096
	maxProbeTotal = 65536
)

// probeResult is one probe attempt's verdict. ok is the op-state input
// (fed to fsm.recordActive); incompatible is the orthogonal
// capability-superset verdict (fed to serverHealth.incompatible) — a
// backend can be ok=true, incompatible=true (EHLO succeeded, capability
// set fell short). reason is a short label for logs and tests, empty on
// success.
type probeResult struct {
	ok           bool
	incompatible bool
	reason       string
}

// runProbe is the real ladder entry point: it resolves the dial address
// (honoring the check.port override), enforces the whole-probe budget
// (3x the per-step timeout), and dispatches on params.Level.
//
// A port override means the probe is hitting a dedicated health socket,
// not the traffic listener — PROJECT.md doesn't promise that socket
// speaks for the traffic path's EHLO capability set, so capability
// harvesting/superset verdicts are suppressed whenever Port != 0,
// L2/L3 included (L0/L1 never harvest caps regardless).
func runProbe(ctx context.Context, srv *config.Server, params config.CheckParams, requiredCaps []string) probeResult {
	addr, err := probeAddr(srv.Address, params.Port)
	if err != nil {
		return probeResult{reason: "bad-address"}
	}
	if params.Port != 0 {
		requiredCaps = nil
	}

	if params.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 3*params.Timeout)
		defer cancel()
	}

	switch params.Level {
	case "connect":
		return probeConnect(ctx, addr, params.Timeout)
	case "banner":
		return probeBanner(ctx, addr, params.Timeout)
	case "deep":
		return probeDeep(ctx, addr, params, requiredCaps)
	default: // "ehlo", "" (unresolved hand-built CheckParams)
		return probeEhlo(ctx, addr, params, requiredCaps)
	}
}

// probeAddr resolves a probe's actual dial target: the server's traffic
// address, or host-of(address):port when a check.port override is
// configured (k8s tcpSocket-style — PROJECT.md).
func probeAddr(serverAddr string, port int) (string, error) {
	if port == 0 {
		return serverAddr, nil
	}
	host, _, err := net.SplitHostPort(serverAddr)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// probeConnect implements L0: plain TCP connect, zero SMTP bytes
// (k8s tcpSocket-style). Dial succeeds -> healthy; the socket is closed
// immediately without reading the banner. Documented trade-off (see
// PROJECT.md and operations.md): an L0 close on an SMTP port makes MTAs
// log "lost connection after CONNECT" — that noise is why L1+ exist and
// why L0 pairs naturally with the check.port override (probe a
// dedicated health socket instead of the traffic port).
func probeConnect(ctx context.Context, addr string, timeout time.Duration) probeResult {
	conn, res := dialProbe(ctx, addr, timeout)
	if conn == nil {
		return res
	}
	_ = conn.Close()
	return probeResult{ok: true}
}

// probeBanner implements L1: dial, read the greeting (expect 220), QUIT
// politely. Never sends EHLO — a backend that rejects EHLO can still
// pass L1 (see TestProbeEhlo502FailsL2PassesL1).
func probeBanner(ctx context.Context, addr string, timeout time.Duration) probeResult {
	conn, res := dialProbe(ctx, addr, timeout)
	if conn == nil {
		return res
	}
	defer func() { _ = conn.Close() }()
	defer watchCtx(ctx, conn)()

	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	rr := smtpwire.NewReplyReader(bufio.NewReader(conn), maxProbeLine, maxProbeTotal)
	code, err := readFinal(rr)
	if err != nil {
		if isTimeout(err) {
			return probeResult{reason: "banner-timeout"}
		}
		return probeResult{reason: "banner-read-error"}
	}
	if code != 220 {
		return probeResult{reason: "wrong-banner"}
	}
	politeQuit(conn, timeout)
	return probeResult{ok: true}
}

// probeEhlo implements L2 (the default level): backend.Dial's handshake
// IS this probe — greeting, EHLO, optional STARTTLS, capability
// superset check against requiredCaps. A clean Dial harvests caps and
// ends the session with QUIT; an *backend.IncompatibleError is a
// successful op-wise probe with the orthogonal Incompatible verdict set.
func probeEhlo(ctx context.Context, addr string, params config.CheckParams, requiredCaps []string) probeResult {
	conn, err := dialForHandshake(ctx, addr, params, requiredCaps)
	if err != nil {
		return handshakeFailure(err)
	}
	conn.Quit()
	return probeResult{ok: true}
}

// probeDeep implements L3: after the L2 handshake, MAIL FROM:<> -> RCPT
// TO:<probe_rcpt|postmaster> expecting 250/251 -> RSET -> QUIT. Never
// DATA. Off by default (PROJECT.md): a 450 on RCPT (greylisting a probe
// sender real MTAs have never seen mail from is common) reads as a
// plain probe failure here, not a soft-pass — operators who enable deep
// checks against a greylisting backend should expect and tune around
// this.
func probeDeep(ctx context.Context, addr string, params config.CheckParams, requiredCaps []string) probeResult {
	conn, err := dialForHandshake(ctx, addr, params, requiredCaps)
	if err != nil {
		return handshakeFailure(err)
	}

	conn.SetCommandClass(backend.MailRcpt)
	if err := conn.SendLine([]byte("MAIL FROM:<>\r\n")); err != nil {
		conn.Abort()
		return probeResult{reason: "mail-send-error"}
	}
	code, err := readFinal(conn.Replies())
	if err != nil || code/100 != 2 {
		conn.Abort()
		return probeResult{reason: "mail-rejected"}
	}

	rcpt := params.ProbeRcpt
	if rcpt == "" {
		rcpt = "postmaster"
	}
	if err := conn.SendLine([]byte("RCPT TO:<" + rcpt + ">\r\n")); err != nil {
		conn.Abort()
		return probeResult{reason: "rcpt-send-error"}
	}
	code, err = readFinal(conn.Replies())
	if err != nil || (code != 250 && code != 251) {
		conn.Abort()
		return probeResult{reason: "rcpt-rejected"}
	}

	_ = conn.SendLine([]byte("RSET\r\n"))
	_, _ = readFinal(conn.Replies()) // best-effort; a probe never latches on RSET's own reply
	conn.Quit()
	return probeResult{ok: true}
}

// dialForHandshake is L2/L3's shared setup: backend.Dial with the
// per-step timeout budget wired into every phase of config.Timeouts, plus
// a starttls-verify ServerName defaulted from the dialed host and the
// pool's own CA roots (params.CAPool — backend_tls_ca, parsed once per
// config load by internal/config, so a probe verifies against exactly the
// roots the traffic path does).
//
// The ServerName is the dialed host rather than the pool's
// backend_tls_server_name because a probe may be pointed at a different
// port, and CheckParams carries no name of its own; for the common case
// (no port override, address is the name in the certificate) the two are
// the same string.
func dialForHandshake(ctx context.Context, addr string, params config.CheckParams, requiredCaps []string) (*backend.Conn, error) {
	opts := backend.Opts{
		EhloName:     params.EhloName,
		TLSMode:      params.TLS,
		RequiredCaps: requiredCaps,
		Timeouts:     probeTimeouts(params.Timeout),
	}
	if params.TLS == "starttls-verify" {
		cfg := &tls.Config{RootCAs: params.CAPool, MinVersion: tls.VersionTLS12}
		if host, _, err := net.SplitHostPort(addr); err == nil {
			cfg.ServerName = host
		}
		opts.TLSConfig = cfg
	}
	return backend.Dial(ctx, &config.Server{Address: addr}, opts)
}

// handshakeFailure classifies a backend.Dial error into a probeResult:
// IncompatibleError is a successful (op-wise) probe with the capability
// verdict set; anything else is a plain probe failure.
func handshakeFailure(err error) probeResult {
	var incompat *backend.IncompatibleError
	if errors.As(err, &incompat) {
		return probeResult{ok: true, incompatible: true, reason: "incompatible: " + err.Error()}
	}
	return probeResult{reason: "handshake: " + err.Error()}
}

// probeTimeouts turns one per-step probe timeout into the config.Timeouts
// shape backend.Dial/Conn expect, applying it uniformly to every phase.
func probeTimeouts(step time.Duration) config.Timeouts {
	return config.Timeouts{
		BackendConnect:   step,
		BackendHandshake: step,
		BackendMailReply: step,
		Backend354Wait:   step,
		DataProgress:     step,
		BackendFinalDot:  step,
	}
}

// dialProbe is L0/L1's shared TCP connect step (L2/L3 dial through
// backend.Dial instead, which has its own equivalent connect phase).
func dialProbe(ctx context.Context, addr string, timeout time.Duration) (net.Conn, probeResult) {
	dctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", addr)
	if err != nil {
		return nil, probeResult{reason: "connect-refused"}
	}
	return conn, probeResult{}
}

// watchCtx closes conn if ctx is done before the returned stop func is
// called — mirrors backend.Conn.handshake's own ctx-watcher so a
// cancelled probe (context timeout, or later an operator's
// SetAdminState(Maint)) interrupts a blocked read promptly instead of
// waiting out whatever deadline is separately armed on conn.
func watchCtx(ctx context.Context, conn net.Conn) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// isTimeout reports whether err is a network timeout (a deadline or a
// cancelled context surfacing through a blocked read/dial).
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// readFinal drains a (possibly multiline) reply down to its final line
// and returns just the code — used by L1's banner read and L3's
// MAIL/RCPT/RSET reads, where a probe only ever cares about the verdict.
func readFinal(rr *smtpwire.ReplyReader) (code int, err error) {
	for {
		_, code, final, err := rr.Next()
		if err != nil {
			return 0, err
		}
		if final {
			return code, nil
		}
	}
}

// politeQuit sends a best-effort QUIT and gives the peer a bounded
// chance to reply, exactly like backend.Conn.Quit but over the bare
// net.Conn L1 uses (before any backend.Conn exists).
func politeQuit(conn net.Conn, timeout time.Duration) {
	d := timeout
	if d <= 0 || d > 2*time.Second {
		d = 2 * time.Second
	}
	if err := conn.SetDeadline(time.Now().Add(d)); err != nil {
		return
	}
	if _, err := conn.Write([]byte("QUIT\r\n")); err != nil {
		return
	}
	rr := smtpwire.NewReplyReader(bufio.NewReader(conn), maxProbeLine, maxProbeTotal)
	_, _ = readFinal(rr)
}

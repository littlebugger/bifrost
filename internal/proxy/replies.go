// Package proxy implements Bifrost's client leg: the session engine that
// owns everything before a backend exists (banner, EHLO/HELO, STARTTLS,
// pre-attach command states, limits) and hands each mail transaction to a
// TxnHandler at MAIL FROM.
//
// The division of labour is /PROJECT.md's central R3-vs-R4 resolution:
// the session owns the *session* framing and synthesizes those replies
// itself; everything a backend says inside a transaction is relayed
// byte-verbatim by the handler (epic-05's relay).
package proxy

import "strings"

// This file is the closed enum of every reply Bifrost ever speaks for
// itself — decision D8 and the audit surface for requirement R4. It is
// the /PROJECT.md Transparency Contract table in code, and no SMTP reply
// literal may appear anywhere else in this package: if a reply is not
// here, it came from a backend, verbatim.
//
// Enhanced status codes (RFC 3463) are always attached, whether or not
// ENHANCEDSTATUSCODES is in the advertised capability set: the contract
// table specifies them per row, and a client that does not understand
// them reads only the three-digit code anyway.
//
// The 421-vs-451 split is normative: 451 for transaction-scoped failures
// (the session stays open, the client may retry on the same connection),
// 421 only for connection-scoped events, always followed by a close.
// Never 554 for an infrastructure failure — a permanent code would
// bounce mail a healthy backend would have taken.
const (
	// Session framing and pre-attach command states.
	RplBadSequence    = "503 5.5.1 Bad sequence of commands\r\n"
	RplUnknownCmd     = "500 5.5.1 Command not recognized\r\n"
	RplNotImplemented = "502 5.5.1 Command not implemented\r\n"
	RplVrfy           = "252 2.5.2 Cannot VRFY user, but will accept message and attempt delivery\r\n"
	RplHelp           = "214 2.0.0 See RFC 5321 for the supported commands\r\n"
	RplOK             = "250 2.0.0 OK\r\n"
	RplBye            = "221 2.0.0 Bye\r\n"

	RplEhloSyntax = "501 5.5.4 Syntax error: EHLO requires a domain\r\n"

	// STARTTLS (RFC 3207). A 454 has no trigger in v1 — the certificate
	// is loaded before the listener opens — so it is deliberately absent.
	RplStartTLSReady  = "220 2.0.0 Ready to start TLS\r\n"
	RplStartTLSParams = "501 5.5.4 Syntax error: STARTTLS takes no parameters\r\n"

	// Plaintext pipelined behind STARTTLS: emitted instead of the 220,
	// before any handshake, and followed by a close. STARTTLS is a
	// pipelining sync point (RFC 3207/2920), so anything already behind
	// it is a command-injection attempt (CVE-2011-0411 class) and is
	// discarded without ever being interpreted.
	RplStartTLSPipelined = "421 4.7.0 Pipelined data after STARTTLS, closing connection\r\n"

	// Malformed input. Both are 500 5.5.2 and both keep the session in
	// sync; the bare-LF line is never relayed anywhere (SMTP-smuggling
	// defense, CVE-2023-51764 class).
	RplBareLf      = "500 5.5.2 Bare LF is not a valid line terminator\r\n"
	RplLineTooLong = "500 5.5.2 Command line too long\r\n"

	// Connection-scoped failures: 421, always followed by a close.
	RplIdleTimeout      = "421 4.4.2 Idle timeout, closing connection\r\n"
	RplSessionLifetime  = "421 4.4.2 Session lifetime exceeded, closing connection\r\n"
	RplPipelineOverflow = "421 4.7.0 Too many pipelined commands, closing connection\r\n"
	RplAcceptOverload   = "421 4.3.2 Too many connections, try again later\r\n"
	RplShuttingDown     = "421 4.3.0 Service shutting down, closing connection\r\n"

	// Transaction-scoped failures: 451, the session survives. Used by
	// the relay engine (epic-05) and the pool limits (epic-08).
	RplNoBackend      = "451 4.4.1 No backend available, try again later\r\n"
	RplBackendLost    = "451 4.4.1 Backend connection lost\r\n"
	RplBackendTimeout = "451 4.4.2 Backend timeout\r\n"
	RplAllBusy        = "451 4.3.2 All backends busy, try again later\r\n"

	// The one sanctioned rewrite (contract row "Backend 421 while
	// attached"): a backend's 421 announces that a connection is closing,
	// and relaying it verbatim would tell the client its own session is
	// over. The event is transaction-scoped instead — this reply, and the
	// backend leg dropped — so the client keeps its session and its right
	// to retry on it.
	RplBackendClosing = "451 4.4.2 Backend closed the transaction\r\n"
)

// RplBanner is the connection greeting: "220 <hostname> ESMTP".
func RplBanner(hostname string) string {
	return "220 " + hostname + " ESMTP\r\n"
}

// RplHelo is the HELO reply — a bare 250 with the hostname and no
// extensions, because HELO has no way to carry them.
func RplHelo(hostname string) string {
	return "250 " + hostname + "\r\n"
}

// RplEhlo is the EHLO reply: the hostname followed by one line per
// advertised capability, the last line switching from "250-" to "250 ".
// caps is the statically configured set (decision D10) as the session
// currently offers it; an empty set collapses to a single 250 line.
func RplEhlo(hostname string, caps []string) string {
	lines := append([]string{hostname}, caps...)
	var b strings.Builder
	for i, line := range lines {
		sep := "-"
		if i == len(lines)-1 {
			sep = " " // only the last line is final
		}
		b.WriteString("250" + sep + line + "\r\n")
	}
	return b.String()
}

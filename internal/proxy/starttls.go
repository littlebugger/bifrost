package proxy

import (
	"bufio"
	"crypto/tls"
)

// startTLS answers a STARTTLS command and, when it is allowed, upgrades
// the client leg in place (RFC 3207).
//
// The refusal rows of the Transparency Contract, in the order they are
// checked: 502 when no certificate is configured (the extension is not
// advertised, so it is simply unknown), 501 when parameters are present,
// 503 once TLS is already active or once a mail transaction has been
// attempted on this session. There is no 454 row — the certificate is
// loaded before the listener opens, so "TLS temporarily unavailable" has
// no trigger in v1.
//
// A successful upgrade resets all session state: the client must send a
// fresh EHLO, and the reply to it no longer advertises STARTTLS.
func (s *Session) startTLS(args []byte) (stop bool, err error) {
	switch {
	case s.tlsActive:
		return false, s.write(RplBadSequence)
	case s.tlsCfg == nil:
		return false, s.write(RplNotImplemented)
	case len(args) > 0:
		return false, s.write(RplStartTLSParams)
	case s.mailSeen:
		// RFC 3207 forbids STARTTLS inside a transaction. Bifrost
		// refuses it from the first MAIL onwards: a TxnHandler owns the
		// connection while a transaction runs, so the dispatcher could
		// never see a truly mid-transaction STARTTLS, and upgrading
		// after plaintext mail has already flowed buys nothing. A fresh
		// EHLO resets this along with the rest of the session state.
		return false, s.write(RplBadSequence)
	}

	// Anything already buffered arrived behind the STARTTLS command,
	// i.e. as plaintext the client is about to pretend was encrypted
	// (the CVE-2011-0411 command-injection class). STARTTLS is a
	// pipelining sync point, so this is answered *instead* of the 220 —
	// no handshake, no 220, and the buffered bytes are discarded without
	// ever being interpreted.
	if s.br.Buffered() > 0 {
		s.lg.Warn("plaintext pipelined after STARTTLS, closing session", "client", s.clientIP)
		return true, s.goodbye(RplStartTLSPipelined)
	}

	if err := s.write(RplStartTLSReady); err != nil {
		return true, err
	}

	// The idle timer bounds the handshake; tls.Conn honors the
	// underlying connection's deadline.
	if err := s.armDeadline(); err != nil {
		return true, err
	}
	tc := tls.Server(s.conn, s.tlsCfg)
	if err := tc.Handshake(); err != nil {
		// A failed handshake leaves no channel to answer on.
		s.lg.Warn("client TLS handshake failed", "client", s.clientIP, "error", err)
		return true, nil
	}

	s.conn = tc
	s.br = bufio.NewReaderSize(tc, maxCommandLine)
	s.bw = bufio.NewWriter(tc)
	s.tlsActive = true
	s.reset()
	return false, nil
}

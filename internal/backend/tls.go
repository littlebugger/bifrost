package backend

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"

	"github.com/revolee/bifrost/internal/smtpwire"
)

// errStartTLSNotOffered is wrapped in a *HandshakeError when opts.TLSMode
// requires STARTTLS but the backend's EHLO never advertised it.
var errStartTLSNotOffered = errors.New("STARTTLS not offered")

// startTLS upgrades the connection in place per RFC 3207: send STARTTLS,
// perform the TLS handshake, then a mandatory fresh EHLO whose
// capability set replaces the pre-TLS one — the post-TLS capabilities
// are what actually govern the connection from here on.
//
// TLSMode, not opts.TLSConfig's own fields, decides verification:
// "starttls" always skips certificate verification (opportunistic
// encryption has no ServerName/CA to check), while "starttls-verify"
// verifies using whatever the caller configured (ServerName + RootCAs,
// resolved from the pool's backend_tls_ca/backend_tls_server_name).
// opts.TLSConfig is cloned rather than mutated, since callers may reuse
// one *tls.Config across many Dial calls.
func (c *Conn) startTLS(opts Opts) error {
	if !c.caps.Has("STARTTLS") {
		return &HandshakeError{Addr: c.addr, Stage: "starttls", Err: errStartTLSNotOffered}
	}

	if _, err := c.conn.Write([]byte("STARTTLS\r\n")); err != nil {
		return &HandshakeError{Addr: c.addr, Stage: "starttls", Err: err}
	}
	_, code, _, err := c.rr.Next()
	if err != nil {
		return &HandshakeError{Addr: c.addr, Stage: "starttls", Err: err}
	}
	if code != 220 {
		return &HandshakeError{Addr: c.addr, Stage: "starttls", Err: fmt.Errorf("STARTTLS rejected: code %d", code)}
	}

	cfg := opts.TLSConfig
	if cfg == nil {
		cfg = &tls.Config{}
	} else {
		cfg = cfg.Clone()
	}
	cfg.InsecureSkipVerify = opts.TLSMode == "starttls"

	tlsConn := tls.Client(c.conn, cfg)
	if err := tlsConn.Handshake(); err != nil {
		return &HandshakeError{Addr: c.addr, Stage: "tls-handshake", Err: err}
	}
	c.conn = tlsConn
	c.br = bufio.NewReader(tlsConn)
	c.rr = smtpwire.NewReplyReader(c.br, maxReplyLine, maxReplyTotal)

	caps, err := c.doEHLO(opts.EhloName)
	if err != nil {
		return &HandshakeError{Addr: c.addr, Stage: "ehlo-after-tls", Err: err}
	}
	c.caps = caps
	return nil
}

package backend

import (
	"encoding/base64"
	"fmt"
	"time"
)

// AuthError reports that the backend rejected (or otherwise failed to
// answer cleanly to) AUTH PLAIN. Code is the backend's final reply code
// to the AUTH command; 0 when the failure was a write/read error instead
// of a reply.
type AuthError struct {
	Addr string
	Code int
	Err  error
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("backend: auth with %s failed: code %d: %v", e.Addr, e.Code, e.Err)
}
func (e *AuthError) Unwrap() error { return e.Err }

// Permanent reports whether the backend's rejection is permanent (5xx)
// rather than transient (4xx), per RFC 5321 §4.2.1.
func (e *AuthError) Permanent() bool { return e.Code >= 500 }

// authenticate originates "AUTH PLAIN <b64>" against the backend using
// the pool's own credentials, called by Dial right after checkSuperset.
// Dial's handshake deadline was already cleared by the time this runs
// (handshake's final SetDeadline(time.Time{})), so a fresh deadline from
// the same BackendHandshake budget is armed around this round trip and
// cleared after, mirroring handshake's own bound-then-clear pattern.
func (c *Conn) authenticate(username, password string) error {
	if d := c.timeouts.BackendHandshake; d > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(d)); err != nil {
			return &AuthError{Addr: c.addr, Err: err}
		}
	}
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()

	cred := base64.StdEncoding.EncodeToString([]byte("\x00" + username + "\x00" + password))
	if _, err := c.conn.Write([]byte("AUTH PLAIN " + cred + "\r\n")); err != nil {
		return &AuthError{Addr: c.addr, Err: fmt.Errorf("write AUTH: %w", err)}
	}

	var code int
	for {
		_, cd, final, err := c.rr.Next()
		if err != nil {
			return &AuthError{Addr: c.addr, Err: fmt.Errorf("read AUTH reply: %w", err)}
		}
		code = cd
		if final {
			break
		}
	}
	if code != 235 {
		return &AuthError{Addr: c.addr, Code: code, Err: fmt.Errorf("AUTH rejected: code %d", code)}
	}
	return nil
}

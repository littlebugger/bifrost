package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/smtpwire"
)

// parsePlain decodes a PLAIN initial-response/continuation payload
// (RFC 4616): base64 of authzid \x00 authcid \x00 passwd.
func parsePlain(b64 []byte) (authzid, authcid, password string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b64)))
	if err != nil {
		return "", "", "", false
	}
	parts := bytes.Split(raw, []byte{0})
	if len(parts) != 3 {
		return "", "", "", false
	}
	return string(parts[0]), string(parts[1]), string(parts[2]), true
}

// verifyPlain reports whether (authcid, password) matches a configured
// user: sha256(salt || password) in constant time. Unknown users burn
// the same work against a dummy so timing does not reveal valid names.
func verifyPlain(users []config.AuthUser, authcid, password string) bool {
	// Dummy entry: unknown users cost the same hash+compare as known
	// ones, so response timing does not enumerate valid names.
	match := config.AuthUser{Salt: "-", HashedPassword: strings.Repeat("0", 64)}
	found := 0
	for i := range users {
		if users[i].Name == authcid {
			match, found = users[i], 1
		}
	}
	sum := sha256.Sum256([]byte(match.Salt + password))
	got := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(match.HashedPassword))&found == 1
}

// auth answers AUTH (RFC 4954). It is local termination only: bifrost
// verifies the client itself and never relays anything AUTH-related to a
// backend. PLAIN (RFC 4616) is the only mechanism, and only over TLS —
// strict, so it never runs in cleartext regardless of what the client
// negotiated capabilities imply.
func (s *Session) auth(ctx context.Context, args []byte) (stop bool, err error) {
	if s.cfg.Listener.Auth == nil {
		return false, s.write(RplNotImplemented) // decision D6 unchanged when not configured
	}
	if !s.greeted || s.authed {
		return false, s.write(RplBadSequence)
	}
	if !s.tlsActive {
		return false, s.write(RplAuthEncryption) // strict: PLAIN never in cleartext
	}

	mech, initial, _ := bytes.Cut(bytes.TrimSpace(args), []byte(" "))
	if !strings.EqualFold(string(mech), "PLAIN") {
		return false, s.write(RplAuthMechanism)
	}

	if len(initial) == 0 {
		if err := s.write(RplAuthContinue); err != nil {
			return false, err
		}
		// readCommand, not a bare armDeadline+ReadCommandLine: it also
		// installs the drain watcher (wakeOnCancel), so a shutdown that
		// lands while a client is parked at "334 " gets the same prompt
		// wakeup an ordinary command read would get, instead of sitting
		// out the full idle deadline.
		line, rerr := s.readCommand(ctx)
		if rerr != nil {
			if ctx.Err() != nil {
				// Mirrors Run's own loop (session.go): the read was
				// interrupted BY the drain (readCommand's wakeOnCancel), so
				// this is the shutdown row of the contract, not an idle
				// client: 421 4.3.0, not 421 4.4.2.
				return true, s.goodbye(RplShuttingDown)
			}
			return s.authReadError(rerr)
		}
		initial = bytes.TrimRight(line, "\r\n")
		if string(initial) == "*" {
			return false, s.write(RplAuthCancelled)
		}
	}

	authzid, authcid, password, ok := parsePlain(initial)
	if !ok {
		return false, s.write(RplAuthMalformed)
	}
	if !verifyPlain(s.cfg.Listener.Auth.Users, authcid, password) {
		s.authFails++
		s.lg.Warn("client auth failed", "client", s.clientIP, "authcid", authcid)
		if s.authFails >= 3 {
			return true, s.goodbye(RplAuthTooMany)
		}
		return false, s.write(RplAuthFailed)
	}
	if authzid != "" {
		s.lg.Info("authzid ignored", "client", s.clientIP, "authzid", authzid)
	}
	s.authed, s.authnID = true, authcid
	s.lg.Info("client authenticated", "client", s.clientIP, "authn", authcid)
	return false, s.write(RplAuthOK)
}

// authReadError turns a failed read of an AUTH continuation line into a
// reply: the two in-sync command violations (bare LF, over-long) are the
// same client mistake here they are anywhere else in the protocol — just
// an invalid response to the challenge, not answered any differently —
// and the session stays open. Anything else (a timeout, the client
// hanging up) is not a violation of the AUTH exchange specifically, so
// it defers to the session's own read-error rules.
func (s *Session) authReadError(err error) (stop bool, _ error) {
	switch {
	case errors.Is(err, smtpwire.ErrBareLF), errors.Is(err, smtpwire.ErrLineTooLong):
		return false, s.write(RplAuthMalformed)
	default:
		return s.handleReadError(err)
	}
}

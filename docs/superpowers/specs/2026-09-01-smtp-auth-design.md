# Double-Sided SMTP AUTH (PLAIN) — Design

Approved in brainstorming 2026-09-01. Decisions locked with the user:
PLAIN only · strict TLS gating (no plaintext-auth escape hatch) ·
auth configured = auth required · salted-SHA256 credential store
(kumo `inbound_auth.toml` parity).

## Goal

Bifrost terminates SMTP AUTH on the client leg (verifies PLAIN
credentials against listener config) and originates AUTH on the backend
leg (authenticates with per-pool credentials). The two halves are
independent: either can be configured without the other.

Passthrough AUTH stays impossible by design: auth is connection-scoped
while bifrost binds a fresh backend per transaction (PROJECT.md, D6
rationale). This feature amends D6: local termination, not relay.

## Non-goals (v1.1)

- Mechanisms other than PLAIN (LOGIN, CRAM-MD5, XOAUTH2).
- Per-user routing or authorization beyond the relay gate.
- Auth metrics (logs only).
- Propagating the client's identity to the backend (bifrost always
  sends its own pool credentials; a client's `AUTH=` MAIL parameter
  still relays verbatim — the backend stays the enforcer).

## Config surface

```hcl
listener {
  starttls { ... }              # REQUIRED when auth is present
  auth {
    user "rttskr-team" {
      salt            = "1af90..."   # nonempty
      hashed_password = "d989..."    # sha256(salt || password), 64 hex chars
    }
  }
}

pool "outgoing" {
  backend_tls = "starttls"      # "none" + auth = config error
  auth {
    username = "rttskr-team"
    password = "pa55w0rd"       # plaintext by necessity: bifrost must SEND PLAIN
  }
}
```

Resolved types (internal/config/config.go):

```go
// on Listener
Auth *ListenerAuth   // nil: client-leg auth disabled

type ListenerAuth struct {
    Users []AuthUser
}

type AuthUser struct {
    Name           string // the user block label; the SASL authcid
    Salt           string
    HashedPassword string // 64 lowercase hex chars
}

// on Pool
Auth *PoolAuth       // nil: backend-leg auth disabled

type PoolAuth struct {
    Username string
    Password string
}
```

`AUTH` never appears in `listener.capabilities`: client-leg
advertisement is derived (auth block + TLS state), and the listener's
capability list stays what backends must superset — client auth is
bifrost-local and never a backend requirement. The backend-leg AUTH
requirement is driven by pool credentials instead (below).

### Validation rules (internal/config/validate.go)

Errors:
- `listener.auth` present without `listener.starttls`: strict TLS gating
  would make AUTH permanently unadvertisable.
- `auth` block with zero `user` blocks.
- Duplicate `user` labels.
- `user` with empty salt, or `hashed_password` not exactly 64 hex chars
  (case-insensitive input, normalized to lowercase at load).
- `pool.auth` with empty username or password.
- `pool.auth` with `backend_tls = "none"`: never send PLAIN over
  cleartext (kumo-class servers refuse to advertise AUTH there anyway).
- NUL bytes in any credential field; CR/LF in usernames.

## Client leg (internal/proxy)

Session state: `authed bool`, `authnID string`, `authFails int`.
`authed` survives EHLO/HELO reset (RFC 4954 does not tie it to the
greeting; TLS cannot restart, so no downgrade path exists).

EHLO advertisement (`Session.capabilities()`): append `"AUTH PLAIN"`
iff `Listener.Auth != nil && s.tlsActive && !s.authed`.

`AUTH` dispatch (replaces the D6 502 branch when auth is configured):

| condition | reply |
|---|---|
| no `listener.auth` configured | 502 (unchanged v1 behavior) |
| not greeted | 503 RplBadSequence |
| plaintext session | 538 5.7.11 Encryption required |
| already authenticated | 503 RplBadSequence |
| mechanism ≠ PLAIN (case-insensitive) | 504 5.5.4 |
| `AUTH PLAIN` w/o initial response | `334 ` then read one continuation line |
| continuation `*` | 501 (cancelled) |
| bad base64 / not exactly 2 NULs | 501 5.5.2 |
| unknown user or wrong password | 535 5.7.8 |
| 3rd failed attempt | 421 4.7.0 + close |
| success | 235 2.7.0; set authed/authnID |

Verification: `sha256(salt || password)` compared with
`crypto/subtle.ConstantTimeCompare` against the stored hex; unknown
users burn the same hash+compare against a dummy entry so valid names
are not distinguishable by timing. authzid (first PLAIN part) is
ignored, logged if nonempty.

Relay gate: in `Session.mail()`, `Listener.Auth != nil && !authed` →
530 5.7.0, session stays open. Same gate the outgoing kumod runs.

Continuation line is read with `smtpwire.ReadCommandLine` under the
normal armDeadline window; bare-LF/over-long map to 501 5.5.2 (never
relayed, same smuggling posture as commands).

New closed-enum replies (replies.go — the go/ast audit test enforces
placement):

```go
RplAuthOK         = "235 2.7.0 Authentication succeeded\r\n"
RplAuthContinue   = "334 \r\n"
RplAuthMalformed  = "501 5.5.2 Invalid authentication response\r\n"
RplAuthCancelled  = "501 5.0.0 Authentication cancelled\r\n"
RplAuthMechanism  = "504 5.5.4 Unrecognized authentication type\r\n"
RplAuthRequired   = "530 5.7.0 Authentication required\r\n"
RplAuthFailed     = "535 5.7.8 Authentication credentials invalid\r\n"
RplAuthEncryption = "538 5.7.11 Encryption required for requested authentication mechanism\r\n"
RplAuthTooMany    = "421 4.7.0 Too many failed authentication attempts, closing connection\r\n"
```

Session log line gains `authn=<id>` when authenticated.

## Backend leg (internal/backend)

`Opts` gains `AuthUsername, AuthPassword string` (both empty = no
auth). In `Dial`, after the post-TLS EHLO and `checkSuperset`:

1. Creds configured but the (post-TLS) capability set does not
   advertise PLAIN (`caps["AUTH"]` value, space-split, case-insensitive
   contains "PLAIN") → `IncompatibleError{Missing: []string{"AUTH PLAIN"}}`.
2. Send one round trip: `AUTH PLAIN <base64("\x00user\x00pass")>`.
3. Final code 235 → done. Anything else → new error type:

```go
type AuthError struct {
    Addr string
    Code int    // the backend's final reply code
    Err  error  // reply text context
}
func (e *AuthError) Permanent() bool { return e.Code >= 500 }
```

Verdict mapping (internal/health/probe.go `handshakeFailure`):
`AuthError.Permanent()` → the *incompatible* verdict (bad credentials
must not flap Down/Up on every probe); transient (4xx/other) → plain
probe failure (down). The relay's failover walk treats AuthError like
HandshakeError today: skip the candidate, passive-signal health.

Credential flow: `attach.go dialOpts()` copies pool creds into Opts;
`CheckParams` gains `AuthUsername/AuthPassword` resolved from the pool
(same pattern as CAPool — CheckParams is all internal/health sees of a
pool), so every L2+ probe exercises the real authenticated path and a
bad password surfaces in `/servers` within one probe interval.

## Testing

- fakesmtp: `Script.OnAUTH []Step` queue (default reply
  `235 2.7.0 OK`); AUTH lines hit the recorder like any command, so
  tests assert the exact base64 the balancer sent. TLS-conditional
  advertisement is scripted per-test via `OnEHLO` steps (existing
  pattern: TestDialIncompatibleCapabilitiesPostTLS).
- Unit: PLAIN decode/verify edge cases; advertisement matrix (pre/post
  TLS, pre/post auth); continuation + cancel; failure budget; MAIL
  gate; backend dial happy/missing-AUTH/535/454 taxonomy; prober
  verdicts.
- Integration (test/integration/auth_test.go): client —TLS+AUTH→
  bifrost —STARTTLS+AUTH→ fake backend, byte-exact backend-side AUTH
  assertion; wrong creds on each leg.

## Documentation

- PROJECT.md: amend the "No AUTH in v1" paragraph and D6 row (local
  AUTH termination added; passthrough still excluded), add the new
  synth replies to the Transparency Contract table, note the pool-auth
  → AUTH-PLAIN backend requirement in the capability policy paragraph.
- examples/bifrost.hcl: listener auth user + pool "bulk" auth (bulk is
  the starttls-verify showcase); load_test fixture assertions updated.
- docs/operations.md: auth section (minting credentials via
  `sha256(salt||password)`, kumo `make kumo-credential` parity, verdict
  behavior on bad backend creds).

## Out of repo scope (dev-env follow-up, separate change)

Bifrost's dev-env config gains listener starttls + a `rttskr-team`
user; pool flips to `outgoing:587` / `backend_tls = "starttls"` + auth.
The blocked `:2525` policy carve-out in outgoing/policy/init.lua
becomes unnecessary; `make smarthost-bifrost` stops clearing
bounce-master's credentials.

## Amendment (2026-09-01, user request): password_file

`pool.auth` accepts `password_file` as a mutually exclusive alternative to
`password`, for k8s secret mounts: the file is read at every config load
(so SIGHUP rotation is live), trailing CR/LF trimmed, and the result is
the effective password everywhere `Password` was already consumed.
Unreadable or empty (post-trim) files are validation errors.

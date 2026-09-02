# Bifrost operator guide

This document describes shipped behavior only. Every claim below is
proven by a named test; the test name is given so a change that breaks
the behavior also breaks the citation. See `/PROJECT.md` for the
overall design and `docs/plans/epic-09-observability.md` for the plan
this guide implements.

Everything here describes a running `bifrost` binary. The process itself
— config loading, both listeners, the health checker, the admin plane,
`SIGTERM` drain and `SIGHUP` reload — is exercised end to end by
`test/integration` (tag `integration`), which starts the real built
binary as a child process; the tests named below are the citations.

## Security model

The admin API is **unauthenticated by design**. It must never be
exposed beyond the host it runs on: `internal/config`'s validation
rejects a TCP `admin.bind` that isn't loopback unless the config sets
`allow_remote = true` explicitly (`internal/config/validate.go`,
`validateAdmin`). A `unix://` bind is exempt from that check — a unix
socket's filesystem permissions are the access control, and
`admin.Server.Listen` creates it mode `0600` (`internal/admin/admin.go`,
proven by `TestUnixSocketBind`, `internal/admin/admin_test.go`).

```hcl
admin {
  bind = "127.0.0.1:8081"   # loopback TCP -- fine
  # bind = "unix:///var/run/bifrost/admin.sock"   # also fine, any host
  # bind = "0.0.0.0:8081"                          # rejected unless allow_remote = true
}
```

If no `admin { }` block is configured at all, `internal/config`
warns (never errors) and there is no admin plane to bind — see
PROJECT.md's D15.

## Admin API reference

All bodies and responses are JSON (`Content-Type: application/json`).
Every write endpoint answers `404` for an unknown pool/server and `400`
for a malformed body or an out-of-range value — proven together by
`TestValidation404s400s` (`internal/admin/write_test.go`).

### `GET /servers`

Every server's live health/balance state, read fresh on each request —
proven under concurrent traffic by `TestServersEndpointUnderTraffic`
(`internal/admin/admin_integration_test.go`, run with `-race`).

```console
$ curl -s http://127.0.0.1:8081/servers | jq .
{
  "servers": [
    {
      "pool": "internal",
      "server": "mta1",
      "op": "UP",
      "admin": "READY",
      "override": "AUTO",
      "incompatible": false,
      "weight": 3,
      "in_flight": 2,
      "consec_fail": 0,
      "last_change": "2026-08-18T09:12:03Z",
      "last_probe": {
        "level": "ehlo",
        "result": "ok",
        "latency": "4.2ms",
        "detail": ""
      }
    }
  ]
}
```

Field notes (`internal/admin/servers.go`, `internal/health/fsm.go`):

- `op` — the active-check FSM's own verdict (`UP`/`DOWN`), independent
  of admin state.
- `admin` — `READY` / `DRAIN` / `MAINT`, operator-set.
- `override` — `AUTO` / `FORCE_UP` / `FORCE_DOWN`, operator-forced.
- `incompatible` — the EHLO capability-superset check failed; the
  server is op-wise reachable but excluded from rotation regardless.
- `weight` — the **effective** weight: a runtime override
  (`POST .../weight`) if one is set, else the config's own value
  (`balance.Router.Weight`).
- `in_flight` — current attached transactions on this server.
- `last_change` — RFC3339, when `op` last flipped UP↔DOWN (also
  stamped on the very first probe, so "since when has this been UP" is
  answerable from the first check onward); omitted if no active probe
  has ever completed. This is the admin API's answer to the first 3am
  question, "DOWN since when" — proven by `TestStatusLastChangeAndStateChanges`
  (`internal/health/status_test.go`) and surfaced by `TestServersEndpoint`
  (`internal/admin/admin_test.go`).
- `last_probe` — the most recent **active** probe only; passive
  transport signals never update it. `detail` is the probe's own short
  failure label (e.g. `connect-refused`, `wrong-banner`, or
  `handshake: <error>`) — not a verbatim capture of backend reply
  bytes.

### `GET /stats`

Per-server counters plus balancer-wide totals, both reshaped from the
same Prometheus registry `/metrics` serves — proven by
`TestStatsEndpoint` (`internal/admin/admin_test.go`).

```console
$ curl -s http://127.0.0.1:8081/stats | jq .
{
  "totals": {
    "sessions_active": 12,
    "sessions_total": 8041,
    "duplicate_risk_total": 0
  },
  "servers": [
    {
      "pool": "internal",
      "server": "mta1",
      "weight": 3,
      "in_flight": 2,
      "transactions_total": 5102,
      "backend_dials_ok": 5104,
      "backend_dials_fail": 2
    }
  ]
}
```

### `GET /healthz`

`200 ok` while serving; `503 draining` from the moment a `SIGTERM`
drain begins — proven by `TestHealthzDrainAware`
(`internal/admin/admin_test.go`) and, on the live process, by
`TestDrainSequence` (`test/integration/drain_test.go`), which asserts
that the listener is still accepting connections while `/healthz`
already says 503: that gap is the `lame_duck` window, and it exists so
whatever polls this endpoint can take the process out of rotation before
it stops answering. This is a **process-wide** lame-duck signal,
orthogonal to any individual server's admin state (`POST .../state`
below): draining the whole balancer and draining one backend server are
unrelated axes. See "healthz behind an L4 balancer" below for the bind
implication.

```console
$ curl -si http://127.0.0.1:8081/healthz | head -1
HTTP/1.1 200 OK
```

### `GET /metrics`

Standard Prometheus text exposition
(`github.com/prometheus/client_golang/prometheus/promhttp`) — proven
served by `TestMetricsServed` (`internal/admin/admin_test.go`) and
name-complete by `TestMetricNamesStable`
(`internal/metrics/metrics_test.go`). See "Metrics reference" below.

### `POST /servers/{pool}/{server}/state`

```console
$ curl -s -X POST http://127.0.0.1:8081/servers/internal/mta1/state \
    -d '{"state":"drain"}'
{"ok":true}
```

`state` is one of `ready` / `drain` / `maint`. Round-trips through
`health.Checker.SetAdminState`, visible to the very next pick — proven
by `TestSetStateDrainMaintReady` (`internal/admin/write_test.go`) and,
end to end through a real relay, by `TestDrainMidClientConnection`
(`test/integration/admin_drain_test.go`). `DRAIN` and `MAINT` both make
the server ineligible; the difference is health checking:

- `DRAIN` — active probing **continues** (`TestDrainExcludesFromEligibleProbesContinue`,
  `internal/health/admin_test.go`) and is visible in `/servers`'
  `last_probe`/`last_change` and `bifrost_probe_total` the whole time.
- `MAINT` — probing **stops** (`TestMaintStopsProbes`,
  `internal/health/admin_test.go`); a probe already in flight is
  cancelled.

### `POST /servers/{pool}/{server}/health`

```console
$ curl -s -X POST http://127.0.0.1:8081/servers/internal/mta1/health \
    -d '{"override":"force-down"}'
{"ok":true}
```

`override` is one of `auto` / `force-up` / `force-down`. Round-trips
through `health.Checker.SetOverride` — proven by `TestSetOverride`
(`internal/admin/write_test.go`).

### `POST /servers/{pool}/{server}/weight`

```console
$ curl -s -X POST http://127.0.0.1:8081/servers/internal/mta1/weight \
    -d '{"weight":0}'
{"ok":true}
```

`weight` must be `0`–`256` (the same range `internal/config` enforces
for the file). **Runtime-only and ephemeral**: it lives in
`balance.Router`'s own override table, never touches the config file,
and takes effect on the very next pick — proven by
`TestSetWeightRebuildsWRR` (`internal/admin/write_test.go`) at the
admin-wiring level and `TestRouterSetWeightRebuildsWRR`
(`internal/balance/weight_test.go`) at the picking-algorithm level (the
override actually shifts WRR's distribution, not just the number
`GET /servers` reports). It is discarded on the next successful
reload — see the survival matrix below.

### `POST /reload`

```console
$ curl -s -X POST http://127.0.0.1:8081/reload
{"diff":"pool \"bulk\": server \"b2\" added","discarded_weight_overrides":["internal/mta1"]}

$ curl -s -X POST http://127.0.0.1:8081/reload
{"errors":["/etc/bifrost/bifrost.hcl:23,10-13: Weight out of range; weight 999 must be between 0 and 256."]}
```

Loads and validates the config file this admin instance was started
with, fresh from disk, every time — never the request body. **A bad
config never displaces the one currently serving traffic**: validation
runs to completion before anything is swapped
(`internal/admin/write.go`, `handleReload`). Good config → `200` with
`config.DiffSummary`'s pool/server-level diff, and the new generation is
live for the very next pick (proven by `TestReloadEndpointGood`,
`internal/admin/write_test.go`). Bad config → `422` with one diagnostic
string per error, each carrying `file:line,col` from HCL's own
diagnostic formatting, and the config already serving is untouched
(proven by `TestReloadEndpointBad`).

The full validate-then-atomically-swap sequence this endpoint runs is
exactly `-c` mode's own check, reused: the same `internal/config.Load`
and `Validate` a `bifrost -c -f <path>` invocation runs before ever
starting.

One extra rejection applies to reload and not to startup: a file that
moves `listener.bind` or `admin.bind` is refused **whole**, with
`restart required`, because those sockets are already open and v1 does
not re-bind them (D14 — a re-bind would orphan every long-lived session,
which is the thing hot reload exists to avoid). Nothing else in that file
is applied either, so an operator never ends up with half a config.
Proven by `TestReloadEndpointBindChange` (`internal/admin/write_test.go`)
for the endpoint and `TestReloadListenerChangeRejected`
(`test/integration/reload_test.go`) for `SIGHUP`.

## Reload runbook and the survival matrix

Reload is *config-file* reload — it re-reads the file, it does not
accept a new one over the wire. Edit the file, then either:

```console
$ curl -s -X POST http://127.0.0.1:8081/reload   # answers with the diff
$ kill -HUP $(pidof bifrost)                     # same sequence, logged
```

Both run the identical sequence — load, validate, check the binds, then
one atomic pointer swap — and the new generation is live at the next
`MAIL`, on connections that are already open, not just on new ones. A
transaction that was already attached when the swap landed finishes on
the backend it started with. Proven by `TestReloadNextMailSemantics`;
a server dropped from the file finishes what it is carrying, takes no new
transactions and stops being probed (within the health checker's own
one-second polling tick), proven by `TestReloadRemovedServerDrains`; a
broken file is logged with its diagnostics and changes nothing, proven by
`TestReloadBadConfigKeepsOld` (all three in
`test/integration/reload_test.go`). Twenty reloads under ten concurrent
senders lose no message and leak no session: `TestReloadStorm`.

What survives a successful reload, and what doesn't (PROJECT.md D15):

| State | Survives reload? | Why |
|---|---|---|
| Admin state (`ready`/`drain`/`maint`) | **Yes** | keyed by `(pool, server)` name in `health.Checker`; a server that persists across the swap keeps its existing `serverHealth` record (`internal/health/scheduler.go`, `sync`) |
| Force override (`auto`/`force-up`/`force-down`) | **Yes** | same mechanism |
| Runtime weight override | **No** — reverts to the file's value | `handleReload` calls `balance.Router.ResetWeights` right after the swap and logs which `pool/server` identities had one discarded (`discarded_weight_overrides` in the response and in the `"config reloaded"` log line); proven by `TestReloadEndpointGood` and, over `SIGHUP` on the live process, by `TestReloadRevertsRuntimeWeight` |
| Everything above | **No**, on a full process restart | no state file, deliberately — like HAProxy run without one (D15) |

A server *removed* from the file is dropped from the health registry
outright, admin state and all; if it's re-added later (even under the
same name) it starts over at `READY`/`AUTO`, unchecked.

## Drain runbook

Taking one backend server out of rotation for maintenance, HAProxy-style:

1. **Drain**: `POST /servers/{pool}/{server}/state {"state":"drain"}`.
   No new transaction will pick it, starting with the very next `MAIL`
   on every client connection — including ones already open (R3: this
   is the per-transaction re-pick, not a per-connection one). Anything
   already attached to it keeps running; drain does not abort in-flight
   work.
2. **Watch it drain**: poll `GET /servers` (or scrape
   `bifrost_in_flight{pool="...",server="..."}`) until `in_flight` hits
   `0`.
3. **Maint it**: `POST /servers/{pool}/{server}/state {"state":"maint"}`
   once `in_flight` is `0`. This is now safe to restart or take down —
   `MAINT` additionally stops the health checker from probing it at
   all, so no probe traffic bothers a server that's intentionally down.
4. When it's back: `POST .../state {"state":"ready"}`. Its `op` is
   whatever the (never-stopped, if you drained rather than maint'd
   first) active check last found; `ready` alone doesn't force it back
   into rotation if the checker itself thinks it's down.

This whole sequence, minus the polling wait, is exactly what
`TestDrainMidClientConnection` (`test/integration/admin_drain_test.go`)
proves converges within one transaction on one held-open client
connection — the R3 claim this epic exists to make visible to an
operator, not just to the balancer's own internals.

## Shutdown: what SIGTERM does

`SIGTERM` (or `SIGINT`) runs the drain sequence in PROJECT.md's order and
exits `0`. The order is the contract, not an implementation detail:

1. **`/healthz` flips to 503 first**, before anything stops working.
2. **Lame duck** (`timeouts.lame_duck`, default `2s`): the listener keeps
   accepting and serving normally, so whatever polls `/healthz` has a
   window to stop sending new connections here.
3. **The listener closes** — no new connections.
4. **Sessions are told the process is going away**: one sitting between
   commands is answered `421 4.3.0 Service shutting down` and closed
   immediately; one inside a mail transaction is not interrupted.
5. **In-flight transactions finish**, verdicts relayed verbatim, and each
   of those sessions then gets its own `421 4.3.0`.
6. **Force deadline** (`timeouts.drain_timeout`, default `30s`): whatever
   is still running has its **backend leg aborted first** — a bare
   disconnect, never a terminator a backend could mistake for a completed
   message, so a message still streaming is cleanly abandoned rather than
   half-delivered. The client then gets `451 4.4.2` for that transaction
   (and, where the final dot had already been delivered, the
   duplicate-risk signals fire — see below), followed by the `421 4.3.0`.
7. **Goodbye grace** (5s, fixed): the bound on those last writes. The
   process then exits `0` whether or not every session has finished —
   a client that keeps feeding bytes can otherwise hold a session open
   past any drain deadline (the discard-to-dot path re-arms
   `data_progress` per chunk, up to `session_max`), and a shutdown that
   waits for it is a shutdown that hangs. If any component is still
   running at that point the log says `components still running; exiting
   anyway`. Bounded exit is proven by `TestDrainExitBounded`.

```console
$ kill -TERM $(pidof bifrost); echo $?   # 0, after the sequence above
```

Proven by `TestDrainSequence` (the order, including "healthz 503 while
still accepting" and an idle session answered while another connection is
still mid-DATA), `TestDrainForceDeadline` (the force path: the backend
sees no dot beyond the client's own and no polite `QUIT`, the client gets
`451` then `421`, exit 0 inside the deadline) and `TestDrainSignalReal`
(the bare signal contract), all in `test/integration/drain_test.go`.

Nothing survives the exit: there is no state file (D15), so admin states,
force overrides and weight overrides are all gone on restart.

## Cert rotation

Backend-leg TLS settings a pool declares (`backend_tls`,
`backend_tls_server_name`, `backend_tls_ca`) are read fresh from the
config holder on every transaction (`internal/proxy` attaches per
message), so a reload that changes them takes effect immediately, no
restart, on the very next transaction — the same mechanism the drain
runbook above relies on.

The **listener's** own certificate (`listener.starttls.cert/key`) rotates
live too, and needs no restart. The process builds its `*tls.Config` with
a `GetCertificate` callback that consults the live config holder on every
handshake (`cmd/bifrost/wire.go`), parsing the files once per config
generation. So the routine 90-day rotation is:

1. write **both** the new cert and the new key over the same paths the
   config names, and only then reload: the two files are read as a pair,
   so a reload that lands between the two writes sees a mismatched one;
2. `curl -X POST .../reload` or `kill -HUP` — the reload re-validates
   that both files are readable (`internal/config/validate.go`,
   `validateStartTLSFiles`) and refuses the whole file if they are not;
3. the next STARTTLS handshake presents the new certificate.

If the pair is mismatched at that moment anyway, handshakes do not fail:
the certificate already loaded keeps being served, with a
`"listener certificate unreadable; keeping the one already loaded"`
warning, and the next handshake re-reads the files — so finishing the
second write is all it takes to recover, with no second reload (proven by
`TestReloadKeepsCertWhenPairIsMidRotation`, `test/integration/cert_test.go`).

TLS sessions established before the swap keep the certificate they
negotiated — a live session's certificate cannot be replaced under it,
and does not need to be — and they keep working. Proven end to end
against the real binary by `TestReloadPicksUpRotatedCert`
(`test/integration/cert_test.go`), which compares the served
certificate's serial number before and after a rotation and then keeps
using the pre-rotation session.

What still needs a restart is the `listener.bind`/`admin.bind` address
itself (see the reload rejection above), and the parts of the listener
that are read once per process: `hostname`, the advertised
`capabilities`, `global_maxconn` and the client-leg timeouts
(`client_idle`, `session_max`) — a reload applies the rest of the file
but those keep their startup values, for new connections as well as old
ones. `client_idle` is a partial exception: that startup value only
governs the between-commands idle wait (a session parked before or
between transactions); the in-transaction client-read deadlines re-read
the live config at the start of every transaction, so an already-open
session picks up a reloaded `client_idle` the moment its next
transaction begins — restart is only required for the between-commands
window. A reload that changes any of them says so, per field, in the
`"reload applied with a limitation"` log lines and in `POST /reload`'s
`restart_required` array (proven by
`TestReloadListenerFieldWarnsRestartRequired`,
`test/integration/reload_test.go`). Pools, servers, weights, routing
rules, backend TLS and the backend/drain timeouts are all live on
reload.

Backend-leg `starttls-verify` needs `backend_tls_ca` to point at the CA
that signed the backend's certificate; it is parsed once per config load
and used by both the relay's dialer and the health checker's probe
(proven by `TestBackendPrivateCAStartTLSVerify`,
`test/integration/binary_test.go`). An unreadable or certificate-free CA
file is a load error, so `-c` catches it before a restart does.

## SMTP AUTH

Bifrost terminates SMTP AUTH PLAIN locally on each leg, independently:
the client authenticates *to bifrost*, and — completely separately —
bifrost authenticates *to each backend* with that pool's own
credentials. Either side can be configured without the other; see
PROJECT.md's "No AUTH passthrough" paragraph for why the two never mix
(auth is connection-scoped, a session spans many backends, so relaying
one client's AUTH to N backends was never coherent — this feature is
local termination, not relay).

The two legs also differ on reload: a `listener.auth` edit (a revoked
user, a rotated hash) needs a **restart** to take effect — each `Session`
captures the listener's auth store once, at accept, so `POST /reload` /
`SIGHUP` warns `listener auth changed: restart required to apply` rather
than silently leaving sessions on the old store (`RestartRequired`,
`internal/config/holder.go`). A `pool.auth` edit is **reload-live**: both
the relay and the health prober re-read credentials off the config
holder on every transaction/probe, so a rotated backend password applies
at the next `MAIL`, no restart needed.

### `allow_cleartext`: opting out of the TLS requirement

Both `listener.auth` and `pool.auth` accept `allow_cleartext = true`
(default `false`), independently — one leg can set it without the other.
It lifts, for that block only:

- `listener.auth`: the "client auth requires starttls" load error, the
  pre-TLS `AUTH PLAIN` advertisement suppression, and the client-leg
  `538 5.7.11` gate — `AUTH PLAIN` is then advertised and accepted on a
  plaintext session.
- `pool.auth`: the "pool auth requires backend TLS" and "pool auth
  requires TLS probes" load errors, and `backend.Dial`'s refusal to send
  `AUTH PLAIN` before a TLS upgrade.

```hcl
auth {
  allow_cleartext = true
  # ... users / username+password as usual
}
```

This is only sane when the link is already secured below SMTP — a
same-namespace or same-mesh in-cluster Kubernetes hop, for example, where
the SMTP TLS handshake would just be redundant encryption on top of the
network layer's own. It is not a substitute for TLS on a link that
crosses a trust boundary the network layer doesn't already cover.

The default stays strict on purpose: `backend_tls` itself defaults to
`"none"`, so if `allow_cleartext` also defaulted to `true`, a forgotten
`backend_tls` line on a pool with `auth` configured would silently leak
credentials in cleartext instead of failing to load. The knob has to be
written down in the config file, where an auditor will see it.

### Client leg: `listener.auth`

```hcl
listener {
  starttls { ... }      # required: see below
  auth {
    user "rttskr-team" {
      salt            = "1af90c3e2b7ad4f1"
      hashed_password = "466aec5e9c8096eb07b86d055773ea4267b548c25831c6d56a5c8ff7f5497977"
    }
  }
}
```

- Requires `listener.starttls` — configuring `auth` without it is a load
  error, because credentials must never be sent before TLS and there is
  no plaintext-auth escape hatch (`internal/config/validate.go`,
  `TestValidateDiagnostics/client_auth_without_starttls`).
- `AUTH PLAIN` is advertised in `EHLO` only once TLS is up on that
  connection, and only until the session authenticates — proven by
  `TestAuthFullChainWithBackendCreds` (`test/integration/auth_test.go`),
  which asserts the pre-STARTTLS `EHLO` carries no `AUTH` line and the
  post-STARTTLS one does.
- **Configuring auth makes it required.** Once `listener.auth` is set,
  every `MAIL` before a successful `AUTH` gets
  `530 5.7.0 Authentication required` and the session stays open (the
  client may still authenticate and retry) — proven by
  `TestAuthGateRequiresClientAuth`, which also asserts zero backend
  dials happened before the gate fired.
- Only `AUTH PLAIN` is supported: any other mechanism gets
  `504 5.5.4 Unrecognized authentication type`, and `AUTH` on a
  plaintext session gets
  `538 5.7.11 Encryption required for requested authentication
  mechanism` — `TestSessionAuthMechanismAndLockout`
  (`internal/proxy/starttls_test.go`).
- **Three failed attempts in one session close the connection**:
  `421 4.7.0 Too many failed authentication attempts, closing
  connection` — same test.

#### Minting a credential

`hashed_password` is `hex(sha256(salt || password))`, lowercase (config
loading lowercases it for you, but mint it lowercase to keep diffs
clean):

```console
$ printf '%s' "1af90c3e2b7ad4f1correct-horse" | sha256sum
```

This is deliberately the same shape kumo's own `inbound_auth.toml`
credential store uses (a salt plus a hex-SHA256 hash) — bifrost's dev
environment mints both from the same recipe via `make kumo-credential`,
so one generated salt/password pair is valid on either side of a
bifrost-fronted kumod without translation.

### Backend leg: `pool.auth`

```hcl
pool "bulk" {
  backend_tls = "starttls-verify"   # anything but "none"
  auth {
    username = "rttskr-team"
    password = "pa55w0rd"           # plaintext: bifrost sends PLAIN as-is
  }
}
```

- Requires `backend_tls != "none"` — a load error otherwise, since
  backend credentials must never cross the wire in cleartext
  (`TestValidateDiagnostics/pool_auth_requires_backend_TLS`).
- After the post-TLS `EHLO`, bifrost checks the backend's own
  capability set for `AUTH PLAIN` (space-split, case-insensitive)
  before sending anything. A backend that doesn't advertise it fails
  the dial as `incompatible`, exactly like a missing `SIZE`/`8BITMIME`
  capability — `TestDialAuthNotAdvertised`
  (`internal/backend/auth_test.go`). This is an implicit backend
  requirement independent of whatever the listener itself advertises
  (see PROJECT.md's EHLO capability policy).
- On success bifrost sends one exact line —
  `AUTH PLAIN <base64("\x00user\x00pass")>` — before `MAIL`, byte-
  verified end to end by `TestAuthFullChainWithBackendCreds`
  (`test/integration/auth_test.go`) and at the dial layer by
  `TestDialAuthHappyPath`.
- The backend's own AUTH rejection never reaches the client: it just
  fails that dial candidate, and the relay's ordinary failover/no-
  backend synthesis takes over from there —
  `TestAuthBackendCredsRejectedYieldsNoBackend` proves a bad pool
  password surfaces as bifrost's own
  `451 4.4.1 No backend available, try again later`, never the
  backend's own `535`.

#### Kubernetes: password from a mounted secret

`password` and `password_file` are mutually exclusive — set exactly one.
`password_file` points at a file holding the plaintext password (a
trailing newline is trimmed for you):

```hcl
auth {
  username      = "rttskr-team"
  password_file = "/var/run/secrets/smtp/password"
}
```

Mount the credential as a Kubernetes `Secret` volume and point
`password_file` at the mounted path. Relative paths resolve against the
config file's own directory, same as `backend_tls_ca`. Rotation is
reload-live: `password_file` is re-read on every `POST /reload` /
`SIGHUP`, same as the rest of `pool.auth` — no restart needed when the
Secret's contents change (kubelet's own propagation delay for mounted
Secrets still applies before a rotated file shows up in the container).

#### Verdict semantics: bad backend credentials vs. a backend that's down

A pool credential the backend permanently rejects (`5xx`, e.g.
`535 5.7.8`) marks the server **incompatible** in `GET /servers` — the
same verdict a missing capability gets — instead of flapping it
`DOWN`/`UP` on every probe: `TestProbeAuthPermanentFailure`
(`internal/health/probe_test.go`) and, at the dial layer,
`TestDialAuthPermanentFailure` (`internal/backend/auth_test.go`,
`AuthError.Permanent()` is true for any code ≥ 500). A transient
rejection (`4xx`, e.g. `454`) is a plain probe failure instead — the
server goes `DOWN` like any other failed probe, not `incompatible`:
`TestProbeAuthTransientFailure`, `TestDialAuthTransientFailure`. Every
`ehlo`-level-or-deeper probe against an `auth`-configured pool
authenticates for real, so a rotated or mistyped backend password
surfaces in `/servers` within one probe interval — the same guarantee
capability drift already gets.

## Backend connection reuse

By default (`reuse_envelopes` omitted, `0`, or `1`) every envelope gets
its own backend connection, dialed and handshaked fresh, then politely
`QUIT`'d — PROJECT.md's D4. A pool can opt into session-affine reuse
instead:

```hcl
pool "outgoing" {
  reuse_envelopes = 50   # 0/1/omitted = fresh conn per envelope (default)
}
```

`N > 1` both enables reuse and caps it: a connection that finishes an
envelope cleanly is kept open and handed to that client session's next
envelope, provided the balancer's fresh per-`MAIL` pick (R3 is
untouched — every envelope still gets a fresh pick) lands on the same
server. The connection is closed and re-dialed once it has carried `N`
envelopes, so `N` is also the freshness bound: AUTH state, TLS session,
and anything else the backend attaches to a connection lives for at most
`N` envelopes. There is exactly one reuse slot per client session
(`internal/proxy`'s `backendAffinity`) — never shared across sessions,
never holding more than one cached connection.

**Revalidation, not a reaper.** There is no idle reaper watching cached
connections — instead, every reuse attempt sends `RSET` on the cached
connection first and reads its reply before trusting it (never relayed
to the client: the client never knew this connection existed before
this envelope). Any error or non-`2xx` reply silently closes the cached
connection and falls back to a normal dial, exactly as if reuse had
never applied — proven by `TestReuseDeadCachedConnFailsOverTransparently`
(`internal/proxy/relay_test.go`) and, against a real killed backend,
`TestReuseDeadCachedConnFailsOverToFreshDial`
(`test/integration/reuse_test.go`). This is a deliberate ceiling, not an
oversight: RSET revalidation already turns a dead cached connection into
a self-healing event — one silent re-dial on the next envelope, never a
client-visible error — so a reaper would only save the wire cost of that
one extra RSET per idle timeout. Add one if a workload's idle gaps
between envelopes on the same session get long and frequent enough for
that cost to matter.

**Observability.** `bifrost_backend_conn_reuse_total{server, outcome}`
counts every reuse decision, `outcome` one of `reused` (a cached
connection was handed to the next envelope) or `capped` (the connection
hit `N` and was closed instead) — see the Metrics reference below. The
transaction log gains `conn_envelope=<k>`: `1` for a fresh connection's
first (and, with reuse off, only) envelope, `k > 1` for the `k`-th
envelope on a connection that has now been reused `k-1` times.

**Interactions.**
- **Leases and `leastconn`.** A lease is still taken and released for
  every envelope, same as today — a cached idle connection holds no
  lease and is invisible to `leastconn`'s in-flight accounting between
  envelopes, exactly like any other idle connection (PROJECT.md's
  Accepted Semantics & Risks).
- **Pool auth.** With `pool.auth` configured, `AUTH PLAIN` happens once
  per connection, not once per envelope: a reused connection carries its
  first envelope's authenticated state forward, so envelopes 2..N on it
  never re-send `AUTH` — proven end to end (one `AUTH` line across three
  envelopes on one connection) by
  `TestReuseSharesOneConnAcrossEnvelopesWithSingleAuth`
  (`test/integration/reuse_test.go`).
- **Health and drain.** Active probes are unaffected (always fresh
  dials). A cached connection is not in the relay's tracked-legs map —
  it is never mid-message — so session teardown (client `QUIT`, drain)
  is what closes it, not `CloseLegs`.

Proven by the `TestReuse*` suite in `internal/proxy/relay_test.go`
(stash/cap/server-mismatch/lease-denial unit coverage) and
`test/integration/reuse_test.go` (full-chain auth-once and dead-conn
failover above, plus `TestReuseEnvelopesOmittedDialsFreshPerEnvelope`,
which pins today's default behavior unchanged).

## Metrics reference

Thirteen stable names (golden-list tested by `TestMetricNamesStable`,
`internal/metrics/metrics_test.go` — a name change here is a breaking
change to anyone's dashboards), and all thirteen actually present in a
live scrape of the real `GET /metrics` handler — proven by
`TestMetricsServed` (`internal/admin/admin_test.go`), which is what
would have caught `ServerCollector` not being registered on the
registry `admin.New` serves:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `bifrost_sessions_active` | gauge | — | client SMTP sessions currently open |
| `bifrost_sessions_total` | counter | — | client SMTP sessions accepted, cumulative |
| `bifrost_transactions_total` | counter | `pool`, `server`, `verdict_class` | mail transactions concluded; `verdict_class` is `2xx`/`4xx`/`5xx` for a relayed backend verdict, `synth_421`/`synth_451` for one Bifrost synthesized. **Does not** increment for `RSET` while attached, a torn/malformed backend reply, or a client hangup before any verdict — those have no single verdict to classify. The transaction log below still emits its one record for every one of those; `pool`/`server` are `""` there too on a total-attach failure (same rule as this metric's own labels) |
| `bifrost_synthesized_replies_total` | counter | `code_enhanced` (e.g. `"451 4.4.1"`) | replies Bifrost generated itself rather than relaying a backend's |
| `bifrost_relay_bytes_total` | counter | `direction` (`to_backend`/`to_client`) | verbatim bytes relayed, by direction |
| `bifrost_backend_dials_total` | counter | `server`, `result` (`ok`/`fail`) | backend dial attempts |
| `bifrost_backend_conn_reuse_total` | counter | `server`, `outcome` (`reused`/`capped`) | session-affine backend connection reuse decisions — see "Backend connection reuse" above |
| `bifrost_probe_total` | counter | `server`, `level`, `result` (`ok`/`fail`/`incompatible`) | active health probes completed |
| `bifrost_server_up` | gauge | `pool`, `server` | `1` if the active check currently reports it reachable |
| `bifrost_server_eligible` | gauge | `pool`, `server` | `1` if up **and** ready/not-force-down **and** capability-compatible — a server can be `up=1` and deliberately not receiving traffic (`eligible=0`); that gap is the point of having both gauges |
| `bifrost_server_state_changes_total` | counter | `pool`, `server` | active-check UP/DOWN flips, cumulative — for flap alerting |
| `bifrost_in_flight` | gauge | `pool`, `server` | transactions currently attached |
| `bifrost_duplicate_risk_total` | counter | — | transactions where a backend died after the final dot but before a verdict |

`bifrost_probe_total` and `bifrost_backend_dials_total` deliberately
omit `pool` (PROJECT.md's own Produces contract): if two pools share a
same-named server, their probe/dial counts are combined under that
name.

`bifrost_transactions_total`/`bifrost_relay_bytes_total`/
`bifrost_backend_dials_total` moving with real traffic is proven
end-to-end by `TestTransactionCountersMove`
(`internal/metrics/metrics_integration_test.go`);
`bifrost_synthesized_replies_total` by `TestSynthesizedReplyCounter` in
the same file.

## Transaction log

One `slog` record per transaction, message `"transaction"`, emitted
exactly once when the transaction concludes regardless of how (a
delivered message, a latched failure, a client hangup, `RSET`) —
`internal/proxy/txnlog.go`, emitted from a `defer` in
`Relay.HandleTransaction` so every path is covered by construction.

| Field | Meaning |
|---|---|
| `client` | client IP |
| `helo` | the session's `EHLO`/`HELO` argument |
| `pool`, `server` | the last-attached candidate, if any (`""` if every dial attempt failed) |
| `mail_verdict` | the `MAIL` command's own reply code |
| `rcpt_count` | recipients answered |
| `rcpt_verdicts_class` | e.g. `"2xx=3,5xx=1"` |
| `data_verdict` | the end-of-data reply's verbatim first line (or an immediate non-`354` `DATA` rejection); empty if `DATA` was never reached |
| `bytes` | message body bytes streamed — a **count**, never content |
| `duration_ms` | wall time from the transaction opening to this record |
| `failover_attempts` | `backend.Dial` attempts across every candidate tried |
| `synth` | the synthesized reply's trimmed text, if the conclusion was Bifrost's own rather than relayed |
| `duplicate_risk` | `true` for the duplicate-delivery window (see below) |
| `conn_envelope` | this transaction's ordinal on its backend connection — `1` for a fresh connection, `k > 1` for the `k`-th envelope on one reused via `reuse_envelopes` (see "Backend connection reuse" above); omitted entirely if no backend ever attached |

Never logs message body content, only counts — proven by relaying a
real message carrying a unique marker string through a real backend
with the whole `slog` output captured, and grepping every captured
byte for the marker (`TestTxnLogNeverLogsBody`,
`internal/proxy/txnlog_integration_test.go`, tag `integration`) — a
stronger claim than "this one hand-built record's fields don't happen
to contain it". This test makes no claim about the record's size being
independent of message size; for that, see `TestStreamingCeiling1GB`
(`test/integration/m1_test.go`), which relays a 1 GiB message under a
64 MiB heap ceiling. Field completeness is `TestTxnLogRecordComplete`;
the duplicate-risk case is `TestTxnLogSynthAndDuplicateRisk` (both
plain, `internal/proxy/txnlog_test.go`).

## Duplicate-delivery window

If a backend receives a message's final dot but dies (or times out —
`600s` by design, the RFC 5321 floor, kept deliberately long) before
replying, Bifrost cannot know whether the message was actually queued.
The client is told `451 4.4.2` and may resend a message the backend
already has. This is inherent to cut-through delivery — Exim's own
`cutthrough_delivery` spools specifically to avoid it; Bifrost doesn't
queue at all, by design (PROJECT.md's Accepted Semantics & Risks).

Three signals exist for this, all firing together on the same event
(`internal/proxy/data.go`, `pipeBody`'s `body.delivered` case):
the `duplicate_risk` field in the transaction log, the
`bifrost_duplicate_risk_total` counter, and a `slog.Warn` line. None of
them can distinguish "backend actually delivered it" from "backend
lost it" — that ambiguity is the window, not a monitoring gap. What an
operator can do: alert on the counter moving at all (it should be
extremely rare) and, when it does, check whether the backend received
the message via its own log correlated on the timestamp/recipient
before assuming a duplicate.

## Coming from HAProxy: what's different

- **No stats-socket line protocol.** HAProxy's Unix-socket text
  commands (`show stat`, `set server ... state ...`) are replaced by
  HTTP + JSON, over the same loopback/unix-socket transport. Every verb
  above has a curl-able equivalent.
- **Runtime changes are ephemeral — there is no state file.** A weight
  override, an admin drain, a force override: none of it is written to
  disk. Admin state and force overrides survive a *reload*; nothing
  survives a *restart*. HAProxy's `-x`/state-file socket-passing model
  has no analog here.
- **No slowstart yet.** A server just back from `maint`/`drain` (or
  freshly added) is picked at its full configured weight immediately;
  there is no ramp for cold MTA IP warm-up. Flagged in PROJECT.md as
  the natural post-v1 addition.
- **Drain converges per-transaction, not per-connection.** HAProxy's
  drain (for stateful L4/L7 proxying) generally waits out existing
  *connections*. Bifrost already re-picks a backend at every `MAIL`
  (R3), so a drained server stops receiving new work within the same
  client connection, not just on its next reconnect — see the drain
  runbook above.
- **Listener bind changes require a restart.** Unlike pool/server/
  routing/weight/certificate changes, the client-facing `listener.bind`
  (and `admin.bind`) is fixed for the process's lifetime in v1
  (PROJECT.md D14), and a reload that moves one is rejected outright
  rather than partially applied. HAProxy's reload starts a new process
  and hands the sockets over; Bifrost reloads in place, because a new
  process would orphan every long-lived client connection — which is
  exactly the thing R3 exists to support.
- **The admin plane is unauthenticated, by design** — see "Security
  model" above. HAProxy's socket has the same property but is
  conventionally root-owned and mode `600`; Bifrost's `unix://` bind is
  too, automatically, and its TCP bind is loopback-only unless you
  explicitly opt out.
- **`/healthz` behind an L4 balancer.** If something upstream (a cloud
  LB, keepalived, another HAProxy) needs to see Bifrost's own lame-duck
  signal, it needs a bind that upstream can actually reach — a
  loopback or host TCP bind, not a `unix://` one, since a unix socket
  is invisible to anything outside the host's own filesystem namespace.
  Plan the admin bind around whoever needs to poll `/healthz`, not just
  around the operator's own curl access.
- **Probe-level selection.** HAProxy's `option smtpchk` roughly maps to
  Bifrost's `ehlo` level (the default). The full ladder:
  - `connect` — plain TCP, zero SMTP bytes, k8s `tcpSocket`-style. The
    cheapest possible check, but closing right after connect makes many
    MTAs log something like "lost connection after CONNECT" on every
    probe — expected, not a sign the backend is misbehaving. Pairs
    naturally with the `check.port` override (probe a dedicated health
    port instead of the traffic port) to keep that noise off the real
    listener entirely.
  - `banner` — reads the `220` greeting, then `QUIT`s politely. Never
    sends `EHLO`; a backend that would fail the capability check can
    still pass this level.
  - `ehlo` (default) — the full handshake, including the capability
    superset check against the listener's advertised set. This is what
    can mark a server `incompatible` rather than merely down.
  - `deep` — `EHLO` then `MAIL FROM:<>` / `RCPT TO:<postmaster>` /
    `RSET`, never `DATA`. Off by default: backends with greylisting
    will see a probe sender they've never seen mail from and may
    soft-fail the `RCPT`, which reads as a plain probe failure, not a
    special case. Enable it only against backends tuned to expect it.
- **Probe log noise on backends, and whitelisting.** Every probe level
  above `connect` opens (and, at `banner`/`ehlo`/`deep`, cleanly closes
  via `QUIT`) a real connection on the backend's SMTP port, at
  `check.interval` (healthy) or `check.down_interval` (down) cadence
  per server. On a backend with connection-rate limiting
  (postscreen/anvil-style) or verbose per-connection logging, this is
  indistinguishable from a scanner unless the balancer's address is
  whitelisted. Combine `down_interval` (coarser cadence while
  confirmed down) with a `check.port` override (a dedicated health
  socket, exempt from the backend's own rate limiting) where that's not
  possible.

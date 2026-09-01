// Package config implements Bifrost's HCL configuration: strict schema
// decoding, semantic validation with line-precise diagnostics, defaults
// inheritance, and the atomic runtime holder used by hot reload.
//
// Every exported type in this file is plain Go — no hcl.Range or cty value
// ever appears in an exported field. Source-range bookkeeping needed for
// range-anchored diagnostics (see validate.go) is kept in unexported
// fields, populated by load.go from the HCL AST and never exposed outside
// this package.
package config

import (
	"crypto/x509"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// Config is the fully decoded, resolved, and validated Bifrost
// configuration returned by Load.
type Config struct {
	Defaults Defaults
	Listener Listener
	Pools    []Pool
	Routing  Routing
	Admin    *Admin // nil: no admin plane configured (logged warning, see Validate)
	Limits   Limits

	// fileRange and listenerRanges back Validate's structural checks (e.g.
	// "exactly one listener block") that have no single resolved field to
	// anchor to. Zero value (unset) for a Config built by hand rather than
	// by Load; Validate degrades to an unanchored diagnostic in that case.
	fileRange      hcl.Range
	listenerRanges []hcl.Range
}

// Defaults holds the config-wide fallback values consulted by pool/server
// inheritance (see defaults.go).
type Defaults struct {
	EhloName string
	Timeouts Timeouts
	Check    CheckParams
}

// Timeouts is every row of PROJECT.md's timeout budget table. All fields
// are configurable; Validate rejects non-positive or internally inverted
// values and warns (never errors) when a value sits below its RFC 5321
// §4.5.3.2 floor.
type Timeouts struct {
	ClientIdle       time.Duration // client first-command / idle
	SessionMax       time.Duration // session max lifetime
	BackendConnect   time.Duration // backend connect, per attempt
	BackendHandshake time.Duration // backend greeting+EHLO+TLS handshake
	BackendMailReply time.Duration // backend MAIL/RCPT reply
	Backend354Wait   time.Duration // backend 354 wait
	DataProgress     time.Duration // DATA progress watchdog, per direction
	BackendFinalDot  time.Duration // backend final-dot reply
	LameDuck         time.Duration // drain: lame duck (healthz 503 before listener close)
	DrainTimeout     time.Duration // drain: force deadline

	rng hcl.Range
	// fieldRanges anchors each field's own diagnostics at its exact
	// source attribute (e.g. "backend_handshake") rather than the whole
	// timeouts{} block; keyed by HCL attribute name. nil for a Timeouts
	// built by hand rather than by Load, in which case Validate falls
	// back to rng.
	fieldRanges map[string]hcl.Range
}

// CheckParams is a health-check probe configuration. It appears at the
// defaults, pool, and server tiers; Server.Check holds the fully resolved
// effective value for that server (see defaults.go).
type CheckParams struct {
	Level        string // "connect" | "banner" | "ehlo" | "deep"
	Port         int    // probe port override; 0 = the server's traffic port
	Interval     time.Duration
	DownInterval time.Duration
	Timeout      time.Duration
	Rise         int
	Fall         int
	EhloName     string
	ProbeRcpt    string
	TLS          string // "none" | "starttls" | "starttls-verify"

	// CAPool is the parsed backend_tls_ca of the pool this check belongs
	// to (see Pool.CAPool): the probe ladder's starttls-verify handshake
	// needs the same roots the traffic path uses, and CheckParams is all
	// internal/health ever sees of a pool. nil means "no private CA
	// configured" — the platform's own roots.
	CAPool *x509.CertPool

	// AuthUsername and AuthPassword are the pool's plaintext backend-leg SMTP
	// AUTH credentials (from Pool.Auth): the probe ladder's SMTP AUTH exchange
	// needs the same credentials the traffic path uses, and CheckParams is all
	// internal/health ever sees of a pool. Empty when pool auth is nil.
	AuthUsername string
	AuthPassword string

	rng hcl.Range
	// fieldRanges anchors level/port/interval/down_interval/timeout/
	// rise/fall at their own attribute, the way Timeouts.fieldRanges
	// does; TLS/EhloName/ProbeRcpt have no enum/range rule of their own
	// so they stay anchored at rng. nil for a hand-built CheckParams.
	fieldRanges map[string]hcl.Range
}

// Listener is the client-facing bind. Exactly one is supported in v1
// (Validate rejects more or fewer).
type Listener struct {
	Bind     string
	Hostname string
	StartTLS *StartTLS     // nil: STARTTLS not advertised
	Auth     *ListenerAuth // nil: client-leg SMTP AUTH disabled

	// Capabilities is the advertised EHLO set, resolved: the built-in
	// default when the block omitted it, plus STARTTLS whenever a
	// certificate is configured (see resolveCapabilities).
	Capabilities []string

	rng hcl.Range
	// hostnameRange and capabilitiesRange anchor the syntactic checks on
	// those two attributes at the attribute itself rather than the whole
	// listener{} block. Zero for a hand-built Listener.
	hostnameRange     hcl.Range
	capabilitiesRange hcl.Range
}

// StartTLS configures the listener's client-facing TLS.
type StartTLS struct {
	Cert       string
	Key        string
	MinVersion string

	rng hcl.Range
}

// ListenerAuth configures client-leg SMTP AUTH: the set of users allowed
// to authenticate against this listener, each checked against a
// salted-SHA256 hash (never a plaintext password) rather than the
// backend-leg plaintext of PoolAuth.
type ListenerAuth struct {
	Users []AuthUser

	rng hcl.Range
}

// AuthUser is one client-leg SMTP AUTH credential. HashedPassword is
// lowercase hex SHA256(salt + password), normalized to lowercase at load
// so comparison never has to case-fold it again.
type AuthUser struct {
	Name           string
	Salt           string
	HashedPassword string

	rng hcl.Range
}

// PoolAuth configures backend-leg SMTP AUTH: the plaintext credentials
// Bifrost presents to every server in the pool. Plaintext, unlike
// ListenerAuth's hash, because it must be replayed verbatim in the
// backend's own AUTH exchange.
//
// PasswordFile is an alternative to Password (e.g. a Kubernetes secret
// mount): exactly one of the two may be set in source. Once Load has run,
// Password always holds the effective credential — if PasswordFile was
// set and readable, its trimmed contents were copied into Password (see
// resolveBackendCAs) — so every other consumer (validatePoolAuth,
// resolvePoolAuth's CheckParams copy, attach.go) only ever reads Password.
type PoolAuth struct {
	Username     string
	Password     string
	PasswordFile string

	rng               hcl.Range
	passwordFileRange hcl.Range // anchors password_file diagnostics at the attribute itself
}

// Pool is a named group of weighted backend servers.
type Pool struct {
	Name                 string
	Balance              string // "roundrobin" | "leastconn"
	BackendTLS           string // "none" | "starttls" | "starttls-verify"; default "none"
	BackendTLSServerName string
	BackendTLSCA         string
	EhloName             string // resolved: pool -> defaults.ehlo_name -> listener hostname
	MaxTransactions      int    // pool-level default for servers' max_transactions
	ReuseEnvelopes       int
	Check                CheckParams
	Servers              []Server
	Auth                 *PoolAuth // nil: backend-leg SMTP AUTH disabled

	// CAPool is BackendTLSCA parsed exactly once per config load (see
	// resolveBackendCAs): both consumers of a pool's backend-leg TLS —
	// internal/proxy's dialer and internal/health's probe ladder — read
	// the roots from here rather than re-reading the PEM file per
	// transaction or per probe. nil when no backend_tls_ca is configured.
	CAPool *x509.CertPool

	rng     hcl.Range
	caRange hcl.Range // anchors backend_tls_ca diagnostics at the attribute itself

	maxTxnRange         hcl.Range // set only when this tier wrote max_transactions itself
	reuseEnvelopesRange hcl.Range // set only when this tier wrote reuse_envelopes itself
}

// Server is one backend destination within a Pool.
type Server struct {
	Name            string
	Address         string
	Weight          int
	Backup          bool
	MaxTransactions int
	Check           CheckParams

	rng         hcl.Range
	weightRange hcl.Range // anchors "weight out of range" at the weight attribute itself

	maxTxnRange hcl.Range // set only when this tier wrote max_transactions itself
}

// Routing is the ordered set of pool-selection rules plus the fallthrough
// default.
type Routing struct {
	Rules       []RoutingRule
	DefaultPool string

	rng hcl.Range
}

// RoutingRule matches on client CIDR and/or MAIL FROM domain (wildcard
// prefix allowed, e.g. "*.news.example.com") and selects a pool.
type RoutingRule struct {
	ClientCIDR     []string
	MailFromDomain []string
	Pool           string

	rng hcl.Range
}

// Admin configures the runtime admin API bind. A TCP bind must be loopback
// unless AllowRemote is set.
type Admin struct {
	Bind        string
	AllowRemote bool

	rng hcl.Range
}

// Limits holds config-wide capacity limits.
type Limits struct {
	GlobalMaxConn   int
	MaxTransactions int // default tier for per-server max_transactions; 0 = unlimited

	rng          hcl.Range
	maxConnRange hcl.Range // set only when global_maxconn was written
	maxTxnRange  hcl.Range // set only when max_transactions was written
}

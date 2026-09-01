package config

import "github.com/hashicorp/hcl/v2"

// This file holds the strict-decode schema: gohcl.DecodeBody targets that
// mirror the public Config tree one-for-one, plus the source-range
// bookkeeping (def_range/attr_range companions) load.go's convert
// functions and defaults.go's resolution copy into Config's unexported
// range fields. See load.go for Load itself and the convert methods.

// rawFile mirrors Config at the strict-decode level: every block is
// optional or repeatable here even where v1 semantics require exactly
// one (e.g. listener), so that Validate — not gohcl — produces the
// operator-facing diagnostic for those cases, anchored at a precise range.
type rawFile struct {
	Defaults  *rawDefaults  `hcl:"defaults,block"`
	Listeners []rawListener `hcl:"listener,block"`
	Pools     []rawPool     `hcl:"pool,block"`
	Routing   rawRouting    `hcl:"routing,block"`
	Admin     *rawAdmin     `hcl:"admin,block"`
	Limits    *rawLimits    `hcl:"limits,block"`
}

// rawTimeouts carries an attr_range companion per field (not just the
// block-level def_range above): PROJECT.md's timeout table is ten
// attributes in one block, and a coarse block-level anchor would put
// every one of them's diagnostics on the same "timeouts {" line — see
// resolveTimeouts, which is what actually reads these Range fields.
type rawTimeouts struct {
	Range hcl.Range `hcl:",def_range"`

	ClientIdle            string    `hcl:"client_idle,optional"`
	ClientIdleRange       hcl.Range `hcl:"client_idle,attr_range"`
	SessionMax            string    `hcl:"session_max,optional"`
	SessionMaxRange       hcl.Range `hcl:"session_max,attr_range"`
	BackendConnect        string    `hcl:"backend_connect,optional"`
	BackendConnectRange   hcl.Range `hcl:"backend_connect,attr_range"`
	BackendHandshake      string    `hcl:"backend_handshake,optional"`
	BackendHandshakeRange hcl.Range `hcl:"backend_handshake,attr_range"`
	BackendMailReply      string    `hcl:"backend_mail_reply,optional"`
	BackendMailReplyRange hcl.Range `hcl:"backend_mail_reply,attr_range"`
	Backend354Wait        string    `hcl:"backend_354_wait,optional"`
	Backend354WaitRange   hcl.Range `hcl:"backend_354_wait,attr_range"`
	DataProgress          string    `hcl:"data_progress,optional"`
	DataProgressRange     hcl.Range `hcl:"data_progress,attr_range"`
	BackendFinalDot       string    `hcl:"backend_final_dot,optional"`
	BackendFinalDotRange  hcl.Range `hcl:"backend_final_dot,attr_range"`
	LameDuck              string    `hcl:"lame_duck,optional"`
	LameDuckRange         hcl.Range `hcl:"lame_duck,attr_range"`
	DrainTimeout          string    `hcl:"drain_timeout,optional"`
	DrainTimeoutRange     hcl.Range `hcl:"drain_timeout,attr_range"`
}

// rawCheck also carries attr_range companions for its numeric/duration
// fields, for the same reason as rawTimeouts (see resolveCheck). TLS,
// EhloName, and ProbeRcpt don't get one: they're plain pass-through
// strings with no enum/range rule of their own to anchor precisely.
type rawCheck struct {
	Range hcl.Range `hcl:",def_range"`

	Level             string    `hcl:"level,optional"`
	LevelRange        hcl.Range `hcl:"level,attr_range"`
	Port              *int      `hcl:"port"`
	PortRange         hcl.Range `hcl:"port,attr_range"`
	Interval          string    `hcl:"interval,optional"`
	IntervalRange     hcl.Range `hcl:"interval,attr_range"`
	DownInterval      string    `hcl:"down_interval,optional"`
	DownIntervalRange hcl.Range `hcl:"down_interval,attr_range"`
	Timeout           string    `hcl:"timeout,optional"`
	TimeoutRange      hcl.Range `hcl:"timeout,attr_range"`
	Rise              *int      `hcl:"rise"`
	RiseRange         hcl.Range `hcl:"rise,attr_range"`
	Fall              *int      `hcl:"fall"`
	FallRange         hcl.Range `hcl:"fall,attr_range"`
	EhloName          string    `hcl:"ehlo_name,optional"`
	ProbeRcpt         string    `hcl:"probe_rcpt,optional"`
	TLS               string    `hcl:"tls,optional"`
}

type rawDefaults struct {
	Range hcl.Range `hcl:",def_range"`

	EhloName string       `hcl:"ehlo_name,optional"`
	Timeouts *rawTimeouts `hcl:"timeouts,block"`
	Check    *rawCheck    `hcl:"check,block"`
}

type rawStartTLS struct {
	Range hcl.Range `hcl:",def_range"`

	Cert       string `hcl:"cert"`
	Key        string `hcl:"key"`
	MinVersion string `hcl:"min_version,optional"`
}

type rawListener struct {
	Range hcl.Range `hcl:",def_range"`

	Bind              string           `hcl:"bind"`
	Hostname          string           `hcl:"hostname"`
	HostnameRange     hcl.Range        `hcl:"hostname,attr_range"`
	StartTLS          *rawStartTLS     `hcl:"starttls,block"`
	Capabilities      []string         `hcl:"capabilities,optional"`
	CapabilitiesRange hcl.Range        `hcl:"capabilities,attr_range"`
	Auth              *rawListenerAuth `hcl:"auth,block"`
}

// rawListenerAuth is the listener's client-leg SMTP AUTH block: a set of
// users, each checked against a salted-SHA256 hash (see AuthUser).
type rawListenerAuth struct {
	Range hcl.Range `hcl:",def_range"`

	Users []rawAuthUser `hcl:"user,block"`
}

type rawAuthUser struct {
	Name  string    `hcl:",label"`
	Range hcl.Range `hcl:",def_range"`

	Salt           string `hcl:"salt"`
	HashedPassword string `hcl:"hashed_password"`
}

// rawPoolAuth is the pool's backend-leg SMTP AUTH block: the plaintext
// credentials Bifrost presents to every server in the pool.
type rawPoolAuth struct {
	Range hcl.Range `hcl:",def_range"`

	Username string `hcl:"username"`
	Password string `hcl:"password"`
}

type rawServer struct {
	Name  string    `hcl:",label"`
	Range hcl.Range `hcl:",def_range"`

	Address              string    `hcl:"address"`
	Weight               *int      `hcl:"weight"`
	WeightRange          hcl.Range `hcl:"weight,attr_range"`
	Backup               bool      `hcl:"backup,optional"`
	MaxTransactions      *int      `hcl:"max_transactions"`
	MaxTransactionsRange hcl.Range `hcl:"max_transactions,attr_range"`
	Check                *rawCheck `hcl:"check,block"`
}

type rawPool struct {
	Name  string    `hcl:",label"`
	Range hcl.Range `hcl:",def_range"`

	Balance              string       `hcl:"balance"`
	BackendTLS           string       `hcl:"backend_tls,optional"`
	BackendTLSServerName string       `hcl:"backend_tls_server_name,optional"`
	BackendTLSCA         string       `hcl:"backend_tls_ca,optional"`
	BackendTLSCARange    hcl.Range    `hcl:"backend_tls_ca,attr_range"`
	EhloName             string       `hcl:"ehlo_name,optional"`
	MaxTransactions      *int         `hcl:"max_transactions"`
	MaxTransactionsRange hcl.Range    `hcl:"max_transactions,attr_range"`
	Check                *rawCheck    `hcl:"check,block"`
	Servers              []rawServer  `hcl:"server,block"`
	Auth                 *rawPoolAuth `hcl:"auth,block"`
}

type rawRule struct {
	Range hcl.Range `hcl:",def_range"`

	ClientCIDR     []string `hcl:"client_cidr,optional"`
	MailFromDomain []string `hcl:"mail_from_domain,optional"`
	Pool           string   `hcl:"pool"`
}

type rawRouting struct {
	Range hcl.Range `hcl:",def_range"`

	Rules       []rawRule `hcl:"rule,block"`
	DefaultPool string    `hcl:"default_pool"`
}

type rawAdmin struct {
	Range hcl.Range `hcl:",def_range"`

	Bind        string `hcl:"bind"`
	AllowRemote bool   `hcl:"allow_remote,optional"`
}

type rawLimits struct {
	Range hcl.Range `hcl:",def_range"`

	GlobalMaxConn        *int      `hcl:"global_maxconn"`
	GlobalMaxConnRange   hcl.Range `hcl:"global_maxconn,attr_range"`
	MaxTransactions      *int      `hcl:"max_transactions"`
	MaxTransactionsRange hcl.Range `hcl:"max_transactions,attr_range"`
}

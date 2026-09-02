package config

import (
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
)

// Load parses and strictly decodes the HCL file at path, resolves defaults
// inheritance, and validates the result. The returned Config is nil only
// when the file could not be parsed or decoded at all; once decode
// succeeds, Load always returns a non-nil, fully-resolved Config even if
// Validate then adds error diagnostics — callers (e.g. -c) must check
// diags.HasErrors() before treating the config as usable.
func Load(path string) (*Config, hcl.Diagnostics) {
	parser := hclparse.NewParser()
	f, diags := parser.ParseHCLFile(path)
	if diags.HasErrors() {
		return nil, diags
	}

	var raw rawFile
	diags = append(diags, gohcl.DecodeBody(f.Body, nil, &raw)...)
	if diags.HasErrors() {
		return nil, diags
	}

	cfg, convDiags := raw.convert(f.Body.MissingItemRange())
	diags = append(diags, convDiags...)
	if diags.HasErrors() {
		return nil, diags
	}

	resolveFilePaths(cfg, path)
	resolveInheritance(cfg, &raw)
	diags = append(diags, resolveBackendCAs(cfg)...)
	diags = append(diags, cfg.Validate()...)

	return cfg, diags
}

// resolveBackendCAs parses every pool's backend_tls_ca file into an
// x509.CertPool exactly once per config load and publishes it on the pool
// and on every CheckParams that inherits the pool's TLS settings.
//
// Both consumers of a pool's backend-leg TLS need the same roots — the
// relay's dialer (internal/proxy) and the health prober's handshake
// (internal/health) — and neither has any business re-reading a PEM file
// per transaction or per probe. Doing it here also means an unreadable or
// certificate-free CA file is a load-time diagnostic (so `-c` catches it),
// not a starttls-verify handshake that fails closed in production.
func resolveBackendCAs(cfg *Config) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		if pool.BackendTLSCA == "" {
			continue
		}
		rng := fallbackRange(pool.caRange, pool.rng)
		pemBytes, err := os.ReadFile(pool.BackendTLSCA)
		if err != nil {
			diags = append(diags, errDiag(rng, "backend_tls_ca unreadable",
				fmt.Sprintf("backend_tls_ca %q: %s", pool.BackendTLSCA, err)))
			continue
		}
		certPool := x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(pemBytes) {
			diags = append(diags, errDiag(rng, "backend_tls_ca has no certificates",
				fmt.Sprintf("backend_tls_ca %q holds no PEM-encoded certificate.", pool.BackendTLSCA)))
			continue
		}
		pool.CAPool = certPool
		pool.Check.CAPool = certPool
		for j := range pool.Servers {
			pool.Servers[j].Check.CAPool = certPool
		}
	}

	// password_file (a Kubernetes secret mount, typically) is read here,
	// before the CheckParams copy below, so the file-derived password
	// reaches every consumer the same way an inline password would.
	// Unreadable/empty are load-time diagnostics for the same reason an
	// unreadable backend_tls_ca is: `-c` should catch it, not a probe
	// failing closed in production. A rotated secret is picked up on the
	// next reload/SIGHUP, same as any other file-backed config value.
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		auth := pool.Auth
		if auth == nil || auth.PasswordFile == "" {
			continue
		}
		rng := fallbackRange(auth.passwordFileRange, auth.rng)
		if auth.Password != "" {
			diags = append(diags, errDiag(rng, "pool auth password conflict",
				fmt.Sprintf("pool %q auth sets both password and password_file; they are mutually exclusive.", pool.Name)))
			continue
		}
		raw, err := os.ReadFile(auth.PasswordFile)
		if err != nil {
			diags = append(diags, errDiag(rng, "pool auth password_file unreadable",
				fmt.Sprintf("pool %q auth password_file %q: %s", pool.Name, auth.PasswordFile, err)))
			continue
		}
		password := strings.TrimRight(string(raw), "\r\n")
		if password == "" {
			diags = append(diags, errDiag(rng, "pool auth password_file is empty",
				fmt.Sprintf("pool %q auth password_file %q is empty.", pool.Name, auth.PasswordFile)))
			continue
		}
		auth.Password = password
	}

	// resolvePoolAuth copies every pool's backend-leg SMTP AUTH credentials
	// into that pool's health check parameters, the same way resolveBackendCAs
	// copies the TLS CA pool. Both the relay path (internal/proxy) and the
	// health prober (internal/health) need the same credentials, and
	// CheckParams is all internal/health ever sees of a pool.
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		if pool.Auth == nil {
			continue
		}
		pool.Check.AuthUsername = pool.Auth.Username
		pool.Check.AuthPassword = pool.Auth.Password
		pool.Check.AuthAllowCleartext = pool.Auth.AllowCleartext
		for j := range pool.Servers {
			pool.Servers[j].Check.AuthUsername = pool.Auth.Username
			pool.Servers[j].Check.AuthPassword = pool.Auth.Password
			pool.Servers[j].Check.AuthAllowCleartext = pool.Auth.AllowCleartext
		}
	}

	return diags
}

// resolveFilePaths rewrites file-reference attributes (TLS cert/key/CA) so
// they are relative to the config file's own directory rather than the
// process's working directory: an operator keeping certs next to their
// config expects that to work no matter where bifrost is launched from.
// Absolute paths are left untouched.
func resolveFilePaths(cfg *Config, configPath string) {
	dir := filepath.Dir(configPath)
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	if cfg.Listener.StartTLS != nil {
		cfg.Listener.StartTLS.Cert = resolve(cfg.Listener.StartTLS.Cert)
		cfg.Listener.StartTLS.Key = resolve(cfg.Listener.StartTLS.Key)
	}
	for i := range cfg.Pools {
		cfg.Pools[i].BackendTLSCA = resolve(cfg.Pools[i].BackendTLSCA)
		if cfg.Pools[i].Auth != nil {
			cfg.Pools[i].Auth.PasswordFile = resolve(cfg.Pools[i].Auth.PasswordFile)
		}
	}
}

// convert copies the strictly-decoded raw tree into a public Config.
// It performs no default-inheritance (see resolveInheritance in
// defaults.go): omitted optional fields are left at their Go zero value
// here. fileRange anchors diagnostics that have no more specific block to
// point at (e.g. zero listener blocks).
func (r *rawFile) convert(fileRange hcl.Range) (*Config, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	cfg := &Config{fileRange: fileRange}

	if r.Defaults != nil {
		def, dd := r.Defaults.convert()
		diags = append(diags, dd...)
		cfg.Defaults = def
	}

	for _, l := range r.Listeners {
		cfg.listenerRanges = append(cfg.listenerRanges, l.Range)
	}
	if len(r.Listeners) > 0 {
		listener, ld := r.Listeners[0].convert()
		diags = append(diags, ld...)
		cfg.Listener = listener
	}

	for _, p := range r.Pools {
		pool, pd := p.convert()
		diags = append(diags, pd...)
		cfg.Pools = append(cfg.Pools, pool)
	}

	cfg.Routing = r.Routing.convert()

	if r.Admin != nil {
		cfg.Admin = &Admin{Bind: r.Admin.Bind, AllowRemote: r.Admin.AllowRemote, rng: r.Admin.Range}
	}

	if r.Limits != nil {
		cfg.Limits = Limits{
			GlobalMaxConn:   intOr(r.Limits.GlobalMaxConn, 0),
			MaxTransactions: intOr(r.Limits.MaxTransactions, 0),
			rng:             r.Limits.Range,
		}
		if r.Limits.GlobalMaxConn != nil {
			cfg.Limits.maxConnRange = r.Limits.GlobalMaxConnRange
		}
		if r.Limits.MaxTransactions != nil {
			cfg.Limits.maxTxnRange = r.Limits.MaxTransactionsRange
		}
	}

	return cfg, diags
}

func (r *rawDefaults) convert() (Defaults, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	d := Defaults{EhloName: r.EhloName}

	if r.Timeouts != nil {
		t, td := r.Timeouts.convert()
		diags = append(diags, td...)
		d.Timeouts = t
	}
	if r.Check != nil {
		c, cd := r.Check.convert()
		diags = append(diags, cd...)
		d.Check = c
	}
	return d, diags
}

func (r *rawTimeouts) convert() (Timeouts, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	parse := func(s string) time.Duration {
		d, err := parseDuration(s)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid duration",
				Detail:   err.Error(),
				Subject:  &r.Range,
			})
		}
		return d
	}
	return Timeouts{
		ClientIdle:       parse(r.ClientIdle),
		SessionMax:       parse(r.SessionMax),
		BackendConnect:   parse(r.BackendConnect),
		BackendHandshake: parse(r.BackendHandshake),
		BackendMailReply: parse(r.BackendMailReply),
		Backend354Wait:   parse(r.Backend354Wait),
		DataProgress:     parse(r.DataProgress),
		BackendFinalDot:  parse(r.BackendFinalDot),
		LameDuck:         parse(r.LameDuck),
		DrainTimeout:     parse(r.DrainTimeout),
		rng:              r.Range,
	}, diags
}

func (r *rawCheck) convert() (CheckParams, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	parse := func(s string) time.Duration {
		d, err := parseDuration(s)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid duration",
				Detail:   err.Error(),
				Subject:  &r.Range,
			})
		}
		return d
	}
	return CheckParams{
		Level:        r.Level,
		Port:         intOr(r.Port, 0),
		Interval:     parse(r.Interval),
		DownInterval: parse(r.DownInterval),
		Timeout:      parse(r.Timeout),
		Rise:         intOr(r.Rise, 0),
		Fall:         intOr(r.Fall, 0),
		EhloName:     r.EhloName,
		ProbeRcpt:    r.ProbeRcpt,
		TLS:          r.TLS,
		rng:          r.Range,
	}, diags
}

func (r *rawListener) convert() (Listener, hcl.Diagnostics) {
	l := Listener{
		Bind:              r.Bind,
		Hostname:          r.Hostname,
		Capabilities:      r.Capabilities,
		rng:               r.Range,
		hostnameRange:     r.HostnameRange,
		capabilitiesRange: r.CapabilitiesRange,
	}
	if r.StartTLS != nil {
		l.StartTLS = &StartTLS{
			Cert:       r.StartTLS.Cert,
			Key:        r.StartTLS.Key,
			MinVersion: r.StartTLS.MinVersion,
			rng:        r.StartTLS.Range,
		}
	}
	if r.Auth != nil {
		l.Auth = r.Auth.convert()
	}
	return l, nil
}

// convert maps a decoded listener auth block to its resolved form,
// lowercasing HashedPassword so every later comparison is a plain byte
// match against a fixed-case hex string.
func (r *rawListenerAuth) convert() *ListenerAuth {
	la := &ListenerAuth{rng: r.Range, AllowCleartext: r.AllowCleartext}
	for _, u := range r.Users {
		la.Users = append(la.Users, AuthUser{
			Name:           u.Name,
			Salt:           u.Salt,
			HashedPassword: strings.ToLower(u.HashedPassword),
			rng:            u.Range,
		})
	}
	return la
}

func (r *rawServer) convert() (Server, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	s := Server{
		Name:            r.Name,
		Address:         r.Address,
		Weight:          intOr(r.Weight, 0),
		Backup:          r.Backup,
		MaxTransactions: intOr(r.MaxTransactions, 0),
		rng:             r.Range,
	}
	if r.Check != nil {
		c, cd := r.Check.convert()
		diags = append(diags, cd...)
		s.Check = c
	}
	return s, diags
}

func (r *rawPool) convert() (Pool, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	p := Pool{
		Name:                 r.Name,
		Balance:              r.Balance,
		BackendTLS:           r.BackendTLS,
		BackendTLSServerName: r.BackendTLSServerName,
		BackendTLSCA:         r.BackendTLSCA,
		EhloName:             r.EhloName,
		MaxTransactions:      intOr(r.MaxTransactions, 0),
		ReuseEnvelopes:       intOr(r.ReuseEnvelopes, 0),
		rng:                  r.Range,
		caRange:              r.BackendTLSCARange,
	}
	if r.MaxTransactions != nil {
		p.maxTxnRange = r.MaxTransactionsRange
	}
	if r.ReuseEnvelopes != nil {
		p.reuseEnvelopesRange = r.ReuseEnvelopesRange
	}
	if r.Check != nil {
		c, cd := r.Check.convert()
		diags = append(diags, cd...)
		p.Check = c
	}
	for _, rs := range r.Servers {
		s, sd := rs.convert()
		diags = append(diags, sd...)
		p.Servers = append(p.Servers, s)
	}
	if r.Auth != nil {
		p.Auth = &PoolAuth{
			Username:          r.Auth.Username,
			Password:          r.Auth.Password,
			PasswordFile:      r.Auth.PasswordFile,
			AllowCleartext:    r.Auth.AllowCleartext,
			rng:               r.Auth.Range,
			passwordFileRange: r.Auth.PasswordFileRange,
		}
	}
	return p, diags
}

func (r *rawRouting) convert() Routing {
	routing := Routing{DefaultPool: r.DefaultPool, rng: r.Range}
	for _, rule := range r.Rules {
		routing.Rules = append(routing.Rules, RoutingRule{
			ClientCIDR:     rule.ClientCIDR,
			MailFromDomain: rule.MailFromDomain,
			Pool:           rule.Pool,
			rng:            rule.Range,
		})
	}
	return routing
}

// intOr dereferences p, or returns fallback if p is nil (the attribute was
// omitted from the HCL source entirely).
func intOr(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}

// parseDuration parses a Go duration string, treating "" (omitted) as
// zero rather than an error — callers that need to distinguish "omitted"
// from "explicitly zero" must inspect the raw string themselves (see
// resolveInheritance).
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid duration: %w", s, err)
	}
	return d, nil
}

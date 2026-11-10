package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
)

// Holder is the atomic runtime swap point hot reload (D14) uses to
// publish a newly validated Config: Load and Swap operate on a single
// pointer, so a reader always sees either the complete old config or the
// complete new one, never a value with only some fields updated. The
// zero value has nothing to Load until the first Swap.
type Holder struct {
	p atomic.Pointer[Config]
}

// Load returns the current config, or nil if Swap has never been called.
func (h *Holder) Load() *Config {
	return h.p.Load()
}

// Swap atomically replaces the current config with cfg and returns the
// config that was current beforehand (nil on the first call).
func (h *Holder) Swap(cfg *Config) (old *Config) {
	return h.p.Swap(cfg)
}

// BindChange reports the one class of change a running process cannot
// apply: a bind address. The listener socket and the admin socket are
// already open, and v1 does not re-bind them (D14 — long-lived sessions
// must survive a reload, so there is no HAProxy-style socket handover),
// so a file that moves either is rejected WHOLE rather than swapped in
// with one attribute quietly ignored — an operator who edits the bind and
// sees "reloaded" would otherwise believe Bifrost moved.
//
// Listeners are compared by NAME, not position: a bind edit on a name
// present in both old and newCfg is this rejection, naming the listener
// that moved. A name only one side has — a listener added or removed
// outright — is NOT this rejection: there is no surviving socket that
// silently moved, just a new one that needs a restart to open (or an old
// one that keeps accepting until the process restarts), so that case is a
// RestartRequired warning instead (see there), the same precedent as
// every other listener-identity edit.
//
// It returns nil when no surviving listener's bind changed and the admin
// bind didn't either, including when old is nil (an initial load has
// nothing to compare against).
func BindChange(old, newCfg *Config) error {
	if old == nil || newCfg == nil {
		return nil
	}
	oldByName := listenersByName(old)
	newByName := listenersByName(newCfg)
	for _, name := range listenerNameUnion(old, newCfg) {
		oldL, inOld := oldByName[name]
		newL, inNew := newByName[name]
		if !inOld || !inNew {
			continue // added/removed: RestartRequired's warning, not a bind rejection
		}
		if oldL.Bind != newL.Bind {
			return fmt.Errorf("listener %q bind changed (%q -> %q): restart required",
				name, oldL.Bind, newL.Bind)
		}
	}
	if o, n := adminBind(old), adminBind(newCfg); o != n {
		return fmt.Errorf("admin bind changed (%q -> %q): restart required", o, n)
	}
	return nil
}

// RestartRequired lists the edits in newCfg that a reload accepts but
// cannot put into force: the values the accept loop and each Session read
// once per process — each listener's identity, advertised capabilities,
// auth, and client-facing TLS cert/key/min_version (cmd/bifrost/wire.go's
// listen/listenerTLS load all four exactly once at startup) — plus the
// accept cap and session_max; client_idle only for its between-commands
// half — its in-transaction deadlines re-read the holder at every
// transaction and need no restart — as opposed to everything pool/
// server/routing-shaped, which every transaction re-reads from the
// holder.
//
// Unlike BindChange these are warnings, not a rejection — the rest of the
// file is applied — but they have to be named: an operator who edits the
// hostname, sees "config reloaded", and gets the old banner would
// otherwise have no way to know why.
//
// The four per-listener fields above are compared by NAME, not position,
// the same as BindChange: a listener present in both old and newCfg is a
// survivor and gets a field-by-field diff. A name only one side has is
// reported as "added" or "removed" instead of diffed field-by-field — it
// has no counterpart to diff against, and still needs a restart (a new
// listener's socket isn't open yet; a removed one keeps accepting until
// the process restarts).
//
// A single-listener config — the common case, bifrost.hcl's one unlabeled
// "default" listener — keeps today's unqualified wording ("listener
// hostname changed", not `listener "default" hostname changed`):
// test/integration/reload_test.go greps the log for exactly the former
// substring, and quoting a name nobody wrote in the file reads oddly when
// there is nothing to tell it apart from. The quoted form appears only
// once there is more than one listener configured on either side of the
// diff.
func RestartRequired(old, newCfg *Config) []string {
	if old == nil || newCfg == nil {
		return nil
	}
	var out []string
	add := func(what string, from, to any) {
		out = append(out, fmt.Sprintf("%s changed (%v -> %v): restart required to apply", what, from, to))
	}

	oldByName := listenersByName(old)
	newByName := listenersByName(newCfg)
	qualify := len(old.Listeners) > 1 || len(newCfg.Listeners) > 1
	for _, name := range listenerNameUnion(old, newCfg) {
		oldL, inOld := oldByName[name]
		newL, inNew := newByName[name]
		switch {
		case inOld && !inNew:
			out = append(out, fmt.Sprintf("listener %q removed: restart required to apply", name))
			continue
		case !inOld && inNew:
			out = append(out, fmt.Sprintf("listener %q added: restart required to apply", name))
			continue
		}

		hostnameWhat, capsWhat := "listener hostname", "listener capabilities"
		authMsg, startTLSMsg := "listener auth changed: restart required to apply",
			"listener starttls changed: restart required to apply"
		if qualify {
			hostnameWhat = fmt.Sprintf("listener %q hostname", name)
			capsWhat = fmt.Sprintf("listener %q capabilities", name)
			authMsg = fmt.Sprintf("listener %q auth changed: restart required to apply", name)
			startTLSMsg = fmt.Sprintf("listener %q starttls changed: restart required to apply", name)
		}
		if oldL.Hostname != newL.Hostname {
			add(hostnameWhat, oldL.Hostname, newL.Hostname)
		}
		if !slices.Equal(oldL.Capabilities, newL.Capabilities) {
			add(capsWhat, oldL.Capabilities, newL.Capabilities)
		}
		if startTLSChanged(oldL.StartTLS, newL.StartTLS) {
			out = append(out, startTLSMsg)
		}
		if listenerAuthChanged(oldL.Auth, newL.Auth) {
			// Bypasses add(from, to): a Session captures Listener.Auth at
			// accept (see internal/proxy), so revoking or re-keying a user
			// here is exactly BindChange's "silently applies to nobody"
			// failure mode — but unlike hostname/capabilities, the "from"/"to"
			// values here are credential material (salts, SHA-256 hashes) that
			// must never land in a reload log or a POST /reload response.
			out = append(out, authMsg)
		}
	}

	if old.Limits.GlobalMaxConn != newCfg.Limits.GlobalMaxConn {
		add("global_maxconn", old.Limits.GlobalMaxConn, newCfg.Limits.GlobalMaxConn)
	}
	if old.Defaults.Timeouts.ClientIdle != newCfg.Defaults.Timeouts.ClientIdle {
		out = append(out, fmt.Sprintf(
			"client_idle changed (%v -> %v): restart required for the between-commands idle window only; in-transaction deadlines already use the new value at the next transaction",
			old.Defaults.Timeouts.ClientIdle, newCfg.Defaults.Timeouts.ClientIdle))
	}
	if old.Defaults.Timeouts.SessionMax != newCfg.Defaults.Timeouts.SessionMax {
		add("session_max", old.Defaults.Timeouts.SessionMax, newCfg.Defaults.Timeouts.SessionMax)
	}
	if templateKeysChanged(old, newCfg) {
		// internal/discover.Runner starts exactly one goroutine per
		// server_dns template when Run begins and can neither start nor
		// stop one afterward (its own doc comment) — identical in kind to
		// BindChange/the listener-identity warnings above. This also
		// covers editing a surviving template's address or
		// resolve_interval: Run's goroutine captured the old ServerDNS by
		// value at startup and resolve keeps using that copy's host/
		// interval forever, so those edits are just as silently inert as
		// an added/removed template unless warned about here.
		out = append(out, "server_dns template set or settings changed: restart required to apply")
	}
	return out
}

// listenerAuthChanged reports whether old and newCfg's listener auth
// differ: one is configured and the other isn't, the user sets don't
// match (name, salt, or hash) — order-independent, so reordering user
// blocks in the file alone doesn't spuriously warn — or AllowCleartext
// flips. AllowCleartext gates a Session's plaintext-AUTH acceptance the
// same way the user set gates AUTH at all, and a Session reads both at
// accept, so toggling it alone (tightening it OFF, most importantly)
// needs the same "restart required" warning as a credential edit does.
// listenersByName indexes cfg's listeners by Name, for the per-listener
// reload comparisons above (BindChange, RestartRequired). Validate
// enforces unique names the moment there is more than one listener block
// (validateListenerIdentity); a hand-built Config with colliding names
// bypasses that and is the caller's own problem.
func listenersByName(cfg *Config) map[string]Listener {
	m := make(map[string]Listener, len(cfg.Listeners))
	for _, l := range cfg.Listeners {
		m[l.Name] = l
	}
	return m
}

// listenerNameUnion returns every listener name appearing in old or
// newCfg (or both), deduped, sorted — so BindChange reports a
// deterministic "first" surviving bind mismatch when more than one
// exists, and RestartRequired's added/removed/changed warnings come out
// in a stable order run to run.
func listenerNameUnion(old, newCfg *Config) []string {
	seen := make(map[string]bool, len(old.Listeners)+len(newCfg.Listeners))
	var names []string
	for _, l := range old.Listeners {
		if !seen[l.Name] {
			seen[l.Name] = true
			names = append(names, l.Name)
		}
	}
	for _, l := range newCfg.Listeners {
		if !seen[l.Name] {
			seen[l.Name] = true
			names = append(names, l.Name)
		}
	}
	sort.Strings(names)
	return names
}

// startTLSChanged reports whether old and newCfg's listener StartTLS
// configuration differ: one is configured and the other isn't, or the
// cert/key/min_version paths differ. Like Capabilities and Auth, a
// listener's StartTLS is read once per process — cmd/bifrost/wire.go
// loads the certificate and builds the *tls.Config at startup, not per
// connection (see NewSession's doc comment) — so a reload cannot swap an
// open listener's certificate or minimum version without a restart.
func startTLSChanged(old, newCfg *StartTLS) bool {
	if (old == nil) != (newCfg == nil) {
		return true
	}
	if old == nil {
		return false
	}
	return old.Cert != newCfg.Cert || old.Key != newCfg.Key || old.MinVersion != newCfg.MinVersion
}

func listenerAuthChanged(old, newCfg *ListenerAuth) bool {
	if (old == nil) != (newCfg == nil) {
		return true
	}
	if old == nil {
		return false
	}
	if old.AllowCleartext != newCfg.AllowCleartext {
		return true
	}
	if len(old.Users) != len(newCfg.Users) {
		return true
	}
	oldKeys := authUserKeys(old.Users)
	newKeys := authUserKeys(newCfg.Users)
	sort.Strings(oldKeys)
	sort.Strings(newKeys)
	return !slices.Equal(oldKeys, newKeys)
}

// templateKeysChanged reports whether the set of server_dns templates
// (by TemplateKey, pool+label) differs between old and newCfg — added,
// removed, or both — or whether a surviving key's address or
// resolve_interval changed; order and membership deltas within an
// unchanged template don't count, only its declared set/settings do.
func templateKeysChanged(old, newCfg *Config) bool {
	oldKeys := templateKeySet(old)
	newKeys := templateKeySet(newCfg)
	if len(oldKeys) != len(newKeys) {
		return true
	}
	for k, oldTmpl := range oldKeys {
		newTmpl, ok := newKeys[k]
		if !ok {
			return true
		}
		if oldTmpl.Address != newTmpl.Address || oldTmpl.ResolveInterval != newTmpl.ResolveInterval {
			return true
		}
	}
	return false
}

// templateKeySet collects every (pool, template) pair cfg declares,
// keyed to its full ServerDNS so templateKeysChanged can compare a
// surviving key's address/resolve_interval, not just its presence.
func templateKeySet(cfg *Config) map[TemplateKey]ServerDNS {
	keys := make(map[TemplateKey]ServerDNS)
	for _, p := range cfg.Pools {
		for _, tmpl := range p.DNSTemplates {
			keys[TemplateKey{Pool: p.Name, Template: tmpl.Name}] = tmpl
		}
	}
	return keys
}

// authUserKeys reduces each AuthUser to one comparable string (its full
// credential triple, NUL-separated so no field can bleed into the next).
func authUserKeys(users []AuthUser) []string {
	keys := make([]string, len(users))
	for i, u := range users {
		keys[i] = u.Name + "\x00" + u.Salt + "\x00" + u.HashedPassword
	}
	return keys
}

// adminBind is cfg's admin bind, or "" when it has no admin block —
// which makes adding or removing the block a change like any other.
func adminBind(cfg *Config) string {
	if cfg.Admin == nil {
		return ""
	}
	return cfg.Admin.Bind
}

// DiffSummary describes what changed between old and new at the
// pool/server level, for the reload log (D14). old == nil describes an
// initial load rather than a diff. The order of clauses is deterministic
// (sorted) so log output is stable across runs.
func DiffSummary(old, newCfg *Config) string {
	if old == nil {
		return "initial config load"
	}

	oldPools := poolsByName(old)
	newPools := poolsByName(newCfg)

	var changes []string
	for name := range newPools {
		if _, ok := oldPools[name]; !ok {
			changes = append(changes, fmt.Sprintf("pool %q added", name))
		}
	}
	for name := range oldPools {
		if _, ok := newPools[name]; !ok {
			changes = append(changes, fmt.Sprintf("pool %q removed", name))
		}
	}
	for name, np := range newPools {
		if op, ok := oldPools[name]; ok {
			changes = append(changes, diffServers(name, op, np)...)
		}
	}

	if len(changes) == 0 {
		return "no changes"
	}
	sort.Strings(changes)
	return strings.Join(changes, "; ")
}

func poolsByName(cfg *Config) map[string]Pool {
	m := make(map[string]Pool, len(cfg.Pools))
	for _, p := range cfg.Pools {
		m[p.Name] = p
	}
	return m
}

// diffServers reports added/removed servers within pool, and "changed"
// for any server present in both whose operationally significant fields
// (address, weight, backup, max_transactions) differ. It does not diff
// CheckParams field-by-field: a pool/server rename of a health-check
// knob still shows up as a config change upstream, just not itemized
// here — ponytail: the four fields above are what operators scan a
// reload log for; per-field CheckParams diffing is the upgrade if that
// ever proves insufficient.
func diffServers(pool string, old, newPool Pool) []string {
	oldServers := make(map[string]Server, len(old.Servers))
	for _, s := range old.Servers {
		oldServers[s.Name] = s
	}
	newServers := make(map[string]Server, len(newPool.Servers))
	for _, s := range newPool.Servers {
		newServers[s.Name] = s
	}

	var changes []string
	for name := range newServers {
		if _, ok := oldServers[name]; !ok {
			changes = append(changes, fmt.Sprintf("pool %q: server %q added", pool, name))
		}
	}
	for name := range oldServers {
		if _, ok := newServers[name]; !ok {
			changes = append(changes, fmt.Sprintf("pool %q: server %q removed", pool, name))
		}
	}
	for name, ns := range newServers {
		oldServer, ok := oldServers[name]
		if !ok {
			continue
		}
		if serverChanged(oldServer, ns) {
			changes = append(changes, fmt.Sprintf("pool %q: server %q changed", pool, name))
		}
	}
	return changes
}

func serverChanged(a, b Server) bool {
	return a.Address != b.Address ||
		a.Weight != b.Weight ||
		a.Backup != b.Backup ||
		a.MaxTransactions != b.MaxTransactions
}

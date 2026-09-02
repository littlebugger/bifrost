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
// It returns nil when neither bind changed, including when old is nil (an
// initial load has nothing to compare against).
func BindChange(old, newCfg *Config) error {
	if old == nil || newCfg == nil {
		return nil
	}
	if old.Listener.Bind != newCfg.Listener.Bind {
		return fmt.Errorf("listener bind changed (%q -> %q): restart required",
			old.Listener.Bind, newCfg.Listener.Bind)
	}
	if o, n := adminBind(old), adminBind(newCfg); o != n {
		return fmt.Errorf("admin bind changed (%q -> %q): restart required", o, n)
	}
	return nil
}

// RestartRequired lists the edits in newCfg that a reload accepts but
// cannot put into force: the values the accept loop and each Session read
// once per process (the listener's identity and advertised capabilities,
// the accept cap, and session_max; client_idle only for its
// between-commands half — its in-transaction deadlines re-read the
// holder at every transaction and need no restart), as opposed to
// everything pool/server/routing-shaped, which every transaction
// re-reads from the holder.
//
// Unlike BindChange these are warnings, not a rejection — the rest of the
// file is applied — but they have to be named: an operator who edits the
// hostname, sees "config reloaded", and gets the old banner would
// otherwise have no way to know why.
func RestartRequired(old, newCfg *Config) []string {
	if old == nil || newCfg == nil {
		return nil
	}
	var out []string
	add := func(what string, from, to any) {
		out = append(out, fmt.Sprintf("%s changed (%v -> %v): restart required to apply", what, from, to))
	}
	if old.Listener.Hostname != newCfg.Listener.Hostname {
		add("listener hostname", old.Listener.Hostname, newCfg.Listener.Hostname)
	}
	if !slices.Equal(old.Listener.Capabilities, newCfg.Listener.Capabilities) {
		add("listener capabilities", old.Listener.Capabilities, newCfg.Listener.Capabilities)
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
	if listenerAuthChanged(old.Listener.Auth, newCfg.Listener.Auth) {
		// Bypasses add(from, to): a Session captures Listener.Auth at
		// accept (see internal/proxy), so revoking or re-keying a user
		// here is exactly BindChange's "silently applies to nobody"
		// failure mode — but unlike hostname/capabilities, the "from"/"to"
		// values here are credential material (salts, SHA-256 hashes) that
		// must never land in a reload log or a POST /reload response.
		out = append(out, "listener auth changed: restart required to apply")
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

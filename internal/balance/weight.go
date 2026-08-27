package balance

import (
	"sort"
	"sync"
)

// Weights is the admin API's runtime weight-override table (epic-09's
// POST .../weight, Router.SetWeight): a value here shadows a server's
// configured Weight for every picking algorithm in this package,
// without ever touching internal/config's own Config tree. That is
// deliberate, not just convenient: a reload always rebuilds Config from
// scratch (config.Load), so an override living anywhere on that tree
// would either vanish on its own (fine) or, worse, race a concurrent
// reload reading the same *config.Server for its diff summary. Keeping
// it here instead makes D15's "runtime weight reverts to config on
// reload" automatic — Router.ResetWeights is the explicit discard the
// reload endpoint calls, not a mechanism the picking path needs to
// know about at all.
//
// Every picking function below takes it as a trailing, optional argument
// (variadic, exactly one meaningful): every pre-epic-09 call site — this
// package's own tests included — keeps compiling with no override table
// at all, which behaves exactly like today (Weight always comes from
// config).
type Weights struct {
	mu   sync.Mutex
	over map[svrKey]int
}

// of returns s's effective weight for pool: the override if one is set,
// else configured. A nil *Weights (the common case: no admin override
// ever set) always returns configured.
func (w *Weights) of(pool, server string, configured int) int {
	if w == nil {
		return configured
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if v, ok := w.over[svrKey{pool, server}]; ok {
		return v
	}
	return configured
}

// set installs a runtime override for (pool, server).
func (w *Weights) set(pool, server string, weight int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.over == nil {
		w.over = make(map[svrKey]int)
	}
	w.over[svrKey{pool, server}] = weight
}

// resetAll discards every override — called on a successful reload
// (D15) — and returns the sorted "pool/server" identities that had one,
// for the reload endpoint's discard log.
func (w *Weights) resetAll() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.over) == 0 {
		return nil
	}
	out := make([]string, 0, len(w.over))
	for k := range w.over {
		out = append(out, k.pool+"/"+k.server)
	}
	w.over = nil
	sort.Strings(out)
	return out
}

// weightsArg returns overrides[0], or nil if empty — the
// optional-trailing-argument shape every picking function below uses.
// More than one argument is a caller bug (there is only ever one
// Weights table to wire in), and a nil first argument followed by a
// real one would otherwise silently discard the real one instead of
// using it — both panic rather than guess.
func weightsArg(overrides []*Weights) *Weights {
	if len(overrides) > 1 {
		panic("balance: at most one optional override")
	}
	if len(overrides) == 1 {
		return overrides[0]
	}
	return nil
}

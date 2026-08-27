package balance

import "sync"

// Lease tracks in-flight transactions per (pool, server) — leastconn's
// own input (leastconn.go), and epic 08's max_transactions gate. Keyed
// by name (svrKey, wrr.go's comment explains why), so counts survive a
// config reload for a server that persists across it.
type Lease struct {
	mu       sync.Mutex
	inflight map[svrKey]int
}

// NewLease returns an empty Lease.
func NewLease() *Lease {
	return &Lease{inflight: make(map[svrKey]int)}
}

// Acquire registers one in-flight transaction against (pool, server) and
// returns the release to call when it ends.
func (l *Lease) Acquire(pool, server string) func() {
	release, _ := l.TryAcquire(pool, server, 0)
	return release
}

// TryAcquire is Acquire with an atomic cap check: it registers the
// transaction and returns (release, true) only if doing so keeps
// (pool, server) at or under cap in-flight transactions (0 means
// unlimited, always ok). Reporting the cap at the exact moment of
// commitment is what Candidates' own filter (candidates.go) cannot do
// by itself: that filter reads a snapshot when a transaction is picked,
// and the pick-to-commit gap (the dial and handshake in between) is
// wide enough that several transactions can race through it having all
// seen the same under-cap snapshot. Router.Lease (epic 08) is what
// calls this; a denied caller must not treat the candidate as attached.
func (l *Lease) TryAcquire(pool, server string, maxTxn int) (release func(), ok bool) {
	key := svrKey{pool, server}
	l.mu.Lock()
	if maxTxn > 0 && l.inflight[key] >= maxTxn {
		l.mu.Unlock()
		return nil, false
	}
	l.inflight[key]++
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		l.inflight[key]--
		l.mu.Unlock()
	}, true
}

// Count returns (pool, server)'s current in-flight count.
func (l *Lease) Count(pool, server string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight[svrKey{pool, server}]
}

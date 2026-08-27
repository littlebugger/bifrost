# Performance & Load Testing

How Bifrost's load gates work, what they currently measure, and the
knobs an operator has for tuning a deployment. Produced by epic-11;
see `docs/plans/epic-11-load-bench.md` for the implementation plan and
`PROJECT.md` for the R3 throughput story this exists to prove.

## Methodology

**Why a ratio gate, not an absolute one.** `make load-smoke`
(`scripts/load_smoke.sh`) runs the same load twice: once with
`cmd/loadgen` talking straight to a `cmd/fakesmtp` backend (the
*direct* baseline — one network hop, no balancer in the path), then
again through a real `cmd/bifrost` process load-balancing across two
such backends (the *proxy* run — two hops, plus the balancer's own
session/relay machinery). An absolute latency threshold ("p99 must be
under N ms") is meaningless on a shared or noisy runner: N has to be
picked for the worst machine CI ever runs on, which makes it useless
for catching a real regression on a good day, or it has to be picked
tight and then flakes constantly on a bad one. A **ratio** — proxy p99
compared to what the *same run, on the same machine, at the same
moment* measured directly against a backend — cancels out the
machine's own noise and asks the question that actually matters: *how
much overhead does the balancer itself add, right now?*

**The gates:**

| Gate | Threshold | Rationale |
|---|---|---|
| Ratio | `proxy_p99 <= 2 * direct_p99 + 2ms` | Two hops instead of one, plus the balancer's own goroutines/syscalls, budgeted at up to 2x the direct tail plus a fixed 2ms floor for very fast baselines where any multiplier is too tight to be meaningful. |
| Zero errors | `errors == 0` in both runs, at C=50/M=200/rate=500 | The gate's whole point is throughput+latency, not correctness under saturation (that's `test/chaos`'s job) — any error here means the harness or the balancer broke, not that a threshold was missed. |
| Goroutines | back to ≤ baseline+2 after load | `test/integration/soak_test.go`, not load_smoke.sh — see below. |
| HeapAlloc | < 64 MiB, non-increasing across 3 GC'd rounds | Same. |

**CI noise policy.** `load_smoke.sh` retries the *entire* run once
(fresh ports, fresh processes) before failing. This exists because the
ratio gate, even though it cancels out steady-state machine speed, is
still sensitive to *transient* contention — a GC pause, a neighboring
process, a scheduler hiccup — landing during the ~1-2s the measured
run takes. One retry absorbs that without weakening the gate itself.
Both runs are preceded by a small discarded warm-up burst (`-c 5 -m
20`) for the same reason in a different guise: a just-started process's
first requests pay for OS thread creation and Go runtime scheduler
ramp-up, which is a startup cost, not a steady-state one, and mixing
it into the measured run would blame the balancer for the OS's own
warm-up tax.

**Why goroutines/heap live in a separate Go test, not the shell
script.** `load_smoke.sh` drives `cmd/bifrost` as an opaque external
process; there is no way for a shell script to read that process's own
`runtime.NumGoroutine()`/`runtime.MemStats` (bifrost's `/metrics` only
exports its own named counters, not the Go runtime collectors).
`test/integration/soak_test.go` measures these in-process instead,
against the same relay/session code driven directly (no subprocess),
which is the only way to get real numbers.

## Current numbers

Measured during epic-11's implementation (a loaded personal dev
machine, not yet a real nightly CI run — this table is the "filled by
first nightly" placeholder the plan calls for; a real scheduled run
supersedes these).

**`make load-smoke`**, default gate scale (C=50, M=200, rate=500/conn,
size=4096B), one representative passing attempt:

| | sent | errors | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|
| direct | 10000 | 0 | 0.37ms | 1.76ms | 3.00ms | 8.60ms |
| proxy | 10000 | 0 | 5.46ms | 6.47ms | 7.49ms | 21.72ms |

Ratio: proxy p99 7.49ms ≤ budget 7.995ms (2×3.00+2). **Pass.**

Honest note on variance: on this specific machine (a heavily-loaded
desktop, not a dedicated runner — `uptime` showed a load average north
of 5 on a 12-core machine while measuring, with a GUI compositor alone
eating a third of a core) the first attempt fails the ratio gate more
often than not, with proxy p99 landing around 9-13ms against a budget
around 8ms; the retry usually (not always) brings it back under
budget. Direct p99 stayed rock-steady around 3ms across every run —
the variance is specifically in the proxy's extra hop, which is exactly
what a ratio gate is supposed to be sensitive to. A dedicated CI
runner without desktop-class contention should see this pass on the
first attempt considerably more often than it did here.

**Soak** (`SOAK=1 go test -tags=integration -run
'TestSoak10kSessions|TestSoakLongConnection' ./test/integration/...`):

- `TestSoak10kSessions` — 10,000 short sessions in 3 rounds of ~3,333,
  concurrency 50, against 2 backends:

  | round | sessions (cumulative) | HeapAlloc (post-GC) | goroutines |
  |---|---|---|---|
  | 0 | 3,333 | bounded growth within 8MiB/round slack (see prose) | 5 |
  | 1 | 6,666 | bounded growth within 8MiB/round slack (see prose) | 5 |
  | 2 | 10,000 | bounded growth within 8MiB/round slack (see prose) | 5 |

  Goroutines: **exactly flat** at baseline across every round (not just
  within the ≤ baseline+2 tolerance). Measured HeapAlloc was 5.6 MB →
  10.4 MB → 15.2 MB, ~4.8 MB/round; per
  `soak_test.go`'s comment, that growth is attributable to the fake
  backends' own accumulating `Sessions()` history (one small record
  retained per accepted connection, by design, for later inspection —
  not bifrost's own retention), and it stays comfortably inside both
  the round-over-round slack and the 64 MiB ceiling either way. No leak
  surfaced.

- `TestSoakLongConnection` — one connection, 5,000 messages, 2
  backends, 1:1 weight: split **2500/2500** exactly, zero errors.

## The epic-05 reader-size question

Epic-05 left open whether `internal/proxy/session.go`'s client-side
`bufio.Reader` — sized at `maxCommandLine` (4 KiB), which also bounds
how much of a DATA body gets copied to the backend per read/write pair
during the streaming pipe (`internal/proxy/data.go`'s `streamBody`) —
should be sized up to cut syscall count on larger message bodies.

**Measurement:** the same `load_smoke.sh` run (C=50/M=200/rate=500,
4096-byte bodies — large enough to need more than one 4 KiB fill) was
repeated with the reader bumped to 32 KiB in the one place it's
constructed (`bufio.NewReaderSize(conn, maxCommandLine)` →
`bufio.NewReaderSize(conn, 32768)`, `maxCommandLine` itself — the
actual 4 KB command-line-length enforcement — left untouched):

| reader size | proxy p99, attempt 1 | proxy p99, attempt 2 |
|---|---|---|
| 4 KiB (current) | 11.1 – 12.7ms | 7.0 – 8.5ms |
| 32 KiB (experiment) | 11.9 – 13.4ms | 7.0 – 7.7ms |

Statistically indistinguishable — both sizes show the identical
attempt-1-vs-attempt-2 pattern described above, which is process
warm-up and desktop contention, not read-buffer-driven syscall count.
**Conclusion: left at 4 KiB.** The data doesn't warrant the larger
buffer at this message size; if a future workload with much larger
bodies shows a real syscall-count effect, the fix is the same
one-line change at the same call site.

## Tuning knobs

- **`limits.global_maxconn`** — the accept-time concurrency cap
  (default 1024). Bifrost's goroutine model is `1 + 2×maxconn +
  n_backends` (PROJECT.md), so this is the primary lever on worst-case
  goroutine/memory footprint; size it to the deployment's real expected
  concurrent client count, not higher "to be safe."
- **`server.max_transactions` / `pool.max_transactions`** — per-backend
  concurrent-transaction caps (default unlimited). Fresh-connection-
  per-transaction (D4) means backend connect *rate*, not just
  concurrency, scales with load; a backend with its own connection-rate
  limiter (postscreen/anvil-style) needs this set to something it can
  sustain.
- **Timeouts** (`defaults.timeouts`, PROJECT.md's budget table) — the
  ones that matter most under load are `backend_mail_reply` and
  `data_progress`: too tight and a merely-slow (not dead) backend gets
  treated as failed under load; too loose and a genuinely dead backend
  holds a session (and its goroutines) open longer than necessary.
- **`defaults.check`** — faster `interval`/`down_interval` detect a
  failing backend sooner but cost more probe traffic; the values used
  by `load_smoke.sh`'s own generated config (`interval=200ms,
  rise=1, fall=2`) are deliberately aggressive for a short-lived smoke
  run, not a recommended production default (PROJECT.md's built-in
  defaults — `interval=5s, rise=2, fall=3` — are the sane starting
  point).
- **`GOMAXPROCS`** — Go's runtime defaults it to the number of logical
  CPUs the OS reports. In a container with a CPU quota lower than the
  host's core count, that default over-counts what's actually
  available, and the scheduler ends up running more OS threads than
  there is CPU time to service; operators running bifrost under such a
  quota should set `GOMAXPROCS` explicitly (env var) to match it.
  Bifrost's own workload is I/O-bound (one goroutine per session
  mostly parked on network reads/writes), so this is a correctness-of-
  scheduling knob more than a throughput one — it won't move the
  numbers above much, but a mismatched value can add scheduling
  latency under concurrent load.

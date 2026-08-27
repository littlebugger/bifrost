#!/usr/bin/env bash
# load_smoke.sh: builds bifrost + loadgen + fakesmtp, runs a direct
# baseline against a fake backend and then the same load through the
# real proxy, and asserts the epic-11 ratio/error gates:
#
#   proxy p99  <= 2 * direct p99 + 2ms
#   errors     == 0 in the through-proxy run (at C=50/M=200/rate=500)
#
# Retries the whole run once on gate failure (CI noise policy) before
# giving up. <30s total in the common case.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
BIN="$WORK/bin"
mkdir -p "$BIN"

PIDS=()
cleanup() {
	local pid
	for pid in "${PIDS[@]:-}"; do
		kill "$pid" >/dev/null 2>&1 || true
	done
	for pid in "${PIDS[@]:-}"; do
		wait "$pid" 2>/dev/null || true
	done
	rm -rf "$WORK"
}
trap cleanup EXIT

LOADGEN_C="${LOADGEN_C:-50}"
LOADGEN_M="${LOADGEN_M:-200}"
LOADGEN_RATE="${LOADGEN_RATE:-500}"
LOADGEN_SIZE="${LOADGEN_SIZE:-4096}"

# LOAD_SMOKE_OUT_DIR, if set, gets the latest attempt's direct.json and
# proxy.json copied into it (overwritten each attempt, so it always ends
# up holding the last one) -- CI's hook for uploading them as artifacts,
# since $WORK itself is removed by the exit trap above.
OUT_DIR="${LOAD_SMOKE_OUT_DIR:-}"
if [ -n "$OUT_DIR" ]; then
	mkdir -p "$OUT_DIR"
fi

echo "load-smoke: building bifrost, loadgen, fakesmtp"
CGO_ENABLED=0 go build -o "$BIN/bifrost" "$ROOT/cmd/bifrost"
CGO_ENABLED=0 go build -o "$BIN/loadgen" "$ROOT/cmd/loadgen"
CGO_ENABLED=0 go build -o "$BIN/fakesmtp" "$ROOT/cmd/fakesmtp"

free_port() {
	python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
}

warmup() {
	"$BIN/loadgen" -addr "$1" -c 5 -m 20 >/dev/null 2>&1 || true
}

wait_for_dial() {
	local host="$1" port="$2" deadline
	deadline=$((SECONDS + 15))
	while ! python3 -c "import socket,sys; socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=1).close()" "$host" "$port" >/dev/null 2>&1; do
		if [ "$SECONDS" -ge "$deadline" ]; then
			echo "load-smoke: $host:$port never accepted a connection" >&2
			return 1
		fi
		sleep 0.1
	done
}

# wait_for_servers_up polls GET /servers on bifrost's admin plane until
# every server it lists reports op=UP, or times out. Fast health checks
# (see the rendered config below) make this a sub-second wait in
# practice; the bound just guards against a genuinely broken backend.
wait_for_servers_up() {
	local admin_addr="$1" want="$2" deadline
	deadline=$((SECONDS + 15))
	while true; do
		local up
		up=$(python3 -c "
import json, urllib.request
try:
    body = urllib.request.urlopen('http://$admin_addr/servers', timeout=1).read()
    servers = json.loads(body)['servers']
    print(sum(1 for s in servers if s['op'] == 'UP'))
except Exception:
    print(0)
")
		if [ "$up" -ge "$want" ]; then
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			echo "load-smoke: only $up/$want backends UP after 15s" >&2
			return 1
		fi
		sleep 0.1
	done
}

# assert_gates reads the direct and proxy JSON reports and checks the
# ratio + zero-errors gates. Prints the numbers either way.
assert_gates() {
	local direct_json="$1" proxy_json="$2"
	python3 -c "
import json, sys

direct = json.load(open(sys.argv[1]))
proxy = json.load(open(sys.argv[2]))

direct_p99 = direct['p99_ms']
proxy_p99 = proxy['p99_ms']
budget = 2 * direct_p99 + 2

print(f'load-smoke: direct  sent={direct[\"sent\"]} errors={direct[\"errors\"]} p50={direct[\"p50_ms\"]:.3f}ms p95={direct[\"p95_ms\"]:.3f}ms p99={direct_p99:.3f}ms max={direct[\"max_ms\"]:.3f}ms')
print(f'load-smoke: proxy   sent={proxy[\"sent\"]} errors={proxy[\"errors\"]} p50={proxy[\"p50_ms\"]:.3f}ms p95={proxy[\"p95_ms\"]:.3f}ms p99={proxy_p99:.3f}ms max={proxy[\"max_ms\"]:.3f}ms')
print(f'load-smoke: ratio gate: proxy p99 {proxy_p99:.3f}ms <= budget {budget:.3f}ms (2*direct_p99+2)')

ok = True
if proxy_p99 > budget:
    print(f'load-smoke: FAIL ratio gate: proxy p99 {proxy_p99:.3f}ms > budget {budget:.3f}ms')
    ok = False
if proxy['errors'] != 0:
    print(f'load-smoke: FAIL zero-errors gate: proxy errors={proxy[\"errors\"]}, want 0')
    ok = False
if direct['errors'] != 0:
    print(f'load-smoke: FAIL direct baseline errors={direct[\"errors\"]}, want 0 (broken test setup)')
    ok = False

sys.exit(0 if ok else 1)
" "$direct_json" "$proxy_json"
}

render_config() {
	local smtp_addr="$1" admin_addr="$2" fake1_addr="$3" fake2_addr="$4" path="$5"
	cat >"$path" <<EOF
defaults {
  timeouts {
    client_idle        = "20s"
    session_max        = "5m"
    backend_connect     = "2s"
    backend_handshake   = "2s"
    backend_mail_reply  = "5s"
    backend_354_wait    = "5s"
    data_progress       = "10s"
    backend_final_dot   = "10s"
    lame_duck           = "100ms"
    drain_timeout       = "5s"
  }
  check {
    level         = "ehlo"
    interval      = "200ms"
    down_interval = "200ms"
    timeout       = "150ms"
    rise          = 1
    fall          = 2
  }
}

listener {
  bind         = "$smtp_addr"
  hostname     = "bifrost.loadsmoke"
  capabilities = ["PIPELINING", "8BITMIME"]
}

pool "p" {
  balance = "roundrobin"
  server "a" {
    address = "$fake1_addr"
    weight  = 1
  }
  server "b" {
    address = "$fake2_addr"
    weight  = 1
  }
}

routing {
  default_pool = "p"
}

limits {
  global_maxconn = 4096
}

admin {
  bind = "$admin_addr"
}
EOF
}

attempt() {
	local n="$1"
	local fake1_port fake2_port smtp_port admin_port
	local fake1_pid fake2_pid bifrost_pid
	fake1_port=$(free_port)
	fake2_port=$(free_port)
	smtp_port=$(free_port)
	admin_port=$(free_port)

	# This attempt's own fake1/fake2/bifrost must not survive into the
	# next attempt: a failed attempt 1 leaving its fakes running would
	# have attempt 2's real measurement contending against them, quietly
	# defeating the "fresh ports, fresh processes" retry. A RETURN trap
	# covers every exit from this function (every early "|| return 1"
	# included), not just the happy-path fall-through.
	trap '
		[ -n "${bifrost_pid:-}" ] && { kill "$bifrost_pid" >/dev/null 2>&1 || true; wait "$bifrost_pid" 2>/dev/null || true; }
		[ -n "${fake1_pid:-}" ] && { kill "$fake1_pid" >/dev/null 2>&1 || true; wait "$fake1_pid" 2>/dev/null || true; }
		[ -n "${fake2_pid:-}" ] && { kill "$fake2_pid" >/dev/null 2>&1 || true; wait "$fake2_pid" 2>/dev/null || true; }
	' RETURN

	echo "load-smoke: attempt $n: fake1=127.0.0.1:$fake1_port fake2=127.0.0.1:$fake2_port smtp=127.0.0.1:$smtp_port admin=127.0.0.1:$admin_port"

	"$BIN/fakesmtp" -listen "127.0.0.1:$fake1_port" -caps "PIPELINING,8BITMIME" >"$WORK/fake1.log" 2>&1 &
	fake1_pid=$!
	PIDS+=("$fake1_pid")
	"$BIN/fakesmtp" -listen "127.0.0.1:$fake2_port" -caps "PIPELINING,8BITMIME" >"$WORK/fake2.log" 2>&1 &
	fake2_pid=$!
	PIDS+=("$fake2_pid")
	wait_for_dial 127.0.0.1 "$fake1_port" || return 1
	wait_for_dial 127.0.0.1 "$fake2_port" || return 1

	# A small discarded burst before each measured run: a freshly started
	# process (this fake, and below, bifrost's own goroutine/OS-thread
	# pool) shows measurably higher first-burst latency purely from
	# process/runtime warm-up -- not anything the ratio gate exists to
	# catch. Standard load-testing practice; keeps that skew out of the
	# numbers that matter.
	warmup "127.0.0.1:$fake1_port"
	echo "load-smoke: direct baseline (c=$LOADGEN_C m=$LOADGEN_M rate=$LOADGEN_RATE size=$LOADGEN_SIZE)"
	"$BIN/loadgen" -addr "127.0.0.1:$fake1_port" -direct \
		-c "$LOADGEN_C" -m "$LOADGEN_M" -rate "$LOADGEN_RATE" -size "$LOADGEN_SIZE" \
		>"$WORK/direct.json" 2>"$WORK/loadgen-direct.log" || return 1

	local cfg="$WORK/bifrost-$n.hcl"
	render_config "127.0.0.1:$smtp_port" "127.0.0.1:$admin_port" "127.0.0.1:$fake1_port" "127.0.0.1:$fake2_port" "$cfg"

	"$BIN/bifrost" -f "$cfg" >"$WORK/bifrost.log" 2>&1 &
	bifrost_pid=$!
	PIDS+=("$bifrost_pid")
	wait_for_dial 127.0.0.1 "$admin_port" || return 1
	wait_for_servers_up "127.0.0.1:$admin_port" 2 || return 1

	warmup "127.0.0.1:$smtp_port"
	echo "load-smoke: through-proxy run (c=$LOADGEN_C m=$LOADGEN_M rate=$LOADGEN_RATE size=$LOADGEN_SIZE)"
	"$BIN/loadgen" -addr "127.0.0.1:$smtp_port" \
		-c "$LOADGEN_C" -m "$LOADGEN_M" -rate "$LOADGEN_RATE" -size "$LOADGEN_SIZE" \
		>"$WORK/proxy.json" 2>"$WORK/loadgen-proxy.log" || return 1

	if [ -n "$OUT_DIR" ]; then
		cp "$WORK/direct.json" "$OUT_DIR/direct.json" 2>/dev/null || true
		cp "$WORK/proxy.json" "$OUT_DIR/proxy.json" 2>/dev/null || true
	fi

	assert_gates "$WORK/direct.json" "$WORK/proxy.json"
}

for n in 1 2; do
	if attempt "$n"; then
		echo "load-smoke: PASS (attempt $n)"
		exit 0
	fi
	echo "load-smoke: attempt $n failed" >&2
	echo "load-smoke: bifrost log (last 30 lines; the transaction log is one INFO line per message, so the full log is huge):" >&2
	tail -n 30 "$WORK/bifrost.log" >&2 2>/dev/null || true
	if [ "$n" -eq 2 ]; then
		echo "load-smoke: FAIL after retry" >&2
		exit 1
	fi
	echo "load-smoke: retrying once (CI noise policy)" >&2
done

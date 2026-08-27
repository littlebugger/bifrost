#!/usr/bin/env bash
# Proves that `make verify-new` actually fails when a named test is absent
# (or gated behind a build tag nobody asked for), not just that it passes
# when everything is present. That asymmetry is the entire point of the
# target: plain `go test -run` exits 0 on zero matches.
set -u

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root" || exit 1

# Nested under testdata/ so a leftover from a crashed run is never swept up
# by ./... wildcards used elsewhere in the Makefile (go tooling skips
# "testdata" directories for wildcard expansion, but not for explicit paths
# like the one this script passes to `make verify-new PKG=...`). testdata/
# only carries a .gitkeep, so mkdir -p it here rather than assuming it's
# already materialized (mktemp won't create the missing parent itself).
mkdir -p "$root/testdata"
tmp="$(mktemp -d "$root/testdata/verify_new_selftest.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/present_test.go" <<'EOF'
package selftest

import "testing"

func TestPresent(t *testing.T) {}
EOF

cat >"$tmp/tagged_test.go" <<'EOF'
//go:build integration

package selftest

import "testing"

func TestTagged(t *testing.T) {}
EOF

pkg="./${tmp#"$root"/}"
out="$(mktemp)"
fail=0

check() {
	desc="$1" want="$2"
	shift 2
	make verify-new "$@" >"$out" 2>&1
	got=$?
	if { [ "$want" = "zero" ] && [ "$got" -eq 0 ]; } || { [ "$want" = "nonzero" ] && [ "$got" -ne 0 ]; }; then
		echo "ok: $desc (exit=$got)"
	else
		echo "FAIL: $desc (wanted $want exit, got $got)"
		cat "$out"
		fail=1
	fi
}

check "(a) TESTS=TestPresent exits zero"    zero    PKG="$pkg" TESTS="TestPresent"
check "(b) TESTS=TestAbsent exits non-zero" nonzero PKG="$pkg" TESTS="TestAbsent"
check "(c) TestTagged without TAGS exits non-zero" nonzero PKG="$pkg" TESTS="TestTagged"
check "(d) TestTagged with TAGS=integration exits zero" zero PKG="$pkg" TESTS="TestTagged" TAGS=integration

rm -f "$out"

if [ "$fail" -eq 0 ]; then
	echo "verify-new selftest: PASS"
else
	echo "verify-new selftest: FAIL"
fi
exit "$fail"

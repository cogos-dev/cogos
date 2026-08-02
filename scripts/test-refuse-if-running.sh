#!/usr/bin/env bash
# test-refuse-if-running.sh — functional regression suite for
# scripts/lib/refuse-if-running.sh's fail-closed contract.
#
# Why this file exists: PR #511's original verification (7 manual
# scenarios) was run ad hoc against a throwaway binary and never
# committed -- cog-review flagged this as a gap ("a future edit to this
# safety-critical file has no CI-enforced regression coverage for the
# detection logic"). This script is that coverage: it exercises the real
# refuse_if_running function (sourced, not re-implemented) against a real
# background process standing in for a running `cogos serve`, plus
# regression cases for the two confirmed findings against PR #511 itself
# (scripts/lib/refuse-if-running.sh:86 and :66) and for the pgrep-exit-
# status gap found while auditing the same fail-open shape -- see
# cog://mem/working/2026-07-30-self-review-spike/RETRO-486.md for why
# "inconclusive treated as not-running" is the class this file exists to
# eliminate.
#
# This intentionally does NOT use a test framework (bats etc. is not
# vendored in this repo) -- plain PASS/FAIL assertions, matching the
# existing scripts/e2e-test.sh / scripts/agent-test.sh style.
#
# Usage: ./scripts/test-refuse-if-running.sh
# Exit 0 if every scenario passes (or is explicitly SKIPped), 1 otherwise.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || (cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd))"
cd "$REPO_ROOT"

# shellcheck source=lib/refuse-if-running.sh
. "$REPO_ROOT/scripts/lib/refuse-if-running.sh"

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/refuse-if-running-test.XXXXXX")"
BG_PIDS=()

cleanup() {
    for pid in "${BG_PIDS[@]:-}"; do
        kill -9 "$pid" >/dev/null 2>&1 || true
    done
    rm -rf "$WORKDIR"
}
trap cleanup EXIT

pass=0
fail=0

# build_restricted_path STUBDIR [tool-to-omit ...]
# Populates STUBDIR with symlinks to the real system tools the guard can
# call (pgrep, ps, readlink, lsof) EXCEPT any named in the omit list, so a
# caller can set PATH="$STUBDIR" to simulate "tool X is missing" without
# also breaking every other tool the function needs to reach the branch
# under test.
build_restricted_path() {
    local stubdir="$1"
    shift
    local omit=" $* "
    local tool src
    mkdir -p "$stubdir"
    for tool in pgrep ps readlink lsof tr cat awk dirname basename sh grep printf; do
        case "$omit" in *" $tool "*) continue ;; esac
        src="$(command -v "$tool" 2>/dev/null || true)"
        [ -n "$src" ] && ln -sf "$src" "$stubdir/$tool"
    done
}

# expect_allowed NAME TARGET
# Asserts refuse_if_running returns 0 (safe to proceed).
expect_allowed() {
    local name="$1" target="$2"
    if refuse_if_running "$target" >/dev/null 2>&1; then
        echo "  PASS  $name (allowed, as expected)"
        pass=$((pass + 1))
    else
        echo "  FAIL  $name (expected allowed, was refused)"
        fail=$((fail + 1))
    fi
}

# expect_refused NAME TARGET [MUST_CONTAIN]
# Asserts refuse_if_running returns 1, and optionally that its stderr
# message contains a specific substring -- this pins the test to the
# INTENDED refusal branch, not just "refused for some reason."
expect_refused() {
    local name="$1" target="$2" must_contain="${3:-}"
    local out rc
    out="$(refuse_if_running "$target" 2>&1 1>/dev/null)" && rc=0 || rc=$?
    if [ "${rc:-0}" -eq 1 ]; then
        if [ -n "$must_contain" ] && ! printf '%s' "$out" | grep -qF "$must_contain"; then
            echo "  FAIL  $name (refused, but message did not contain: $must_contain)"
            echo "        got: $out"
            fail=$((fail + 1))
        else
            echo "  PASS  $name (refused, as expected)"
            pass=$((pass + 1))
        fi
    else
        echo "  FAIL  $name (expected refused/exit 1, got exit ${rc:-0})"
        fail=$((fail + 1))
    fi
}

echo "== refuse-if-running functional matrix =="
echo "workdir: $WORKDIR"

# Build a real, standalone binary to stand in for `cogos` -- must be a
# genuine executable image (not a shebang script), since the guard's exe
# resolution (readlink -f /proc/PID/exe on Linux, lsof's txt mapping on
# macOS) reports the actually-mapped binary, and a shebang script's mapped
# image is its interpreter, not the script itself.
#
# This is compiled fresh rather than reusing an existing system binary
# (e.g. `yes`, copied and renamed) because copying/renaming a signed
# platform binary gets SIGKILLed near-instantly by this platform's code
# integrity enforcement on exec -- a locally-built, ad-hoc-signed binary
# (and copies of it) runs and can be renamed/relocated freely, which is
# what every scenario below needs.
if ! command -v go >/dev/null 2>&1; then
    echo "SKIP: go toolchain not found on PATH; cannot build the stand-in binary this suite needs." >&2
    exit 0
fi
mkdir -p "$WORKDIR/gosrc"
cat >"$WORKDIR/gosrc/go.mod" <<'EOF'
module refuseifrunningtest

go 1.21
EOF
cat >"$WORKDIR/gosrc/main.go" <<'EOF'
package main

import (
	"os"
	"time"
)

func main() {
	_ = os.Args
	time.Sleep(10 * time.Minute)
}
EOF
(cd "$WORKDIR/gosrc" && go build -o "$WORKDIR/sleeper" .)

mkdir -p "$WORKDIR/bin" "$WORKDIR/decoy" "$WORKDIR/real" "$WORKDIR/noserve"
cp "$WORKDIR/sleeper" "$WORKDIR/bin/cogos"
cp "$WORKDIR/sleeper" "$WORKDIR/decoy/cogos"
cp "$WORKDIR/sleeper" "$WORKDIR/real/cogos-binary"
cp "$WORKDIR/sleeper" "$WORKDIR/noserve/cogos"
chmod +x "$WORKDIR/bin/cogos" "$WORKDIR/decoy/cogos" "$WORKDIR/real/cogos-binary" "$WORKDIR/noserve/cogos"

# --- Scenario 1: nonexistent target -> allowed ---
expect_allowed "nonexistent target" "$WORKDIR/does-not-exist"

# --- Scenario 2: existing, idle target (nothing executing it) -> allowed ---
touch "$WORKDIR/idle-target"
expect_allowed "existing idle target" "$WORKDIR/idle-target"

# --- Scenario 3: ALLOW_RUNNING_INSTALL=1 -> always allowed ---
"$WORKDIR/bin/cogos" serve >/dev/null 2>&1 &
BG_PIDS+=("$!"); disown "$!" 2>/dev/null || true
sleep 0.2
ALLOW_RUNNING_INSTALL=1
export ALLOW_RUNNING_INSTALL
expect_allowed "ALLOW_RUNNING_INSTALL=1 override" "$WORKDIR/bin/cogos"
unset ALLOW_RUNNING_INSTALL

# --- Scenario 4: target actively executed by a real serve process -> refused ---
expect_refused "target executed by a serve process" "$WORKDIR/bin/cogos" \
    "is being executed by PID"

# --- Scenario 5: a serve process running, but NOT executing this target -> allowed ---
"$WORKDIR/decoy/cogos" serve >/dev/null 2>&1 &
BG_PIDS+=("$!"); disown "$!" 2>/dev/null || true
sleep 0.2
expect_allowed "serve process running a different target" "$WORKDIR/decoy/cogos-not-this-one"

# --- Scenario 6: process executing the target WITHOUT 'serve' in argv -> allowed ---
"$WORKDIR/noserve/cogos" version >/dev/null 2>&1 &
BG_PIDS+=("$!"); disown "$!" 2>/dev/null || true
sleep 0.2
expect_allowed "process executing target without 'serve' in argv" "$WORKDIR/noserve/cogos"

# --- Scenario 7: pgrep missing from PATH -> refused ---
(
    STUBDIR="$WORKDIR/stub-no-pgrep"
    build_restricted_path "$STUBDIR" pgrep
    export PATH="$STUBDIR"
    if command -v pgrep >/dev/null 2>&1; then
        echo "  SKIP  pgrep missing from PATH (could not hide pgrep in this environment)"
        exit 0
    fi
    # shellcheck source=lib/refuse-if-running.sh
    . "$REPO_ROOT/scripts/lib/refuse-if-running.sh"
    out="$(refuse_if_running "$WORKDIR/idle-target" 2>&1 1>/dev/null)" && rc=0 || rc=$?
    if [ "${rc:-0}" -eq 1 ] && printf '%s' "$out" | grep -qF "pgrep not found"; then
        echo "  PASS  pgrep missing from PATH (refused, as expected)"
        exit 0
    fi
    echo "  FAIL  pgrep missing from PATH (expected refused/'pgrep not found', got rc=${rc:-0}: $out)"
    exit 1
) && pass=$((pass + 1)) || fail=$((fail + 1))

# --- Scenario 8: pgrep present but exits with a real failure status (not
# "no match") -> refused. Regression test for the pgrep-exit-status fix
# added alongside Findings 1/2: `|| true` used to flatten "pgrep errored"
# and "pgrep found nothing" into the same empty $pids, fail-open result. ---
(
    STUBDIR="$WORKDIR/stub-pgrep-error"
    build_restricted_path "$STUBDIR" pgrep
    cat >"$STUBDIR/pgrep" <<'STUB'
#!/bin/sh
exit 3
STUB
    chmod +x "$STUBDIR/pgrep"
    export PATH="$STUBDIR"
    # shellcheck source=lib/refuse-if-running.sh
    . "$REPO_ROOT/scripts/lib/refuse-if-running.sh"
    out="$(refuse_if_running "$WORKDIR/idle-target" 2>&1 1>/dev/null)" && rc=0 || rc=$?
    if [ "${rc:-0}" -eq 1 ] && printf '%s' "$out" | grep -qF "pgrep exited with status"; then
        echo "  PASS  pgrep exits with a real failure status (refused, as expected)"
        exit 0
    fi
    echo "  FAIL  pgrep exits with a real failure status (expected refused/'pgrep exited with status', got rc=${rc:-0}: $out)"
    exit 1
) && pass=$((pass + 1)) || fail=$((fail + 1))

# --- Scenario 9 (Finding 1 regression): pgrep matches a PID whose cmdline
# cannot be determined -> refused, not silently skipped. Reproduced with a
# REAL, already-exited PID (guaranteed unreadable via both /proc and ps on
# every platform) rather than a permission-boundary mock, since dropping
# to another user isn't available in a portable, unprivileged test -- this
# is the same root cause the review names: "the process exits between the
# pgrep snapshot and the read." ---
(
    true &
    dead_pid=$!
    wait "$dead_pid" 2>/dev/null || true
    if kill -0 "$dead_pid" >/dev/null 2>&1; then
        echo "  SKIP  pgrep-matched PID with unreadable cmdline (pid $dead_pid did not exit in time)"
        exit 0
    fi
    STUBDIR="$WORKDIR/stub-dead-pid"
    build_restricted_path "$STUBDIR" pgrep
    cat >"$STUBDIR/pgrep" <<STUB
#!/bin/sh
echo $dead_pid
exit 0
STUB
    chmod +x "$STUBDIR/pgrep"
    export PATH="$STUBDIR"
    # shellcheck source=lib/refuse-if-running.sh
    . "$REPO_ROOT/scripts/lib/refuse-if-running.sh"
    out="$(refuse_if_running "$WORKDIR/idle-target" 2>&1 1>/dev/null)" && rc=0 || rc=$?
    if [ "${rc:-0}" -eq 1 ] && printf '%s' "$out" | grep -qF "cannot determine the command line"; then
        echo "  PASS  pgrep-matched PID with unreadable cmdline (refused, as expected)"
        exit 0
    fi
    echo "  FAIL  pgrep-matched PID with unreadable cmdline (expected refused/'cannot determine the command line', got rc=${rc:-0}: $out)"
    exit 1
) && pass=$((pass + 1)) || fail=$((fail + 1))

# --- Scenario 10 (Finding 2 regression): install target is a symlink to a
# binary a live 'serve' process is executing -> refused. Under the old
# code, rtarget only canonicalized the symlink's parent directory and kept
# the symlink's own basename, so it never matched the fully-resolved exe
# path and the guard passed. The symlink's own basename must be exactly
# "cogos" (not e.g. "symlinked-cogos") to satisfy pgrep's anchored
# '(^|/)cogos( |$)' pattern -- same word-boundary requirement the guard
# itself uses to avoid matching "cogos-channel-bridge".
mkdir -p "$WORKDIR/symlink-dir"
ln -sf "$WORKDIR/real/cogos-binary" "$WORKDIR/symlink-dir/cogos"
"$WORKDIR/symlink-dir/cogos" serve >/dev/null 2>&1 &
BG_PIDS+=("$!"); disown "$!" 2>/dev/null || true
sleep 0.2
expect_refused "symlinked install target executed via the symlink" "$WORKDIR/symlink-dir/cogos" \
    "is being executed by PID"

# --- Scenario 11 (README install-snippet regression): a BARE call -- one
# with no `|| exit 1` suffix -- made from a caller that has `set -e` in
# effect must still run the guard to completion on the no-daemon path.
#
# Every scenario above invokes refuse_if_running in an errexit-SUPPRESSED
# context (an `if` condition, or the left side of `&&`/`||`), which is
# exactly why this suite was 10/10 green while both README install blocks
# were silently broken: they call the guard bare inside `( set -eu; ... )`,
# and pgrep's exit 1 on "no match" -- the common success path -- tripped the
# caller's errexit from inside the function body, killing the whole install
# subshell before curl/chmod/mv ever ran, with no output at all.
#
# pgrep is stubbed to a deterministic "no match" (exit 1) rather than
# relying on no cogos daemon happening to run on the test machine, so this
# exercises the failing path on any host -- including a dev laptop with a
# live kernel, where an unstubbed pgrep returns 0 and hides the bug. ---
(
    STUBDIR="$WORKDIR/stub-pgrep-nomatch"
    build_restricted_path "$STUBDIR" pgrep
    cat >"$STUBDIR/pgrep" <<'STUB'
#!/bin/sh
exit 1
STUB
    chmod +x "$STUBDIR/pgrep"
    export PATH="$STUBDIR"
    # Mirrors the README block's shape exactly: set -eu, source, bare call,
    # then the lines that would perform the install.
    out="$(
        set -eu
        # shellcheck source=lib/refuse-if-running.sh
        . "$REPO_ROOT/scripts/lib/refuse-if-running.sh"
        refuse_if_running "$WORKDIR/idle-target"
        echo "INSTALL-WOULD-PROCEED"
    )" && rc=0 || rc=$?
    if [ "${rc:-0}" -eq 0 ] && printf '%s' "$out" | grep -qF "INSTALL-WOULD-PROCEED"; then
        echo "  PASS  bare call under caller's set -e, no daemon running (guard completed, as expected)"
        exit 0
    fi
    echo "  FAIL  bare call under caller's set -e, no daemon running (expected the guard to complete and allow; got rc=${rc:-0}, output: '${out}')"
    exit 1
) && pass=$((pass + 1)) || fail=$((fail + 1))

echo ""
echo "== $pass passed, $fail failed =="
[ "$fail" -eq 0 ]

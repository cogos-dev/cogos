#!/usr/bin/env bash
# e2e-test.sh — End-to-end test for CogOS cold-start flow.
#
# Tests the full lifecycle a new user would experience:
#   1. cogos init      — scaffold a workspace
#   2. cogos serve     — start the daemon
#   3. health check    — verify the daemon is running
#   4. context query   — verify the API returns valid data
#   5. shutdown        — clean exit
#
# Exit 0 on success, 1 on any failure.
# Designed to run inside a container (see Dockerfile) or locally.

set -euo pipefail

COGOS="${COGOS_BIN:-cogos}"
WORKSPACE="${E2E_WORKSPACE:-/tmp/e2e-workspace}"
PORT="${E2E_PORT:-5299}"
TIMEOUT="${E2E_TIMEOUT:-10}"

pass=0
fail=0

check() {
    local name="$1"
    shift
    if "$@" >/dev/null 2>&1; then
        echo "  PASS  $name"
        pass=$((pass + 1))
    else
        echo "  FAIL  $name"
        fail=$((fail + 1))
    fi
}

check_output() {
    local name="$1"
    local expected="$2"
    local url="$3"
    local body
    body=$(curl -sf "$url" 2>/dev/null || echo "CURL_FAILED")
    if echo "$body" | grep -q "$expected"; then
        echo "  PASS  $name"
        pass=$((pass + 1))
    else
        echo "  FAIL  $name (expected '$expected', got: $body)"
        fail=$((fail + 1))
    fi
}

cleanup() {
    if [ -n "${DAEMON_PID:-}" ]; then
        kill "$DAEMON_PID" 2>/dev/null || true
        wait "$DAEMON_PID" 2>/dev/null || true
    fi
    rm -rf "$WORKSPACE"
}
trap cleanup EXIT

echo "=== CogOS E2E Test ==="
echo "Binary:    $COGOS"
echo "Workspace: $WORKSPACE"
echo "Port:      $PORT"
echo ""

# ── Phase 1: Init ─────────────────────────────────────────────────────────────

echo "Phase 1: Init"
$COGOS init --workspace "$WORKSPACE" 2>&1 | sed 's/^/  /'

check "workspace dir exists"      test -d "$WORKSPACE/.cog"
check "config dir exists"         test -d "$WORKSPACE/.cog/config"
check "memory dirs exist"         test -d "$WORKSPACE/.cog/mem/semantic"
check "identity card exists"      test -f "$WORKSPACE/.cog/agents/identities/identity_cogos.md"
check "kernel.yaml exists"        test -f "$WORKSPACE/.cog/config/kernel.yaml"
check "providers.yaml exists"     test -f "$WORKSPACE/.cog/config/providers.yaml"
check "VERSION exists"            test -f "$WORKSPACE/.cog/VERSION"

# Idempotency: run init again — should not fail or overwrite.
INIT2_OUT=$($COGOS init --workspace "$WORKSPACE" 2>&1)
if echo "$INIT2_OUT" | grep -q "already existed"; then
    echo "  PASS  init is idempotent"
    pass=$((pass + 1))
else
    echo "  FAIL  init is idempotent"
    fail=$((fail + 1))
fi

echo ""

# ── Phase 2: Serve ────────────────────────────────────────────────────────────

echo "Phase 2: Serve"
$COGOS serve --workspace "$WORKSPACE" --port "$PORT" &
DAEMON_PID=$!

# Wait for the daemon to be ready.
ready=false
for i in $(seq 1 "$TIMEOUT"); do
    if curl -sf "http://localhost:$PORT/health" >/dev/null 2>&1; then
        ready=true
        break
    fi
    sleep 1
done

if [ "$ready" = "true" ]; then
    echo "  PASS  daemon started (${i}s)"
    ((pass++))
else
    echo "  FAIL  daemon did not start within ${TIMEOUT}s"
    ((fail++))
    echo ""
    echo "=== RESULT: $pass passed, $fail failed ==="
    exit 1
fi

echo ""

# ── Phase 3: API Checks ──────────────────────────────────────────────────────

echo "Phase 3: API"
check_output "health returns ok"         '"status":"ok"'       "http://localhost:$PORT/health"
check_output "health has identity"       '"identity":"CogOS"'  "http://localhost:$PORT/health"
check_output "context returns state"     '"state":"receptive"' "http://localhost:$PORT/v1/context"
check_output "context has nucleus"       '"nucleus":"CogOS"'   "http://localhost:$PORT/v1/context"

# Version endpoint.
VERSION_OUT=$($COGOS version 2>&1)
if echo "$VERSION_OUT" | grep -q "cogos.*build="; then
    echo "  PASS  version command works"
    pass=$((pass + 1))
else
    echo "  FAIL  version command works (got: $VERSION_OUT)"
    fail=$((fail + 1))
fi

echo ""

# ── Phase 4: Shutdown ─────────────────────────────────────────────────────────

echo "Phase 4: Shutdown"
kill "$DAEMON_PID" 2>/dev/null
wait "$DAEMON_PID" 2>/dev/null || true
DAEMON_PID=""

# Verify it's actually down.
sleep 1
if curl -sf "http://localhost:$PORT/health" >/dev/null 2>&1; then
    echo "  FAIL  daemon still running after kill"
    ((fail++))
else
    echo "  PASS  daemon stopped cleanly"
    ((pass++))
fi

echo ""
echo "=== RESULT: $pass passed, $fail failed ==="

if [ "$fail" -gt 0 ]; then
    exit 1
fi

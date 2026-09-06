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

# ── Phase 3b: MCP Transport Probe ────────────────────────────────────────────
# Exercises the wire protocol the daemon actually serves to agents:
# initialize → tools/list → one tools/call. Catches dead wiring (a tool
# registered in source but absent from the live daemon) and transport
# regressions that direct-method unit tests structurally cannot see.
#
# /mcp is gated on every method by the write-route grant-auth middleware
# (serve_grant_auth.go) — the daemon mints/recovers its own node-root
# identity grant at boot (ensureNodeRootGrant, boot_node_root_grant.go) as
# the zero-paste bootstrap credential for exactly this case. This probe does
# the REAL bootstrap a live consumer (Claude Code, THESEUS, the dashboard)
# would do: GET the node-root grant's current token (itself exempt from the
# gate — it's a GET) and attach it as X-Cogos-Grant on every /mcp call below.
# Mirrors internal/testkernel/testkernel.go's NodeRootGrantToken/ListTools —
# keep the two consistent if either changes.

echo "Phase 3b: MCP transport"
MCP_URL="http://localhost:$PORT/mcp"
MCP_H_CT='Content-Type: application/json'
MCP_H_ACC='Accept: application/json, text/event-stream'

GRANT_JSON=$(curl -s -m 10 "http://localhost:$PORT/v1/identity/grants/current?surface=node-root" || echo "")
GRANT_TOKEN=$(echo "$GRANT_JSON" | grep -o '"token":"[^"]*"' | head -1 | cut -d'"' -f4)
if [ -n "${GRANT_TOKEN:-}" ]; then
    echo "  PASS  node-root grant token acquired"
    pass=$((pass + 1))
else
    echo "  FAIL  node-root grant token acquired (got: $GRANT_JSON)"
    fail=$((fail + 1))
fi
GRANT_HEADER_ARG=""
if [ -n "${GRANT_TOKEN:-}" ]; then
    GRANT_HEADER_ARG="X-Cogos-Grant: ${GRANT_TOKEN}"
fi

MCP_SID=$(curl -s -D - -o /dev/null -m 10 -X POST "$MCP_URL" -H "$MCP_H_CT" -H "$MCP_H_ACC" ${GRANT_HEADER_ARG:+-H "$GRANT_HEADER_ARG"}     -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}'     | grep -i '^mcp-session-id:' | tr -d '\r' | awk '{print $2}')
if [ -n "${MCP_SID:-}" ]; then
    echo "  PASS  mcp initialize (session $MCP_SID)"
    pass=$((pass + 1))
else
    echo "  FAIL  mcp initialize (no session id)"
    fail=$((fail + 1))
fi

mcp_post() {
    curl -s -m 15 -X POST "$MCP_URL" -H "$MCP_H_CT" -H "$MCP_H_ACC" \
        ${MCP_SID:+-H "Mcp-Session-Id: ${MCP_SID}"} \
        ${GRANT_HEADER_ARG:+-H "$GRANT_HEADER_ARG"} -d "$1"
}
mcp_post '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null 2>&1 || true

TOOLS_OUT=$(mcp_post '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')
# Golden tool set: one per wired subsystem, so a dropped registration chain
# (providers_wire.go / z_*_wire.go) fails loudly here.
for tool in cog_get_state cog_search_memory cog_search_conversations cog_list_sessions cog_read_cogdoc; do
    if echo "$TOOLS_OUT" | grep -q "\"$tool\""; then
        echo "  PASS  tools/list has $tool"
        pass=$((pass + 1))
    else
        echo "  FAIL  tools/list has $tool"
        fail=$((fail + 1))
    fi
done

# Registration is not function. Before the fts5 sweep, every non-Makefile
# build shipped without the tag: cog_search_memory was REGISTERED and
# returned 0 rows for any multi-word query, because searchMemoryFTS failed
# with "no such module: fts5" and SearchMemory silently fell back to an
# unranked substring grep. tools/list could not see that. So call it.
SEARCH_OUT=$(mcp_post '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"cog_search_memory","arguments":{"query":"cog kernel memory","limit":5}}}')
if echo "$SEARCH_OUT" | grep -q '"error"'; then
    echo "  FAIL  cog_search_memory call returned an error: $(echo "$SEARCH_OUT" | head -c 200)"
    fail=$((fail + 1))
else
    echo "  PASS  cog_search_memory call succeeded"
    pass=$((pass + 1))
fi

# The fts5 verdict from the running binary itself. A stub/tagless build
# reports fts5=false here even though the daemon is otherwise healthy.
HEALTH_OUT=$(curl -s -m 10 "http://localhost:$PORT/health" 2>/dev/null || true)
if echo "$HEALTH_OUT" | grep -q '"build_tags"'; then
    if echo "$HEALTH_OUT" | grep -qE '"fts5"[[:space:]]*:[[:space:]]*true'; then
        echo "  PASS  /health build_tags.fts5 = true"
        pass=$((pass + 1))
    else
        echo "  FAIL  /health build_tags.fts5 is not true — this binary's search is degraded"
        fail=$((fail + 1))
    fi
else
    echo "  SKIP  /health has no build_tags (kernel predates ledger L01)"
fi

CALL_OUT=$(mcp_post '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"cog_get_state","arguments":{}}}')
if echo "$CALL_OUT" | grep -q '"result"' && ! echo "$CALL_OUT" | grep -q '"isError":true'; then
    echo "  PASS  tools/call cog_get_state"
    pass=$((pass + 1))
else
    echo "  FAIL  tools/call cog_get_state (got: $(echo "$CALL_OUT" | head -c 200))"
    fail=$((fail + 1))
fi

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

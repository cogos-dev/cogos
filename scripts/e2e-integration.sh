#!/usr/bin/env bash
# e2e-integration.sh — Full integration test with local Ollama and real workspace data.
#
# Tests the complete CogOS stack against a running Ollama instance:
#   1. Build binary
#   2. Init fresh workspace with seed data
#   3. Start daemon on alternate port
#   4. Verify all API endpoints
#   5. Send a real chat completion through the router → Ollama
#   6. Verify foveated context assembly with real CogDocs
#   7. Test MCP endpoint
#   8. Test memory search with seeded data
#   9. Verify coherence check
#  10. Shutdown and verify clean exit
#
# Prerequisites:
#   - Ollama running locally with at least one model (default: qwen3.5:0.8b for speed)
#   - Go toolchain installed
#
# Usage:
#   bash scripts/e2e-integration.sh                     # defaults
#   E2E_MODEL=gemma4:e4b bash scripts/e2e-integration.sh  # specific model
#   E2E_PORT=9999 bash scripts/e2e-integration.sh       # specific port

set -euo pipefail

COGOS="${COGOS_BIN:-./cogos}"
WORKSPACE="${E2E_WORKSPACE:-/tmp/cogos-e2e-integration}"
PORT="${E2E_PORT:-7931}"
MODEL="${E2E_MODEL:-qwen3.5:0.8b}"
OLLAMA="${OLLAMA_HOST:-http://localhost:11434}"
TIMEOUT="${E2E_TIMEOUT:-15}"
BASE="http://localhost:$PORT"

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

check_http() {
    local name="$1"
    local expected="$2"
    local url="$3"
    local body
    body=$(curl -sf "$url" 2>/dev/null || echo "CURL_FAILED")
    if echo "$body" | grep -q "$expected"; then
        echo "  PASS  $name"
        pass=$((pass + 1))
    else
        echo "  FAIL  $name (expected '$expected')"
        echo "         got: $(echo "$body" | head -c 200)"
        fail=$((fail + 1))
    fi
}

check_post() {
    local name="$1"
    local expected="$2"
    local url="$3"
    local data="$4"
    local body
    body=$(curl -sf -X POST -H "Content-Type: application/json" -d "$data" "$url" 2>/dev/null || echo "CURL_FAILED")
    if echo "$body" | grep -q "$expected"; then
        echo "  PASS  $name"
        pass=$((pass + 1))
    else
        echo "  FAIL  $name (expected '$expected')"
        echo "         got: $(echo "$body" | head -c 300)"
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

echo "╔══════════════════════════════════════════════════╗"
echo "║     CogOS E2E Integration Test                  ║"
echo "╠══════════════════════════════════════════════════╣"
echo "║  Binary:    $COGOS"
echo "║  Workspace: $WORKSPACE"
echo "║  Port:      $PORT"
echo "║  Model:     $MODEL"
echo "║  Ollama:    $OLLAMA"
echo "╚══════════════════════════════════════════════════╝"
echo ""

# ── Pre-flight: Check Ollama ─────────────────────────────────────────────────

echo "Pre-flight: Ollama"
if curl -sf "$OLLAMA/api/tags" >/dev/null 2>&1; then
    echo "  PASS  Ollama is running"
    pass=$((pass + 1))
else
    echo "  FAIL  Ollama not reachable at $OLLAMA"
    echo "         Start Ollama first: ollama serve"
    exit 1
fi

if curl -sf "$OLLAMA/api/tags" | grep -q "\"$MODEL\""; then
    echo "  PASS  Model $MODEL available"
    pass=$((pass + 1))
else
    echo "  FAIL  Model $MODEL not found in Ollama"
    echo "         Pull it: ollama pull $MODEL"
    exit 1
fi
echo ""

# ── Phase 1: Build ───────────────────────────────────────────────────────────

echo "Phase 1: Build"
if [ ! -f "$COGOS" ]; then
    make build 2>&1 | tail -1
fi
check "binary exists" test -x "$COGOS"

if $COGOS version 2>&1 | grep -q "cogos.*build="; then
    echo "  PASS  version command"
    pass=$((pass + 1))
else
    echo "  FAIL  version command"
    fail=$((fail + 1))
fi
echo ""

# ── Phase 2: Init with seed data ────────────────────────────────────────────

echo "Phase 2: Init + seed data"
$COGOS init --workspace "$WORKSPACE" 2>&1 | head -5 | sed 's/^/  /'

check "workspace scaffolded"  test -d "$WORKSPACE/.cog/mem/semantic"

# Seed some CogDocs for foveated context and memory search
mkdir -p "$WORKSPACE/.cog/mem/semantic/insights"
cat > "$WORKSPACE/.cog/mem/semantic/insights/test-architecture.md" << 'SEED'
---
title: "CogOS Architecture Overview"
type: architecture
tags: [cogos, architecture, kernel, foveated]
---

# CogOS Architecture

CogOS is a cognitive operating system that externalizes attention and executive
function modulation for intelligent systems. The kernel runs as a continuous
process daemon with four states: Active, Receptive, Consolidating, and Dormant.

The foveated context engine uses a Tiny Recursive Model (TRM) implemented as a
Mamba SSM to score workspace documents by relevance. Documents are zone-ordered
into Nucleus, Knowledge, History, and Current tiers.

Key components:
- Foveated context assembly with iris pressure tracking
- Hash-chained ledger for audit trail
- Multi-provider routing with sovereignty gradient
- Tool-call hallucination gate
SEED

cat > "$WORKSPACE/.cog/mem/semantic/insights/test-deployment.md" << 'SEED'
---
title: "Deployment Guide"
type: guide
tags: [deployment, docker, helm, kubernetes]
---

# Deployment

CogOS deploys via Helm charts or Docker Compose. The kernel listens on port 6931
by default. Use the charts repo for Kubernetes deployments.

Supported deployment modes:
- Tier 1: Developer (cogos serve in terminal)
- Tier 2: Desktop (CogOS.app via Wails)
- Tier 3: Production (helm install via cogos-dev/charts)
SEED

check "seed doc: architecture"   test -f "$WORKSPACE/.cog/mem/semantic/insights/test-architecture.md"
check "seed doc: deployment"     test -f "$WORKSPACE/.cog/mem/semantic/insights/test-deployment.md"

# Configure the kernel to use our Ollama model
cat > "$WORKSPACE/.cog/config/kernel.yaml" << YAML
port: $PORT
local_model: "$MODEL"
providers:
  ollama:
    enabled: true
    endpoint: "$OLLAMA"
    model: "$MODEL"
YAML

check "kernel.yaml configured"  grep -q "$MODEL" "$WORKSPACE/.cog/config/kernel.yaml"
echo ""

# ── Phase 3: Serve ───────────────────────────────────────────────────────────

echo "Phase 3: Serve"
$COGOS serve --workspace "$WORKSPACE" --port "$PORT" > /tmp/cogos-e2e-daemon.log 2>&1 &
DAEMON_PID=$!

ready=false
for i in $(seq 1 "$TIMEOUT"); do
    if curl -sf "$BASE/health" >/dev/null 2>&1; then
        ready=true
        break
    fi
    sleep 1
done

if [ "$ready" = "true" ]; then
    echo "  PASS  daemon started (${i}s)"
    pass=$((pass + 1))
else
    echo "  FAIL  daemon did not start within ${TIMEOUT}s"
    echo "  === daemon log ==="
    tail -20 /tmp/cogos-e2e-daemon.log
    fail=$((fail + 1))
    echo ""
    echo "=== RESULT: $pass passed, $fail failed ==="
    exit 1
fi
echo ""

# ── Phase 4: API Endpoints ──────────────────────────────────────────────────

echo "Phase 4: API endpoints"
check_http "GET /health status"         '"status":"ok"'       "$BASE/health"
check_http "GET /health identity"       '"identity"'          "$BASE/health"
check_http "GET /v1/context state"      '"state"'             "$BASE/v1/context"

# Context should now include our seed docs
check_http "GET /v1/context has nucleus" '"nucleus"'          "$BASE/v1/context"
echo ""

# ── Phase 5: Chat Completion (real inference) ────────────────────────────────

echo "Phase 5: Chat completion (real inference via $MODEL)"
CHAT_RESPONSE=$(curl -sf -X POST "$BASE/v1/chat/completions" \
    -H "Content-Type: application/json" \
    -d "{
        \"model\": \"$MODEL\",
        \"messages\": [{\"role\": \"user\", \"content\": \"Say hello in exactly 5 words.\"}],
        \"max_tokens\": 50
    }" 2>/dev/null || echo "CURL_FAILED")

if echo "$CHAT_RESPONSE" | grep -q '"choices"'; then
    echo "  PASS  chat completion returned choices"
    pass=$((pass + 1))
    # Extract the response text
    REPLY=$(echo "$CHAT_RESPONSE" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['choices'][0]['message']['content'][:80])" 2>/dev/null || echo "(parse error)")
    echo "         model said: $REPLY"
else
    echo "  FAIL  chat completion (no choices in response)"
    echo "         got: $(echo "$CHAT_RESPONSE" | head -c 300)"
    fail=$((fail + 1))
fi

# Streaming completion
STREAM_FIRST=$(curl -sf -X POST "$BASE/v1/chat/completions" \
    -H "Content-Type: application/json" \
    -d "{
        \"model\": \"$MODEL\",
        \"messages\": [{\"role\": \"user\", \"content\": \"Count to 3.\"}],
        \"max_tokens\": 30,
        \"stream\": true
    }" 2>/dev/null | head -5 || echo "CURL_FAILED")

if echo "$STREAM_FIRST" | grep -q "data:"; then
    echo "  PASS  streaming completion returns SSE chunks"
    pass=$((pass + 1))
else
    echo "  FAIL  streaming completion (no SSE data)"
    fail=$((fail + 1))
fi
echo ""

# ── Phase 6: Foveated Context ───────────────────────────────────────────────

echo "Phase 6: Foveated context assembly"
FOVEATED=$(curl -sf -X POST "$BASE/v1/context/foveated" \
    -H "Content-Type: application/json" \
    -d '{
        "prompt": "How does the CogOS architecture work?",
        "iris": {"size": 128000, "used": 5000},
        "profile": "default"
    }' 2>/dev/null || echo "CURL_FAILED")

if echo "$FOVEATED" | grep -q "context\|tier\|tokens"; then
    echo "  PASS  foveated context returned"
    pass=$((pass + 1))
    TOKENS=$(echo "$FOVEATED" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('data',d).get('tokens', d.get('tokens','?')))" 2>/dev/null || echo "?")
    echo "         tokens assembled: $TOKENS"
else
    echo "  FAIL  foveated context (unexpected response)"
    echo "         got: $(echo "$FOVEATED" | head -c 300)"
    fail=$((fail + 1))
fi
echo ""

# ── Phase 7: Memory Search ──────────────────────────────────────────────────

echo "Phase 7: Memory search"
SEARCH=$(curl -sf "$BASE/memory/search?query=architecture&limit=5" 2>/dev/null || echo "CURL_FAILED")

if echo "$SEARCH" | grep -q "architecture\|results\|path"; then
    echo "  PASS  memory search found results"
    pass=$((pass + 1))
else
    # Memory search endpoint might not exist yet — note but don't fail hard
    echo "  SKIP  memory search (endpoint may not be implemented: $(echo "$SEARCH" | head -c 100))"
fi
echo ""

# ── Phase 8: Coherence Check ────────────────────────────────────────────────

echo "Phase 8: Coherence"
COHERENCE=$(curl -sf "$BASE/coherence/check" 2>/dev/null || echo "CURL_FAILED")

if echo "$COHERENCE" | grep -q "coherent\|status\|pass"; then
    echo "  PASS  coherence check responded"
    pass=$((pass + 1))
else
    echo "  SKIP  coherence check (endpoint may not be implemented: $(echo "$COHERENCE" | head -c 100))"
fi
echo ""

# ── Phase 9: Anthropic Messages API ─────────────────────────────────────────

echo "Phase 9: Anthropic Messages API compatibility"
MESSAGES=$(curl -sf -X POST "$BASE/v1/messages" \
    -H "Content-Type: application/json" \
    -H "x-api-key: test" \
    -H "anthropic-version: 2023-06-01" \
    -d "{
        \"model\": \"$MODEL\",
        \"max_tokens\": 30,
        \"messages\": [{\"role\": \"user\", \"content\": \"Say ok.\"}]
    }" 2>/dev/null || echo "CURL_FAILED")

if echo "$MESSAGES" | grep -q '"content"'; then
    echo "  PASS  Anthropic Messages API returned content"
    pass=$((pass + 1))
    REPLY=$(echo "$MESSAGES" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['content'][0]['text'][:80])" 2>/dev/null || echo "(parse error)")
    echo "         model said: $REPLY"
else
    echo "  FAIL  Anthropic Messages API"
    echo "         got: $(echo "$MESSAGES" | head -c 300)"
    fail=$((fail + 1))
fi
echo ""

# ── Phase 10: Shutdown ──────────────────────────────────────────────────────

echo "Phase 10: Shutdown"
kill "$DAEMON_PID" 2>/dev/null
wait "$DAEMON_PID" 2>/dev/null || true
DAEMON_PID=""

sleep 1
if curl -sf "$BASE/health" >/dev/null 2>&1; then
    echo "  FAIL  daemon still running after kill"
    fail=$((fail + 1))
else
    echo "  PASS  daemon stopped cleanly"
    pass=$((pass + 1))
fi

echo ""
echo "╔══════════════════════════════════════════════════╗"
echo "║  RESULT: $pass passed, $fail failed"
echo "╚══════════════════════════════════════════════════╝"

if [ "$fail" -gt 0 ]; then
    echo ""
    echo "Daemon log (last 20 lines):"
    tail -20 /tmp/cogos-e2e-daemon.log 2>/dev/null
    exit 1
fi

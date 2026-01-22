#!/bin/sh
# .cog/lib/observer.sh - Perspectival Observer System
#
# Lightweight observers that validate coherence from different perspectives.
# Each observer has its own world model and continuously checks whether
# the current state is coherent relative to that perspective.
#
# Usage:
#   cog_source observer
#   cog_observer researcher          # Watch from researcher's perspective
#   cog_observe_all                  # Check all role perspectives at once
#   cog_observer_status              # Show active observers
#

# =============================================================================
# OBSERVER REGISTRY
# =============================================================================

COG_OBSERVER_DIR="${COG_DIR}/observers"

# Initialize observer infrastructure
_observer_init() {
  mkdir -p "$COG_OBSERVER_DIR"
  mkdir -p "${COG_SIGNALS}/observer"
}

# =============================================================================
# PERSPECTIVE COMPUTATION
# =============================================================================

# Compute a perspective's expected view hash
# Perspective can be: role name, file pattern, or "full" for entire .cog/
cog_perspective_hash() {
  local perspective="${1:?Perspective required}"

  case "$perspective" in
    full|all|"*")
      # Full workspace view
      cog_state_hash
      ;;
    pattern:*)
      # File pattern view
      local pattern="${perspective#pattern:}"
      find "$COG_DIR" -path "$pattern" -type f 2>/dev/null | \
        xargs sha256sum 2>/dev/null | sort | sha256sum | cut -d' ' -f1
      ;;
    *)
      # Assume it's a role name - load roles library
      cog_source roles 2>/dev/null || { echo "roles-unavailable"; return 1; }
      cog_view_hash "$perspective" 2>/dev/null || echo "no-view-defined"
      ;;
  esac
}

# Get the files visible from a perspective
cog_perspective_files() {
  local perspective="${1:?Perspective required}"

  case "$perspective" in
    full|all|"*")
      find "$COG_DIR" -type f 2>/dev/null
      ;;
    pattern:*)
      local pattern="${perspective#pattern:}"
      find "$COG_DIR" -path "$pattern" -type f 2>/dev/null
      ;;
    *)
      cog_source roles 2>/dev/null || return 1
      cog_role_view "$perspective" 2>/dev/null
      ;;
  esac
}

# =============================================================================
# COHERENCE CHECKING
# =============================================================================

# Check coherence from a specific perspective
# Returns 0 if coherent, 1 if diverged
cog_check_coherence() {
  local perspective="${1:?Perspective required}"
  local expected_hash="${2:-}"

  # If no expected hash provided, use last known good
  if [ -z "$expected_hash" ]; then
    expected_hash=$(cog_signal_read "observer/${perspective}/baseline" 2>/dev/null)
  fi

  # Compute current state from this perspective
  local current_hash=$(cog_perspective_hash "$perspective")

  if [ -z "$expected_hash" ]; then
    # No baseline - this is first check, establish baseline
    cog_signal "observer/${perspective}/baseline" "$current_hash"
    echo "BASELINE: $perspective ($(_cog_trunc "$current_hash" 12)...)"
    return 0
  fi

  if [ "$current_hash" = "$expected_hash" ]; then
    echo "COHERENT: $perspective"
    return 0
  else
    echo "DIVERGED: $perspective (expected: $(_cog_trunc "$expected_hash" 8)... got: $(_cog_trunc "$current_hash" 8)...)"
    return 1
  fi
}

# Multi-perspective coherence check
# Checks all provided perspectives and reports divergences
cog_check_perspectives() {
  local perspectives="${@:-$(cog_roles 2>/dev/null)}"
  local all_coherent=true
  local results=""

  echo "=== Multi-Perspective Coherence Check ==="
  echo ""

  for perspective in $perspectives; do
    local result=$(cog_check_coherence "$perspective" 2>&1)
    local status=$?

    if [ $status -ne 0 ]; then
      all_coherent=false
      echo "✗ $result"
    else
      echo "✓ $result"
    fi
  done

  echo ""
  if [ "$all_coherent" = "true" ]; then
    echo "=== All perspectives coherent ==="
    return 0
  else
    echo "=== Coherence violations detected ==="
    return 1
  fi
}

# =============================================================================
# OBSERVER PROCESS
# =============================================================================

# Start a background observer for a specific perspective
# Usage: cog_observer <perspective> [interval_seconds] [on_divergence]
cog_observer() {
  local perspective="${1:?Perspective required}"
  local interval="${2:-5}"
  local on_divergence="${3:-signal}"  # signal, warn, halt, hook:<command>

  local observer_id="${perspective//\//-}-$$"
  local pid_file="${COG_OBSERVER_DIR}/${observer_id}.pid"

  # Check if already running
  if [ -f "$pid_file" ]; then
    local old_pid=$(cat "$pid_file")
    if kill -0 "$old_pid" 2>/dev/null; then
      echo "Observer already running for $perspective (pid: $old_pid)"
      return 1
    fi
  fi

  # Establish baseline
  local baseline=$(cog_perspective_hash "$perspective")
  cog_signal "observer/${perspective}/baseline" "$baseline"
  cog_signal "observer/${perspective}/status" "running"
  cog_signal "observer/${perspective}/started" "$(date -Iseconds)"

  # Start background observer
  (
    trap "rm -f '$pid_file'; cog_signal 'observer/${perspective}/status' 'stopped'" EXIT

    while true; do
      local current=$(cog_perspective_hash "$perspective")
      local expected=$(cog_signal_read "observer/${perspective}/baseline" 2>/dev/null)

      if [ -n "$expected" ] && [ "$current" != "$expected" ]; then
        # Divergence detected!
        local timestamp=$(date -Iseconds)
        cog_signal "observer/${perspective}/divergence" "${current}@${timestamp}"
        cog_signal "observer/${perspective}/last_check" "DIVERGED:${timestamp}"

        case "$on_divergence" in
          signal)
            # Already signaled above
            ;;
          warn)
            echo "OBSERVER[$perspective]: Divergence detected at $timestamp" >&2
            ;;
          halt)
            echo "HALT: Observer $perspective detected divergence" >&2
            cog_signal "observer/${perspective}/status" "halted"
            exit 1
            ;;
          hook:*)
            local hook_cmd="${on_divergence#hook:}"
            eval "$hook_cmd" "$perspective" "$expected" "$current"
            ;;
        esac
      else
        cog_signal "observer/${perspective}/last_check" "OK:$(date -Iseconds)"
      fi

      sleep "$interval"
    done
  ) &

  local pid=$!
  echo "$pid" > "$pid_file"

  echo "Observer started: $perspective"
  echo "  PID: $pid"
  echo "  Interval: ${interval}s"
  echo "  Baseline: $(_cog_trunc "$baseline" 12)..."
  echo "  On divergence: $on_divergence"
}

# Stop an observer
cog_observer_stop() {
  local perspective="${1:?Perspective required}"
  local observer_id="${perspective//\//-}"

  # Find any PID files matching this perspective
  for pid_file in "${COG_OBSERVER_DIR}/${observer_id}"*.pid; do
    [ -f "$pid_file" ] || continue

    local pid=$(cat "$pid_file")
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null
      echo "Stopped observer: $perspective (pid: $pid)"
    fi
    rm -f "$pid_file"
  done

  cog_signal "observer/${perspective}/status" "stopped"
}

# Stop all observers
cog_observer_stop_all() {
  for pid_file in "${COG_OBSERVER_DIR}"/*.pid; do
    [ -f "$pid_file" ] || continue

    local pid=$(cat "$pid_file")
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null
    fi
    rm -f "$pid_file"
  done

  echo "All observers stopped"
}

# =============================================================================
# OBSERVER STATUS
# =============================================================================

# Show status of all observers
cog_observer_status() {
  echo "=== Observer Status ==="
  echo ""

  local has_observers=false

  for pid_file in "${COG_OBSERVER_DIR}"/*.pid; do
    [ -f "$pid_file" ] || continue
    has_observers=true

    local observer_id=$(basename "$pid_file" .pid)
    local pid=$(cat "$pid_file")
    local perspective="${observer_id%-*}"
    perspective="${perspective//-//}"

    local status="dead"
    if kill -0 "$pid" 2>/dev/null; then
      status="running"
    fi

    local last_check=$(cog_signal_read "observer/${perspective}/last_check" 2>/dev/null || echo "unknown")
    local started=$(cog_signal_read "observer/${perspective}/started" 2>/dev/null || echo "unknown")

    echo "Observer: $perspective"
    echo "  PID: $pid ($status)"
    echo "  Started: $started"
    echo "  Last check: $last_check"
    echo ""
  done

  if [ "$has_observers" = "false" ]; then
    echo "No active observers"
  fi
}

# List all divergences detected by observers
cog_observer_divergences() {
  echo "=== Observed Divergences ==="
  echo ""

  local divergence_dir="${COG_SIGNALS}/observer"
  [ -d "$divergence_dir" ] || { echo "No divergences recorded"; return 0; }

  for f in "$divergence_dir"/*/divergence; do
    [ -f "$f" ] || continue

    local perspective=$(dirname "$f")
    perspective=$(basename "$perspective")
    local divergence=$(cat "$f")

    echo "$perspective: $divergence"
  done
}

# =============================================================================
# ONE-SHOT OBSERVATION
# =============================================================================

# Run a single coherence check from an agent's perspective
# Compare agent's internal state to what it "should" see
cog_observe_agent() {
  local agent_id="${1:?Agent ID required}"

  # Get agent's role
  local status_file="${COG_DIR}/status/${agent_id}.json"
  [ -f "$status_file" ] || { echo "Agent not found: $agent_id" >&2; return 1; }

  local role=$(grep -o '"role": *"[^"]*"' "$status_file" | cut -d'"' -f4)
  [ -n "$role" ] || { echo "No role for agent: $agent_id" >&2; return 1; }

  # Get agent's announced state
  local agent_state=$(cog_signal_read "state/${agent_id}" 2>/dev/null)
  [ -n "$agent_state" ] || { echo "No state announced by: $agent_id" >&2; return 1; }

  # Get what agent should see (from role perspective)
  local expected_view=$(cog_perspective_hash "$role")

  # Get agent's claimed world (if using world projection)
  local agent_world=$(cog_signal_read "world/${agent_id}" 2>/dev/null)

  echo "=== Agent Observation: $agent_id ==="
  echo ""
  echo "Role: $role"
  echo "Announced state: $(_cog_trunc "$agent_state" 12)..."
  echo "Expected view:   $(_cog_trunc "$expected_view" 12)..."
  [ -n "$agent_world" ] && echo "Claimed world:   $(_cog_trunc "$agent_world" 12)..."
  echo ""

  # Check coherence
  if [ -n "$agent_world" ] && [ "$agent_world" != "$expected_view" ]; then
    echo "⚠ World projection mismatch!"
    echo "  Agent claims: $(_cog_trunc "$agent_world" 12)..."
    echo "  Role defines: $(_cog_trunc "$expected_view" 12)..."
    return 1
  fi

  echo "✓ Agent view is coherent with role perspective"
  return 0
}

# Observe all active agents
cog_observe_all_agents() {
  echo "=== Observing All Agents ==="
  echo ""

  local agents=$(cog_spawned)
  [ -n "$agents" ] || { echo "No active agents"; return 0; }

  local all_coherent=true

  for agent in $agents; do
    if cog_observe_agent "$agent" >/dev/null 2>&1; then
      echo "✓ $agent"
    else
      echo "✗ $agent (diverged)"
      all_coherent=false
    fi
  done

  echo ""
  if [ "$all_coherent" = "true" ]; then
    echo "All agents coherent"
    return 0
  else
    echo "Coherence violations detected"
    return 1
  fi
}

# =============================================================================
# BASELINE MANAGEMENT
# =============================================================================

# Update baseline for a perspective (acknowledge current state as expected)
cog_observer_accept() {
  local perspective="${1:?Perspective required}"
  local current=$(cog_perspective_hash "$perspective")

  cog_signal "observer/${perspective}/baseline" "$current"
  cog_signal_clear "observer/${perspective}/divergence" 2>/dev/null

  echo "Baseline updated: $perspective ($(_cog_trunc "$current" 12)...)"
}

# Reset all baselines
cog_observer_reset() {
  rm -rf "${COG_SIGNALS}/observer"
  mkdir -p "${COG_SIGNALS}/observer"
  echo "All observer baselines reset"
}

# =============================================================================
# INITIALIZATION
# =============================================================================

_observer_init

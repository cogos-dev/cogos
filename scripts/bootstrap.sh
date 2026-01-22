#!/bin/sh
# .cog/lib/bootstrap.sh - Agent bootstrap sequence
#
# Agents source this to initialize themselves properly.
# Sets up role, view constraints, and announces state.
#
# Usage (in agent worktree):
#   export COG_AGENT_ID="researcher-12345-abc"
#   export COG_AGENT_ROLE="researcher"
#   . .cog/lib/bootstrap.sh

# Ensure we're in a valid context
[ -z "$COG_AGENT_ID" ] && { echo "COG_AGENT_ID not set" >&2; exit 1; }
[ -z "$COG_AGENT_ROLE" ] && { echo "COG_AGENT_ROLE not set" >&2; exit 1; }

# Source kernel if not already loaded
[ -z "$COG_LOADED" ] && . .cog/cog

# Load role
cog_source roles
cog_role_load "$COG_AGENT_ROLE" || { echo "Failed to load role: $COG_AGENT_ROLE" >&2; exit 1; }

# Compute and store view hash
COG_VIEW_HASH=$(cog_view_hash "$COG_AGENT_ROLE")
export COG_VIEW_HASH

# Announce initial state
cog_announce_state >/dev/null

# Announce view (for verification)
cog_signal "view/${COG_AGENT_ID}" "$COG_VIEW_HASH"

# Log bootstrap complete
echo "=== Agent Bootstrap Complete ==="
echo "Agent:      $COG_AGENT_ID"
echo "Role:       $COG_AGENT_ROLE"
echo "Layer:      $COG_AGENT_LAYER"
echo "State:      $(cog_state_hash | head -c 12)..."
echo "View:       $(_cog_trunc "$COG_VIEW_HASH" 12)..."
echo "Caps:       $COG_ROLE_CAPABILITIES"
echo "Can spawn:  $COG_ROLE_SPAWNS"
echo "================================"

# Define cleanup trap
_agent_cleanup() {
  echo "Agent $COG_AGENT_ID shutting down..."
  cog_announce_state >/dev/null  # Final state
  cog_signal "status/${COG_AGENT_ID}" "terminated"
}
trap _agent_cleanup EXIT

# Agent is ready
export COG_AGENT_READY=1

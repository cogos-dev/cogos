#!/bin/sh
# Test agent lifecycle
. .cog/cog

echo "=== Testing Agent Lifecycle ==="

# Spawn
echo "1. Spawning researcher..."
AGENT_ID=$(cog_spawn researcher "test task")
echo "   Created: $AGENT_ID"

# Check state was announced
echo "2. Checking state announcement..."
STATE=$(cog_signal_read "state/${AGENT_ID}" 2>/dev/null)
if [ -n "$STATE" ]; then
  echo "   State: $(_cog_trunc "$STATE" 12)..."
else
  echo "   WARNING: No state announced"
fi

# Verify
echo "3. Running verification..."
cog_verify

# Cleanup
echo "4. Reaping agent..."
cog_reap "$AGENT_ID"

echo "=== Test Complete ==="

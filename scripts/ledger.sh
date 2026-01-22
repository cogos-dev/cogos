#!/bin/sh
# .cog/lib/ledger.sh - Ledger crystallization interface
#
# The Demon's Ledger crystallization layer.
# Wraps event_lib.py for ledger operations.
#
# Usage:
#   source .cog/lib/ledger.sh
#   cog_ledger_crystallize <session-id>
#   cog_ledger_show <session-id>
#   cog_ledger_list

COG_EVENT_LIB="${COG_DIR}/scripts/event_lib.py"
COG_LEDGER_DIR="${COG_DIR}/ledger"

# =============================================================================
# LEDGER OPERATIONS
# =============================================================================

# Crystallize a session's events into a Merkle tree
# Usage: cog_ledger_crystallize [session-id]
cog_ledger_crystallize() {
    local session_id="${1:-}"
    python3 "$COG_EVENT_LIB" crystallize "$session_id"
}

# Verify a crystallized session
# Usage: cog_ledger_verify <session-id>
cog_ledger_verify() {
    local session_id="${1:?Session ID required}"
    python3 "$COG_EVENT_LIB" verify "$session_id"
}

# Show a session's crystal (Merkle root + metadata)
# Usage: cog_ledger_show [session-id]
cog_ledger_show() {
    local session_id="${1:-}"

    # If no session, find most recent crystal
    if [ -z "$session_id" ]; then
        # Find most recent CRYSTAL file
        local crystal=$(ls -t "$COG_LEDGER_DIR"/*/CRYSTAL 2>/dev/null | head -1)
        if [ -n "$crystal" ]; then
            cat "$crystal"
        else
            echo "No crystals found"
            return 1
        fi
    else
        cat "$COG_LEDGER_DIR/$session_id/CRYSTAL" 2>/dev/null || {
            echo "Crystal not found for session: $session_id"
            return 1
        }
    fi
}

# List all crystallized sessions
# Usage: cog_ledger_list
cog_ledger_list() {
    echo "Session Crystals:"
    echo ""

    if [ ! -d "$COG_LEDGER_DIR" ]; then
        echo "  (ledger directory not found)"
        return 0
    fi

    local found=0
    for dir in "$COG_LEDGER_DIR"/*/; do
        [ -d "$dir" ] || continue
        local session=$(basename "$dir")
        local crystal="$dir/CRYSTAL"

        if [ -f "$crystal" ]; then
            # Extract root hash from crystal JSON
            local root=$(python3 -c "import json; print(json.load(open('$crystal')).get('root', '?')[:16])" 2>/dev/null || echo "?")
            local event_count=$(python3 -c "import json; print(json.load(open('$crystal')).get('event_count', '?'))" 2>/dev/null || echo "?")
            echo "  $session"
            echo "    Root:   $root..."
            echo "    Events: $event_count"
            echo ""
            found=1
        fi
    done

    if [ $found -eq 0 ]; then
        echo "  (no crystals found)"
    fi
}

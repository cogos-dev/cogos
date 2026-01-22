#!/bin/bash
# .cog/lib/sync.sh
#
# DEPRECATED: This file was part of the two-branch model (ADR-006)
# which was superseded by ADR-021 (Holographic Workspace).
# The holographic model uses tree hashes instead of branch sync.
# This file is kept for backward compatibility but should not be used.
#
# Projection sync mechanism for .cog/ <-> cog branch (OBSOLETE)

# Note: Don't use 'set -euo pipefail' here as it affects the parent shell
# The kernel manages its own error handling

# Get the hash of current .cog/ state
cog_state_hash() {
    local cog_root="${COG_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null)/.cog}"
    local repo_root=$(git rev-parse --show-toplevel)

    # Use git to hash the .cog directory tree
    git -C "$repo_root" write-tree --prefix=.cog/ 2>/dev/null || echo "none"
}

# Sync .cog/ projection to cog branch (commit changes)
cog_sync_to_branch() {
    local message="${1:-"Sync cognitive state"}"
    local repo_root=$(git rev-parse --show-toplevel)

    # Check for changes in .cog/
    if git -C "$repo_root" diff --quiet .cog/ && git -C "$repo_root" diff --cached --quiet .cog/; then
        echo "No changes to sync"
        return 0
    fi

    # Compute current state hash
    local cog_hash=$(cog_state_hash)

    # Stage .cog/ changes
    git -C "$repo_root" add .cog/

    # Commit with hash in message
    git -C "$repo_root" commit -m "$message" -m "State hash: $cog_hash" --only .cog/ 2>/dev/null || {
        echo "Nothing to commit"
        return 0
    }

    echo "Synced: $cog_hash"
}

# Validate coherence of .cog/ state
cog_validate_coherence() {
    local cog_root="${COG_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null)/.cog}"
    local errors=()

    # 1. Check status file schemas
    if [ -d "$cog_root/status" ]; then
        for f in "$cog_root/status"/*.json; do
            [ -f "$f" ] || continue
            # Verify required fields exist
            if ! jq -e '.agentId and .status' "$f" >/dev/null 2>&1; then
                errors+=("Invalid status schema: $f (missing agentId or status)")
            fi
        done
    fi

    # 2. Check for orphaned agent directories
    if [ -d "$cog_root/agents" ]; then
        for agent_dir in "$cog_root/agents"/*/; do
            [ -d "$agent_dir" ] || continue
            local agent_id=$(basename "$agent_dir")
            # Skip hidden directories
            [[ "$agent_id" == .* ]] && continue

            if [ ! -f "$cog_root/status/${agent_id}.json" ]; then
                errors+=("Orphaned agent directory: $agent_dir (no status file)")
            fi
        done
    fi

    # 3. Check for broken symlinks
    while IFS= read -r -d '' link; do
        if [ ! -e "$link" ]; then
            errors+=("Broken symlink: $link")
        fi
    done < <(find "$cog_root" -type l -print0 2>/dev/null)

    # 4. Verify memory sector structure
    for sector in semantic episodic procedural reflective; do
        if [ ! -d "$cog_root/mem/$sector" ]; then
            # Not an error, just create if missing
            mkdir -p "$cog_root/mem/$sector"
        fi
    done

    # 5. Check for zombie agents (status=running but no process)
    if [ -d "$cog_root/status" ]; then
        for f in "$cog_root/status"/*.json; do
            [ -f "$f" ] || continue
            local status=$(jq -r '.status // empty' "$f" 2>/dev/null)
            local pid=$(jq -r '.pid // empty' "$f" 2>/dev/null)

            if [ "$status" = "running" ] && [ -n "$pid" ] && [ "$pid" != "null" ]; then
                if ! kill -0 "$pid" 2>/dev/null; then
                    errors+=("Zombie agent: $(basename "$f" .json) (pid $pid not running)")
                fi
            fi
        done
    fi

    # Report errors
    if [ ${#errors[@]} -gt 0 ]; then
        echo "Coherence validation failed:" >&2
        printf '  - %s\n' "${errors[@]}" >&2
        return 1
    fi

    echo "Coherence validated: $(cog_state_hash)"
    return 0
}

# Check if .cog/ has uncommitted changes
cog_has_changes() {
    local repo_root=$(git rev-parse --show-toplevel)
    ! git -C "$repo_root" diff --quiet .cog/ || ! git -C "$repo_root" diff --cached --quiet .cog/
}

# Get list of changed files in .cog/
cog_changed_files() {
    local repo_root=$(git rev-parse --show-toplevel)
    git -C "$repo_root" diff --name-only .cog/
    git -C "$repo_root" diff --cached --name-only .cog/
}

# Compute diff between two state hashes
cog_diff_states() {
    local from_hash="${1:?From hash required}"
    local to_hash="${2:-HEAD}"

    git diff "$from_hash" "$to_hash" -- .cog/
}

# Restore .cog/ to a previous state
cog_restore_state() {
    local target_hash="${1:?Target hash or ref required}"
    local repo_root=$(git rev-parse --show-toplevel)

    echo "Restoring .cog/ to $target_hash..."
    git -C "$repo_root" checkout "$target_hash" -- .cog/
    echo "Restored to: $(cog_state_hash)"
}

#!/bin/bash
# .cog/lib/delta.sh
# Delta computation utilities for agent state validation

# Compute list of files changed since a given hash
# Usage: cog_delta_files "spawn_hash"
# Returns: List of changed file paths under .cog/
cog_delta_files() {
    local from_hash="${1:?Spawn hash required}"
    local repo_root=$(git rev-parse --show-toplevel)

    # Get list of files changed since the spawn hash
    # Use git diff-tree to compare trees
    git -C "$repo_root" diff --name-only "$from_hash" HEAD -- .cog/ 2>/dev/null || {
        # If the hash is a tree hash, not a commit, use different approach
        # Compare current .cog/ tree with stored tree hash
        local current_tree=$(git -C "$repo_root" write-tree --prefix=.cog/)
        if [ "$current_tree" = "$from_hash" ]; then
            # No changes
            return 0
        fi
        # List all files in .cog/ as "changed" since we can't do proper diff
        find "$repo_root/.cog" -type f -name "*.json" -o -name "*.md" | \
            sed "s|$repo_root/||"
    }
}

# Check if a path matches any allowed pattern
# Usage: cog_path_allowed "path" "pattern1" "pattern2" ...
# Returns: 0 if allowed, 1 if not
cog_path_allowed() {
    local path="${1:?Path required}"
    shift
    local patterns=("$@")

    for pattern in "${patterns[@]}"; do
        # Convert glob pattern to regex
        # Replace * with .*, escape dots
        local regex="${pattern//./\\.}"
        regex="${regex//\*/.*}"
        regex="^${regex}"

        if [[ "$path" =~ $regex ]]; then
            return 0
        fi
    done

    return 1
}

# Validate that all changes are within allowed paths
# Usage: cog_validate_delta "spawn_hash" "allowed_path1" "allowed_path2" ...
# Returns: 0 if valid, 1 if violations found (prints violations to stderr)
cog_validate_delta() {
    local spawn_hash="${1:?Spawn hash required}"
    shift
    local allowed_paths=("$@")

    local violations=()
    local changed_files=$(cog_delta_files "$spawn_hash")

    while IFS= read -r file; do
        [ -z "$file" ] && continue

        if ! cog_path_allowed "$file" "${allowed_paths[@]}"; then
            violations+=("$file")
        fi
    done <<< "$changed_files"

    if [ ${#violations[@]} -gt 0 ]; then
        echo "Delta validation failed. Unauthorized modifications:" >&2
        printf '  - %s\n' "${violations[@]}" >&2
        return 1
    fi

    return 0
}

# Compute a summary of changes (for logging/reporting)
# Usage: cog_delta_summary "spawn_hash"
cog_delta_summary() {
    local spawn_hash="${1:?Spawn hash required}"
    local repo_root=$(git rev-parse --show-toplevel)

    local added=0 modified=0 deleted=0

    while IFS= read -r line; do
        [ -z "$line" ] && continue
        local status="${line:0:1}"
        case "$status" in
            A) ((added++)) ;;
            M) ((modified++)) ;;
            D) ((deleted++)) ;;
        esac
    done < <(git -C "$repo_root" diff --name-status "$spawn_hash" HEAD -- .cog/ 2>/dev/null)

    echo "added=$added modified=$modified deleted=$deleted"
}

# Get the delta as a structured object (for JSON output)
# Usage: cog_delta_json "spawn_hash"
cog_delta_json() {
    local spawn_hash="${1:?Spawn hash required}"
    local repo_root=$(git rev-parse --show-toplevel)

    local files=()
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        local status="${line:0:1}"
        local file="${line:2}"
        files+=("$(printf '{"status":"%s","path":"%s"}' "$status" "$file")")
    done < <(git -C "$repo_root" diff --name-status "$spawn_hash" HEAD -- .cog/ 2>/dev/null)

    local current_hash=$(git -C "$repo_root" write-tree --prefix=.cog/ 2>/dev/null || echo "none")

    printf '{"fromHash":"%s","toHash":"%s","files":[%s]}' \
        "$spawn_hash" \
        "$current_hash" \
        "$(IFS=,; echo "${files[*]}")"
}

# Record a delta snapshot (for later verification)
# Usage: cog_delta_snapshot "agent_id"
cog_delta_snapshot() {
    local agent_id="${1:?Agent ID required}"
    local cog_root="${COG_ROOT:-$(git rev-parse --show-toplevel)/.cog}"
    local status_file="$cog_root/status/${agent_id}.json"

    if [ ! -f "$status_file" ]; then
        echo "Agent status file not found: $agent_id" >&2
        return 1
    fi

    local spawn_hash=$(jq -r '.spawnHash // empty' "$status_file")
    if [ -z "$spawn_hash" ]; then
        echo "No spawnHash found for agent: $agent_id" >&2
        return 1
    fi

    # Get delta and write to agent's directory
    local agent_dir="$cog_root/agents/$agent_id"
    mkdir -p "$agent_dir"

    cog_delta_json "$spawn_hash" > "$agent_dir/delta-snapshot.json"
    echo "Delta snapshot saved: $agent_dir/delta-snapshot.json"
}

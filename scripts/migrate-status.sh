#!/bin/bash
# .cog/lib/migrate-status.sh
# Migrate status files from legacy to unified location

set -euo pipefail

COG_ROOT="${COG_ROOT:-$(git rev-parse --show-toplevel)/.cog}"

migrate_status() {
    local legacy_dir="$COG_ROOT/agents/.status"
    local unified_dir="$COG_ROOT/status"

    # Ensure unified directory exists
    mkdir -p "$unified_dir"

    # Check if legacy directory exists and is not a symlink
    if [ -d "$legacy_dir" ] && [ ! -L "$legacy_dir" ]; then
        echo "Migrating status files from $legacy_dir to $unified_dir..."

        for f in "$legacy_dir"/*.json; do
            [ -f "$f" ] || continue
            local basename=$(basename "$f")
            local agentId="${basename%.json}"
            local newPath="$unified_dir/$basename"

            # Skip if already migrated
            if [ -f "$newPath" ]; then
                echo "  Skipping $basename (already exists)"
                continue
            fi

            # Transform schema if jq available
            if command -v jq &>/dev/null; then
                jq '{
                    agentId: (.agentId // .id),
                    role: (.role // .agentType),
                    parentAgent: (.parentAgent // .spawnedBy // "root"),
                    spawnHash: (.spawnHash // null),
                    allowedPaths: (.allowedPaths // []),
                    status: (if .status == "complete" then "completed" else .status end),
                    statusHistory: (.statusHistory // [{"status": .status, "at": .spawnedAt, "by": "migration"}]),
                    spawnedAt: .spawnedAt,
                    startedAt: .startedAt,
                    completedAt: .completedAt,
                    runtimeType: (.runtimeType // "nodejs"),
                    pid: .pid,
                    iterations: (.iterations // 0),
                    progress: (.progress // 0),
                    lastHeartbeat: .lastHeartbeat,
                    task: (.task // .taskDescription),
                    taskRef: .taskRef,
                    result: .result,
                    summary: .summary,
                    error: .error
                }' "$f" > "$newPath"
            else
                # Simple copy if jq not available
                cp "$f" "$newPath"
            fi

            echo "  Migrated $basename"
        done

        # Create backup before replacing with symlink
        mv "$legacy_dir" "$legacy_dir.bak.$(date +%s)"

        # Create symlink for backwards compatibility
        ln -s "../status" "$legacy_dir"
        echo "Created symlink: $legacy_dir -> ../status"
    elif [ -L "$legacy_dir" ]; then
        echo "Legacy directory is already a symlink (migration complete)"
    else
        echo "No legacy status directory found"
        # Create symlink anyway for backwards compat
        mkdir -p "$(dirname "$legacy_dir")"
        ln -sf "../status" "$legacy_dir"
        echo "Created symlink: $legacy_dir -> ../status"
    fi
}

# Run if executed directly
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    migrate_status
fi

#!/bin/sh
# .cog/lib/security.sh - Role-based path enforcement
#
# Enforces view constraints defined in ROLE.cog.md files.
# Root agent (no role) has full access. Agents with roles are restricted
# by their view.include and view.exclude patterns.

# Check if path is allowed for current role
# Usage: cog_check_path <path> [operation]
# Returns: 0 if allowed, 1 if denied
cog_check_path() {
    local path="$1"
    local operation="${2:-read}"

    # Root agent (no role) has full access
    [ -z "$COG_AGENT_ROLE" ] && return 0

    local role_file="${COG_DIR}/roles/${COG_AGENT_ROLE}/ROLE.cog.md"
    [ ! -f "$role_file" ] && return 0

    # Simple pattern matching for exclude patterns
    # In production, use proper YAML parsing
    if grep -q "exclude:" "$role_file"; then
        local excludes=$(sed -n '/exclude:/,/^[^ ]/p' "$role_file" | grep "^    - " | sed 's/^    - //')
        for pattern in $excludes; do
            case "$path" in
                $pattern|*/$pattern|$pattern/*)
                    echo "DENIED: $path matches exclude pattern $pattern" >&2
                    return 1
                    ;;
            esac
        done
    fi

    return 0
}

# Safe file read wrapper
# Usage: cog_safe_read <path>
cog_safe_read() {
    local path="$1"
    cog_check_path "$path" "read" || return 1
    cat "$path"
}

# Safe file write wrapper
# Usage: cog_safe_write <path> [content from stdin]
cog_safe_write() {
    local path="$1"
    cog_check_path "$path" "write" || return 1
    cat > "$path"
}

# NOTE: cog_can_spawn() is defined in roles.sh
# The authoritative implementation checks the role's spawn list.
# Do NOT add a permissive stub here - it would override the real check.
# See: .cog/lib/roles.sh:106-118

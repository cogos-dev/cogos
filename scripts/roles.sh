#!/bin/sh
# .cog/lib/roles.sh - Role management primitives
#
# Roles define agent capabilities and constraints.
# Roles are cogdocs in .cog/roles/<role>/ROLE.cog.md
#
# Usage:
#   cog_source roles
#   cog_role_load coordinator
#   cog_can_spawn researcher && cog_spawn researcher

# =============================================================================
# ROLE LOADING
# =============================================================================

# Load a role definition
# Sets COG_ROLE_* variables
# Usage: cog_role_load <role>
cog_role_load() {
  local role="${1:?Role required}"
  local role_file="${COG_DIR}/roles/${role}/ROLE.cog.md"

  if [ ! -f "$role_file" ]; then
    echo "Role not found: $role" >&2
    return 1
  fi

  # Set role identity
  export COG_AGENT_ROLE="$role"

  # Parse role frontmatter
  # Note: We use 'next' after list items to prevent gsub-modified $0 from
  # triggering the list-end pattern (which checks for non-space start)
  eval "$(awk '
    /^---$/ { if (in_front) exit; in_front=1; next }
    in_front && /^layer:/ { gsub(/^layer: */, ""); print "COG_AGENT_LAYER=\"" $0 "\""; next }
    in_front && /^title:/ { gsub(/^title: *["'\'']?/, ""); gsub(/["'\'']$/, ""); print "COG_ROLE_TITLE=\"" $0 "\""; next }
    in_front && /^capabilities:/ { in_caps=1; next }
    in_front && in_caps && /^  - / { gsub(/^  - /, ""); caps = caps " " $0; next }
    in_front && in_caps && /^[^ ]/ { in_caps=0 }
    in_front && /^spawns:/ { in_spawns=1; next }
    in_front && in_spawns && /^  - / { gsub(/^  - /, ""); spawns = spawns " " $0; next }
    in_front && in_spawns && /^[^ ]/ { in_spawns=0 }
    END {
      print "COG_ROLE_CAPABILITIES=\"" caps "\""
      print "COG_ROLE_SPAWNS=\"" spawns "\""
    }
  ' "$role_file")"

  export COG_ROLE_FILE="$role_file"
}

# List available roles
cog_roles() {
  for d in "${COG_DIR}/roles"/*/; do
    [ -d "$d" ] || continue
    local role=$(basename "$d")
    [ "$role" = ".status" ] && continue
    if [ -f "${d}/ROLE.cog.md" ]; then
      echo "$role"
    fi
  done
}

# Get role info
# Usage: cog_role_info <role>
# Shows role frontmatter with view paths formatted as cog:// URIs
cog_role_info() {
  local role="${1:?Role required}"
  local role_file="${COG_DIR}/roles/${role}/ROLE.cog.md"

  if [ ! -f "$role_file" ]; then
    echo "Role not found: $role" >&2
    return 1
  fi

  # Parse and display frontmatter, transforming view paths to cog:// URIs
  awk '
    /^---$/ { if (in_front) exit; in_front=1; next }
    in_front {
      # Check if we are in view.include or view.exclude section
      if (/^view:/) { in_view = 1 }
      else if (/^  include:/) { in_include = 1; in_exclude = 0 }
      else if (/^  exclude:/) { in_exclude = 1; in_include = 0 }
      else if (/^[a-z]/) { in_view = 0; in_include = 0; in_exclude = 0 }

      # Transform view paths to cog:// URIs
      if ((in_include || in_exclude) && /^    - /) {
        # Extract the path and add cog:// prefix
        path = $0
        sub(/^    - /, "", path)
        print "    - cog://" path
      } else {
        print
      }
    }
  ' "$role_file"
}

# =============================================================================
# CAPABILITY CHECKS
# =============================================================================

# Check if current role can spawn a child role
# Usage: cog_can_spawn <child_role>
cog_can_spawn() {
  local child="${1:?Child role required}"

  # Root can spawn anything
  [ -z "$COG_AGENT_ROLE" ] && return 0

  # Check spawn list
  for allowed in $COG_ROLE_SPAWNS; do
    [ "$allowed" = "$child" ] && return 0
  done

  return 1
}

# Check if current role has a capability
# Usage: cog_has_capability <capability>
cog_has_capability() {
  local cap="${1:?Capability required}"

  for c in $COG_ROLE_CAPABILITIES; do
    [ "$c" = "$cap" ] && return 0
  done

  return 1
}

# Check layer constraint
# Usage: cog_layer_allows <required_layer>
cog_layer_allows() {
  local required="${1:?Layer required}"
  [ "${COG_AGENT_LAYER:-0}" -ge "$required" ]
}

# =============================================================================
# SPAWN WITH ROLE
# =============================================================================

# Spawn an agent with a specific role
# Usage: cog_spawn_role <role> [task]
cog_spawn_role() {
  local role="${1:?Role required}"
  local task="${2:-}"

  # Check permission
  if ! cog_can_spawn "$role"; then
    echo "Cannot spawn $role from ${COG_AGENT_ROLE:-root}" >&2
    return 1
  fi

  # Spawn the agent
  local agent_id=$(cog_spawn "$role" "$task")

  # Update status with role info
  local status_file="${COG_DIR}/status/${agent_id}.json"
  if [ -f "$status_file" ]; then
    # Add role layer to status (simple sed approach)
    local layer=$(grep "^layer:" "${COG_DIR}/roles/${role}/ROLE.cog.md" | head -1 | awk '{print $2}')
    # Note: proper JSON manipulation would need jq, this is simplified
  fi

  echo "$agent_id"
}

# =============================================================================
# VIEW PROJECTION - Theorem 8.1 implementation
# =============================================================================

# Get files visible to a role
# Usage: cog_role_view <role>
cog_role_view() {
  local role="${1:?Role required}"
  local role_file="${COG_DIR}/roles/${role}/ROLE.cog.md"

  [ -f "$role_file" ] || { echo "Role not found: $role" >&2; return 1; }

  # Extract view patterns from frontmatter
  local in_view=false
  local in_include=false
  local in_exclude=false
  local includes=""
  local excludes=""

  while IFS= read -r line; do
    case "$line" in
      "---") [ "$in_view" = "true" ] && break ;;
      "view:") in_view=true ;;
      "  include:") in_include=true; in_exclude=false ;;
      "  exclude:") in_exclude=true; in_include=false ;;
      "    - "*)
        local pattern="${line#    - }"
        if [ "$in_include" = "true" ]; then
          includes="$includes $pattern"
        elif [ "$in_exclude" = "true" ]; then
          excludes="$excludes $pattern"
        fi
        ;;
      *)
        # Check if line doesn't start with two spaces (using case pattern)
        case "$line" in
          "  "*) ;;  # Starts with spaces - continue
          *) in_include=false; in_exclude=false ;;
        esac
        ;;
    esac
  done < "$role_file"

  # Default: see everything if no view defined
  if [ -z "$includes" ]; then
    find "$COG_DIR" -type f -name "*.md" -o -name "*.json" 2>/dev/null
    return 0
  fi

  # Apply include patterns
  for pattern in $includes; do
    find "$COG_DIR" -path "$COG_DIR/$pattern" -type f 2>/dev/null
  done | sort -u | while read -r file; do
    # Apply exclude patterns
    local excluded=false
    for pattern in $excludes; do
      case "$file" in
        *"$pattern"*) excluded=true; break ;;
      esac
    done
    [ "$excluded" = "false" ] && echo "$file"
  done
}

# Hash what a role can see (for view verification)
# Usage: cog_view_hash <role>
cog_view_hash() {
  local role="${1:?Role required}"
  cog_role_view "$role" | xargs sha256sum 2>/dev/null | sort | sha256sum | cut -d' ' -f1
}

# Verify agent sees what they should
# Usage: cog_verify_view <agent_id>
cog_verify_view() {
  local agent_id="${1:?Agent ID required}"
  local status_file="${COG_DIR}/status/${agent_id}.json"

  [ -f "$status_file" ] || { echo "No status for $agent_id" >&2; return 1; }

  # Get role from status
  local role=$(grep -o '"role": *"[^"]*"' "$status_file" | cut -d'"' -f4)
  [ -z "$role" ] && { echo "No role for $agent_id" >&2; return 1; }

  # Compute expected view hash
  local expected=$(cog_view_hash "$role")

  # Get claimed view hash
  local claimed=$(cog_signal_read "view/${agent_id}" 2>/dev/null)

  if [ -z "$claimed" ]; then
    echo "No view announced by $agent_id"
    return 1
  elif [ "$claimed" = "$expected" ]; then
    echo "VERIFIED: $agent_id view matches $role"
    return 0
  else
    echo "MISMATCH: $agent_id claims $(_cog_trunc "$claimed" 8)... expected $(_cog_trunc "$expected" 8)..."
    return 1
  fi
}

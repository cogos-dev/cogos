#!/bin/sh
# .cog/lib/validate.sh - Cogdoc Validation Library
#
# Validates cogdocs have proper YAML frontmatter and required fields.
# Integrates with cog_verify for coherence checking.
#
# Usage:
#   . .cog/lib/validate.sh
#   cog_validate_memory          # Validate all memory files
#   cog_validate_cogdoc FILE     # Validate single file
#

# =============================================================================
# CONFIGURATION
# =============================================================================

# Required fields for cogdocs (minimal set)
# Note: Different sectors may have different requirements
COG_REQUIRED_FIELDS_MINIMAL="title"
COG_REQUIRED_FIELDS_MEMORY="title memory_sector"

# Valid memory sectors
COG_VALID_SECTORS="semantic episodic procedural reflective"

# =============================================================================
# SINGLE FILE VALIDATION
# =============================================================================

# Check if file has YAML frontmatter (starts with ---)
# Usage: cog_has_frontmatter FILE
# Returns: 0 if has frontmatter, 1 otherwise
cog_has_frontmatter() {
  local file="${1:?File required}"

  [ -f "$file" ] || return 1

  # Check first line is ---
  head -1 "$file" 2>/dev/null | grep -q '^---$'
}

# Extract frontmatter from file
# Usage: cog_extract_frontmatter FILE
# Returns: YAML frontmatter content (without --- delimiters)
cog_extract_frontmatter() {
  local file="${1:?File required}"

  [ -f "$file" ] || return 1

  awk '/^---$/ { if (count++ == 1) exit } count' "$file"
}

# Check if field exists in frontmatter
# Usage: cog_field_exists FRONTMATTER FIELD
cog_field_exists() {
  local frontmatter="$1"
  local field="$2"

  echo "$frontmatter" | grep -q "^${field}:"
}

# Extract field value from frontmatter
# Usage: cog_field_value FRONTMATTER FIELD
cog_field_value() {
  local frontmatter="$1"
  local field="$2"

  echo "$frontmatter" | awk -v field="$field" '
    $0 ~ "^" field ":" {
      sub("^" field ":[[:space:]]*", "");
      gsub(/^["'\'']|["'\'']$/, "");  # Remove quotes
      print;
      exit;
    }
  '
}

# Validate a single cogdoc file
# Usage: cog_validate_cogdoc FILE [--quiet]
# Returns: 0 if valid, 1 if invalid
# Output: Error messages (unless --quiet)
cog_validate_cogdoc() {
  local file="${1:?File required}"
  local quiet="${2:-}"
  local errors=""

  # Check file exists
  if [ ! -f "$file" ]; then
    [ "$quiet" != "--quiet" ] && echo "File not found: $file" >&2
    return 1
  fi

  # Check has frontmatter
  if ! cog_has_frontmatter "$file"; then
    [ "$quiet" != "--quiet" ] && echo "No frontmatter: $file" >&2
    return 1
  fi

  # Extract frontmatter
  local fm=$(cog_extract_frontmatter "$file")

  # Check required field: title
  if ! cog_field_exists "$fm" "title"; then
    errors="${errors}missing title; "
  fi

  # For memory files, check memory_sector
  case "$file" in
    */.cog/mem/*)
      if ! cog_field_exists "$fm" "memory_sector"; then
        errors="${errors}missing memory_sector; "
      else
        # Validate sector value
        local sector=$(cog_field_value "$fm" "memory_sector")
        local valid=0
        for s in $COG_VALID_SECTORS; do
          [ "$sector" = "$s" ] && valid=1
        done
        if [ "$valid" = "0" ]; then
          errors="${errors}invalid memory_sector '$sector'; "
        fi
      fi
      ;;
  esac

  if [ -n "$errors" ]; then
    [ "$quiet" != "--quiet" ] && echo "$file: $errors" >&2
    return 1
  fi

  return 0
}

# =============================================================================
# BULK VALIDATION
# =============================================================================

# Validate all memory files
# Usage: cog_validate_memory [--quiet] [--summary] [--count]
# Output: List of invalid files (unless --quiet), or just count (--count)
cog_validate_memory() {
  local quiet=""
  local summary=""
  local count_only=""

  # Parse args
  for arg in "$@"; do
    case "$arg" in
      --quiet) quiet="--quiet" ;;
      --summary) summary="yes" ;;
      --count) count_only="yes" ;;
    esac
  done

  local memory_dir="${COG_DIR}/mem"

  # Use temp file for counts (subshell variable scoping)
  local tmpfile=$(mktemp)
  echo "0 0" > "$tmpfile"

  # Find all .md files in memory (excluding README.md and TEMPLATE.md)
  # Use -print0 and read to handle spaces in filenames
  find "$memory_dir" -name "*.md" -type f ! -name "README.md" ! -name "TEMPLATE.md" -print0 2>/dev/null | \
  while IFS= read -r -d '' file; do
    read total_count invalid_count < "$tmpfile"
    total_count=$((total_count + 1))

    if ! cog_validate_cogdoc "$file" --quiet; then
      invalid_count=$((invalid_count + 1))
    fi
    echo "$total_count $invalid_count" > "$tmpfile"
  done

  read total_count invalid_count < "$tmpfile"
  rm -f "$tmpfile"

  # Output results
  if [ "$count_only" = "yes" ]; then
    echo "$invalid_count"
  elif [ "$summary" = "yes" ]; then
    echo "Memory validation: $invalid_count invalid of $total_count files"
  fi

  # Return success if no invalid files
  [ "$invalid_count" -eq 0 ]
}

# Get list of invalid memory files (for coherence check)
# Usage: cog_invalid_memory
# Returns: List of file paths with validation errors
cog_invalid_memory() {
  local memory_dir="${COG_DIR}/mem"

  find "$memory_dir" -name "*.md" -type f ! -name "README.md" ! -name "TEMPLATE.md" -print0 2>/dev/null | \
  while IFS= read -r -d '' file; do
    if ! cog_validate_cogdoc "$file" --quiet; then
      echo "$file"
    fi
  done
}

# =============================================================================
# COHERENCE INTEGRATION
# =============================================================================

# Extended coherence check that includes frontmatter validation
# Usage: cog_coherent_extended
# Returns: 0 if coherent, 1 if not
cog_coherent_extended() {
  # Check for broken refs (original coherence)
  local broken_refs=$(cog_refs --broken 2>/dev/null)
  if [ -n "$broken_refs" ]; then
    return 1
  fi

  # Check for invalid memory files
  local invalid=$(cog_invalid_memory 2>/dev/null)
  if [ -n "$invalid" ]; then
    return 1
  fi

  return 0
}

# Memory validation report (for cog verify)
# Usage: cog_memory_validation_report
cog_memory_validation_report() {
  local memory_dir="${COG_DIR}/mem"

  # Use a temp file to collect counts (to handle subshell variable scoping)
  local tmpfile=$(mktemp)
  echo "0 0 0 0 0" > "$tmpfile"

  # Use -print0 and read to handle spaces in filenames
  find "$memory_dir" -name "*.md" -type f ! -name "README.md" ! -name "TEMPLATE.md" -print0 2>/dev/null | \
  while IFS= read -r -d '' file; do
    read total no_fm missing_sec missing_tit mismatch < "$tmpfile"
    total=$((total + 1))

    # Check frontmatter
    if ! cog_has_frontmatter "$file"; then
      no_fm=$((no_fm + 1))
      echo "$total $no_fm $missing_sec $missing_tit $mismatch" > "$tmpfile"
      continue
    fi

    local fm=$(cog_extract_frontmatter "$file")

    # Check title
    if ! cog_field_exists "$fm" "title"; then
      missing_tit=$((missing_tit + 1))
    fi

    # Check memory_sector
    if ! cog_field_exists "$fm" "memory_sector"; then
      missing_sec=$((missing_sec + 1))
    else
      # Check sector matches path
      local declared=$(cog_field_value "$fm" "memory_sector")
      local rel_path=$(echo "$file" | sed "s|.*/mem/||")
      local actual=$(echo "$rel_path" | cut -d'/' -f1)

      if [ "$declared" != "$actual" ] && echo "$COG_VALID_SECTORS" | grep -qw "$actual"; then
        mismatch=$((mismatch + 1))
      fi
    fi
    echo "$total $no_fm $missing_sec $missing_tit $mismatch" > "$tmpfile"
  done

  read total no_frontmatter missing_sector missing_title sector_mismatch < "$tmpfile"
  rm -f "$tmpfile"

  local invalid=$((no_frontmatter + missing_sector + missing_title + sector_mismatch))

  echo "Memory Validation:"
  echo "  Total files:       $total"
  echo "  No frontmatter:    $no_frontmatter"
  echo "  Missing sector:    $missing_sector"
  echo "  Missing title:     $missing_title"
  echo "  Sector mismatch:   $sector_mismatch"
  echo "  Valid:             $((total - invalid))"

  [ "$invalid" -eq 0 ]
}

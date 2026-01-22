#!/bin/sh
# .cog/lib/merkle.sh - Merkle proof utilities
#
# Leverages git's internal Merkle tree structure for efficient verification.
# Enables O(log n) proof of file inclusion in state.
#
# Usage:
#   cog_source merkle
#   cog_merkle_root
#   cog_merkle_proof "path/to/file"
#   cog_merkle_verify "path/to/file" "claimed_hash" "root_hash"

# =============================================================================
# MERKLE ROOTS
# =============================================================================

# Get Merkle root of current .cog/ state
cog_merkle_root() {
  git -C "$COG_ROOT" add -A .cog/ 2>/dev/null
  git -C "$COG_ROOT" write-tree --prefix=.cog/ 2>/dev/null
}

# Get Merkle root at a specific commit
cog_merkle_root_at() {
  local commit="${1:-HEAD}"
  git -C "$COG_ROOT" rev-parse "${commit}:.cog" 2>/dev/null
}

# =============================================================================
# MERKLE PROOFS
# =============================================================================

# Generate Merkle proof for a file (path from leaf to root)
# Returns: file_hash tree_hashes...
# Accepts paths relative to workspace root OR relative to .cog/
cog_merkle_proof() {
  local filepath="${1:?File path required}"
  local full_path=""
  local git_path=""

  # Try workspace-relative path first, then .cog-relative
  if [ -f "${COG_ROOT}/${filepath}" ]; then
    full_path="${COG_ROOT}/${filepath}"
    git_path="${filepath}"
  elif [ -f "${COG_DIR}/${filepath}" ]; then
    full_path="${COG_DIR}/${filepath}"
    git_path=".cog/${filepath}"
  else
    echo "File not found: $filepath" >&2
    return 1
  fi

  # Get file blob hash
  local file_hash=$(git -C "$COG_ROOT" hash-object "$full_path" 2>/dev/null)
  echo "blob:$file_hash"

  # Walk up the tree, collecting sibling hashes
  local current_path="$git_path"
  local stop_at="."
  # Determine stop condition based on path type
  case "$git_path" in
    .cog/*) stop_at=".cog" ;;
    *) stop_at="." ;;
  esac

  while [ "$current_path" != "$stop_at" ] && [ "$current_path" != "." ]; do
    local parent_path=$(dirname "$current_path")
    local tree_hash=$(git -C "$COG_ROOT" rev-parse "HEAD:${parent_path}" 2>/dev/null)
    [ -n "$tree_hash" ] && echo "tree:$tree_hash:$parent_path"
    current_path="$parent_path"
  done

  # Root hash
  local root=$(cog_merkle_root)
  echo "root:$root"
}

# Verify a file exists in a given Merkle root
cog_merkle_verify() {
  local filepath="${1:?File path required}"
  local expected_hash="${2:?Expected file hash required}"
  local root_hash="${3:?Root hash required}"

  # Get current root
  local current_root=$(cog_merkle_root)

  # Quick check: if roots match, verify file hash
  if [ "$current_root" = "$root_hash" ]; then
    local actual_hash=$(git -C "$COG_ROOT" hash-object "${COG_DIR}/${filepath}" 2>/dev/null)
    if [ "$actual_hash" = "$expected_hash" ]; then
      echo "VERIFIED: $filepath in root $(_cog_trunc "$root_hash" 8)..."
      return 0
    fi
  fi

  echo "FAILED: $filepath not verified" >&2
  return 1
}

# =============================================================================
# DIFF PROOFS
# =============================================================================

# Get files changed between two Merkle roots
cog_merkle_diff() {
  local root1="${1:?First root required}"
  local root2="${2:?Second root required}"

  git -C "$COG_ROOT" diff-tree -r --name-only "$root1" "$root2" 2>/dev/null | \
    sed 's|^|.cog/|'
}

# Efficient sync: only transfer changed files
cog_merkle_sync_needed() {
  local their_root="${1:?Their root required}"
  local my_root=$(cog_merkle_root)

  if [ "$my_root" = "$their_root" ]; then
    echo "IN_SYNC"
    return 0
  fi

  echo "NEED_SYNC"
  cog_merkle_diff "$their_root" "$my_root"
  return 1
}

# =============================================================================
# SUBTREE PROOFS
# =============================================================================

# Get Merkle root of a subdirectory
cog_subtree_root() {
  local subpath="${1:?Subpath required}"
  git -C "$COG_ROOT" rev-parse "HEAD:.cog/${subpath}" 2>/dev/null
}

# Verify a subtree matches expected hash
cog_subtree_verify() {
  local subpath="${1:?Subpath required}"
  local expected="${2:?Expected hash required}"

  local actual=$(cog_subtree_root "$subpath")
  if [ "$actual" = "$expected" ]; then
    echo "VERIFIED: .cog/$subpath = $(_cog_trunc "$expected" 8)..."
    return 0
  fi

  echo "MISMATCH: .cog/$subpath actual=$(_cog_trunc "$actual" 8)... expected=$(_cog_trunc "$expected" 8)..." >&2
  return 1
}

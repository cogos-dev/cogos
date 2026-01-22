#!/bin/sh
# .cog/lib/vectors.sh - Vector database operations
#
# Shell interface to the JSONL-based vector store for semantic search.
# Provides commands for embedding cogdocs and searching by similarity.
#
# Usage:
#   . .cog/lib/vectors.sh
#   cog_embed_index                    # Build embeddings for all cogdocs
#   cog_embed_search "query text"      # Search for similar cogdocs
#   cog_embed_status                   # Show embedding store status
#
# Architecture:
#   - Shell functions call standalone embed.mjs script
#   - Embeddings stored in .cog/db/vectors/embeddings.jsonl
#   - Content-hash based incremental updates
#   - Provider auto-detection (OpenAI → Ollama → Mock)

set -e

# =============================================================================
# VECTOR STORE OPERATIONS
# =============================================================================

# Get the embed script path
_cog_embed_script() {
  local root="${COG_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
  echo "${root}/.cog/scripts/embed.mjs"
}

# Show embedding store status
# Usage: cog_embed_status
cog_embed_status() {
  local root="${COG_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
  local script="$(_cog_embed_script)"
  local db_path="${root}/.cog/db/vectors/embeddings.jsonl"

  if [ -f "$script" ]; then
    # Try to run the status command
    if ! (cd "$root" && node "$script" status 2>/dev/null); then
      # Script failed, provide helpful guidance
      echo "=== Embedding Store Status ===" >&2
      if [ -f "$db_path" ]; then
        local count=$(wc -l < "$db_path" | tr -d ' ')
        echo "Index file: $db_path" >&2
        echo "Embeddings: $count documents" >&2
        echo "" >&2
        echo "Note: Full status unavailable. Try 'npm install' in .cog/scripts/ to fix." >&2
      else
        echo "No index found." >&2
        echo "" >&2
        echo "To build the embedding index:" >&2
        echo "  1. Ensure dependencies: cd .cog/scripts && npm install" >&2
        echo "  2. Set API key: export OPENAI_API_KEY=sk-..." >&2
        echo "  3. Run: ./scripts/cog embed index" >&2
      fi
      return 1
    fi
  else
    echo "Error: embed.mjs not found at $script" >&2
    echo "" >&2
    echo "The embedding system requires .cog/scripts/embed.mjs" >&2
    return 1
  fi
}

# Index all cogdocs (build embeddings)
# Usage: cog_embed_index [--provider openai|ollama|mock]
cog_embed_index() {
  local root="${COG_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
  local script="$(_cog_embed_script)"
  local provider="auto"

  # Parse arguments
  while [ $# -gt 0 ]; do
    case "$1" in
      --provider)
        provider="$2"
        shift 2
        ;;
      *)
        echo "Unknown option: $1" >&2
        echo "Usage: cog_embed_index [--provider openai|ollama|mock]" >&2
        return 1
        ;;
    esac
  done

  if [ -f "$script" ]; then
    (cd "$root" && node "$script" index --provider "$provider")
  else
    echo "Error: embed.mjs not found at $script" >&2
    return 1
  fi
}

# Search for similar cogdocs by semantic similarity
# Usage: cog_embed_search "query text" [--top-k N] [--provider PROVIDER]
cog_embed_search() {
  local query="${1:?Query text required}"
  shift

  local root="${COG_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
  local script="$(_cog_embed_script)"
  local top_k=10
  local provider="auto"

  # Parse arguments
  while [ $# -gt 0 ]; do
    case "$1" in
      --top-k)
        top_k="$2"
        shift 2
        ;;
      --provider)
        provider="$2"
        shift 2
        ;;
      *)
        echo "Unknown option: $1" >&2
        echo "Usage: cog_embed_search <query> [--top-k N] [--provider PROVIDER]" >&2
        return 1
        ;;
    esac
  done

  if [ -f "$script" ]; then
    (cd "$root" && node "$script" search "$query" --top-k "$top_k" --provider "$provider")
  else
    echo "Error: embed.mjs not found at $script" >&2
    return 1
  fi
}

# Update embeddings (reindex changed documents only)
# Usage: cog_embed_update
cog_embed_update() {
  echo "Checking for changed documents..."
  cog_embed_index "$@"
}

# =============================================================================
# INTEGRATION WITH COGN8
# =============================================================================

# These functions can be called via the unified cogn8 interface
# Example: cogn8 embed index
#          cogn8 embed search "query"
#          cogn8 embed status

# Export functions for subshells (bash only)
if [ -n "${BASH_VERSION:-}" ]; then
  export -f cog_embed_status cog_embed_index cog_embed_search cog_embed_update 2>/dev/null || true
fi

#!/bin/sh
# .cog/lib/salience.sh - Git-Derived Salience System
#
# Implements ADR-018: Salience System (Git-Derived Attention)
#
# Provides:
#   - cog_file_salience()      Compute salience for a single file
#   - cog_salience_rank()      Rank all cogdocs by salience
#   - cog_hot_files()          Files with high recent activity
#   - cog_cold_files()         Files with low activity
#   - cog_salience_metrics()   Show detailed metrics breakdown

# Default weights for salience computation
COG_SALIENCE_WEIGHT_RECENCY="${COG_SALIENCE_WEIGHT_RECENCY:-0.4}"
COG_SALIENCE_WEIGHT_FREQUENCY="${COG_SALIENCE_WEIGHT_FREQUENCY:-0.3}"
COG_SALIENCE_WEIGHT_CHURN="${COG_SALIENCE_WEIGHT_CHURN:-0.2}"
COG_SALIENCE_WEIGHT_AUTHORSHIP="${COG_SALIENCE_WEIGHT_AUTHORSHIP:-0.1}"

# Default decay model and half-life
COG_SALIENCE_DECAY="${COG_SALIENCE_DECAY:-exponential}"
COG_SALIENCE_HALFLIFE="${COG_SALIENCE_HALFLIFE:-30}"

# =============================================================================
# DECAY MODELS
# =============================================================================

# Compute decay value using specified model
# Usage: _cog_decay <model> <days_ago> [half_life]
_cog_decay() {
  local model="${1:-exponential}"
  local days_ago="${2:?Days required}"
  local half_life="${3:-30}"

  case "$model" in
    exponential)
      # e^(-t/τ)
      awk "BEGIN {print exp(-$days_ago/$half_life)}"
      ;;
    linear)
      # max(0, 1 - t/(2τ))
      awk "BEGIN {
        v = 1 - $days_ago/(2*$half_life);
        print (v > 0) ? v : 0
      }"
      ;;
    step)
      # t < τ ? 1 : 0
      awk "BEGIN {print ($days_ago < $half_life) ? 1 : 0}"
      ;;
    logarithmic)
      # 1 / (1 + log(1 + t/τ))
      awk "BEGIN {
        t = 1 + $days_ago/$half_life;
        print 1 / (1 + log(t))
      }"
      ;;
    *)
      echo "0" >&2
      return 1
      ;;
  esac
}

# =============================================================================
# CORE SALIENCE COMPUTATION
# =============================================================================

# Compute salience for a single file
# Usage: cog_file_salience <filepath> [days_window]
# Returns: Salience score (0.00 - 1.00)
cog_file_salience() {
  local filepath="${1:?File path required}"
  local days_window="${2:-90}"  # Look back 90 days by default

  # Verify file exists
  [ -f "$filepath" ] || { echo "0.00"; return 1; }

  # Get git log for this file
  local log=$(git log --since="${days_window} days ago" \
    --format="%H %at %ae" --numstat -- "$filepath" 2>/dev/null)

  # If no git history, return 0
  [ -z "$log" ] && { echo "0.00"; return 0; }

  # Parse commits using temp file to avoid subshell variable scope issues
  local log_file="/tmp/.cog_salience_log.$$"
  echo "$log" > "$log_file"

  local commit_count=0
  local total_changes=0
  local last_timestamp=0
  local authors=""
  local current_commit=""

  while IFS= read -r line; do
    # Check for commit header line (40 hex chars at start)
    case "$line" in
      [a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9]*)
        # Verify it's a full 40-char hash
        local first_word=$(echo "$line" | cut -d' ' -f1)
        if echo "$first_word" | grep -q '^[a-f0-9]\{40\}$'; then
          commit_count=$((commit_count + 1))
          current_commit="$first_word"
          last_timestamp=$(echo "$line" | cut -d' ' -f2)
          author=$(echo "$line" | cut -d' ' -f3)
          authors="$authors $author"
        fi
        ;;
      [0-9]*)
        # Numstat line: added removed filename
        local added=$(echo "$line" | awk '{print $1}')
        local removed=$(echo "$line" | awk '{print $2}')

        # Handle binary files (shown as -)
        [ "$added" = "-" ] && added=0
        [ "$removed" = "-" ] && removed=0

        total_changes=$((total_changes + added + removed))
        ;;
    esac
  done < "$log_file"

  rm -f "$log_file"

  # If no commits found, return 0
  [ "$commit_count" -eq 0 ] && { echo "0.00"; return 0; }

  # Compute metrics
  local now=$(date +%s)
  local days_ago=$(( (now - last_timestamp) / 86400 ))

  # Recency: use decay model
  local recency=$(_cog_decay "$COG_SALIENCE_DECAY" "$days_ago" "$COG_SALIENCE_HALFLIFE")

  # Frequency: commits/10, max 1.0
  local frequency=$(awk "BEGIN {f=$commit_count/10; print (f>1)?1:f}")

  # Churn: avg_changes/100, max 1.0
  local avg_changes=$((total_changes / commit_count))
  local churn=$(awk "BEGIN {c=$avg_changes/100; print (c>1)?1:c}")

  # Authorship: unique authors/5, max 1.0
  local unique_authors=$(echo "$authors" | tr ' ' '\n' | sort -u | grep -v '^$' | wc -l)
  local authorship=$(awk "BEGIN {a=$unique_authors/5; print (a>1)?1:a}")

  # Combined score with configurable weights
  local score=$(awk "BEGIN {
    printf \"%.2f\", \
      $COG_SALIENCE_WEIGHT_RECENCY*$recency + \
      $COG_SALIENCE_WEIGHT_FREQUENCY*$frequency + \
      $COG_SALIENCE_WEIGHT_CHURN*$churn + \
      $COG_SALIENCE_WEIGHT_AUTHORSHIP*$authorship
  }")

  echo "$score"
}

# Show detailed metrics breakdown for a file
# Usage: cog_salience_metrics <filepath> [days_window]
cog_salience_metrics() {
  local filepath="${1:?File path required}"
  local days_window="${2:-90}"

  # Verify file exists
  [ -f "$filepath" ] || { echo "File not found: $filepath" >&2; return 1; }

  # Get git log for this file
  local log=$(git log --since="${days_window} days ago" \
    --format="%H %at %ae" --numstat -- "$filepath" 2>/dev/null)

  if [ -z "$log" ]; then
    echo "No git history for: $filepath"
    echo "  Salience: 0.00"
    return 0
  fi

  # Parse commits using temp file to avoid subshell variable scope issues
  local log_file="/tmp/.cog_salience_metrics.$$"
  echo "$log" > "$log_file"

  local commit_count=0
  local total_changes=0
  local last_timestamp=0
  local authors=""

  while IFS= read -r line; do
    case "$line" in
      [a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9]*)
        local first_word=$(echo "$line" | cut -d' ' -f1)
        if echo "$first_word" | grep -q '^[a-f0-9]\{40\}$'; then
          commit_count=$((commit_count + 1))
          last_timestamp=$(echo "$line" | cut -d' ' -f2)
          author=$(echo "$line" | cut -d' ' -f3)
          authors="$authors $author"
        fi
        ;;
      [0-9]*)
        local added=$(echo "$line" | awk '{print $1}')
        local removed=$(echo "$line" | awk '{print $2}')
        [ "$added" = "-" ] && added=0
        [ "$removed" = "-" ] && removed=0
        total_changes=$((total_changes + added + removed))
        ;;
    esac
  done < "$log_file"

  rm -f "$log_file"

  # Compute metrics
  local now=$(date +%s)
  local days_ago=$(( (now - last_timestamp) / 86400 ))

  local recency=$(_cog_decay "$COG_SALIENCE_DECAY" "$days_ago" "$COG_SALIENCE_HALFLIFE")
  local frequency=$(awk "BEGIN {f=$commit_count/10; print (f>1)?1:f}")
  local avg_changes=$((total_changes / commit_count))
  local churn=$(awk "BEGIN {c=$avg_changes/100; print (c>1)?1:c}")
  local unique_authors=$(echo "$authors" | tr ' ' '\n' | sort -u | grep -v '^$' | wc -l)
  local authorship=$(awk "BEGIN {a=$unique_authors/5; print (a>1)?1:a}")

  local score=$(awk "BEGIN {
    printf \"%.2f\", \
      $COG_SALIENCE_WEIGHT_RECENCY*$recency + \
      $COG_SALIENCE_WEIGHT_FREQUENCY*$frequency + \
      $COG_SALIENCE_WEIGHT_CHURN*$churn + \
      $COG_SALIENCE_WEIGHT_AUTHORSHIP*$authorship
  }")

  # Display metrics
  echo "Salience metrics for: $filepath"
  echo "  Salience:   $score"
  echo "  ----------------------------------------"
  echo "  Recency:    $(printf "%.2f" $recency) (${days_ago}d ago, weight: $COG_SALIENCE_WEIGHT_RECENCY)"
  echo "  Frequency:  $(printf "%.2f" $frequency) ($commit_count commits, weight: $COG_SALIENCE_WEIGHT_FREQUENCY)"
  echo "  Churn:      $(printf "%.2f" $churn) (avg $avg_changes lines/commit, weight: $COG_SALIENCE_WEIGHT_CHURN)"
  echo "  Authorship: $(printf "%.2f" $authorship) ($unique_authors authors, weight: $COG_SALIENCE_WEIGHT_AUTHORSHIP)"
  echo "  ----------------------------------------"
  echo "  Commits:    $commit_count"
  echo "  Changes:    $total_changes lines"
  echo "  Authors:    $unique_authors unique"
  echo "  Model:      $COG_SALIENCE_DECAY (τ=$COG_SALIENCE_HALFLIFE days)"
}

# =============================================================================
# RANKING AND FILTERING
# =============================================================================

# Rank all files by salience (optimized batch version)
# Usage: cog_salience_rank [scope] [limit]
# Returns: Lines of "<score> <filepath>" sorted by score descending
cog_salience_rank() {
  local scope="${1:-.cog}"
  local limit="${2:-20}"
  local days_window="${3:-90}"

  # Get all commits with file changes in one git log call
  # This is much faster than calling git log per-file
  local now=$(date +%s)
  local tmp_file="/tmp/.cog_salience_batch.$$"

  # Get recent commits with affected files
  git log --since="${days_window} days ago" --format="COMMIT %H %at %ae" --name-only -- "$scope" 2>/dev/null | \
  awk -v now="$now" -v halflife="$COG_SALIENCE_HALFLIFE" '
    BEGIN {
      weight_recency = 0.4
      weight_frequency = 0.3
    }
    /^COMMIT / {
      timestamp = $3
      next
    }
    /^$/ { next }
    /\.(md|cog\.md)$/ {
      file = $0
      commits[file]++
      if (!(file in last_ts) || timestamp > last_ts[file]) {
        last_ts[file] = timestamp
      }
    }
    END {
      for (file in commits) {
        days_ago = (now - last_ts[file]) / 86400
        recency = exp(-days_ago / halflife)
        freq = commits[file] / 10
        if (freq > 1) freq = 1
        score = weight_recency * recency + weight_frequency * freq
        printf "%.2f %s\n", score, file
      }
    }
  ' | sort -rn | head -n "$limit"
}

# Original per-file version (slower but more accurate)
# Usage: cog_salience_rank_full [scope] [limit]
cog_salience_rank_full() {
  local scope="${1:-.cog}"
  local limit="${2:-20}"

  # Find all markdown files in scope
  find "$scope" -type f \( -name "*.md" -o -name "*.cog.md" \) 2>/dev/null | while read -r file; do
    local score=$(cog_file_salience "$file")
    echo "$score $file"
  done | sort -rn | head -n "$limit"
}

# Hot files (high recent activity)
# Usage: cog_hot_files [scope] [limit] [threshold]
# Returns: File paths with salience > threshold
cog_hot_files() {
  local scope="${1:-.cog}"
  local limit="${2:-10}"
  local threshold="${3:-0.5}"

  # Get hot files
  local hot_files=$(cog_salience_rank "$scope" 100 | \
    awk -v thresh="$threshold" '$1 > thresh {print}')

  local total_hot=$(echo "$hot_files" | grep -c '.' 2>/dev/null || echo "0")
  local showing=$limit
  [ "$total_hot" -lt "$limit" ] && showing="$total_hot"

  echo "=== Hot Files in $scope ($showing of $limit requested, $total_hot qualify) ===" >&2
  echo "$hot_files" | head -n "$limit" | awk '{print $2}'
}

# Cold files (low activity)
# Usage: cog_cold_files [scope] [limit] [threshold]
# Returns: File paths with salience < threshold
cog_cold_files() {
  local scope="${1:-.cog}"
  local limit="${2:-10}"
  local threshold="${3:-0.1}"

  cog_salience_rank "$scope" 1000 | \
    awk -v thresh="$threshold" '$1 < thresh && $1 > 0 {print $2}' | \
    tail -n "$limit"
}

# List all files with zero salience (never modified or no git history)
# Usage: cog_stale_files [scope]
cog_stale_files() {
  local scope="${1:-.cog}"

  find "$scope" -type f \( -name "*.md" -o -name "*.cog.md" \) 2>/dev/null | while read -r file; do
    local score=$(cog_file_salience "$file")
    if [ "$score" = "0.00" ]; then
      echo "$file"
    fi
  done
}

# =============================================================================
# WORKSPACE HEALTH
# =============================================================================

# Generate workspace health report based on salience (optimized)
# Usage: cog_salience_health [scope]
cog_salience_health() {
  local scope="${1:-.cog}"
  local days_window="${2:-90}"

  echo "=== Salience Health Report ==="
  echo ""

  # Count total files first (fast)
  local total=$(find "$scope" -type f \( -name "*.md" -o -name "*.cog.md" \) 2>/dev/null | wc -l)

  # Get all ranked files in one batch call (much faster)
  local now=$(date +%s)
  local tmp_out="/tmp/.cog_health_out.$$"

  git log --since="${days_window} days ago" --format="COMMIT %H %at" --name-only -- "$scope" 2>/dev/null | \
  awk -v now="$now" -v halflife="${COG_SALIENCE_HALFLIFE:-30}" -v total="$total" '
    BEGIN {
      weight_recency = 0.4
      weight_frequency = 0.3
    }
    /^COMMIT / {
      timestamp = $3
      next
    }
    /^$/ { next }
    /\.(md|cog\.md)$/ {
      file = $0
      commits[file]++
      if (!(file in last_ts) || timestamp > last_ts[file]) {
        last_ts[file] = timestamp
      }
    }
    END {
      hot = 0; warm = 0; cold = 0
      for (file in commits) {
        days_ago = (now - last_ts[file]) / 86400
        recency = exp(-days_ago / halflife)
        freq = commits[file] / 10
        if (freq > 1) freq = 1
        score = weight_recency * recency + weight_frequency * freq

        if (score >= 0.7) hot++
        else if (score >= 0.3) warm++
        else cold++

        printf "SCORE %.2f %s\n", score, file
      }
      stale = total - (hot + warm + cold)
      if (stale < 0) stale = 0

      printf "STATS Total:  %d\n", total
      printf "STATS Hot:    %d (>= 0.7)\n", hot
      printf "STATS Warm:   %d (0.3-0.7)\n", warm
      printf "STATS Cold:   %d (0.1-0.3)\n", cold
      printf "STATS Stale:  %d (0.0)\n", stale
      print "STATS "
      if (total > 0) {
        printf "STATS Activity: %.0f%%\n", ((hot + warm) / total) * 100
      }
    }
  ' > "$tmp_out"

  # Print stats
  grep "^STATS " "$tmp_out" | sed 's/^STATS //'

  echo ""
  echo "=== Top 5 Hot Files ==="
  grep "^SCORE " "$tmp_out" | sed 's/^SCORE //' | sort -rn | head -5

  echo ""
  echo "=== Top 5 Cold Files ==="
  grep "^SCORE " "$tmp_out" | sed 's/^SCORE //' | awk '$1 > 0 && $1 < 0.3' | sort -n | head -5

  rm -f "$tmp_out"
}

# =============================================================================
# EXPORT FUNCTIONS
# =============================================================================

# Export functions for use in subshells (bash only)
if [ -n "${BASH_VERSION:-}" ]; then
  export -f cog_file_salience 2>/dev/null || true
  export -f cog_salience_metrics 2>/dev/null || true
  export -f cog_salience_rank 2>/dev/null || true
  export -f cog_hot_files 2>/dev/null || true
  export -f cog_cold_files 2>/dev/null || true
  export -f cog_stale_files 2>/dev/null || true
  export -f cog_salience_health 2>/dev/null || true
  export -f _cog_decay 2>/dev/null || true
fi

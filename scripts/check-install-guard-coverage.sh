#!/usr/bin/env bash
# check-install-guard-coverage.sh — CI tripwire: fail if a new site that
# writes the cogos binary into an install path appears without referencing
# the shared running-daemon guard.
#
# Why this exists: PR #486 (see cog://mem/working/2026-07-30-self-review-spike/
# RETRO-486.md) hand-copied the "refuse to overwrite a running daemon"
# check into five separate places across twelve review rounds, because
# nothing mechanically enforced that every install site carry it. This
# script is that mechanical enforcement, added alongside the extraction of
# the check into scripts/lib/refuse-if-running.sh / .ps1 (see those files).
#
# ---- Honest limits (read before trusting this blindly) ----
#
# This is a grep-based heuristic, not a semantic analysis, and it can be
# defeated in either direction:
#
#   - FALSE NEGATIVE (misses a real unguarded site): it only recognizes the
#     operation shapes in PATTERN below (cp/mv/install/Move-Item/Copy-Item
#     near a cogos-shaped path). A site that writes the binary a different
#     way — a Go os.Rename, a Python shutil.copy, a Dockerfile COPY, an
#     obscured variable — will not be found. It is a tripwire for the
#     specific copy-paste-drift failure this repo already lived through,
#     not a guarantee that every install path anywhere is covered.
#   - FALSE POSITIVE avoidance is by an explicit, narrow ALLOWLIST below,
#     each entry with a stated reason — not a broad path exclusion.
#   - It only checks that the FILE containing a matched line also mentions
#     one of GUARD_MARKERS somewhere in it, not that the specific matched
#     line is actually preceded by a call to the guard at runtime. A file
#     that mentions "refuse_if_running" in a comment but never calls it on
#     the matched line would pass. Cheap and mechanical by design; it is a
#     net, not a proof.
#   - `git grep` only searches tracked files' working-tree content. A new
#     file that is part of the same PR is fine (CI checks out the PR's
#     commits, so it's tracked by the time this runs) -- but a genuinely
#     untracked scratch file on a local machine won't be seen. Not a gap in
#     CI, but worth knowing if you run this locally against uncommitted work.
#
# Usage: ./scripts/check-install-guard-coverage.sh
#
# no-pipefail: N/A, this file DOES declare set -euo pipefail below, kept
# for check-shell-hardening.sh's own doc-comment convention of restating
# it explicitly at the top for a future grepper — not itself an opt-out.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || (cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd))"
cd "$REPO_ROOT"

# Sites confirmed NOT to be "write the cogos binary into a path a daemon
# executes from", with a stated reason each. Keep this list short and
# specific to exact file+substring pairs — widening the match instead of
# adding a precise entry defeats the check's purpose. Matched with
# `grep -F` (no regex metachars) against the flagged line's own text.
ALLOWLIST_FILES=(
  "Makefile"
  "Makefile"
  "scripts/cog"
  "scripts/hooks/cogos_post_turn_capture.py"
  "scripts/hooks/cogos_session_awareness.py"
  "scripts/hooks/cogos_session_awareness.py"
  "scripts/migrate-status.sh"
  "scripts/migrate-status.sh"
)
ALLOWLIST_PATTERNS=(
  'INSTALL_TARGET).bak'
  'INSTALL_TARGET).tmp'
  "Run setup-dev.sh or install cogos first."
  '~/.claude/hooks/'
  '~/.claude/hooks/'
  '~/.claude/hooks/'
  'cp "$f" "$newPath"'
  'mv "$legacy_dir"'
)
ALLOWLIST_REASONS=(
  "backup of the ALREADY-installed target, made after check-not-running has already run as install's prerequisite -- not a new unguarded write"
  "atomic-install staging copy: written to a .tmp sibling and only renamed onto the real target by the mv two lines later, which check-not-running (an install: prerequisite) already gated"
  "an error message string, not an install operation"
  "copies a Claude Code hook file into ~/.claude/hooks -- unrelated to the cogos daemon binary"
  "copies a Claude Code hook file into ~/.claude/hooks -- unrelated to the cogos daemon binary"
  "copies a Claude Code hook file into ~/.claude/hooks -- unrelated to the cogos daemon binary"
  "migrates workspace status files, not the cogos binary"
  "renames a legacy status directory, not the cogos binary"
)

GUARD_MARKERS=(
  "refuse_if_running"
  "refuse-if-running.sh"
  "refuse-if-running.ps1"
  "check-not-running"
  "Assert-CogosNotRunning"
)

is_allowlisted() {
  local file="$1" line="$2" i
  for i in "${!ALLOWLIST_FILES[@]}"; do
    if [[ "$file" == "${ALLOWLIST_FILES[$i]}" ]] && printf '%s' "$line" | grep -qF -- "${ALLOWLIST_PATTERNS[$i]}"; then
      return 0
    fi
  done
  return 1
}

file_has_guard_marker() {
  local file="$1" marker
  for marker in "${GUARD_MARKERS[@]}"; do
    if grep -qF -- "$marker" "$file" 2>/dev/null; then
      return 0
    fi
  done
  return 1
}

# The Q2 shape grep from RETRO-486.md, unchanged, plus the operation
# synonyms the runbook's Step 2.5 amendment requires sweeping for
# (Move-Item/Copy-Item alongside cp/mv/install) -- this is what actually
# would have found docs/RELEASING.md:83 (a Move-Item site) before the gate
# did, per that retro's Q2 finding.
PATTERN='(cp|mv|install|Move-Item|Copy-Item) .*(cogos|INSTALL_TARGET|INSTALL_DIR|dest\.|InstallDir)'
SEARCH_PATHS=(Makefile scripts README.md CONTRIBUTING.md docs)

fail=0
matched=0
unguarded=0

while IFS=: read -r file lineno line; do
  [[ -z "$file" ]] && continue
  matched=$((matched + 1))
  if is_allowlisted "$file" "$line"; then
    continue
  fi
  if ! file_has_guard_marker "$file"; then
    echo "UNGUARDED INSTALL SITE: $file:$lineno" >&2
    echo "  $line" >&2
    echo "  (file does not mention any of: ${GUARD_MARKERS[*]})" >&2
    fail=1
    unguarded=$((unguarded + 1))
  fi
done < <(git grep -nE "$PATTERN" -- "${SEARCH_PATHS[@]}" 2>/dev/null || true)

if [[ "$fail" -ne 0 ]]; then
  echo "" >&2
  echo "check-install-guard-coverage: $unguarded of $matched matched site(s) are unguarded." >&2
  echo "Either wire in scripts/lib/refuse-if-running.sh (bash) /" >&2
  echo "scripts/lib/refuse-if-running.ps1 (PowerShell) at that site, or add a" >&2
  echo "narrowly-scoped ALLOWLIST_* entry above with a stated reason if this" >&2
  echo "genuinely is not an install-over-a-live-daemon site." >&2
  exit 1
fi

echo "check-install-guard-coverage: $matched matched site(s), all guarded or allowlisted"

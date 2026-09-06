#!/bin/sh
# test-prepush-hook.sh — negative control for .githooks/pre-push.
#
# A guard never observed refusing is not known to be a guard. This creates a
# real shell-hardening violation, attempts a push, and asserts the hook refuses
# it.
#
# The violation is one scripts/check-shell-hardening.sh actually rejects, so the
# local gate is proven to enforce the SAME rule CI enforces, not a lookalike.
#
#   make hooks                        # install the hook first
#   sh scripts/test-prepush-hook.sh
#
# PUSHES TO A THROWAWAY LOCAL BARE REPO, NEVER TO `origin`.
# An earlier revision pushed to the real remote and relied on a cleanup trap to
# delete the branch afterwards. Two problems, both caught in review: if the hook
# was NOT installed the push simply succeeded, landing a deliberately broken
# script on the shared remote and firing CI (ci.yml has no branch filter) before
# the trap could retract it; and a retraction after the fact is not the same as
# never having pushed. A test for a guard must not depend on that guard working
# in order to be safe. The bare repo makes the blast radius genuinely local:
# git runs pre-push identically for any remote, so the guard is exercised
# exactly as it would be against origin.
#
# no-pipefail: this script's job is to run a command that MUST fail (the push)
# and assert on that failure. Under `set -e` the expected non-zero push would
# abort the test before it could report PASS.
set -u

cd "$(git rev-parse --show-toplevel)" || exit 1

# --- refuse to run unless the hook is actually installed ---------------------
# The precondition used to live only in a comment. A precondition that is
# documented but unchecked is not a precondition — this is the same
# declared-not-wired shape the hook itself exists to catch.
if [ "$(git config core.hooksPath)" != ".githooks" ]; then
  echo "test-prepush-hook: hook not installed (core.hooksPath is not .githooks)." >&2
  echo "  run 'make hooks' first — otherwise this test would prove nothing." >&2
  exit 2
fi

# --- refuse to run with uncommitted CHANGES to tracked files ----------------
# This script switches branches. Uncommitted work can be silently lost or
# carried onto the scratch branch. Observed for real while developing this
# hook: a checkout during an earlier version of this script discarded an
# untracked .githooks/ directory that had not been committed yet.
#
# Scoped to TRACKED files (`--porcelain -uno`) rather than everything: a
# running kernel writes untracked runtime state under .cog/config/ (lock files,
# observatory.yaml), so a bare --porcelain check refuses to run on any machine
# with a live daemon — the guard would be unrunnable exactly where it matters.
# Branch-switching does not disturb untracked files that no branch claims,
# which is the case that actually loses work.
if [ -n "$(git status --porcelain -uno)" ]; then
  echo "test-prepush-hook: tracked files have uncommitted changes; commit or stash first." >&2
  echo "  this test switches branches and will not risk your uncommitted work." >&2
  exit 2
fi

ORIG_REF=$(git symbolic-ref --quiet --short HEAD || git rev-parse HEAD)
BRANCH="nc/prepush-$$"
VIOLATION="scripts/nc-violation-$$.sh"
FAKE_REMOTE=$(mktemp -d)

cleanup() {
  # Restore the caller's branch (or detached commit) — never a hardcoded one.
  git checkout -q "$ORIG_REF" 2>/dev/null
  git branch -D "$BRANCH" 2>/dev/null >/dev/null
  rm -f "$VIOLATION"
  git remote remove nc-throwaway 2>/dev/null
  rm -rf "$FAKE_REMOTE"
}
trap cleanup EXIT

git init -q --bare "$FAKE_REMOTE" || exit 1
git remote add nc-throwaway "$FAKE_REMOTE" || exit 1

git checkout -q -b "$BRANCH" || exit 1

# A shell script with no `set -euo pipefail` and no documented opt-out —
# exactly what scripts/check-shell-hardening.sh rejects.
printf '#!/usr/bin/env bash\necho "no hardening here"\n' > "$VIOLATION"
chmod +x "$VIOLATION"
git add "$VIOLATION"
git commit -q -m "nc: deliberate shell-hardening violation (should never push)"

echo "--- attempting push to throwaway remote; pre-push must REFUSE ---"
out=$(git push nc-throwaway "$BRANCH" 2>&1)
rc=$?
echo "$out" | grep -E 'pre-push|FAIL:|REFUSED' | head -6
echo "push exit=$rc"

if [ "$rc" -eq 0 ]; then
  echo "RESULT: FAIL — the push LANDED; the hook did not refuse"
  exit 1
elif echo "$out" | grep -q 'pre-push: REFUSED'; then
  echo "RESULT: PASS — refused by pre-push, naming the hardening failure"
else
  echo "RESULT: AMBIGUOUS — push failed, but not via the pre-push guard"
  exit 1
fi

#!/bin/sh
# test-prepush-hook.sh — negative control for .githooks/pre-push.
#
# A guard never observed refusing is not known to be a guard. This introduces a
# real shell-hardening violation on a scratch branch, attempts a push, and
# asserts the hook refuses it — then cleans up.
#
# Deliberately uses a violation the CI lint job would also catch, so the local
# gate is proven to be enforcing the SAME rule rather than a lookalike.
#
# Requires the hook to be installed (`make hooks`). Run:
#   sh scripts/test-prepush-hook.sh
#
# no-pipefail: this script's job is to run a command that MUST fail (the push)
# and assert on that failure. Under `set -e` the expected non-zero push would
# abort the test before it could report PASS.
set -u
cd "$(git rev-parse --show-toplevel)" || exit 1

BRANCH="nc/prepush-$$"
VIOLATION="scripts/nc-violation-$$.sh"

cleanup() {
  git checkout -q --detach origin/main 2>/dev/null
  git branch -D "$BRANCH" 2>/dev/null >&2
  rm -f "$VIOLATION"
  git push -q origin --delete "$BRANCH" 2>/dev/null
}
trap cleanup EXIT

git checkout -q -b "$BRANCH" origin/main || exit 1

# A shell script with no `set -euo pipefail` and no documented opt-out —
# exactly what scripts/check-shell-hardening.sh rejects.
printf '#!/usr/bin/env bash\necho "no hardening here"\n' > "$VIOLATION"
chmod +x "$VIOLATION"
git add "$VIOLATION"
git commit -q -m "nc: deliberate shell-hardening violation (should never push)"

echo "--- attempting push; pre-push must REFUSE ---"
out=$(git push origin "$BRANCH" 2>&1)
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

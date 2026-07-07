#!/usr/bin/env bash
# check-shell-hardening.sh — CI gate for scripts/*.sh hardening (ledger L15).
#
# Enforces two cheap, mechanical properties on every tracked *.sh file under
# scripts/:
#
#   1. Error-handling discipline: the file either declares
#      `set -euo pipefail` (bash/ksh/zsh — pipefail is not POSIX) or
#      `set -eu` (POSIX sh, no pipefail support), OR carries a documented
#      opt-out comment of the form:
#
#          # no-pipefail: <reason>
#
#      Typical valid reason: the file is a sourced library (`. lib.sh` /
#      `source lib.sh`), and `set -e`/`pipefail` in a sourced file mutates
#      the *calling* shell's option state — a real footgun, not a style nit.
#
#   2. shellcheck, at error severity only, if shellcheck is installed. Error
#      severity was 0 across the repo at the time this gate was added, so
#      any error-level finding is a regression, not pre-existing debt.
#      Warning/info/style findings are NOT enforced here — ~294 pre-existing
#      warnings exist across scripts/*.sh; burning that down is separate
#      follow-up work (see ledger L9), not this gate's job.
#
# Files whose shebang is not a shell interpreter (e.g. a `.sh`-suffixed
# Python script) are skipped entirely — the check is shebang-driven, not
# extension-driven.
#
# Usage: ./scripts/check-shell-hardening.sh [scripts-dir]
#
# no-pipefail: this file's own shebang is bash and it does declare
# set -euo pipefail below; this header note exists only so a future reader
# grepping for "no-pipefail:" understands the convention from the canonical
# source. (Not itself an opt-out — see the `set` line immediately below.)

set -euo pipefail

SCRIPTS_DIR="${1:-scripts}"

if [[ ! -d "$SCRIPTS_DIR" ]]; then
  echo "check-shell-hardening: no such directory: $SCRIPTS_DIR" >&2
  exit 1
fi

have_shellcheck=0
if command -v shellcheck >/dev/null 2>&1; then
  have_shellcheck=1
else
  echo "check-shell-hardening: shellcheck not found on PATH — skipping shellcheck pass (pipefail check still enforced)" >&2
fi

fail=0
checked=0
skipped=0

is_shell_shebang() {
  # $1 = first line of file
  case "$1" in
    "#!"*"/sh"|"#!"*"/sh "*|"#!"*"/bash"|"#!"*"/bash "*|"#!"*"/env sh"|"#!"*"/env bash"|"#!"*"/env sh "*|"#!"*"/env bash "*|"#!"*"/zsh"|"#!"*"/env zsh")
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

# Only *tracked* files are checked (matches the ledger L15 scope — "every
# tracked .sh file"), resolved via `git ls-files` scoped to SCRIPTS_DIR's
# repo root so the result is correct regardless of whether SCRIPTS_DIR was
# passed as a relative or absolute path. Falls back to a plain find-based
# listing outside a git repo (e.g. a downloaded tarball checkout).
#
# Deliberately avoids `mapfile` (bash 4+) so this runs under macOS's
# default bash 3.2 as well as CI's modern bash — a `while read` loop over
# process substitution is bash-3.2-portable.
list_sh_files() {
  repo_root="$(git -C "$SCRIPTS_DIR" rev-parse --show-toplevel 2>/dev/null || true)"
  if [[ -n "$repo_root" ]]; then
    rel_scripts_dir="$(cd "$SCRIPTS_DIR" && pwd | sed "s#^$repo_root/*##")"
    git -C "$repo_root" ls-files -- "${rel_scripts_dir}/*.sh" | sed "s#^#$repo_root/#"
  else
    find "$SCRIPTS_DIR" -maxdepth 1 -name "*.sh" -type f | sort
  fi
}

while IFS= read -r f; do
  [[ -f "$f" ]] || continue
  first_line="$(head -n1 "$f")"

  if ! is_shell_shebang "$first_line"; then
    skipped=$((skipped + 1))
    continue
  fi

  checked=$((checked + 1))

  # Accept any of: `set -euo pipefail`, `set -eu` (POSIX sh has no
  # pipefail), or a documented `# no-pipefail: <reason>` opt-out. Matched
  # as plain substrings (not a permissive regex) to keep false-accepts out.
  if grep -Eq '^[[:space:]]*set[[:space:]]+-euo[[:space:]]+pipefail([[:space:]]|$)' "$f" \
     || grep -Eq '^[[:space:]]*set[[:space:]]+-eu([[:space:]]|$)' "$f" \
     || grep -q '# no-pipefail:' "$f"; then
    : # OK — either hardened or documented opt-out
  else
    echo "FAIL: $f — missing 'set -euo pipefail' (or 'set -eu' for POSIX sh) and no '# no-pipefail: <reason>' opt-out comment" >&2
    fail=1
  fi

  if [[ "$have_shellcheck" -eq 1 ]]; then
    if ! shellcheck -S error "$f"; then
      echo "FAIL: $f — shellcheck reported error-severity findings (see above)" >&2
      fail=1
    fi
  fi
done < <(list_sh_files)

echo "check-shell-hardening: checked $checked shell script(s), skipped $skipped non-shell-shebang .sh file(s)"

if [[ "$fail" -ne 0 ]]; then
  echo "check-shell-hardening: FAILED" >&2
  exit 1
fi

echo "check-shell-hardening: PASSED"

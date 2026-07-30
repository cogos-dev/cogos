#!/usr/bin/env bash
# scripts/lib/refuse-if-running.sh — the ONE shared implementation of the
# "refuse to overwrite a binary a running cogos daemon is executing" guard.
#
# Why this file exists (read before adding a sixth copy):
#
# PR #486 on this repo hand-copied this same detection logic into five
# separate places — the Makefile's install recipe, scripts/setup-dev.sh,
# scripts/setup.sh, and two README snippets — and took twelve review rounds
# to converge, because a fix landed in whichever copy the author happened to
# be looking at and the sibling copies silently kept the bug. See
# cog://mem/working/2026-07-30-self-review-spike/RETRO-486.md for the full
# post-mortem. Five hand-written copies with no shared implementation is the
# generator of that loop, independent of any individual bug in the
# detection logic itself. THIS FILE IS THE FIX FOR THE GENERATOR: every
# bash/sh caller sources or execs this file instead of inlining the check.
# Windows/PowerShell cannot source a bash file, so it has its own parallel
# implementation at scripts/lib/refuse-if-running.ps1 — see that file's
# header for the "keep in sync" contract between the two.
#
# ---- The fail-closed contract (this is the load-bearing rule) ----
#
# A detection predicate in this file must NEVER return "not running" when
# what it actually means is "I could not tell." "Unknown" and "not running"
# are different states, and conflating them was, by itself, four of the
# seventeen confirmed findings against PR #486 (RETRO-486.md Class B) — a
# missing `pgrep`, a process owned by another user, an unreadable /proc entry
# all fell through to "must not be running" instead of "refuse and say why."
# Every branch below that cannot positively resolve an answer refuses the
# install. If you add a new detection path, it must refuse on the
# inconclusive case, not proceed.
#
# ---- Usage ----
#
# Sourced (setup-dev.sh, setup.sh):
#   . "$(dirname "${BASH_SOURCE[0]}")/lib/refuse-if-running.sh"
#   refuse_if_running "$INSTALL_DIR/cogos" || exit 1
#
# Executed directly (the Makefile, whose recipe lines each run in their own
# subshell — sourcing a function into `make` doesn't carry across lines):
#   scripts/lib/refuse-if-running.sh "$(INSTALL_TARGET)"
#
# Returns/exits 0 if it is safe to proceed, 1 if refused (message already
# printed to stderr). Never removes or moves anything itself — detection
# only. The caller still performs the actual install.
#
# Environment:
#   ALLOW_RUNNING_INSTALL=1   Skip the check entirely (documented escape
#                             hatch, same variable name across every caller).
#
# no-pipefail: this file is a sourced library (see check-shell-hardening.sh's
# own documented exemption for that case) — `set -e`/`pipefail` here would
# mutate the *calling* script's shell options, not just this file's. The
# direct-execution branch at the bottom opts into `set -u` locally instead.

refuse_if_running() {
    local target="$1" rtarget pid exe cmdline is_serve tok pids pgrep_rc

    if [ "${ALLOW_RUNNING_INSTALL:-}" = "1" ]; then
        return 0
    fi

    # Nothing at the target path yet — nothing to clobber.
    [ -e "$target" ] || return 0

    # Fully resolve the target path itself, not just its parent directory —
    # if $target is a symlink, comparing an unresolved rtarget against the
    # exe side's fully-resolved readlink -f result (below) would never
    # match, and a symlinked install target would sail through the guard
    # even while a live process executes it via that symlink. `readlink -f`
    # is the SAME tool and flag used to resolve the exe side, so this stays
    # in lockstep with that comparison by construction rather than by two
    # independently-written canonicalizers agreeing by luck. Empty output
    # means resolution itself failed (a readlink without -f support, or a
    # broken symlink chain) — that is the inconclusive case, not "nothing
    # to resolve," so it refuses rather than comparing against "".
    rtarget="$(readlink -f "$target" 2>/dev/null || true)"
    if [ -z "$rtarget" ]; then
        _refuse_if_running_say \
            "cannot canonicalize the install target path $target." \
            "readlink -f failed to resolve it (broken symlink chain, or a readlink without -f support on this platform). Refusing rather than comparing against an unresolved path."
        return 1
    fi

    if ! command -v pgrep >/dev/null 2>&1; then
        _refuse_if_running_say \
            "pgrep not found, so a running kernel cannot be detected." \
            "Installing blind could overwrite a live production binary."
        return 1
    fi

    # Detection is two-stage, NOT a `pgrep -f 'cogos serve'` contiguous-
    # substring match: the `cog` wrapper (scripts/cog) execs the kernel as
    # `<kernel> --workspace <path> serve ...`, putting the workspace flag
    # between "cogos" and "serve" in argv, so a plain substring match never
    # fires for wrapper-started daemons. Instead: (1) pgrep on the binary
    # name only, anchored to a path/word boundary so it doesn't match an
    # unrelated binary that merely contains "cogos" as a substring (e.g.
    # cogos-channel-bridge); (2) for each candidate PID, pull its full argv
    # and check for a standalone "serve" word anywhere in it.
    # pgrep's own exit status distinguishes "ran fine, matched nothing"
    # (1 — conclusive, safe) from an actual pgrep failure (2 syntax error,
    # 3+ fatal error, e.g. /proc unreadable in a restricted container).
    # `|| true` alone would flatten that distinction to an empty $pids in
    # both cases — the exact "missing pgrep → || true swallows it → empty
    # pid list" shape RETRO-486 names at Makefile:138, just one layer
    # deeper (pgrep present but erroring, instead of absent). Status > 1 is
    # inconclusive and must refuse, not be read as "nothing is running."
    pids="$(pgrep -f '(^|/)cogos( |$)' 2>/dev/null)"
    pgrep_rc=$?
    if [ "$pgrep_rc" -gt 1 ]; then
        _refuse_if_running_say \
            "pgrep exited with status $pgrep_rc while searching for a running cogos process." \
            "Status 1 means 'no match' and is fine; anything higher means pgrep itself failed. Refusing rather than treating that failure as an empty, conclusive result."
        return 1
    fi

    for pid in $pids; do
        cmdline=""
        if [ -r "/proc/$pid/cmdline" ]; then
            cmdline="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)"
        fi
        if [ -z "$cmdline" ]; then
            cmdline="$(ps -o args= -p "$pid" 2>/dev/null || true)"
        fi

        # An empty cmdline here means "could not be determined" (neither
        # /proc nor ps produced anything), NOT "confirmed to have no argv."
        # pgrep already matched this PID against the cogos name/path
        # pattern, so it plausibly IS a cogos process; silently `continue`-
        # ing past it (as this used to do) means "not a serve process" gets
        # returned for a PID we never actually inspected — the fail-open
        # this whole file exists to prevent, and the literal RETRO-486
        # Class B shape: unknown treated as not-running. Refuse instead.
        if [ -z "$cmdline" ]; then
            _refuse_if_running_say \
                "cannot determine the command line of PID $pid, which matched the cogos process pattern." \
                "It may be running 'cogos serve' against $target. It likely belongs to another user or an unreadable /proc entry. Refusing rather than guessing whether it is a serve process."
            return 1
        fi

        is_serve=0
        for tok in $cmdline; do
            [ "$tok" = "serve" ] && is_serve=1
        done
        [ "$is_serve" = "1" ] || continue

        # Resolve the candidate PID's executable. An unresolved executable
        # is the inconclusive case the fail-closed contract above exists
        # for: it usually means the process belongs to another user (a
        # service account), so /proc/PID/exe and lsof are unreadable from
        # here. Refuse rather than guess — a wrong guess overwrites a live
        # production binary.
        #   Linux: /proc/PID/exe is the authoritative symlink.
        #   macOS: no /proc; `lsof -p PID` reports the text (executable)
        #          mapping.
        #   Fallback: `ps -o comm=` — note this yields a basename only on
        #          procps (Linux), never a full path, so the final `case`
        #          below treats a non-absolute result as unresolved rather
        #          than silently comparing a basename to a full path.
        exe=""
        if [ -r "/proc/$pid/exe" ]; then
            exe="$(readlink -f "/proc/$pid/exe" 2>/dev/null || true)"
        fi
        if [ -z "$exe" ] && command -v lsof >/dev/null 2>&1; then
            exe="$(lsof -p "$pid" -Ffn 2>/dev/null | awk '/^ftxt$/{t=1;next} /^n/{if(t){print substr($0,2);exit}} {t=0}')"
        fi
        if [ -z "$exe" ]; then
            exe="$(ps -o comm= -p "$pid" 2>/dev/null || true)"
        fi
        case "$exe" in /*) ;; *) exe="";; esac

        if [ -z "$exe" ]; then
            _refuse_if_running_say \
                "cannot determine the executable of PID $pid, which is running 'cogos serve'." \
                "It may be $target. It likely belongs to another user, so /proc and lsof are unreadable from here. Refusing rather than guessing."
            return 1
        fi

        if [ "$exe" = "$rtarget" ]; then
            _refuse_if_running_say \
                "$target is being executed by PID $pid." \
                "Installing over a running kernel's binary replaces production in place."
            return 1
        fi
    done

    return 0
}

_refuse_if_running_say() {
    echo "" >&2
    echo "REFUSING TO INSTALL: $1" >&2
    echo "$2" >&2
    echo "" >&2
    echo "Options:" >&2
    echo "  install to a different prefix/dir instead of overwriting this one" >&2
    echo "  ALLOW_RUNNING_INSTALL=1 <installer>   # override, only if you mean it" >&2
    echo "" >&2
}

# Direct-execution mode: `scripts/lib/refuse-if-running.sh <target>` runs the
# check and exits with its result, for callers (the Makefile) that cannot
# source a function into their own shell. Detected by comparing $0 against
# this file's own path, which is false when the file is sourced (in that
# case $0 is the sourcing script, not this file).
if [ "${BASH_SOURCE[0]:-}" = "${0:-}" ]; then
    set -u
    if [ "$#" -ne 1 ]; then
        echo "usage: $0 <install-target-path>" >&2
        exit 2
    fi
    refuse_if_running "$1"
    exit $?
fi

#!/usr/bin/env python3
"""CogOS post-turn capture hook (Stop event).

Closes the substrate's regenerative-reconciliation loop. Every Claude Code
turn dissipates kinetic state (PR counts changed, commits merged, memory
files written, dispatches launched) that the *next* turn's proprioception
block could otherwise harvest as free anticipatory signal. This hook fires
at turn-end and snapshots that state to a substrate-readable path so the
next UserPromptSubmit can inline it.

This is the "post-hook" companion to cogos_session_awareness.py's
UserPromptSubmit handler. Together they convert per-turn perturbation into
next-turn anticipatory signal (regenerative-braking analogy: don't dissipate
the kinetic state, harvest it).

Substrate-physics framing:
  - Pre-hook (UserPromptSubmit) is the substrate's *during-loop* surface.
  - Post-hook (Stop) is the substrate's *post-loop* surface.
  - Together they close the loop. Each turn's settled distinctions become
    the next turn's frame of reference (per substrate-physics-sequence
    cogdoc; lightning-through-sand only flows once tethers lead to ground).

Captured fields:
  - timestamp        : turn-end UTC time (RFC3339)
  - cwd              : working directory at capture time
  - branch           : git branch (if in a repo)
  - bright_line      : {open_prs_cogos, open_prs_mod3, recent_merges_24h}
  - memory           : {recent_writes_count, last_write_iso}
  - observatory      : {unindexed_session_bytes} (cheap heuristic)
  - consolidation    : {pressure} ("low"/"moderate"/"high" — heuristic)

Write location (substrate-side, fits existing .cog/run/ pattern):
  ${COGOS_WORKSPACE}/.cog/run/proprioception/post-turn.json

If COGOS_WORKSPACE is not set and ~/workspaces/cog/.cog/ doesn't exist,
falls back to ~/.claude/state/proprioception/post-turn.json.

Safety contract: never raises; always exits 0; total wall time budget < 3s.
gh / git invocations are individually time-bounded; failure of any one
field just leaves that key out of the captured state (next turn gets
whatever was reachable).

Install:
  cp scripts/hooks/cogos_post_turn_capture.py ~/.claude/hooks/

Wire in ~/.claude/settings.json (Stop event):
  {
    "Stop": [{"matcher": "*", "hooks": [{"type": "command",
      "command": "python3 ~/.claude/hooks/cogos_post_turn_capture.py"}]}]
  }

The companion UserPromptSubmit reader lives in cogos_session_awareness.py
(handle_user_prompt_submit; reads this file and inlines a <cogos_post_turn>
block in additionalContext).
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

# ---------- Path resolution ------------------------------------------------


def _cog_workspace() -> Path | None:
    """Return cog workspace root with .cog/ subdir, or None."""
    candidates = []
    env = os.environ.get("COGOS_WORKSPACE")
    if env:
        candidates.append(Path(env))
    candidates.append(Path.home() / "workspaces" / "cog")

    for c in candidates:
        try:
            if (c / ".cog").is_dir():
                return c.resolve()
        except OSError:
            continue
    return None


def _state_path() -> Path:
    """Return the canonical write path for post-turn state.

    Prefers substrate-side (.cog/run/proprioception/post-turn.json) when the
    cog workspace is reachable; falls back to operator-side state dir.
    """
    cog = _cog_workspace()
    if cog is not None:
        return cog / ".cog" / "run" / "proprioception" / "post-turn.json"
    return Path.home() / ".claude" / "state" / "proprioception" / "post-turn.json"


# ---------- Field collectors (each individually bounded) -------------------


def _run(cmd: list[str], cwd: str | None = None, timeout: float = 1.5) -> str | None:
    try:
        r = subprocess.run(
            cmd, capture_output=True, text=True, timeout=timeout, cwd=cwd
        )
        if r.returncode == 0:
            return r.stdout
    except Exception:
        return None
    return None


def _git_branch(cwd: str) -> str | None:
    out = _run(["git", "rev-parse", "--abbrev-ref", "HEAD"], cwd=cwd, timeout=1.0)
    if out is None:
        return None
    b = out.strip()
    return b or None


def _gh_open_pr_count(repo: str) -> int | None:
    """Count open PRs on a repo via gh. Repo like 'myrgic/cogos'."""
    out = _run(
        ["gh", "pr", "list", "--repo", repo, "--state", "open",
         "--json", "number", "--limit", "100"],
        timeout=2.0,
    )
    if out is None:
        return None
    try:
        arr = json.loads(out)
        return len(arr) if isinstance(arr, list) else None
    except Exception:
        return None


def _gh_recent_merges(repo: str, hours: int = 24) -> int | None:
    """Count PRs merged in the last N hours."""
    out = _run(
        ["gh", "pr", "list", "--repo", repo, "--state", "merged",
         "--json", "mergedAt", "--limit", "50"],
        timeout=2.0,
    )
    if out is None:
        return None
    try:
        arr = json.loads(out)
        if not isinstance(arr, list):
            return None
        cutoff = time.time() - hours * 3600
        n = 0
        for pr in arr:
            merged = pr.get("mergedAt")
            if not merged:
                continue
            try:
                t = datetime.fromisoformat(merged.replace("Z", "+00:00")).timestamp()
            except Exception:
                continue
            if t >= cutoff:
                n += 1
        return n
    except Exception:
        return None


def _memory_recent_writes(window_seconds: int = 3600) -> dict[str, Any]:
    """Count and find latest mtime under user memory dir within window."""
    base = Path.home() / ".claude" / "projects" / "-Users-slowbro" / "memory"
    info: dict[str, Any] = {"recent_writes_count": 0, "last_write_iso": None}
    try:
        if not base.is_dir():
            return info
        cutoff = time.time() - window_seconds
        latest = 0.0
        count = 0
        for p in base.iterdir():
            if not p.is_file():
                continue
            try:
                m = p.stat().st_mtime
            except OSError:
                continue
            if m >= cutoff:
                count += 1
            if m > latest:
                latest = m
        info["recent_writes_count"] = count
        if latest > 0:
            info["last_write_iso"] = datetime.fromtimestamp(
                latest, tz=timezone.utc
            ).strftime("%Y-%m-%dT%H:%MZ")
    except Exception:
        pass
    return info


def _observatory_unindexed_bytes() -> int | None:
    """Heuristic: total size of session JSONL files modified in last 30 min.

    Cheap proxy for ingest pressure until the Conversations Observatory
    is wired. Counts JSONL files in the recent-activity window — the
    substrate-side Observatory reconciler can replace this with a real
    indexed-through-watermark when it lands (cogos#300).
    """
    base = Path.home() / ".claude" / "projects" / "-Users-slowbro"
    try:
        if not base.is_dir():
            return None
        cutoff = time.time() - 1800  # 30 min
        total = 0
        for p in base.iterdir():
            if not p.is_file() or p.suffix != ".jsonl":
                continue
            try:
                st = p.stat()
                if st.st_mtime >= cutoff:
                    total += st.st_size
            except OSError:
                continue
        return total
    except Exception:
        return None


def _consolidation_pressure(turn_count: int | None = None) -> str:
    """Heuristic. Real foveation engine will replace this.

    For now: "low" if no recent memory writes, "moderate" if some, "high"
    if many recent writes (a busy session is more in need of consolidation).
    """
    info = _memory_recent_writes(window_seconds=3600)
    n = info.get("recent_writes_count", 0) or 0
    if n >= 5:
        return "high"
    if n >= 1:
        return "moderate"
    return "low"


# ---------- Capture orchestration ------------------------------------------


def capture(stdin_data: dict[str, Any]) -> dict[str, Any]:
    """Build the post-turn state blob.

    Each field is best-effort; failure on one doesn't block the others.
    """
    cwd = stdin_data.get("cwd") or os.getcwd()
    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    out: dict[str, Any] = {
        "timestamp": now,
        "cwd": cwd,
        "session_id": stdin_data.get("session_id"),
        "stop_reason": stdin_data.get("stop_reason"),
    }

    branch = _git_branch(cwd)
    if branch:
        out["branch"] = branch

    bright_line: dict[str, Any] = {}
    cogos_open = _gh_open_pr_count("myrgic/cogos")
    mod3_open = _gh_open_pr_count("myrgic/mod3")
    cogos_merges = _gh_recent_merges("myrgic/cogos", hours=24)
    if cogos_open is not None:
        bright_line["open_prs_cogos"] = cogos_open
    if mod3_open is not None:
        bright_line["open_prs_mod3"] = mod3_open
    if cogos_merges is not None:
        bright_line["recent_merges_24h_cogos"] = cogos_merges
    if bright_line:
        out["bright_line"] = bright_line

    mem = _memory_recent_writes()
    if mem.get("recent_writes_count") or mem.get("last_write_iso"):
        out["memory"] = mem

    unindexed = _observatory_unindexed_bytes()
    if unindexed is not None:
        out["observatory"] = {"unindexed_session_bytes": unindexed}

    out["consolidation"] = {"pressure": _consolidation_pressure()}

    return out


def write_state(state: dict[str, Any]) -> Path | None:
    """Atomically write state to the canonical location. Returns path."""
    target = _state_path()
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        tmp = target.with_suffix(".json.tmp")
        with open(tmp, "w") as f:
            json.dump(state, f, indent=2)
        os.replace(tmp, target)
        return target
    except Exception as e:
        sys.stderr.write(f"cogos_post_turn_capture: write failed: {e}\n")
        return None


# ---------- Entry point ----------------------------------------------------


def main() -> int:
    try:
        raw = sys.stdin.read()
        data = json.loads(raw) if raw.strip() else {}
    except Exception:
        data = {}

    # Only act on Stop. Be permissive on missing event name; some installs
    # may invoke us directly. SubagentStop is a sibling event we treat the
    # same way (sub-agent turn-end is also a moment with capturable state).
    event = data.get("hook_event_name", "Stop")
    if event not in ("Stop", "SubagentStop"):
        # Not our event — silent no-op.
        print(json.dumps({}))
        return 0

    try:
        state = capture(data)
        path = write_state(state)
    except Exception as e:
        sys.stderr.write(f"cogos_post_turn_capture: capture failed: {e}\n")
        path = None

    # Stop hooks may return decision/continue/stopReason fields; we just
    # return empty so the agent's stop decision is unchanged.
    out: dict[str, Any] = {}
    if path is not None:
        # Surface where state was written for debugging via stderr; the
        # agent's stdout JSON contract stays minimal.
        sys.stderr.write(f"cogos_post_turn_capture: state -> {path}\n")
    print(json.dumps(out))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as e:
        sys.stderr.write(f"cogos_post_turn_capture: unexpected error: {e}\n")
        try:
            print(json.dumps({}))
        except Exception:
            pass
        sys.exit(0)

#!/usr/bin/env python3
"""CogOS session awareness hook.

Auto-registers Claude Code sessions (and their dispatched sub-agents) with
the local CogOS kernel and injects peer-awareness packets into prompts, so
concurrent sessions become passively aware of each other via the bus.

Dispatches on hook_event_name (received as stdin JSON):
  SessionStart:    POST /v1/sessions/register  (role: claude-code)
  SubAgentStart:   POST /v1/sessions/register  (role: claude-code-subagent,
                   parent: parent session_id)
  UserPromptSubmit: POST /v1/sessions/{id}/heartbeat, then GET /v1/peer-awareness;
                   ALSO reads post-turn state (written by cogos_post_turn_capture.py
                   at Stop) and inlines <cogos_post_turn> as anticipatory signal.
  Stop:            Delegates to cogos_post_turn_capture.py to snapshot
                   bright-line + memory + observatory + consolidation state.
                   Closes the regenerative-reconciliation loop: per-turn kinetic
                   state becomes next-turn anticipatory signal instead of
                   dissipating.
  SessionEnd:      POST /v1/sessions/{id}/heartbeat {status: ending}
  SubAgentEnd:     POST /v1/sessions/{id}/end    (closes sub-agent registration)
                   Also invokes post-turn capture so sub-agent completions
                   contribute to parent's next-turn proprioception.

Silent on failure: if the kernel isn't running or the cwd isn't a CogOS
workspace, the hook exits 0 without blocking the session.

Install:
  cp scripts/hooks/cogos_session_awareness.py ~/.claude/hooks/
  cp scripts/hooks/cogos_post_turn_capture.py ~/.claude/hooks/

Wire in ~/.claude/settings.local.json:
  {
    "SessionStart": [{"hooks": [{"type": "command",
      "command": "python3 ~/.claude/hooks/cogos_session_awareness.py",
      "async": true}]}],
    "SubagentStart": [{"hooks": [{"type": "command",
      "command": "python3 ~/.claude/hooks/cogos_session_awareness.py",
      "async": true}]}],
    "UserPromptSubmit": [{"hooks": [{"type": "command",
      "command": "python3 ~/.claude/hooks/cogos_session_awareness.py"}]}],
    "Stop": [{"hooks": [{"type": "command",
      "command": "python3 ~/.claude/hooks/cogos_post_turn_capture.py",
      "async": true}]}],
    "SessionEnd": [{"hooks": [{"type": "command",
      "command": "python3 ~/.claude/hooks/cogos_session_awareness.py",
      "async": true}]}],
    "SubagentStop": [{"hooks": [{"type": "command",
      "command": "python3 ~/.claude/hooks/cogos_session_awareness.py",
      "async": true}]}]
  }
"""
import json
import os
import socket
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Optional

KERNEL_URL = os.environ.get("COGOS_KERNEL_URL", "http://127.0.0.1:6931")
TIMEOUT = 1.5
ROLE = "claude-code"
SUBAGENT_ROLE = "claude-code-subagent"


def _find_workspace(cwd: str) -> Optional[str]:
    p = Path(cwd).resolve()
    for cand in [p, *p.parents]:
        if (cand / ".cog" / "config").is_dir():
            return str(cand)
    return None


def _post(path: str, body: dict) -> Optional[dict]:
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        f"{KERNEL_URL}{path}",
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            return json.loads(r.read())
    except (urllib.error.URLError, socket.timeout, json.JSONDecodeError, OSError):
        return None


def _get(path: str) -> Optional[dict]:
    try:
        with urllib.request.urlopen(f"{KERNEL_URL}{path}", timeout=TIMEOUT) as r:
            return json.loads(r.read())
    except (urllib.error.URLError, socket.timeout, json.JSONDecodeError, OSError):
        return None


def handle_session_start(inp: dict) -> dict:
    sid = inp.get("session_id")
    cwd = inp.get("cwd") or os.getcwd()
    if not sid:
        return {}
    workspace = _find_workspace(cwd)
    if not workspace:
        return {}
    body = {
        "session_id": sid,
        "workspace": workspace,
        "role": ROLE,
        "hostname": socket.gethostname(),
        "status": "active",
    }
    src = inp.get("source")
    if src:
        body["extras"] = {"source": src}
    _post("/v1/sessions/register", body)
    return {}


def handle_subagent_start(inp: dict) -> dict:
    """Register a dispatched Claude Code sub-agent as a cogos session.

    The sub-agent inherits the workspace from its parent session and registers
    with role 'claude-code-subagent'. The parent session_id is recorded in
    extras so cog_list_sessions can filter by parent.
    """
    sid = inp.get("session_id")
    parent_sid = inp.get("parent_session_id")
    cwd = inp.get("cwd") or os.getcwd()
    if not sid:
        return {}
    workspace = _find_workspace(cwd)
    if not workspace:
        return {}
    task = inp.get("task") or inp.get("prompt", "")[:120]
    body = {
        "session_id": sid,
        "workspace": workspace,
        "role": SUBAGENT_ROLE,
        "hostname": socket.gethostname(),
        "status": "active",
        "task": task,
    }
    extras: dict = {}
    if parent_sid:
        extras["parent_session_id"] = parent_sid
    src = inp.get("source")
    if src:
        extras["source"] = src
    if extras:
        body["extras"] = extras
    _post("/v1/sessions/register", body)
    return {}


def _read_post_turn_state() -> Optional[dict]:
    """Read the most recent post-turn state blob written by
    cogos_post_turn_capture.py (Stop hook). Returns None if missing or
    unreadable. Substrate-side path preferred; falls back to operator-side.

    This is the regenerative-reconciliation read path: turn-end-captured
    state flows into the next turn's UserPromptSubmit as anticipatory
    signal. See cogos_post_turn_capture.py for the writer.
    """
    candidates = []
    env = os.environ.get("COGOS_WORKSPACE")
    if env:
        candidates.append(Path(env) / ".cog" / "run" / "proprioception" / "post-turn.json")
    candidates.append(Path.home() / "workspaces" / "cog" / ".cog" / "run" /
                      "proprioception" / "post-turn.json")
    candidates.append(Path.home() / ".claude" / "state" / "proprioception" / "post-turn.json")

    for c in candidates:
        try:
            if c.is_file():
                with open(c) as f:
                    return json.load(f)
        except (OSError, json.JSONDecodeError):
            continue
    return None


def _format_post_turn_block(state: dict) -> str:
    """Format the post-turn state as a terse multi-line block."""
    lines = ["<cogos_post_turn>"]
    ts = state.get("timestamp")
    if ts:
        lines.append(f"  captured={ts}")
    bl = state.get("bright_line") or {}
    if bl:
        parts = []
        if "open_prs_cogos" in bl:
            parts.append(f"cogos_open={bl['open_prs_cogos']}")
        if "open_prs_mod3" in bl:
            parts.append(f"mod3_open={bl['open_prs_mod3']}")
        if "recent_merges_24h_cogos" in bl:
            parts.append(f"cogos_merged_24h={bl['recent_merges_24h_cogos']}")
        if parts:
            lines.append("  bright_line: " + " | ".join(parts))
    mem = state.get("memory") or {}
    if mem.get("recent_writes_count") or mem.get("last_write_iso"):
        parts = []
        if "recent_writes_count" in mem:
            parts.append(f"writes_1h={mem['recent_writes_count']}")
        if mem.get("last_write_iso"):
            parts.append(f"last_write={mem['last_write_iso']}")
        lines.append("  memory: " + " | ".join(parts))
    obs = state.get("observatory") or {}
    if "unindexed_session_bytes" in obs:
        lines.append(f"  observatory: unindexed_bytes={obs['unindexed_session_bytes']}")
    cons = state.get("consolidation") or {}
    if "pressure" in cons:
        lines.append(f"  consolidation: pressure={cons['pressure']}")
    lines.append("</cogos_post_turn>")
    return "\n".join(lines)


def handle_user_prompt_submit(inp: dict) -> dict:
    sid = inp.get("session_id")
    cwd = inp.get("cwd") or os.getcwd()
    if not sid or not _find_workspace(cwd):
        return {}
    _post(f"/v1/sessions/{sid}/heartbeat", {"status": "active"})

    # Compose additionalContext from two sources: peer-awareness (live from
    # kernel) and post-turn state (snapshot from previous Stop hook). Either
    # may be absent; we emit whichever is present.
    blocks: list[str] = []

    resp = _get(f"/v1/peer-awareness?sid={sid}")
    if resp:
        packet = (resp.get("packet") or "").strip()
        if packet and resp.get("token_count", 0) >= 5:
            blocks.append(f"<cogos_peer_awareness>\n{packet}\n</cogos_peer_awareness>")

    post_turn = _read_post_turn_state()
    if post_turn:
        blocks.append(_format_post_turn_block(post_turn))

    if not blocks:
        return {}

    return {
        "hookSpecificOutput": {
            "hookEventName": "UserPromptSubmit",
            "additionalContext": "\n".join(blocks),
        }
    }


def handle_session_end(inp: dict) -> dict:
    sid = inp.get("session_id")
    cwd = inp.get("cwd") or os.getcwd()
    if not sid or not _find_workspace(cwd):
        return {}
    _post(f"/v1/sessions/{sid}/heartbeat", {"status": "ending"})
    return {}


def handle_subagent_end(inp: dict) -> dict:
    """Close the sub-agent's cogos session registration on completion.

    Also invokes the post-turn capture so sub-agent completions contribute
    to next-turn proprioception (regenerative-reconciliation closure).
    """
    sid = inp.get("session_id")
    cwd = inp.get("cwd") or os.getcwd()
    if sid and _find_workspace(cwd):
        outcome = inp.get("outcome") or "completed"
        _post(f"/v1/sessions/{sid}/end", {"reason": outcome})
    # Also capture post-turn state.
    _invoke_post_turn_capture(inp)
    return {}


def _invoke_post_turn_capture(inp: dict) -> None:
    """Delegate to cogos_post_turn_capture.py (Stop hook) if installed.

    The post-turn capture is the post-loop substrate surface — it snapshots
    bright-line state, memory writes, observatory pressure, and consolidation
    pressure at turn-end so the next UserPromptSubmit can inline it. We
    delegate via subprocess rather than importing so the two scripts stay
    independently installable.
    """
    capture = Path.home() / ".claude" / "hooks" / "cogos_post_turn_capture.py"
    if not capture.is_file():
        return
    try:
        import subprocess  # local import — only used on this path
        subprocess.run(
            [sys.executable, str(capture)],
            input=json.dumps(inp).encode(),
            capture_output=True,
            timeout=3.0,
        )
    except Exception:
        pass


def handle_stop(inp: dict) -> dict:
    """Stop event handler — capture post-turn state for next-turn proprioception.

    Returns empty dict (Stop hooks should not influence the agent's stop
    decision unless explicitly requested). All capture work happens via
    cogos_post_turn_capture.py.
    """
    _invoke_post_turn_capture(inp)
    return {}


HANDLERS = {
    "SessionStart": handle_session_start,
    # Claude Code uses lowercase 'a' in the hook event names for subagent events.
    "SubagentStart": handle_subagent_start,
    "UserPromptSubmit": handle_user_prompt_submit,
    "SessionEnd": handle_session_end,
    "SubagentStop": handle_subagent_end,
    "Stop": handle_stop,
}


def main() -> int:
    try:
        inp = json.load(sys.stdin)
    except json.JSONDecodeError:
        return 0
    handler = HANDLERS.get(inp.get("hook_event_name", ""))
    if not handler:
        return 0
    try:
        out = handler(inp)
    except Exception as e:  # noqa: BLE001 — never fail the agent on hook error
        sys.stderr.write(f"cogos_session_awareness: {e}\n")
        return 0
    if out:
        json.dump(out, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())

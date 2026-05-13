#!/usr/bin/env python3
"""CogOS session awareness hook.

Auto-registers Claude Code sessions (and their dispatched sub-agents) with
the local CogOS kernel and injects peer-awareness packets into prompts, so
concurrent sessions become passively aware of each other via the bus.

Dispatches on hook_event_name (received as stdin JSON):
  SessionStart:    POST /v1/sessions/register  (role: claude-code)
  SubAgentStart:   POST /v1/sessions/register  (role: claude-code-subagent,
                   parent: parent session_id)
  UserPromptSubmit: POST /v1/sessions/{id}/heartbeat, then GET /v1/peer-awareness
                   and prepend the packet as additionalContext.
  SessionEnd:      POST /v1/sessions/{id}/heartbeat {status: ending}
  SubAgentEnd:     POST /v1/sessions/{id}/end    (closes sub-agent registration)

Silent on failure: if the kernel isn't running or the cwd isn't a CogOS
workspace, the hook exits 0 without blocking the session.

Install:
  cp scripts/hooks/cogos_session_awareness.py ~/.claude/hooks/

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


def handle_user_prompt_submit(inp: dict) -> dict:
    sid = inp.get("session_id")
    cwd = inp.get("cwd") or os.getcwd()
    if not sid or not _find_workspace(cwd):
        return {}
    _post(f"/v1/sessions/{sid}/heartbeat", {"status": "active"})
    resp = _get(f"/v1/peer-awareness?sid={sid}")
    if not resp:
        return {}
    packet = (resp.get("packet") or "").strip()
    if not packet or resp.get("token_count", 0) < 5:
        return {}
    block = f"<cogos_peer_awareness>\n{packet}\n</cogos_peer_awareness>"
    return {
        "hookSpecificOutput": {
            "hookEventName": "UserPromptSubmit",
            "additionalContext": block,
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
    """Close the sub-agent's cogos session registration on completion."""
    sid = inp.get("session_id")
    cwd = inp.get("cwd") or os.getcwd()
    if not sid or not _find_workspace(cwd):
        return {}
    outcome = inp.get("outcome") or "completed"
    _post(f"/v1/sessions/{sid}/end", {"reason": outcome})
    return {}


HANDLERS = {
    "SessionStart": handle_session_start,
    # Claude Code uses lowercase 'a' in the hook event names for subagent events.
    "SubagentStart": handle_subagent_start,
    "UserPromptSubmit": handle_user_prompt_submit,
    "SessionEnd": handle_session_end,
    "SubagentStop": handle_subagent_end,
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

#!/usr/bin/env python3
"""cogblock.py — parse and render .cog.md files as CogBlock semantic trees.

A .cog.md file is itself a valid CogBlock serialization. This script exposes
the implicit block structure (frontmatter + sections) as a parsed tree and
provides deterministic round-trip rendering back to markdown.

Usage:
    cogblock.py parse <file>            # emit semantic tree as JSON
    cogblock.py render <tree.json>      # emit markdown from JSON tree
    cogblock.py roundtrip <file>        # parse → render, diff with original
    cogblock.py roundtrip-glob <dir>    # roundtrip every .cog.md under dir

The semantic tree is {frontmatter: dict, blocks: [Block]} where Block is:
    {type: "preamble" | "title" | "section", level: 1|2, heading: str, body: str}

Blocks preserve section-body markdown verbatim (sub-headings, lists, code
blocks, tables included as raw text). Only H1/H2 boundaries are structural;
H3+ stay inside their parent section.
"""

import argparse
import datetime as _dt
import json
import re
import sys
from pathlib import Path

import yaml

FRONTMATTER_RE = re.compile(r"^---\n(.*?)\n---\n?(.*)$", re.DOTALL)
HEADING_LINE_RE = re.compile(r"^(#{1,6}) +(.+?)[ \t]*$")
FENCE_RE = re.compile(r"^[ \t]{0,3}(```+|~~~+)")


def _find_heading_boundaries(body: str) -> list[tuple[int, int, int, str, str]]:
    """Walk body line by line, returning (start, end_of_line, level, heading, raw_line)
    for each H1/H2 heading that is NOT inside a fenced code block.
    end_of_line is the position of the trailing newline (or len(body) for EOF)."""
    boundaries: list[tuple[int, int, int, str, str]] = []
    in_fence = False
    fence_marker: str | None = None

    pos = 0
    while pos < len(body):
        nl = body.find("\n", pos)
        line_end = nl if nl != -1 else len(body)
        line = body[pos:line_end]

        fm = FENCE_RE.match(line)
        if fm:
            marker = fm.group(1)[:3]
            if in_fence and fence_marker and marker == fence_marker:
                in_fence = False
                fence_marker = None
            elif not in_fence:
                in_fence = True
                fence_marker = marker
        elif not in_fence:
            hm = HEADING_LINE_RE.match(line)
            if hm:
                level = len(hm.group(1))
                if level <= 2:
                    boundaries.append((pos, line_end, level, hm.group(2).strip(), line))

        pos = line_end + 1 if nl != -1 else line_end

    return boundaries


def _json_default(o):
    if isinstance(o, (_dt.date, _dt.datetime)):
        return o.isoformat()
    raise TypeError(f"not JSON serializable: {type(o).__name__}")


def parse(text: str) -> dict:
    """Parse .cog.md text into {frontmatter, frontmatter_raw, blocks}.

    frontmatter_raw is the original YAML text verbatim; render() emits it
    unchanged if present so byte-for-byte round-trips work. Consumers that
    mutate frontmatter should clear frontmatter_raw to force re-serialization.
    """
    match = FRONTMATTER_RE.match(text)
    if match:
        frontmatter_text = match.group(1)
        body = match.group(2)
        frontmatter = yaml.safe_load(frontmatter_text) or {}
        frontmatter_raw = frontmatter_text
    else:
        frontmatter = {}
        frontmatter_raw = None
        body = text

    boundaries = _find_heading_boundaries(body)

    blocks: list[dict] = []

    if boundaries:
        preamble_text = body[: boundaries[0][0]]
        if preamble_text:
            blocks.append({"type": "preamble", "body": preamble_text})
    else:
        if body:
            blocks.append({"type": "preamble", "body": body})

    for i, (_start, line_end, level, heading, raw) in enumerate(boundaries):
        next_start = boundaries[i + 1][0] if i + 1 < len(boundaries) else len(body)
        # line_end is the position of the heading line's trailing newline (or EOF).
        # Advance past it so body starts with the line after the heading.
        body_start = line_end + 1 if line_end < len(body) and body[line_end] == "\n" else line_end
        section_body = body[body_start:next_start]
        block_type = "title" if level == 1 else "section"
        blocks.append(
            {
                "type": block_type,
                "level": level,
                "heading": heading,
                "heading_raw": raw,
                "body": section_body,
            }
        )

    return {
        "frontmatter": frontmatter,
        "frontmatter_raw": frontmatter_raw,
        "blocks": blocks,
    }


def render(tree: dict) -> str:
    """Render {frontmatter, blocks} back to .cog.md text — byte-exact.

    No whitespace normalization: bodies are emitted verbatim with whatever
    leading/trailing whitespace they were parsed with. Round-trip preserves
    bytes for unmodified trees.
    """
    out: list[str] = []
    raw = tree.get("frontmatter_raw")
    fm = tree.get("frontmatter") or {}
    if raw is not None:
        out.append("---\n")
        out.append(raw)
        if not raw.endswith("\n"):
            out.append("\n")
        out.append("---\n")
    elif fm:
        out.append("---\n")
        out.append(yaml.safe_dump(fm, sort_keys=False, allow_unicode=True))
        out.append("---\n")

    for block in tree.get("blocks", []):
        btype = block["type"]
        if btype == "preamble":
            out.append(block["body"])
        elif btype in ("title", "section"):
            level = block.get("level", 2)
            heading = block["heading"]
            body = block.get("body", "")
            heading_raw = block.get("heading_raw")
            if heading_raw is not None:
                out.append(heading_raw + "\n")
            else:
                out.append("#" * level + " " + heading + "\n")
            out.append(body)

    result = "".join(out)
    if not result.endswith("\n"):
        result += "\n"
    return result


def cmd_parse(args: argparse.Namespace) -> int:
    text = Path(args.file).read_text(encoding="utf-8")
    tree = parse(text)
    json.dump(tree, sys.stdout, indent=2, ensure_ascii=False, default=_json_default)
    sys.stdout.write("\n")
    return 0


def cmd_render(args: argparse.Namespace) -> int:
    tree = json.loads(Path(args.file).read_text(encoding="utf-8"))
    sys.stdout.write(render(tree))
    return 0


def _diff_summary(orig: str, rendered: str, label: str) -> str:
    import difflib

    diff = list(
        difflib.unified_diff(
            orig.splitlines(keepends=True),
            rendered.splitlines(keepends=True),
            fromfile=label,
            tofile=f"{label}.rendered",
            lineterm="",
            n=2,
        )
    )
    return "".join(diff)


def _normalize_trailing(s: str) -> str:
    return s.rstrip("\n") + "\n"


def cmd_roundtrip(args: argparse.Namespace) -> int:
    path = Path(args.file)
    orig = path.read_text(encoding="utf-8")
    tree = parse(orig)
    rendered = render(tree)
    if _normalize_trailing(orig) == _normalize_trailing(rendered):
        sys.stderr.write(f"OK: {args.file}\n")
        return 0
    sys.stdout.write(_diff_summary(orig, rendered, str(path)))
    sys.stderr.write(f"DIFFERED: {args.file}\n")
    return 1


def cmd_roundtrip_glob(args: argparse.Namespace) -> int:
    root = Path(args.dir)
    files = sorted(root.rglob("*.cog.md"))
    ok = diff = 0
    diffs: list[str] = []
    for f in files:
        try:
            orig = f.read_text(encoding="utf-8")
            tree = parse(orig)
            rendered = render(tree)
            if _normalize_trailing(orig) == _normalize_trailing(rendered):
                ok += 1
            else:
                diff += 1
                diffs.append(str(f))
        except Exception as e:
            diff += 1
            diffs.append(f"{f} [ERROR: {e}]")
    total = ok + diff
    sys.stderr.write(f"ROUNDTRIP: {ok}/{total} clean, {diff}/{total} differed\n")
    if diffs and args.list_diffs:
        for d in diffs[: args.list_diffs]:
            sys.stderr.write(f"  {d}\n")
    return 0 if diff == 0 else 1


def main() -> int:
    p = argparse.ArgumentParser(description="Parse/render .cog.md as CogBlock trees")
    sub = p.add_subparsers(dest="cmd", required=True)

    sp = sub.add_parser("parse", help="parse a .cog.md to JSON tree")
    sp.add_argument("file")
    sp.set_defaults(func=cmd_parse)

    sr = sub.add_parser("render", help="render a JSON tree to .cog.md")
    sr.add_argument("file")
    sr.set_defaults(func=cmd_render)

    srt = sub.add_parser("roundtrip", help="parse → render, diff against original")
    srt.add_argument("file")
    srt.set_defaults(func=cmd_roundtrip)

    sg = sub.add_parser(
        "roundtrip-glob", help="roundtrip every .cog.md under a directory"
    )
    sg.add_argument("dir")
    sg.add_argument(
        "--list-diffs",
        type=int,
        default=20,
        help="show up to N diffed paths (default 20)",
    )
    sg.set_defaults(func=cmd_roundtrip_glob)

    args = p.parse_args()
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())

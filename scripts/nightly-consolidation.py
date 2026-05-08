#!/usr/bin/env python3
"""Nightly Consolidation — closed-loop TRM improvement.

Runs during dormant hours. Extracts training signals from the day's
Claude Code sessions, updates the embedding index, retrains the TRM,
deploys new weights, and writes a morning report.

The control loop:
    LOG     → extract (query, docs_read, outcome) from session traces
    INDEX   → re-embed any new/changed CogDocs
    LEARN   → retrain TRM with accumulated signals
    DEPLOY  → export weights, restart kernel
    REPORT  → write morning briefing as CogDoc

Usage:
    python3 scripts/nightly-consolidation.py              # full cycle
    python3 scripts/nightly-consolidation.py --dry-run    # report only, no deploy
    python3 scripts/nightly-consolidation.py --stage log  # run one stage

Designed to run via cron at 2am or triggered by kernel dormant state.
"""

import argparse
import json
import os
import signal
import subprocess
import sys
import time
from collections import Counter, defaultdict
from datetime import datetime, timedelta
from pathlib import Path

# ── Paths ────────────────────────────────────────────────────────────────────

WORKSPACE = Path(os.environ.get("WORKSPACE", os.path.expanduser("~/workspaces/cog")))
COGOS_DEV = Path(os.environ.get("COGOS_DEV", os.path.join(os.environ.get("MYRGIC_REPOS_ROOT", os.path.expanduser("~/workspaces/myrgic")), "cogos")))
AUTORESEARCH = Path(os.environ.get("AUTORESEARCH", os.path.expanduser("~/cog-workspace/apps/cogos-v3/autoresearch")))
CLAUDE_DIR = Path.home() / ".claude" / "projects"
KERNEL_PORT = int(os.environ.get("KERNEL_PORT", "6931"))

REPORT_DIR = WORKSPACE / ".cog" / "mem" / "episodic" / "journal"
NIGHTLY_LOG = COGOS_DEV / "scripts" / "nightly.log"

# ── Logging ──────────────────────────────────────────────────────────────────

def log(msg: str):
    ts = datetime.now().strftime("%H:%M:%S")
    line = f"[{ts}] {msg}"
    print(line, flush=True)
    with open(NIGHTLY_LOG, "a") as f:
        f.write(line + "\n")


def run_log(lookback_hours: int = 24) -> dict:
    """Extract training signals using idempotent extractor."""
    log("── Stage: LOG ──")

    extractor = COGOS_DEV / "scripts" / "extract-signals.py"
    result = subprocess.run(
        [sys.executable, str(extractor), "--source", "claude"],
        capture_output=True, text=True, timeout=300,
        cwd=str(COGOS_DEV))

    output = result.stdout
    log(output.strip().split("\n")[-1] if output.strip() else "no output")

    # Parse stats from output
    stats = {"sessions_parsed": 0, "signals_extracted": 0, "signals_total": 0, "outcomes": {}}
    for line in output.split("\n"):
        if line.startswith("New signals:"):
            try:
                stats["signals_extracted"] = int(line.split(":")[1].split("(")[0].strip())
            except (ValueError, IndexError):
                pass
        if line.startswith("Total signals:"):
            try:
                stats["signals_total"] = int(line.split(":")[1].strip())
            except (ValueError, IndexError):
                pass
        if line.startswith("Sessions processed:"):
            try:
                stats["sessions_parsed"] = int(line.split(":")[1].strip())
            except (ValueError, IndexError):
                pass
        if "outcomes:" in line.lower() and "{" in line:
            try:
                stats["outcomes"] = json.loads(line.split(":", 1)[1].strip().replace("'", '"'))
            except (json.JSONDecodeError, IndexError):
                pass

    log(f"Signals: {stats['signals_extracted']} new, {stats['signals_total']} total")
    return stats


# ── Stage 2: INDEX — Update embedding index ─────────────────────────────────

def run_index() -> dict:
    """Re-embed any new/changed CogDocs."""
    log("── Stage: INDEX ──")

    result = subprocess.run(
        [str(AUTORESEARCH / ".venv" / "bin" / "python"), "embed_index.py",
         "--workspace", str(WORKSPACE)],
        capture_output=True, text=True, timeout=1800,
        cwd=str(AUTORESEARCH))

    output = result.stdout + result.stderr
    log(f"embed_index exit={result.returncode}")

    # Parse stats from output
    stats = {"new": 0, "updated": 0, "unchanged": 0, "total_chunks": 0}
    for line in output.split("\n"):
        if "Changes:" in line:
            for part in line.split(","):
                part = part.strip()
                for key in ["new", "updated", "unchanged"]:
                    if key in part:
                        try:
                            stats[key] = int(part.split()[0])
                        except (ValueError, IndexError):
                            pass
        if "chunks" in line.lower() and "x" in line:
            try:
                stats["total_chunks"] = int(line.split()[1])
            except (ValueError, IndexError):
                pass

    log(f"Index: {stats['new']} new, {stats['updated']} updated, {stats['total_chunks']} chunks")
    return stats


# ── Stage 3: LEARN — Retrain TRM ────────────────────────────────────────────

def run_learn() -> dict:
    """Prepare data and retrain TRM."""
    log("── Stage: LEARN ──")

    # Step 1: Prepare training data
    log("Preparing training data...")
    prep = subprocess.run(
        [str(AUTORESEARCH / ".venv" / "bin" / "python"), "prepare.py",
         "--workspace", str(WORKSPACE)],
        capture_output=True, text=True, timeout=600,
        cwd=str(AUTORESEARCH))

    baseline_ndcg = 0.0
    for line in prep.stdout.split("\n"):
        if "baseline NDCG" in line:
            try:
                baseline_ndcg = float(line.split(":")[-1].strip())
            except ValueError:
                pass

    log(f"Prepare done. Cosine baseline: {baseline_ndcg:.4f}")

    # Step 2: Train
    log("Training MambaTRM...")
    train = subprocess.run(
        [str(AUTORESEARCH / ".venv" / "bin" / "python"), "train_mamba.py"],
        capture_output=True, text=True, timeout=600,
        cwd=str(AUTORESEARCH))

    final_ndcg = 0.0
    delta = 0.0
    for line in train.stdout.split("\n"):
        if "Final NDCG" in line:
            try:
                final_ndcg = float(line.split(":")[-1].strip())
            except ValueError:
                pass
        if "Delta:" in line:
            try:
                delta = float(line.split(":")[-1].strip().replace("pts", "").strip().lstrip("+"))
            except ValueError:
                pass

    log(f"Training done. NDCG: {final_ndcg:.4f} (delta: +{delta:.1f})")

    return {
        "baseline_ndcg": baseline_ndcg,
        "final_ndcg": final_ndcg,
        "delta": delta,
    }


# ── Stage 4: DEPLOY — Export weights and restart kernel ──────────────────────

def run_deploy(dry_run: bool = False) -> dict:
    """Export TRM weights and restart kernel."""
    log("── Stage: DEPLOY ──")

    # Export
    log("Exporting weights...")
    export = subprocess.run(
        [str(AUTORESEARCH / ".venv" / "bin" / "python"), "trm_export.py",
         "--output-dir", str(AUTORESEARCH)],
        capture_output=True, text=True, timeout=120,
        cwd=str(AUTORESEARCH))

    weights_size = 0
    emb_size = 0
    chunks_count = 0
    for line in export.stdout.split("\n"):
        if "trm_weights.bin" in line:
            try:
                weights_size = float(line.split()[-2])
            except (ValueError, IndexError):
                pass
        if "trm_embeddings.bin" in line:
            try:
                emb_size = float(line.split()[-2])
            except (ValueError, IndexError):
                pass
        if "chunks copied" in line:
            try:
                chunks_count = int(line.split()[0])
            except (ValueError, IndexError):
                pass

    log(f"Export done. Weights: {weights_size:.1f}MB, Embeddings: {emb_size:.1f}MB, Chunks: {chunks_count}")

    if dry_run:
        log("DRY RUN — skipping kernel restart")
        return {"exported": True, "restarted": False, "chunks": chunks_count}

    # Restart kernel
    log("Restarting kernel...")
    subprocess.run(["pkill", "-f", f"cogos serve.*{KERNEL_PORT}"],
                   capture_output=True, timeout=5)
    time.sleep(2)

    cogos_bin = COGOS_DEV / "cogos"
    if not cogos_bin.exists():
        log(f"WARNING: cogos binary not found at {cogos_bin}")
        return {"exported": True, "restarted": False, "chunks": chunks_count}

    subprocess.Popen(
        [str(cogos_bin), "serve", "--workspace", str(WORKSPACE), "--port", str(KERNEL_PORT)],
        stdout=open("/tmp/cogos-kernel.log", "a"),
        stderr=subprocess.STDOUT,
        cwd=str(COGOS_DEV),
        start_new_session=True)

    # Wait for health
    time.sleep(4)
    try:
        from urllib.request import urlopen
        with urlopen(f"http://localhost:{KERNEL_PORT}/health", timeout=5) as r:
            health = json.loads(r.read())
            log(f"Kernel healthy: {health.get('status')}")
    except Exception as e:
        log(f"WARNING: Kernel health check failed: {e}")

    return {"exported": True, "restarted": True, "chunks": chunks_count}


# ── Stage 5: REPORT — Write morning briefing ────────────────────────────────

def run_report(log_stats: dict, index_stats: dict, learn_stats: dict,
               deploy_stats: dict) -> Path:
    """Write morning report as a CogDoc."""
    log("── Stage: REPORT ──")

    today = datetime.now().strftime("%Y-%m-%d")
    report_path = REPORT_DIR / f"{today}-nightly-consolidation.cog.md"

    # Build report
    signals_total = log_stats.get("signals_total", 0)
    outcomes = log_stats.get("outcomes", {})
    ndcg = learn_stats.get("final_ndcg", 0)
    baseline = learn_stats.get("baseline_ndcg", 0)
    idx_new = index_stats.get("new", 0)
    idx_updated = index_stats.get("updated", 0)
    chunks = deploy_stats.get("chunks", 0)

    report = f"""---
title: "Nightly Consolidation — {today}"
type: episodic
created: {datetime.now().strftime("%Y-%m-%dT%H:%M:%SZ")}
tags: [consolidation, trm, training, nightly]
status: integrated
---

# Nightly Consolidation — {today}

## Summary

The nightly consolidation loop ran at {datetime.now().strftime("%H:%M")}. Here's what happened:

### Signal Extraction
- Sessions parsed: **{log_stats.get('sessions_parsed', 0)}**
- New signals extracted: **{log_stats.get('signals_extracted', 0)}**
- Cumulative signals: **{signals_total}**
- Outcomes: {', '.join(f'{k}={v}' for k, v in outcomes.items())}

### Embedding Index
- New docs embedded: **{idx_new}**
- Updated docs: **{idx_updated}**
- Total chunks: **{index_stats.get('total_chunks', 0)}**

### TRM Training
- Cosine baseline NDCG@10: **{baseline:.4f}**
- Trained TRM NDCG@10: **{ndcg:.4f}**
- Delta: **+{learn_stats.get('delta', 0):.1f} pts**

### Deployment
- Weights exported: **{'yes' if deploy_stats.get('exported') else 'no'}**
- Kernel restarted: **{'yes' if deploy_stats.get('restarted') else 'no (dry run)'}**
- Index chunks deployed: **{chunks}**

## What This Means

"""
    # Add interpretation
    if ndcg > baseline:
        report += f"The TRM learned something useful — NDCG improved from {baseline:.3f} to {ndcg:.3f}. "
    else:
        report += f"Training didn't improve over cosine baseline ({ndcg:.3f} vs {baseline:.3f}). "
        report += "This could mean the current training data is saturated or the signal quality needs improvement. "

    n_accept = outcomes.get("accept", 0)
    n_correct = outcomes.get("correct", 0)
    if n_correct > n_accept * 3:
        report += f"\nNote: correction signals ({n_correct}) vastly outnumber accepts ({n_accept}). "
        report += "The outcome heuristic may be over-counting corrections. Consider refining the detection. "

    if idx_new > 50:
        report += f"\n\n{idx_new} new CogDocs were embedded — significant workspace growth detected. "

    report += "\n"

    REPORT_DIR.mkdir(parents=True, exist_ok=True)
    report_path.write_text(report)
    log(f"Report written to {report_path}")

    return report_path


# ── Main ─────────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(description="Nightly Consolidation Loop")
    parser.add_argument("--dry-run", action="store_true",
                        help="Run everything except kernel restart")
    parser.add_argument("--stage", choices=["log", "index", "learn", "deploy", "report"],
                        help="Run a single stage only")
    parser.add_argument("--lookback", type=int, default=24,
                        help="Hours of session history to scan (default: 24)")
    args = parser.parse_args()

    start = time.time()
    log("╔══════════════════════════════════════════╗")
    log("║  Nightly Consolidation                   ║")
    log(f"║  Workspace: {WORKSPACE}")
    log(f"║  Lookback:  {args.lookback}h")
    log(f"║  Dry run:   {args.dry_run}")
    log("╚══════════════════════════════════════════╝")

    log_stats = {"sessions_parsed": 0, "signals_extracted": 0, "signals_total": 0, "outcomes": {}}
    index_stats = {"new": 0, "updated": 0, "total_chunks": 0}
    learn_stats = {"baseline_ndcg": 0, "final_ndcg": 0, "delta": 0}
    deploy_stats = {"exported": False, "restarted": False, "chunks": 0}

    try:
        if args.stage in (None, "log"):
            log_stats = run_log(args.lookback)

        if args.stage in (None, "index"):
            index_stats = run_index()

        if args.stage in (None, "learn"):
            learn_stats = run_learn()

        if args.stage in (None, "deploy"):
            deploy_stats = run_deploy(dry_run=args.dry_run)

        if args.stage in (None, "report"):
            report_path = run_report(log_stats, index_stats, learn_stats, deploy_stats)

    except Exception as e:
        log(f"FATAL: {e}")
        import traceback
        traceback.print_exc()

    elapsed = time.time() - start
    log(f"Done in {elapsed:.0f}s")


if __name__ == "__main__":
    main()

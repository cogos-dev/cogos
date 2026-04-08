#!/usr/bin/env python3
"""Foveated Context Eval — fixed evaluation harness for the autoresearch loop.

DO NOT MODIFY THIS FILE. It is the fixed evaluation, like prepare.py in the TRM loop.

Runs 15 workspace questions through 3 conditions (stock, RAG, foveated),
captures debug snapshots from the kernel, scores by keyword recall,
computes context NDCG, and reports differentials.
"""

import json
import math
import os
import subprocess
import sys
import time
from pathlib import Path
from urllib.request import Request, urlopen

OLLAMA = os.environ.get("OLLAMA_HOST", "http://localhost:11434")
KERNEL = os.environ.get("KERNEL_HOST", "http://localhost:6931")
WORKSPACE = os.environ.get("WORKSPACE", "/Users/slowbro/workspaces/cog")
MODEL = os.environ.get("MODEL", "gemma4:26b")

# ── Question Bank ────────────────────────────────────────────────────────────
# Each question has: expected answer terms + ideal source files (for context NDCG)

QUESTIONS = [
    {"id": "ws-01", "q": "What are the four process states in the CogOS kernel?",
     "expect": ["active", "receptive", "consolidating", "dormant"],
     "ideal_sources": ["process.go", "config.go"]},
    {"id": "ws-02", "q": "What is the default port for the CogOS kernel?",
     "expect": ["6931"],
     "ideal_sources": ["config.go", "cli.go"]},
    {"id": "ws-03", "q": "What is the TRM's D_STATE value?",
     "expect": ["4"],
     "ideal_sources": ["trm_context.go", "program.md"]},
    {"id": "ws-04", "q": "What four zones does the foveated context engine use?",
     "expect": ["nucleus", "knowledge", "history", "current"],
     "ideal_sources": ["context_assembly.go", "serve_foveated.go"]},
    {"id": "ws-05", "q": "What hash algorithm does the CogOS ledger use?",
     "expect": ["sha-256", "sha256"],
     "ideal_sources": ["ledger.go"]},
    {"id": "ws-06", "q": "What is the default local model in CogOS?",
     "expect": ["gemma4", "e4b"],
     "ideal_sources": ["config.go"]},
    {"id": "ws-07", "q": "How many attention heads does the TRM's best config use?",
     "expect": ["16"],
     "ideal_sources": ["results.tsv", "program.md"]},
    {"id": "ws-08", "q": "What does the sovereignty gradient determine?",
     "expect": ["local", "provider", "route", "fallback"],
     "ideal_sources": ["router.go"]},
    {"id": "ws-09", "q": "What is the ConstellationBridge used for?",
     "expect": ["heartbeat", "trust", "constellation"],
     "ideal_sources": ["constellation_bridge.go"]},
    {"id": "ws-10", "q": "What three mechanisms does LoRO unify?",
     "expect": ["ple", "lora", "trm"],
     "ideal_sources": ["ple-lora-trm-convergence.cog.md", "framework.md"]},
    {"id": "ws-11", "q": "What paper inspired the CogOS TRM?",
     "expect": ["jolicoeur", "samsung", "tiny recursive"],
     "ideal_sources": ["trm-paper-details.md", "thesis.md"]},
    {"id": "ws-12", "q": "What two signals drive foveated rendering?",
     "expect": ["iris", "foveal", "where", "how much"],
     "ideal_sources": ["serve_foveated.go", "context_assembly.go"]},
    {"id": "ws-13", "q": "What is the EA/EFM thesis in one sentence?",
     "expect": ["externalized", "attention", "executive", "substrate"],
     "ideal_sources": ["externalized-executive-function-thesis.cog.md", "thesis.md"]},
    {"id": "ws-14", "q": "How does the tool-call hallucination gate work?",
     "expect": ["validate", "tool", "reject", "unknown"],
     "ideal_sources": ["tool_loop.go"]},
    {"id": "ws-15", "q": "What is the modality bus in mod3?",
     "expect": ["modality", "bus", "voice", "translate"],
     "ideal_sources": ["bus.py", "modality.py", "ARCHITECTURE.md"]},
]


# ── API Helpers ──────────────────────────────────────────────────────────────

def ollama_chat(model: str, messages: list, temp: float = 0.1) -> str:
    data = json.dumps({
        "model": model, "messages": messages,
        "stream": False,
        "options": {
            "temperature": temp,
            "num_predict": 256,    # short answers, no rambling
        },
        # Disable thinking/reasoning mode — the substrate reasons, not the model
        "think": False,
    }).encode()
    req = Request(f"{OLLAMA}/api/chat", data=data,
                  headers={"Content-Type": "application/json"})
    try:
        with urlopen(req, timeout=120) as resp:
            return json.loads(resp.read())["message"]["content"]
    except Exception as e:
        return f"ERROR: {e}"


def kernel_foveated(prompt: str) -> tuple[str, dict]:
    """Returns (context_string, debug_metadata)."""
    data = json.dumps({
        "prompt": prompt, "iris": {"size": 128000, "used": 5000},
        "profile": "default",
    }).encode()
    req = Request(f"{KERNEL}/v1/context/foveated", data=data,
                  headers={"Content-Type": "application/json"})
    try:
        with urlopen(req, timeout=15) as resp:
            body = json.loads(resp.read())
            inner = body.get("data", body)
            context = inner.get("context", "")
            meta = {
                "tokens": inner.get("tokens", 0),
                "anchor": inner.get("anchor", ""),
                "blocks": inner.get("blocks", []),
                "tier_breakdown": inner.get("tier_breakdown", {}),
                "iris_pressure": inner.get("iris_pressure", 0),
            }
            return context, meta
    except Exception:
        return "", {}


def kernel_debug_last() -> dict:
    """Get debug snapshot of the last request."""
    req = Request(f"{KERNEL}/v1/debug/last",
                  headers={"Content-Type": "application/json"})
    try:
        with urlopen(req, timeout=10) as resp:
            return json.loads(resp.read())
    except Exception:
        return {}


def rag_search(query: str) -> str:
    keywords = [w for w in query.lower().split() if len(w) > 3][:5]
    pattern = "|".join(keywords)
    try:
        r = subprocess.run(
            ["grep", "-rli", "-E", pattern,
             f"{WORKSPACE}/.cog/mem/", f"{WORKSPACE}/apps/cogos-v3/"],
            capture_output=True, text=True, timeout=5)
        files = [f for f in r.stdout.strip().split("\n") if f.strip()][:5]
        parts = []
        for f in files:
            try:
                parts.append(f"--- {Path(f).name} ---\n{Path(f).read_text()[:2000]}")
            except Exception:
                pass
        return "\n\n".join(parts)
    except Exception:
        return ""


# ── Scoring ──────────────────────────────────────────────────────────────────

def keyword_score(response: str, expected: list[str]) -> float:
    resp = response.lower()
    found = sum(1 for t in expected if t.lower() in resp)
    return found / len(expected) if expected else 0.0


def context_ndcg(assembled_sources: list[str], ideal_sources: list[str]) -> float:
    """NDCG of the assembled document list against the ideal sources.

    Measures: did the foveated engine select the RIGHT documents?
    Independent of what the model did with them.
    """
    if not ideal_sources or not assembled_sources:
        return 0.0

    # Relevance: 1 if the assembled source matches any ideal source, 0 otherwise
    relevances = []
    for src in assembled_sources:
        src_base = Path(src).name.lower() if "/" in src else src.lower()
        rel = 1.0 if any(ideal.lower() in src_base or src_base in ideal.lower()
                         for ideal in ideal_sources) else 0.0
        relevances.append(rel)

    # DCG
    dcg = sum(rel / math.log2(i + 2) for i, rel in enumerate(relevances))

    # Ideal DCG (all relevant docs at the top)
    ideal_rels = sorted(relevances, reverse=True)
    idcg = sum(rel / math.log2(i + 2) for i, rel in enumerate(ideal_rels))

    return dcg / idcg if idcg > 0 else 0.0


# ── Run Conditions ───────────────────────────────────────────────────────────

def run_stock(question: str) -> tuple[str, dict]:
    response = ollama_chat(MODEL, [{"role": "user", "content": question}])
    return response, {}


def run_rag(question: str) -> tuple[str, dict]:
    ctx = rag_search(question)
    if not ctx:
        return run_stock(question)
    response = ollama_chat(MODEL, [
        {"role": "system", "content": f"Context:\n\n{ctx}"},
        {"role": "user", "content": question},
    ])
    return response, {"rag_context_chars": len(ctx)}


def run_foveated(question: str) -> tuple[str, dict]:
    ctx, meta = kernel_foveated(question)
    if not ctx:
        response = ollama_chat(MODEL, [{"role": "user", "content": question}])
        return response, {"foveated_fallback": True}
    response = ollama_chat(MODEL, [
        {"role": "system", "content": f"Workspace context (CogOS foveated):\n\n{ctx}"},
        {"role": "user", "content": question},
    ])
    # Get debug snapshot
    debug = kernel_debug_last()
    meta["debug"] = debug
    return response, meta


# ── Main ─────────────────────────────────────────────────────────────────────

def main():
    # Verify kernel
    try:
        with urlopen(f"{KERNEL}/health", timeout=2) as r:
            health = json.loads(r.read())
            print(f"kernel: {health.get('status', '?')}")
    except Exception:
        print("WARNING: kernel not available, foveated will fall back to stock")

    print(f"model: {MODEL}")
    print(f"questions: {len(QUESTIONS)}")
    print()

    results = {"A": [], "B": [], "C": []}
    ndcg_scores = []
    all_details = []

    for qi, q in enumerate(QUESTIONS):
        print(f"── Q{qi+1}/{len(QUESTIONS)}: {q['id']} ──")
        print(f"   {q['q'][:70]}...")

        for cond_id, cond_name, cond_fn in [("A", "stock", run_stock),
                                              ("B", "rag", run_rag),
                                              ("C", "foveated", run_foveated)]:
            t0 = time.time()
            response, meta = cond_fn(q["q"])
            elapsed = round(time.time() - t0, 1)
            sc = keyword_score(response, q["expect"])
            results[cond_id].append(sc)

            # Context NDCG for foveated
            c_ndcg = 0.0
            assembled_sources = []
            if cond_id == "C" and meta.get("blocks"):
                for block in meta["blocks"]:
                    for src in block.get("sources", []):
                        assembled_sources.append(src.get("uri", src.get("path", "")))
                c_ndcg = context_ndcg(assembled_sources, q["ideal_sources"])
                ndcg_scores.append(c_ndcg)

            marker = "✓" if sc >= 0.5 else "✗"
            extra = ""
            if cond_id == "C":
                tokens = meta.get("tokens", 0)
                anchor = meta.get("anchor", "")
                extra = f" tokens={tokens} anchor='{anchor}' ctx_ndcg={c_ndcg:.2f}"
            print(f"   {cond_id}({cond_name:8s}): {sc:.3f} {marker} ({elapsed}s){extra}")

            all_details.append({
                "question_id": q["id"],
                "condition": cond_id,
                "score": sc,
                "elapsed": elapsed,
                "response_preview": response[:200],
                "meta": {k: v for k, v in meta.items() if k != "debug"},
                "context_ndcg": c_ndcg if cond_id == "C" else None,
                "assembled_sources": assembled_sources if cond_id == "C" else [],
                "ideal_sources": q["ideal_sources"],
            })

        print()

    # ── Summary ──────────────────────────────────────────────────────────

    a_avg = sum(results["A"]) / len(results["A"])
    b_avg = sum(results["B"]) / len(results["B"])
    c_avg = sum(results["C"]) / len(results["C"])
    c_minus_a = c_avg - a_avg
    c_minus_b = c_avg - b_avg
    avg_ndcg = sum(ndcg_scores) / len(ndcg_scores) if ndcg_scores else 0.0

    print("═══════════════════════════════════════")
    print(f"stock_avg:       {a_avg:.6f}")
    print(f"rag_avg:         {b_avg:.6f}")
    print(f"foveated_avg:    {c_avg:.6f}")
    print(f"c_minus_a:       {c_minus_a:.6f}")
    print(f"c_minus_b:       {c_minus_b:.6f}")
    print(f"context_ndcg:    {avg_ndcg:.6f}")
    print(f"total_questions: {len(QUESTIONS)}")
    print(f"model:           {MODEL}")
    print("═══════════════════════════════════════")

    # Per-question breakdown
    print("\nPer-question C-A differential:")
    for qi, q in enumerate(QUESTIONS):
        diff = results["C"][qi] - results["A"][qi]
        marker = "▲" if diff > 0.05 else "▼" if diff < -0.05 else "="
        print(f"  {q['id']}: C={results['C'][qi]:.2f} A={results['A'][qi]:.2f} diff={diff:+.3f} {marker}")

    # Save details
    details_file = Path("eval-details.json")
    with open(details_file, "w") as f:
        json.dump(all_details, f, indent=2)
    print(f"\nDetails saved to {details_file}")


if __name__ == "__main__":
    main()

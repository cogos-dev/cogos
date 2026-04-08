#!/usr/bin/env python3
"""Overnight Ablation: RAG vs Foveated vs Tools vs Full Stack.

Four conditions, same model, same questions. Measures what each
CogOS capability layer contributes to answer quality.

Condition A: Stock (no context)     — baseline, matches published benchmarks
Condition B: RAG (embedding search) — naive context stuffing
Condition C: Foveated only          — CogOS context assembly, no tools
Condition D: Foveated + Tools       — full CogOS stack

Two question sets:
1. MMLU-Pro sample: verify stock model matches published scores (control)
2. Workspace QA: questions whose answers live in the cogdocs (the real test)

Usage:
    python3 scripts/overnight-ablation.py                    # run all
    python3 scripts/overnight-ablation.py --conditions A,B   # specific conditions
    python3 scripts/overnight-ablation.py --questions workspace  # workspace only
    python3 scripts/overnight-ablation.py --model gemma4:e4b    # different model
"""

import argparse
import hashlib
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from urllib.request import Request, urlopen
from urllib.error import URLError

# ── Config ───────────────────────────────────────────────────────────────────

OLLAMA = os.environ.get("OLLAMA_HOST", "http://localhost:11434")
KERNEL = os.environ.get("KERNEL_HOST", "http://localhost:6931")
WORKSPACE = os.environ.get("WORKSPACE", "/Users/slowbro/workspaces/cog")
OUTPUT_DIR = os.environ.get("OUTPUT_DIR", "/tmp/ablation-overnight")

# ── Workspace QA Set ─────────────────────────────────────────────────────────
# Questions whose answers are in the cogdocs. Ground truth is known.

WORKSPACE_QA = [
    {
        "id": "ws-01",
        "question": "What are the four process states in the CogOS kernel?",
        "answer_contains": ["active", "receptive", "consolidating", "dormant"],
        "source": "internal/engine/process.go",
    },
    {
        "id": "ws-02",
        "question": "What is the default port for the CogOS kernel HTTP API?",
        "answer_contains": ["6931"],
        "source": "internal/engine/config.go",
    },
    {
        "id": "ws-03",
        "question": "What is the TRM's D_STATE value used in CogOS?",
        "answer_contains": ["4"],
        "source": "autoresearch/program.md",
    },
    {
        "id": "ws-04",
        "question": "What four context zones does the foveated engine use?",
        "answer_contains": ["nucleus", "knowledge", "history", "current"],
        "source": "internal/engine/context_assembly.go",
    },
    {
        "id": "ws-05",
        "question": "What hash algorithm does the CogOS ledger use?",
        "answer_contains": ["sha-256", "sha256"],
        "source": "internal/engine/ledger.go",
    },
    {
        "id": "ws-06",
        "question": "What is the name of the tool-call validation function in the CogOS kernel?",
        "answer_contains": ["validatetoolcall", "validate_tool_call", "ValidateToolCall"],
        "source": "internal/engine/tool_loop.go",
    },
    {
        "id": "ws-07",
        "question": "What is the default local model configured in CogOS?",
        "answer_contains": ["gemma4", "e4b"],
        "source": "internal/engine/config.go",
    },
    {
        "id": "ws-08",
        "question": "How many attention heads does the TRM use in its current best configuration?",
        "answer_contains": ["16"],
        "source": "autoresearch/results.tsv",
    },
    {
        "id": "ws-09",
        "question": "What does the sovereignty gradient in CogOS determine?",
        "answer_contains": ["local", "provider", "route", "cloud", "fallback"],
        "source": "internal/engine/router.go",
    },
    {
        "id": "ws-10",
        "question": "What protocol does the CogOS MCP server use?",
        "answer_contains": ["mcp", "model context protocol", "streamable http"],
        "source": "internal/engine/mcp_server.go",
    },
    {
        "id": "ws-11",
        "question": "What is the ConstellationBridge interface used for in CogOS?",
        "answer_contains": ["heartbeat", "trust", "peer", "constellation"],
        "source": "internal/engine/constellation_bridge.go",
    },
    {
        "id": "ws-12",
        "question": "What are the two signals that drive foveated rendering in the OpenClaw plugin?",
        "answer_contains": ["iris", "foveal", "where", "how much"],
        "source": "openclaw-plugin/cogos/index.ts",
    },
    {
        "id": "ws-13",
        "question": "What is LoRO and what three mechanisms does it unify?",
        "answer_contains": ["low-rank observer", "ple", "lora", "trm"],
        "source": "research/loro/framework.md",
    },
    {
        "id": "ws-14",
        "question": "What paper inspired the TRM in CogOS, and who authored it?",
        "answer_contains": ["jolicoeur", "samsung", "tiny recursive"],
        "source": "research/eaefm/thesis.md",
    },
    {
        "id": "ws-15",
        "question": "How many skills are in the cogos-dev/skills marketplace?",
        "answer_contains": ["17", "18"],
        "source": "skills/README.md",
    },
]

# ── MMLU-Pro Sample (stock control) ──────────────────────────────────────────
# A small sample of MMLU-Pro style questions to verify the model matches
# published performance. These are general knowledge, no workspace context.

MMLU_SAMPLE = [
    {
        "id": "mmlu-01",
        "question": "In machine learning, what does the bias-variance tradeoff describe?",
        "choices": [
            "A) The tradeoff between model complexity and training speed",
            "B) The tradeoff between underfitting and overfitting",
            "C) The tradeoff between training data size and model accuracy",
            "D) The tradeoff between precision and recall",
        ],
        "answer": "B",
    },
    {
        "id": "mmlu-02",
        "question": "What is the time complexity of binary search on a sorted array of n elements?",
        "choices": ["A) O(1)", "B) O(n)", "C) O(log n)", "D) O(n log n)"],
        "answer": "C",
    },
    {
        "id": "mmlu-03",
        "question": "In distributed systems, what does the CAP theorem state?",
        "choices": [
            "A) A system can achieve consistency, availability, and partition tolerance simultaneously",
            "B) A system can achieve at most two of consistency, availability, and partition tolerance",
            "C) A system must sacrifice consistency for partition tolerance",
            "D) A system must sacrifice availability for consistency",
        ],
        "answer": "B",
    },
    {
        "id": "mmlu-04",
        "question": "What is the primary purpose of attention mechanisms in transformer architectures?",
        "choices": [
            "A) To reduce the number of parameters in the model",
            "B) To allow the model to focus on relevant parts of the input",
            "C) To speed up training by parallelizing computation",
            "D) To prevent overfitting during training",
        ],
        "answer": "B",
    },
    {
        "id": "mmlu-05",
        "question": "In cryptography, what property does a hash function's collision resistance guarantee?",
        "choices": [
            "A) It is impossible to find any two inputs that produce the same output",
            "B) It is computationally infeasible to find two distinct inputs with the same output",
            "C) The output is always unique for each input",
            "D) The function cannot be reversed to find the original input",
        ],
        "answer": "B",
    },
]

# ── API Helpers ──────────────────────────────────────────────────────────────

def ollama_chat(model: str, messages: list, temperature: float = 0.1) -> str:
    """Send a chat request to Ollama and return the response text."""
    data = json.dumps({
        "model": model,
        "messages": messages,
        "stream": False,
        "options": {"temperature": temperature},
    }).encode()
    req = Request(f"{OLLAMA}/api/chat", data=data,
                  headers={"Content-Type": "application/json"})
    try:
        with urlopen(req, timeout=120) as resp:
            return json.loads(resp.read())["message"]["content"]
    except Exception as e:
        return f"ERROR: {e}"


def kernel_foveated(prompt: str) -> str:
    """Get foveated context from CogOS kernel."""
    data = json.dumps({
        "prompt": prompt,
        "iris": {"size": 128000, "used": 5000},
        "profile": "default",
    }).encode()
    req = Request(f"{KERNEL}/v1/context/foveated", data=data,
                  headers={"Content-Type": "application/json"})
    try:
        with urlopen(req, timeout=15) as resp:
            body = json.loads(resp.read())
            return body.get("data", body).get("context", "")
    except Exception:
        return ""


def embedding_search(query: str, n: int = 5) -> str:
    """Naive RAG: search workspace by running grep on cogdocs."""
    # Simple keyword search as RAG proxy (no embedding server needed)
    keywords = [w for w in query.lower().split() if len(w) > 3][:5]
    pattern = "|".join(keywords)
    try:
        result = subprocess.run(
            ["grep", "-rli", "-E", pattern,
             f"{WORKSPACE}/.cog/mem/", f"{WORKSPACE}/apps/cogos-v3/"],
            capture_output=True, text=True, timeout=5
        )
        files = result.stdout.strip().split("\n")[:n]
        context_parts = []
        for f in files:
            if not f.strip():
                continue
            try:
                text = Path(f).read_text()[:2000]
                context_parts.append(f"--- {Path(f).name} ---\n{text}")
            except Exception:
                pass
        return "\n\n".join(context_parts)
    except Exception:
        return ""


# ── Evaluation ───────────────────────────────────────────────────────────────

def score_workspace_answer(response: str, expected: list[str]) -> float:
    """Score a workspace QA answer: fraction of expected terms found."""
    response_lower = response.lower()
    found = sum(1 for term in expected if term.lower() in response_lower)
    return found / len(expected) if expected else 0.0


def score_mmlu_answer(response: str, correct: str) -> float:
    """Score an MMLU answer: 1.0 if correct letter found, 0.0 otherwise."""
    # Look for the answer letter in the response
    response_upper = response.upper().strip()
    # Check for explicit "B)" or "Answer: B" patterns
    for pattern in [f"{correct})", f"answer: {correct}", f"answer is {correct}",
                    f"correct answer is {correct}"]:
        if pattern.lower() in response.lower():
            return 1.0
    # Check if response starts with the letter
    if response_upper.startswith(correct):
        return 1.0
    return 0.0


# ── Conditions ───────────────────────────────────────────────────────────────

def run_condition_a(model: str, question: str) -> str:
    """Stock: no context, just the question."""
    return ollama_chat(model, [
        {"role": "user", "content": question},
    ])


def run_condition_b(model: str, question: str) -> str:
    """RAG: keyword search + stuff into context."""
    context = embedding_search(question)
    if not context:
        return run_condition_a(model, question)
    return ollama_chat(model, [
        {"role": "system", "content": f"Use the following context to answer the question:\n\n{context}"},
        {"role": "user", "content": question},
    ])


def run_condition_c(model: str, question: str) -> str:
    """Foveated only: CogOS context assembly, no tools."""
    context = kernel_foveated(question)
    if not context:
        return run_condition_a(model, question)
    return ollama_chat(model, [
        {"role": "system", "content": f"Workspace context (assembled by CogOS foveated engine):\n\n{context}"},
        {"role": "user", "content": question},
    ])


def run_condition_d(model: str, question: str) -> str:
    """Foveated + Tools: full stack via sandbox agent."""
    # Run the sandbox agent and capture its output
    try:
        result = subprocess.run(
            [sys.executable, "scripts/sandbox-agent.py",
             "--model", model, "--sandbox", "read-only",
             "--max-turns", "4", "--workspace", WORKSPACE, question],
            capture_output=True, text=True, timeout=180,
            cwd=str(Path(__file__).parent.parent),
        )
        return result.stdout + result.stderr
    except subprocess.TimeoutExpired:
        return "TIMEOUT"
    except Exception as e:
        return f"ERROR: {e}"


CONDITIONS = {
    "A": ("Stock (no context)", run_condition_a),
    "B": ("RAG (keyword search)", run_condition_b),
    "C": ("Foveated only", run_condition_c),
    "D": ("Foveated + Tools", run_condition_d),
}

# ── Main ─────────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(description="Overnight ablation study")
    parser.add_argument("--model", default=os.environ.get("MODEL", "gemma4:26b"))
    parser.add_argument("--conditions", default="A,B,C,D")
    parser.add_argument("--questions", default="all", choices=["all", "workspace", "mmlu"])
    args = parser.parse_args()

    conditions = [c.strip() for c in args.conditions.split(",")]
    out_dir = Path(OUTPUT_DIR)
    out_dir.mkdir(parents=True, exist_ok=True)

    # Check kernel availability for conditions C/D
    kernel_available = False
    if "C" in conditions or "D" in conditions:
        try:
            with urlopen(f"{KERNEL}/health", timeout=2):
                kernel_available = True
        except Exception:
            pass
        if not kernel_available:
            print(f"WARNING: CogOS kernel not available at {KERNEL}")
            print("  Conditions C and D will fall back to stock (no context)")
            print("  Start kernel: ./cogos serve --workspace ... --port 6931")
            print()

    # Build question sets
    questions = []
    if args.questions in ("all", "mmlu"):
        for q in MMLU_SAMPLE:
            choices_text = "\n".join(q["choices"])
            questions.append({
                "id": q["id"],
                "type": "mmlu",
                "text": f"{q['question']}\n\n{choices_text}\n\nAnswer with just the letter.",
                "scorer": lambda resp, q=q: score_mmlu_answer(resp, q["answer"]),
            })
    if args.questions in ("all", "workspace"):
        for q in WORKSPACE_QA:
            questions.append({
                "id": q["id"],
                "type": "workspace",
                "text": q["question"],
                "scorer": lambda resp, q=q: score_workspace_answer(resp, q["answer_contains"]),
            })

    print(f"╔═══════════════════════════════════════════════════════╗")
    print(f"║  Ablation Study: Context Assembly                    ║")
    print(f"╠═══════════════════════════════════════════════════════╣")
    print(f"║  Model:      {args.model}")
    print(f"║  Conditions: {', '.join(conditions)}")
    print(f"║  Questions:  {len(questions)} ({args.questions})")
    print(f"║  Kernel:     {'yes' if kernel_available else 'no'}")
    print(f"║  Output:     {out_dir}")
    print(f"╚═══════════════════════════════════════════════════════╝")
    print()

    results = []

    for qi, q in enumerate(questions):
        print(f"── Question {qi+1}/{len(questions)}: {q['id']} ({q['type']}) ──")
        print(f"   {q['text'][:80]}...")

        for cond_id in conditions:
            cond_name, cond_fn = CONDITIONS[cond_id]
            t0 = time.time()
            response = cond_fn(args.model, q["text"])
            elapsed = time.time() - t0
            score = q["scorer"](response)

            results.append({
                "question_id": q["id"],
                "question_type": q["type"],
                "condition": cond_id,
                "condition_name": cond_name,
                "score": score,
                "elapsed_sec": round(elapsed, 1),
                "response_preview": response[:200],
                "model": args.model,
                "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            })

            marker = "✓" if score >= 0.5 else "✗"
            print(f"   {cond_id} ({cond_name:20s}): {score:.2f} {marker}  ({elapsed:.1f}s)")

        print()

    # Save results
    results_file = out_dir / f"results-{args.model.replace(':', '-')}-{int(time.time())}.json"
    with open(results_file, "w") as f:
        json.dump(results, f, indent=2)

    # Print summary
    print("═══════════════════════════════════════════════════════")
    print("  SUMMARY")
    print("═══════════════════════════════════════════════════════")

    for qtype in ["mmlu", "workspace"]:
        type_results = [r for r in results if r["question_type"] == qtype]
        if not type_results:
            continue
        print(f"\n  {qtype.upper()} Questions:")
        for cond_id in conditions:
            cond_results = [r for r in type_results if r["condition"] == cond_id]
            if not cond_results:
                continue
            avg_score = sum(r["score"] for r in cond_results) / len(cond_results)
            cond_name = CONDITIONS[cond_id][0]
            bar = "█" * int(avg_score * 20) + "░" * (20 - int(avg_score * 20))
            print(f"    {cond_id} ({cond_name:20s}): {avg_score:.3f}  {bar}")

    # Compute differentials
    print("\n  DIFFERENTIALS (what each layer adds):")
    for qtype in ["workspace"]:
        type_results = [r for r in results if r["question_type"] == qtype]
        if not type_results:
            continue
        avgs = {}
        for cond_id in conditions:
            cond_results = [r for r in type_results if r["condition"] == cond_id]
            if cond_results:
                avgs[cond_id] = sum(r["score"] for r in cond_results) / len(cond_results)
        if "A" in avgs and "B" in avgs:
            print(f"    B-A (RAG over stock):        {avgs['B']-avgs['A']:+.3f}")
        if "A" in avgs and "C" in avgs:
            print(f"    C-A (foveated over stock):   {avgs['C']-avgs['A']:+.3f}")
        if "B" in avgs and "C" in avgs:
            print(f"    C-B (foveated over RAG):     {avgs['C']-avgs['B']:+.3f}")
        if "C" in avgs and "D" in avgs:
            print(f"    D-C (tools over foveated):   {avgs['D']-avgs['C']:+.3f}")
        if "A" in avgs and "D" in avgs:
            print(f"    D-A (full stack over stock):  {avgs['D']-avgs['A']:+.3f}")

    print(f"\n  Results saved to: {results_file}")


if __name__ == "__main__":
    main()

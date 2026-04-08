# Foveated Context Engine — Autoresearch

You are optimizing the **foveated context assembly pipeline** in CogOS. The pipeline decides which workspace documents to show the model, in what order, and at what resolution. Your job is to make the pipeline select better context.

## What This Is

CogOS assembles context for every model request using:
1. **TRM scoring** — a Mamba SSM that scores documents by temporal relevance
2. **Salience field** — recency + frequency + churn signals from the workspace
3. **Zone ordering** — Nucleus (always), Knowledge (stable), History (volatile), Current (immediate)
4. **Iris pressure** — available token budget determines how much context to include
5. **Anchor extraction** — foveal center derived from the user's prompt

The pipeline is at `internal/engine/context_assembly.go` and `internal/engine/serve_foveated.go`. The TRM scorer is at `internal/engine/trm_context.go`.

## The Experiment

You're running an ablation study comparing three context strategies:
- **A (Stock):** No context — model answers from weights alone
- **B (RAG):** Keyword search (grep) → stuff matching docs into context
- **C (Foveated):** CogOS kernel's full pipeline (TRM + salience + zones + iris)

The goal: **maximize the C-A differential** (how much foveated helps over stock) while also beating B (proving foveated is better than naive RAG).

## The Kernel

A CogOS kernel is running at `http://localhost:6931`. It serves:
- `GET /health` — liveness
- `POST /v1/context/foveated` — the context assembly endpoint
- `GET /v1/debug/context` — current attentional field state
- `GET /v1/debug/last` — debug snapshot of the last request

The foveated endpoint takes `{prompt, iris: {size, used}, profile}` and returns assembled context with metadata (tokens, anchor, zone breakdown, block sources).

## What You CAN Modify

`config.yaml` is the file you modify. It contains parameters that control the context assembly:

```yaml
# Zone budget allocation (fraction of total budget per zone)
zone_budgets:
  nucleus: 0.15      # identity, always present
  knowledge: 0.40    # workspace docs, scored by TRM
  history: 0.30      # conversation turns, scored by recency
  current: 0.15      # current message

# Salience scoring weights
salience:
  recency_weight: 0.4
  frequency_weight: 0.3
  churn_weight: 0.3
  decay_half_life: 3600  # seconds

# TRM integration
trm:
  enabled: true
  score_weight: 0.6      # blend with salience (0=salience only, 1=TRM only)
  min_score_threshold: 0.1

# Iris pressure response
iris:
  pressure_curve: "linear"  # linear, sigmoid, exponential
  max_budget_fraction: 0.8  # max context / total window
  min_budget_tokens: 500    # always include at least this much

# Anchor extraction
anchor:
  method: "keyword"         # keyword, embedding, hybrid
  max_keywords: 5
  boost_factor: 2.0         # multiply scores of anchor-matching docs
```

Each experiment changes ONE parameter (or a combination in PLATEAU mode), runs the eval, and records the result.

## What You CANNOT Modify

- `eval.py` — the evaluation harness. Read-only.
- The kernel code — you're tuning parameters, not rewriting the engine.
- The question set — fixed for consistency across experiments.

## Evaluation

`eval.py` does the following:
1. Runs 15 workspace questions through all 3 conditions (A, B, C)
2. Scores each response by keyword recall against known answers
3. Calls `GET /v1/debug/last` after each foveated request to capture what the engine assembled
4. Computes per-condition averages and differentials
5. Reports:
   - `foveated_avg` — average score for condition C
   - `stock_avg` — average score for condition A
   - `rag_avg` — average score for condition B  
   - `c_minus_a` — the primary metric (foveated value over stock)
   - `c_minus_b` — secondary metric (foveated value over RAG)
   - `context_ndcg` — NDCG of the documents the engine selected vs the ideal documents
   - `context_tokens` — average tokens assembled by foveated
   - `debug_snapshot` — what the engine actually selected (for diagnosis)

**The primary metric is `c_minus_a`.** Maximize it.
**The secondary metric is `context_ndcg`.** This measures document selection quality independent of the model.

## Setup

1. Verify the kernel is running: `curl -sf http://localhost:6931/health`
2. Read `results.tsv` to see what's been tried
3. Read `config.yaml` to see current parameters
4. Run: `python3 eval.py 2>&1 | tee run.log`
5. Record results

## Logging Results

Log to `results.tsv` (tab-separated):

```
commit	c_minus_a	context_ndcg	foveated_avg	stock_avg	rag_avg	status	description
```

1. git commit hash (short)
2. c_minus_a differential
3. context_ndcg (document selection quality)
4. foveated_avg (condition C score)
5. stock_avg (condition A score)
6. rag_avg (condition B score)
7. status: `keep`, `discard`, or `crash`
8. description of what was changed

## Adaptive Search Strategy

Same as TRM autoresearch:
- **EXPLORE** (<5 experiments): try diverse parameter changes
- **EXPLOIT** (2+ keeps in last 10): tune around what's working
- **PLATEAU** (0 keeps in last 8+): radical changes — swap scoring methods, change zone structure
- **RECOVER** (3+ crashes): revert to known-good config

## Supervisor Barge-In (every 5 minutes)

Every 5 minutes of wall time, a supervisor node reads the chain and:
1. Analyzes which questions foveated wins/loses on
2. Reads `GET /v1/debug/last` for failed questions to see what the engine assembled
3. Generates training signal: (query, good_docs, bad_docs) triples
4. Adjusts the next experiment strategy based on patterns
5. Appends analysis to `supervisor-chain.jsonl`

The supervisor is the adaptation mechanism. Ralph runs experiments; the supervisor steers.

## The Experiment Loop

LOOP FOREVER:

1. Read `results.tsv` — what's been tried, what's the current best?
2. **Determine search mode** (EXPLORE/EXPLOIT/PLATEAU/RECOVER)
3. Read `config.yaml` — what parameters are set now?
4. Pick an experiment **constrained by search mode**
5. Modify `config.yaml` with the experimental change
6. `git add config.yaml && git commit -m "exp [MODE]: description"`
7. Apply config to kernel: `curl -X POST localhost:6931/v1/config/reload` (or restart)
8. Run: `python3 eval.py 2>&1 | tee run.log`
9. Read results: `grep "^c_minus_a:\|^context_ndcg:" run.log`
10. Record in `results.tsv`
11. If c_minus_a improved → keep
12. If equal or worse → discard (`git checkout config.yaml`)

**NEVER STOP.** Run until interrupted.

## Parameters to Explore

Priority order (highest impact first):
1. **Zone budgets** — how much of the window goes to each zone
2. **TRM score weight** — how much to trust TRM vs salience
3. **Anchor boost factor** — how much to favor prompt-matching docs
4. **Iris pressure curve** — how aggressively to compress under pressure
5. **Salience weights** — balance of recency vs frequency vs churn
6. **Min score threshold** — cutoff for including low-scoring docs
7. **Max budget fraction** — how much of the window to use for context

## Principles

- **One change per experiment.** Isolate variables.
- **Trust the metric.** c_minus_a is ground truth.
- **Capture the debug snapshot.** When foveated loses, check what it assembled — the diagnosis is in the `debug_snapshot`.
- **The supervisor reads the debug.** That's the feedback loop to the TRM training pipeline.
- **Simplicity wins.** If removing a parameter improves or maintains the score, remove it.

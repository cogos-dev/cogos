# TRM Evaluation Methodology

This document describes the evaluation methodology for the Tiny Recursive Model (TRM), the 2.3M-parameter Mamba SSM that powers context selection in CogOS. The goal is to make the NDCG@10 claim verifiable and reproducible.

## What NDCG@10 Measures

NDCG@10 (Normalized Discounted Cumulative Gain at 10) measures how well a ranking system places relevant items near the top of a result list. In plain terms:

1. The model scores 64 candidate document chunks for a given query.
2. We take the top 10 by score and check which ones are actually relevant (label = 1.0).
3. Relevant items ranked higher get more credit than relevant items ranked lower (logarithmic discount).
4. The score is normalized against the ideal ranking (if the model had placed every relevant item first).

A score of 1.0 means perfect ranking. A score of 0.5 means the model is placing relevant items roughly in the middle of its ranking. The cosine similarity baseline on this task scores ~0.402, meaning pure embedding similarity is a weak signal for predicting what the user will actually need next.

### Formal Definition

```
DCG@k  = sum_{i=1}^{k} label[rank_i] / log2(i + 1)
IDCG@k = sum_{i=1}^{k} label[ideal_i] / log2(i + 1)
NDCG@k = DCG@k / IDCG@k
```

Implementation: `train_mamba.py:evaluate_ndcg()` and `prepare_sequences.py:evaluate_ndcg()`.

## The Task: Next-Retrieval Prediction

The TRM is evaluated on a **next-retrieval prediction** task, not a static query-document matching task. This distinction is critical.

**Input:** A prefix of temporally ordered session events — user queries, file reads, searches, and edits — representing the trajectory of an agent working through the workspace up to time `t`.

**Output:** Scores for 64 candidate document chunks, predicting which chunks the agent will actually read at time `t+1`.

**Ground truth:** Binary labels derived from real Claude Code session transcripts. A chunk is labeled 1.0 if the agent actually read a document containing that chunk in the next exchange.

This is fundamentally different from information retrieval benchmarks like MS MARCO or BEIR. Those measure "given a query, find relevant documents." The TRM measures "given a trajectory of work, predict what the agent needs next." Cosine similarity is a weak baseline here because the next-needed document often has low embedding similarity to the current query — the agent may need a config file after asking a theoretical question, because the trajectory context tells you they are about to implement something.

## Evaluation Conditions

The evaluation has four distinct layers, each testing something different:

### 1. Validation NDCG@10 (Primary Metric)

**What it tests:** Ranking quality on held-out temporal sequences.

**Method:** The training data is split 80/20 (by random permutation, seed=42). The validation set (~130 sequences, depending on the corpus) is never seen during training. Every 100 training steps, the model is evaluated on the full validation set. The best checkpoint by validation NDCG is kept.

**This is the metric reported in the results TSV.** All 439 experiment rows in `results_mamba.tsv` report this number.

### 2. Cosine Baseline (Reference)

**What it tests:** How well pure embedding similarity performs on the same task.

**Method:** For each validation sequence, take the last event embedding before the prediction point and compute cosine similarity against all 64 candidates. Rank by cosine similarity. Compute NDCG@10.

**Result:** ~0.402 on the temporal prediction task (varies slightly with data). This is the floor the TRM must beat to be useful.

Note: cosine scores much higher (~0.769) on the static query-document task from Phase 3 of development. The temporal task is harder because the relevant document is often semantically distant from the most recent query.

### 3. Downstream Context Judge (eval_downstream.py)

**What it tests:** Whether TRM-selected context is actually better for answering questions, as judged by an LLM.

**Method:**
1. 33 hand-written test queries spanning architecture, theory, implementation, cognitive science, and cross-domain topics.
2. For each query, TRM selects top-10 chunks and cosine selects top-10 chunks.
3. Both context sets are shown to a judge model (Claude via `claude -p`) in randomized A/B order to prevent position bias.
4. The judge evaluates relevance, coverage, coherence, and signal-to-noise, then picks a winner.

**Limitation:** The judge evaluates context quality, not response quality. It can see tokens but not document structure, which may bias toward surface-level similarity — the same thing cosine already optimizes for.

### 4. Response-Based Downstream Eval (eval_response.py)

**What it tests:** Whether TRM context produces better *responses*, not just better-looking context sets.

**Method:**
1. Same 33 test queries.
2. TRM context and cosine context are each fed to Claude to generate actual responses.
3. The judge sees only the two responses (never the context), evaluating accuracy, completeness, coherence, and insight depth.

This is the strongest evaluation because structural coherence of the context becomes visible through its downstream effects even when the judge cannot directly observe document structure.

## Training Data

### Source: Claude Code Session Transcripts

Training data is mined from real Claude Code interaction history — the actual record of an agent navigating the workspace. Two scripts extract this data:

**`mine_sessions.py`** parses Claude Code session JSONL files from `~/.claude/projects/` and CogOS thread transcripts. For each user message, it records which files were subsequently Read, Grepped, Globbed, or Edited. Each (user_message, files_read) pair becomes a training example.

**`prepare_sequences.py`** converts parsed sessions into temporal sequences: ordered lists of (event_embedding, event_type) tuples suitable for the Mamba SSM. For each retrieval event at position `t`, it creates a sample with the prefix events `[:t]`, a pool of 64 candidate chunks (positives + hard negatives + random negatives), and binary labels.

### Training Signals (as of April 10, 2026)

| Source | Count | Description |
|--------|-------|-------------|
| Sessions parsed | 805 | Claude Code sessions + CogOS threads |
| Total signals | 2,298 | Individual training examples |
| Continue | 1,333 | Weak positive — user continued without complaint |
| Provenance | 317 | Gold — reading these docs produced an artifact |
| Crystallization | 237 | Gold — agent read docs then wrote new knowledge |
| Accept | 100 | Gold — explicit user approval |
| Correct | 41 | Hard negative — docs led to a correction |
| Cascade | 40 | Gold — flow state detected (rapid reading) |

**694 gold-standard positives and 41 hard negatives.** The gold signals come from behavioral patterns: if an agent reads documents A, B, C and then writes a new document D that synthesizes them, those reads are gold-standard relevance labels for queries that would produce D.

### Embedding Index

- **Model:** nomic-ai/nomic-embed-text-v1.5 (Matryoshka truncation to 384 dimensions)
- **Corpus:** 3,557+ CogDoc files from the `.cog/` workspace
- **Chunks:** 30,340 (after sectioning documents into manageable pieces)
- **Candidate pool per sample:** 64 chunks (positives + ~21 hard negatives by cosine + random fill)

### Event Types

Each event in a temporal sequence is tagged with one of four types:

| ID | Type | Description |
|----|------|-------------|
| 0 | Query | User message / perturbation |
| 1 | Retrieval | File read (Read tool call) |
| 2 | Search | Pattern search (Grep/Glob) |
| 3 | Edit | File modification (Edit/Write) |

## Model Architecture

### Mamba Selective State Space Model

The TRM is not a transformer. It is a Mamba-style selective SSM that processes session events sequentially, maintaining a hidden state ("light cone") that compresses the observer's trajectory through the workspace.

```
Event embeddings (384-dim)
  + Event type embeddings (384-dim)
  → Linear projection (768 → 384)
  → SelectiveSSM Layer 1 (pre-norm, DropPath 0.05)
  → SelectiveSSM Layer 2 (pre-norm, DropPath 0.05)
  → LayerNorm
  → Max+Mean trajectory pooling
  → 4x Attention Probe (pre-norm, d_head=128)
  → Score head (768 → 384 → 1) per candidate
```

### Final Hyperparameters

| Parameter | Value | How Determined |
|-----------|-------|----------------|
| D_MODEL | 384 | Matches nomic-embed-text-v1.5 Matryoshka dim |
| D_STATE | 4 | Empirical: swept 2-128, 4 optimal (198 experiments) |
| D_CONV | 2 | Empirical: swept 1-4, 2 optimal |
| N_LAYERS | 2 | Empirical: swept 1-4, 2 optimal |
| EXPAND_FACTOR | 1 | No inner expansion — bottleneck forces compression |
| Attention probes | 4 rounds, d_head=128 | Empirical: swept 1-8 rounds, 4 optimal |
| Parameters | 2,282,113 (2.28M) | |
| Hidden state | 1,536 floats (6KB) | D_MODEL * EXPAND * D_STATE * N_LAYERS |

### Why These Choices

**D_STATE=4 (not larger):** Every increase in D_STATE made performance worse. D_STATE=64 (first run): 0.424. D_STATE=4: 0.832. The SSM state is a compressed representation of the observer's trajectory; forcing it through a 4-dimensional bottleneck produces better generalization than giving it more capacity to memorize.

**N_LAYERS=2:** Deeper models (3-4 layers) are slower to train within the fixed step budget and don't improve ranking. Shallower (1 layer) loses sequential processing capacity.

**4 attention probes:** After the Mamba layers pool trajectory context, four rounds of cross-attention between the context vector and the candidate set allow comparative ranking — the model can see all candidates before scoring, enabling it to distinguish relative relevance.

**DropPath 0.05:** Stochastic depth regularization on the Mamba layers. This broke the 0.879 plateau to reach 0.886. More aggressive rates (0.07, 0.10) hurt.

## Training Progression

### Phase 1-3: Transformer TRM (148 experiments)

| Milestone | NDCG | Architecture |
|-----------|------|-------------|
| First run | 0.604 | Cross-attention, K=5, latent=128 |
| Cosine baseline | 0.654 | Target to beat |
| Best transformer | 0.824 | K=3, 16 heads, skip connections |

### Phase 4: 30k-Chunk Scaling Crisis

Scaling from ~1,500 chunks to 30,340 chunks broke the transformer TRM. It scored 0.749 against a cosine baseline of 0.769 — the transformer could not beat cosine on realistic data.

### Phase 5: Mamba Pivot (435 experiments)

Architecture sweep and hyperparameter optimization on the Mamba SSM.

| Milestone | NDCG | Key Change |
|-----------|------|------------|
| First Mamba run | 0.424 | D_STATE=64, N_LAYERS=4 |
| D_STATE=4 discovered | 0.832 | Smaller state is better |
| Architecture frozen | 0.860 | D_CONV=2, EXPAND=1, 4 probes |
| Training dynamics | 0.875 | Scheduled label smoothing, LR=1.2e-3 |
| DropPath breakthrough | 0.886 | Stochastic depth regularization |
| Peak (rerun 90 of 183) | **0.900** | Same config, favorable MPS randomness |

### Variance Characterization (183 reruns)

After experiment ~250, every attempted improvement failed to beat 0.886. The autonomous research agent (Ralph) recognized the plateau was variance-dominated and ran 183 identical reruns to characterize the noise floor.

| Statistic | Value |
|-----------|-------|
| N (reruns) | 183 |
| Mean NDCG | 0.8785 |
| Std Dev | 0.0049 |
| Min | 0.8628 |
| Max (rerun 90) | **0.8996** |
| P25 | 0.8764 |
| P75 | 0.8812 |

The 0.900 peak is a **+4.4 sigma outlier** — it occurred once in 183 runs of the identical configuration. The true expected performance of this architecture on this dataset is **0.878 +/- 0.005**.

**Variance sources:** Apple Metal (MPS) non-determinism, thermal throttling (runs range from 85s cool to 160s severely throttled, affecting step count), and checkpoint selection granularity (evaluated every 100 steps).

### All-Time Results Summary

| Metric | Value |
|--------|-------|
| Total experiments (all architectures) | ~631 |
| Mamba experiments | 435 |
| Unique Mamba configurations | 253 |
| Variance reruns | 183 |
| Peak NDCG@10 | 0.900 (single run, +4.4 sigma) |
| True mean NDCG@10 | 0.878 +/- 0.005 |
| Cosine baseline (temporal task) | ~0.402 |
| Delta vs cosine (mean) | +476 points |
| Delta vs cosine (peak) | +498 points |

## Nightly Consolidation

The TRM retrains nightly via an automated pipeline:

```
1. extract-signals.py   → mine new training signals from session logs
2. prepare_sequences.py  → build temporal training sequences
3. train_mamba.py         → train model, evaluate on validation set
4. trm_export.py          → export weights to binary format (9.1MB)
5. Kernel restart         → production loads new weights
```

Total pipeline runtime: ~2.5 minutes. The latest nightly (April 10, 2026) reported:
- Cosine baseline: 0.7850
- Trained TRM: 0.8748
- Delta: +473 points

(The nightly cosine baseline differs from the research baseline because it runs on the latest corpus with updated embeddings.)

## How to Reproduce

### Prerequisites

- Python 3.12+ with `uv` package manager
- PyTorch with MPS support (Apple Silicon) or CUDA
- `nomic-ai/nomic-embed-text-v1.5` (downloaded automatically)
- Claude Code session transcripts in `~/.claude/projects/`
- CogOS workspace with `.cog/mem/` documents

### Step 1: Build Embedding Index

```bash
cd apps/cogos-v3/autoresearch
uv run prepare.py
```

This embeds all CogDoc chunks using nomic-embed-text-v1.5 at 384 dimensions. Output: `~/.cache/cogos-autoresearch/embeddings.pt` and `chunks.json`.

### Step 2: Mine Session Data

```bash
uv run mine_sessions.py --dry-run          # Preview: see signal counts
uv run mine_sessions.py                     # Generate judge_data.pt
```

### Step 3: Build Temporal Sequences

```bash
uv run prepare_sequences.py --stats         # Preview: see session stats
uv run prepare_sequences.py                  # Generate sequences.pt
```

### Step 4: Train and Evaluate

```bash
uv run train_mamba.py
```

This will:
1. Load sequences from `~/.cache/cogos-autoresearch/sequences.pt`
2. Compute cosine baseline NDCG@10 on the validation split
3. Train the MambaTRM for 3,200 steps (~90 seconds on Apple M-series)
4. Report final NDCG@10 and delta vs cosine
5. Save best checkpoint to `best_model_mamba.pt`
6. Append results to `results_mamba.tsv`

### Step 5: Downstream Evaluation (Optional)

```bash
uv run eval_downstream.py --no-judge        # Context comparison without LLM judge
uv run eval_downstream.py                   # Full A/B evaluation (requires Claude CLI)
uv run eval_response.py                     # Response-based evaluation (slower)
```

## Limitations

### What the Evaluation Does Measure

- Ranking quality on held-out temporal sequences from the same workspace
- Improvement over cosine similarity baseline on next-retrieval prediction
- Consistency across 183 identical reruns (variance characterization)

### What the Evaluation Does Not Measure

- **Generalization to other workspaces.** The TRM is trained on and evaluated against a single workspace (~3,500 documents). Performance on a different corpus is unknown and likely lower until retrained.

- **Generalization to other users.** Training data comes from one user's interaction patterns. Different users navigating the same workspace differently would need their own training data.

- **Causal impact on task completion.** The downstream evals (eval_downstream.py, eval_response.py) measure context/response quality as judged by an LLM, not whether the user actually completed tasks faster or more accurately.

- **Cold-start performance.** New documents not yet in the embedding index fall back to cosine similarity via the residual scoring blend (TRM 0.6 + cosine 0.4). The TRM score for truly novel content is undefined.

- **Cross-architecture transfer.** The Mamba hidden state is specific to this architecture. Transferring learned retrieval patterns to a different model architecture is not yet implemented (this is part of the LoRO vision, not current capability).

### Known Confounds

- **MPS non-determinism.** Apple Metal does not guarantee deterministic floating-point operations. The same code, same data, same seed produces different results across runs. This is characterized (sigma = 0.005) but not eliminated.

- **Thermal throttling.** Training runs on laptop hardware vary from 85s (cool) to 160s (severely throttled). Throttled runs complete fewer steps in the same step budget (step-based training was introduced to mitigate time-based variance, but MPS throughput still varies).

- **Small validation set.** With ~130 validation sequences, individual NDCG scores have meaningful variance. The 183-rerun characterization addresses this at the population level but any single run's number has +/- 0.005 uncertainty.

- **Self-referential training data.** The TRM is trained on interaction patterns from a workspace that was being built partly to develop the TRM. This creates a mild circularity: the training data reflects the priorities of someone building a retrieval system, which biases toward retrieval-relevant documents.

## File Reference

| File | Purpose |
|------|---------|
| `train_mamba.py` | Model definition, training loop, evaluation |
| `prepare_sequences.py` | Temporal sequence extraction from sessions |
| `mine_sessions.py` | Session transcript parsing and signal extraction |
| `prepare.py` | Embedding index construction |
| `eval_downstream.py` | LLM-as-judge context comparison (33 queries) |
| `eval_response.py` | Response-based downstream evaluation |
| `results_mamba.tsv` | Full experiment log (439 rows) |
| `results.tsv` | Transformer-era experiment log (69 rows) |
| `program_mamba.md` | Research program specification for Mamba phase |
| `program.md` | Original transformer research program |
| `training-signals/signals/` | 2,298 extracted training signal JSON files |
| `best_model_mamba.pt` | Best checkpoint (PyTorch) |
| `trm_export.py` | Export weights to Go-compatible binary format |

All source files are in `apps/cogos-v3/autoresearch/` relative to the cog workspace root.

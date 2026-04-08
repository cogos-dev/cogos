# Autoresearch Harness — Claude Code + CogOS + Local/Remote Models

> Autonomous experiment loop using Claude Code as the harness,
> CogOS for context assembly, and a mix of local (Ollama) and
> remote (Anthropic) models for cost-efficient research.

## Launch Command

```bash
# Start the CogOS kernel first
./cogos serve --workspace ~/cog-workspace --port 6931 &

# Launch Claude Code backed by CogOS for exploration turns
ANTHROPIC_BASE_URL=http://localhost:6931 \
ANTHROPIC_API_KEY=local \
claude --model gemma4:26b \
  --bare \
  --dangerously-skip-permissions \
  --system-prompt-file scripts/autoresearch-harness.md \
  --add-dir /path/to/autoresearch \
  -p "Begin the autoresearch experiment loop. Read program.md and results.tsv, determine search mode, and run the next experiment."
```

### For synthesis/verification checkpoints (use real Claude):
```bash
claude --bare \
  --dangerously-skip-permissions \
  --system-prompt-file scripts/autoresearch-harness.md \
  --add-dir /path/to/autoresearch \
  -p "Review the last 10 experiments in results.tsv. Synthesize patterns and propose the next 3 most promising directions."
```

## Clock-Time Moderation

| Phase | Model | Cost | Purpose |
|-------|-------|------|---------|
| **Explore** | gemma4:26b via CogOS | Free (local) | Read code, run experiments, iterate |
| **Synthesize** | Claude (opus/sonnet) via API | Paid (budgeted) | Pattern recognition, strategy, writing |
| **Verify** | gemma4:26b via CogOS | Free (local) | Test hypotheses, run benchmarks |
| **Report** | Claude (sonnet) via API | Paid (budgeted) | Final writeup, cogdoc updates |

## The Experiment Loop (adapted from program.md)

You are an autonomous research agent optimizing a Tiny Recursive Model (TRM)
for cognitive context assembly. The TRM is a Mamba SSM that scores workspace
documents by relevance — the foveated context engine's core component.

### Your Environment

- **Working directory**: The autoresearch folder (contains train.py, prepare.py, results.tsv)
- **CogOS kernel**: Running at localhost:6931 (provides foveated workspace context)
- **Ollama**: Running at localhost:11434 (gemma4:26b warm in VRAM)
- **Tools**: Read, Write, Bash (with sandbox constraints)

### The Loop

```
LOOP FOREVER:
  1. Read results.tsv — what's been tried, what's the current best?
  2. Determine search mode (EXPLORE/EXPLOIT/PLATEAU/RECOVER)
  3. Read current train.py — what does the architecture look like?
  4. Pick experiment constrained by search mode
  5. Modify train.py with the experimental idea
  6. git add train.py && git commit -m "exp [MODE]: description"
  7. Run: uv run train.py 2>&1 | tee run.log
  8. Read results: grep "^val_ndcg:\|^num_params:" run.log
  9. Record in results.tsv
  10. If improved → keep. If not → discard (git checkout train.py)
  11. After every 5 experiments: write a brief synthesis note
  12. NEVER STOP until max_turns or interrupted
```

### Search Mode Detection

Count the last 10 experiments:
- **EXPLORE** (<5 experiments or fresh start): Diverse, divergent ideas
- **EXPLOIT** (2+ keeps in last 10): Small variations around current best
- **PLATEAU** (0 keeps in last 8+): MUST break out — radical changes
- **RECOVER** (3+ crashes in last 10): Stabilize first, conservative changes

### Constraints

- **One change per experiment** (except PLATEAU mode: combinatorial allowed)
- **train.py is the ONLY file you edit**
- **Do NOT modify**: prepare.py, embed_index.py, data files
- **Trust the metric**: NDCG@10 is ground truth
- **Track reasoning**: Commit messages explain WHY, not just WHAT
- **Don't repeat**: Check results.tsv before trying something

### Current State (Phase 4)

- ~30,000 chunk embeddings from 3,500+ workspace documents
- 1,162 judge labels from real cognitive trajectories
- Cosine similarity baseline: 0.769
- Current best: check results.tsv for latest
- Architecture: Mamba SSM, K=3 iterations, latent=384, 16 heads, 1.5M params
- Training budget: 120 seconds on Apple Silicon MPS

### Safety

- You are sandboxed: no network access, no file writes outside the autoresearch directory
- All experiments are git-committed before running
- Failed experiments are reverted (git checkout train.py)
- Results are append-only in results.tsv

### Synthesis Checkpoints

After every 5 experiments, write a brief note:
```
## Synthesis after experiments N-M

### What's working
- ...

### What's not working
- ...

### Emerging patterns
- ...

### Next directions
- ...
```

Save these to a `notes/` directory in the autoresearch folder.

### When to Escalate to Remote Model

Flag for synthesis (human will dispatch to Claude):
- After 10+ experiments with no keep
- When you've identified a pattern but can't articulate the insight
- When you need to compare your findings to published literature
- When the experiment log is getting long and needs strategic review

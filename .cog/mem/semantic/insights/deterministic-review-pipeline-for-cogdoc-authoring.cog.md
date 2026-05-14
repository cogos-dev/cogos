---
type: insight
id: deterministic-review-pipeline-for-cogdoc-authoring
title: "Deterministic Review Pipeline for Cogdoc Authoring"
created: 2026-05-14
status: active
tags: [cogdoc, authoring, pipeline, review, prior-art, reconcilable, pre-commit, skill, trmadditional]
refs:
  - uri: cog://adr/052
    rel: caught-by
    description: >
      ADR-052 ratified the workflow primitive four months before PR #241 landed a
      workflow-as-DAG cogdoc that reframed the same idea. The deterministic review
      pipeline is the substrate layer that would have surfaced ADR-052 before authoring
      began and forced explicit acknowledgment. ADR-052 is the canonical prior-art
      exhibit for this primitive.
  - uri: cog://rfc/034
    rel: binding-pattern
    description: >
      RFC-034's Reconcilable Binding Pattern (Class/Claim/PhysicalInstantiation/Reconciler)
      is the governance structure for the always-on Reconcilable layer of this pipeline.
      The CogdocReviewReconciler is one application of that pattern, running before
      constellation indexing.
  - uri: cog://mem/semantic/insights/trm-training-signal-from-reach-and-action
    rel: forward-work
    description: >
      The pipeline's output tuples (candidates_surfaced + acknowledgments + action_taken)
      are the training signal for the TRM. The pipeline schema must be designed now to
      capture this labeled data; the TRM hookup is forward work.
  - uri: cog://mem/semantic/insights/weigh-against-corpus-not-conversational-context
    rel: operationalizes
    description: >
      [[weigh-against-corpus-not-conversational-context]] is the discipline this pipeline
      enforces structurally. The pipeline makes "weigh against corpus" deterministic rather
      than advisory.
  - uri: cog://mem/semantic/insights/substrate-self-attention-as-dog-food
    rel: instantiates
    description: >
      The pipeline is the substrate self-attending at the authoring layer — the same
      dogfood loop that makes memory consolidation the load-bearing demo.
---

# Deterministic Review Pipeline for Cogdoc Authoring

## The Primitive

A deterministic prior-art acknowledgment gate that runs before any cogdoc reaches the
constellation index. Before an author's proposed cogdoc is committed, the pipeline:

1. Receives the proposed cogdoc (title + frontmatter + body draft)
2. Runs grep-embed similarity search against the existing corpus
3. Surfaces top-N structurally similar documents (cogdocs, ADRs, RFCs)
4. Forces explicit acknowledgment per candidate: "read + distinct" or "this IS that — amend instead"
5. On "amend instead": routes to amendment workflow
6. On "read + distinct": proceeds to authoring, with acknowledgment recorded in the cogdoc's provenance field

The key property is the word "forces." The pipeline cannot be bypassed by inattention. It
is not a suggestion; it is a gate.

## The Failure Mode It Addresses

Chaz's verbatim account from 2026-05-14:

> "The workflow-as-DAG cogdoc landed at PR #241 on chazmaniandinkle/cog-workspace, but
> ADR-052 ratified the workflow primitive four months earlier. The foveated context engine
> should have surfaced it but didn't, because foveal is probabilistic, not deterministic."

This is the exact failure mode the pipeline prevents. The foveated context engine is an
attentional probabilistic system — it surfaces what it predicts is relevant given the
session trajectory. It can miss. When a new cogdoc duplicates prior art in the corpus,
the foveated engine may simply not have activated on the right candidates because the
session context didn't pull in that direction.

The pipeline does not replace foveal — it adds a deterministic enforcement layer that
runs independent of session trajectory.

## Soft vs. Hard: Foveal as Probabilistic, Pipeline as Deterministic Enforcement

The foveated context engine (TRM + Ollama bge-m3 embedding, `internal/engine/trm_context.go`)
is the input layer for the pipeline's candidate set. It is a predictive ranking system that
works well when session trajectory is aligned with relevant prior art. It is not a gate.

This pipeline is a gate. The distinction:

| Layer | Mechanism | Failure mode |
|---|---|---|
| Foveated context engine | Probabilistic ranking via TRM + embeddings | Miss when session trajectory is misaligned |
| Deterministic review pipeline | Explicit grep-embed search + forced ACK | Cannot miss — runs regardless of session state |

The pipeline uses the same embedding infrastructure as the foveated engine (bge-m3, Ollama)
but invokes it deterministically at authoring time, not predictively during session assembly.

## Three Composable Layers

The pipeline is designed as three composable layers, each providing a different enforcement point:

### Layer A: Reconcilable (Always-On Substrate Guarantee)

A `CogdocReviewReconciler` implementing the `Reconcilable` interface from `pkg/reconcile/types.go`
(RFC-008 contract). The Reconcilable runs before constellation indexing on every proposed cogdoc.

- **Class**: `CogdocReviewClass` — declares the review policy (similarity threshold, top-N,
  required acknowledgment fields)
- **Claim**: the proposed cogdoc itself (LogicalClaim: "I want to author this")
- **PhysicalInstantiation**: the committed cogdoc with provenance recorded
- **Reconciler**: the gate that binds the Claim to the Physical only after acknowledgments are complete

This is the substrate-level guarantee. It cannot be bypassed by authoring tools that
respect the Reconcilable lifecycle.

### Layer B: Pre-Commit Hook (Earlier, Faster Feedback)

An extension of the existing `.cog/hooks/git-pre-commit.py` pattern. The hook runs at
`git commit` time and catches the failure mode one step earlier — before the cogdoc
reaches the kernel's indexing pipeline.

The hook calls the same grep-embed similarity logic as Layer A but provides immediate
terminal feedback. It blocks the commit if:
- Similarity score above threshold against any existing cogdoc AND
- No acknowledgment frontmatter field present

This layer is optional per workspace configuration (some operators may prefer Layer A alone)
but is the recommended default for the `~/workspaces/cog` workspace.

### Layer C: Skill (Operator-Facing Surface)

A `/cogdoc:propose` skill (or equivalent invocation) that wraps the full pipeline with an
interactive conversational surface. The author proposes a cogdoc; the skill runs the
similarity search; presents candidates with structural context; requests acknowledgment
per candidate; then either routes to amendment workflow or confirms to proceed.

The skill is the Layer A + B complement at the conversational layer. It provides:
- Formatted candidate display with similarity scores and structural relationships
- Per-candidate acknowledgment prompts with amendment routing
- Provenance generation for the "read + distinct" acknowledgment record

## Composition with Existing Substrate Pieces

**ADR-052 (Executable Cogdocs)**: The amendment workflow (Layer A "amend instead" branch)
is itself a workflow cogdoc under ADR-052 — a `type: workflow` document that guides the
author through the amendment process. This closes a recursive loop: the pipeline enforces
the corpus, and the corpus describes the pipeline.

**RFC-034 (Reconcilable Binding Pattern)**: The Reconcilable layer (Layer A) is one
application of RFC-034's four-part binding structure. `CogdocReviewReconciler` is a
Reconciler in the RFC-034 sense.

**Constellation indexer (`constellation_sessions.go`)**: The indexer provides the
searchable corpus that the pipeline queries. The pipeline runs before the indexer writes
a new document — the gate sits upstream of the write.

**Foveated context engine (TRM, `internal/engine/trm_context.go` + `trm_lightcone.go`)**:
The foveal is the probabilistic input layer. The pipeline's top-N candidate retrieval uses
the same bge-m3 embedding infrastructure but invokes it deterministically at authoring time.

**[[weigh-against-corpus-not-conversational-context]]**: This pipeline operationalizes that
discipline structurally. "Weigh against corpus" is no longer advisory — it is enforced.

**[[trm-training-signal-from-reach-and-action]]**: The pipeline's output tuples form TRM
training data. The schema design (see below) must capture the labeled signal now even though
the training hookup is forward work.

## TRM Training Signal: Schema Design Intent

Each pipeline execution produces a labeled tuple:

```
{
  "proposed_cogdoc_id": "...",
  "query_text": "...",          // title + abstract of the proposed cogdoc
  "candidates_surfaced": [      // top-N from similarity search
    {"cogdoc_id": "...", "score": 0.87, "structural_relationship": "..."},
    ...
  ],
  "acknowledgments": [          // one per candidate
    {
      "cogdoc_id": "...",
      "decision": "read+distinct" | "amend-instead",
      "acknowledged_at": "..."
    }
  ],
  "action_taken": "authored" | "amended" | "abandoned",
  "session_id": "...",
  "operator_id": "..."
}
```

The TRM can learn from this corpus: which surfacing rankings correlated with operator reach,
which (surfaced + acknowledged + action) shapes signal canonical prior art vs. genuinely new
work. The pipeline is self-generating its own training data during normal operation.

**This is forward work.** The immediate implementation slice is the pipeline itself. The
TRM hookup follows once the pipeline is live and accumulating tuples.

## Open Questions

1. **Similarity threshold tuning**: What score threshold triggers the forced-ACK gate?
   Too low produces noise; too high misses genuine duplicates. Initial calibration needed
   against the existing corpus.

2. **Amendment workflow cogdoc**: The "amend instead" branch needs a concrete ADR-052
   workflow document. Designed as part of the implementation, not in advance.

3. **Layer A / Layer B coordination**: If both layers are running, do they share the same
   acknowledgment record, or does the pre-commit hook re-run the search independently?
   Preference: share the record (Layer B reads the provenance field written by Layer A).

4. **Top-N default**: How many candidates to surface? Start with N=5; tune from pipeline
   execution logs.

5. **Cross-repo scope**: The pipeline currently scoped to `~/workspaces/cog`. Extension
   to `~/workspaces/myrgic/` workspace and other substrates is follow-on.

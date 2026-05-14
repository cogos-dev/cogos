---
type: workflow
id: deterministic-review-pipeline-impl-plan-2026-05-14
title: "Implementation Plan: Deterministic Review Pipeline for Cogdoc Authoring"
created: 2026-05-14
status: proposed
tags: [implementation-plan, cogdoc, review-pipeline, reconcilable, pre-commit, skill, proposed]
author: chaz
refs:
  - uri: cog://mem/semantic/insights/deterministic-review-pipeline-for-cogdoc-authoring
    rel: implements
  - uri: cog://adr/052
    rel: workflow-schema-source
  - uri: cog://rfc/034
    rel: binding-pattern-source
  - uri: cog://rfc/008
    rel: reconcilable-contract

workflow:
  entry: wave-1-foundation
  model: sonnet
  max_turns: 50
  state:
    persistence: session
    location: .cog/mem/working/state/

params:
  cog_workspace: "${COGOS_WORKSPACE:-$HOME/workspaces/cog}"
  myrgic_repos: "${MYRGIC_REPOS_ROOT:-$HOME/workspaces/myrgic}"
  plan_status: proposed
---

# Implementation Plan: Deterministic Review Pipeline for Cogdoc Authoring

**Status: PROPOSED — awaiting operator + parent review before execution begins.**

This plan covers all three layers of the deterministic review pipeline:
- Layer A: Reconcilable (always-on substrate guarantee, `myrgic/cogos` repo)
- Layer B: Pre-commit hook (authoring-time feedback, `~/workspaces/cog` workspace)
- Layer C: Skill operator surface (`~/workspaces/cog` workspace or `myrgic/plugins`)

---

## Task Table

| ID | Track | Task | Wave | Role | Tier | Repo | Deps |
|----|-------|------|------|------|------|------|------|
| T01 | infra | Design CogdocReview Reconcilable types (Class, Claim, schema) | 1 | architect | Sonnet | myrgic/cogos | — |
| T02 | infra | Implement similarity search module (grep-embed top-N) | 1 | engineer | Sonnet | myrgic/cogos | — |
| T03 | infra | Design acknowledgment record schema + TRM training tuple format | 1 | architect | Sonnet | cog-workspace | — |
| T04 | layer-a | Implement CogdocReviewReconciler struct (RFC-008 contract) | 2 | engineer | Sonnet | myrgic/cogos | T01, T02 |
| T05 | layer-a | Wire Reconcilable into reconcile registry | 2 | engineer | Sonnet | myrgic/cogos | T04 |
| T06 | layer-a | Write unit tests for Reconciler lifecycle (plan/apply/state) | 2 | engineer | Sonnet | myrgic/cogos | T04 |
| T07 | layer-b | Extend git-pre-commit.py with similarity-check gate | 2 | engineer | Sonnet | cog-workspace | T02, T03 |
| T08 | layer-b | Add config knob to hook-config.yaml for threshold + enable/disable | 2 | engineer | Haiku | cog-workspace | T07 |
| T09 | layer-a | Integration test: Reconciler surfaces ADR-052 given workflow-as-DAG input | 3 | engineer | Sonnet | myrgic/cogos | T05, T06 |
| T10 | layer-b | Integration test: pre-commit hook blocks commit on high-similarity cogdoc | 3 | engineer | Sonnet | cog-workspace | T07, T08 |
| T11 | layer-c | Implement /cogdoc:propose skill (SKILL.md + procedure) | 3 | engineer | Sonnet | cog-workspace | T03, T04 |
| T12 | layer-c | Amendment workflow cogdoc (ADR-052 workflow type) | 3 | engineer | Sonnet | cog-workspace | T11 |
| T13 | layer-a | PR: CogdocReviewReconciler to myrgic/cogos | 4 | reviewer | Sonnet | myrgic/cogos | T09 |
| T14 | layer-b | PR: pre-commit hook extension to cog-workspace | 4 | reviewer | Sonnet | cog-workspace | T10 |
| T15 | layer-c | PR: /cogdoc:propose skill + amendment workflow to cog-workspace | 4 | reviewer | Sonnet | cog-workspace | T12 |
| T16 | corpus | Calibration run: similarity threshold tuning against existing corpus | 4 | analyst | Sonnet | cog-workspace | T09, T10 |
| T17 | corpus | Update cogdoc authoring procedural guide with pipeline reference | 4 | writer | Haiku | cog-workspace | T16 |

---

## Wave Grouping

### Wave 1 — Foundation (parallel, independent)

Three independent design tasks that can run concurrently. No cross-dependencies.

- **T01** (architect/Sonnet, myrgic/cogos): CogdocReview Reconcilable type design — Class,
  Claim, policy schema. Produces Go structs and interface stubs.
- **T02** (engineer/Sonnet, myrgic/cogos): Similarity search module — the grep-embed top-N
  retrieval function that both Layer A and Layer B depend on. Uses bge-m3 via Ollama embed
  endpoint (`OllamaEmbed` in `internal/engine/trm_context.go`).
- **T03** (architect/Sonnet, cog-workspace): Acknowledgment record schema + TRM training
  tuple format. Cogdoc-shaped design artifact that governs the provenance field.

Wave 1 produces: Go type stubs (T01), similarity search function (T02), schema cogdoc (T03).

### Wave 2 — Parallel Layer Implementation

Four tasks: two in myrgic/cogos (Layer A), two in cog-workspace (Layer B).
T04+T05+T06 depend on T01+T02. T07+T08 depend on T02+T03. All four can run in parallel
across worktrees.

- **T04** (engineer/Sonnet, myrgic/cogos): CogdocReviewReconciler struct — LoadConfig,
  FetchLive (candidate retrieval), ComputePlan (similarity gate), ApplyPlan (provenance write),
  BuildState, Health per RFC-008 contract.
- **T05** (engineer/Sonnet, myrgic/cogos): Wire into reconcile registry — register the
  Reconcilable type so the kernel's Reconciler loop drives it.
- **T06** (engineer/Sonnet, myrgic/cogos): Unit tests — Reconciler lifecycle, similarity
  threshold behavior, plan/apply/state invariants.
- **T07** (engineer/Sonnet, cog-workspace): Pre-commit hook extension — add similarity check
  to `git-pre-commit.py`, blocking commit on high-similarity + missing provenance field.
- **T08** (engineer/Haiku, cog-workspace): Config knob in `hook-config.yaml` —
  `cogdoc_review.enabled`, `cogdoc_review.threshold`, `cogdoc_review.top_n`.

Wave 2 produces: complete Layer A implementation + Layer B hook extension.

### Wave 3 — Integration Testing + Skill (can begin as Wave 2 completes per track)

- **T09** (engineer/Sonnet, myrgic/cogos): Integration test — feed workflow-as-DAG cogdoc
  as input, verify ADR-052 is surfaced as top candidate. This is the regression test for the
  PR #241 failure mode.
- **T10** (engineer/Sonnet, cog-workspace): Integration test — pre-commit hook blocks commit;
  provenance field unlocks commit.
- **T11** (engineer/Sonnet, cog-workspace): /cogdoc:propose skill — SKILL.md with procedure
  section covering: receive proposal, run similarity, display candidates, prompt ACK per
  candidate, generate provenance, route to amendment or proceed.
- **T12** (engineer/Sonnet, cog-workspace): Amendment workflow cogdoc — `type: workflow`
  per ADR-052, entry: `assess-overlap`, sections for overlap analysis, amendment drafting,
  frontmatter update, re-review gate.

### Wave 4 — PRs, Calibration, Docs (serial gate)

All Wave 4 tasks require Wave 3 completion. The three PRs open in parallel after tests pass.
Calibration run (T16) informs threshold configuration after PRs merge.

- **T13, T14, T15**: Three PRs, one per repo/workspace per layer.
- **T16**: Calibration run — run similarity search against full cog-workspace corpus with
  varying thresholds; establish the operating threshold for the config default.
- **T17**: Procedural guide update — add "proposing a new cogdoc" section referencing
  the pipeline.

---

## ASCII Wave Diagram

```
Wave 1 (parallel, independent)
┌────────────────────────────────────────────────────────────────┐
│  T01: Type design        T02: Similarity search    T03: Schema  │
│  (cogos, architect)      (cogos, engineer)         (cog, arch)  │
└────────────────────────────────────────────────────────────────┘
          │                        │                    │
          ▼                        ▼                    │
Wave 2 (parallel across repos)    │                    │
┌───────────────────────────┐   ┌─┴──────────────────────────┐
│ myrgic/cogos track:       │   │ cog-workspace track:       │
│  T04: Reconciler impl     │   │  T07: Hook extension       │◄─┘
│  T05: Registry wire       │   │  T08: Config knob          │
│  T06: Unit tests          │   │                            │
└───────────────────────────┘   └────────────────────────────┘
          │                                │
          ▼                                ▼
Wave 3 (integration + skill, each track independent)
┌───────────────────────────┐   ┌────────────────────────────┐
│ T09: Integration test     │   │ T10: Hook integration test │
│     (cogos)               │   │ T11: /cogdoc:propose skill │
│                           │   │ T12: Amendment workflow    │
└───────────────────────────┘   └────────────────────────────┘
          │                                │
          └──────────────────┬─────────────┘
                             ▼
Wave 4 (serial gate — all W3 complete)
┌──────────────────────────────────────────────────────────────┐
│ T13: PR cogos  │ T14: PR hook  │ T15: PR skill  (parallel)  │
│                                                              │
│ T16: Calibration run (after PRs merged)                     │
│ T17: Procedural guide update                                │
└──────────────────────────────────────────────────────────────┘
```

---

## Execution Schedule

| Task | Subagent type | Model | Background | Worktree isolation |
|------|---------------|-------|------------|-------------------|
| T01 | architect | claude-sonnet | no | worktree-cogos-layer-a |
| T02 | engineer | claude-sonnet | no | worktree-cogos-layer-a |
| T03 | architect | claude-sonnet | no | cog-workspace branch |
| T04 | engineer | claude-sonnet | no | worktree-cogos-layer-a |
| T05 | engineer | claude-sonnet | no | worktree-cogos-layer-a |
| T06 | engineer | claude-sonnet | no | worktree-cogos-layer-a |
| T07 | engineer | claude-sonnet | no | cog-workspace branch |
| T08 | engineer | claude-haiku | no | cog-workspace branch (share T07) |
| T09 | engineer | claude-sonnet | no | worktree-cogos-layer-a |
| T10 | engineer | claude-sonnet | no | cog-workspace branch |
| T11 | engineer | claude-sonnet | no | cog-workspace branch |
| T12 | engineer | claude-sonnet | no | cog-workspace branch |
| T13 | reviewer | claude-sonnet | yes (CI watch) | — |
| T14 | reviewer | claude-sonnet | yes (CI watch) | — |
| T15 | reviewer | claude-sonnet | yes (CI watch) | — |
| T16 | analyst | claude-sonnet | no | cog-workspace |
| T17 | writer | claude-haiku | no | cog-workspace |

**Worktree isolation notes:**
- myrgic/cogos work (T01–T06, T09, T13) lands on one branch in a single worktree.
  No parallel cogos agents needed — all cogos tasks are sequential per wave.
- cog-workspace work (T03, T07–T08, T10–T12, T14–T17) lands on a branch in the
  cog-workspace repo. T07 and T08 share a branch (co-located in same file).
- The two repo tracks (cogos vs. cog-workspace) are fully parallel in Waves 1–3.

---

## Cross-Repo Work Summary

| Repo | Work | Wave scope |
|------|------|------------|
| `myrgic/cogos` | T01, T02, T04, T05, T06, T09, T13 | Waves 1–4 |
| `~/workspaces/cog` (cog-workspace) | T03, T07, T08, T10, T11, T12, T14, T15, T16, T17 | Waves 1–4 |
| `myrgic/plugins` (optional) | Skill could live here for portability | Post-Wave 4 |

The `myrgic/plugins` track is not in scope for this plan. The skill ships in cog-workspace
first; portability to plugins is a follow-on decision.

---

## Plan Statistics

| Metric | Value |
|--------|-------|
| Total tasks | 17 |
| Waves | 4 |
| Critical path | T02 → T04 → T09 → T13 |
| Max parallelism | Wave 2: 2 concurrent tracks (cogos + cog-workspace) |
| Repos touched | 2 (myrgic/cogos, ~/workspaces/cog) |
| Model mix | Sonnet (15 tasks), Haiku (2 tasks) |
| Review gates | 1 (Wave 4 entry: all Wave 3 tests pass) |
| PRs | 3 (one per layer, opened in parallel) |

---

## Review Gate (Wave 3 → Wave 4)

Before opening any PR (Wave 4), the following must pass:

1. T09 passes: The regression test — workflow-as-DAG cogdoc surfaces ADR-052 as top-N candidate
2. T10 passes: Pre-commit hook blocks a test cogdoc without provenance field
3. T11 complete: Skill procedure is reviewable (does not need to be tested end-to-end before PR)
4. T12 complete: Amendment workflow cogdoc is reviewable

The gate is explicit: the executing orchestrator must verify all four conditions before
proceeding to Wave 4. No auto-continue.

---

## NOT YET EXECUTED

This plan is in `status: proposed`. The operator and Opus parent must review and accept
before any execution work begins. The plan is the deliverable of this dispatch; execution
is a separate dispatch.

---

## Workflow Sections (per ADR-052 executable cogdoc pattern) {#wave-1-foundation}

This section is the workflow entry point if this cogdoc is later invoked as an executable
workflow via `cog workflow run deterministic-review-pipeline-impl-plan-2026-05-14`.

### Wave 1 {#wave-1-foundation}

Dispatch three parallel agents:
- Agent A (architect/Sonnet): execute T01
- Agent B (engineer/Sonnet): execute T02
- Agent C (architect/Sonnet): execute T03

Collect outputs. Proceed to [[#wave-2-layer-impl]].

### Wave 2 {#wave-2-layer-impl}

Dispatch two parallel tracks (worktree isolation required):
- cogos track (engineer/Sonnet): execute T04, T05, T06 sequentially
- cog-workspace track (engineer/Sonnet + Haiku): execute T07, T08 sequentially

Collect outputs. Proceed to [[#wave-3-integration]].

### Wave 3 {#wave-3-integration}

Dispatch two parallel tracks:
- cogos track (engineer/Sonnet): execute T09
- cog-workspace track (engineer/Sonnet): execute T10, T11, T12 sequentially

Verify review gate: T09 pass + T10 pass + T11 complete + T12 complete.
If gate fails: [[#gate-failure]]
If gate passes: proceed to [[#wave-4-pr-and-calibration]].

### Wave 4 {#wave-4-pr-and-calibration}

Open three PRs in parallel (T13, T14, T15). Watch CI in background.
After PRs merge: execute T16 (calibration), then T17 (docs).

**TERMINATE**

### Gate Failure {#gate-failure}

Log which gate condition failed. Diagnose and fix within the relevant track.
Re-run the failing task. Re-check gate.
If fixed: [[#wave-4-pr-and-calibration]]
If not fixable without scope change: **HALT** — surface to operator for decision.

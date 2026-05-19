# ADR-102: Operator as Reconcilable — Codifying the Substrate's Already-Running Operating Mode

| Field   | Value                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
|---------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Status  | Proposed                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| Author  | @chazmaniandinkle                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| Created | 2026-05-19                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| Layer   | Substrate (framing) — doc-only, no implementation                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| Refs    | [ADR-091](091-substrate-as-named-architectural-layer.md) (Substrate/Kernel/Module trichotomy), [ADR-092](092-substrate-contracts-and-concurrency.md) §4 (Reconcilable contract), [ADR-095](095-daemon-reconcile-loop-driver.md) (ReconcileDaemon), [ADR-096](096-worktree-reconciler.md) (WorktreeReconciler), [ADR-097](097-memory-projection-reconciler.md) (MemoryProjectionReconciler), [ADR-098](098-skill-projection-reconciler.md) (SkillProjectionReconciler), [ADR-099](099-node-identity-layering.md) (node-identity layering), [ADR-100](100-substrate-library-extraction.md) (substrate library extraction), [ADR-101](101-testkernel-in-process-boot-harness.md) (testkernel boot harness), cogos#285 (HarnessBindingCRD) |

---

## Context

### The gap that crystallized the framing

Substrate development through Wave 6 (identity CRDs, RBAC bindings, substrate library
extraction, testkernel) has been assembling primitives in sequence: Reconcilable contract,
self-as-Reconcilable, environment-as-Reconcilable, operator-coupling-as-Reconcilable. Each
wave codified one more layer of what the substrate actually is. What Wave 6b revealed is
that the final layer was not in the future — the operator-substrate coupling has been running
continuously through every agentic harness session since the harness existed. The work ahead
is not building new coupling; it is transcribing the operating mode already running into
durable, inspectable, peer-visible substrate form.

### What the harness is, structurally

An agentic harness session (Claude Code, mod3 audio surface, dashboard, channels) is not
a tool the operator uses to interact with the substrate. It is the structural locus where
operator intent and substrate execution co-inhabit a single coordinated process. Two
identity claims bind simultaneously to one harness instance:

- `HarnessBinding(session=<id>, identity=<operator>, type=user)`
- `HarnessBinding(session=<id>, identity=<agent>, type=agent)`

The session is a 2-seat coordination object. The transcript is a durable attributable
record. Tool calls are policy enactments. The `CLAUDE.md` and memory entries loaded at
session start are the operator's intent as substrate-readable durable form, written by
prior sessions and read by the current one.

Every single session has run the operator-substrate coupling. The `HarnessBindingCRD` that
landed in cogos#285 is the first schema-level recognition of this: the substrate now has
a type that names the relationship. ADR-102 takes the next step and names the *coupling
itself* as Reconcilable-shaped.

### Why this is foundational rather than incremental

Prior evaluation test for proposed substrate work: "does this add capability?" or "does
this fit Reconcilable shape?" These are valid but insufficiently directed. Any
Reconcilable-shaped primitive passes them. The framing in this ADR sharpens both into a
single question:

**Does this make existing operator-substrate coupling more visible to the substrate itself?**

Almost all queued substrate work passes this test with a specific answer, not a vague
"yes." RFC #280 (MCP consolidation) makes the daemon's own tool surface visible via a
unified process. Substrate extraction Step 3 makes the substrate's own schema types
addressable from outside the kernel process. Distillation as Reconcilable makes the
substrate's conversion of ephemeral context into durable artifacts inspectable. Each move
is operator-substrate coupling made visible, not capability invented.

The prior test had no directional pull. This test does. It is the sharpened form and
replaces the prior question as the primary evaluation criterion for ADR-shape decisions.

### Theoretical grounding

The framing is enactivism in code. Cognition is not a process inside a brain that then
interfaces with an environment. It is the structural coupling itself, with the harness as
the embodied locus. Clark/Chalmers extended mind, Hutchins distributed cognition, and
Maturana's autopoiesis-with-environment all arrive at the same structural description from
different vantage points: the agent-environment coupling is primary, not derived.

Kauffman's "Autopoiesis and Eigenform" (Computation 2023, 11(12), 247;
DOI: 10.3390/computation11120247) formalizes this with the equation SS = F(SS): an
autopoietic system is one that is its own eigenform under its own operation. The Reconcilable
contract implements this: a system that applies its seven methods to itself (LoadConfig of
its own spec, FetchLive of its own state, ComputePlan toward its own target) is enacting
the eigenform condition operationally. The `pkg/reconcile` module is the Kauffman equation
made into a Go interface.

---

## Decision

### §1 — The framing claim: operator+harness coupling is already Reconcilable-shaped

The agentic harness — in its current form, today — is carrying both ends of the
operator-substrate coupling. Operator intent enters through `LoadConfig`-equivalent
operations (CLAUDE.md loaded at session start, memory entries indexed into context, scope
packets surfaced via HUD). Current substrate state surfaces through `FetchLive`-equivalent
operations (session.json, traces, active conversation context, MCP tool results). Gaps
between intent and state are visible to the operator through `ComputePlan`-equivalent
reasoning. Operator-approved steps are executed through `ApplyPlan`-equivalent tool calls.
The session itself is the `BuildState`-equivalent output: a transcript, a set of memory
writes, a set of PRs, a set of cogdocs that constitute the durable record of the cycle.

This is not aspirational. This is the current operating mode, running through every session
that has ever used the substrate.

`HarnessBindingCRD` (cogos#285) is the substrate's first schema-level recognition of
this shape. It does not implement the coupling — it names it. ADR-102 names the next level
up: the coupling is Reconcilable-shaped, and the substrate should be built to reflect that
explicitly.

**Decision: the operator-substrate coupling is Reconcilable-shaped and already operational.
The work ahead is making it inspectable, durable across sessions, and peer-visible — not
implementing it from scratch.**

### §2 — The seven-method mapping for operator-as-Reconcilable

The `pkg/reconcile.Reconcilable` interface defines seven methods. Each maps to a concrete
analog in the operator-substrate coupling:

| Method | Reconcilable contract | Operator-as-Reconcilable analog |
|--------|----------------------|----------------------------------|
| `LoadConfig` | Read the spec from durable storage | Ingest operator-intent artifacts: `CLAUDE.md`, memory entries, scope packets, `next-moves-shortlist.md`, dispatch history, open PR queue |
| `FetchLive` | Capture current live state from the environment | Capture current substrate-mediated cognitive state: HUD snapshot, session traces, `session.json`, active conversation context, reconcile daemon state, MCP tool surface |
| `ComputePlan` | Diff spec against live state; produce proposed changes | Identify gaps between operator-intent (loaded spec) and current substrate state (live state); surface as a proposed action list |
| `ApplyPlan` | Execute approved changes against the environment | Propose operator-actionable steps; execute steps that receive approval (tool calls, PRs, memory writes, branch pushes) |
| `BuildState` | Project the current reconciled state as a queryable record | Project the operator-substrate coupling state as a queryable record: session summary, open decisions, pending work, identity bindings active |
| `Type` | Return the provider type string | `"operator"` |
| `Health` | Report the health of the reconcile relationship | Drift between stated operator-intent (spec artifacts) and observed enactment (substrate state, open work, decisions acted on) |

The mapping is not metaphorical. Each method corresponds to a concrete operation the
operator-harness cycle performs. The Reconcilable contract is the interface already
running; the substrate work is making that interface explicit so tools, dashboards, and
agents can observe it.

**Decision: the seven Reconcilable methods map to concrete operator-harness cycle operations
as described in the table above. This mapping is the normative specification for any
operator-as-Reconcilable implementation.**

### §3 — The four-stage growth path

The substrate's development maps onto four stages of self-recognition:

| Stage | Description | Status |
|-------|-------------|--------|
| **Stage 1** | Reconcilable-the-primitive: `pkg/reconcile` proven; 11+ providers; the seven-method contract established as the core substrate extension point | **DONE** — `pkg/reconcile/` is a versioned Go module; ReconcileDaemon drives the loop |
| **Stage 2** | Self-as-Reconcilable: substrate infrastructure (workspace, memory, skill, worktree, identity) becoming Reconcilable instances that manage themselves | **IN PROGRESS** — ADR-096 (WorktreeReconciler), ADR-097 (MemoryProjectionReconciler), ADR-098 (SkillProjectionReconciler), ADR-099 (node identity layering), ADR-100 (substrate extraction), Wave 6b RBAC bindings |
| **Stage 3** | Environment-as-Reconcilable: channels, MCP tool surface, modality bus, external services becoming Reconcilable-managed entities the substrate can inspect and act on | **IN PROGRESS** — RFC #280 (MCP consolidation), RFC-037 (ChannelClass), `HarnessBindingCRD` (cogos#285), `pkg/modality/` channel descriptor primitives |
| **Stage 4** | Operator-as-Reconcilable: the coupling between operator intent and substrate execution becomes explicitly tracked, queryable, and persistent across sessions | **This ADR codifies the framing.** Implementing primitives are queued (see §6). |

Each stage does not replace the prior; it adds a level of self-recognition. The substrate
does not need to complete Stage 2 before Stage 3 work proceeds — the stages are concurrent.
Completion means: the substrate can observe and operate on entities at that level
using standard Reconcilable tooling. Stage 4 completion means: the operator's coupling
with the substrate is as observable and durable as a WorktreeReconciler entry or a
HarnessBinding record.

**Decision: the four-stage growth path is the architectural narrative for the substrate's
self-recognition. New substrate proposals should be located within this path explicitly.**

### §4 — The sharpened evaluation test

Prior evaluation criterion for proposed substrate work: "does this fit Reconcilable shape?"

This is a necessary condition but not a directional one. Any primitive that implements
`LoadConfig / FetchLive / ComputePlan / ApplyPlan / BuildState / Type / Health` passes
the prior test. The prior test does not distinguish between substrate work that advances
the operator-coupling and substrate work that adds unrelated capability.

The sharpened evaluation test:

**"Does this make existing operator-substrate coupling more visible to the substrate itself?"**

Application:

- RFC #280 (MCP consolidation): the daemon ingests its own tool surface as a stable
  self-known genome. Operator tooling visibility goes from implicit (what tools does
  the MCP server have? grep the source) to explicit (the daemon knows its own surface
  and can report it). **Passes.**
- ADR-100 substrate extraction: substrate schema types become importable without a
  running kernel. The substrate's own structure becomes observable from outside the kernel
  process boundary. **Passes.**
- ADR-101 testkernel: the kernel's own behavior becomes testable in-process. The substrate
  can observe and assert its own reconcile lifecycle. **Passes.**
- Conversations Observatory (§6 primitive 1): session JSONLs become substrate-indexed.
  Every operator utterance and tool call becomes searchable as a cogblock. The operator's
  conversational history is the most information-dense operator-coupling artifact in
  existence; making it substrate-readable is the highest-leverage visibility move. **Passes.**
- A new provider that fetches weather data: no relationship to operator-substrate coupling.
  **Does not pass.** This is not a reason to reject it — but it belongs in a Module layer,
  not as a substrate primitive.

The sharpened test replaces "does this fit Reconcilable shape?" as the primary question.
The Reconcilable shape test remains a necessary condition; the sharpened test adds
directionality.

**Decision: "does this make existing operator-substrate coupling more visible to the substrate
itself?" is the primary evaluation criterion for ADR-shape decisions from this point forward.**

### §5 — The closure pathology and the Markov-blanket discipline

The Reconcilable contract's seven methods divide into three structural groups by information
flow:

| Group | Methods | Information direction |
|-------|---------|----------------------|
| **Environment-input** | `LoadConfig`, `FetchLive` | Environment → substrate: the substrate receives signals from outside itself |
| **Substrate-output** | `ComputePlan`, `ApplyPlan` | Substrate → environment: the substrate acts on the world |
| **Self-state** | `Type`, `BuildState`, `Health` | Substrate → substrate: the substrate observes itself |

This three-way division is the Reconcilable contract's implementation of a Markov blanket.
`LoadConfig` and `FetchLive` are the sensory surface; `ComputePlan` and `ApplyPlan` are
the active surface; `BuildState` and `Health` are the internal-state surface. The substrate
maintains a boundary between inside (self-state methods) and outside (input/output methods).

The **closure pathology** is what happens when a substrate primitive collapses this
distinction. A primitive that can only operate on things the substrate already knows
(pure self-reflection) loses the ability to detect environmental drift. A substrate that
is entirely self-reflective has no sensorimotor coupling — it cannot be surprised, cannot
update its model, and cannot generate work that changes the world. The eigenform condition
φ(s\*) = s\* requires a process that applies the function to the state; if the function
never reads from outside, the eigenform is trivially trivial.

This pathology is operationally visible as a substrate that produces a high volume of
internal artifacts (cogdocs, planning documents, memory entries) without any corresponding
change in operator behavior or external state. The artifact-production is `BuildState`
running without `ApplyPlan`.

The Markov-blanket discipline: when reviewing a proposed Reconcilable implementation,
verify that all three groups are represented:

- Does it have `LoadConfig` / `FetchLive` that read from sources outside the substrate's
  own ledger? (environmental coupling)
- Does it have `ComputePlan` / `ApplyPlan` that produce changes visible to the environment
  or operator? (active coupling)
- Does `BuildState` / `Health` reflect the relationship's health, not just self-state?

A primitive that satisfies only the self-state group is not a Reconcilable primitive — it
is a reporting primitive. Reporting primitives have value; they are not Reconcilable shape.

For operator-as-Reconcilable specifically: `LoadConfig` must read actual operator-intent
artifacts from outside the substrate's own ledger (CLAUDE.md, session transcripts, memory
entries as they exist on disk); `ApplyPlan` must produce changes the operator can observe
in their environment (PRs, branch pushes, memory writes, dashboard state). The cycle must
be grounded in environmental signals, not purely in substrate-internal state.

**Decision: the input/output/self-state split within the Reconcilable contract is a design
discipline (Markov-blanket discipline), not just an interface convention. Implementations
that collapse the distinction exhibit the closure pathology. Reviewers should check for
all three groups explicitly.**

### §6 — The implementing primitives queue

The following concrete substrate primitives directly codify the operator-as-Reconcilable
framing in code. Each is scoped briefly here; each warrants its own ADR or RFC when
implementation begins.

#### Primitive 1: Conversations Observatory

**What it is:** A Reconcilable that ingests the session JSONLs from
`~/.claude/projects/-Users-slowbro/*.jsonl` (and analogous locations on other nodes),
projects them as cogblocks/events on the substrate bus, and exposes them as queryable
records via `cog_search_traces` and related MCP tools.

**Why it is the highest-leverage operator-visibility primitive:** Every operator utterance,
every tool call, every timestamp, every approval and rejection is already recorded in these
files. They are the densest operator-coupling artifacts in existence. Today they are
brute-force-grep only. Once the Observatory is live, every future operator-context retrieval
becomes `cog_search_traces "<query>"` returning structured cogblocks rather than a
transcript-walking agent dispatch.

**Reconcilable mapping:**
- `LoadConfig`: discover session JSONL paths, read projection-state from substrate
- `FetchLive`: stat JSONL files for new content since last projection cycle
- `ComputePlan`: identify sessions and turns not yet projected as cogblocks
- `ApplyPlan`: project new turns as substrate events; emit on bus
- `BuildState`: indexed conversation graph summary (session count, turn count, last-indexed timestamp)
- `Health`: drift between JSONL disk state and indexed-state (unindexed turn count)

PR #247 was scope-only; implementation follows this ADR's ratification.

#### Primitive 2: Operator-intent CRD

**What it is:** A formal CRD type that makes explicit what `CLAUDE.md`, memory entries,
scope packets, and dispatch history are doing implicitly. The operator's intent becomes a
substrate-addressable spec rather than a loosely-coupled filesystem convention.

**Why:** Under the current model, the substrate has no way to query "what does the operator
currently intend about topic X?" without loading CLAUDE.md and scanning memory entries
in-context. An operator-intent CRD makes intent queryable as substrate state. The
MemoryProjectionReconciler (ADR-097) is Stage 2 infrastructure for this; the
operator-intent CRD is Stage 4 infrastructure that builds on top.

**Scoping note:** This primitive does NOT propose that operators are a new Identity-class
resource. The HarnessBinding (cogos#285) is the identity-layer record; the
operator-intent CRD is the intent-expression layer record. The two are orthogonal.
Identity says "who is coupled to the substrate here." Operator-intent says "what do they
want the substrate to be doing."

#### Primitive 3: Distillation as Reconcilable

**What it is:** A Reconcilable that treats the substrate's productive output — the
conversion of ephemeral operator-context into durable code, cogdocs, skills, and Reconciler
implementations — as an explicit tracked process.

**Why:** Per the endosymbiotic distillation framing
(`feedback_endosymbiosis_as_distillation_pipeline.md`), the substrate runs a four-stage
pipeline from in-context derivation (Stage 1) through substrate-encoded primitives
(Stage 2) through weights-encoded competence (Stage 3) to co-evolved organelle (Stage 4).
Today this pipeline is implicit: derivations happen, some get encoded, none is tracked as
a pipeline operation. A Distillation Reconcilable makes the pipeline observable:
`LoadConfig` identifies high-frequency in-context derivations; `ComputePlan` proposes
which ones are ready for Stage 2 encoding; `ApplyPlan` creates the primitive (new
Reconcilable, new cogdoc, new skill) and emits the ledger event; `BuildState` tracks
the substrate's distillation state.

This is the substrate's mechanism for generating its own substrate-shaped primitives from
operator+substrate cognitive activity. It is what makes the system self-extending rather
than requiring manual primitive authoring for every new pattern.

#### Primitive 4: MetaProvider expansion

**What it is:** Expansion of `pkg/reconcile/meta.go` to support Reconcilables that take
other Reconcilables as inputs — substrate becoming reflexive at the operator-coupling layer.

**Why:** The operator-as-Reconcilable framing requires a substrate that can operate on
its own Reconcilable structure. A MetaProvider receives a list of Reconcilable types as
input, observes their BuildState outputs, and produces a higher-order reconciliation plan.
The practical form is: an operator-reconcile provider that synthesizes across the
Conversations Observatory, Operator-intent CRD, and Distillation Reconcilable to produce
an integrated operator-substrate coupling assessment. `pkg/reconcile/meta.go` exists but
its surface is narrow. The MetaProvider expansion is the primitive that makes
operator-as-Reconcilable reflexive: D = [D, D] in Kauffman's notation, the domain that
contains its own function space.

---

## Architectural Consequences

### What this enables

**A unified evaluation direction for substrate work.** The sharpened test (§4) gives
reviewers and proposers a consistent directional criterion. Primitives that advance
operator-coupling visibility are Stage 4 primitives; primitives that advance substrate
self-management are Stage 2 primitives; primitives that advance environmental coupling are
Stage 3 primitives. All are valid; the staging makes priority legible.

**An inspectable substrate.** Once the Conversations Observatory (§6 primitive 1) is live,
the substrate can answer questions about its own operational history without requiring
a human to load and grep transcript files. Session summaries become cogblocks. Tool call
patterns become queryable. The operator's expressed intent (in conversation) becomes
accessible to subsequent agents and sessions without requiring manual transcript review.

**Operator-coupling persistence across session boundaries.** Today, each session starts
from memory entries and CLAUDE.md. The operator-substrate coupling that evolved over 12
months of sessions is partially captured in these artifacts but incompletely. Once the
Observatory and operator-intent CRD are live, a new session can query structured substrate
records instead of relying on the current sparse approximation. The coupling survives
session termination as substrate state.

**A concrete implementation target for Wave 6c+.** Prior wave planning has been driven by
unordered capability backlogs. The four-stage growth path (§3) provides an ordered
narrative: complete Stage 2 and 3 primitives in parallel; begin Stage 4 implementing
primitives as soon as ADR-102 is accepted. The implementing primitives in §6 are the
Stage 4 queue.

### What it costs

**This ADR is doc-only.** It implements nothing. The Conversations Observatory, operator-
intent CRD, Distillation Reconcilable, and MetaProvider expansion each require their own
implementation PRs. The cost of this ADR is the opportunity cost of the draft session
and the constraint it imposes on subsequent primitive proposals to locate themselves
within the four-stage path.

**Naming discipline required.** The term "operator" is used with a specific meaning here:
the human who holds the substrate's operator role (owns the workspace, configures
CLAUDE.md, approves apply-plan steps). This is not "operator" in the Kubernetes sense
(a controller), not "operator" in the mathematical sense (a function), and not
"operator" in the telecommunications sense. The substrate's existing vocabulary must
be updated carefully to avoid collision. In code and CRDs, prefer `OperatorCoupling`
or `OperatorIntent` as type prefixes to avoid ambiguity with Go's operator concepts.

**The closure pathology is now a named failure mode.** Naming a failure mode increases
the probability that reviewers will check for it. This is a cost in the sense that it
adds a review criterion; it is a benefit in the sense that the failure mode exists
whether named or not.

### Risks

**Naming drift.** "Operator-as-Reconcilable" is a framing; it is not a Go type, a CRD
name, or an API. If implementations use the framing loosely (an `OperatorReconciler` that
does not implement the Reconcilable interface, a "distillation" that is actually just a
cron job), the framing decouples from the code. The mitigation is: any primitive described
in §6 must implement `pkg/reconcile.Reconcilable` (seven methods) to count as a
codification of this framing. Name-only implementations do not count.

**Scope creep into identity territory.** This ADR is explicit (§6, Primitive 2 note) that
operator-as-Reconcilable does NOT propose a new Identity class. The HarnessBinding records
in cogos#285 are the identity-layer shape. If a future implementer reads §6 and proposes
an `Operator` CRD that extends the Identity CRD, that is out of scope and should be
redirected. Operator identity is already represented via `HarnessBinding(type=user)`.
The operator-as-Reconcilable framing adds an *intent and coupling* layer on top of an
existing identity layer, not a competing identity model.

**The closure pathology, self-referentially.** This ADR is a substrate document about the
substrate's self-recognition. If ADR-102 acceptance results in a surge of internal
substrate documentation and framing work without corresponding implementation progress
on the §6 primitives, that is the closure pathology manifesting at the meta level. The
mitigation is the `ApplyPlan`-analog check: within two weeks of ADR-102 acceptance, at
least one §6 primitive should have an open implementation PR. If not, something is wrong.

---

## Four-Stage Growth Path — Current Progress

| Stage | Description | Key in-flight work | Gate for completion |
|-------|-------------|-------------------|---------------------|
| **Stage 1** | Reconcilable-the-primitive | (complete) | `pkg/reconcile/` versioned module; 11+ providers; ReconcileDaemon |
| **Stage 2** | Self-as-Reconcilable | ADR-100 Steps 1-2 done; Step 3 (root extraction) pending RFC #280 non-overlap; RBAC binding CRDs in cogos#285 | All substrate schema types in `pkg/substrate/`; WorktreeReconciler, MemoryProjectionReconciler, SkillProjectionReconciler all passing in CI |
| **Stage 3** | Environment-as-Reconcilable | RFC #280 (MCP consolidation) open; RFC-037 (ChannelClass) composes with ADR-100 Step 3; `HarnessBindingCRD` merged in cogos#285 | MCP tools unified in daemon; channel schema in `pkg/substrate/channel/`; HarnessBinding records in the ledger for all active sessions |
| **Stage 4** | Operator-as-Reconcilable | This ADR codifies the framing; §6 primitives queued | Conversations Observatory passing in CI; at least one operator-coupling query answerable via `cog_search_traces` without human transcript inspection |

---

## Implementing Primitives Queue

Full descriptions in §6. Ordered by the sharpened evaluation test (most operator-coupling
visibility per implementation effort):

1. **Conversations Observatory** — ingest session JSONLs, project as cogblocks, queryable
   via `cog_search_traces`. Blocks nothing; can start immediately after ADR-102 acceptance.
   Scoping: new provider under `internal/providers/observatory/` or
   `internal/providers/conversations/`; JSONL parser; cogblock emitter; integration test
   via testkernel (ADR-101 Phase 3 prerequisite).

2. **Operator-intent CRD** — formal schema for what CLAUDE.md + memory entries are doing.
   Depends on: ADR-097 (MemoryProjectionReconciler) landing first (it provides the
   projection infrastructure that the intent CRD reads). Implementation: new CRD type
   `OperatorIntentExpression`, provider in `internal/providers/operator_intent/`, MCP tool
   `cog_get_operator_intent`.

3. **Distillation as Reconcilable** — tracks the substrate's productive output as a
   pipeline. Depends on: Conversations Observatory (input data), operator-intent CRD
   (spec for what to distill toward). Implementation: provider in
   `internal/providers/distillation/`; reads Observatory output; emits
   `DistillationProposal` events; ApplyPlan creates the proposed artifact.

4. **MetaProvider expansion** — `pkg/reconcile/meta.go` surface broadened to support
   Reconcilables-over-Reconcilables. Depends on: Stages 2-3 primitives stable. This is
   the reflexivity move; it should follow, not precede, the concrete primitives.

---

## Relationship to Existing ADRs

### ADR-100 (Substrate Library Extraction)

ADR-100 is Stage 2 infrastructure that is a prerequisite for Stage 4 primitives. The
Conversations Observatory needs to emit cogblocks using `pkg/substrate/block/`; the
operator-intent CRD needs to use `pkg/substrate/identity/` types for the HarnessBinding
reference. ADR-100 Step 3 (root extraction) should be coordinated so the Stage 4
primitives can import from `pkg/substrate/` cleanly rather than from root `package main`.
ADR-102 does not block ADR-100 progress; they proceed in parallel.

### ADR-101 (testkernel)

ADR-101 Phase 3 (`kernel.CallTool`) is the prerequisite for integration-testing the
Conversations Observatory and operator-intent CRD end-to-end. The pattern is: boot
testkernel with an isolated registry containing the Observatory provider, trigger a
reconcile cycle, call `cog_search_traces`, assert structured output. This is exactly
the Gap B pattern testkernel was designed for. ADR-102's §6 implementations should not
land before ADR-101 Phase 3.

### RFC #280 (MCP Consolidation)

RFC #280 is Stage 3 infrastructure that directly advances the sharpened evaluation test:
the daemon ingesting its own tool surface as a unified genome is operator-coupling
visibility in the most literal sense. ADR-102 provides the architectural framing that
explains *why* RFC #280 matters beyond "fewer processes." The recommendation: merge RFC
#280's ratification unblocked by ADR-102; the framing is complementary, not gating.

### HarnessBindingCRD (cogos#285)

The HarnessBindingCRD is the substrate-level schema that makes the operator-as-Reconcilable
framing concrete at the identity layer. ADR-102 names it as the first schema-level
recognition of the operator-substrate coupling. The CRD does not need changes to satisfy
this ADR; the operator-intent CRD (§6 primitive 2) is an additive layer that references
it, not a replacement.

---

## Critical Disambiguations

**This ADR does not propose a new `Operator` CRD type or Identity subclass.** The
HarnessBinding (cogos#285) is the schema-level shape for the operator-identity binding.
The operator's intent is currently expressed through identity-expression fields and
CLAUDE.md conventions. The operator-intent CRD (§6 primitive 2) is an intent-and-coupling
layer that builds on the existing identity model, not a new identity class.

**This ADR does not advocate for substrate-AI replacing the operator.** The framing is
co-enactment: operator and substrate together produce cognitive output through the harness.
Making the coupling more visible and durable makes the collaboration more capable — it does
not change the relationship from collaboration to automation. The growth path in §3 is
about durability and inspectability, not autonomy from the operator.

**The K8s analogies in related ADRs and memories are exact, not decorative.** PVC/PV
spec/asset/binding, RoleBinding records, controller convergence — the substrate has
recreated these patterns because they are the actual shape of declarative intent-to-state
reconciliation at the infrastructure layer. The analogy holds because both systems solved
the same problem (managing coupling between intent and state across a distributed system
boundary) and arrived at the same shape. Describing the substrate's binding model as
"K8s-shaped" is a structural observation, not a comparison.

---

## Naming Caveat

The following naming conventions are applied throughout this ADR per
`feedback_substrate_naming_forward_direction.md`:

- "Operator" — plain English for the human who holds the substrate's operator role.
  Not Eigen, EigSight, HyperCycle, or other research-vocabulary naming.
- "Harness" — plain English for the agentic client software (Claude Code, mod3 dashboard,
  etc.) that binds operator and agent identities in a session.
- "Reconcilable" — the `pkg/reconcile.Reconcilable` interface and its seven-method
  contract. Not "Operator Reconcilable" as a distinct type name; the framing is that the
  coupling *is* Reconcilable-shaped, not that a new type called `OperatorReconcilable`
  exists.
- "Distillation" — used in its general sense (conversion of high-complexity input to
  compact durable form) and in the specific sense of the four-stage substrate pipeline
  (Stage 1-4 per §6 primitive 3). The biological and cognitive science isomorphs are
  referenced in the memory corpus; they do not appear in this ADR's normative text.

---

## References

- Kauffman, S.A. "Autopoiesis and Eigenform." *Computation* 2023, 11(12), 247.
  DOI: 10.3390/computation11120247. The central equation SS = F(SS) is the mathematical
  form of what the Reconcilable contract implements operationally. File at
  `~/Downloads/computation-11-00247-with-cover.pdf`.

- [[feedback_operator_as_reconcilable_already_running]] — canonical memory entry that
  crystallized the framing; originating session 47124ad3-b4e9-43d0-aeea-902f931bf5a6
  (2026-05-19).

- [[feedback_substrate_as_fep_ai_implementation]] — the FEP-AI grounding; the substrate
  as a computational implementation of active inference with engineered solutions to
  philosophical shortcomings.

- [[feedback_agentic_harness_multi_identity]] — multi-identity binding structure of
  agentic harnesses; the structural basis for the two-identity claim in §1.

- [[feedback_channel_as_substrate_primitive]] — six-concept vocabulary and containment
  hierarchy; the channel/seat/harness/session stack that §1 references.

- [[feedback_substrate_bindings_as_k8s_rbac]] — bindings-as-Reconcilables framing; K8s
  RBAC as the exact operational pattern; the disambiguation that bindings are URI-space
  records, not OS-path conflations.

- [[feedback_endosymbiosis_as_distillation_pipeline]] — four-stage distillation pipeline
  framing; the substrate's productive output as a self-extending mechanism; grounding for
  §6 primitive 3.

- [[reference_substrate_lineage_attribution]] — lineage sources: von Foerster, Kauffman,
  Spencer-Brown (Laws of Form); the distinction-as-primitive basis for identity-from-
  maintained-distinction.

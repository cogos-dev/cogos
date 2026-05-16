# ADR-091: Substrate as Named Architectural Layer

| Field       | Value                                                                          |
|-------------|--------------------------------------------------------------------------------|
| Status      | Accepted                                                                       |
| Author      | @chazmaniandinkle                                                              |
| Created     | 2026-05-15                                                                     |
| Companion   | [ADR-092 (Substrate contracts and concurrency)](092-substrate-contracts-and-concurrency.md) |
| Refs        | ADR-090 (Kind dispatch via registry), RFC-0001 (root package refactor), RFC-0005 (cog_fork_session), RFC-0006 (vLLM PagedAttention), RFC-0008 (Inference Control Plane), `docs/SYSTEM-SPEC.md` (Membrane/Nucleus/Workspace three-zone model) |

## Context

CogOS has accumulated a set of concerns that share a common shape: they
describe *the field within which observers, identities, reconciliations, and
channels exist*, as distinct from concerns that describe *the agent loop's
execution* or *a specific modality's projection*. Packages and types
implementing these concerns are already present in the codebase, but the
concerns themselves have not been named as a coherent layer:

- `pkg/cogblock/` — hash-chained event ledger (append-only, content-addressable)
- `pkg/reconcile/` — Reconcilable interface and reconcile-loop primitives
- `pkg/modality/` — modality module registration (the extension mechanism)
- `identity_provider.go` (repo root, `package main`) — OIDC-anchored observer identity (CRD)
- `capability_resolver.go` (repo root, `package main`) — live capability advertisement and resolution
- `session_manager.go` (repo root, `package main`) — session continuity, rotation, fork
- `internal/engine/handler_span.go` — handler-level span emission for projection

Each of these has been built and stabilized through independent forcing
functions (RFC-0001 through RFC-0008, ADR-079, ADR-082, ADR-089, ADR-090).
RFC-0001 is currently relocating the `package main` files into
`internal/engine/`; the package paths listed above reflect their current
locations on `main`, not their post-RFC-0001 destinations.

### Relationship to the existing three-zone model

`docs/SYSTEM-SPEC.md` §2 defines a canonical three-zone partition of a CogOS
node, with **Workspace** literally named as *"the cognitive substrate."* This
ADR does **not** replace, supersede, or modify that partition. The three-zone
model partitions a node by **what's running where**:

| Zone        | Contents                                          | Question it answers          |
|-------------|---------------------------------------------------|------------------------------|
| Membrane    | MCP Server, HTTP API, Router, Coherence           | "What mediates inside/outside?" |
| Nucleus     | Identity Core, Process Loop                       | "What defines the node?"     |
| Workspace   | Context Engine, Salience, Ledger, Memory, Blob Store | "What is the cognitive substrate?" |

The trichotomy this ADR introduces (Substrate / Kernel / Module) partitions
concerns by **what kind of operation each is**. The two partitions are
**orthogonal**: a given package or type has both a SYSTEM-SPEC zone AND a
trichotomy layer.

## Decision

Establish **Substrate / Kernel / Module** as a named architectural trichotomy
in CogOS, orthogonal to the SYSTEM-SPEC three-zone model, with the following
commitments:

1. **Conceptual trichotomy is accepted**, *implementation extraction is
   deferred*. The substrate-as-layer is a recognized category in the
   architecture; it does not yet exist as a separately-deployed service or
   process. Promotion to an explicit deployable boundary requires named
   forcing functions (see *Forcing functions* below).

2. **The trichotomy partitions concern-types orthogonally to the three-zone:**

   | Layer         | Concern shape                              | Test                                                                      |
   |---------------|--------------------------------------------|---------------------------------------------------------------------------|
   | **Substrate** | Field-of-existence                         | Does this concern exist independent of any specific agent-loop running?   |
   | **Kernel**    | Agent-loop execution                       | Does this concern require a planning/observing/acting loop to be present? |
   | **Module**    | Modality / channel / surface               | Does this concern project the substrate through a specific medium?        |

   A concrete package has both a SYSTEM-SPEC zone AND a trichotomy layer:

   | Concrete concern                                            | SYSTEM-SPEC zone | Trichotomy layer |
   |-------------------------------------------------------------|------------------|------------------|
   | `pkg/cogblock/` (ledger)                                    | Workspace        | Substrate        |
   | `pkg/reconcile/` (Reconcilable + reconcile loop)            | Workspace        | Substrate        |
   | `pkg/modality/` (modality registration)                     | Membrane         | Substrate        |
   | `identity_provider.go` (CRD)                                | Nucleus          | Substrate        |
   | `capability_resolver.go` (live capability)                  | Nucleus          | Substrate        |
   | `session_manager.go` (session continuity)                   | Workspace        | Substrate        |
   | `internal/engine/handler_span.go` (projection)              | Membrane         | Substrate        |
   | `internal/engine/agent_controller.go`, `internal/engine/autonomic_ticker.go` (agent loop) | Nucleus | Kernel           |
   | `internal/engine/agent_dispatch.go`, `internal/engine/agent_dispatch_query.go` (dispatch executor) | Nucleus | Kernel           |
   | `internal/engine/context_assembly.go` (context assembly)    | Workspace        | Kernel           |
   | MCP Server, HTTP API, Router (`internal/engine/*`)          | Membrane         | Kernel           |
   | `mod3` (separate repo, voice channel)                       | (external)       | Module           |
   | `cog-sandbox-mcp` (separate repo, MCP bridge)               | (external)       | Module           |

   Substrate concerns span multiple SYSTEM-SPEC zones (Workspace + Nucleus +
   Membrane). Kernel concerns span multiple zones too. Module concerns are
   external to the node-scoped three-zone partition. The orthogonality is
   load-bearing: future ADRs should cite both axes when locating new code.

3. **Substrate-shaped packages stay where they are** until a forcing function
   triggers extraction. The classification in section 2's table is
   documentation, not a refactor plan. (Note: `pkg/cogblock/`,
   `pkg/reconcile/`, `pkg/modality/`, `pkg/bep/`, `pkg/coordination/`,
   `pkg/uri/`, and `pkg/cogfield/` are already separately-versioned Go
   modules with their own `go.mod` files. "Extraction" in this ADR refers
   to *deployable boundaries* — separate processes, services, or repositories
   — not to Go-module-as-versioning-unit, which already exists.)

4. **Reconcile-loop vocabulary, honestly stated:**

   *Today:* the substrate's reconcile loop is **Terraform-style** — edge-
   triggered, CLI-invoked plan/apply, no watch/informer, no level-triggered
   convergence guarantee. This is canonical per `pkg/reconcile/doc.go`:
   *"the same Terraform-style loop used throughout the CogOS kernel."*

   *External pitch:* the **Kubernetes operator pattern** (level-triggered,
   watch-based, eventually consistent, single locus of authority) is the
   structural analogy used in documentation and ecosystem positioning. The
   conceptual primitive (reconcile against an observed-vs-desired state diff)
   is the same in both Terraform and Kubernetes; the operational shape
   differs significantly.

   *Target:* evolution toward K8s-shape (level-triggered, watch-based) is an
   *open question*, not a commitment. Forces that would drive that evolution
   include federation-scale operation (multiple kernels reconciling against a
   shared substrate) and the need for autonomic continuous reconciliation
   rather than CLI-invoked one-shots. When and how to evolve is deferred to
   a future RFC.

   Documentation and contributor onboarding should describe the current shape
   accurately (Terraform-style) while noting the K8s analogy as the
   conceptual primitive. External pitches to cloud-native audiences may lead
   with the K8s analogy provided they don't misrepresent the current code.

5. **The ledger is the single write-ahead log for substrate state mutations.**
   Where substrate primitives have both an authoritative record (the
   hash-chained ledger via `pkg/cogblock`) and a derived view (an in-memory
   cache, registry, or projection), the ledger MUST be written first; the
   derived view is rebuilt or invalidated from ledger state. This rule pre-
   empts the controller-bypasses-state-store failure mode documented in
   early-Kubernetes operator history.

   *Note:* this is a logical write-ordering rule, not a concurrency
   guarantee. The ledger's current `AppendEvent` implementation is not
   serialized across concurrent writers; ADR-092 specifies the concurrency
   contract this rule depends on.

## Forcing functions (when substrate is extracted)

Extraction from the conceptual layer into a deployable boundary (separate
process, service, or repository) waits for at least one of:

1. **Dependency inversion trigger** — a change in one substrate-shaped package
   requires coordinated changes across two or more others such that re-testing
   the entire kernel is necessary for what should be a localized change.

2. **Resource contention trigger** — a substrate concern (e.g., ledger replay,
   reconcile-loop scheduling, projection fan-out) consumes resources that
   interfere with the determinism of the primary kernel loop.

3. **Deployment divergence trigger** — a substrate concern (e.g., ledger,
   federation, identity service) needs to run physically separate from kernel
   logic (sidecar pattern, separate process, or remote endpoint).

Until one of these fires, the substrate layer remains implicit-but-named:
code stays where it is, and the trichotomy governs new additions.

## Rationale

### Why a new layer when SYSTEM-SPEC already exists

SYSTEM-SPEC's three-zone model partitions by *what's running where in a
node*; it answers questions like "is this concern in the always-loaded
nucleus or the workspace-scoped substrate?" The new trichotomy partitions by
*what kind of operation each concern is*; it answers questions like "is this
concern an agent-loop execution detail, or part of the field the loop runs
in?"

These are different questions, and contributors need both answers when
locating new code. A reconcile primitive lives in Workspace (zone) AND
Substrate (layer); the agent loop lives in Nucleus (zone) AND Kernel (layer);
a Membrane component like the MCP Server lives in Membrane (zone) AND Kernel
(layer). Without the trichotomy, the layer answer has to be reconstructed by
reading code each time. The trichotomy makes the layer answer citable.

### Why honest vocabulary on Terraform vs K8s

The structural difference between Terraform-style and K8s-style reconcile
loops is not aesthetic. Terraform is edge-triggered (run when invoked);
Kubernetes is level-triggered (run continuously, converge to desired state).
Idempotency means different things in each model. Watch-based observation is
core to K8s-style operation and absent in Terraform-style. Misrepresenting
the current shape as K8s-style would mislead contributors writing new
reconcilers about what guarantees the substrate provides.

The K8s analogy remains useful as the conceptual primitive (reconcile against
state-diff is the operator pattern's primitive) and as external pitch
positioning (K8s's accretion of an ecosystem from a reconciliation primitive
is the empirical precedent for the substrate's bet). Both can be true: the
primitive is K8s-shaped, the current implementation is Terraform-shaped, the
evolution toward K8s-shape is an open question.

### Why deferred extraction

A multi-seat architecture review (six perspectives across two orthogonal
review angles, plus three cross-family local-model perspectives) converged on
"premature abstraction" as the primary risk of extraction without forcing
functions present. Specific findings that survived cross-model verification:

- A proposed ten-directory carving (identity / ledger / reconcile / capability
  / metabolism / boundary / continuity / observatory / extension / federation)
  was over-specified; no reviewer defended it intact.
- Several proposed concerns ("metabolism", "boundary") failed the
  field-of-existence test and belong elsewhere (operator tooling and identity
  respectively).
- The cleanest extraction with non-zero value at near-zero cost is moving
  `pkg/reconcile` and `pkg/cogblock` to a `substrate/` package path, but this
  can be deferred to coincide with the first forcing-function trigger.
- The cognitive-state vs infrastructure-state analogy has real structural
  limits (no decidable convergence predicate for some cognitive operations;
  no single locus of authority equivalent to etcd for cross-substrate truth)
  that should be acknowledged in any reconciler implementation rather than
  hidden behind extraction-as-credibility.

### What this ADR is not

This ADR does not:

- Authorize a refactor of any existing package.
- Define a new public API surface.
- Commit to a target module structure for future extraction.
- Replace or supersede `docs/SYSTEM-SPEC.md` or any existing RFC or ADR.
- Commit to evolving the reconcile loop toward K8s-shape (open question).

It establishes naming, an orthogonal partition, and an honest vocabulary
record. Implementation decisions are deferred to subsequent ADRs that will
reference this one when forcing functions arrive.

## Consequences

### Positive

- New ADRs and RFCs can reference *Substrate*, *Kernel*, and *Module* as
  defined layers without restating the trichotomy each time.
- Contributors locating new code can use both the SYSTEM-SPEC zone test and
  the trichotomy test orthogonally, narrowing placement decisions on both
  axes.
- The honest vocabulary record (Terraform-style today, K8s-shape as analogy
  and open evolution target) prevents future contributors from being
  misled by external-pitch vocabulary that doesn't match the code.
- The ledger-first rule in section 5 of the *Decision* pre-empts a known
  failure mode without requiring any code change today (concurrency contract
  is specified separately in ADR-092).

### Negative

- The substrate-shaped packages remain physically distributed across `pkg/`,
  the repo root, and `internal/engine/`. Readers will need to consult this
  ADR or future onboarding documentation to recognize them as part of the
  same layer.
- The trichotomy boundary will be contested at the margins for some
  additions. The field-of-existence test is the canonical adjudication
  criterion; ambiguous cases should produce a follow-up ADR.
- Two architectural vocabularies (SYSTEM-SPEC three-zone, trichotomy layer)
  now coexist. Contributor onboarding documentation must explain both and
  their orthogonality.

### Neutral

- The Kubernetes-as-conceptual-primitive analogy is structurally apt for
  single-substrate operation and strains at federation boundaries (no single
  locus of authority for cross-substrate truth). The current implementation
  being Terraform-style is a separate matter from the long-run target. Both
  are flagged here; both are addressed when the relevant forcing functions
  arrive.

## Implementation

No implementation work is authorized by this ADR. The following are
documentation-only follow-ups that may be performed at any time:

1. Update `docs/SYSTEM-SPEC.md` to reference the trichotomy as an orthogonal
   partition (a footnote or a new section, not a replacement).
2. Add a section to `docs/architecture-diagram-source.md` showing the layers
   alongside the zones.
3. Add a "Layer" field (Substrate / Kernel / Module) and a "Zone" field
   (Membrane / Nucleus / Workspace / external) to subsequent ADRs and RFCs.

When a forcing function fires, the implementing ADR will reference this one
and supply the extraction shape.

## Open questions

- **Federation truth.** Cross-substrate consistency (when more than one
  substrate instance exists) has no defined ground-truth authority. Flagged
  as a known open problem; future ADR when a second substrate instance is on
  the roadmap.
- **Reconcile-loop evolution.** Whether and how to evolve from Terraform-
  style (edge-triggered, CLI-invoked) to K8s-style (level-triggered, watch-
  based) for federation-scale operation. Future RFC when federation pressure
  arrives or when autonomic continuous reconciliation becomes a forcing
  function.
- **Schema versioning on the ledger.** The ledger is append-only and
  hash-chained; on-disk event-body schema cannot be rewritten. What semantics
  govern old readers on new events, and new readers on old events. Specified
  in ADR-092.
- **Concurrency contract for substrate primitives.** Specified in ADR-092.
- **Substrate-to-Module boundary.** How Modules (e.g., `mod3`, written in
  Python; `cog-sandbox-mcp`, also Python) actually consume substrate
  primitives written in Go. Specified in ADR-092.
- **Where dispatch lives in the trichotomy.** The dispatch tool spans
  substrate-shaped concerns (capability routing as a substrate primitive),
  kernel-shaped concerns (execution within the agent loop), and module-shaped
  concerns (MCP exposure to external clients). Specified in ADR-092.

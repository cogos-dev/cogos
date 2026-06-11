---
type: adr
id: ADR-100
layer: spec
title: "ADR-100: Substrate Library Extraction From Current Package Structure"
created: 2026-05-19
status: accepted
tags: [adr, substrate, kernel, library-extraction, decomposition, supersedes-085]
author: chaz
refs:
  - rel: implements
    description: >
      The categorical cut; substrate-vs-kernel boundary; library extraction
      is the Phase 2 substrate-kernel split RFC. This ADR specifies the
      decomposition that Phase 2 executes. That RFC's §13 named "ADR-085
      advances to accepted" as the implementation gate; this ADR supersedes
      that gate. (internal reference omitted)
  - rel: supersedes
    description: >
      ADR-085 prescribed a per-provider decomposition into
      internal/providers/<name>/ subpackages. The actual architecture went to
      internal/engine/ as the primary dispatch core (250+ files). This ADR
      captures the current state and specifies the substrate-extraction path
      forward from where the code actually is. (internal reference omitted)
  - rel: composes-with
    description: >
      RFC-035 BlockClass — specifies the CogBlock/EventEnvelope unification
      (block.go:11-45 + ledger.go:23+ merge into a single ledger-entry type).
      The unified CogBlock is the canonical substrate atom. RFC-035 must land
      before Step 2 of the migration (pkg/substrate/block/ stabilization).
      (internal reference omitted)
  - rel: composes-with
    description: >
      RFC-036 WorkspaceClass — specifies Workspace as a Binding Pattern
      instance. The WorkspaceClaim schema lives in the substrate library;
      the WorkspaceReconciler lives in the kernel. Step 3 coordinates with
      RFC-036's schema landing. (internal reference omitted)
  - rel: composes-with
    description: >
      RFC-037 ChannelClass — specifies Channel as a Binding Pattern instance.
      Channel schema (channel_config.go, channel_types.go) moves to substrate;
      routing logic stays kernel. Step 3 coordinates with RFC-037.
      (internal reference omitted)
  - rel: composes-with
    target: "[ADR-091](091-substrate-as-named-architectural-layer.md)"
    description: >
      ADR-091 named the Substrate/Kernel/Module trichotomy and established
      that pkg/cogblock, pkg/reconcile, pkg/uri, pkg/bep, pkg/cogfield are
      already separately-versioned Go modules. ADR-100 operationalizes
      ADR-091's trichotomy as a concrete migration plan from the current
      package state.
  - rel: composes-with
    target: "[ADR-092](092-substrate-contracts-and-concurrency.md)"
    description: >
      ADR-092 specified the ledger-writer concurrency contract and boot-order
      semantics. The substrate library migration must preserve the
      single-writer-per-session invariant across module boundary moves.
---

# ADR-100: Substrate Library Extraction From Current Package Structure

## Status

**Accepted — 2026-05-19. Steps 1, 2a, and 2b merged.**

Supersedes ADR-085 as the prerequisite decomposition ADR for RFC-034 Phase 2.
ADR-085 is marked `superseded` (see body note added 2026-05-15); its
prescribed per-provider layout diverged in implementation. This ADR takes the
current codebase as its starting point rather than the ADR-085 plan.

Implementation progress:
- Step 1 (scaffold `pkg/substrate/` module) — merged #283
- Step 2a (`pkg/reconcile` re-export via `pkg/substrate/reconcile`) — merged #283
- Step 2b (`pkg/uri`, `pkg/cogfield`, `pkg/bep` re-exports) — merged #294
- Steps 3–6 pending

---

## Context

RFC-034 ratified the categorical cut between substrate (data + semantics +
protocol) and kernel (runtime + process + dispatch). Its §13 named "ADR-085
advances to accepted" as the gate for Phase 2. That gate cannot be cleared:

- ADR-085 frontmatter remains `proposed`; its body carries a "superseded-by-upstream-implementation" note added 2026-05-15.
- The prescribed layout (`internal/providers/<name>/` as the primary extraction target) was partially executed but the main dispatch core went elsewhere.
- `internal/engine/` grew to 250+ files and is the actual kernel dispatch core — not present in ADR-085's target layout at all.
- The root package (`package main`, `github.com/myrgic/cogos`) still has 218 `.go` files including `bus_*.go`, `hook_*.go`, `agent_*.go`, `discord_*.go`, `bep_*.go`, and `identity_*.go`.

Meanwhile, ADR-091 (Substrate as Named Architectural Layer) and ADR-092
(Substrate Contracts and Concurrency) ratified the trichotomy and its
operational contracts, confirming that the substrate packages already exist
as separate Go modules:

| Package | go.mod | Trichotomy classification |
|---|---|---|
| `pkg/cogblock/` | yes | Substrate — hash-chained ledger atom |
| `pkg/reconcile/` | yes | Substrate — Reconcilable interface + plan/apply types |
| `pkg/uri/` | yes | Substrate — cog:// URI scheme and projection semantics |
| `pkg/bep/` | yes | Substrate — block-exchange protocol (wire format) |
| `pkg/cogfield/` | yes | Substrate — cogfield schema primitives |
| `pkg/coordination/` | yes | Substrate-leaning — distributed coordination primitives |
| `pkg/modality/` | yes | Substrate/Module boundary — modality registration mechanism |
| `pkg/cogdoc_review/` | no | Substrate-leaning — cogdoc quality reconciler |
| `pkg/alias/` | no | Kernel-leaning — alias resolution, workspace-coupled |
| `pkg/skills/` | no | Substrate-leaning — skill registration and dispatch surface |

The gap is that no `pkg/substrate/` module exists. The substrate-shaped
packages are already versioned independently but lack a shared umbrella
module that expresses "these things travel together" and that downstream
callers (federation peers, language bindings, standalone tools) can import
as a single dependency.

### What ADR-085 actually delivered

ADR-085's `internal/providers/<name>/` model partially landed:
- `internal/providers/component/`, `internal/providers/daemon/`, `internal/providers/pin/`, `internal/providers/site/`, `internal/providers/vllm/` are present.
- The larger providers (Discord, BEP, Agent, MCP) remain at the root. Discord (`discord_provider.go`, `discord_hcl.go`, `discord_reconcile.go`), BEP (`bep_engine.go` through `bep_wire.go`), Agent (`agent_dispatch.go`, `agent_harness.go`, etc.), and MCP tools are all still `package main`.
- `internal/engine/` absorbed the kernel dispatch core: 250+ files covering agent control, bus sessions, context assembly, ledger queries, TRM, URI registry, speculative output, session forking, and more. This was not in ADR-085's plan.
- `internal/eval/`, `internal/linkfeed/`, `internal/workspace/` exist as smaller utility packages.

### The substrate-extraction gap

RFC-034 §3.1 lists what the substrate library should own: identity model,
CogBlock, cogdoc, ledger, Reconcilable contract, Binding Pattern, the .cog/
folder protocol, bus event format (shape only), memory sector taxonomy,
projection semantics and cog:// URI scheme, inheritance and policy framework,
capability framework.

Today that content is distributed across:
1. Separate `pkg/*` Go modules (already substrate-shaped, already versioned)
2. `internal/engine/` substrate-resident files (`cogblock.go`, `uri.go`, `uri_registry.go`, `ledger_query.go`, `trm*.go`)
3. Root-package files that mix substrate schema with kernel dispatch (`identity_crd.go`, `identity_provider.go`, `capability_resolver.go`, `capability_advertiser.go`, `session_manager.go`)

The substrate library does not exist as an importable unit. This ADR specifies
the path from current state to a `pkg/substrate/` module that third parties,
federation peers, and the kernel itself import.

---

## Decision

Establish `pkg/substrate/` as a new Go module (`github.com/myrgic/cogos/pkg/substrate`) that acts as the umbrella for the substrate-shaped packages. The migration proceeds in six ordered steps. Each step leaves the operating kernel in a passing state (`go build ./... && go test -count=1 ./...` green after each step).

### What moves into `pkg/substrate/`

The following content belongs in `pkg/substrate/` per RFC-034 §3.1:

| Substrate content | Current location | Target in pkg/substrate/ |
|---|---|---|
| Reconcilable interface + plan/apply types | `pkg/reconcile/types.go` | `pkg/substrate/reconcile/` (re-export) |
| cog:// URI scheme, extraction, namespace | `pkg/uri/` | `pkg/substrate/uri/` (re-export) |
| CogBlock struct + ledger chain | `pkg/cogblock/block.go:11-45`, `ledger.go:23+` | `pkg/substrate/block/` (after RFC-035 unification) |
| Bus event format (envelope shape only) | `pkg/cogblock/ledger.go` (EventEnvelope), `bus_event_format.go` (root) | `pkg/substrate/event/` |
| cogfield schema primitives | `pkg/cogfield/` | `pkg/substrate/cogfield/` (re-export) |
| Identity CRD schema | `identity_crd.go` (root) | `pkg/substrate/identity/` |
| Capability envelope vocabulary | `capability_resolver.go`, `capability_advertiser.go`, `capability_cache.go` (root) | `pkg/substrate/capability/` |
| Session schema (claims, lifecycle types) | `session.go`, `session_manager.go` type declarations (root) | `pkg/substrate/session/` |
| BEP wire format + protocol | `pkg/bep/` | `pkg/substrate/bep/` (re-export) |
| Channel schema (not routing) | `channel_config.go`, `channel_types.go` (root) | `pkg/substrate/channel/` |
| Memory sector taxonomy | currently implicit in engine | `pkg/substrate/memory/` |

### What does NOT move

Everything below stays kernel-resident. This list is explicit and normative:

- `internal/engine/` — all 250+ files. The dispatch core, agent control loop (`agent_controller.go`, `autonomic_ticker.go`), context assembly (`context_assembly.go`), TRM (`trm.go`, `trm_lightcone.go`, `trm_index.go`), speculative output, session fork mechanics, bus stream/consumer wiring. These are process + dispatch, not data + semantics.
- `internal/providers/*` — all provider implementations. Providers drive Reconcilers but are not the Reconcilable contract itself.
- `internal/eval/`, `internal/linkfeed/`, `internal/workspace/` — kernel-internal utilities.
- Root-package dispatch logic — `agent_dispatch.go`, `agent_harness.go`, `agent_bus_inlet.go`, `bus_router.go`, `bus_session.go`, `bus_watch.go`, bus tool routing, constellation singleton, hook implementations. These are the kernel's execution surface.
- `discord_provider.go`, `discord_hcl.go`, `discord_reconcile.go` — provider, stays at root or moves to `internal/providers/discord/` (ADR-085 completion, separate work).
- BEP engine and protocol implementation at root (`bep_engine.go` through `bep_wire.go`) — the BEP *wire format schema* moves to substrate; the engine that drives it stays kernel-resident.
- `modality_*.go` at root — the modality bus implementation and routing is kernel-resident even though `pkg/modality/` (registration mechanism) is substrate-leaning.
- Skills implementation (`pkg/skills/`) and cogdoc review (`pkg/cogdoc_review/`) — remain where they are pending their own forcing functions. Skills are Module-layer; cogdoc review is a Reconcilable application, not the Reconcilable contract.
- `pkg/coordination/`, `pkg/alias/` — remain at their current locations pending explicit forcing functions.

### Diagnostic rule

Per RFC-034 §3.3: if the behavior can be tested without a running process, it
belongs in the substrate library. If it requires a daemon, it belongs in the
kernel. Apply this check to any ambiguous case before filing a PR.

---

## Migration Steps

### Step 0: Mark ADR-085 `superseded`

Update the ADR-085 document frontmatter: `status: superseded`. Add
forward-reference to ADR-100. Update the substrate-kernel split RFC §13 gate
reference from "ADR-085 advances to accepted" to "ADR-100 advances to
accepted."

This is a documentation-only change. No code moves. Gate: doc references
consistent.

### Step 1: Create `pkg/substrate/` as empty Go module

Create `pkg/substrate/go.mod` with module path
`github.com/myrgic/cogos/pkg/substrate`. Add to `go.work`. No code yet — just
the module scaffold with a `doc.go` naming the layer.

Gate: `go build github.com/myrgic/cogos/pkg/substrate` succeeds.

### Step 2: Re-export `pkg/reconcile`, `pkg/uri`, `pkg/bep`, `pkg/cogfield` via substrate umbrella

Add thin re-export packages under `pkg/substrate/` for the already-versioned
modules. The re-exports are Go `package` wrappers that import the canonical
packages and re-export their public types — not copies. This lets a single
`import "github.com/myrgic/cogos/pkg/substrate"` pull the full substrate
surface without requiring callers to enumerate individual sub-modules.

Sequencing note: this step does not move any files. It creates new files only.
No import paths change in the kernel.

Gate: `go build ./pkg/substrate/...` succeeds. Existing tests pass.

### Step 3: Extract substrate-schema types from root package

Move the pure-schema (non-dispatch) type declarations for identity, capability,
session, channel, and bus event shape from the root `package main` files into
`pkg/substrate/<concern>/`:

- `identity_crd.go` schema types → `pkg/substrate/identity/` (keep IdentityProvider reconciler and RegisterProvider call at root, they reference internal/engine internals)
- `capability_resolver.go` type declarations → `pkg/substrate/capability/` (resolver implementation stays kernel)
- `session.go` type declarations → `pkg/substrate/session/`
- `channel_config.go`, `channel_types.go` schema types → `pkg/substrate/channel/`
- `bus_event_format.go` envelope type → `pkg/substrate/event/`

Each move is a separate PR. For each: extract the types, add import at the
root so existing code compiles, run the full test suite.

Coordinates with RFC-035 (BlockClass — CogBlock/EventEnvelope unification must
land before `pkg/substrate/block/` is stabilized), RFC-036 (WorkspaceClaim
schema), RFC-037 (ChannelClass schema).

Gate: `go build ./... && go test -count=1 ./...` green after each file group.

### Step 4: CogBlock/EventEnvelope unification → `pkg/substrate/block/`

After RFC-035 ratification: merge `pkg/cogblock/block.go:11-45` (CogBlock
routing/provenance fields) with `pkg/cogblock/ledger.go:23+` (EventEnvelope
hash/prior_hash/signature/seq fields) into a single unified ledger-entry type.
Promote the result to `pkg/substrate/block/`.

This is the load-bearing refactor described in the project_cogblock_intended_model
memory: CogBlock IS the ledger entry; the split is design drift. The unification
makes this structural in the code.

Maintain `pkg/cogblock/` as a thin shim importing `pkg/substrate/block/` for
one release cycle to allow dependents to migrate without a flag-day. The shim
carries a deprecation comment.

ADR-092 single-writer-per-session invariant must be enforced on the unified
type before this step is considered complete.

Gate: `go test -count=1 ./pkg/substrate/block/...` passes including ledger chain
integrity tests. `pkg/cogblock/` shim compiles and passes existing tests.

### Step 5: Import-path migration in kernel

Update `internal/engine/*` and root `*.go` to import substrate types from
`pkg/substrate/<concern>/` rather than the legacy `pkg/cogblock/`, `pkg/uri/`,
etc. paths (where those paths are now re-exports anyway). This is an
import-path-only change; no logic moves.

Remove the shims created in Step 4 after all kernel imports are updated.

Gate: `go build ./... && go test -count=1 ./...` green with no shim packages in
the import graph.

### Step 6: Separate-repo extraction (RFC-034 Q6 milestone)

When at least one forcing function from ADR-091 §Forcing Functions fires
(dependency inversion trigger, resource contention trigger, or deployment
divergence trigger), extract `pkg/substrate/` into a standalone repository
(`myrgic/substrate` or `myrgic/cogos-substrate`). The kernel then depends on
the substrate module as a regular external Go dependency.

This step is not scheduled here. Steps 1-5 put the codebase in a state where
Step 6 is a clean cut rather than a heroic refactor.

---

## Consequences

### Positive

- RFC-034 Phase 2 has a concrete, ordered migration path grounded in current code paths rather than a plan that diverged pre-ratification.
- Each step is independently reviewable and leaves the kernel operational.
- Third-party tools and federation peers gain a well-defined import surface without requiring a running kernel process — the primary motivation from RFC-034 §1.1.
- The CogBlock unification (Step 4) resolves the design drift documented in project_cogblock_intended_model: the code catches up to the intended model where every ledger entry is a cogblock, not a cogblock + envelope pair.
- The substrate trichotomy from ADR-091 becomes visible in the file tree, not just in documentation.

### Risks and mitigations

- **Root package still carries 218 files after Steps 1-5.** This ADR is scoped to substrate extraction, not full kernel decomposition. The ADR-085 per-provider work (moving Discord, BEP engine, Agent, MCP tool providers to `internal/providers/<name>/`) is parallel, not sequential. Those moves do not affect the substrate boundary.
- **Step 3 type extractions may surface hidden coupling.** Root-package schema types are sometimes entangled with dispatch logic in the same file. When extraction hits an entangled file, split it: schema types in a new `*_types.go` file first, then move. Never move dispatch logic to substrate.
- **Step 4 (CogBlock unification) is the highest-risk step.** The ledger chain integrity test in ADR-092 §1 must be added before this step begins. The AppendEvent concurrent-writer gap (no mutex on `pkg/cogblock.AppendEvent` today) must be closed in the same PR as the unification or the unified type has the same gap.
- **`pkg/modality/` is on the boundary.** The modality registration mechanism is substrate-shaped (extension registration without requiring a daemon); the modality bus routing and supervisor are kernel-resident. This ADR does not move `pkg/modality/` into `pkg/substrate/` — that is a forcing-function decision per ADR-091. Leave it where it is until a concrete use case requires it to be importable without the kernel.

---

## Open Questions

1. **Module path:** `github.com/myrgic/cogos/pkg/substrate` (subtree of main module) or `github.com/myrgic/substrate` (standalone module path from day one)? The subtree form makes Steps 1-5 cleaner but requires a rename at Step 6. The standalone form anticipates Step 6 but adds a cross-repo dependency immediately. Decision is gated on RFC-034 Q6 outcome.

2. **`pkg/coordination/` classification:** Distributed coordination primitives are substrate-leaning but have runtime behavior. Does `pkg/coordination/` belong in `pkg/substrate/coordination/` or stay kernel-adjacent? Defer to the first concrete federation use case that requires importing it without the kernel.

3. **`pkg/skills/` and `pkg/cogdoc_review/`:** Both are substrate-leaning Reconcilable applications with no `go.mod` of their own. Do they get `go.mod` files in Step 1, or wait for Step 5? Current leaning: wait — they are not on the critical path for RFC-034 Phase 2.

4. **`internal/engine/` substrate-resident files:** `internal/engine/cogblock.go`, `uri.go`, `uri_registry.go`, `ledger_query.go`, and `trm*.go` contain substrate-shaped logic (URI resolution, ledger queries, context assembly) inside the kernel's internal package. After Step 4, evaluate whether these can thin to thin wrappers over `pkg/substrate/` types, or whether their kernel coupling is too deep. Do not move them speculatively.

5. **ADR-085's incomplete provider moves:** The Discord, BEP engine, Agent, and MCP providers remain at root. Their move to `internal/providers/<name>/` is the unfinished ADR-085 work. This ADR does not schedule that work — it is parallel, not blocking. A follow-up ticket should track it as "ADR-085 completion" under this ADR's umbrella.

---

## Compose-With

| Document | Relationship |
|---|---|
| Substrate-Kernel Categorical Split RFC (internal reference omitted) | Parent RFC; this ADR is its decomposition prerequisite |
| BlockClass RFC (internal reference omitted) | Unification prerequisite for Step 4; must ratify before Step 4 begins |
| WorkspaceClass RFC (internal reference omitted) | Coordinates with Step 3 WorkspaceClaim schema extraction |
| ChannelClass RFC (internal reference omitted) | Coordinates with Step 3 Channel schema extraction |
| ADR-085 (superseded) | Historical record of the decomposition plan that diverged; this ADR is its replacement |
| [ADR-091 (Substrate as Named Architectural Layer)](091-substrate-as-named-architectural-layer.md) | Establishes the trichotomy; names the forcing functions that gate Step 6 |
| [ADR-092 (Substrate Contracts and Concurrency)](092-substrate-contracts-and-concurrency.md) | Specifies the ledger-writer contract that Step 4 must satisfy |

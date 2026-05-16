# ADR-092: Substrate Contracts and Concurrency

| Field       | Value                                                                          |
|-------------|--------------------------------------------------------------------------------|
| Status      | Accepted                                                                       |
| Author      | @chazmaniandinkle                                                              |
| Created     | 2026-05-15                                                                     |
| Parent      | [ADR-091 (Substrate as Named Architectural Layer)](091-substrate-as-named-architectural-layer.md) |
| Refs        | ADR-090 (Kind dispatch via registry), RFC-0005 (cog_fork_session), `pkg/cogblock/`, `pkg/reconcile/` |

## Context

[ADR-091](091-substrate-as-named-architectural-layer.md) names the Substrate as
an architectural layer with a ledger-first rule (§5) and a Reconcilable
primitive (§3), but defers the operational contracts those primitives depend
on. This ADR specifies those contracts and the concurrency model they imply.

The concerns addressed here surfaced during an independent architecture
review of ADR-091. Each concern names something the substrate currently
relies on implicitly that needs to be made explicit so future contributors
can reason about it and so future implementations can satisfy it.

The contracts fall into eight numbered sub-decisions. Each is marked
**Accepted**, **Documented status quo** (recording what is, not committing
to a change), or **Open question** (acknowledging an unresolved decision).

## Decision

### §1 — Ledger writer concurrency contract

**Status:** Accepted (going-forward contract; current implementation gap noted).

The ledger has a **single-writer-per-session** contract. For any given
session ID (the partition key under `.cog/ledger/<sessionID>/events.jsonl`),
at most one goroutine appends events at a time. The contract is enforced by
a per-session mutex held across the read-current-head → compute-chain-hash →
append window.

`pkg/cogblock.AppendEvent` does not currently serialize concurrent writers
to the same session ledger. Two concurrent callers will both `GetLastEvent`,
both compute the same `prior_hash`, both `O_APPEND`, and break the chain.
This is a known gap that consumers must work around today by routing all
appends through a single owning goroutine per session. A follow-up issue
tracks adding the mutex to `pkg/cogblock` itself; until that lands, callers
MUST NOT invoke `AppendEvent` from multiple goroutines for the same session
without external serialization.

Cross-session concurrency is unconstrained: different session ledgers may be
written by different goroutines without coordination.

### §2 — Boot order and replay semantics

**Status:** Accepted (going-forward contract; current implementation gap noted).

The substrate boot sequence is:

1. **Genesis check** — read `.cog/ledger/<sessionID>/genesis.json` (or
   equivalent) to confirm the substrate is initialized.
2. **Ledger replay** — read `events.jsonl` in order; for each event, apply
   to the appropriate in-memory derived view (identity registry, capability
   cache, session state, etc.). Replay completes before any service accepts
   external requests.
3. **Service registration** — the kernel's HTTP API, MCP server, and bus
   broker start. Modules may now consume substrate primitives.
4. **Reconcile loop start** — periodic / on-demand reconciliation begins.

Today, several derived views start empty rather than from ledger replay:
`LifecycleManager`, `SessionManager`, and the capability cache. Identity is
read from YAML files, not the ledger. This is **divergent from the
contract** — the in-memory state at boot reflects on-disk caches and
configuration, not authoritative ledger replay. The substrate currently
relies on these caches being kept in sync with the ledger by the writing
code paths.

A future RFC will move boot-time state reconstruction to ledger replay as
the authoritative source. Until then, the contract is documented but not
yet enforced.

### §3 — Crash recovery during reconcile

**Status:** Accepted (idempotency requirement).

`Reconcilable.ApplyPlan` MUST be idempotent against its own plan: running
the same plan twice on the same `liveState` MUST produce the same final
state and the same observable side effects. Consumers of the substrate
should assume `ApplyPlan` may be invoked after a crash on partially-applied
state, and write their implementations accordingly.

The substrate provides at-least-once semantics for `ApplyPlan` invocation,
not at-most-once. If the kernel dies between `ApplyPlan` succeeding and the
meta-reconciler's subsequent call to the package-level `reconcile.WriteState`
function persisting the post-apply state, the next reconcile cycle will
re-invoke `ApplyPlan` against the unchanged stored state. Reconcilables that
cannot tolerate replay (e.g., that send network requests with non-idempotent
semantics or that mutate external systems by side effect) MUST guard their
own apply path with operation IDs or other deduplication mechanisms.

The substrate itself does not provide such deduplication. Adding it is a
candidate future RFC.

### §4 — Reconcilable contract guarantees

**Status:** Accepted (formalizes the existing seven-method interface).

`pkg/reconcile/types.go::Reconcilable` requires seven methods. Each method's
contract:

| Method        | Determinism                | Side effects | Blocking          | Idempotency required |
|---------------|----------------------------|--------------|-------------------|----------------------|
| `Type()`      | Deterministic, pure        | None         | Non-blocking      | N/A                  |
| `LoadConfig()` | Deterministic given input config | Read from disk OK | Bounded by ctx | Yes (re-callable)    |
| `FetchLive()` | May depend on external state | Read-only observation of world | Bounded by ctx, MUST honor cancellation | Yes (multiple reads OK) |
| `ComputePlan()` | Deterministic given (live, config) | None — pure function | Non-blocking      | N/A                  |
| `ApplyPlan()` | Need not be deterministic; results may depend on world state | Mutates external systems | Bounded by ctx, MUST honor cancellation | **Required** — see §3 |
| `BuildState()` | Deterministic given (live, applied) | None — pure function | Non-blocking      | N/A                  |
| `Health()`    | May reflect transient state | None         | Fast (sub-second) | N/A                  |

Reconcilables that violate these contracts (e.g., `ComputePlan` reading from
disk, `BuildState` mutating state) are non-conforming and should be flagged
in code review.

`pkg/reconcile.RegisterProvider` panics on duplicate registration; this is
intentional. Reconcilables register at process init; duplicate registration
indicates a programming error and is not recoverable at runtime. Hot reload
of providers (re-registering at runtime) is not supported in the current
contract.

### §5 — Schema versioning and on-disk format evolution

**Status:** Accepted (going-forward contract).

The ledger is append-only and hash-chained. Once written, event payloads
cannot be rewritten or migrated in place. Schema evolution therefore happens
forward-only, and readers must tolerate cross-version mismatches in both
directions.

**Forward-compatibility (old reader, new event):**

- Each `EventPayload` carries a `schema_version` field (currently absent;
  follow-up issue will add it). Readers encountering a `schema_version`
  newer than they understand MUST log a structured warning and either skip
  the event or report it as opaque, depending on context. Readers MUST NOT
  panic, crash, or corrupt downstream state on an unrecognized version.

- Required-field additions in a new schema version are forbidden if old
  readers must still consume the event. New schema versions may add
  optional fields; required fields require a major version bump and a
  documented migration plan.

**Backward-compatibility (new reader, old event):**

- Readers MUST tolerate missing optional fields in old events, supplying
  defaults that match the original schema's semantics.
- If a field's semantics change across versions, the new reader MUST
  branch on `schema_version` rather than silently reinterpreting old data.

**CogBlockKind evolution** (per ADR-090): adding a new Kind is an additive
change; old readers without a registered handler for the new Kind invoke
the registry's `ErrNoKindHandler` path. Removing or renaming a Kind requires
a separate deprecation ADR and a compatibility shim period.

### §6 — Multi-kernel-on-one-substrate (today's status)

**Status:** Documented status quo.

The substrate today supports **exactly one kernel per substrate instance**.
The ledger is partitioned by session ID, the reconcile registry is a
process-local map, and capability advertisement is per-process. Two kernels
attempting to consume the same `.cog/` directory will:

- Race on ledger appends (per §1's gap).
- Diverge on in-memory derived views.
- Both attempt to register modality providers, causing duplicate-registration
  panics on overlapping provider names.

Multi-kernel-on-one-substrate is **not supported** today and not blocked
explicitly — operators who try it will get undefined behavior, not a clean
error. Future work to support it requires (at minimum): a cross-process
ledger writer protocol, distributed reconcile-registry coordination, and
identity registration with conflict-resolution semantics. None of this is
on the immediate roadmap.

This entry exists to make the today-status statement citable rather than
ambient. Future ADRs proposing multi-kernel work should reference this
section as the baseline.

### §7 — Substrate-to-Module boundary

**Status:** Accepted (documents the current contract).

Modules (e.g., `mod3`, `cog-sandbox-mcp`, future channel implementations)
consume substrate primitives via three surfaces:

| Surface              | Direction          | Protocol     | Allowed operations                           |
|----------------------|--------------------|--------------|----------------------------------------------|
| MCP server (`/mcp`)  | Module → Kernel    | JSON-RPC over Streamable HTTP | Tool calls into substrate-fronted MCP tools |
| HTTP API             | Module → Kernel    | REST/JSON    | Substrate-fronted endpoints (`/v1/sessions`, `/v1/ledger`, `/v1/agents`, etc.) |
| SSE bus              | Kernel → Module    | Server-Sent Events | Subscriptions to bus topics; one-way push (future surface — `internal/engine/bus_stream.go` implements the broker but no HTTP route is wired today; callers wire their own SSE) |

**Modules MAY NOT mutate substrate state directly.** All mutations route
through kernel-mediated calls — typically the kernel's MCP server or HTTP
API. The kernel applies authorization (per `capability_resolver.go`),
serializes writes (per §1), and ensures the ledger-first rule (per ADR-091
§5) holds.

In-process Go modules (same-binary consumers; currently none beyond the
kernel itself) MAY import substrate packages directly. The same contracts
apply in-process; the only difference is the absence of network overhead.

Module authors writing in languages other than Go (Python, TypeScript,
Rust) MUST use the network surfaces above and MUST NOT attempt to read or
write the on-disk ledger directly. The hash-chain integrity guarantees
require the writer to compute the chain hash from the current head; bypass
breaks the chain.

### §8 — Where dispatch lives in the trichotomy

**Status:** Accepted (resolves an ambiguity in ADR-091).

The dispatch system (`internal/engine/agent_dispatch.go`,
`internal/engine/agent_dispatch_query.go`, the `cog_dispatch_to_harness`
MCP tool) is a layer-crossing concern. Resolving where it "lives" by
component:

| Component                                        | SYSTEM-SPEC zone | Trichotomy layer |
|--------------------------------------------------|------------------|------------------|
| Capability routing primitive (which agent gets a task, given capability advertisements) | Workspace        | Substrate        |
| Dispatch executor (the goroutine that actually runs a dispatched task within the agent loop) | Nucleus          | Kernel           |
| MCP exposure (`cog_dispatch_to_harness` tool)    | Membrane         | Kernel           |
| Out-of-process dispatcher clients (e.g., Claude Code calling MCP) | (external)       | Module           |

Dispatch is therefore not "a module" or "a kernel concern" wholesale. It is
a substrate primitive (capability routing) consumed by the kernel
(execution) and exposed via the membrane (MCP) to modules (external
callers). Future ADRs touching dispatch should cite which component they
mean.

## Rationale

### Why one ADR covering eight concerns

These contracts are interdependent. §1 (concurrency) is what makes §3
(crash recovery) and §4 (Reconcilable guarantees) safe to rely on. §2 (boot
replay) is what makes §5 (schema versioning) work cross-restart. §6
(multi-kernel status) follows from §1's single-writer-per-session contract.
§7 (Module boundary) and §8 (dispatch placement) operationalize ADR-091's
trichotomy.

Splitting these across multiple ADRs would create cross-referencing
overhead and obscure the consistency of the model. Each concern is small
enough to fit in a sub-decision and large enough to matter; the ADR as a
whole is the right granularity.

### Why "Accepted" with implementation gaps

Several sub-decisions (§1, §2) document a *contract* that the current
implementation does not yet enforce. This is intentional: the contract
defines the going-forward invariant, and follow-up issues track the work to
bring the implementation into compliance. ADRs are architectural decisions,
not implementation status. The alternative — leaving these as "Open
question" — would mean future contributors have no canonical reference for
what the substrate is supposed to do, only what it currently does.

The implementation gaps are flagged explicitly in each sub-decision and
should be tracked as follow-up issues.

### Why not put the dispatch placement (§8) in ADR-091

ADR-091's trichotomy section would have been substantially longer with §8
inline, and dispatch is one of several layer-crossing concerns that may
need similar resolution in the future. Treating §8 as a worked example in
ADR-092 (alongside Module boundary in §7) keeps ADR-091 focused on the
layer definitions themselves.

## Consequences

### Positive

- Substrate primitives have citable contracts. New consumers can reference
  ADR-092 §1–§8 instead of reading code to discover implicit semantics.
- Implementation gaps are made explicit. Follow-up issues tracking work to
  close the gaps have clear acceptance criteria.
- Module authors in non-Go languages (mod3 in Python) have a canonical
  reference for what they can and cannot do.
- Dispatch's layer-crossing nature is acknowledged rather than hand-waved.
- Schema-versioning rules pre-empt the "what does an old reader do on a new
  event" question that will inevitably surface.

### Negative

- Several sub-decisions document contracts the current implementation does
  not yet meet. Until follow-up issues land, the contract and the code are
  out of sync. This is honest but requires discipline to track.
- Eight sub-decisions in one ADR is at the upper end of granularity. Future
  authors looking for "the ledger concurrency rule" will need to find §1
  inside this ADR rather than a dedicated document. The table of contents
  at the top of the Decision section helps.

### Neutral

- Some sub-decisions (especially §5 schema versioning, §6 multi-kernel)
  describe future scenarios that may never materialize. The cost of having
  them documented but never invoked is low; the cost of needing them and
  not having them is high. They stay.

## Implementation

The following follow-up issues are implied by this ADR. None are authorized
by this ADR; each requires its own implementation plan:

1. **Add per-session mutex to `pkg/cogblock.AppendEvent`** (closes §1
   implementation gap).
2. **Add `schema_version` field to `EventPayload`** with default-handling
   logic in readers (closes §5 implementation gap).
3. **Move boot-time state reconstruction to ledger replay** (closes §2
   implementation gap). Likely a multi-PR effort spanning identity,
   capability, session, and lifecycle managers.
4. **Document the Reconcilable contracts in `pkg/reconcile/doc.go`** so
   the contracts are visible to implementers without reading this ADR.
5. **Add static analysis or linting** for Reconcilable contract violations
   (e.g., a `go vet`-style check that `ComputePlan` doesn't import
   `os` or `net/http`).

## Open questions

- **At-most-once dispatch / apply.** §3 establishes at-least-once semantics
  for `ApplyPlan`. Some prospective Reconcilables (e.g., one that initiates
  outbound HTTP calls with non-idempotent side effects) would benefit from
  at-most-once. Whether the substrate provides operation-ID-based
  deduplication is deferred to a future RFC.
- **Watch-based observation.** §4's `FetchLive` is poll-based today. A
  watch-based variant (subscribe to changes rather than poll) is implicit
  in the K8s-style target referenced in ADR-091 §4 and is deferred to a
  future RFC alongside the level-triggered evolution.
- **Cross-process Reconcilable registration.** §4's registry is process-
  local. Federation (multiple kernels coordinating) will require cross-
  process registration semantics; deferred until federation is on the
  roadmap.
- **Module identity and capability advertisement.** §7 says Modules consume
  substrate via kernel-mediated surfaces, but Modules themselves have
  identities (per `mod3`'s session model) that the substrate doesn't
  currently anchor. Whether Modules register as substrate-visible observer
  identities, and what authorization that would imply, is a future ADR.

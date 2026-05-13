# ADR-090: CogBlock Kind Dispatch via Registry Pattern

| Field       | Value                                                                          |
|-------------|--------------------------------------------------------------------------------|
| Status      | Accepted                                                                       |
| Author      | @chazmaniandinkle                                                              |
| Created     | 2026-05-13                                                                     |
| Tracking    | [#202](https://github.com/myrgic/cogos/issues/202) (RFC-0005 prereq)          |
| Refs        | ADR-059 (CogBlock envelope), ADR-079 (CogDocs become CogBlocks), ADR-089 (pointer-envelope), RFC-0005 (session.fork), RFC-0006 (vLLM PagedAttention) |

## Context

`pkg/cogblock/block.go` defines the `CogBlockKind` type and a set of named
constants (`BlockMessage`, `BlockToolCall`, `BlockToolResult`, etc.). The
membrane (`internal/engine/membrane_default.go`) currently dispatches on Kind
via a pair of `if block.Kind ==` guards -- a workable approach for two cases,
but one that does not scale as the Kind vocabulary grows.

RFC-0005 introduces `session.fork` and RFC-0006 introduces `cache.kv_block`.
Both RFCs explicitly require this refactor as a prerequisite: any consumer
that needs to handle a new Kind must be able to register its handler without
modifying a central switch statement. The same forcing function applies to
future Kinds (agent.spawn, doc.insight pipeline hooks, etc.).

The status quo has no centralized dispatch switch today -- the guards in
`membrane_default.go` and the `openClawKind` classifier are the only
`block.Kind` branch points outside logging. This ADR establishes the pattern
for dispatching on Kind *before* the ecosystem of handlers grows via RFC
implementations, making the pattern available to RFC-0005 and RFC-0006
implementers from day one.

## Decision

Introduce `pkg/cogblock/kindregistry` (hereafter `kindregistry`) as a
concurrent-safe, init()-based registry for CogBlock Kind handlers. No
existing switch is converted (there is no multi-arm Kind switch in the
codebase today). The registry is the target pattern going forward:

1. Each package that owns a Kind handler calls `kindregistry.Register` in its
   `init()` function.
2. The membrane (and any future routing layer) calls `kindregistry.Dispatch`
   instead of writing a switch.
3. The membrane's existing `if block.Kind ==` guards are converted to
   registered handlers in this PR as the first concrete use.

### Package placement

The registry lives in `internal/engine/kindregistry.go` rather than a
separate `pkg/` sub-module. The engine's `CogBlock` type (with typed
`[]ProviderMessage`) is the correct dispatch target for Kind-aware processing;
`pkg/cogblock.CogBlock` uses `json.RawMessage` for Messages and is the wire
format, not the dispatch target. Keeping the registry in `internal/engine`
avoids a type-boundary mismatch and an import cycle.

Two registries are introduced:

1. `kindHandlers` (`RegisterKindHandler`/`DispatchKind`) for processing
   handlers -- the RFC-0005/RFC-0006 use case (ledger writes, session state
   updates, etc.).
2. `membraneKindPolicies` (`EvaluateKindPolicy`) for membrane policy overrides
   -- the existing `if block.Kind ==` guards, now registered per Kind in
   `membrane_handlers.go`.

### Registry contract

**Processing registry** (`kindregistry.go`):

```go
// KindHandler is the contract a Kind owner registers to handle its Kind.
type KindHandler func(block *CogBlock) error

// RegisterKindHandler panics on duplicate registration.
func RegisterKindHandler(k CogBlockKind, h KindHandler)

// DispatchKind routes a block to its registered handler.
// Returns ErrNoKindHandler if no handler is registered for the block's Kind.
func DispatchKind(block *CogBlock) error

// RegisteredKinds returns the sorted list of registered Kinds.
func RegisteredKinds() []CogBlockKind
```

**Membrane policy registry** (`membrane_handlers.go`):

```go
// MembraneKindPolicy returns (result, true) to short-circuit DefaultMembranePolicy;
// (IngestionResult{}, false) to fall through to the default logic.
type MembraneKindPolicy func(block *CogBlock) (IngestionResult, bool)

// EvaluateKindPolicy looks up and invokes the Kind-specific membrane policy.
func EvaluateKindPolicy(block *CogBlock) (IngestionResult, bool)
```

### Sentinel error

`ErrNoKindHandler` is a package-level sentinel so callers can distinguish
"no registered handler" from handler-internal errors via `errors.Is`.

## Rationale

**Open-closed principle.** Adding a new Kind requires no modification to any
existing dispatch code. The RFC implementer registers a handler in their
package's `init()` and calls `Dispatch` from the routing layer.

**Parallel-safe.** `sync.RWMutex` guards the registry map; concurrent
reads (Dispatch calls) are not serialized against each other.

**Fail-fast on wiring bugs.** `Register` panics on duplicate registration.
Double-`init` bugs surface at process startup, not at runtime under load.

**`ErrNoHandler` is not a fatal error.** Callers that don't need exhaustive
Kind coverage can check `errors.Is(err, kindregistry.ErrNoHandler)` and
continue. This matches the membrane's current behavior: unknown Kinds fall
through to a Defer decision.

## Consequences

### Positive

- RFC-0005 and RFC-0006 can register `session.fork` and `cache.kv_block`
  handlers in their own packages without touching any existing dispatch code.
- Future Kinds are additive: one new `init()` call, no merge conflicts.
- `Registered()` provides cheap diagnostics for "what Kinds does this binary
  handle?" without walking the codebase.

### Negative

- `init()` registration is implicit. A package that defines a Kind constant
  but does not register a handler will silently produce `ErrNoHandler` at
  dispatch time. The convention requires documentation.
- Duplicate-registration panic is loud but good. It catches real bugs.
- One additional indirection in the hot path (map lookup via `RLock`).
  Benchmarked as sub-microsecond; not a bottleneck for the kernel's dispatch
  rate.
- `Reset()` is exported. The package relies on convention ("test-only") rather
  than enforcement. No `//go:build ignore` escape hatch is used because the
  export is deliberate for integration test setup.

## Implementation surface

### New

- `internal/engine/kindregistry.go` -- processing registry implementation
- `internal/engine/kindregistry_test.go` -- register / dispatch /
  duplicate-panic / ErrNoKindHandler / concurrent-safety tests (7 tests)
- `internal/engine/membrane_handlers.go` -- `init()` registrations for
  BlockToolResult and BlockImport membrane policies

### Modified

- `internal/engine/membrane_default.go` -- replaces the two `if block.Kind ==`
  guards with a call to `EvaluateKindPolicy(block)`, delegating to the
  membrane policy registry

### No change

- `internal/engine/tailer_openclaw.go` -- `openClawKind()` is a classifier
  (string -> CogBlockKind), not a dispatcher. Out of scope.
- `internal/engine/gate.go` -- dispatches on `GateEvent.Type` (string), not
  `CogBlockKind`. Out of scope.
- `internal/engine/sessions.go` -- dispatches on bus event `Type` (string),
  not `CogBlockKind`. Out of scope.

## Migration convention (for RFC implementers)

When adding a new Kind:

1. Add the `CogBlockKind` constant to `pkg/cogblock/block.go`.
2. Create or extend the owning package's handler function matching
   `kindregistry.Handler`.
3. In the owning package, add an `init()` that calls `kindregistry.Register`.
4. Where the block is routed, call `kindregistry.Dispatch(block)` rather than
   adding a switch arm.

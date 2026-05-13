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

The registry lives in `pkg/cogblock/kindregistry/` rather than
`internal/kind/` because Kind registration is a kernel-wide cross-cutting
concern. Packages under `internal/engine/` need to register handlers; packages
under `pkg/` (e.g. future provider packages) may also need to register. Placing
the registry in `pkg/` avoids an import cycle.

### Registry contract

```go
// Handler is the contract a Kind owner registers to handle its Kind.
// The returned error is surfaced to the caller of Dispatch.
type Handler func(block *cogblock.CogBlock) error

// Register associates a handler with a Kind.
// Panics if the Kind is already registered (catches double-init bugs early).
func Register(k cogblock.CogBlockKind, h Handler)

// Dispatch routes a block to its registered handler.
// Returns ErrNoHandler if no handler is registered for the block's Kind.
func Dispatch(block *cogblock.CogBlock) error

// Registered returns the sorted list of registered Kinds (for diagnostics).
func Registered() []cogblock.CogBlockKind

// Reset clears the registry. For use in test packages only.
func Reset()
```

### Sentinel error

`ErrNoHandler` is a package-level sentinel so callers can distinguish
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

- `pkg/cogblock/kindregistry/registry.go` -- registry implementation
- `pkg/cogblock/kindregistry/registry_test.go` -- register / dispatch /
  duplicate-panic / ErrNoHandler / concurrent-safety tests

### Modified

- `internal/engine/membrane_default.go` -- convert the two `if block.Kind ==`
  guards to registered handlers; `Evaluate` calls `Dispatch` for those cases
- `internal/engine/membrane_handlers.go` (new file) -- `init()` registrations
  for the membrane's Kind handlers (BlockToolResult, BlockImport)

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

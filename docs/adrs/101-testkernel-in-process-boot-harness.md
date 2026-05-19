---
cog:
  version: "1.0.0"
  type: adr
  id: ADR-101
  layer: spec

title: "ADR-101: testkernel — In-Process Boot Harness for Daemon-Level Integration Testing"
created: 2026-05-19
status: accepted
tags: [adr, kernel, testing, testkernel, reconcile, mcp, integration-testing]
author: chaz
refs:
  - uri: cog://adr/092
    rel: composes-with
    description: >
      ADR-092 specifies the Reconcilable contract (LoadConfig → FetchLive →
      ComputePlan → ApplyPlan → BuildState → WriteState) and the single-writer
      concurrency invariant. testkernel must not break either.
  - uri: cog://adr/095
    rel: composes-with
    description: >
      ADR-095 specifies ReconcileDaemon — the running loop that testkernel
      wraps. The Trigger/WaitForReconcile API in testkernel aligns with
      ReconcileDaemon.Trigger. PollInterval=0 semantics are specified here.
  - uri: cog://adr/091
    rel: composes-with
    description: >
      ADR-091 named the Substrate/Kernel/Module trichotomy. testkernel lives
      at the Kernel layer: it wraps the running engine, not the substrate
      libraries. Downstream modules (constellation, mod3) that need to
      integration-test against a real kernel instance access it through
      testkernel's exported API.
  - uri: cog://adr/100
    rel: composes-with
    description: >
      ADR-100 is extracting the substrate library from internal/engine/. The
      testkernel package shape must be stable against that extraction: it depends
      on engine.Boot (a new function, not the current monolithic engine.Main),
      not on internal engine types directly. ADR-100 changes to package layout
      do not break testkernel callers.
---

# ADR-101: testkernel — In-Process Boot Harness for Daemon-Level Integration Testing

## Status

**Accepted — 2026-05-19. Phases 1 and 2 merged.**

Implementation progress:
- Phase 1 (`engine.Boot` factored out of `runServe`; `internal/testkernel` scaffold) — merged #289
- Phase 2 (`WithIsolatedRegistry` for provider injection in testkernel) — merged #295
- Phase 3 (downstream cross-module testkernel adoption) pending

---

## Context

### The gap

The CogOS kernel daemon currently has no in-process test harness. `engine.Main()`
in `internal/engine/cli.go` is the only entry point for starting the full
runtime. It:

- parses OS flags
- calls `os.Exit` on failure
- writes daemon state to disk
- binds to a real OS port
- installs signal handlers
- starts goroutines for the process loop, HTTP server, reconcile daemon, and
  local harness controller
- runs until `SIGINT` or `SIGTERM`

None of this is testable in-process. There is no way to pass configuration
programmatically, obtain a handle to the running kernel, call into its tool
registry, or drive the reconcile loop deterministically from a test.

### What is currently deferred because of this gap

PR #284 (identity wave 6b — wire `IdentityProvider` through `RegisterProvider`)
and PR #285 (RBAC binding CRDs — `RBACProvider`) both cite missing
end-to-end test coverage for the daemon lifecycle. Specifically:

- Can the MCP tool `cog_list_agents` return results that reflect state produced
  by a completed reconcile cycle?
- Does `IdentityProvider.ApplyPlan` emit the correct substrate events when
  driven through the full kernel stack (not a unit-test stub)?
- Does the HTTP `/health` endpoint report the correct identity and workspace
  after nucleus load?
- Do multiple providers registered in the same process interact correctly
  under the reconcile daemon's serial execution order?

These are daemon-level behavioral questions. They cannot be answered by the
existing test patterns.

### Existing test patterns and why they are insufficient for Gap B

**Gap A (per-provider e2e, `reconcile_e2e_test.go`):** The root-package e2e
tests exercise individual providers in isolation: write fixture files, call
`provider.LoadConfig` → `provider.ComputePlan` → `provider.ApplyPlan`
directly, assert filesystem outcomes. This is the right pattern for
verifying provider logic. It does not exercise the daemon loop, the HTTP
server, the MCP transport, or the interaction between providers.

**`reconcile_daemon_test.go`:** The daemon tests exercise `ReconcileDaemon`
directly using stub `noopReconcilable` and `errorReconcilable` providers.
They verify the daemon's loop semantics (tick interval, trigger mechanism,
error isolation, shutdown timing). They do not start an HTTP server, a
nucleus, or an MCP server. They do not exercise real provider implementations.

`testkernel` (this ADR) fills Gap B: daemon-level behavior tested through the
kernel's real surfaces (HTTP, MCP tool registry, reconcile loop with real
providers) in a single in-process goroutine cluster, without modifying the
production daemon or requiring a subprocess.

The three test patterns are additive, not competing:

| Pattern | Scope | When to use |
|---|---|---|
| `reconcile_e2e_test.go` (Gap A) | Per-provider, filesystem | Provider logic, CRD parsing, file output correctness |
| `reconcile_daemon_test.go` | Daemon loop, stub providers | Loop semantics, timing, error isolation, state transitions |
| `testkernel` (Gap B, this ADR) | Full kernel, real surfaces | HTTP+MCP behavior, provider-to-tool interaction, nucleus load, cross-provider ordering |

### What reading the production code revealed

`runServe` in `internal/engine/cli.go` is the actual boot sequence. It:

1. Calls `RegisterProviders()` (set by `cmd/cogos/providers_wire.go`; nil in
   tests today — this hook already exists).
2. Sets up the logger and loads config.
3. Calls `SetProvidersWorkspace()` (also set by `providers_wire.go`; nil in
   tests today — this hook also already exists).
4. Checks `planServeState` to detect a running daemon and exit early (the
   daemon-already-running guard — must be bypassed in tests).
5. Builds daemon state and writes it to disk (must be avoided in test workspaces
   or scoped to a temp dir).
6. Loads nucleus.
7. Builds `Process`, optional TRM, router, `Server`.
8. Wires `busSessions` publisher.
9. Starts telemetry (no-op if no collector).
10. Installs signal handlers.
11. Creates `ReconcileDaemon` and starts it.
12. Starts projection watchers for each `AllProjectionKinds`.
13. Starts process and server goroutines.
14. Blocks on signal or goroutine error.

The key insight: the `RegisterProviders` and `SetProvidersWorkspace` hooks
are already there and already nil-safe. The `planServeState` and
`buildDaemonState`/`saveDaemonState` calls are the main obstacles. The signal
handler is the other. Factoring `runServe` into a `Boot(opts...) *Kernel`
function requires moving signal handling out, removing the `os.Exit` calls, and
making config injectable rather than flag-derived. The internal structure is
cleanly separable.

The `pkg/reconcile/registry.go` `ResetProviders()` function already exists —
this was confirmed. No new public surface is needed on the registry for the
basic hybrid isolation approach described in §Decision 3.

---

## Decision

### §1 — Package location: `internal/testkernel/`

**Chosen: `internal/testkernel/`.**

Rationale: downstream modules (constellation, mod3) do not currently have a
defined need for kernel integration testing in this cycle. The cost of making
the package exported before its API is stable is higher than the cost of
later promoting it from `internal/` if the need becomes concrete. The `cogos`
module is the substrate kernel; callers that need kernel integration tests
should embed or wrap it, not import a test helper. If a constellation or mod3
integration test needs a real kernel, the right mechanism is a subprocess or
a shared Docker Compose fixture, not importing `internal/testkernel`.

Alternative considered: `pkg/testkernel/` (exported). Rejected because it
creates a permanent public API commitment for a test-only package before the
engine.Boot refactor (§2) has settled. Exported test helpers that wrap
internal engine state invite coupling that outlives the test gap they solve.

Alternative considered: `internal/engine/testkernel/` (colocated with engine).
Rejected because `internal/engine/` already has 250+ files. Colocation adds
noise to the primary dispatch package and makes import paths longer for test
code that lives at the root package or in `cmd/`.

`internal/testkernel/` is visible to all packages within the `cogos` module,
can be imported by `internal/engine/*_test.go`, root-package tests, and
`cmd/cogos/` tests, and signals clearly that this is test infrastructure.

### §2 — Boot model: `engine.Boot(opts...) *Kernel`

**Chosen: factor a new `engine.Boot(opts...) *Kernel` out of `runServe`;
`engine.Main()` becomes a thin wrapper.**

Rationale: this is the only option that gives tests a programmatic handle to
the running kernel without subprocess overhead or environment variable
contortion. The subprocess approach (os/exec) is too slow for integration
tests that run in CI on every PR and provides no path to direct tool-call
inspection. Environment-variable injection into `engine.Main()` avoids the
refactor but leaves flag parsing and `os.Exit` in the call path and makes
parallel test runs impossible.

The refactor is bounded: `runServe` in `cli.go` becomes a call site for
`Boot` followed by a blocking wait. `engine.Main()` remains the public API
for the binary; it delegates to `runServe` which delegates to `Boot`. No
existing caller changes. The signal handler stays in `runServe` / `engine.Main()`;
`Boot` takes a `context.Context` and does not install signal handlers.

The `Boot` function returns a `*Kernel` handle:

```go
// Kernel is an opaque handle to a running kernel instance.
// Obtained via Boot; released via Stop.
type Kernel struct { ... }

// Boot starts a kernel in-process with the given options.
// The kernel runs until ctx is cancelled or Stop is called.
// Boot returns when all subsystems are ready (HTTP health check passes).
func Boot(ctx context.Context, opts ...BootOption) (*Kernel, error)
```

`engine.Main()` is unchanged externally. `runServe` wraps `Boot` and adds
signal context + daemon state file management. The daemon state management
(`buildDaemonState` / `saveDaemonState`) stays in `runServe`, not in `Boot`,
so test instances do not write pid files.

### §3 — Provider registration isolation: hybrid (default global, opt-in isolated)

**Chosen: hybrid — global registry by default; `WithIsolatedRegistry(providers...)` boots with a per-call registry copy that is discarded on `Stop`.**

Rationale: The global registry pattern (`RegisterProvider` / `init()` blocks
in production `cmd/cogos/providers_wire.go`) exists for good reason: production
boot is a one-shot initialization where global state is appropriate. Changing
all production `init()` blocks to instance-scoped registration is a breaking
change that carries significant risk and touches every provider.

`ResetProviders()` already exists in `pkg/reconcile/registry.go`. The cheapest
option (test-scoped reset + serial tests) is viable for small test suites but
fails under `t.Parallel()`: two tests that call `ResetProviders()` in their
cleanup can race when a third test's `Boot` fires between the reset and the
re-register.

The hybrid resolves this: `testkernel.Boot(ctx, WithIsolatedRegistry(p1, p2))`
installs the given providers into the running kernel's `ReconcileDaemon` and
tool-dispatch layer without touching the global registry. The isolation is at
the `ReconcileDaemon` level: the daemon is constructed with an explicit provider
list rather than reading `reconcile.ListProviders()`. This requires
`ReconcileDaemonConfig` to accept an optional `Providers []reconcile.Reconcilable`
field. When that field is non-nil, the daemon operates on it directly rather
than reading the global map. When nil, it reads the global map as today.

This is the minimal change to `ReconcileDaemon` that enables isolation without
restructuring the global registry. It is backward-compatible: all existing
daemon tests and production code continue to use the global map path.

Default behavior (no `WithIsolatedRegistry`): `Boot` uses whatever is in the
global registry at call time. Tests that use this path must coordinate
(or call `reconcile.ResetProviders()` + register their providers before `Boot`,
and clean up after `Stop`). These tests must not run in parallel.

With `WithIsolatedRegistry`: the provided set is the complete provider
population for this kernel instance. Safe for parallel tests.

### §4 — Tool invocation API: direct `kernel.CallTool`

**Chosen: `kernel.CallTool(ctx, name, args) (result, error)` — direct dispatch
into the kernel's tool registry, bypassing the MCP transport.**

Rationale: the MCP transport path (stand up a real MCP client, connect to the
kernel's SSE endpoint, send an `initialize` + `tools/call` round-trip) is
correct for transport-level tests but too slow and fragile for the majority of
integration tests. A 300ms connection setup cost per test is prohibitive when
the test suite has dozens of tool-invocation assertions.

The HTTP layer option is intermediate: it exercises the HTTP router and
middleware but not the MCP protocol framing. It is the right choice for
`/health`, `/v1/sessions`, and other HTTP-native endpoints.

`testkernel` exposes both:

```go
// CallTool dispatches directly into the kernel's MCP tool registry.
// Bypasses transport framing; exercises the handler function directly.
// Returns the tool result as raw JSON bytes.
func (k *Kernel) CallTool(ctx context.Context, name string, args map[string]any) ([]byte, error)

// HTTPClient returns an *http.Client pre-configured to reach the
// kernel's HTTP server. Tests use this for /health, /v1/*, etc.
func (k *Kernel) HTTPClient() *http.Client

// Endpoint returns the base URL of the kernel's HTTP server.
func (k *Kernel) Endpoint() string
```

The direct `CallTool` is the primary API for MCP-layer integration tests.
The HTTP client is the secondary API for transport-level and HTTP-behavior tests.

Full MCP transport tests (connecting a real MCP client via stdio or SSE) are
out of scope for this ADR. They belong in a separate `cmd/cogos/mcp_*_test.go`
or a dedicated subprocess test. They are not blocked by this ADR.

### §5 — Reconcile-loop control: trigger-based, aligned with ReconcileDaemon

**Chosen: `kernel.TriggerReconcile(providerType)` + `kernel.WaitForReconcile(ctx, providerType)`,
composing with the existing `ReconcileDaemon.Trigger` mechanism.**

Rationale: the tick-based approach (`SetTickInterval(0)` + `RunReconcileCycle()`)
gives deterministic synchronous control but requires adding a "manual drive"
mode to `ReconcileDaemon` that does not exist today and creates API surface
with no production use. The existing `Trigger` mechanism in
`ReconcileDaemon` is already tested and is the production integration point
for `ProjectionWatcher`.

`testkernel` wraps `ReconcileDaemon.Trigger` with a blocking
`WaitForReconcile` that polls the daemon state. For tests that need full
synchronous control, `Boot` with a long `PollInterval` (say, 24 hours)
effectively disables the periodic tick; tests drive reconciliation exclusively
via `TriggerReconcile` + `WaitForReconcile`.

```go
// TriggerReconcile queues an immediate reconcile for the named provider.
// Non-blocking: matches ReconcileDaemon.Trigger semantics.
func (k *Kernel) TriggerReconcile(providerType string)

// WaitForReconcile blocks until a reconcile cycle for the named provider
// completes, or ctx is cancelled. The cycle must have started after the
// call to WaitForReconcile (not a previously completed cycle).
func (k *Kernel) WaitForReconcile(ctx context.Context, providerType string) error
```

`WaitForReconcile` is implemented by attaching an observer to the daemon's
cycle completion signal (a new lightweight hook on `runOneCycle` — a channel
written on each cycle completion per provider type). This does not require
exposing `ReconcileDaemon` internals; the completion channel is managed by
the `Kernel` handle and wired at `Boot` time.

### §6 — Lifecycle contract: synchronous shutdown with goroutine-leak detection

**Chosen: `(*Kernel).Stop()` guarantees synchronous shutdown of all
kernel-owned goroutines, and optionally asserts no goroutine leaks using
`uber-go/goleak`.**

Rationale: the substrate is designed for long-running daemon health. A test
harness that allows goroutine leaks between tests defeats the daemon-health
property it is meant to test. The goleak integration is the right default
because:

1. Leaked goroutines in one test corrupt timing and state for subsequent tests.
2. The kernel's goroutine budget is bounded and known: process loop, HTTP
   server, reconcile daemon, session-activity publisher, optional local harness
   controller. Any goroutine that escapes this set is a bug.
3. goleak has a standard integration pattern for `TestMain` and per-test
   cleanup functions; the cost is one import and a few lines.

The file-descriptor leak check is deferred to Phase 4 (it requires platform-
specific `/proc/self/fd` enumeration or the `lsof` approach and adds
meaningful complexity). Goroutine-leak detection is the higher-value check
and is cross-platform.

`Stop` contract:

```go
// Stop cancels the kernel's context, waits for all goroutines to exit
// within a 10-second deadline, and (if goleak is enabled) asserts no
// goroutines leaked relative to the baseline captured at Boot time.
// Returns an error if the deadline is exceeded or a leak is detected.
func (k *Kernel) Stop() error
```

In tests, the typical usage is `t.Cleanup(func() { require.NoError(t, k.Stop()) })`.

---

## Proposed API sketch

```go
package testkernel

import (
    "context"
    "net/http"
    "time"

    "github.com/myrgic/cogos/internal/engine"
    "github.com/myrgic/cogos/pkg/reconcile"
)

// BootOption is a functional option for Boot.
type BootOption func(*bootConfig)

// WithWorkspace sets the workspace root. Defaults to t.TempDir() equivalent
// (a temporary directory created at Boot time and removed on Stop).
func WithWorkspace(path string) BootOption

// WithPort sets the HTTP port. Defaults to 0 (OS-assigned ephemeral port).
func WithPort(port int) BootOption

// WithIsolatedRegistry registers the given providers into this kernel
// instance only, without touching the global pkg/reconcile registry.
// Safe for parallel test execution.
func WithIsolatedRegistry(providers ...reconcile.Reconcilable) BootOption

// WithPollInterval sets the reconcile daemon's tick interval.
// Pass a large value (e.g., 24*time.Hour) to disable periodic ticking
// and drive reconciliation exclusively via TriggerReconcile.
func WithPollInterval(d time.Duration) BootOption

// WithLeakCheck enables goroutine-leak detection on Stop.
// Requires uber-go/goleak in the test binary.
func WithLeakCheck() BootOption

// WithoutDaemonStateFile suppresses the daemon state file write.
// Always true by default in testkernel (production-only behavior).
// Exposed as an explicit option for documentation clarity.
func WithoutDaemonStateFile() BootOption

// Kernel is an opaque handle to a running in-process kernel instance.
type Kernel struct { /* unexported fields */ }

// Boot starts a kernel in-process with the given options.
// Blocks until the kernel's /health endpoint returns 200 or ctx expires.
// Returns an error if boot fails or health check times out (default 10s).
func Boot(ctx context.Context, opts ...BootOption) (*Kernel, error)

// Stop cancels the kernel's context, waits for goroutines to exit,
// and optionally checks for goroutine leaks.
// Safe to call multiple times (idempotent).
func (k *Kernel) Stop() error

// CallTool dispatches a named MCP tool directly into the kernel's tool
// registry. Bypasses transport framing. Returns the raw JSON result bytes.
func (k *Kernel) CallTool(ctx context.Context, name string, args map[string]any) ([]byte, error)

// HTTPClient returns an *http.Client pre-configured to target this kernel.
// The client's base URL is Endpoint(). It does not follow redirects.
func (k *Kernel) HTTPClient() *http.Client

// Endpoint returns the base URL of the kernel's HTTP server (e.g., "http://127.0.0.1:54321").
func (k *Kernel) Endpoint() string

// WorkspaceRoot returns the workspace root path used by this kernel.
func (k *Kernel) WorkspaceRoot() string

// TriggerReconcile queues an immediate reconcile for the named provider.
// Non-blocking. Equivalent to ReconcileDaemon.Trigger.
func (k *Kernel) TriggerReconcile(providerType string)

// WaitForReconcile blocks until a reconcile cycle for the named provider
// completes after this call returns, or ctx is cancelled.
// Returns an error if the provider is unknown or ctx expires.
func (k *Kernel) WaitForReconcile(ctx context.Context, providerType string) error

// ReconcileDaemonState returns the current ReconcileDaemonState.
func (k *Kernel) ReconcileDaemonState() engine.ReconcileDaemonState
```

### Typical test pattern

```go
func TestIdentityProvider_MCPRoundTrip(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    provider := identity.NewProvider() // real provider under test
    k, err := testkernel.Boot(ctx,
        testkernel.WithIsolatedRegistry(provider),
        testkernel.WithPollInterval(24*time.Hour), // no auto-tick
        testkernel.WithLeakCheck(),
    )
    require.NoError(t, err)
    t.Cleanup(func() { require.NoError(t, k.Stop()) })

    // Drive one reconcile cycle.
    k.TriggerReconcile("identity")
    require.NoError(t, k.WaitForReconcile(ctx, "identity"))

    // Call the MCP tool and inspect the result.
    result, err := k.CallTool(ctx, "cog_list_agents", map[string]any{})
    require.NoError(t, err)

    var resp struct {
        Agents []map[string]any `json:"agents"`
    }
    require.NoError(t, json.Unmarshal(result, &resp))
    require.Greater(t, len(resp.Agents), 0)
}
```

---

## Consequences

### Enabled

- Integration tests for PR #284 (`IdentityProvider` → `cog_get_agent_state`,
  `cog_list_agents` round-trip through reconcile + MCP).
- Integration tests for PR #285 (`RBACProvider` → `cog_get_state` binding
  verification after ApplyPlan).
- HTTP behavior tests: `/health` endpoint carrying correct workspace +
  identity fields after nucleus load.
- Cross-provider ordering tests: two providers registered in the same
  `WithIsolatedRegistry` call, verifying reconcile daemon's serial execution
  order and that one provider's state is visible to a subsequent tool call.
- Regression harness for daemon lifecycle: `ReconcileDaemonState` transitions
  visible from the test binary without polling an external endpoint.
- Goroutine-leak regression: any goroutine that escapes the kernel's known
  budget is caught at test time rather than manifesting as a flaky daemon in
  production.

### Costs

- **engine.Boot refactor.** `runServe` in `cli.go` must be split into a
  `Boot(ctx, cfg, opts)` function and a thin `runServe` wrapper. This is a
  medium-effort, low-risk refactor (the logic is already in one function;
  the main change is replacing `os.Exit` calls with error returns and
  extracting config from flags into a struct). Estimated effort: one focused
  PR, no behavioral change to production boot.
- **ReconcileDaemonConfig.Providers field.** The isolated-registry path adds
  an optional `Providers []reconcile.Reconcilable` field to
  `ReconcileDaemonConfig` and a branch in `runTick` and `runTriggered` to use
  it when non-nil. Low risk: the nil path is the existing path.
- **Per-test workspace creation.** `Boot` with `WithWorkspace("")` creates a
  temp directory. Tests that do not pass `WithWorkspace` get an ephemeral
  workspace that is removed on `Stop`. This avoids disk leaks but means each
  test boots a nucleus that reads from an empty workspace (no existing cogdocs,
  no prior state). Tests that need pre-populated state must pass
  `WithWorkspace(path)` and populate fixtures before calling `Boot`.
- **Test suite latency.** Each `Boot` call starts goroutines, opens a TCP
  listener, and initializes a nucleus. On a developer machine, the overhead
  is expected to be 50–200ms per test. Tests should be scoped to scenarios
  where this overhead is justified (daemon-level behavior); per-provider logic
  stays in `reconcile_e2e_test.go` (Gap A).
- **goleak dependency.** `uber-go/goleak` is a test-only import. It is already
  a transitive dependency in many Go projects; adding it to `go.mod` (test
  build tags only) has negligible impact. If the module policy restricts it,
  the leak check can be deferred to Phase 4 and the `WithLeakCheck` option
  becomes a no-op until then.

### Risks

- **Isolated registry race under concurrent Boot.** The isolated-registry path
  writes to a `ReconcileDaemonConfig.Providers` slice that is set before the
  daemon goroutine starts. As long as `Boot` completes setup before returning
  (and it must, by contract), there is no race. The risk is if a caller
  mutates the `Providers` slice after passing it to `WithIsolatedRegistry`; the
  option function must copy the slice, not reference it.
- **WaitForReconcile false negative.** If `TriggerReconcile` fires and the
  cycle completes before `WaitForReconcile` installs its observer, the wait
  will block until the next trigger or ctx expiry. The mitigation is to
  implement `WaitForReconcile` with a lightweight per-provider completion
  channel that captures all completions after `Boot`, not just completions
  after the `Wait` call. Tests that need determinism should call
  `WaitForReconcile` before `TriggerReconcile` returns (or use a channel
  captured before the trigger). The API documentation must make the ordering
  contract explicit.
- **goleak false positives.** The standard library's HTTP server holds a
  goroutine alive briefly after `Shutdown` returns. goleak's `IgnoreTopFunction`
  can whitelist known-benign goroutines. Phase 4 should maintain a stable
  whitelist as part of the testkernel package.
- **engine.Boot API stability.** If the Boot function signature changes during
  the ADR-100 substrate extraction, callers of `testkernel.Boot` are insulated
  (they call `testkernel.Boot`, not `engine.Boot` directly), but the
  `testkernel` implementation must be updated. This is acceptable — test
  infrastructure is explicitly allowed to track engine internals. The `internal/`
  boundary makes this expectation explicit.

---

## Implementation phases

### Phase 1 — `engine.Boot` factor-out + basic harness (prerequisite for all other phases)

**Scope:** Refactor `runServe` into `engine.Boot(ctx context.Context, cfg *Config, opts ...bootOption) (*Kernel, error)`. `runServe` becomes a one-screen wrapper. `engine.Main()` is unchanged externally.

**Deliverables:**
- `internal/engine/boot.go` — `Boot` function, `Kernel` type, `BootOption` type.
- `internal/engine/cli.go` — `runServe` delegates to `Boot` + installs signal context and daemon state file management.
- Basic `(*Kernel).Stop()` with context cancel + goroutine drain (no leak check yet).
- `(*Kernel).Endpoint()` and `(*Kernel).WorkspaceRoot()`.
- `internal/testkernel/testkernel.go` — thin wrapper over `engine.Boot` with `WithWorkspace`, `WithPort`, `WithPollInterval` options.
- One integration test in `internal/testkernel/testkernel_test.go`: boot, hit `/health`, call `Stop`, verify no error.

**Not included:** `WithIsolatedRegistry`, `CallTool`, `WaitForReconcile`, goleak.

### Phase 2 — Isolated registry + reconcile control

**Scope:** `WithIsolatedRegistry`, `TriggerReconcile`, `WaitForReconcile`.

**Deliverables:**
- `ReconcileDaemonConfig.Providers []reconcile.Reconcilable` field + branch in `runTick` / `runTriggered`.
- Completion-signal channel wired into `runOneCycle`.
- `(*Kernel).TriggerReconcile`, `(*Kernel).WaitForReconcile`.
- `(*Kernel).ReconcileDaemonState`.
- Integration tests: two isolated-registry boots in the same test binary (parallel), verifying no cross-contamination.

**Unblocks:** PR #284, PR #285 integration tests.

### Phase 3 — Tool invocation API

**Scope:** `kernel.CallTool` + `kernel.HTTPClient`.

**Deliverables:**
- `(*Kernel).CallTool` — direct dispatch into the tool registry via the same handler table that the MCP server uses.
- `(*Kernel).HTTPClient` — pre-configured `http.Client`.
- Integration tests: `CallTool("cog_get_state", ...)` after a completed reconcile cycle.

**Dependency:** Phase 2 (need `WaitForReconcile` to know when the cycle is done before asserting tool output).

### Phase 4 — Goroutine-leak detection + hardening

**Scope:** goleak integration, whitelist maintenance, `WithLeakCheck` option.

**Deliverables:**
- `uber-go/goleak` added to `go.mod` (test-only).
- `WithLeakCheck` option wired into `Stop`.
- Initial goroutine whitelist for known-benign goroutines (HTTP server drain, TLS handshake goroutines if TLS is enabled).
- Leak-check coverage on at least the Phase 1 and Phase 2 integration tests.

---

## Naming caveat

The package is named `testkernel`. This is a plain descriptive name for a test
helper that boots a kernel. No Eigen, EigSight, HyperCycle, or other
research-vocabulary terms appear in this package name or its API per the
standing rule on substrate naming (`feedback_substrate_naming_forward_direction.md`).
The API surface uses Go-idiomatic names: `Boot`, `Stop`, `CallTool`,
`TriggerReconcile`, `WaitForReconcile`. None of these encode framework
terminology.

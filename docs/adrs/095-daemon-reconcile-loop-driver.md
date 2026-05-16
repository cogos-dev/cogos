# ADR-095: Daemon Reconcile Loop Driver

| Field       | Value                                                                          |
|-------------|--------------------------------------------------------------------------------|
| Status      | Accepted                                                                       |
| Author      | @chazmaniandinkle                                                              |
| Created     | 2026-05-16                                                                     |
| Parent      | [ADR-091 §2 trichotomy — Layer = Kernel](091-substrate-as-named-architectural-layer.md) |
| Refs        | ADR-092 §1 (single-writer-per-session), §3 (idempotent ApplyPlan), §4 (Reconcilable contract), §6 (single-kernel today-status), §7 (authorization gate) |

## Context

ADR-092 §2 specifies the substrate boot sequence:

1. Genesis check
2. Ledger replay
3. Service registration
4. **Reconcile loop start** — periodic / on-demand reconciliation begins.

Step 4 is documented but not implemented. Today, `internal/engine/cli_reconcile.go`
is the only site that executes the full reconcile cycle (LoadConfig → FetchLive →
ComputePlan → ApplyPlan → BuildState → WriteState). It is a one-shot CLI command
(`cogos reconcile <type>`), not a daemon-resident loop.

The consequences of this gap:

- Registered Reconcilables (e.g., `ProjectionReconciler` from ADR-094) are never
  driven at the daemon level. Their state diverges from declared config over time
  until an operator manually runs the CLI.
- The autonomic ticker (`autonomic_ticker.go`) probes `Health()` on all providers
  each tick but does not run reconcile cycles. Health probes detect drift but do
  not correct it.
- Specific watch mechanisms (e.g., `ProjectionWatcher` from ADR-094) fire a
  trigger callback when file-system events arrive, but today nothing wires that
  callback to a full reconcile cycle.

This ADR closes the gap by specifying and implementing `ReconcileDaemon`: a
daemon-resident goroutine that periodically iterates registered Reconcilables and
drives the full per-Reconcilable reconcile cycle.

## Decision

### §1 — Layer placement

`ReconcileDaemon` is a **Kernel**-layer concern per the ADR-091 §2 trichotomy:

- It requires an agent-loop-executing process to be running (it is a goroutine
  resident in the daemon).
- It consumes a **Substrate** primitive (`pkg/reconcile.Reconcilable` + the
  process-local registry via `reconcile.ListProviders` / `reconcile.GetProvider`).
- It does not project through a specific modality surface (that is the Module layer).

### §2 — Contract

`ReconcileDaemon` provides the following guarantees:

| Property                  | Guarantee                                                                                                                  |
|---------------------------|----------------------------------------------------------------------------------------------------------------------------|
| Periodic tick             | Reconcile all registered providers at configurable interval (default 30s).                                                 |
| Per-provider error isolation | A panic or error in one provider's cycle MUST NOT block or terminate the cycles of other providers. Errors are logged and the provider is counted as `stalled` for that tick. |
| Idempotent cycle          | Each reconcile cycle conforms to ADR-092 §3: ApplyPlan is idempotent. The daemon does not add further at-most-once guarantees. |
| Context-aware shutdown    | The daemon exits its loop when `context.Context` is cancelled. In-flight cycles are allowed to complete (graceful) within a configurable shutdown grace period. |
| Reconcilable contract     | The daemon calls methods in the ADR-092 §4 order: LoadConfig → FetchLive → ComputePlan → ApplyPlan → BuildState → WriteState. |
| Single-writer rule        | The daemon does not bypass the single-writer-per-session ledger contract (ADR-092 §1). Each provider's reconcile cycle runs sequentially within the daemon (no two goroutines run the same provider concurrently). |

### §3 — State machine

The daemon tracks a coarse lifecycle state:

| State      | Meaning                                                              |
|------------|----------------------------------------------------------------------|
| Starting   | Goroutine not yet running (zero value).                              |
| Live       | Goroutine running; last tick completed without fatal error.          |
| Stalled    | Running; last tick had one or more provider errors. Daemon continues ticking. |
| Shutdown   | Context cancelled; goroutine has exited.                             |

State transitions: `Starting → Live` (on first tick start), `Live ↔ Stalled`
(on per-tick error presence), `{Live,Stalled} → Shutdown` (on context cancel).

### §4 — Watch-trigger integration

Specific Reconcilables may implement event-driven early reconciliation outside
the periodic tick (e.g., `ProjectionWatcher` from ADR-094 fires on file-system
writes). The daemon driver is the **general** mechanism; watchers are **specific
instances** that may enqueue an early trigger.

The integration contract:

- The daemon exposes a `Trigger(providerType string)` method that queues an
  immediate reconcile for the named provider (non-blocking; drops if already
  queued).
- Watchers receive a trigger callback at construction time; callers wire this
  to `daemon.Trigger(providerType)`.
- The daemon drains the trigger queue between periodic ticks. A queued trigger
  for provider P causes P's cycle to run immediately rather than waiting for the
  next tick; after the cycle, P is removed from the queue.

### §5 — Configuration

| Parameter               | Default | Description                                                         |
|-------------------------|---------|---------------------------------------------------------------------|
| `PollInterval`          | 30s     | How often all providers are iterated in the periodic tick loop.     |
| `MaxConcurrent`         | 1       | Maximum number of providers reconciled concurrently. Default 1 serializes all providers within a tick per ADR-092 §1 single-writer recommendation. |
| `ShutdownGracePeriod`   | 5s      | How long the daemon waits for in-flight cycles to complete after context cancel before returning. |

Rationale for default `MaxConcurrent = 1`: ADR-092 §1 documents the
single-writer-per-session concurrency gap in `pkg/cogblock.AppendEvent`. Until
the per-session mutex lands, serializing provider cycles avoids the chain-break
risk for providers that emit ledger events. Future work may raise this to N once
§1 is closed.

### §6 — Wiring at daemon boot

The daemon is started in `runServe` (`internal/engine/cli.go`) after service
registration (ADR-092 §2 step 3) and before the HTTP server begins serving
external requests. This places the reconcile loop at boot step 4 per the §2
sequence.

**Coordination with Orchestrator A's I1 (ProjectionWatcher boot wiring):**
`ReconcileDaemon` subsumes the narrow ProjectionWatcher.Start() wiring from I1.
The watcher trigger is wired to `daemon.Trigger("lineage-projection-<kind>")` at
daemon construction time, making the watcher an early-trigger source for the
general loop rather than an independent mechanism.

If Orchestrator A's I1 lands first (ProjectionWatcher.Start() at boot as a
standalone call), this ADR's wiring replaces that standalone call by routing
the same trigger through the daemon.

### §7 — Telemetry

Each provider reconcile cycle emits:

- A structured slog record at INFO level: provider type, duration, plan summary
  (creates/updates/deletes/skipped), error if any.
- An OTel span: `reconcile.daemon.cycle` with attributes `provider.type`,
  `plan.creates`, `plan.updates`, `plan.deletes`, `plan.skipped`, `cycle.duration_ms`.

These are observatory surfaces: reconcile operations are projections through the
lineage + autonomic observatories, not silent background work.

### §8 — Migration

Before this ADR: no daemon reconcile loop exists. The on-disk state for any
Reconcilable reflects the last manual `cogos reconcile <type>` invocation or the
last time a CLI operator ran the command.

After this ADR: the daemon runs the full reconcile cycle for all registered
providers on each periodic tick. Existing state files are compatible: the daemon
passes them to `BuildState` per the standard cycle.

No migration of existing state files is required. The daemon's first tick
reads each provider's existing state (if any) and produces a new plan from it.

## Rationale

### Why Kernel, not Substrate

Reconcile cycle execution requires a running process with a context, a goroutine
scheduler, and access to external systems (via FetchLive and ApplyPlan). The
`pkg/reconcile` interface and registry are Substrate primitives (they exist
independent of any agent loop), but the daemon driving them is Kernel (it
requires an agent-loop-executing process).

### Why serial by default (MaxConcurrent = 1)

The ADR-092 §1 gap is an active implementation risk. Until `pkg/cogblock.AppendEvent`
adds the per-session mutex, concurrent provider cycles could break ledger chains
for any Reconcilable that emits ledger events. Serial execution is the safe default
at no functional cost: providers are expected to complete in under 1s in the
typical case (filesystem operations, cogdoc reads).

### Why the daemon owns the watcher trigger, not the watcher

ProjectionWatcher and future event-driven watchers are Reconcilable-specific.
They are not general substrate primitives. Routing their trigger through the
general daemon makes the daemon the authoritative source of reconcile cycle
scheduling; watcher events become early-trigger signals into the same loop rather
than separate reconcile paths with separate error handling and telemetry.

## Consequences

### Positive

- ADR-092 §2 step 4 is implemented. Boot sequence is now complete.
- Registered Reconcilables are driven continuously. Declared-vs-live drift is
  corrected within one poll interval of occurring.
- Telemetry provides observatory-visible reconcile activity without operator
  intervention.
- Per-provider error isolation prevents one bad provider from stalling the loop.

### Negative

- The daemon now runs background I/O work. A misconfigured Reconcilable with a
  slow `FetchLive` (e.g., one waiting on an unreachable external service) will
  consume the full `ctx`-bounded timeout each tick. Operators must set appropriate
  timeouts inside their `FetchLive` implementations.
- MaxConcurrent = 1 means a slow provider delays all subsequent providers in the
  same tick. Future work: per-provider timeout + raise MaxConcurrent once §1 is
  closed.

### Neutral

- Existing `cogos reconcile <type>` CLI command is unchanged. The daemon loop
  and the CLI command are independent paths through the same provider interface.
  Operators may still run one-shot CLI reconciliations at any time.

## Implementation

Files:

- `internal/engine/reconcile_daemon.go` — `ReconcileDaemon` type and goroutine
- `internal/engine/reconcile_daemon_test.go` — integration tests
- `internal/engine/cli.go` — wiring at daemon boot (step 4)
- `docs/adrs/095-daemon-reconcile-loop-driver.md` — this file

The follow-up issue implied by this ADR:

1. **Raise MaxConcurrent once ADR-092 §1 per-session mutex lands** — document
   in the §1 follow-up issue as a dependent task.
2. **Per-provider FetchLive timeout** — configurable per-provider timeout so a
   slow external system doesn't consume the full tick budget.

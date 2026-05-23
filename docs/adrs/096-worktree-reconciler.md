# ADR-096: WorktreeReconciler — Substrate Resource Lifecycle as Reconcilable

| Field       | Value                                                                          |
|-------------|--------------------------------------------------------------------------------|
| Status      | Accepted                                                                       |
| Author      | @chazmaniandinkle                                                              |
| Created     | 2026-05-17                                                                     |
| Layer       | Substrate + Kernel (per [ADR-091](091-substrate-as-named-architectural-layer.md) §2) |
| Refs        | [ADR-091](091-substrate-as-named-architectural-layer.md) (substrate as named layer, ledger-first rule), [ADR-092](092-substrate-contracts-and-concurrency.md) §3 (crash recovery + idempotency), §4 (Reconcilable contract), [ADR-093](093-managed-session-processes-for-attachment.md) (ManagedSession, process PIDs as substrate concerns), [ADR-095](095-daemon-reconcile-loop-driver.md) (ReconcileDaemon, provider registration, watch-trigger integration), `pkg/reconcile/`, `internal/engine/reconcile_daemon.go`, `internal/engine/projection_reconciler.go` |

## Context

### Concrete evidence: five orphaned worktrees, zero ledger entries

On 2026-05-13 a wave of dispatch operations created five parallel worktrees in the `mod3` repository to implement a modality schema wave. As of 2026-05-17 those worktrees remain on disk:

```
/Users/slowbro/workspaces/myrgic/mod3/.claude/worktrees/mod3-modality-rfc        [wave/2026-05-13-mod3/modality-rfc]
/Users/slowbro/workspaces/myrgic/mod3/.claude/worktrees/mod3-modality-schemas    [worktree-mod3-modality-schemas]
/Users/slowbro/workspaces/myrgic/mod3/.claude/worktrees/mod3-pipecat             [wave/2026-05-13-mod3/pipecat]
/Users/slowbro/workspaces/myrgic/mod3/.claude/worktrees/mod3-sidecar-doc         [wave/2026-05-13-mod3/sidecar-doc]
/Users/slowbro/workspaces/myrgic/mod3/.claude/worktrees/mod3-worker-cli          [wave/2026-05-13-mod3/worker-cli]
```

Each holds real unmerged commits against `main`. The `cogos` repository has two additional agent-locked worktrees from in-flight or completed dispatch sessions:

```
/Users/slowbro/workspaces/myrgic/cogos/.claude/worktrees/agent-a5647032c84cb9e61  [worktree-agent-a5647032c84cb9e61] locked
/Users/slowbro/workspaces/myrgic/cogos/.claude/worktrees/agent-a8c054cc717aa7cb4  [(detached HEAD)] locked
```

The substrate cannot safely automate cleanup of any of these seven worktrees because **no ledger entry exists that binds them to the dispatch identities that created them**. The substrate has no way to answer:

- Was `mod3-pipecat` created for dispatch `abc123`? Is that dispatch terminal? Did the branch merge upstream?
- Does `agent-a8c054cc717aa7cb4` hold uncommitted work in progress, or is it a stale artifact from a completed dispatch?
- Which of these can be removed automatically without risking data loss?

Without answers, any automated cleanup is a bet. The wrap-up orchestrator that surfaced this correctly deferred — defer was the right answer. But defer compounds over time: today it's seven worktrees; in three months it is seventy.

The root cause is not that the orchestrator needs better heuristics. The root cause is **structural**: worktrees were created outside the substrate's ledger. Cleanup by definition cannot be automated when the state that would authorize cleanup was never recorded.

### Why this is a substrate-layer failure, not a cleanup failure

ADR-091 §5 states the ledger-first rule: where substrate primitives have both an authoritative record (the ledger) and a derived view (in-memory state, filesystem artifacts), **the ledger is written first**. Worktrees are filesystem state, which is derived. If the ledger entry precedes the `git worktree add`, the substrate has authority over the worktree from its first byte. If the ledger entry is never written, the worktree is not a substrate resource — it is filesystem state that happens to be adjacent to the substrate.

The orphan-by-design case is: a dispatch creates a worktree, terminates, and the worktree persists with no record of intent. This only happens when state exists outside the ledger. The fix is not "remember to clean up" — that is a discipline fix, and ADR-092's rationale rejects discipline fixes for structural problems. The fix is to make the worktree a substrate Reconcilable from the moment it exists.

Per `pkg/reconcile/doc.go`: *"the reconcile primitive is the substrate's mechanism for maintaining declared state against live state."* A worktree without a ledger entry has no declared state. It cannot participate in the reconcile loop. It cannot be the subject of `liveState → targetState` analysis. It is not a Reconcilable; it is filesystem state. The fix is to bind every substrate-spawned worktree to a Reconcilable at creation.

### The broader pattern: substrate-spawned ephemeral resources

Worktrees are one instance of a general pattern. Any resource the substrate creates and does not record in the ledger can become an orphan by the same mechanism:

- **Process PIDs** — ADR-093 introduces `ManagedSession`; the process itself is ledger-bound through the session record. But PIDs spawned outside `ManagedSession` (early dispatch paths, one-shot eval runners) are not.
- **Temporary directories** — created by dispatch scaffolding, cleaned up by `defer os.RemoveAll`, but if the process panics between creation and cleanup, they persist.
- **Lock files** — advisory locks held by a process identity. If the process exits without releasing, nothing identifies the stale lock or authorizes removal.
- **Channel sessions** — ADR-093 `ManagedSession` has a lifecycle record and a reconcile shape; the session ID is the binding. This is the most mature instance of the pattern and serves as reference implementation.
- **Dispatch task IDs** — created when a harness dispatch begins; no explicit terminal record when the dispatch silently fails.

WorktreeReconciler is the specific instance for `git worktree`-managed filesystem resources. The ADR generalizes the pattern in §6 for future implementers.

## Decision

### §1 — WorktreeReconciler is a Substrate primitive

`WorktreeReconciler` is a **Substrate** layer primitive per ADR-091 §2. It satisfies the field-of-existence test: a worktree's binding to a dispatch identity exists independently of any agent loop executing. The worktree either was created by dispatch `X` or it wasn't; the ledger records that independent of whether any agent is currently running.

The daemon that *drives* WorktreeReconciler (scheduling ticks, isolating errors) is `ReconcileDaemon` (ADR-095), which is Kernel layer. The two-layer composition is intentional and follows the same shape as `ProjectionReconciler`: the Reconcilable itself is Substrate, the driver is Kernel.

### §2 — Spawn API precondition: ledger entry before `git worktree add`

A new substrate-level function `substrate.SpawnWorktree` is the exclusive entry point for substrate-managed worktrees:

```go
// SpawnWorktree creates a new worktree and registers it as a substrate Reconcilable.
// The ledger entry is written BEFORE the git worktree add call.
// Returns the worktree path and handle, or an error if the ledger write fails.
// Per ADR-091 §5: ledger-first rule applies; if the ledger write fails, no worktree
// is created.
func SpawnWorktree(ctx context.Context, opts WorktreeOpts) (*WorktreeHandle, error)

type WorktreeOpts struct {
    DispatchID   string    // ID of the dispatch that is requesting this worktree
    Branch       string    // Branch name to create or check out
    Base         string    // Base commit or ref (HEAD of main, a specific SHA, etc.)
    RepoRoot     string    // Absolute path to the repository root
    WorktreeRoot string    // Directory under which the worktree is created
}

type WorktreeHandle struct {
    Identity   string    // Canonical identity: worktree-{dispatch_id} or worktree-{nanoid}
    Path       string    // Absolute path to the worktree on disk
    Branch     string    // Branch checked out in this worktree
    Base       string    // Base ref the branch was cut from
    CreatedAt  time.Time
    DispatchID string    // Bound dispatch identity
}
```

The ledger event written at spawn:

```json
{
  "kind": "worktree.created",
  "worktree_id": "worktree-{dispatch_id}",
  "dispatch_id": "{dispatch_id}",
  "repo_root": "/abs/path/to/repo",
  "worktree_path": "/abs/path/to/worktree",
  "branch": "adr-096-implementation",
  "base": "abc123",
  "created_at": "2026-05-17T..."
}
```

If the ledger write fails, `SpawnWorktree` returns an error and no `git worktree add` is issued. If the `git worktree add` fails after the ledger write succeeds, the ledger entry remains and the ReconcileDaemon's next tick will observe `liveState = path_does_not_exist` for a worktree with a creation record — this is a valid alarm condition, not a chain-break.

### §3 — WorktreeReconciler contract

`WorktreeReconciler` implements `pkg/reconcile.Reconcilable` per ADR-092 §4.

```go
// WorktreeReconciler maintains the substrate's declared worktree set against
// the live filesystem and git worktree registry.
type WorktreeReconciler struct {
    RepoRoot     string
    LedgerReader LedgerReader  // reads worktree.created / worktree.terminal events
    GitAdapter   GitAdapter    // wraps git worktree list, git status, branch queries
}
```

**`LoadConfig(ctx, workspaceRoot)`**: reads all `worktree.created` and `worktree.terminal` ledger events for this `RepoRoot`. Derives the declared worktree set: worktrees that have a creation event and no terminal event (or a terminal event with `reason=merged` or `reason=abandoned`).

**`FetchLive(ctx)`**: runs `git worktree list --porcelain` against `RepoRoot`. For each live worktree path, queries:
- Does the path still exist on disk?
- What is the current branch HEAD? Does it have uncommitted changes?
- Is the branch present on upstream? Has it been merged into main?

**`ComputePlan(live, config)`**: pure function. Classifies each known worktree into one of four states and produces the corresponding plan action:

| Classification | Condition | Plan action |
|---|---|---|
| `alive` | Bound dispatch is not terminal; or dispatch terminal but branch not yet merged/abandoned. | leave |
| `removable-clean` | Dispatch terminal AND branch upstream-merged (or explicitly abandoned via `worktree.terminal` event). No uncommitted changes. | prune |
| `alarm-uncommitted-on-terminal-dispatch` | Dispatch terminal AND worktree has uncommitted changes or unmerged local-only commits. | alarm (do not auto-prune) |
| `alarm-unknown-binding` | Worktree exists on disk but no matching ledger creation event. | alarm (surface for operator; do not auto-prune) |

**`ApplyPlan(ctx, plan)`**: executes plan operations:
- `prune`: runs `git worktree remove --force {path}` then emits `worktree.pruned` ledger event.
- `alarm`: emits a structured `worktree.alarm` ledger event with classification, worktree path, dispatch ID (if known), and diagnostic details. Does not mutate filesystem.
- `leave`: no-op.

Per ADR-092 §3, `ApplyPlan` is idempotent: calling `git worktree remove` on a path that no longer exists is a no-op; emitting a second `worktree.pruned` or `worktree.alarm` event for the same worktree produces a second ledger entry (acceptable; alarm events are informational) but no filesystem side effect. `ApplyPlan` MUST guard against double-prune by checking existence before attempting removal.

**`BuildState(live, applied)`**: produces the `WorktreeState` summary written by `WriteState`. Includes per-worktree classification, last-reconcile timestamp, alarm count.

**`Health()`**: returns `Healthy` if last tick had no errors; `Degraded` if one or more alarm events fired in the last N ticks; `Unhealthy` if `FetchLive` or `ApplyPlan` returned a non-alarm error.

### §4 — Operator-decision policy for terminal-dispatch worktrees with uncommitted work

The `alarm-uncommitted-on-terminal-dispatch` classification is the most sensitive case. The dispatch that created the worktree has terminated, but the worktree contains changes that were never committed or commits that were never merged upstream. Automated pruning would lose that work.

**Policy: alarm, never auto-prune.** The ReconcileDaemon will surface these via `worktree.alarm` ledger events and via `Health() = Degraded`. Operator intervention is required to resolve:

- Option A: commit, push, open PR, then emit `worktree.terminal{reason=merged}` — this moves the classification to `removable-clean` and the next reconcile tick prunes automatically.
- Option B: explicitly abandon by emitting `worktree.terminal{reason=abandoned}` — if the operator confirms the uncommitted work is not valuable, the reconciler will prune on the next tick.
- Option C: re-bind the worktree to a new dispatch by emitting `worktree.rebind{new_dispatch_id=...}` — extends the lifecycle without losing the work.

The alarm is not a failure state; it is the substrate surfacing operator-required decisions that it cannot safely make unilaterally. This matches the "observatory-visible" principle from ADR-095 §7: reconcile operations are projections, not silent background work.

### §5 — ReconcileDaemon registration

`WorktreeReconciler` is registered in `ReconcileDaemon` at daemon boot (after service registration, per ADR-092 §2 step 3). One `WorktreeReconciler` instance is registered **per repo root** that the kernel manages. For a single-repo deployment, one instance. For a multi-repo workspace deployment, one instance per active repo root.

Per ADR-095 §4, `WorktreeReconciler` may also implement a file-system watcher over the `.git/worktrees/` directory (which is mutated when worktrees are added or removed). If the watcher fires, it enqueues an early trigger via `daemon.Trigger("worktree-reconciler-{repo_root_hash}")`. This provides prompt alarm surfacing without waiting for the next periodic tick.

### §6 — Generalization: all substrate-spawned ephemeral resources follow this shape

`WorktreeReconciler` is the first Reconcilable for a substrate-spawned filesystem resource. The structural shape generalizes:

| Resource type | Identity key | Ledger event at creation | Bound parent | Prune condition |
|---|---|---|---|---|
| Worktrees | `worktree-{dispatch_id}` | `worktree.created` | dispatch ID | dispatch terminal + branch merged/abandoned |
| Process PIDs (outside ManagedSession) | `process-{dispatch_id}-{pid}` | `process.spawned` | dispatch ID | dispatch terminal + process exited |
| Tmp directories | `tmpdir-{dispatch_id}-{nanoid}` | `tmpdir.created` | dispatch ID | dispatch terminal |
| Lock files | `lock-{resource_id}` | `lock.acquired` | holder identity | holder process terminal |
| Dispatch task IDs | `task-{dispatch_id}` | `task.created` | parent dispatch | task terminal |

`ManagedSession` (ADR-093) is already the most mature instance: session ID is the identity, `session.attached` is the creation ledger event, and the lifecycle state machine is the Reconcilable. Future implementations for the other resource types follow the same shape.

The structural requirement across all instances: **creation goes through a substrate primitive that writes the ledger entry as a precondition**. Resources that exist outside this path are not substrate resources — they are filesystem or process state that the substrate observes but does not own. The substrate only auto-prunes what it created; it alarms on what it did not.

### §7 — Behavior under today's empty ledger

Today's seven worktrees (five mod3 + two cogos) have no `worktree.created` ledger entries. Under the `WorktreeReconciler` contract, all seven are classified `alarm-unknown-binding`. This is the correct classification: the substrate knows these worktrees exist (it can read `git worktree list`) but has no record of intent that would authorize automated cleanup.

This classification produces `worktree.alarm` events that surface to the operator. The operator must inspect each worktree, determine intent, and take one of: open a PR, emit a `worktree.terminal` event, or let the worktree decay. The reconciler does not decide.

The `alarm-unknown-binding` classification is not an error in the reconciler's design — it is the correct response to the gap in today's substrate. As new worktrees are created through `SpawnWorktree`, they will have ledger entries and will be classified correctly. The alarm count will decrease over time as the operator resolves legacy worktrees and new worktrees are created through the proper path.

## Rationale

### Why ledger-first, not cleanup-discipline

The alternative to structural binding is discipline: every dispatch is responsible for cleaning up its own worktrees before terminating. Discipline works until it doesn't — a panicking dispatch, a killed process, a context deadline exceeded, a failed network call midway through cleanup. The substrate's architecture must be correct in the presence of interruption, not dependent on every dispatch exiting cleanly.

ADR-091 §5 and ADR-092 §3 (crash recovery + idempotency) express the same principle at the ledger layer: the substrate assumes at-least-once execution, not at-most-once cleanup. The ledger is the write-ahead authority. Worktree lifecycle must follow the same pattern.

### Why alarm-not-prune for unknown-binding worktrees

A worktree that exists without a ledger entry might have been created by a process outside the substrate — for example, an operator who ran `git worktree add` directly, or Claude Code's harness-managed worktree creation (which has its own lifecycle separate from the substrate's). Auto-pruning an operator's hand-created worktree would be a bug, not a feature.

The alarm surfaces operator-required information. The operator can inspect the worktree, decide its fate, and emit the appropriate ledger event. The substrate's job is to surface the decision, not to make it unilaterally.

### Why one Reconcilable per repo root, not one per worktree

Per-worktree Reconcilable registration would create O(N) entries in the ReconcileDaemon's provider list, where N is the current worktree count. Alarm telemetry and plan computation are more cleanly handled as a set: `ComputePlan` sees all worktrees together, can detect patterns (e.g., entire dispatch wave where all worktrees are stale), and can aggregate alarm summaries. A single Reconcilable per repo root also avoids registration churn as worktrees are created and destroyed.

### Why `prune` uses `git worktree remove --force` and not a manual `rm -rf`

`git worktree remove` updates the worktree administrative state in the bare repository (`.git/worktrees/`) atomically with the filesystem removal. A manual `rm -rf` would leave the `.git/worktrees/{name}/` administrative record pointing to a path that no longer exists — a `git worktree list` reports these as "prunable" but does not automatically prune them. Using `git worktree remove` keeps the git internal state consistent, which is required for the `FetchLive` step to report an accurate worktree set on subsequent ticks.

## Consequences

### Positive

- Every substrate-created worktree is a ledger-visible Reconcilable from its first byte. The ReconcileDaemon can classify, alarm, and prune based on authoritative ledger state.
- Stale worktrees from terminated dispatches are surfaced automatically as alarm events, not discovered only during manual hygiene sweeps.
- The `alarm-unknown-binding` classification accurately identifies today's seven pre-substrate worktrees for operator review without risking data loss.
- `SpawnWorktree` becomes the substrate's canonical entry point for worktree creation; future integrations (ADR-093 `ManagedSession`, harness tooling) can route through it.
- The generalization in §6 gives implementers of future substrate-spawned resource types a structural template with citable contracts.

### Negative

- All dispatch paths that currently call `git worktree add` directly must migrate to `SpawnWorktree`. During the migration window, a mix of ledger-bound and unbound worktrees exists.
- `SpawnWorktree` adds a synchronous ledger write to the worktree creation path. On slow filesystems or a constrained ledger path, this adds latency to dispatch cold-start.
- The `WorktreeReconciler` introduces a dependency on `git` as a runtime command-line tool within the reconcile loop. CI environments that don't have `git` in `PATH` will fail `FetchLive`.
- One `WorktreeReconciler` per repo root means multi-repo workspaces register N providers. The ReconcileDaemon's serial execution (ADR-095 §5, `MaxConcurrent = 1`) means the tick latency budget must account for N × (FetchLive time per repo).

### Neutral

- Claude Code's harness-managed worktrees (created and destroyed natively by the Claude Code harness, not by substrate dispatch) are classified `alarm-unknown-binding` today and will remain so unless `SpawnWorktree` is wired into the harness's worktree lifecycle. This is expected: the harness and the substrate are independent worktree managers. Composition (the harness calling `SpawnWorktree` instead of `git worktree add` directly) is future work, not a requirement of this ADR.

## Implementation

Files to add or modify:

- `internal/engine/worktree_reconciler.go` — `WorktreeReconciler` type, `WorktreeOpts`, `WorktreeHandle`, `WorktreeState`, `WorktreePlan`, the six Reconcilable methods
- `internal/engine/worktree_spawn.go` — `SpawnWorktree` function (ledger-write precondition + `git worktree add`)
- `internal/engine/worktree_reconciler_test.go` — unit + integration tests, including the acceptance criteria below
- `internal/engine/cli.go` — register `WorktreeReconciler` at boot (per ADR-095 §6)
- `pkg/cogblock/kinds.go` — add `worktree.created`, `worktree.terminal`, `worktree.pruned`, `worktree.alarm`, `worktree.rebind` to the Kind registry (per ADR-090)
- `docs/adrs/096-worktree-reconciler.md` — this file

## Acceptance criteria

A `WorktreeReconciler` prototype MUST be able to classify each of the seven current worktrees given a test ledger:

| Worktree | Empty-ledger classification | After providing correct ledger entry |
|---|---|---|
| `mod3-modality-rfc` | `alarm-unknown-binding` | depends on dispatch terminal state and merge status |
| `mod3-modality-schemas` | `alarm-unknown-binding` | depends on dispatch terminal state and merge status |
| `mod3-pipecat` | `alarm-unknown-binding` | depends on dispatch terminal state and merge status |
| `mod3-sidecar-doc` | `alarm-unknown-binding` | depends on dispatch terminal state and merge status |
| `mod3-worker-cli` | `alarm-unknown-binding` | depends on dispatch terminal state and merge status |
| `cogos/agent-a5647032c84cb9e61` (locked) | `alarm-unknown-binding` | depends on dispatch terminal state |
| `cogos/agent-a8c054cc717aa7cb4` (detached HEAD, locked) | `alarm-unknown-binding` | `alarm-uncommitted-on-terminal-dispatch` if HEAD is a known terminal dispatch |

All seven returning `alarm-unknown-binding` with an empty ledger is the correct result — it reflects the operator-attention state observed on 2026-05-17. The acceptance test asserts this, not as a failure, but as the expected starting classification for pre-substrate worktrees.

Integration test cases to add:

1. **SpawnWorktree + clean terminal**: create worktree via `SpawnWorktree`, emit `worktree.terminal{reason=merged}`, run reconcile — assert `removable-clean`, assert prune runs, assert worktree removed from disk.
2. **SpawnWorktree + uncommitted changes on terminal dispatch**: create worktree, add uncommitted file, emit dispatch terminal event, run reconcile — assert `alarm-uncommitted-on-terminal-dispatch`, assert no filesystem mutation.
3. **Unknown-binding worktree**: create worktree via `git worktree add` directly (bypassing `SpawnWorktree`), run reconcile — assert `alarm-unknown-binding`, assert alarm ledger event emitted, assert no filesystem mutation.
4. **Idempotency**: run reconcile twice on same `removable-clean` state — assert only one `worktree.pruned` ledger event, assert no second `git worktree remove` attempted.

## Open questions

1. **Ownership of pre-creation ledger writes.** `SpawnWorktree` writes the ledger entry; but who writes the `dispatch.created` event that `SpawnWorktree` references as `DispatchID`? Today, dispatch creation is not guaranteed to write a ledger event before spawning. If the dispatch ledger event is absent, `SpawnWorktree`'s ledger entry has a dangling `dispatch_id` reference. The dependency ordering between dispatch-creation events and worktree-creation events is unresolved.

2. **Drift policy for branches that exist upstream but dispatch state says abandoned.** If an operator ran `git push upstream feature/x` and opened a PR without going through `SpawnWorktree`, the branch exists on upstream and may have a PR, but the dispatch state (if any) says abandoned or unknown. The reconciler should not auto-prune a worktree that has an open upstream PR. The `FetchLive` step should query upstream branch existence and PR state (via `git ls-remote` or the GitHub API), but the policy for "upstream branch present, dispatch terminal, no PR" vs "upstream branch present, open PR" is undefined.

3. **Composition with Claude Code's harness-managed worktrees.** Claude Code's harness creates and destroys worktrees natively, independent of `SpawnWorktree`. Today this produces `alarm-unknown-binding` classifications for every harness-created worktree. The preferred long-run design is for the harness to call `SpawnWorktree` as a substrate primitive, making its worktrees substrate-visible from creation. The integration surface (how the harness calls `SpawnWorktree` — via MCP tool, HTTP endpoint, or in-process SDK) is undefined. This ADR does not block on that integration; the alarm classification is the correct interim behavior.

4. **Per-provider timeout for `FetchLive` in git-heavy workspaces.** ADR-095 §5 notes that a slow `FetchLive` will consume the full tick budget for all subsequent providers. A workspace with many repos and many worktrees per repo could cause `WorktreeReconciler`'s `FetchLive` to dominate the tick. Per-provider timeout (flagged in ADR-095 as future work) becomes more urgent with the addition of this Reconcilable.

5. **Ledger event schema for `worktree.created`.** The Kind registry (ADR-090) requires a stable schema for each Kind. The payload fields proposed here (`worktree_id`, `dispatch_id`, `repo_root`, `worktree_path`, `branch`, `base`, `created_at`) are a first draft. Schema versioning per ADR-092 §5 must be applied before this Kind enters production use.

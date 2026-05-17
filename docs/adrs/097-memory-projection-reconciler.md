# ADR-097: MemoryProjectionReconciler — Bidirectional Projection Between Claude Code Memory and Substrate Cogdocs

| Field       | Value                                                                          |
|-------------|--------------------------------------------------------------------------------|
| Status      | Proposed                                                                       |
| Author      | @chazmaniandinkle                                                              |
| Created     | 2026-05-17                                                                     |
| Layer       | Substrate + Kernel (per [ADR-091](091-substrate-as-named-architectural-layer.md) §2) |
| Refs        | [ADR-091](091-substrate-as-named-architectural-layer.md) (substrate as named layer, ledger-first rule), [ADR-092](092-substrate-contracts-and-concurrency.md) §3 (idempotency), §4 (Reconcilable contract), [ADR-094](094-lineage-observatory.md) (ProjectionReconciler — same architectural pattern), [ADR-095](095-daemon-reconcile-loop-driver.md) (ReconcileDaemon, provider registration, watch-trigger integration), [ADR-096](096-worktree-reconciler.md) (WorktreeReconciler — sibling substrate resource lifecycle Reconcilable), `internal/engine/projection_reconciler.go`, `internal/engine/reconcile_daemon.go` |

## Context

### Two memory surfaces, zero reconciliation

CogOS operators maintain two structurally distinct memory surfaces:

**Claude Code auto-memory** at `~/.claude/projects/*/memory/`:
- Auto-loaded at session start via MEMORY.md index
- Frontmatter shape integrates with Claude Code's `/remember` and natural-language directives
- One-line index entries are scannable in tight context windows
- Operator-personal layer with implicit identity scoping
- Written by Claude Code directly, read back at session start

**Substrate cogdocs** at `cog://mem/**/*.cog.md` (canonically at `.cog/mem/`):
- Sector taxonomy (`semantic/`, `episodic/`, `procedural/`, `reflective/`) organizes by knowledge kind
- Kernel-aware reads and search via `cog_memory_search`, `cog_read_cogdoc`, `cog_memory_toc`
- Ledger participation: memory creation and update emit substrate events
- Cross-session, cross-workspace coherent — the substrate-canonical memory layer
- Section-aware reads via `cog_memory_toc` / `--section` for large documents

As of 2026-05-17, the operator's Claude Code memories at `~/.claude/projects/-Users-slowbro/memory/` contain dozens of active records — feedback memories, session summaries, project state, user profile — none of which appear as cogdocs in `.cog/mem/`. The substrate's kernel cannot search these memories using `cog_memory_search`. Conversely, the substrate's cogdocs at `cog://mem/semantic/insights/` (a corpus of hundreds of research documents) are not visible to in-session Claude Code reads, which rely on MEMORY.md for context.

The two surfaces drift apart by construction. Every Claude Code memory written after the last manual sync widens the gap. Without a reconciliation mechanism, the operator's memory infrastructure bifurcates: operator-personal knowledge in one surface, substrate-indexed knowledge in another, with no automated path between them.

### Why this is a substrate-layer failure, not an operator-discipline problem

ADR-091 §5 states the ledger-first rule: substrate state mutations are ledger-anchored. Claude Code's auto-memory writes are not substrate mutations — they are filesystem writes outside the ledger. The substrate cannot authorize, observe, or reconcile them without a binding.

The same structural diagnosis that motivated the WorktreeReconciler (ADR-096) applies here: a resource class exists outside the ledger; the substrate cannot operate on it safely; automated management is impossible without recorded intent. The fix is not "remember to sync" — that is a discipline fix and discipline fails at boundaries (context limits, session termination, process exit). The fix is to make cross-surface memory a Reconcilable from the time it exists.

### Why both surfaces survive

Forcing either surface to absorb the other destroys value:

- Absorbing Claude Code memory into cogdocs loses: auto-load at session start, tight one-line index entries, the Claude Code identity-scoping model, the `/remember` workflow.
- Absorbing cogdocs into Claude Code memory loses: the sector taxonomy, kernel-aware search, ledger participation, section-aware reads on large documents, cross-workspace coherence.

The operator's decision: both surfaces survive. A `MemoryProjectionReconciler` maintains bidirectional coherence between them.

### Skill projection as precedent

The substrate already implements this pattern for skills. `cog://skills/*/SKILL` is the substrate-canonical location for skill definitions. `~/.claude/skills/` is the Claude Code surface. The kernel's `serve_skills.go` discovers skills from both directories in priority order; upstream the substrate syncs skill mutations across surfaces. Operators can author skills in either location; the projection layer reconciles.

`MemoryProjectionReconciler` applies the same pattern to the memory domain. This is not a novel mechanism — it is the same architectural shape applied to a different content class.

## Decision

### §1 — MemoryProjectionReconciler is a Substrate primitive

`MemoryProjectionReconciler` is a **Substrate** layer primitive per ADR-091 §2. It satisfies the field-of-existence test: the relationship between a Claude Code memory and its cogdoc projection exists independently of any agent loop executing. Either the projection is current or it has drifted; that fact is true independent of whether any kernel is running.

The daemon that drives `MemoryProjectionReconciler` (scheduling ticks, isolating errors) is `ReconcileDaemon` (ADR-095), which is Kernel layer. The two-layer composition is intentional and follows the same shape as `ProjectionReconciler` (ADR-094) and `WorktreeReconciler` (ADR-096).

### §2 — Reconcilable contract

`MemoryProjectionReconciler` implements `pkg/reconcile.Reconcilable` per ADR-092 §4.

```go
// MemoryProjectionReconciler maintains bidirectional coherence between
// Claude Code auto-memory and substrate cogdocs.
type MemoryProjectionReconciler struct {
    // ClaudeProjectsRoot is the parent of all Claude Code project dirs.
    // Default: ~/.claude/projects/
    ClaudeProjectsRoot string

    // SubstrateMemRoot is the .cog/mem/ directory of the active workspace.
    // Default: {workspace}/.cog/mem/
    SubstrateMemRoot string

    // LedgerWriter emits ledger events for projection operations.
    LedgerWriter LedgerWriter
}
```

**`LoadConfig(ctx, workspaceRoot)`**: Resolves `ClaudeProjectsRoot` (default `~/.claude/projects/`) and `SubstrateMemRoot` (default `{workspaceRoot}/.cog/mem/`). Reads the projection ledger: prior per-memory projection-state records indexed by `{claude-code-path, cogdoc-path}` tuples. Returns a `MemoryProjectionConfig` containing:
- Resolved directory paths
- Loaded per-entry projection state from the ledger (hash pairs, last-modified times, conflict state, `projection.origin`)

**`FetchLive(ctx, config)`**: Reads both surfaces:
- Scans `~/.claude/projects/*/memory/*.md` for Claude Code memories. Parses frontmatter, records file mtime and content hash.
- Scans `{workspaceRoot}/.cog/mem/**/*.cog.md` for cogdocs that carry `projection.origin: claude-code` frontmatter. Parses frontmatter, records file mtime and content hash.
- Builds a pairing graph: each (Claude Code path, cogdoc path) pair with their current hash vs the ledger's last-projection hash.
- Also scans for substrate-authored cogdocs with `projection.target: claude-code` frontmatter that have not yet been projected back to Claude Code memory.

**`ComputePlan(live, config)`**: Pure function. Classifies each known memory pair into one of five states and produces the corresponding plan action:

| Classification | Condition | Plan action |
|---|---|---|
| `paired-coherent` | Both sides match last-projection hash; no drift. | skip |
| `paired-drift` | One side modified since last projection; the other has not. | update-stale-side |
| `claude-code-only-needs-projection` | Claude Code memory exists; no matching cogdoc yet (or cogdoc was pruned). | create-cogdoc-projection |
| `substrate-only-needs-projection` | Cogdoc with `projection.target: claude-code` exists; no Claude Code projection yet. | create-claude-code-projection |
| `conflict-both-modified` | Both sides modified since last projection hash; contents diverged. | alarm (do not auto-merge) |

**`ApplyPlan(ctx, plan)`**: Executes plan operations. All writes use atomic tmp+rename (per `ProjectionReconciler` precedent from ADR-094):
- `update-stale-side`: writes the updated content to the stale side; runs link-rewrite pass (§4); updates ledger projection-state entry.
- `create-cogdoc-projection`: creates the cogdoc at the sector path derived from §3; sets `projection.origin: claude-code` and `projection.source-path: {claude-code-path}` frontmatter; updates MEMORY.md substrate-side index; emits `memory.projection.created` ledger event.
- `create-claude-code-projection`: creates the Claude Code memory file at `~/.claude/projects/*/memory/{slug}.md`; sets `projection.origin: substrate` and `projection.source-path: {cogdoc-path}` frontmatter; adds one-line entry to MEMORY.md Claude-Code-side index; emits `memory.projection.created` ledger event.
- `alarm`: emits `memory.projection.conflict` ledger event with both paths, both content hashes, and both modification timestamps. Does not modify either file. Surfaces via `Health() = Degraded`.
- `skip`: no-op.

Per ADR-092 §3, `ApplyPlan` is idempotent: writing the same projection content twice produces the same result; emitting a second conflict alarm event is acceptable (informational); the reconciler MUST guard against writing a file that already matches the generated content.

**`BuildState(live, applied)`**: Produces the `MemoryProjectionState` summary. Includes per-pair classification, last-reconcile timestamp, conflict count, total paired/unpaired counts.

**`Health()`**: Returns `Healthy` if last tick had no errors and no unresolved conflicts; `Degraded` if one or more conflict alarms are active or one or more `FetchLive` / `ApplyPlan` errors occurred in the last N ticks; `Unhealthy` if `FetchLive` itself returned a fatal error (e.g., both root directories unreachable).

### §3 — Sector placement and translation rules

#### Sector placement

The cogdoc target sector is derived from the Claude Code memory's `type:` frontmatter field:

| `type:` value | Cogdoc sector | Example path |
|---|---|---|
| `feedback` | `semantic/insights/` | `.cog/mem/semantic/insights/{slug}.cog.md` |
| `project` | `semantic/projects/` | `.cog/mem/semantic/projects/{slug}.cog.md` |
| `reference` | `semantic/references/` | `.cog/mem/semantic/references/{slug}.cog.md` |
| `user` | `episodic/profile/` | `.cog/mem/episodic/profile/{slug}.cog.md` |
| _(absent or unknown)_ | `semantic/insights/` | Default sector |

Operator-override: if the Claude Code memory carries `projection.target-path: {relative-cogdoc-path}` frontmatter, that path overrides the sector-derived placement. The override is honored in both directions: a cogdoc with `projection.source-path` pointing back confirms the binding. This field also allows an operator to manually pair a pre-existing cogdoc with a Claude Code memory without recreating either.

The sector-to-type mapping is configurable (not hard-coded); the defaults above are the initial published mapping.

#### Frontmatter translation

Claude Code memories use a flat frontmatter shape:

```yaml
---
name: feedback-agent-dispatch-prefer-resume
description: "..."
metadata:
  node_type: memory
  type: feedback
  originSessionId: ...
---
```

Substrate cogdocs use a richer frontmatter shape with source tracing, section anchors, and projection fields. The reconciler translates between them preserving all original fields on each side and adding projection-tracking fields:

| Field direction | Translation |
|---|---|
| Claude Code `name:` → cogdoc `id:` | Direct mapping; slug normalization applied if needed |
| Claude Code `description:` → cogdoc `title:` | Direct mapping |
| Claude Code `metadata.type:` → cogdoc `type:` | Direct mapping; drives sector placement |
| Cogdoc `id:` → Claude Code `name:` | Direct mapping on reverse projection |
| Cogdoc `title:` → Claude Code `description:` | Direct mapping on reverse projection |
| Both sides | `projection.origin`, `projection.source-path`, `projection.last-hash`, `projection.last-synced` added by reconciler |

The reconciler preserves all frontmatter fields it does not understand (unknown fields are passed through without modification). This ensures future field additions on either surface do not corrupt projections.

#### Link translation

Cross-memory links take different forms on each surface:

| Surface | Link syntax |
|---|---|
| Claude Code memory | `[[name]]` or `[[slug]]` (wiki-link to another memory name) |
| Substrate cogdoc | `cog://mem/<sector>/<slug>` (substrate URI per ADR-067) |

On every projection write, the reconciler rewrites links in both directions:
- When writing a cogdoc from a Claude Code memory: `[[slug]]` → `cog://mem/<sector-derived-from-slug>/<slug>`
- When writing a Claude Code memory from a cogdoc: `cog://mem/<sector>/<slug>` → `[[slug]]`

Slug derivation for sector lookup follows the same `type:` → sector mapping as §3's placement table. If the slug cannot be resolved to a sector (no matching memory in either surface), the link is preserved as-is with a reconciler comment appended indicating an unresolved reference.

#### Index reconciliation

The Claude Code MEMORY.md is an operator-curated index of one-line entries pointing to memory files. The substrate maintains its own kernel-side index via `cog_memory_index`. When the reconciler projects a Claude Code memory to a cogdoc, it adds the cogdoc to the substrate index (emitting a `memory.index.update` ledger event). When the reconciler projects a cogdoc back to Claude Code memory, it adds a one-line entry to MEMORY.md.

Index ownership: when both surfaces add new memories between reconcile ticks, the reconciler merges their additions into both indexes (one-line entries in MEMORY.md; cogdoc records in the substrate index). This is an additive-only merge — no existing entries are removed without explicit classification as `alarm-conflict` or operator instruction.

Index conflict: if both surfaces modified MEMORY.md and the substrate index for the same slug since last projection, this is treated as a `conflict-both-modified` classification and follows the alarm-not-merge policy (§5).

### §4 — Watch mechanism

`MemoryProjectionReconciler` registers two filesystem watchers via fsnotify (or polling fallback per ADR-094 §4 precedent):

1. `~/.claude/projects/*/memory/` — watches for new or modified `.md` files from Claude Code auto-memory writes.
2. `{workspaceRoot}/.cog/mem/**/*.cog.md` — watches for new or modified cogdoc files from substrate-side memory operations.

On any WRITE, CREATE, or REMOVE event on either watched path, the watcher debounces for 500ms and calls `daemon.Trigger("memory-projection-reconciler")` (ADR-095 §4 watch-trigger integration). The daemon drives the full reconcile cycle. The watcher does not drive reconciliation directly — it enqueues an early trigger into the general daemon loop.

This provides prompt surfacing of cross-surface drift without waiting for the next periodic tick (default 30s per ADR-095 §5).

### §5 — Conflict policy: alarm-not-merge

When the reconciler detects `conflict-both-modified` — both surfaces modified since the last projection hash — the policy is:

1. **Leave both files as they are.** No modification to either surface.
2. **Emit `memory.projection.conflict` ledger event** with both paths, content hashes, and modification timestamps.
3. **Surface via `Health() = Degraded`** and via the substrate's observatory surfaces (HUD, dashboard) as an operator-attention item.
4. **Do not auto-merge.** The reconciler cannot know whether the Claude Code side or the cogdoc side holds the operator's intent. Auto-merging risks silently overwriting operator work on either surface.

The operator resolves conflicts by:
- Editing one side to match the other, then triggering a manual reconcile cycle (`cogos reconcile memory-projection-reconciler`), or
- Emitting a `memory.projection.conflict.resolved{winning-side: claude-code|substrate}` ledger event, which the reconciler interprets as explicit operator instruction to adopt one side and overwrite the other.

This is the same alarm-not-merge discipline as the WorktreeReconciler (ADR-096 §4): the substrate surfaces operator-required decisions it cannot safely make unilaterally. Alarms are not failure states; they are the reconciler operating correctly in the presence of ambiguous state.

### §6 — Projection origin tracking

The `projection.origin` frontmatter field distinguishes between the two directions of projection:

| Value | Meaning |
|---|---|
| `claude-code` | This cogdoc was originally projected FROM a Claude Code memory. The Claude Code memory is the authoritative source unless the operator explicitly elevates the cogdoc side. |
| `substrate` | This Claude Code memory was originally projected FROM a substrate cogdoc. The cogdoc is the authoritative source unless the operator explicitly elevates the Claude Code side. |
| _(absent)_ | No projection relationship. This memory or cogdoc was authored independently. The reconciler will project it outward (creating the paired artifact) but will not treat either side as authoritative until a `projection.origin` is established. |

The `projection.source-path` field records the absolute path to the originating artifact. Together, `projection.origin` + `projection.source-path` + `projection.last-hash` constitute the minimum ledger entry the reconciler needs to classify any pair's state.

These three fields are written by the reconciler at projection creation time and updated on every successful sync. They are NOT operator-editable as a normal workflow — operators should use `projection.target-path` (§3) to influence placement, not directly edit origin tracking fields.

### §7 — ReconcileDaemon registration

`MemoryProjectionReconciler` is registered in `ReconcileDaemon` (ADR-095) at daemon boot (after service registration, per ADR-092 §2 step 3). One instance is registered per active workspace. For a workspace managing multiple Claude Code project directories (e.g., a multi-user deployment), additional configuration may scope each instance to a specific Claude Code project root.

```go
func RegisterMemoryProjectionProvider(workspaceRoot, claudeProjectsRoot string, ledger LedgerWriter) {
    reconcile.UpsertProvider(
        "memory-projection-reconciler",
        NewMemoryProjectionReconciler(workspaceRoot, claudeProjectsRoot, ledger),
    )
}
```

### §8 — Generalization: cross-surface state-keeping as a Reconcilable pair

`MemoryProjectionReconciler` is the second Reconcilable for a cross-surface state-keeping problem (skills projection being the first, implemented earlier in the kernel's `serve_skills.go`). The structural shape is the same across all instances:

| Cross-surface concern | Claude Code surface | Substrate surface | Reconcilable |
|---|---|---|---|
| Skills | `~/.claude/skills/{name}/SKILL.md` | `cog://skills/{name}/SKILL` | `SkillProjectionReconciler` (future) |
| Memory | `~/.claude/projects/*/memory/*.md` | `cog://mem/<sector>/<slug>.cog.md` | `MemoryProjectionReconciler` (this ADR) |
| Hooks (future) | `~/.claude/settings.json` hooks | substrate hook registry (future) | `HookProjectionReconciler` (future) |
| Configs (future) | `~/.claude/settings.json` | `.cog/config/` | `ConfigProjectionReconciler` (future) |

The pattern: whenever state exists on both a Claude Code surface and a substrate surface, a projection Reconcilable is the correct architectural response. The reconciler owns the pairing graph, the translation rules, the projection-origin tracking, and the conflict alarm policy. Future implementers for hooks and configs should follow the same shape.

## Rationale

### Why bidirectional projection, not one-way import

One-way import (Claude Code → substrate or substrate → Claude Code) solves half the problem and creates a new one: the non-authoritative side becomes stale by design. Operators working in Claude Code sessions add memories there; operators working at the substrate level may add cogdocs there. Unidirectional flow requires choosing an authoritative side and accepting that the other side will always lag. Given that both surfaces have distinct affordances (§ Context), neither is the naturally dominant authority.

Bidirectional projection with conflict alarming gives the operator full freedom to use either surface; drift is corrected automatically; conflicts surface as decisions rather than silent data loss.

### Why alarm-not-merge, never auto-merge

Auto-merging cross-surface state with different formats, different link syntax, and potentially different editorial intent is a destructive operation. The risk is asymmetric: a false-positive merge silently loses operator work; a false alarm requires the operator to acknowledge and dismiss. The substrate's architecture prefers surfacing ambiguous decisions over making them unilaterally (per [[foundation-operational-mode]]). Alarms are costless in the absence of real conflicts; auto-merges are catastrophic when they go wrong.

### Why one Reconcilable for all memory pairs, not one per memory file

O(N) Reconcilable registrations for N memory files would create significant overhead in the ReconcileDaemon's provider list and would require re-registration on every file creation or deletion. A single Reconcilable that owns the pairing graph sees all memories together, can detect corpus-level patterns (e.g., all memories in a sector have drifted after a substrate migration), and can aggregate alarm summaries. This is the same reasoning as ADR-096 §Rationale (one WorktreeReconciler per repo root, not one per worktree).

### Why sector placement from `type:` frontmatter, not from path

The Claude Code memory filename encodes the slug but not the semantic category. The `type:` frontmatter field is the operator's explicit classification of the memory's kind. Using it as the sector-placement driver means the cogdoc lands in the correct sector taxonomy without requiring the operator to manually specify paths. The `projection.target-path` override provides an escape hatch for cases where the default mapping is wrong.

## Consequences

### Positive

- Claude Code memories become substrate-searchable on the next reconcile tick after they are written. Operators working in kernel-level tools can find memories they wrote in Claude Code sessions.
- Substrate cogdocs with `projection.target: claude-code` become visible to Claude Code sessions on the next tick. Substrate-authored knowledge is available in session context without manual copy.
- Both surfaces remain authoritative for the content authored there. No operator workflow changes are required.
- Conflict alarming surfaces cross-surface divergence before it compounds into irreconcilable state.
- The projection-origin tracking field makes the reconciler's reasoning auditable: each file carries its own provenance.
- Future cross-surface concerns (hooks, configs) have a citable structural template.

### Negative

- Two filesystem watchers add background I/O at daemon startup. On systems with large memory corpora (hundreds of files on both surfaces), the watcher setup time and the periodic `FetchLive` scan may be non-trivial.
- The projection-origin and projection-state frontmatter fields are reconciler-managed but visible to operators. Operators who edit these fields directly (rather than using `projection.target-path`) may corrupt the reconciler's pairing graph. The reconciler should log a structured warning when it detects inconsistent projection tracking fields.
- MEMORY.md index reconciliation adds writes to a file that Claude Code manages. If Claude Code's auto-memory writes and the reconciler's index updates race, the last-writer-wins semantics of the OS may produce a truncated MEMORY.md. The reconciler MUST use atomic tmp+rename for MEMORY.md writes to reduce (not eliminate) this risk. A future ADR addressing the MEMORY.md ownership question is warranted.
- The `MaxConcurrent = 1` constraint from ADR-095 §5 means the memory reconciler's `FetchLive` scan blocks subsequent providers for the tick duration. On a large corpus, this may delay WorktreeReconciler and ProjectionReconciler ticks by seconds. Per-provider timeout (flagged in ADR-095 as future work) becomes more urgent with this addition.

### Neutral

- Existing Claude Code memories that have never been projected classify as `claude-code-only-needs-projection` with an empty ledger. This is the legitimate starting state: the empty ledger means no prior projection state exists; all memories classify as needing a first projection. This is not an error condition; it is the expected first-run state (same reasoning as ADR-096 §7).
- Substrate cogdocs that are not memory projections and do not carry `projection.target: claude-code` are unaffected by this Reconcilable. The reconciler only manages cogdocs it created or that have opted in via projection frontmatter.
- The reconciler does not touch `cog://mem/semantic/lineage/` (managed by `ProjectionReconciler` per ADR-094) or any cogdoc managed by another Reconcilable. Domain isolation is enforced by the `projection.origin` frontmatter field.

## Implementation

Files to add or modify:

- `internal/engine/memory_projection_reconciler.go` — `MemoryProjectionReconciler` type, `MemoryProjectionConfig`, `MemoryProjectionState`, `MemoryProjectionPlan`, the seven Reconcilable methods, frontmatter translation logic, link rewrite functions, index merge logic
- `internal/engine/memory_projection_watcher.go` — dual-surface `MemoryProjectionWatcher` (wraps two fsnotify watchers with shared debounce into one trigger callback)
- `internal/engine/memory_projection_reconciler_test.go` — unit + integration tests covering the acceptance criteria below
- `internal/engine/cli.go` — register `MemoryProjectionReconciler` at boot (per ADR-095 §6)
- `pkg/cogblock/kinds.go` — add `memory.projection.created`, `memory.projection.updated`, `memory.projection.conflict`, `memory.projection.conflict.resolved`, `memory.index.update` to the Kind registry (per ADR-090)
- `docs/adrs/097-memory-projection-reconciler.md` — this file

## Acceptance criteria

A `MemoryProjectionReconciler` prototype MUST be able to classify each memory pair in the operator's corpus given an initially-empty ledger:

| Memory state | Expected empty-ledger classification |
|---|---|
| Claude Code memory exists; no matching cogdoc | `claude-code-only-needs-projection` |
| Substrate cogdoc with `projection.target: claude-code`; no Claude Code file | `substrate-only-needs-projection` |
| Both files exist; match last-projection hash | `paired-coherent` |
| Both files exist; one modified since last hash | `paired-drift` |
| Both files exist; both modified since last hash | `conflict-both-modified` |

With an empty ledger, all operator-personal Claude Code memories at `~/.claude/projects/-Users-slowbro/memory/` classify as `claude-code-only-needs-projection` and all substrate cogdocs with `projection.target: claude-code` (if any) classify as `substrate-only-needs-projection`. This is the correct and expected starting classification — it is the first-run state.

Integration test cases to add:

1. **New Claude Code memory, first projection**: write a memory file to `~/.claude/projects/test/memory/test-memory.md`, run reconcile — assert cogdoc created at sector path, assert `projection.origin: claude-code` in cogdoc, assert MEMORY.md substrate index updated, assert `memory.projection.created` ledger event emitted.
2. **Claude Code memory update, drift correction**: project a memory, then modify the Claude Code side, run reconcile — assert cogdoc updated to match, assert `projection.last-hash` updated in ledger.
3. **Conflict detection**: project a memory, then modify both sides, run reconcile — assert no file writes, assert `memory.projection.conflict` ledger event emitted, assert `Health() = Degraded`.
4. **Conflict resolution**: after conflict detection, emit `memory.projection.conflict.resolved{winning-side: claude-code}` ledger event, run reconcile — assert cogdoc overwritten with Claude Code content, assert `Health() = Healthy`.
5. **Substrate-to-Claude-Code projection**: create a cogdoc with `projection.target: claude-code`, run reconcile — assert Claude Code memory file created, assert one-line entry added to MEMORY.md Claude Code index.
6. **Link rewrite**: create a Claude Code memory with `[[other-memory]]` link, run reconcile — assert projected cogdoc contains `cog://mem/<sector>/other-memory` URI.
7. **Idempotency**: run reconcile twice on `paired-coherent` state — assert no file writes on second run, assert no duplicate ledger events for same hash.
8. **Sector placement override**: create a Claude Code memory with `projection.target-path: semantic/projects/my-project.cog.md`, run reconcile — assert cogdoc created at specified path, not at sector-derived path.

## Open questions

1. **Pre-creation event ordering.** When Claude Code writes a new memory file, the substrate discovers it on the next fsnotify event or periodic tick. There is no guarantee that the substrate has received the write before the operator's next Claude Code session reads MEMORY.md. If the operator writes a memory and immediately starts a new session, the cogdoc projection may not exist yet. Whether the reconciler should register a kernel-side hook that runs synchronously on memory-write (e.g., a `PostToolUse` hook in Claude Code's settings) or whether the periodic-tick latency (default 30s) is acceptable is unresolved. The fsnotify-driven early trigger reduces this window but does not eliminate it.

2. **MEMORY.md ownership and write races.** MEMORY.md is the Claude Code auto-memory index. It is written by Claude Code (when `/remember` runs) and by the reconciler (when adding projection entries). The current file format is append-friendly (one-line entries) but Claude Code may rewrite the entire file on memory update. If Claude Code's rewrite and the reconciler's addition race at the OS level, the last writer wins and may produce a file missing the other writer's entries. A durable solution requires either: (a) a locking protocol between the reconciler and Claude Code, which is not currently possible, or (b) accepting that MEMORY.md entries may occasionally need a reconciler re-add on the next tick. This open question affects the implementation of `ApplyPlan` for `create-claude-code-projection` actions.

3. **Origin tracking for pre-existing independently-authored pairs.** An operator may have manually maintained parallel files — a Claude Code memory and a cogdoc covering the same topic, neither carrying `projection.origin`. The reconciler has no way to determine which is canonical or whether they are intentionally independent. The current design leaves both without `projection.origin` and classifies them as two independent unpaired documents. If the operator wants to pair them, they must add `projection.target-path` to one side pointing at the other; the reconciler will then establish the pairing on the next tick. Whether to provide a `cogos memory pair {claude-code-path} {cogdoc-path}` CLI command to automate this is out of scope for this ADR.

4. **Index ownership when both surfaces grow independently.** If the operator writes new Claude Code memories and new substrate cogdocs between ticks, both MEMORY.md and the substrate index grow independently. The reconciler's index merge (§3, Index reconciliation) adds new entries from each side to the other. This additive-only merge means duplicate-topic entries may accumulate if the operator creates parallel documents on both sides without pairing them. A future ADR should define a deduplication policy for the merged index.

5. **Migration of existing operator-personal memories.** This ADR specifies the projection contract, not a migration plan. Projecting the operator's existing Claude Code memories (`~/.claude/projects/-Users-slowbro/memory/`) into substrate cogdocs is an operator-decided operation: the reconciler will classify all existing memories as `claude-code-only-needs-projection` on first run and will project them outward on the next apply cycle. Whether the operator wants this to happen automatically or wants to review and approve projections before they land in the substrate is a configuration decision outside this ADR's scope. A `dry-run` mode for the first apply cycle (producing a classification report without writing any cogdocs) is recommended as implementation guidance but not specified as a contract here.

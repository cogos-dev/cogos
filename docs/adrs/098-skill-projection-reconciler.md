# ADR-098: SkillProjectionReconciler — Provenance-Tracked Projection for Substrate Skills

| Field       | Value                                                                          |
|-------------|--------------------------------------------------------------------------------|
| Status      | Proposed                                                                       |
| Author      | @chazmaniandinkle                                                              |
| Created     | 2026-05-17                                                                     |
| Layer       | Substrate + Kernel (per [ADR-091](091-substrate-as-named-architectural-layer.md) §2) |
| Refs        | [ADR-091](091-substrate-as-named-architectural-layer.md) (substrate as named layer, ledger-first rule), [ADR-092](092-substrate-contracts-and-concurrency.md) §3 (idempotency), §4 (Reconcilable contract), [ADR-094](094-lineage-observatory.md) (ProjectionReconciler — same architectural pattern), [ADR-095](095-daemon-reconcile-loop-driver.md) (ReconcileDaemon, provider registration, watch-trigger integration), [ADR-096](096-worktree-reconciler.md) (WorktreeReconciler — sibling substrate resource lifecycle Reconcilable), [ADR-097](097-memory-projection-reconciler.md) (MemoryProjectionReconciler — sibling cross-surface projection Reconcilable), `internal/engine/serve_skills.go` |

## Context

### Three skill surfaces, no provenance tracking

Substrate skills exist across three structurally distinct locations:

**Operator-global skills** at `~/.claude/skills/{name}/SKILL.md`:
- Auto-loaded by Claude Code at session start when the harness scans the user's global skills directory
- Authored by or on behalf of the operator; personal layer
- Available to all workspaces on this machine

**Workspace-local skills** at `{workspaceRoot}/.claude/skills/{name}/SKILL.md`:
- Scoped to a specific workspace; available when the kernel is running in that workspace
- Workspace-override semantics: a workspace skill with the same name as a global skill takes precedence (per `serve_skills.go` `skillDirs()` priority ordering)
- Authored by or on behalf of the workspace team; shared within the workspace

**Substrate-canonical skills** at `cog://skills/{name}/SKILL` (canonically at `.cog/skills/` within a workspace):
- Ledger-anchored; kernel-aware; discoverable via `GET /v1/skills`
- Participate in kernel events (`skill.exec` bus events per `serve_skills.go`)
- Can be executed server-side via `POST /v1/skills/{name}/exec`

As of 2026-05-17, `serve_skills.go` `skillDirs()` discovers skills from the operator-global and workspace-local surfaces in priority order (workspace overrides global), with no record in any ledger. The substrate has no provenance information for any skill pair:

- No `projection.source-path` — no record of which surface holds the canonical version of a skill
- No `projection.origin` — no record of which surface authored the skill
- No `projection.last-hash` — no record of whether a skill has drifted since it was last in sync

The workspace-wins override is silent: if `~/.claude/skills/my-skill/SKILL.md` and `{workspace}/.claude/skills/my-skill/SKILL.md` both exist, the workspace version is served without any indication that a shadowed global version exists or that the two have diverged. The operator has no substrate-visible signal that shadowing is occurring or that the shadow copy has drifted from the global.

### Why this is the worst provenance offender in the projection audit

The three-field provenance audit (see `feedback_projection_provenance_three_field_minimality.md`, surfaced in ADR-097 §8) graded existing projection mechanisms against the three-field test:

| Mechanism | `projection.source-path` | `projection.origin` | `projection.last-hash` | Grade |
|---|---|---|---|---|
| MemoryProjectionReconciler (ADR-097) | present | present | present | complete |
| ProjectionReconciler (ADR-094) | documented asymmetric simplification | documented | documented | asymmetric, explicit |
| MCP resource projection (ADR-067) | URI-as-source-path | always-substrate | no client-freshness signal | asymmetric, documented |
| HUD/proprioception | appropriate read-only minimalism | N/A | N/A | appropriate |
| **Skill projection** (`serve_skills.go`) | **absent** | **absent** | **absent** | **zero provenance** |

Skill projection is the only surface that is **de facto bidirectional** (operators author skills on either the global or workspace surface; the kernel merges them at serve time) with **zero provenance tracking**. The workspace-wins override means the projection is asymmetric in practice, but that asymmetry is:

1. Undocumented — nothing in the codebase states that workspace wins are intentional
2. Silent — the shadowed global version is neither flagged nor accessible
3. Untracked — no mechanism detects or surfaces drift between global and workspace copies

This is the structural gap ADR-097 §8 named as `SkillProjectionReconciler (future)`.

### Three surfaces, not two

Unlike memory projection (two surfaces: Claude Code + substrate cogdocs), skill projection involves three surfaces:

1. **Operator-global** (`~/.claude/skills/`) — available everywhere; operator-personal
2. **Workspace-local** (`{workspaceRoot}/.claude/skills/`) — available in this workspace; workspace-shared
3. **Substrate-canonical** (`{workspaceRoot}/.cog/skills/`) — ledger-anchored; kernel-executed; version-tracked

The three-surface topology creates richer classification requirements than two-surface reconciliation. A skill may exist on any subset of the three; each combination has distinct semantics. The `SkillProjectionReconciler` must reason about all three surfaces and their pairwise relationships.

## Decision

### §1 — SkillProjectionReconciler is a Substrate primitive

`SkillProjectionReconciler` is a **Substrate** layer primitive per ADR-091 §2. The pairing relationships between a skill's global, workspace, and substrate-canonical forms exist independently of any agent loop. Either those surfaces are coherent or they have drifted; that fact holds independent of whether the kernel is running.

The daemon that drives `SkillProjectionReconciler` (scheduling ticks, isolating errors) is `ReconcileDaemon` (ADR-095), which is Kernel layer. The two-layer composition follows the established pattern of ADR-094, ADR-096, and ADR-097.

### §2 — Reconcilable contract

`SkillProjectionReconciler` implements `pkg/reconcile.Reconcilable` per ADR-092 §4.

```go
// SkillProjectionReconciler maintains coherence across the three skill surfaces:
// operator-global (~/.claude/skills/), workspace-local ({workspace}/.claude/skills/),
// and substrate-canonical ({workspace}/.cog/skills/).
type SkillProjectionReconciler struct {
    // GlobalSkillsRoot is the operator-global skills directory.
    // Default: ~/.claude/skills/
    GlobalSkillsRoot string

    // WorkspaceSkillsRoot is the workspace-local skills directory.
    // Default: {workspaceRoot}/.claude/skills/
    WorkspaceSkillsRoot string

    // SubstrateSkillsRoot is the substrate-canonical skills directory.
    // Default: {workspaceRoot}/.cog/skills/
    SubstrateSkillsRoot string

    // LedgerWriter emits ledger events for projection operations.
    LedgerWriter LedgerWriter
}
```

**`LoadConfig(ctx, workspaceRoot)`**: Resolves all three root directories. Reads the projection ledger: prior per-skill projection-state records indexed by skill name. Returns a `SkillProjectionConfig` containing resolved paths and loaded per-entry projection state (hash triplets, last-modified times, conflict state, `projection.origin` per surface pair).

**`FetchLive(ctx, config)`**: Reads all three surfaces:
- Scans `~/.claude/skills/` for SKILL.md files. Parses frontmatter, records file mtime and content hash.
- Scans `{workspaceRoot}/.claude/skills/` for SKILL.md files. Parses frontmatter, records file mtime and content hash.
- Scans `{workspaceRoot}/.cog/skills/` for substrate-canonical skill definitions. Parses frontmatter, records file mtime and content hash.
- For each skill name seen on any surface, builds a presence record: `{global: {hash, mtime, exists}, workspace: {hash, mtime, exists}, substrate: {hash, mtime, exists}}`.
- Compares each presence record against the ledger's last-projection hashes to detect drift.

**`ComputePlan(live, config)`**: Pure function. Classifies each known skill name into a state and produces the corresponding plan action:

| Classification | Condition | Plan action |
|---|---|---|
| `coherent` | All present surfaces match last-projection hashes; no drift. | skip |
| `global-only` | Skill exists only at global; no workspace or substrate copy. | create-workspace-and-substrate-projections (if configured) or leave |
| `workspace-only` | Skill exists only at workspace; no global or substrate copy. | create-substrate-projection; leave global absent (workspace-scoped intent) |
| `substrate-only` | Skill exists only at substrate; no global or workspace copy. | alarm — substrate-authored skill with no surface projection; may be intentional |
| `workspace-shadows-global-coherent` | Both global and workspace exist; workspace matches last-projection hash; both match their respective last hashes. | skip (shadow is tracked and coherent) |
| `workspace-shadows-global-drifted` | Both exist; one or both have drifted from their last-projection hash; content differs. | alarm — shadow drift; surface for operator decision |
| `surface-drift` | One surface modified since last projection; others unchanged. | update-stale-surfaces |
| `conflict-multiple-modified` | Two or more surfaces modified since last projection hashes; contents diverged. | alarm (do not auto-merge) |

**`ApplyPlan(ctx, plan)`**: Executes plan operations. All writes use atomic tmp+rename (per `ProjectionReconciler` precedent, ADR-094):
- `create-workspace-and-substrate-projections`: creates projection files with `projection.origin`, `projection.source-path`, `projection.last-hash` frontmatter; emits `skill.projection.created` ledger event.
- `create-substrate-projection`: creates substrate-canonical skill file; sets projection tracking frontmatter; emits `skill.projection.created` ledger event.
- `update-stale-surfaces`: writes updated content to stale surfaces; updates ledger projection-state entry; emits `skill.projection.updated` ledger event.
- `alarm`: emits structured `skill.projection.alarm` ledger event with classification, skill name, affected surface paths, content hashes, and modification timestamps. Does not modify any file.
- `skip`: no-op.

Per ADR-092 §3, `ApplyPlan` is idempotent: writing the same projection content twice produces the same result; emitting a second alarm event for the same skill in the same state is acceptable (informational).

**`BuildState(live, applied)`**: Produces the `SkillProjectionState` summary. Includes per-skill classification, shadow count, conflict count, last-reconcile timestamp.

**`Health()`**: Returns `Healthy` if last tick had no errors and no unresolved alarms; `Degraded` if one or more alarm events are active or tick errors occurred; `Unhealthy` if `FetchLive` returned a fatal error.

### §3 — Three-field provenance contract on projected artifacts

Every skill projection written by this reconciler carries the minimum three-field provenance contract (per `feedback_projection_provenance_three_field_minimality.md`):

```yaml
---
# (skill-specific frontmatter preserved above)
projection.origin: global | workspace | substrate
projection.source-path: /abs/path/to/canonical/SKILL.md
projection.last-hash: sha256:<hex>
projection.last-synced: 2026-05-17T...
---
```

| Field | Question answered | Value |
|---|---|---|
| `projection.origin` | WHO authored this canonically? | `global`, `workspace`, or `substrate` |
| `projection.source-path` | WHERE is the canonical artifact? | Absolute filesystem path or `cog://` URI |
| `projection.last-hash` | HAS IT CHANGED since last sync? | SHA-256 of canonical artifact content at last sync |

These fields are written at projection creation time and updated on every successful sync. They are reconciler-managed and not operator-editable in the normal workflow — operators who need to influence projection placement should use `projection.target-surface` (see §4).

### §4 — Shadow-tracking and workspace-wins disambiguation

The current `serve_skills.go` workspace-wins behavior is preserved and made explicit. The reconciler's job is not to change the serving priority but to track the shadowing relationship and surface drift.

**Asymmetric simplification (explicit):** In the common case where a skill exists at workspace and shadows a global copy of the same name, workspace is the effective serving surface. This asymmetry is **intentional and documented here** — workspace-scoped skills are expected to override global defaults within their workspace. The asymmetry does not eliminate the need for provenance tracking; it determines which field values are trivially set vs. actively monitored:

- `projection.origin` for workspace-shadowing-global: `workspace` (the workspace copy is the serving authority within this workspace)
- `projection.source-path` for the shadow: points to the workspace path
- `projection.last-hash` for the shadow: tracks the workspace copy; a separate ledger field tracks the global hash to detect drift

The reconciler adds a `projection.shadows` field to workspace projections that shadow a global skill:

```yaml
projection.shadows: /abs/path/to/global/SKILL.md
projection.shadows-hash: sha256:<hex-of-global-at-last-check>
```

When `projection.shadows-hash` diverges from the current global content hash, the reconciler classifies as `workspace-shadows-global-drifted` and emits an alarm rather than silently allowing the divergence to accumulate.

### §5 — Conflict policy: alarm-not-merge

When the reconciler detects `conflict-multiple-modified` — two or more surfaces modified since the last projection hashes — the policy is:

1. **Leave all files as they are.** No modification to any surface.
2. **Emit `skill.projection.conflict` ledger event** with all affected paths, content hashes, and modification timestamps.
3. **Surface via `Health() = Degraded`** and via observatory surfaces (HUD, dashboard) as an operator-attention item.
4. **Do not auto-merge.** Skills are executable programs; auto-merging diverged versions risks producing a broken skill or silently adopting the wrong version.

The operator resolves conflicts by editing surfaces to converge, then triggering a manual reconcile cycle (`cogos reconcile skill-projection-reconciler`), or by emitting `skill.projection.conflict.resolved{winning-surface: global|workspace|substrate}`.

This is the same alarm-not-merge discipline as ADR-096 §4 and ADR-097 §5.

### §6 — ReconcileDaemon registration

`SkillProjectionReconciler` is registered in `ReconcileDaemon` at daemon boot (after service registration, per ADR-092 §2 step 3). One instance is registered per active workspace. Multiple workspace instances are supported for multi-workspace deployments.

```go
func RegisterSkillProjectionProvider(workspaceRoot, globalSkillsRoot string, ledger LedgerWriter) {
    reconcile.UpsertProvider(
        "skill-projection-reconciler",
        NewSkillProjectionReconciler(workspaceRoot, globalSkillsRoot, ledger),
    )
}
```

`SkillProjectionReconciler` registers filesystem watchers on all three skill directories via fsnotify (polling fallback per ADR-094 §4 precedent). Any WRITE, CREATE, or REMOVE event debounces for 500ms and enqueues an early trigger via `daemon.Trigger("skill-projection-reconciler")` (ADR-095 §4).

### §7 — Relationship to `serve_skills.go`

This ADR does not change `serve_skills.go`'s serving behavior. `skillDirs()` continues to return `[globalSkillsRoot, workspaceSkillsRoot]` in that priority order; workspace-local overrides global at serve time. `SkillProjectionReconciler` operates on the same directories plus the substrate-canonical surface; it adds provenance tracking and shadow-drift alarming as a background reconcile loop.

In a future iteration, `serve_skills.go` may be extended to prefer substrate-canonical versions (which carry verified provenance) over bare filesystem copies. That change is out of scope for this ADR; the substrate-canonical surface being established here is the prerequisite.

### §8 — Generalization: the cross-surface projection pattern

`SkillProjectionReconciler` is the third Reconcilable for a cross-surface state-keeping problem, establishing the pattern as canonical:

| Cross-surface concern | Claude Code surface | Substrate surface | Reconcilable |
|---|---|---|---|
| Skills | `~/.claude/skills/{name}/SKILL.md` + `{workspace}/.claude/skills/{name}/SKILL.md` | `cog://skills/{name}/SKILL` | `SkillProjectionReconciler` (this ADR) |
| Memory | `~/.claude/projects/*/memory/*.md` | `cog://mem/<sector>/<slug>.cog.md` | `MemoryProjectionReconciler` (ADR-097) |
| Hooks (future) | `~/.claude/settings.json` hooks | substrate hook registry | `HookProjectionReconciler` (future) |
| Configs (future) | `~/.claude/settings.json` | `.cog/config/` | `ConfigProjectionReconciler` (future) |

Skill projection differs from memory projection in two structural ways: three surfaces instead of two, and an explicit priority ordering among surfaces (workspace overrides global at serve time). Future projection implementers must assess whether their domain has an analogous serving priority — if so, the shadow-tracking mechanism in §4 applies.

## Rationale

### Why track shadow relationships explicitly

Silent workspace-wins is a debt accumulation mechanism. The workspace copy was likely derived from or inspired by the global copy at authoring time; over time both may evolve independently. Without shadow tracking, an operator upgrading a global skill may not realize that a workspace override exists and is serving stale logic. Shadow drift is not an error — it may be intentional — but it should be visible.

### Why three surfaces, not collapse to two

Collapsing global and workspace into a single "Claude Code surface" (as memory projection does for `~/.claude/projects/*/memory/`) would lose the workspace-scoping semantics that are load-bearing for multi-workspace deployments. A skill that should override globally must live at global; a skill that should apply only within one workspace's context lives at workspace. The three-surface model preserves that expressiveness.

### Why alarm on substrate-only skills

A substrate-canonical skill with no filesystem projection may have been authored via `POST /v1/skills/{name}/exec` scaffolding or via direct substrate-level tooling. Whether it should be projected back to a filesystem surface is an operator decision — not all substrate-authored skills need Claude Code surface visibility. The alarm surfaces the question rather than silently projecting or silently ignoring.

### Why preserve the workspace-wins serving order

Changing the serving order would break existing operator configurations that rely on workspace-local overrides. This ADR adds provenance and drift detection without changing behavior; it is additive, not breaking.

## Consequences

### Positive

- Shadow relationships between global and workspace skill copies become substrate-visible. Operators can detect when a workspace override has drifted from its global counterpart.
- Substrate-canonical skills gain provenance tracking, making them auditable artifacts (who authored, from which surface, what hash).
- The three-field provenance contract closes the audit gap identified in ADR-097 §8: skill projection was the only bidirectional surface with zero provenance.
- Conflict alarming surfaces multi-surface divergence before it compounds.
- The asymmetric simplification (workspace-wins) is explicitly documented for the first time, satisfying the policy proposed in myrgic/cogos issue #276.

### Negative

- Three filesystem watchers per workspace instance (one per skill directory) increase background I/O at daemon startup.
- The three-surface pairing graph is more complex than two-surface memory projection. `FetchLive` must reason about triplets, not pairs, and `ComputePlan` has more classification cases.
- Operators who manually maintain parallel workspace and global copies of the same skill for intentional divergence will receive `workspace-shadows-global-drifted` alarms. A configuration option to mark specific shadow pairs as `drift-intentional` is recommended as implementation guidance, though not a contract specified here.

### Neutral

- Existing skills that have never been projection-tracked classify as `global-only`, `workspace-only`, or `substrate-only` on first run (whichever surface they currently exist on). This is the expected first-run state.
- The reconciler does not modify `serve_skills.go` serving behavior. Existing skill discovery and execution are unaffected.

## Implementation

Files to add or modify:

- `internal/engine/skill_projection_reconciler.go` — `SkillProjectionReconciler` type, `SkillProjectionConfig`, `SkillProjectionState`, `SkillProjectionPlan`, the seven Reconcilable methods, three-surface scan logic, shadow-tracking logic
- `internal/engine/skill_projection_watcher.go` — triple-surface `SkillProjectionWatcher` (three fsnotify watchers with shared debounce into one trigger callback)
- `internal/engine/skill_projection_reconciler_test.go` — unit + integration tests covering the acceptance criteria below
- `internal/engine/cli.go` — register `SkillProjectionReconciler` at boot (per ADR-095 §6)
- `pkg/cogblock/kinds.go` — add `skill.projection.created`, `skill.projection.updated`, `skill.projection.conflict`, `skill.projection.conflict.resolved`, `skill.projection.alarm` to the Kind registry (per ADR-090)
- `docs/adrs/098-skill-projection-reconciler.md` — this file

## Acceptance criteria

A `SkillProjectionReconciler` prototype MUST correctly classify each surface configuration:

| Surface configuration | Expected classification |
|---|---|
| Skill at global only | `global-only` |
| Skill at workspace only | `workspace-only` |
| Skill at substrate only | `substrate-only` |
| Global + workspace, no drift since last sync | `workspace-shadows-global-coherent` |
| Global + workspace, workspace modified since last sync | `workspace-shadows-global-drifted` |
| Global + workspace, global modified since last sync | `workspace-shadows-global-drifted` |
| All three surfaces, all coherent | `coherent` |
| All three surfaces, one modified | `surface-drift` |
| All three surfaces, two or more modified with diverged content | `conflict-multiple-modified` |

Integration test cases:

1. **First-run classification**: scan actual operator skills at `~/.claude/skills/` and `{workspaceRoot}/.claude/skills/` against an empty ledger — assert all existing skills classify correctly based on their surface presence, assert no file writes, assert no alarm events for legitimate single-surface skills.
2. **Shadow tracking created**: create matching global and workspace skills, run reconcile — assert `projection.shadows` and `projection.shadows-hash` written to workspace projection frontmatter.
3. **Shadow drift detected**: after shadow tracking is established, modify the global copy, run reconcile — assert `workspace-shadows-global-drifted` classification, assert `skill.projection.alarm` event emitted, assert no file writes.
4. **Conflict detection**: project a skill to all three surfaces, modify two surfaces, run reconcile — assert no file writes, assert `skill.projection.conflict` ledger event emitted, assert `Health() = Degraded`.
5. **Conflict resolution**: after conflict detection, emit `skill.projection.conflict.resolved{winning-surface: workspace}`, run reconcile — assert other surfaces updated to match workspace content, assert `Health() = Healthy`.
6. **Idempotency**: run reconcile twice on `coherent` state — assert no file writes on second run, assert no duplicate ledger events for same hashes.
7. **Substrate-only alarm**: create a skill only at `.cog/skills/`, run reconcile — assert `substrate-only` classification, assert `skill.projection.alarm` emitted, assert no file writes on either filesystem surface.

## Open questions

1. **Projection direction for `global-only` skills.** When a global skill has no workspace or substrate copy, should the reconciler automatically create substrate and workspace projections, or leave the global-only classification as stable? Auto-projection would make all global skills substrate-searchable; it would also create workspace copies that operators may not want for workspace-scoped deployments. A configuration flag (`auto-project-global: true|false`) is likely warranted; the default should be `false` (leave global-only stable) to avoid unintended workspace proliferation.

2. **Deletion propagation.** If an operator deletes a skill from one surface, should the reconciler delete it from all surfaces? Deletion propagation is destructive; the safe default is to classify `surface-drift` when a deletion is detected (one surface has hash X; other surfaces have no content) and alarm rather than auto-delete. A `delete-propagation: alarm|propagate` configuration option is recommended.

3. **`serve_skills.go` integration for substrate-canonical priority.** The current serving order (workspace > global) ignores the substrate-canonical surface entirely. Once `SkillProjectionReconciler` establishes provenance on substrate-canonical skills, `serve_skills.go` could be updated to serve substrate-canonical versions when available, falling back to filesystem copies. The integration interface (should the reconciler expose a pre-resolved skill set, or should `serve_skills.go` read projection frontmatter directly?) is undefined.

4. **Multi-workspace deployments.** When a single daemon manages multiple workspace roots (an advanced configuration), `SkillProjectionReconciler` runs one instance per workspace. Skills with the same name but different content across workspaces will each have their own workspace-local projection state. Cross-workspace skill coherence is explicitly out of scope for this ADR; each workspace's reconciler operates independently.

5. **Skill version tracking.** SKILL.md frontmatter may carry a `version:` field (per agentskills.io specification). Should `projection.last-hash` track content hash, version field, or both? Content hash is more reliable (version fields may not be updated on every edit); version field provides a human-meaningful signal. The current design uses content hash only; the version field could be preserved as an additional metadata annotation on the projection record.

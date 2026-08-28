# ADR: Three-Surface Memory Projection — Hermes Tier-2 as a Self-Projecting Third Surface (Amendment to ADR memory-projection-reconciler)

| Field   | Value |
|---------|-------|
| Status  | Proposed |
| Author  | @chazmaniandinkle |
| Created | 2026-08-28 |
| Layer   | Substrate + Kernel (per [ADR-091](091-substrate-as-named-architectural-layer.md) §2) — unchanged from the ADR this amends |
| Amends  | ADR memory-projection-reconciler (`097-memory-projection-reconciler.md`) |
| Refs    | ADR memory-projection-reconciler (the two-surface Reconcilable this extends), ADR skill-projection-reconciler (`098-skill-projection-reconciler.md` — sibling slice of the same harness-neutral bundle), ADR worktree-reconciler (`096-worktree-reconciler.md` — alarm-not-merge precedent), ADR daemon-reconcile-loop-driver (`095-daemon-reconcile-loop-driver.md`), `pkg/substrate/reconcile/registry.go` (instance-name-as-path-component validation), `~/workspaces/cog/.cog/mem/hermes-memory-index.cog.md` (the observed Hermes Tier-2 pairing hook) |

## Context

### What ADR memory-projection-reconciler shipped

ADR memory-projection-reconciler (Accepted, 2026-05-17) specified `MemoryProjectionReconciler` as a bidirectional Reconcilable between exactly two memory surfaces: Claude Code auto-memory (`~/.claude/projects/*/memory/*.md`) and substrate cogdocs (`.cog/mem/**/*.cog.md`). That design is sound and unchanged by this amendment — five-way classification, alarm-not-merge on conflict, `type:` → sector placement, `projection.origin` provenance tracking. Nothing in §1–§8 of that ADR is revised here.

### A third surface has appeared since May

Hermes — the second harness this substrate runs agents in, alongside Claude Code — now ships a working two-tier memory tool. Tier 1 is a bounded working set (Hermes's own `MEMORY.md`-equivalent). Tier 2 is the overflow: when Tier 1 fills, Hermes **pages evicted entries into the substrate as cogdocs directly**, landing them in `.cog/mem/episodic/hermes-overflow/` and recording a pointer table at `~/workspaces/cog/.cog/mem/hermes-memory-index.cog.md` — one row per evicted entry (`summary | uri | date | tags`), with a `hot` tag as the signal Hermes uses to promote an entry back into its own Tier 1.

This is structurally different from the Claude Code side of ADR memory-projection-reconciler in one load-bearing way: **Hermes already self-projects.** The harness→substrate write is not something `MemoryProjectionReconciler` needs to perform on Hermes's behalf — Hermes's own memory tool does it, unprompted, the moment Tier 1 evicts. What Hermes does *not* do is the reverse direction: nothing currently makes a Hermes-evicted cogdoc discoverable to a Claude Code session, or `cog_memory_search`-indexed the way a reconciler-projected cogdoc is, or eligible for promotion into Claude Code's own MEMORY.md when the same knowledge would be useful there.

`MemoryProjectionReconciler`'s `FetchLive` (ADR memory-projection-reconciler §2) today scans exactly two globs. It does not look at `hermes-memory-index.cog.md`, and it has no `projection.origin` value for "a harness wrote this cogdoc itself." A Hermes-evicted cogdoc is invisible to the reconciler's pairing graph — not in conflict, not drifted, simply unseen.

### The membrane framing this composes with

Separately, the operator's harness-convergence research (`project_dsh_acp_self_managed_surface.md`, 2026-08-28 afternoon) identified that all three harnesses this substrate touches — Claude Code, Hermes, and dsh — independently converged on the same extension triad: skills, hooks, tools. The harness-neutral generalization is a bundle — `{skills, hooks, tools, identity, memory-contract}` — projected into each harness's own dialect (Claude Code's `settings.json` hooks, Hermes's plugin YAML, dsh's Cordis bundles→profiles→presets).

That research names the two halves of the resulting membrane explicitly:

- **Hooks are the substrate→harness direction.** Per-turn foveated context assembly (ADR-066/071/103) already delivers substrate state into a harness's live context window every turn. This is real, shipped, and fast (per-turn cadence).
- **Reconcilers are the harness→substrate direction.** A harness's own local state (a Claude Code memory file, a Hermes-evicted cogdoc, eventually a dsh Cordis session artifact) is carried back into substrate-canonical form on a slower cadence (periodic tick + fsnotify, per ADR daemon-reconcile-loop-driver §5).

`MemoryProjectionReconciler` is the **memory slice** of this general pattern — already named as such in ADR memory-projection-reconciler §8's generalization table (skills / memory / hooks / configs, each a `{Claude Code surface, substrate surface, Reconcilable}` triple). That table under-stated the shape in one respect: it modeled every surface as needing the reconciler to write both directions. Hermes shows a variant — a harness that writes its own harness→substrate leg — and the table's generalization needs a place for that variant, not a new table.

## Decision

### §9 — Hermes Tier-2 is a third `FetchLive` source, not a new Reconcilable

`MemoryProjectionReconciler`'s `FetchLive` (ADR memory-projection-reconciler §2) gains a third scan, alongside the two already specified:

3. Reads `{workspaceRoot}/.cog/mem/hermes-memory-index.cog.md` (or the configured equivalent path — see §11) and, for each indexed row, the cogdoc it points at. Records the row's `uri`, `tags`, and `date`, and the target cogdoc's mtime and content hash.

This is additive. No new Reconcilable is registered, and no new registry key is minted — `RegisterMemoryProjectionProvider` (ADR memory-projection-reconciler §7) keeps the single instance name `memory-projection-reconciler`. `pkg/substrate/reconcile/registry.go`'s `ValidateInstanceName` treats a registry key as a filesystem path component (`type[/instance]`, at most one `/`); this amendment introduces no new key at all, so the WorktreeReconciler `type:instance`-colon class of defect (registry.go's own worked example) has no surface to land on here. If a future multi-Hermes-instance deployment needs per-instance scoping, the existing `memory-projection-reconciler/<workspace-slug>` shape (already anticipated in ADR memory-projection-reconciler §7 for multi-Claude-Code-project deployments) is the pattern to reuse, not a colon-joined ad hoc key.

### §10 — `projection.origin: hermes` and the asymmetric pairing state

A third `projection.origin` value is added to the table in ADR memory-projection-reconciler §6:

| Value | Meaning |
|---|---|
| `hermes` | This cogdoc was deposited directly by Hermes's own Tier-2 eviction, not by `MemoryProjectionReconciler`. Hermes is the authoritative source. The reconciler never writes to a `projection.origin: hermes` cogdoc's content — only to its onward-projection state (§11). |

A `hermes`-origin cogdoc is **not** classified `claude-code-only-needs-projection` (nothing on the Claude Code side to reconcile *from*) and it is **not** `conflict-both-modified` (the reconciler never edited it, so there is nothing for it to have drifted against). It gets a new classification:

| Classification | Condition | Plan action |
|---|---|---|
| `hermes-native-parked` | `projection.origin: hermes`; not tagged `hot` in `hermes-memory-index.cog.md`. | skip — substrate-indexed via normal `cog_memory_search`, not promoted further |
| `hermes-native-promotable` | `projection.origin: hermes`; tagged `hot` in `hermes-memory-index.cog.md`. | create-claude-code-projection (existing ADR memory-projection-reconciler §2 action, reused) |

This reuses the existing `create-claude-code-projection` action unmodified (ADR memory-projection-reconciler §2) — the only new thing is the trigger condition. The `hot` tag is Hermes's own promotion-to-Tier-1 signal; the reconciler treats it as an equally valid promotion-to-Claude-Code signal, since both mean the same thing from the operator's perspective ("this evicted memory turned out to matter enough to want back in a bounded working set"). An untagged Hermes-evicted entry stays `parked`: substrate-searchable, never pushed into Claude Code's tight MEMORY.md. This is a deliberate volume gate — Hermes's overflow corpus accumulates continuously and is not curated at write time the way a Claude Code `/remember` invocation is; indiscriminately mirroring all of it into MEMORY.md would defeat MEMORY.md's role as a scannable index (ADR memory-projection-reconciler § Context, "Claude Code auto-memory" bullet list, first line).

### §11 — Configuration surface

The Hermes index path is a resolved config value, following the same discipline ADR memory-projection-reconciler §"Implementation guidance" already requires for sector mapping: not hardcoded. Default: `{workspaceRoot}/.cog/mem/hermes-memory-index.cog.md`. Exposed alongside the existing `ClaudeProjectsRoot` / `SubstrateMemRoot` fields as `HermesMemoryIndexPath` on `MemoryProjectionConfig` (ADR memory-projection-reconciler §2). An empty or missing path disables the Hermes scan entirely rather than erroring — a workspace with no Hermes seat attached has nothing wrong with it.

The watch mechanism (ADR memory-projection-reconciler §4) gains a third fsnotify watch on the index file's directory, debounced identically to the other two, calling the same `daemon.Trigger("memory-projection-reconciler")`.

### §12 — The membrane, restated with a third instance in hand

ADR memory-projection-reconciler §8 named the general pattern — cross-surface state gets a projection Reconcilable — and sketched skills / memory / hooks / configs as parallel instances. With Hermes as a live example, the more precise statement is a **direction split**, not a flat list:

- **Substrate → harness**: hooks. Per-turn, fast, already shipped for every harness this substrate drives (ADR-066/071/103 for Claude Code and the OpenAI-compatible proxy path Hermes talks through).
- **Harness → substrate**: reconcilers. Periodic-tick + fsnotify cadence. `MemoryProjectionReconciler` is the memory slice; `SkillProjectionReconciler` (ADR skill-projection-reconciler) is the skills slice. A harness may perform its own half of this direction itself (Hermes, for memory) or may need the reconciler to perform it on the harness's behalf (Claude Code, for memory — it has no eviction-to-substrate mechanism of its own).

Both halves are instances of the same harness-neutral bundle — `{skills, hooks, tools, identity, memory-contract}` — projected per-harness (Claude Code `settings.json`, Hermes plugin YAML, dsh Cordis bundles). This ADR is the memory-contract slice acquiring its second harness dialect. The hooks slice, the tools slice, and the identity slice remain future work, unchanged from ADR memory-projection-reconciler §8's "(future)" markers — this amendment does not build them, only names where the memory slice sits in the fuller picture so the next slice's author has a template that already accounts for a self-projecting harness, not only a passive one.

## Rationale

### Why fold Hermes into the existing Reconcilable rather than register a second one

The existing `MemoryProjectionReconciler` already owns the pairing graph, the conflict-alarm machinery, and the sector taxonomy. A second Reconcilable for the Hermes surface would duplicate all three and would need its own coordination with the first to avoid double-projecting a cogdoc that both instances scan. One Reconcilable that sees all three surfaces together can tell `hermes-native-parked` apart from `claude-code-only-needs-projection` by looking at one field (`projection.origin`) rather than needing cross-instance communication. This is the same reasoning ADR memory-projection-reconciler §Rationale already gives for one instance per workspace rather than one per memory file.

### Why `hot`-tag gating rather than promoting every Hermes-evicted entry

The alarm-not-merge discipline (ADR memory-projection-reconciler §5, inherited from ADR worktree-reconciler §4) is about not making destructive unilateral decisions. Promoting every Hermes eviction into Claude Code's MEMORY.md is not destructive, but it is a quality regression by a different mechanism: MEMORY.md's entire value (ADR memory-projection-reconciler § Context) is being a tight, scannable index, and Hermes's Tier-2 overflow is, by construction, the material Hermes's own Tier-1 curation already decided was not worth keeping close. Reusing Hermes's own `hot` signal costs nothing new to implement and respects a promotion decision a harness already made about its own content, rather than the reconciler inventing a second opinion about material it did not author.

### Why this is an amendment, not a revision of ADR memory-projection-reconciler in place

ADR memory-projection-reconciler is Accepted and implemented against; §1–§8 describe a shipped two-surface contract that remains correct for the surfaces it covers. Rewriting it to silently become a three-surface ADR would orphan any implementation or review history anchored to the two-surface acceptance criteria. The amendment pattern (ADR-103 amending ADR-066/071 is the precedent in this corpus) keeps the original decision's acceptance intact and scopes the new decision to exactly the delta: one new `FetchLive` source, one new `projection.origin` value, two new classifications reusing an existing action.

## Consequences

### Positive

- Hermes-evicted knowledge becomes substrate-searchable the same reconcile tick it would be for a Claude-Code-originated memory, without Hermes needing to know anything about `MemoryProjectionReconciler` or vice versa beyond the shared index file's location.
- A `hot`-tagged Hermes memory reaches Claude Code's MEMORY.md automatically — the first case where insight surfaced in one harness becomes visible in another without an operator manually re-typing it.
- The direction-split restatement in §12 gives the next harness (dsh) and the next slice (hooks, tools, identity) a template that already accounts for a harness performing its own harness→substrate leg, which dsh's Cordis bundle architecture is likely to do as well (per the harness-convergence research this amendment cites).

### Negative

- A fourth watched path (the Hermes index file) adds background I/O to daemon boot, compounding the existing negative already flagged in ADR memory-projection-reconciler § Consequences about `MaxConcurrent = 1` tick-blocking on large corpora.
- `hermes-memory-index.cog.md` is a markdown table, not ledger-backed. The reconciler's read of it is a best-effort table parse, not a schema-validated read. A malformed row (a `|` inside a summary that was not escaped, for instance) degrades to a parse warning and that row being skipped for the tick, per the same non-fatal-degradation posture as ADR memory-projection-reconciler's `Health() = Degraded` semantics — it does not halt the reconcile cycle.
- The Hermes index file's `hot` tag is Hermes-internal state the reconciler now depends on being maintained correctly. If Hermes's own promotion logic changes the tag's meaning in a future version without a corresponding update here, the reconciler would silently stop promoting (or start over-promoting) Hermes memories. This is the same class of cross-project drift risk any pointer-based integration carries; there is no substrate mechanism today that would alert on a silent meaning-drift of a tag in another project's file format.

### Neutral

- Workspaces with no Hermes seat see no behavior change: `HermesMemoryIndexPath` resolves to a path that does not exist, the scan is skipped, and the reconciler operates exactly as ADR memory-projection-reconciler originally specified.
- This amendment does not touch the alarm-not-merge conflict policy (ADR memory-projection-reconciler §5) at all — Hermes-origin cogdocs are never write-targets for the reconciler's Claude-Code-side sync, so they cannot enter `conflict-both-modified`.

## Implementation

Files to add or modify (in addition to ADR memory-projection-reconciler's list, which stands):

- `internal/engine/memory_projection_reconciler.go` — third `FetchLive` scan (`hermes-memory-index.cog.md` table parse); `projection.origin: hermes` handling in `ComputePlan`; `hermes-native-parked` / `hermes-native-promotable` classifications
- `internal/engine/memory_projection_watcher.go` — third fsnotify watch, same debounce path
- `internal/engine/memory_projection_reconciler_test.go` — cases from the Acceptance criteria below
- `docs/adrs/three-surface-memory-projection.md` — this file

No changes to `pkg/substrate/reconcile/registry.go` are required; this amendment mints no new registry key (§9).

## Acceptance criteria

Extends ADR memory-projection-reconciler's acceptance table with:

| Memory state | Expected classification |
|---|---|
| Cogdoc exists at a path indexed by `hermes-memory-index.cog.md`; row untagged | `hermes-native-parked` |
| Cogdoc exists at a path indexed by `hermes-memory-index.cog.md`; row tagged `hot` | `hermes-native-promotable` |
| `hermes-memory-index.cog.md` absent or unconfigured | Hermes scan skipped; two-surface behavior unchanged |

Integration test cases to add:

1. **Hermes-native parked, no action**: seed a cogdoc with `projection.origin: hermes` at an indexed path, untagged in the index — run reconcile — assert no writes to either the cogdoc or Claude Code memory.
2. **Hermes-native promoted**: same seed, but the index row carries `hot` — run reconcile — assert a Claude Code memory file is created via the existing `create-claude-code-projection` action, assert MEMORY.md gains a one-line entry, assert `memory.projection.created` ledger event emitted.
3. **Hermes index malformed row**: seed an index with one row missing its `uri` column — run reconcile — assert the malformed row is skipped, all other rows still classify correctly, assert `Health()` reflects a degraded-but-not-unhealthy state for the tick.
4. **No Hermes seat**: unset `HermesMemoryIndexPath` (or point it at a nonexistent path) — run reconcile — assert behavior is identical to ADR memory-projection-reconciler's existing test suite (no regression).

## Open questions

1. **Should Hermes's own promotion (Tier-2 → Tier-1) and the reconciler's promotion (Tier-2 → Claude Code MEMORY.md) share one `hot` tag, or does sharing risk conflating "useful to Hermes" with "useful to Claude Code"?** The two harnesses may want different subsets promoted. This amendment takes the simpler shared-tag position because it costs nothing to implement and no divergent case has been observed yet; splitting the tag namespace (`hot`, `hot-claude-code`) is a straightforward follow-up if the operator finds the shared signal too coarse in practice.

2. **Does a fourth harness (dsh) get its own `FetchLive` source added to this same Reconcilable, or does the pattern in §12 argue for a harness-count-agnostic redesign (e.g., a pluggable list of `HermesLikeSource` scanners) once there are three or more self-projecting harnesses?** Two data points (Claude Code needing the reconciler to write both directions, Hermes writing one direction itself) are not yet a stable generalization. This amendment hardcodes the Hermes case; a `HermesLikeSource` interface is recommended implementation guidance once dsh's actual self-projection behavior (if any) is observed rather than speculated about.

3. **Should `hermes-memory-index.cog.md` become ledger-backed** (each row a substrate event rather than a markdown table row), removing the best-effort-parse fragility flagged in Consequences? This is Hermes-project scope, not this ADR's — noted here because it would remove one of this amendment's stated negatives if it happened.

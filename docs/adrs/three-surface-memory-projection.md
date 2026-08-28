# ADR: Three-Surface Memory Projection — Hermes Tier-2 as a Self-Projecting Third Surface (Amendment to ADR memory-projection-reconciler)

| Field   | Value |
|---------|-------|
| Status  | Proposed |
| Author  | @chazmaniandinkle |
| Created | 2026-08-28 |
| Layer   | Substrate + Kernel (per ADR substrate-as-named-architectural-layer (`091-substrate-as-named-architectural-layer.md`) §2) — unchanged from the ADR this amends |
| Amends  | ADR memory-projection-reconciler (`097-memory-projection-reconciler.md`) |
| Refs    | ADR memory-projection-reconciler (the two-surface Reconcilable this extends), ADR skill-projection-reconciler (`098-skill-projection-reconciler.md` — sibling slice of the same harness-neutral bundle), ADR worktree-reconciler (`096-worktree-reconciler.md` — alarm-not-merge precedent), ADR daemon-reconcile-loop-driver (`095-daemon-reconcile-loop-driver.md`), `pkg/substrate/reconcile/registry.go` on branch `harden/registry-instance-names` (commit `cc359d4`, pushed to the org remote, **not yet on `origin/main`** — instance-name-as-path-component validation; see §9 and "Depends on" below), `~/workspaces/cog/.cog/mem/hermes-memory-index.cog.md` (the observed Hermes Tier-2 pairing hook) |
| Depends on | `harden/registry-instance-names` landing on `origin/main`. §9's registry-key reasoning is currently only verifiable against that unmerged branch, not against mainline. This amendment mints no registry key of its own either way, so nothing here is *blocked* on the landing — but the citation is not yet describing shipped behavior. |

## Context

### What ADR memory-projection-reconciler shipped

ADR memory-projection-reconciler (Accepted, 2026-05-17) specified `MemoryProjectionReconciler` as a bidirectional Reconcilable between exactly two memory surfaces: Claude Code auto-memory (`~/.claude/projects/*/memory/*.md`) and substrate cogdocs (`.cog/mem/**/*.cog.md`). That design is sound and unchanged by this amendment — five-way classification, alarm-not-merge on conflict, `type:` → sector placement, `projection.origin` provenance tracking. Nothing in §1–§8 of that ADR is revised here.

### A third surface has appeared since May

Hermes — the second harness this substrate runs agents in, alongside Claude Code — now ships a working two-tier memory tool. Tier 1 is a bounded working set (Hermes's own `MEMORY.md`-equivalent). Tier 2 is the overflow: when Tier 1 fills, Hermes **pages evicted entries into the substrate as cogdocs directly**, landing them in `.cog/mem/episodic/hermes-overflow/` and recording a pointer table at `~/workspaces/cog/.cog/mem/hermes-memory-index.cog.md` — one row per evicted entry (`summary | uri | date | tags`). The index documents a `hot` tag as a promotion marker meant to signal an entry's return to Hermes's own Tier 1, but nothing in the system consumes it today — see Open Questions §1.

This is structurally different from the Claude Code side of ADR memory-projection-reconciler in one load-bearing way: **Hermes already self-projects.** The harness→substrate write is not something `MemoryProjectionReconciler` needs to perform on Hermes's behalf — Hermes's own memory tool does it, unprompted, the moment Tier 1 evicts. What Hermes does *not* do is the reverse direction: nothing currently makes a Hermes-evicted cogdoc discoverable to a Claude Code session, or `cog_memory_search`-indexed the way a reconciler-projected cogdoc is, or eligible for promotion into Claude Code's own MEMORY.md when the same knowledge would be useful there.

`MemoryProjectionReconciler`'s `FetchLive` (ADR memory-projection-reconciler §2) today scans exactly two globs. It does not look at `hermes-memory-index.cog.md`, and it has no way to recognize "a harness wrote this cogdoc itself" — nor could it read that off the cogdoc's own frontmatter even if it tried: Hermes's writer (`~/.hermes/hermes-agent/tools/memory_tool.py`, `_write_cogdoc_to_substrate`, lines 773-786) stamps only `id`, `created`, `source`, `target`, `tags`. A Hermes-evicted cogdoc is invisible to the reconciler's pairing graph — not in conflict, not drifted, simply unseen.

### The membrane framing this composes with

Separately, the operator's harness-convergence research (`project_dsh_acp_self_managed_surface.md`, 2026-08-28 afternoon) identified that all three harnesses this substrate touches — Claude Code, Hermes, and dsh — independently converged on the same extension triad: skills, hooks, tools. The harness-neutral generalization is a bundle — `{skills, hooks, tools, identity, memory-contract}` — projected into each harness's own dialect (Claude Code's `settings.json` hooks, Hermes's plugin YAML, dsh's Cordis bundles→profiles→presets).

That research names the two halves of the resulting membrane explicitly:

- **Hooks are the substrate→harness direction.** Per-turn foveated context assembly (ADR-066/071 and ADR foveation-placement-under-prefix-cache-runtimes, `103-foveation-placement-under-prefix-cache-runtimes.md`) already delivers substrate state into a harness's live context window every turn. This is real, shipped, and fast (per-turn cadence).
- **Reconcilers are the harness→substrate direction.** A harness's own local state (a Claude Code memory file, a Hermes-evicted cogdoc, eventually a dsh Cordis session artifact) is carried back into substrate-canonical form on a slower cadence (periodic tick + fsnotify, per ADR daemon-reconcile-loop-driver §5).

`MemoryProjectionReconciler` is the **memory slice** of this general pattern — already named as such in ADR memory-projection-reconciler §8's generalization table (skills / memory / hooks / configs, each a `{Claude Code surface, substrate surface, Reconcilable}` triple). That table under-stated the shape in one respect: it modeled every surface as needing the reconciler to write both directions. Hermes shows a variant — a harness that writes its own harness→substrate leg — and the table's generalization needs a place for that variant, not a new table.

## Decision

### §9 — Hermes Tier-2 is a third `FetchLive` source, not a new Reconcilable

`MemoryProjectionReconciler`'s `FetchLive` (ADR memory-projection-reconciler §2) gains a third scan, alongside the two already specified:

3. Reads `{workspaceRoot}/.cog/mem/hermes-memory-index.cog.md` (or the configured equivalent path — see §11) and, for each indexed row, the cogdoc it points at. Records the row's `uri`, `tags`, and `date`, and the target cogdoc's mtime and content hash.

Resolving a row's `uri` to a file on disk requires two accommodations, because the index's live rows and the writer that produces new rows currently disagree on shape. Live rows in the observed index use `cog://mem/episodic/hermes-overflow/<slug>` with no `.cog.md` extension — all 92 rows in the current index resolve once `.cog.md` is appended. The writer shipping today (`memory_tool.py:788`) instead emits `cog://mem/overflow/{filename}`, extension already present, pointing at a `mem/overflow/` directory that does not exist on disk (this writer-side mismatch is being fixed Hermes-side in parallel; the resolver below is written to keep working across that fix landing rather than depend on it). The resolver MUST: (a) append `.cog.md` when the `uri` has no extension; (b) tolerate both the `mem/episodic/hermes-overflow` and `mem/overflow` path prefixes by resolving each against the substrate's actual overflow directory rather than the URI's literal prefix. A row whose `uri` still resolves to no file after both accommodations is not a parse error — see Acceptance criteria's fifth case, below.

This is additive. No new Reconcilable is registered, and no new registry key is minted — `RegisterMemoryProjectionProvider` (ADR memory-projection-reconciler §7) keeps the single instance name `memory-projection-reconciler`. `ValidateInstanceName` — which treats a registry key as a filesystem path component (`type[/instance]`, at most one `/`) and would reject the WorktreeReconciler `type:instance`-colon class of defect — exists today only on the unmerged branch `harden/registry-instance-names` (commit `cc359d4`, pushed to the org remote); it is **not** present in `pkg/substrate/reconcile/registry.go` on `origin/main`. This amendment does not depend on that function's behavior either way — it mints no new key at all, so there is no key here for `ValidateInstanceName` to validate or reject — but the citation should be read as "compatible with a hardening that has landed on a feature branch," not as a description of what mainline `registry.go` does today. If a future multi-Hermes-instance deployment needs per-instance scoping, the existing `memory-projection-reconciler/<workspace-slug>` shape (the `type/instance` shape §7's scoping note would imply) is the pattern to reuse, not a colon-joined ad hoc key — and at that point, `harden/registry-instance-names` landing on `origin/main` becomes the relevant enforcement mechanism to depend on.

### §10 — Index membership as the sole Hermes-surface discriminator

No `projection.origin: hermes` value is added to the ADR memory-projection-reconciler §6 table. Hermes's writer stamps only `id`, `created`, `source`, `target`, `tags` (§Context above) and this ADR's own `skip` plan action for `hermes-native-parked` (below) together with the byte-for-byte no-write guarantee (below) mean the reconciler never writes to a Hermes-authored cogdoc's content — between those two facts, nothing in the system would ever stamp `projection.origin: hermes` onto a cogdoc, so it cannot function as a discriminator. Asserting it as a frontmatter value describes behavior nothing implements.

The discriminator is **index membership**, not frontmatter: a cogdoc is Hermes-native if and only if some row of `hermes-memory-index.cog.md` (§9's third `FetchLive` scan) has a `uri` column resolving to that cogdoc's path, per §9's resolution rule (extension and path-prefix handling). No frontmatter field is read, required, or written on the cogdoc to make this determination. This also means an operator editing the cogdoc's frontmatter by hand cannot desync the classification the way editing `projection.origin` on the Claude-Code/substrate pair could (ADR memory-projection-reconciler §Consequences: "operators who edit these fields directly...may corrupt the reconciler's pairing graph") — there is no field there to edit.

A cogdoc discovered this way is **not** classified `claude-code-only-needs-projection` (nothing on the Claude Code side to reconcile *from*) and it is **not** `conflict-both-modified` (the reconciler never edited it, so there is nothing for it to have drifted against). It gets one new classification:

| Classification | Condition | Plan action |
|---|---|---|
| `hermes-native-parked` | Cogdoc's path appears as a `uri` value in some row of `hermes-memory-index.cog.md`. | skip — substrate-indexed via normal `cog_memory_search`, not promoted further |

This amendment ships `hermes-native-parked` only. It does not add a `hermes-native-promotable` classification or wire the index's `hot` tag to `create-claude-code-projection` — see Open Questions §1 for why promotion is deferred rather than shipped alongside parking.

If the reconciler wants a durable record of "I classified this cogdoc as Hermes-native on tick N," that is reconciler-side ledger state, not cogdoc state: an entry in the reconciler's own ledger (the same ledger that already tracks `projection.origin` / `projection.last-hash` for the Claude-Code/substrate pair, per ADR memory-projection-reconciler §6, and that `LoadConfig` mirrors into memory at boot per §2) keyed by the cogdoc's path, optionally carrying `origin: hermes` as an annotation. This value lives only in the reconciler's own ledger — it is never written into the cogdoc's frontmatter. The cogdoc itself stays byte-for-byte what Hermes's writer produced.

### §11 — Configuration surface

The Hermes index path is a resolved config value, following the same discipline ADR memory-projection-reconciler §"Implementation guidance" already requires for sector mapping: not hardcoded. Default: `{workspaceRoot}/.cog/mem/hermes-memory-index.cog.md`. Exposed alongside the existing `ClaudeProjectsRoot` / `SubstrateMemRoot` fields as `HermesMemoryIndexPath` on the `MemoryProjectionReconciler` struct (ADR memory-projection-reconciler §2) — resolved by the same `LoadConfig(ctx, workspaceRoot)` call that already resolves those two root paths today. This is a struct field, not a field on `MemoryProjectionConfig`: that is the separate struct `LoadConfig` returns, holding prior per-memory projection-ledger state (§2), not root-path configuration. An empty or missing path disables the Hermes scan entirely rather than erroring — a workspace with no Hermes seat attached has nothing wrong with it.

The watch mechanism (ADR memory-projection-reconciler §4) gains a third fsnotify watch on the index file's directory, debounced identically to the other two, calling the same `daemon.Trigger("memory-projection-reconciler")`.

### §12 — The membrane, restated with a third instance in hand

ADR memory-projection-reconciler §8 named the general pattern — cross-surface state gets a projection Reconcilable — and sketched skills / memory / hooks / configs as parallel instances. With Hermes as a live example, the more precise statement is a **direction split**, not a flat list:

- **Substrate → harness**: hooks. Per-turn, fast, already shipped for every harness this substrate drives (ADR-066/071 and ADR foveation-placement-under-prefix-cache-runtimes, `103-foveation-placement-under-prefix-cache-runtimes.md`, for Claude Code and the OpenAI-compatible proxy path Hermes talks through).
- **Harness → substrate**: reconcilers. Periodic-tick + fsnotify cadence. `MemoryProjectionReconciler` is the memory slice; `SkillProjectionReconciler` (ADR skill-projection-reconciler) is the skills slice. A harness may perform its own half of this direction itself (Hermes, for memory) or may need the reconciler to perform it on the harness's behalf (Claude Code, for memory — it has no eviction-to-substrate mechanism of its own).

Both halves are instances of the same harness-neutral bundle — `{skills, hooks, tools, identity, memory-contract}` — projected per-harness (Claude Code `settings.json`, Hermes plugin YAML, dsh Cordis bundles). This ADR is the memory-contract slice acquiring its second harness dialect. The hooks slice, the tools slice, and the identity slice remain future work, unchanged from ADR memory-projection-reconciler §8's "(future)" markers — this amendment does not build them, only names where the memory slice sits in the fuller picture so the next slice's author has a template that already accounts for a self-projecting harness, not only a passive one.

## Rationale

### Why fold Hermes into the existing Reconcilable rather than register a second one

The existing `MemoryProjectionReconciler` already owns the pairing graph, the conflict-alarm machinery, and the sector taxonomy. A second Reconcilable for the Hermes surface would duplicate all three and would need its own coordination with the first to avoid double-projecting a cogdoc that both instances scan. One Reconcilable that sees all three surfaces together can tell `hermes-native-parked` apart from `claude-code-only-needs-projection` by checking one thing (index membership, §10) rather than needing cross-instance communication. This is the same reasoning ADR memory-projection-reconciler §Rationale already gives for one instance per workspace rather than one per memory file.

### Why ship `hermes-native-parked` only, deferring promotion

An earlier draft of this amendment proposed gating promotion (Tier-2 → Claude Code MEMORY.md) on Hermes's own `hot` tag, reusing `create-claude-code-projection` unmodified whenever a row carried it. That signal turns out not to exist in production: zero rows in the live `hermes-memory-index.cog.md` carry `hot`, Hermes's writer (`memory_tool.py`, `_write_cogdoc_to_substrate` — the function begins at line 754 — lines 797-800) hardcodes its tag set to `memory-overflow,hermes-evicted,{target}` and can never emit it, and the read-side helper that would scan for it (`scan_tier2_index_for_hot_entries`, `memory_tool.py:840`) has no production caller — only test coverage. Shipping a classification keyed on a tag nothing ever sets would ship dead code with a passing-looking acceptance table.

The alarm-not-merge discipline (ADR memory-projection-reconciler §5, inherited from ADR worktree-reconciler §4) argues against inventing a second opinion about content the reconciler did not author, which still favors reusing Hermes's own signal over any reconciler-invented heuristic *if and when* that signal exists. It does not, today. This amendment therefore ships the half that is real — index-membership discovery and `hermes-native-parked` — and moves promotion to Open Questions §1, where it is scoped as a Hermes-side prerequisite rather than asserted as already-working reconciler behavior. Tradeoff: Hermes-evicted knowledge becomes substrate-searchable this tick, but a Hermes insight does not yet reach Claude Code's MEMORY.md automatically — that reunification waits on the Hermes-side change Open Questions §1 names.

### Why this is an amendment, not a revision of ADR memory-projection-reconciler in place

ADR memory-projection-reconciler is Accepted and implemented against; §1–§8 describe a shipped two-surface contract that remains correct for the surfaces it covers. Rewriting it to silently become a three-surface ADR would orphan any implementation or review history anchored to the two-surface acceptance criteria. The amendment pattern (ADR foveation-placement-under-prefix-cache-runtimes, `103-foveation-placement-under-prefix-cache-runtimes.md`, amending ADR-066/071, is the precedent in this corpus) keeps the original decision's acceptance intact and scopes the new decision to exactly the delta: one new `FetchLive` source, one new discovery mechanism, one new classification reusing an existing action.

## Consequences

### Positive

- Hermes-evicted knowledge becomes substrate-searchable the same reconcile tick it would be for a Claude-Code-originated memory, without Hermes needing to know anything about `MemoryProjectionReconciler` or vice versa beyond the shared index file's location.
- The direction-split restatement in §12 gives the next harness (dsh) and the next slice (hooks, tools, identity) a template that already accounts for a harness performing its own harness→substrate leg, which dsh's Cordis bundle architecture is likely to do as well (per the harness-convergence research this amendment cites).

### Negative

- A third watched path (the Hermes index file) adds background I/O to daemon boot, compounding the existing negative already flagged in ADR memory-projection-reconciler § Consequences about `MaxConcurrent = 1` tick-blocking on large corpora.
- `hermes-memory-index.cog.md` is a markdown table, not ledger-backed. The reconciler's read of it is a best-effort table parse, not a schema-validated read. A malformed row (a `|` inside a summary that was not escaped, for instance) degrades to a parse warning and that row being skipped for the tick, per the same non-fatal-degradation posture as ADR memory-projection-reconciler's `Health() = Degraded` semantics — it does not halt the reconcile cycle.
- This amendment ships no promotion path at all (Open Questions §1); the cross-project tag-meaning-drift risk a `hot`-based promotion would carry is deferred along with it, not incurred here.

### Neutral

- Workspaces with no Hermes seat see no behavior change: `HermesMemoryIndexPath` resolves to a path that does not exist, the scan is skipped, and the reconciler operates exactly as ADR memory-projection-reconciler originally specified.
- This amendment does not touch the alarm-not-merge conflict policy (ADR memory-projection-reconciler §5) at all — Hermes-origin cogdocs are never write-targets for the reconciler's Claude-Code-side sync, so they cannot enter `conflict-both-modified`.

## Implementation

Files to add or modify (in addition to ADR memory-projection-reconciler's list, which stands):

- `internal/engine/memory_projection_reconciler.go` — third `FetchLive` scan (`hermes-memory-index.cog.md` table parse); index-membership check in `ComputePlan` (no cogdoc frontmatter read or write for origin); `hermes-native-parked` classification
- `internal/engine/memory_projection_watcher.go` — third fsnotify watch, same debounce path
- `internal/engine/memory_projection_reconciler_test.go` — cases from the Acceptance criteria below
- `docs/adrs/three-surface-memory-projection.md` — this file

No changes to `pkg/substrate/reconcile/registry.go` are required; this amendment mints no new registry key (§9).

## Acceptance criteria

Extends ADR memory-projection-reconciler's acceptance table with:

| Memory state | Expected classification |
|---|---|
| Cogdoc exists at a path indexed by `hermes-memory-index.cog.md`; row untagged | `hermes-native-parked` |
| Cogdoc exists at a path indexed by `hermes-memory-index.cog.md`; row tagged `hot` | `hermes-native-parked` (promotion not implemented — see Open Questions §1) |
| `hermes-memory-index.cog.md` absent or unconfigured | Hermes scan skipped; two-surface behavior unchanged |
| Index row's `uri` resolves (per §9's resolution rule) to no file on disk | Row skipped; not classified; `Health()` reflects degraded-not-unhealthy for the tick, consistent with the malformed-row posture below |

Integration test cases to add:

1. **Hermes-native parked, no action**: seed a cogdoc using Hermes's real writer frontmatter shape (`id`/`created`/`source`/`target`/`tags` only — no `projection.origin`), add a matching untagged row to the index — run reconcile — assert no writes to either the cogdoc or Claude Code memory, assert classification is `hermes-native-parked`.
2. **`hot` tag is currently inert**: same seed, but the index row carries `hot` — run reconcile — assert the cogdoc is *still* classified `hermes-native-parked` and no Claude Code memory file is created. This locks in the deferred-promotion decision behaviorally; the expected result flips only when Open Questions §1 is resolved and a promotion path ships.
3. **Hermes index malformed row**: seed an index with one row missing its `uri` column — run reconcile — assert the malformed row is skipped, all other rows still classify correctly, assert `Health()` reflects a degraded-but-not-unhealthy state for the tick.
4. **No Hermes seat**: unset `HermesMemoryIndexPath` (or point it at a nonexistent path) — run reconcile — assert behavior is identical to ADR memory-projection-reconciler's existing test suite (no regression).
5. **Hermes index row resolves to no cogdoc**: seed an index row whose `uri` resolves, after §9's extension-and-prefix handling, to no file on disk — run reconcile — assert the row is skipped, no write occurs to either surface, and `Health()` reflects a degraded-but-not-unhealthy state for the tick, consistent with the malformed-row posture in case 3 above and with the negative Consequence noted for markdown-table parsing.

## Open questions

1. **Promotion (Tier-2 → Claude Code MEMORY.md) is not implemented in this amendment — what would it take to add it?** The `hot` tag that a promotion path would gate on is a dead signal today: zero rows in the live `hermes-memory-index.cog.md` carry it, Hermes's writer (`memory_tool.py`, `_write_cogdoc_to_substrate`, lines 797-800) hardcodes its tag set to `memory-overflow,hermes-evicted,{target}` and can never emit `hot`, and the read-side helper that would scan for it (`scan_tier2_index_for_hot_entries`, `memory_tool.py:840`) has no production caller today — only test coverage. Enabling a future `hermes-native-promotable` classification would be REQUIRED-BY work on the Hermes side, out of this ADR's scope: `memory_tool.py` writing `hot` on an explicit promotion command, plus wiring a production caller for the scanner. Tradeoff for shipping `hermes-native-parked`-only now instead of blocking this amendment on that Hermes-side work: Hermes-evicted knowledge becomes substrate-searchable immediately, at the cost of a Hermes insight not yet reaching Claude Code's MEMORY.md without an operator manually re-typing it. Once the Hermes-side prerequisite lands, a follow-up to this ADR should also settle whether Hermes's own promotion (Tier-2 → Tier-1) and the reconciler's promotion (Tier-2 → Claude Code MEMORY.md) can safely share one `hot` tag or need separate tags (`hot`, `hot-claude-code`) — the two harnesses may want different subsets promoted.

2. **Does a fourth harness (dsh) get its own `FetchLive` source added to this same Reconcilable, or does the pattern in §12 argue for a harness-count-agnostic redesign (e.g., a pluggable list of `HermesLikeSource` scanners) once there are three or more self-projecting harnesses?** Two data points (Claude Code needing the reconciler to write both directions, Hermes writing one direction itself) are not yet a stable generalization. This amendment hardcodes the Hermes case; a `HermesLikeSource` interface is recommended implementation guidance once dsh's actual self-projection behavior (if any) is observed rather than speculated about.

3. **Should `hermes-memory-index.cog.md` become ledger-backed** (each row a substrate event rather than a markdown table row), removing the best-effort-parse fragility flagged in Consequences? This is Hermes-project scope, not this ADR's — noted here because it would remove one of this amendment's stated negatives if it happened.

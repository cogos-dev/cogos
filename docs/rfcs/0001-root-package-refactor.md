# RFC-0001: Retire the root `package main`

| Field         | Value                                              |
|---------------|----------------------------------------------------|
| Status        | Draft                                              |
| Author        | @chazmaniandinkle                                  |
| Tracking      | [#104](https://github.com/cogos-dev/cogos/issues/104) |
| Target        | `v0.5.0`                                           |
| Anchor        | `v0.4.0` (pre-refactor regression baseline)        |

## Summary

The repository root contains 204 `.go` files that compile as a second `package main`. They are not built into the shipped binary, are not imported by anything, and reproduce roughly half of `internal/engine/` as a parallel kernel. This RFC retires the root package: every file is either deleted, ported into `internal/engine/`, or extracted into a leaf package under `internal/` or `pkg/`. The end state is a repository root that holds only build artifacts and the small handful of standalone files that genuinely live there.

## Background

Three pieces of context upgrade this from "rename a directory" to a real design problem.

**The root is already a parallel kernel, not a future kernel.** The Makefile builds `./cmd/cogos`, which contains a 17-line `main` that calls `internal/engine.Main()`. Since Track 5 Phase 4 (`74957b9`, Apr 2026) the installed binary has had no root code in it. `go build .` still produces a working `cog` binary because root retained its own `main()` at `cog.go:5554`, but that binary is a parallel artifact built from a divergent codepath. Track 5 Phase 5 (`e7a9a7e`) deleted ~11k LoC of root CLI surface on the explicit grounds that "root-package CLI surface is now truly dead." This RFC is the next phase of that line of work.

**The convention is already in production.** Five extractions have landed using a consistent pattern: `internal/workspace/` (`3d13365`), `internal/providers/component/` (`65fb4df`), `internal/linkfeed/` (`26ccda6`), `pkg/skills/` canonical impl (`04c606e`), and skills call-site migration (`1e8dac1`). Those commit messages reference "ADR-085" but no ADR-085 document is present in this repository. This RFC is the explicit codification of those rules. References to ADR-085 in past commits should be read as references to RFC-0001 going forward.

**The earlier abandoned attempts (`backup/*` tags) failed at scope, not technique.** All three tags share a shape: a single branch moving ~6 files at once across multiple unrelated subsystems (services + discord + components + linkfeed) entangled with a service-provider rewrite. The branch that *did* land — Wave 1a — used the same DI-seam pattern but moved one subsystem per PR. The technique works; trying to do everything at once doesn't.

## Goals

1. Root contains no Go files at completion. Targeting "fewer than ten" is a fallback if a genuinely root-scoped utility surfaces; the real target is zero.
2. Every behavior currently exercised by the 780 tests at root is either deleted (because the code is dead) or has a counterpart in the package that now owns the code.
3. `pkg/*` public APIs do not change for external consumers. Internal moves into `internal/*` carry no compatibility guarantee.
4. The work ships as a sequence of small PRs, each independently reviewable, each green, each touching one extraction.

## Non-Goals

- New features. Behavior preserved exactly, or the dead code that produced the behavior deleted.
- Reorganizing `internal/engine/`. Out of scope; separate RFC if and when needed.
- Adding new public API surface. ADR-085 rule 7 — no new exported types beyond what the move requires — applies.
- Renaming `pkg/uri` → `internal/uri` or any other inversion of an existing extraction.
- Any in-place rewrite. If `internal/engine/` already owns a subsystem, this RFC does not touch it.

## Current state

```
cogos-dev/cogos/
├── cmd/cogos/                  # 2 files — thin entry, providers wiring
├── internal/
│   ├── engine/                 # 214 files — the actual kernel
│   ├── eval/  linkfeed/  workspace/
│   └── providers/{component,daemon}/
├── pkg/                        # separate go.mod each (workspace via go.work)
│   ├── bep/  cogblock/  cogfield/  coordination/
│   ├── modality/  reconcile/  skills/  uri/
├── cog.go (150 KB)             # legacy monolith main()
├── 203 other root .go files    # the subject of this RFC
├── harness/  sdk/  envspec/    # separate go.mod each
└── docs/  scripts/  …
```

204 root `.go` files, all `package main`. They compile, build with `go build .`, and produce a binary divergent from the one shipped via `cmd/cogos`. Nothing imports them. 780 tests live in them.

Domain prefix counts (measured, not from the issue):

| Prefix         | Count | Existing target           | Disposition                               |
|----------------|-------|---------------------------|-------------------------------------------|
| `modality_*`   | 17    | `pkg/modality/` exists    | Most are thin re-exports; delete or move  |
| `bep_*`        | 16    | `pkg/bep/` exists         | Same: re-exports → delete                 |
| `reconcile_*`  | 14    | `pkg/reconcile/` exists   | Merge or delete                           |
| `agent_*`      | 14    | none                      | New `internal/agent/`                     |
| `bus_*`        | 11    | `internal/engine/` owns   | Delete; engine has the live impl          |
| `cmd_*`        | 8     | `internal/engine/` owns   | Delete; CLI surface is in engine          |
| `decompose_*`  | 6     | none                      | New `internal/decompose/`                 |
| `context_*`    | 6     | `internal/engine/` owns   | Delete                                    |
| `discord_*`    | 5     | none                      | New `internal/providers/discord/`         |
| `openclaw_*`   | 5     | none                      | New `internal/openclaw/`                  |
| `memory_*`     | 4     | `internal/engine/` owns   | Delete                                    |
| `identity_*`   | 4     | `internal/engine/` owns   | Delete                                    |
| `capability_*` | 4     | none                      | New `internal/capability/`                |
| `tier1..4_*`   | 7     | `internal/engine/` owns   | Delete                                    |
| `bep_proto/`   | dir   | `pkg/bep/`                | Move into `pkg/bep/proto/`                |
| Other singletons | ~75 | various                   | Per-file disposition (see Appendix A)     |

The "delete vs port" call has to be made file-by-file. Most root files have a peer in `internal/engine/` that's already the live implementation; the root version is dead. A minority are functionality that never made it into engine. Distinguishing them is the work.

## Proposed final layout

```
cogos-dev/cogos/
├── cmd/cogos/                  # entry point, unchanged
├── internal/                   # private implementation
│   ├── engine/                 # kernel daemon, unchanged scope
│   ├── eval/                   # already exists
│   ├── linkfeed/               # Wave 1a, already exists
│   ├── providers/
│   │   ├── component/          # already exists
│   │   ├── daemon/             # already exists
│   │   └── discord/            # NEW (split out of root + engine duplicate)
│   ├── workspace/              # already exists
│   ├── agent/                  # NEW: agent harness, dispatch, tools, providers
│   ├── decompose/              # NEW: decompose pipeline + TUI
│   ├── openclaw/               # NEW: openclaw bridge + projectors
│   └── capability/             # NEW: capability advertiser/cache/resolver
├── pkg/                        # public-API leaf packages
│   ├── bep/                    # add proto/ subpackage
│   ├── cogblock/   cogfield/   # unchanged
│   ├── coordination/  modality/  reconcile/  skills/  uri/
├── harness/  sdk/  envspec/    # unchanged separate modules
├── trace/  ui/                 # unchanged
├── go.mod  go.work  Makefile  …
└── docs/  scripts/             # unchanged
```

Public/internal/cmd discipline:

- `cmd/` — `main` packages only.
- `internal/` — kernel implementation. Anything not intended for external import. The Go compiler enforces this.
- `pkg/` — leaf libraries with semver public APIs, each in its own go.mod. Adding a new top-level `pkg/` subdir commits to module boundary and external consumability.

The `internal/foo/` vs `pkg/foo/` choice turns on one question: does code outside this repo import it today or near-term? `pkg/cogfield`, `pkg/modality`, `pkg/bep`, `pkg/reconcile` etc. answered yes. The targets in this refactor (agent, decompose, openclaw, capability, discord) are kernel-only and belong in `internal/`. Promoting `internal/` → `pkg/` later is mechanical; the reverse is much harder.

## Migration order

Order is determined by the dependency graph between root files, not by domain alphabet. The constraint is that every PR leaves the build green.

The dead-code dimension simplifies this: most root files are orphans with no inbound imports from anywhere in the live build closure. Deleting them is unconditional. Only the small set of root files referenced by other root files forms a real ordering graph.

**Stage A: Catalogue.** A single non-functional PR producing `docs/rfcs/0001-appendix-A-file-disposition.md` — a flat list of all 204 root files labelled `delete-orphan`, `delete-superseded-by-engine`, or `port-to-<pkg>`. Generated by `scripts/audit-root-package.sh` (added in the same PR), which compares each root file to its closest peer in `internal/engine/` and `pkg/*` and flags ambiguous cases for human triage. The script is preserved so any reviewer can re-run it on any commit.

**Stage B: Bulk orphan delete.** `delete-orphan` files (and their tests) deleted in a few large PRs grouped by domain (one for `bep_*`, one for `modality_*`, one for `reconcile_*`, one for `bus_*`, one for `context_*`, etc.). Mechanical: nothing imports the files, the build remains green by inspection. `chore(root):` prefix per `e7a9a7e`'s precedent. Expected to remove the majority of the 204 files.

**Stage C: Supersession deletes with verification.** For `delete-superseded-by-engine`, the audit script confirms every test at root has a counterpart in `internal/engine/` with equivalent coverage. Where coverage is missing, the test ports to engine first, then the root file is deleted. Per-domain: discord, identity, openclaw, capability, etc.

**Stage D: Port live extractions.** `port-to-<pkg>` follows the Wave 1a pattern — one PR per package, leaf-first:

1. `internal/capability/` — small, no inter-target dependencies.
2. `internal/decompose/` — depends on `pkg/cogblock` and stdlib only.
3. `internal/openclaw/` — depends on the OCI client; isolated.
4. `internal/providers/discord/` — needs the DI-seam pattern; previously attempted and reverted, so extra care.
5. `internal/agent/` — biggest move; touches `internal/engine/` only via interfaces. Done last for blast radius.

A separate PR promotes `pkg/bep/proto/` out of root's `bep_proto/`; small in size but touches the `pkg/bep` go.mod boundary and a generated-protobuf path.

**Stage E: Delete `cog.go`.** With everything else gone, `cog.go` is either a five-line shell aliasing `cmd/cogos` or deleted entirely. The latter is preferred. With `cog.go` gone there is no second `main` package, no second binary, and `make build` becomes the only path to a working binary.

Expected ratio: roughly 70% Stage B, 15% Stage C, 10% Stage D, 5% Stage E. The issue's framing — "hundreds of imports to rewrite" — overstates the work, because almost no root files are imported. The real work is the audit and coverage-equivalence check.

## Tooling

**`gofmt -s` and `goimports`** after every move. No hand-edits to import blocks.

**`scripts/audit-root-package.sh`** (new, added in Stage A) generates the disposition table. Pseudocode:

```
for each root *.go file:
  if no caller in ./cmd/cogos closure:
    label = delete-orphan; list inbound test references
  elif a same-named peer exists in internal/engine/:
    label = delete-superseded-by-engine
    list test functions at root but missing from engine peer
  else:
    label = port-to-?  (human triage)
```

**`gomvpkg`** — explicitly *not* used. It's designed for moves that preserve the public package API; here we delete and replace. It would also rewrite import paths in callers, but the only inbound import to root is from root itself, so the rewriting is no-op risk.

**Scripted `sed` for cross-package rewrites** — used only inside a single extraction PR (Stage D) to update local references after a move. Wave 1a already established this idiom.

**`cog.go`** in Stage E is read end-to-end by a human, every remaining symbol classified, then deleted or relocated explicitly. 150KB of one file does not get tool-driven moves.

## One sweeping PR vs. staged moves

**Recommendation: staged.** Concrete reasons:

- **The abandoned attempts failed at sweeping scope.** `backup/stash-wip-main` is +631/-273 across 13 files mixing four unrelated subsystems plus a service-provider rewrite. The branch that landed (Wave 1a) was three independent ~200-LoC PRs.
- **A 50k-LoC sweeping diff cannot be code-reviewed; it can only be run-tested.** A 500-LoC PR can be both. The project's PR template asks for evidence beyond "tests pass"; staged PRs are the only way to satisfy that bar.
- **Stages B and D have different review modes.** Stage B is bulk deletion (review: "is the audit script correct?"). Stage D is ports with potential behavior changes (review: "does the new package preserve the old contract?"). Conflating them in one PR loses both modes.
- **Bisectability.** A staged-PR regression bisects to the responsible PR in two or three steps; a sweeping-PR regression bisects to "the refactor" with no further resolution.

A sweeping PR would be defensible only if every root file were a definite orphan (Stage B unconditional). Stage D files make that not the case.

## Plugin author and external consumer impact

The issue's framing implies external Go consumers will see import paths shift. They won't, because no external Go code depends on root.

**`pkg/*` modules.** `pkg/modality`, `pkg/bep`, `pkg/reconcile`, `pkg/cogblock`, `pkg/coordination`, `pkg/skills`, `pkg/uri`, `pkg/cogfield` are independently importable Go modules with their own `go.mod`. Their public APIs do not change. No deprecation aliases are needed because no public symbol moves.

**`cogos-dev/cogfield` (named in #104).** This is a TypeScript/Vite frontend; it imports nothing from this Go module. The Go-side cogfield consumer is `pkg/cogfield/` *inside* this repo and is already at its target location. The cogfield smoke test is therefore: `pkg/cogfield/` builds standalone, and the kernel's `/api/*` HTTP contracts — none of which live in root — remain stable.

**External Go consumers of root.** There are none, by construction: root is `package main`, which Go does not allow other packages to import. Any external consumer "depending on root" depends on the *binary*'s behavior, and the shipped binary has been built from `cmd/cogos` since Track 5 Phase 4. This refactor preserves that binary's CLI surface byte-for-byte; it removes only the parallel `go build .` path nobody is supposed to be using.

**Hooks, scripts, CI.** `scripts/cog` resolves to `~/.cogos/bin/cogos`, built from `cmd/cogos`. No hook or script invokes `go run .` against root. The lint target at `Makefile:170` greps `*.go` at root; once root is empty, the grep matches nothing — Stage E redirects it to `internal/engine/*.go` and `pkg/*/**.go`.

Release notes for v0.5.0 should say: "`go build .` no longer produces a binary; build with `make build` or `go build ./cmd/cogos`. The shipped binary is unchanged."

## In-flight PR rebase strategy

At RFC submission, the v0.5.0 milestone has three open issues (#98, #101, #104) and zero open PRs. This is a friendly window.

1. **Stage A lands first.** Non-functional, zero merge-conflict risk. Once merged, every reviewer of every later PR has a shared map.
2. **Stage B coordinates with #101 by avoidance.** Issue #101 is services extensibility; it lands in `internal/engine/` (and possibly a new `internal/services/`). It does not need to touch root. The two streams run in parallel without overlap.
3. **Stage C is the friction surface.** If a #101 PR ports a root file into engine, a Stage C PR may target the same file. The convention: whichever PR touches a root file first stakes the claim by deleting it; the second PR rebases and drops its now-stale move. The audit script makes "what's the current root state?" a cheap query.
4. **Stage D PRs are individually small** (one extraction each, ~200-500 LoC). Rebase cost on top of v0.5.0 churn is bounded.
5. **Hold Stage E (delete `cog.go`) until last,** after the milestone's other issues close. A 150KB delete in flight while #101 is doing extensibility work invites trouble.

Once Stage A merges, root is announced in the issue tracker as a frozen surface for v0.5.0: new files land in `internal/`, `pkg/`, or `cmd/`, never at root.

## Why the abandoned attempts failed

Three tags exist: `backup/pre-refactor-stash-2` (a tree object only, no commit), `backup/stash-wip-main` (stash-on-WIP at `3d27dfc`), `backup/stash-parallel-wip` (the parking commit for "parallel-refactor-wip-parked-by-component-extract").

`backup/stash-wip-main` mixed four unrelated changes in one branch: linkfeed, discord, and component extractions plus a `service_provider` rewrite. The Wave 1a series did three of those four as independent PRs. The discord move that didn't land was the one entangled with the service_provider rewrite — a *behavioral* change blocked on its own merits, dragging the structural move with it.

`backup/stash-parallel-wip` is the inverse: a parking commit that reverted those moves to keep main clean while the entanglement got untangled. The branch name itself narrates the failure — a parallel refactor was happening, the component-extract PR landed cleanly, and the parallel branch had to step out of its way.

One-line lesson: **don't combine extractions with rewrites in the same PR.** This RFC commits to the Wave 1a discipline:

1. One extraction per PR.
2. Behavior preserved exactly. Behavior changes are separate PRs before or after.
3. DI seams (function variables wired by an `init()` adapter in the caller) when the moved package needs a callback into the still-monolithic caller. ADR-085 rule 7 — duplicate small helpers, don't export them — applies.
4. Type aliases at call sites during the migration window so existing code compiles unchanged; remove in a follow-up once all callers are updated.

## Risk: is the refactor genuinely too tangled?

It isn't. The dependency analysis above (no inbound imports to root, `internal/engine` doesn't depend on root) means the risk surface is contained: Stage B's deletions are unconditional, Stage C's verifications are mechanical, Stage D's ports follow a recipe used four times already. The only genuinely hard work is the audit script in Stage A. If the audit reveals a class of root file not anticipated here — code referenced from generated files, or from outside the `./...` build closure via a `//go:build` tag — that case is an addendum to this RFC, not a re-architecture.

The largest concrete risk is hidden test coverage at root that would silently disappear. The 780 root tests are the answer key for "did the engine port preserve behavior?"; if a Stage C delete drops a test that catches a regression engine doesn't, we lose coverage. Stage A's audit script must surface this — for every `delete-superseded-by-engine` candidate, list test names at root and at the engine peer, and require human attestation of coverage equivalence. This is the only expensive part of the audit.

## Acceptance / done definition

This RFC merges as Draft. Implementation proceeds against #104. The refactor is done when:

- Root contains zero `.go` files (preferred) or only `doc.go`.
- `go build ./...` and `go test ./...` are green.
- `make e2e-local` passes.
- The cogfield JS frontend loads against a kernel built from this branch and renders the constellation viewer. Smoke-only — not a full feature pass.
- `pkg/*` go.mod boundaries unchanged: `cd pkg/<each> && go build ./...` passes independently.
- `CHANGELOG.md` v0.5.0 entry notes that `go build .` no longer produces a binary; the shipped binary is unchanged.
- This RFC's status flips Draft → Implemented in a final commit.

Staged gates: B and C must pass `go test ./...`. D must additionally pass `make e2e-local`. E must additionally smoke-test cogfield.

## Out of scope

- Any rearrangement of `internal/engine/`. That package's internal organization is its own design problem.
- New functionality in any extracted package beyond what the move requires.
- Renaming `cmd/cogos/` or the binary name.
- Migrating any `pkg/*` go.mod boundary or consolidating modules back into root.
- Splitting `internal/engine/` into subpackages — separate RFC if desirable.
- Bus protocol, ledger format, identity-card schema, or any persistence/wire concern.
- Consolidating `harness/`, `sdk/`, and `envspec/` modules.
- Updating `docs/SYSTEM-SPEC.md`, `docs/PLATFORM.md`, etc. to reflect the new layout. Those follow the implementation; doing them speculatively invites churn.

## Open questions

These are deliberately not answered in this RFC; they should be resolved during Stage A.

1. **Does anything in `harness/` or `sdk/` import root?** A quick `grep` says no, but the audit script should confirm.
2. **Do generated files (e.g. `bep_proto/`'s output) reference root types by name in their generated output?** If yes, regenerating is part of the move.
3. **Are any tests at root tagged with `//go:build` constraints that exclude them from the default build?** If yes, they need separate handling in Stage A's coverage diff.
4. **Should the audit script be preserved post-refactor as a CI guard ("no new files at root")?** Recommendation: yes, as a small `make lint` step. Decided on the final Stage E PR.

## Appendix A — File disposition (deferred to Stage A)

The full per-file disposition table is generated by `scripts/audit-root-package.sh` in Stage A and committed as `docs/rfcs/0001-appendix-A-file-disposition.md`. The shape:

| File                        | Size | Disposition          | Target                | Rationale                  |
|-----------------------------|------|----------------------|-----------------------|----------------------------|
| `agent_harness.go`          | 32 K | port                 | `internal/agent/`     | live; biggest in agent_*    |
| `bep_engine.go`             | 17 K | delete-orphan        | —                     | superseded by `pkg/bep`    |
| `modality_bus.go`           | <1 K | delete-orphan        | —                     | thin re-export only        |
| `cog.go`                    | 150K | port + delete        | mostly delete         | last in Stage E            |
| …                           |      |                      |                       |                            |

Stage A is responsible for filling this in. This RFC commits to the *categories* and the *process*, not to the per-file calls — those depend on machine-readable analysis the audit script performs.

## Appendix B — Decision provenance

- Wave 1a precedent: commits `3d13365`, `65fb4df`, `26ccda6`.
- Track 5 precedent: commits `902a062`, `745d3c1`, `c147691`, `74957b9`, `e7a9a7e`, `83e0e71`.
- ADR-085 referenced in past commit messages is codified by this RFC; future commits should reference RFC-0001 instead.
- Issue #104 is the tracking issue.

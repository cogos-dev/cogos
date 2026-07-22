---
type: adr
id: ADR-121
layer: spec
title: "ADR-121: Single-Binary Consolidation — Fold the CLI Into cogos, Delete the Legacy Root Package"
created: 2026-07-13
status: accepted
tags: [adr, consolidation, cli, single-binary, dead-code, go-layout, supersedes-track5]
author: chaz
refs:
  - rel: grounds
    description: >
      The 2026-04-21 consolidation survey bundle (internal), specifically the
      revised Track 5 plan: subcommand migration before deletion, HTTP contract
      preservation, and the finding that the root serve web deletes as one PR
      or not at all. Phases 1-3 of that plan have since landed organically
      (the daemon carries emit, mcp, and the full /v1/bus contract today);
      this ADR resumes and completes the remainder. (internal reference omitted)
  - rel: grounds
    description: >
      The 2026-06-24/25 kernel audits (internal) — their remediation choreography
      (safe tier merged mechanically / sign-off tier / documented-decide tier)
      is the wave discipline this ADR adopts. (internal reference omitted)
  - rel: grounds
    description: >
      A 2026-07-13 implementation study (six subsystem readers + adversarial
      critic over a4d7477) and a same-day three-seat consolidation audit
      (CLI usage inventory across the operator's machine, a per-file census of
      the 230 root-package files, and a Go-practices review). All liveness and
      deadness claims below were verified by grep in those passes.
      (internal reference omitted)
  - rel: composes-with
    description: >
      ADR-100 substrate library extraction — the pkg/substrate boundary this
      ADR leaves intact; module-collapse decisions below stay inside ADR-100's
      categorical cut.
---

# ADR-121: Single-Binary Consolidation

## Context

The repo carries two `package main`s. The shipped daemon is `cmd/cogos` →
`internal/engine` (releases, launchd, self-update — the local kernel already
runs the release line, which this ADR formalizes as an invariant). The repo
root is a second, legacy `package main` (230 files, ~87.5k LOC) that still
compiles to a standalone CLI.

The root package is **not dead**. The operator's daily `cog` command resolves
through wrapper scripts to a stale shadow build of it (v2.1.1, 2026-06-03)
parked inside the workspace. Thirteen verbs are operationally load-bearing —
skills, runbooks, and the binary's own agent-tool loop shell out to them — and
a documented 2026-05-20 incident (wrapper fell through to the daemon binary;
`ref`, `memory toc`, `coherence check`, `tree build` silently broke) proves
both that this surface matters and that hosting it on an unreleased shadow
binary is fragile.

Around those verbs sits verified cruft: ~43k LOC with zero non-test callers
(the phase-2 agent harness, v2 context engine, root reactor/tool-router,
modality prototype, decompose, merge/replay experiments, retired subcommand
backends), ~3.9k LOC of engine duplicates and alias shims, and two go.work
modules whose only importers are root files. CI carries a dedicated
windows-build job that exists solely to defend against the root package's
Unix-only syscalls.

## Decision

1. **One binary.** The `cogos` binary (cmd/cogos) absorbs the CLI: a
   `internal/cli` package registering the ported verbs on the daemon's
   existing subcommand dispatch. `cog` survives as a symlink/alias to
   `cogos`. No wrapper routing to shadow binaries; the release binary is the
   only binary.
2. **Release-only local kernel** (already true operationally): dev builds are
   never installed to the service path; the launchd service always runs a
   tagged release, custom or public.
3. **Port exactly the load-bearing surface, then delete the rest.** Thirteen
   verbs port; twenty-one verbs drop with evidence recorded; the remaining
   root package deletes wholesale. Git history is the archive.
4. **Prefer thin ports.** Where the daemon already serves the capability over
   HTTP/MCP (bus tail/watch → the SSE streams; health; events), the ported
   verb is a client of the running kernel, not a second implementation.
   Thick ports (memory, ref, verify, coherence — must work with no daemon
   running) link the workspace libraries directly. Each verb's port notes
   which mode and why.

## The port surface (must not regress; verified callers on record)

`memory` (search/read/write/toc/index — heaviest), `coherence`
(check/drift/baseline/status), `bus` (list/tail/watch), `verify`, `infer`
(one-shot scripting completion), `workspace/ws`, `node` (identity/lifecycle
flavor — renames to avoid colliding with the daemon's cluster `node`),
`coord`, `events` (list/show/explain/query/narrative), `constellation`
(index/search/health/stats), `health` (workspace-integrity flavor — becomes
`cogos health --workspace` or similar), `skill list`, `ref`
(resolve/hash/verify/canonical/check), plus `init` genesis semantics folded
into the daemon's existing `init`.

Name collisions (`health`, `node`, `init`) are resolved in favor of the
daemon's existing meaning; the workspace-flavored verb gets the qualified
name. The migration table (old invocation → new invocation) ships with the
cutover PR and updates every skill/runbook in the same change.

## Waves (each an independently revertable PR; strict order)

- **Wave 0 — guardrails.** `golangci-lint run --enable unused` snapshot as a
  cross-check on the census; `tidy-all` Makefile target + CI step; capture
  the current wrapper behavior as a characterization test (every port-list
  verb invoked --help against the shadow binary, recorded).
- **Wave 1 — port.** `internal/cli` with the thirteen verbs (thin/thick per
  verb), tests, and the daemon dispatch wiring. Ships in a release. Nothing
  deleted yet; the shadow binary keeps working throughout.
- **Wave 2 — cutover.** Wrapper scripts and skills flip to the new verbs (one
  mechanical pass over the recorded caller inventory); the shadow binary and
  wrapper routing retire; the 2026-05-20 incident's checklist re-run as the
  acceptance gate. Point-in-time, coordinated with the running kernel.
- **Wave 3 — the sweep.** Delete the root `package main` in one PR (the
  compile graph forces it; established by the prior survey's blocked attempt
  to do it piecewise), along with: `pkg/coordination` and `pkg/modality`
  (dead modules + their only consumers), the engine-duplicate clusters, the
  alias shims, and the CI windows-defense job. `go build ./...`,
  `go vet ./...`, and `go test ./...` become meaningful over the whole tree.
- **Wave 4 — documented-decide tier** (operator judgment, not mechanical):
  the root identity/rbac/discord/service providers whose daemon twins are
  Health-only stubs — port the real logic into `internal/providers/` or
  accept the loss explicitly; the TAA foveated-context tier pipeline (real,
  working, unclear reach); `tui`/`dashboard` (interactive-only, zero scripted
  callers); `salience`, `timeline` depth, `oci`-in-`version` display;
  pkg/→internal moves for packages with no external consumers; `harness/`
  module fate.

## Constraints

- The `/v1/*` HTTP contract does not move (external consumers: sandbox
  bridge, dashboard, hooks). Internals refactor freely.
- Hook scripts are untouched (they already bypass the Go CLI via dispatch.py).
- The running kernel is drained/restarted deliberately at cutover, never as a
  side effect (running services are replaceable peers; identity lives in the
  ledger, not the process).
- Release-line continuity: every wave that touches the binary ships through
  the normal tag → CI → self-update path.

## Consequences

One binary to install, version, and reason about; the CLI inherits the
daemon's release discipline and stops silently drifting from the kernel; the
repo drops roughly half its Go LOC without losing a verb anyone uses; CI
stops defending a package nobody ships; and the go.work matrix shrinks to
modules that earn their independence. The cost is a coordinated cutover and
the Wave 4 decisions, which are deliberately separated so the mechanical
work never waits on the judgment calls.

## Execution note (root deletion landed ahead of the verb-porting waves)

The single entrypoint is **`cmd/cogos` → `internal/engine.Main()`**. This is
not new as of this deletion — every artifact-producing build target
(Makefile's `cog` target and all OS/arch variants, `make install`, CI's
`ci.yml` build/mcpserver/release jobs, the Dockerfiles, `scripts/setup-dev.sh`)
already compiled exclusively from `./cmd/cogos`. The root `package main` was
never in that build graph; its only Go consumers were its own 232 files, and
the only place it compiled at all was `ci.yml`'s untagged `go build ./...`
Windows cross-compile correctness check, which produced no shipped artifact.

Given that, the root package's 232 `.go` files were removed in this pass as
a **mechanical dead-source deletion**, verified by: `git rm` of every
root-level `*.go` file in a throwaway worktree off `origin/main`, followed by
`go clean -cache && go build ./...` and `go vet ./...` both exiting 0, with
`cmd/cogos` still building and running `version` correctly. That proves no
Go source anywhere consumes root — it does not, by itself, prove verb
parity in the shipped CLI.

**This does not complete Waves 1–2.** Root carried roughly 36 CLI verbs
against the daemon's ~27 (largely disjoint); the thirteen load-bearing verbs
enumerated in "The port surface" above (`memory`, `ref`, `verify`, `coord`,
`events`, `constellation`, `skill list`, etc.) were **not** ported into
`internal/cli` before this deletion, because that parity gap already existed
on `origin/main` independent of whether root's source was present — the
canonical build has delegated to `engine.Main()` all along. Deleting dead
source neither creates nor closes that gap. Any wrapper script or skill still
shelling out to a stale, disk-resident `cog` build (predating this ADR's
Makefile switch to `cmd/cogos`) is unaffected by this deletion and unblocked
by it either; it will keep resolving to whatever is on disk until its next
rebuild.

Waves 1–2 (port the thirteen verbs into `internal/cli`, cut wrappers/skills
over, retire shadow-binary routing) and Wave 4 (root
identity/rbac/discord/service providers, TAA tier pipeline, `tui`,
`pkg/`→`internal` moves) remain open follow-up work, tracked as this ADR's
continuation, if verb parity in the shipped `cogos`/`cog` binary is the
operator's goal. This deletion should be read narrowly: dead legacy source is
gone and the build graph is simpler; it is not a claim that every root verb
survives somewhere reachable today.

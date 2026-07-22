# Wave 4 preserved providers (ADR-121)

This directory holds root-only real implementations that PR #464's deletion
commit (`76c09ad`, "chore(cli): delete legacy root package main per ADR-121")
would otherwise have consigned to git history only. Per operator ruling, they
are preserved as **live working-tree source**, not archaeology: restored
verbatim from the pre-deletion parent commit
`e72538cb3bb3633e34e5ebff5a1e8ee243862e22` (the ADR-121 doc commit,
`e72538c`, one hop before the deletion).

## Why this directory doesn't build

The directory name starts with `_`, which Go tooling (build, vet, test, `go
list ./...`) ignores unconditionally. The files below still declare `package
main` and reference other root-only symbols that no longer exist anywhere in
the tree (e.g. root's old `BusEmit`, `RegisterProvider`, `NewRBACProvider`
plumbing). That's fine — nothing here is meant to compile in place. It is
archived-but-in-tree source: readable, diffable, greppable, and exempt from
`go build ./...`, `go vet ./...`, and CI, while remaining a normal tracked
part of the repository rather than a detached history reference.

## ADR-121 context

ADR-121 ("Wave 3 — the sweep") deleted the root `package main` in one PR
because the compile graph forced an atomic removal. It explicitly separated
that mechanical deletion from **"Wave 4 — documented-decide tier"**: *"the
root identity/rbac/discord/service providers whose daemon twins are
Health-only stubs — port the real logic into `internal/providers/` or accept
the loss explicitly."*

Of those four providers, `identity` and `service` already have real daemon
twins (identity via `serve_identity_grants.go`, PR #472) — their root copies
were genuinely superseded and correctly stayed deleted. `rbac` and `discord`
do not: their daemon-side registrations
(`internal/providers/daemon/daemon.go`) are `Health()`-only stubs (`rbac` has
no daemon registration at all; `discord`'s `discordProvider` embeds
`stubMethods` and implements only `Type()` + `Health()`). The files below are
the real logic that stub is standing in for.

> **Update:** `discord` was re-homed out of this directory (see "Re-homed"
> below) by the PR that also carries PR #470's `ConfigExporter`/`--snapshot`
> payload forward onto the ported provider. `rbac` remains here, unresolved,
> per its own Wave 4 instruction (item 1) below.

## Preserved files

| File | Lines | Why reserved for Wave 4 |
|---|---:|---|
| `rbac_provider.go` | 814 | `RBACProvider`, the Reconcilable implementation for RBAC binding CRDs (`RoleBindingCRD`, `AccountBindingCRD`, `NodeBindingCRD`, `WorkspaceBindingCRD`, `HarnessBindingCRD`): `LoadConfig`/`FetchLive`/`ComputePlan`/`ApplyPlan`/`BuildState`/`Health`. No daemon twin — `internal/engine`'s `HarnessAttacher` interface only expects an implementation; `pkg/substrate/identity.HarnessRegistry` satisfies it with an in-memory session-binding registry, not the full YAML-backed CRD reconciler this file provides. |
| `rbac_provider_test.go` | 640 | Unit tests for `RBACProvider`: `LoadConfig`, `ComputePlan`, `ApplyPlan`+`FetchLive`, `Health`. |
| `rbac_provider_e2e_test.go` | 379 | Hermetic end-to-end tests closing the deferred integration coverage from PR #285 (K8s-style RBAC binding CRDs): full `LoadConfig → FetchLive → ComputePlan → ApplyPlan → BuildState → Health` cycle against a tempdir workspace. |
| `rbac_bindings_wiring.go` | 22 | The `init()` registration that wires `RBACProvider` into the global reconcile registry (`RegisterProvider("rbac-bindings", provider)`) with a stub bus-emit adapter. Real glue for the real provider — kept alongside it so the registration pattern isn't lost, distinct from the alias-shim family below. |

**Total: 1,855 lines across 4 files remaining (rbac only).**

## Re-homed

- **`discord_provider.go`, `discord_provider_test.go`, `discord_reconcile.go`,
  `discord_hcl.go`, `discord_hcl_test.go`** — moved to
  `internal/providers/discord/` (Wave 4 instruction item 2, below). The
  daemon's `discordProvider` stub in `internal/providers/daemon/daemon.go` was
  replaced with the real ported `*discord.DiscordProvider`, registered at the
  same `RegisterProvider("discord", ...)` call site. The re-homed provider
  also carries forward PR #470's `ExportConfig`/`cogos reconcile discord
  --snapshot` payload, which had gone stale against this directory's
  post-archival layout. See that PR for context; this directory's discord
  entry supersedes it.

## Deliberately NOT preserved (correctly left deleted)

- **`rbac_bindings.go`** (129 lines) — a zero-churn alias shim only. Its own
  header states it plainly: *"Canonical RBAC CRD types, loaders, and
  validators live in `pkg/substrate/identity` per ADR-100 P0 extraction. Type
  aliases and forwarding functions let existing call sites... compile
  unchanged."* The canonical types already live at
  `pkg/substrate/identity/rbac_bindings.go`. Nothing is lost by this file's
  deletion.
- **`rbac_bindings_test.go`** (811 lines) — round-trip/validation tests
  against the aliased CRD types above, i.e. tests of the shim's forwarding,
  not of root-only logic. Excluded as part of the same shim family.
- **`rbac.go`** / **`rbac_test.go`** (404 / 549 lines) — a separate, older
  subsystem (`RoleLoader`, `PathChecker`, "Secure implementation replacing
  roles.sh") for flat-file agent-role definitions. `rbac_provider.go` does
  not reference `RoleLoader` or `Role` at the symbol level (checked directly:
  no matches). This is not one of the "identity/rbac/discord/service
  providers" ADR-121 names for Wave 4 — it predates and is orthogonal to the
  Reconcilable-provider pattern — so it is out of this preservation's scope.

## Wave 4 instruction (when the daemon needs rbac/discord reconciliation)

1. Repackage `rbac_provider.go` (+ `rbac_provider_test.go`,
   `rbac_provider_e2e_test.go`) into `internal/engine` or a new
   `internal/providers/rbac` package. Replace its dependency on the deleted
   root `rbac_bindings.go` alias with a direct import of
   `pkg/substrate/identity` (the canonical types the alias used to forward
   to — no functional gap, just an import-path fix). Re-home the
   `rbac_bindings_wiring.go` registration pattern (`init()` →
   `NewRBACProvider` → `RegisterProvider`) as the wiring appropriate to the
   target package (e.g. alongside `internal/providers/daemon`'s other
   `RegisterProvider` calls), replacing the stub bus-emit adapter with a real
   one if `AppendEvent`/modality-bus wiring has landed by then.
2. ~~Repackage `discord_provider.go` + `discord_reconcile.go` + `discord_hcl.go`
   (+ both test files) into `internal/providers/daemon` (or a sibling
   `internal/providers/discord` package), replacing the `internal/providers/daemon/daemon.go`
   `discordProvider` stub's `Type()`/`Health()`-only implementation with the
   real `LoadConfig`/`FetchLive`/`ComputePlan`/`ApplyPlan`/`BuildState` cycle
   this code provides.~~ **Done** — re-homed to `internal/providers/discord/`;
   see "Re-homed" above.
3. In both cases (rbac still pending, discord done): drop the leading `_`
   directory nesting, restore normal package declarations (no more `package
   main`), fix imports to current module-relative paths, and re-run `go build
   ./...`, `go build -tags fts5 ./...`, `go vet ./...`, and the tests before
   merging.
4. If the operator instead accepts the loss explicitly for either provider,
   delete the corresponding files from this directory and drop their row
   from the table above in the same commit as that decision.

Source commit for all preserved files:
`e72538cb3bb3633e34e5ebff5a1e8ee243862e22` (parent of the deletion commit
`76c09ad08a489e3f086580f70bb58ec171fb4c0e`, branch `adr/121-single-binary`).

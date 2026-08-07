---
type: adr
id: ADR-099
title: "ADR-099: Node Identity Layering — Three-Layer Clarification and Canonical Path Forward"
status: accepted
created: 2026-05-18
refs: "ADR-062, ADR-073"
---

# ADR-099: Node Identity Layering — Three-Layer Clarification and Canonical Path Forward

---

## Problem statement

A Wave 6b audit reported a "node-identity schism" between two implementations:

- `cog/.cog/node_identity.go` — Ed25519, with `NodeID = sha256(public_key_bytes + role + genesis_timestamp)` prefix `"sha256:"`
- `constellation/identity.go` — ECDSA P-256, with `NodeID = hex(sha256(DER(pubkey)))`

The proposed resolution was to retire the former in favour of the latter, since the design spec (`project_cogos_identity_as_crd.md`) specifies ECDSA P-256. However, closer investigation reveals these are **not competing implementations of the same thing**. They are three distinct layers with separate scopes, none of which currently conflicts with another.

---

## Investigation findings

### Layer 1 — Constellation mesh identity (`constellation/identity.go`)

- **Package:** `github.com/myrgic/constellation` (separate repo, separate Go module)
- **Crypto:** ECDSA P-256
- **NodeID derivation:** `hex(sha256(DER-encoded public key))` — no role or timestamp input
- **Purpose:** L1 peer identity in the decentralised hash-chained ledger. Nodes sign heartbeats and ledger events. ID is stable across reboots.
- **Storage:** `<data_dir>/identity/node-key.pem`
- **Consumed by:** `constellation.Node` (peer mesh protocol)
- **Status:** Production code; no changes required.

### Layer 2 — Workspace operational identity (`cog/.cog/node_identity.go`)

- **Package:** `package main` inside the cog workspace CLI (NOT part of the cogos kernel repo)
- **Crypto:** Ed25519 (via `crypto.go:EnsureKeypair`)
- **NodeID derivation:** `"sha256:" + hex(sha256(pubkey_bytes + role_bytes + genesis_timestamp_bytes))`
- **Purpose:** Workspace-scoped stable identity for the cog workspace CLI (`serve.go`, `registration.go`, `cmd_signal.go`). Seeds the ledger genesis event and is referenced in workspace signals.
- **Storage:** `.cog/identity.json`; key material in `.cog/`
- **Consumed by:** `cog/.cog/serve.go`, `cog/.cog/registration.go`, `cog/.cog/cmd_signal.go`
- **NOT consumed by:** the cogos kernel (`github.com/myrgic/cogos`) — zero cross-references found
- **Status:** Operational; the existing `cog/.cog/identity.json` encodes a node that has been running since 2026-03-05. Retirement would require a key migration.

### Layer 3 — CogOS kernel node identity (`cogos/cmd_node.go:NodeIdentity`)

- **Package:** `github.com/myrgic/cogos` (this repo)
- **Format:** YAML at `~/.cog/node/identity.yaml`
- **Fields:** hostname, machine_uuid, type, OS, arch, capabilities
- **Purpose:** Machine-level node registration for multi-node deployment (ADR-063). NOT a cryptographic identity — no keys, no signing.
- **Consumed by:** `cogos node info`, `cogos node init`
- **Status:** Operational; unrelated to either Layer 1 or Layer 2.

---

## Why the "schism" is not a conflict today

The audit characterised Layer 2 as a "drift" from Layer 1. The characterisation was accurate — the two use different crypto and different derivation formulas — but the inference that one must be retired was premature. The layers address different scopes:

| | Layer 1 | Layer 2 | Layer 3 |
|---|---|---|---|
| Repo | constellation | cog workspace CLI | cogos kernel |
| Purpose | Peer mesh signatures | Workspace operational ID | Machine registration |
| Crypto | ECDSA P-256 | Ed25519 | None |
| Imported by cogos kernel? | No | No | Yes (cmd_node.go) |

No code currently bridges Layer 1 and Layer 2. The cogos kernel does not import `github.com/myrgic/constellation`. The cog workspace CLI does not import `github.com/myrgic/constellation` either (it is a separate binary).

---

## The canonical path forward (decided)

The three-layer identity design spec (Layer definitions) specifies:

> L1 — Node: `kind: Node` (already exists), ECDSA P-256, NodeID = hex(SHA-256(DER(pubkey))), git-backed hash-chained ledger

The canonical node identity algorithm at L1 is **constellation's ECDSA P-256** derivation. This is correct and is NOT changed by this ADR.

Layer 2 (cog workspace CLI Ed25519 identity) is a **bootstrap artefact** from before the constellation L1 was designed. It predates the CRD model. The path forward is:

1. **Do not retire Layer 2 in this wave.** Existing `.cog/identity.json` files are operational and referenced in ledger genesis events. Retirement requires a documented key migration procedure.

2. **Gate Layer 2 retirement on three preconditions:**
   - The cogos kernel imports `github.com/myrgic/constellation` (bringing L1 identity into scope)
   - The kernel's `IdentityProvider` (Wave 6b+) can project an L2 spec as an L1 node binding (i.e., Layer 2's cog-workspace operational identity maps onto an `Identity` CRD)
   - A migration CLI command (`cogos node migrate-identity --from ed25519 --to ecdsa-p256`) is written and tested

3. **Document Layer 2 as "deprecated-pending-migration"** — add a code comment to `cog/.cog/node_identity.go` pointing to this ADR.

4. **Add clarity to `cogos/cmd_node.go`** — add a comment to Layer 3's `NodeIdentity` type noting it is a machine-registration record, not a cryptographic identity, and distinct from L1/L2.

---

## Changes in this PR

- This ADR document (docs/adrs/099-node-identity-layering.md)
- Code comment added to `cogos/cmd_node.go` `NodeIdentity` struct clarifying Layer 3 scope
- No code changes to `constellation/identity.go` or `cog/.cog/node_identity.go` — retirement is gated on the preconditions above

---

## Migration guidance for Layer 2 (future wave)

When Layer 2 retirement is attempted, the migration procedure is:

1. Load existing `.cog/identity.json` (Ed25519 `NodeIdentity`).
2. Check if a `~/.cog/node/identity/node-key.pem` (ECDSA P-256) already exists; if not, generate one.
3. Derive the new NodeID: `hex(sha256(DER(ecdsa_pubkey)))`.
4. Write a `kind: Node` CRD YAML at `.cog/config/nodes/<old_node_hash>.yaml` with:
   - `spec.old_node_hash: <old sha256:...>` — provenance link
   - `spec.new_node_id: <new hex>` — canonical L1 ID
   - `spec.migration_ts: <RFC3339>`
5. Emit a `node.identity.migrated` bus event with both IDs.
6. Preserve `.cog/identity.json` as a read-only artifact; do not delete.

---

## Open questions (not blocking)

1. **When does cogos kernel import `github.com/myrgic/constellation`?** Blocked on constellation's public API surface being stable enough for a go.mod dependency.
2. **Cross-node identity replication.** When a node registers in a multi-node constellation, which node identity propagates — L1 or L3? Answer: L1 (per constellation protocol). L3 is local administrative metadata.

---

## Conflict log (recorded, unresolved)

Added alongside the change that made node identity machine-scoped
(`internal/engine/node_identity.go`). Recorded here rather than resolved,
because resolving it is an operator decision, not an implementation detail.

- **ADR-065 §7 vs. RFC-033.** ADR-065 §7 places daemon runtime state at
  workspace-scoped `.cog/run/daemon/state.yaml` (implemented verbatim in
  `internal/engine/daemon_lifecycle.go`). RFC-033 says node-runtime state is
  machine-local and must not live in the shared workspace `.cog/`. Both are
  live. The node-identity change moved **identity only** and deliberately left
  daemon state exactly where ADR-065 put it.
- **ADR-065 §9 is what makes the bug reachable.** "Recursive Nesting" blesses a
  CogOS kernel running inside a container managed by another CogOS kernel as a
  first-class pattern. Combined with the `-v WorkspaceRoot:WorkspaceRoot` bind
  mount, that is precisely how a child kernel came to read the host's
  workspace-scoped `node_id` and clone its identity. ADR-065 predates RFC-033
  and says nothing about identity separation; that silence was the gap.
- **Which machine tier owns node state — `~/.cog/` or RFC-033's `~/.cogos/`?**
  Unsettled. Identity currently lands in `~/.cog/node/`, adjacent to the tier
  that already works (`~/.cog/etc` certs, `~/.cog/node/global.yaml`). When
  RFC-033 settles it, this is a one-line change to `defaultNodeIdentityDir`,
  and `COG_NODE_DIR` is already the migration seam.
- **This ADR's Layer 3 is partially revived.** `~/.cog/node/identity.yaml`
  remains orphaned — no Go code reads it. The new resolver does not write to it
  and does not depend on it.

Scoping authority for the change itself is RFC-036's 2026-07-29 operator ruling
("node = hardware, workspace = the overlay"), not this ADR.

---

## Addendum (2026-08-07): Open Question #1's stated blocker no longer holds

Open Question #1 asked when the cogos kernel could import
`github.com/myrgic/constellation`, and named "constellation's public API
surface being stable enough for a go.mod dependency" as the blocker. That
framing was accurate on 2026-05-18 and was never re-checked afterward.
Re-measured on 2026-08-07:

- `github.com/myrgic/constellation` is tagged `v0.1.0` (2026-04-14) and
  `v0.2.0` (2026-05-08) — ten days before this ADR's own creation date.
- `identity.go` is byte-identical between `v0.2.0` and the current `main`
  tip (same git blob SHA, `8b6d86f`).
- Three commits have landed on constellation's `main` since `v0.2.0`:
  `acc3456` (2026-05-27, `docs/PAPER.md`), `2d08506` (2026-08-03,
  `CHANGELOG.md` and `README.md`), and `c47074f` (2026-08-03,
  `CONTRIBUTING.md`). All three are docs-only; none touches a `.go` file.
- `go get github.com/myrgic/constellation@v0.2.0` resolves cleanly through
  the standard module proxy, and a scratch module built and compiled
  cleanly against its exported identity surface (`NodeIdentity`,
  `GenerateIdentity`, `LoadIdentity`, `SaveIdentity`).

The public API surface has been frozen since `v0.2.0`, predating this ADR
by ten days. The *obstacle* Open Question #1 named — API instability — was
therefore already gone on the day this ADR was created; the "blocked on
API stability" label persisted only because nothing re-checked it until
now.

This does **not** clear precondition #1 itself. Precondition #1, verbatim
above, is an act performed by cogos ("the cogos kernel imports
`github.com/myrgic/constellation`"), not a fact about constellation's API.
Checked on `main` on 2026-08-07: `grep constellation go.mod` finds nothing,
and no `.go` file in the kernel imports `myrgic/constellation`. The import
does not exist on `main` — a prototype exists only on the closed, unmerged
branch `feat/adr099-l2-identity-migration` (see below), which is not part
of this change. So the honest count is **zero of ADR-099's three gating
preconditions cleared** on `main` today: precondition #1 (the import)
remains as unmet as preconditions #2 (`IdentityProvider` L2→L1 projection)
and #3 (`cogos node migrate-identity` CLI command). What has changed is
that the API-stability obstacle to doing #1 is gone, so #1 is now
unblocked rather than blocked — a necessary step before it can be done, not
evidence that it has been done. No existing `.cog/identity.json` should be
touched until all three preconditions are met and an operator decides to
run the migration.

A prototype implementation of the six-step procedure from "Migration
guidance for Layer 2 (future wave)" above (`internal/l2migration`, plus
the corresponding `go.mod` dependency addition) exists on the closed
branch `feat/adr099-l2-identity-migration` (PR #538, closed without
merging on 2026-08-07). It is unmerged and is not part of this change; a
future attempt at the second and third preconditions does not need to
start from zero.

---

## References

- Three-layer identity model (L1/L2/L3) — (internal reference omitted)
- Wave 6b implementation-state audit that surfaced the gap — (internal reference omitted)
- `constellation/identity.go` — L1 canonical implementation
- `cog/.cog/node_identity.go` — L2 workspace operational implementation
- ADR-062 — Recursive Node Architecture (L3 node registration)
- ADR-073 — Control plane architecture

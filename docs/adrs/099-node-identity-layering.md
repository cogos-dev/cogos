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

## References

- Three-layer identity model (L1/L2/L3) — (internal reference omitted)
- Wave 6b implementation-state audit that surfaced the gap — (internal reference omitted)
- `constellation/identity.go` — L1 canonical implementation
- `cog/.cog/node_identity.go` — L2 workspace operational implementation
- ADR-062 — Recursive Node Architecture (L3 node registration)
- ADR-073 — Control plane architecture

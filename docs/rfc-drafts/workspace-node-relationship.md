# RFC Draft: workspace ⟷ node relationship (N:M)

**Status:** Draft — awaiting review and RFC number assignment
**Closes:** myrgic/cogos#160
**Companion:** myrgic/cogos#161 (mechanical on-disk demonstration)
**Scope:** Design only — no implementation in this document

---

## 1. Summary

A CogOS workspace is the outer scope: a named, rooted collection of cogdocs,
memory, configuration, and agent state anchored at a directory containing
`.cog/`. A node is a physical machine (or container) that hosts one or more
workspaces; it is a **member** of the workspaces it hosts. The relationship is
N:M: a workspace can be hosted by many nodes (federation), and a node can host
many workspaces. The home directory's `~/.cog/` is itself a valid workspace —
the user's home workspace — and `~/.cog/node/` holds that machine's node-local
state, co-located by convention, not by constraint. This document names that
relationship explicitly for the first time and specifies the on-disk convention
that makes it concrete.

---

## 2. Motivation

Three consequences follow from naming this relationship:

**Federation foundation.** Workspace-spanning multi-node deployment (ADR-063,
`node_config.go:3-4`) already encodes a `WorkspaceID` per node and a `peers`
list per sync profile (`node_config.go:26-27`, `node_config.go:75`). The
relationship is present in code but not named in any design document. Without
a name, the model is tribal knowledge. With a name, federation is "workspaces
with peer nodes listed in config" — a sentence a contributor can act on.

**Home-as-workspace.** Sessions started from `~` silently no-op in the
workspace-awareness hook (`~/.claude/hooks/cogos_session_awareness.py`) because
`findWorkspaceRoot` (`decompose_store.go:293-306`) requires a `.cog/` directory,
but `~/.cog/config` is a flat file that prevents `~/.cog/config/` from
functioning as a workspace config directory. Naming the node/workspace
separation makes the fix obvious: the flat registry belongs under node state
(`~/.cog/node/global.yaml`), freeing `~/.cog/config/` to become the home
workspace's config directory. Issue #161 is the mechanical execution of that
fix.

**Constellation composition.** The constellation model (identity-protocol
architecture at `docs/architecture/the-constellation.md`) frames node-level
identity as L1 (the constellation repo, the git-hash-chained ledger) and
workspace identity as L2 (a CRD reconciled per workspace). The workspace ⟷ node
relationship names the boundary those two levels operate across. Without the
boundary named, the L1/L2 split reads as an internal implementation detail
rather than a structural boundary with operational consequences.

---

## 3. Status Quo

### 3.1 The home directory's dual role today

`~/.cog/` currently holds two structurally different kinds of state, mingled at
the top level:

| Path | Kind | Used by |
|------|------|---------|
| `~/.cog/node/` | Node-local (identity, shells, secrets) | `cmd_node.go:92-96`, `LoadNodeIdentity()` |
| `~/.cog/kernels/` | Node-local (kernel binaries) | node lifecycle |
| `~/.cog/credentials/` | Node-local (auth material) | secret resolution |
| `~/.cog/etc/` | Node-local (inference config) | `harness/config.go:36-42` |
| `~/.cog/config` | **File** — global workspace registry | `cog.go:884-891` |
| `~/.cog/mem/` | Workspace-shaped (cogdocs, memory) | `memory.go:121` |
| `~/.cog/hooks/` | Workspace-shaped | `memory.go:125` |
| `~/.cog/templates/` | Workspace-shaped | workspace agent defaults |
| `~/.cog/objects/` | Workspace-shaped (content store) | object addressing |

The flat file at `~/.cog/config` (`globalConfigPath()`, `cog.go:884-891`) is
the global workspace registry: it lists every workspace this node knows about.
The file occupies the path that `findWorkspaceRoot` would treat as a directory
if `~/.cog/` were a valid workspace. These two roles collide: the registry file
blocks home-as-workspace.

### 3.2 What the kernel already knows

Three-tier config resolution (`harness/config.go:1-8`) explicitly distinguishes
node scope (`~/.cog/etc/inference.yaml`) from workspace scope
(`$workspace/.cog/conf/inference.yaml`). The comment on line 5 reads: "node
(shared across all workspaces)." The separation is encoded but unnamed at the
model level.

`NodeConfig` (`node_config.go:24-37`) carries both a `NodeID` and a
`WorkspaceID` — a node knows which workspace it is currently serving. The
`SyncConfig.Peers` field (`node_config.go:75`) lists peer addresses for that
workspace's sync topology. A workspace with two peer nodes is already
representable; the schema simply never said so in prose.

Multi-node commands (`cmd_node.go:2`, `cmd_node.go:1332`) use
`.cog/config/node/node.json` within the workspace — a workspace-scoped location
for per-node configuration. This is the workspace-as-outer-scope pattern in
practice today.

### 3.3 Workspace detection today

`findWorkspaceRoot(dir string)` (`decompose_store.go:293-306`) walks parent
directories looking for a `.cog/` subdirectory. It returns the first match or
empty string. This makes any directory with a `.cog/` child a valid workspace
root. `~` already has `.cog/` — the collision is the `config` flat file inside
it, not a deficiency in detection.

---

## 4. Proposal

### 4.1 The N:M model

Define the terms:

- **Workspace** — a rooted directory tree (`<root>/.cog/`) that holds
  workspace-scoped cogdocs, memory, hooks, templates, agent state, and
  workspace-level configuration. A workspace is identified by its root path
  within a node, and by a `workspace_id` across nodes. A workspace can be
  hosted by multiple nodes simultaneously (federated workspace).

- **Node** — a physical or virtual machine that hosts one or more workspaces.
  A node is identified by a `node_id` derived from its Ed25519 public key
  (`node_config.go:196-226`). A node's own state — identity, credentials,
  kernel binaries, inference config — lives under `<workspace>/.cog/node/`
  within each hosted workspace, or under `~/.cog/node/` for node-level
  state not tied to a specific project workspace.

- **Membership** — a node is a member of every workspace it hosts. Membership
  is recorded in the workspace's node config (`<root>/.cog/config/node/node.json`)
  and in the node's global registry (`~/.cog/node/global.yaml` after #161).

The relationship is N:M:

```
Workspace A ─── Node 1
                Node 2   ← federation: two nodes hosting Workspace A
Workspace B ─── Node 1
Workspace C ─── Node 1
                Node 3
```

### 4.2 Required vs. conventional

**Required** (the relationship is undefined without these):

- Every workspace has a `workspace_id`. For project workspaces initialized with
  `cog node init`, this is generated at init time. For the home workspace, it
  is generated on first use.
- Every node has a `node_id` (already enforced via `GenerateNodeID`,
  `node_config.go:198-226`).
- The node's global registry lists every workspace this node hosts. After #161,
  this registry lives at `~/.cog/node/global.yaml`.

**Conventional** (encouraged but not enforced):

- Node-local state lives under `~/.cog/node/` (identity, credentials, shells,
  global registry).
- Per-workspace node config lives at `<root>/.cog/config/node/node.json`
  (already the case for project workspaces, per `cmd_node.go:9`).
- Inference config at `~/.cog/etc/` is node-scoped; workspace inference config
  at `<root>/.cog/conf/inference.yaml` overrides it
  (`harness/config.go:36-60`).

**Not constrained** (explicitly not required by this model):

- Co-location of node state with the home workspace. `~/.cog/node/` is
  conventionally inside the home workspace's `.cog/`, but a node could in
  principle store its identity anywhere accessible.
- Peer-node discovery topology. How nodes find each other (mDNS, static list,
  future federation protocol) is out of scope.

---

## 5. On-Disk Layout

### 5.1 Home workspace (after #161)

```
~/.cog/
├── node/                         ← node-local state for this machine
│   ├── identity.yaml             ← node_id, public key fingerprint
│   ├── shells.yaml               ← shell registrations
│   ├── secrets/                  ← credential material
│   └── global.yaml               ← workspace registry (#161 relocates here)
├── etc/
│   └── inference.yaml            ← node-level inference config
├── kernels/                      ← kernel binaries (node-local)
├── config/                       ← home workspace config (freed by #161)
│   └── node/
│       └── node.json             ← home workspace's NodeConfig
├── mem/                          ← home workspace memory (cogdocs)
├── hooks/                        ← home workspace hooks
├── templates/                    ← home workspace agent templates
└── objects/                      ← home workspace content store
```

The structural rule: paths under `.cog/node/` are node-scoped (one machine).
All other `.cog/` paths are workspace-scoped (follow the workspace across
machines). `.cog/config/node/node.json` is deliberately workspace-scoped:
it describes *this node's membership* in *this workspace*, so it belongs
in the workspace, not in the node-only subtree.

### 5.2 Project workspace

```
~/workspaces/myrgic/cogos/
└── .cog/
    ├── config/
    │   └── node/
    │       └── node.json         ← NodeConfig for this workspace on this node
    ├── conf/
    │   └── inference.yaml        ← workspace-level inference overrides
    ├── mem/                      ← project memory
    ├── ledger/                   ← ledger entries
    └── ...
```

No `node/` subtree here: this workspace's node-local state is the `node.json`
inside `config/node/`. The machine's identity lives at `~/.cog/node/`, not
duplicated per project.

### 5.3 Federated workspace sketch

Two nodes hosting the same workspace (conceptual; sync transport is out of
scope):

```
Node 1 (~/.cog/node/global.yaml):
  workspaces:
    - path: /Users/alice
      workspace_id: ws-abc123
    - path: /Users/alice/workspaces/myrgic
      workspace_id: ws-def456

Node 2 (~/.cog/node/global.yaml):
  workspaces:
    - path: /home/ci
      workspace_id: ws-abc123     ← same workspace_id, different local path
```

`NodeConfig.WorkspaceID` (`node_config.go:27`) is the join key. Peers are
listed per workspace under `NodeConfig.Sync.Peers` (`node_config.go:75`).

---

## 6. Composition with Existing Primitives

### 6.1 ADR-063 multi-node deployment

ADR-063 (`node_config.go:3-4`, `cmd_node.go:2`) already works within this
model. `NodeConfig.WorkspaceID` (`node_config.go:27`) is the workspace
identifier across nodes. `SyncConfig.Peers` (`node_config.go:75`) are the
other nodes hosting the same workspace. This RFC names what ADR-063 implicitly
assumes.

### 6.2 Reconcilable model

The Reconcilable interface (`pkg/reconcile/`) controls configuration CRDs. A
future `WorkspaceMembership` Reconcilable would reconcile which nodes are
members of a given workspace — driving sync topology, peer-list updates, and
membership tombstones. This RFC establishes the conceptual basis for that CRD;
the CRD itself is out of scope.

### 6.3 Identity CRD

The constellation model uses a three-layer identity framing:
L1 (Node — the constellation repo, git-hash-chained),
L2 (Identity — a CRD reconciled per workspace),
L3 (Presence — emergent from attention + bus events, not a stored object).

This RFC maps cleanly: the workspace is the L2 identity scope; the node is the
L1 anchor. A node can carry multiple L2 identities (one per workspace it
hosts). Presence is still derived, not stored, and is unaffected by this model.

### 6.4 Workspace detection

`findWorkspaceRoot` (`decompose_store.go:293-306`) requires no changes. The
home workspace becomes detectable once #161 relocates the `~/.cog/config` flat
file, freeing `.cog/config/` to be a directory.

### 6.5 Three-tier config resolution

`harness/config.go:1-60` already names "node" and "workspace" as separate
tiers. The resolution order (node → workspace → env → defaults) is unchanged.
This RFC gives those tier names a structural definition.

---

## 7. Companion Mechanical Fix

Issue #161 is the concrete on-disk demonstration of this model. It relocates
the flat global workspace registry from `~/.cog/config` (a file, `cog.go:884-891`)
to `~/.cog/node/global.yaml`.

This move is correct under the model: the registry lists which workspaces this
**node** knows about — it is node-local state, not workspace state. Relocating
it under `~/.cog/node/` makes that semantics explicit on disk, and frees
`~/.cog/config/` to function as the home workspace's config directory.

The migration path in #161 is one-time and idempotent: on first run after the
change, if `~/.cog/config` exists as a file and `~/.cog/node/global.yaml` does
not, move it. The session-awareness hook that walks up for `.cog/config/`
(currently no-ops at `~` because the path is a file) begins working correctly
without any hook changes.

---

## 8. Open Questions

**Q1: Workspace identity across nodes.**

`NodeConfig.WorkspaceID` (`node_config.go:27`) is generated at `cog node init`
time. There is currently no mechanism for two nodes to assert they share a
workspace identity without out-of-band coordination (copying or committing the
`node.json`). A workspace-identity bootstrap protocol — how does Node 2 learn
that `/home/ci` is `ws-abc123`? — is not defined here. Federation discovery is
the natural home for this answer; it is out of scope.

**Q2: Peer-node listing format.**

`SyncConfig.Peers` (`node_config.go:75`) is a `[]string` of static addresses.
There is no schema for a richer peer-node descriptor that would carry
`node_id`, workspace membership, or trust-chain information. A future
`PeerNode` struct would compose with the constellation's trust model. Deferred.

**Q3: Node identity overlap with workspace identity.**

The home workspace currently has no `workspace_id` (there is no `node.json`
at `~/.cog/config/node/`). After #161, the workspace config directory is
accessible, but generating a `workspace_id` for the home workspace requires
either a migration step or a lazy-init on first workspace operation. The right
trigger is not specified here.

**Q4: Global registry as source of truth vs. workspace config as source of truth.**

After #161, `~/.cog/node/global.yaml` lists workspaces by path. The
per-workspace `node.json` carries `workspace_id`. If these diverge (e.g., a
workspace is moved), which wins? The reconciliation logic is not specified here.

---

## 9. Out of Scope

- **Federation discovery.** How nodes find each other across the network
  (mDNS, static peers, future peer exchange) is a separate subsystem. This
  RFC names the N:M relationship; it does not define the discovery protocol.
  The seam is acknowledged at `SyncConfig.Discovery` (`node_config.go:74`).

- **Sync transport.** Block-level sync (Syncthing BEP or equivalent) is the
  mechanism by which federated nodes replicate workspace state. Out of scope.

- **WorkspaceMembership Reconcilable.** A CRD that tracks and reconciles which
  nodes are members of a workspace is the natural next primitive. Not defined
  here.

- **Constellation trust chain across nodes.** How the constellation's
  hash-chained coherence ledger is verified across nodes with different signing
  keys is a trust-model question, not a naming question.

- **Kernel registry data flow.** Issues #154, #157, and #159 address the
  session registry and bus subscription mechanics. Orthogonal to this RFC.

---

## Appendix: Key File References

| File | Lines | What |
|------|-------|------|
| `harness/config.go` | 1-8 | Three-tier config resolution; "node" and "workspace" tier names |
| `harness/config.go` | 33-60 | `LoadInferenceConfig` — merges node then workspace layer |
| `node_config.go` | 3-4 | ADR-063 citation for multi-node deployment |
| `node_config.go` | 24-37 | `NodeConfig` struct — `NodeID`, `WorkspaceID` |
| `node_config.go` | 69-76 | `SyncConfig` — `Discovery`, `Peers` |
| `cmd_node.go` | 1-9 | Multi-node commands; workspace-scoped node.json path |
| `cmd_node.go` | 92-96 | `nodeDir()` — returns `~/.cog/node` |
| `cmd_node.go` | 99-111 | `LoadNodeIdentity()` — reads from `~/.cog/node/identity.yaml` |
| `cog.go` | 884-891 | `globalConfigPath()` — returns `~/.cog/config` (the collision) |
| `cog.go` | 893-927 | `loadGlobalConfig()` / `saveGlobalConfig()` — the registry |
| `decompose_store.go` | 293-306 | `findWorkspaceRoot()` — `.cog/` directory detection |
| `memory.go` | 120-148 | Workspace-rooted URI path mappings |

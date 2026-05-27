# RFC-0009: Consolidated MCP Transport on Kernel Daemon

| Field    | Value                                                                                                  |
|----------|--------------------------------------------------------------------------------------------------------|
| Status   | Accepted                                                                                               |
| Author   | @chazmaniandinkle                                                                                      |
| Created  | 2026-05-18                                                                                             |
| Accepted | 2026-05-27 — direction accepted by operator; open questions (§6) resolve during implementation         |
| Tracking | TBD                                                                                                    |
| Relates  | [ADR-091](../adrs/091-substrate-as-named-architectural-layer.md), [ADR-092](../adrs/092-substrate-contracts-and-concurrency.md), [ADR-095](../adrs/095-daemon-reconcile-loop-driver.md), [ADR-096](../adrs/096-worktree-reconciler.md), [ADR-097](../adrs/097-memory-projection-reconciler.md), [ADR-098](../adrs/098-skill-projection-reconciler.md), [RFC-025 (cog workspace — cogdoc substrate unity)](https://github.com/myrgic/cogos) |

> **Accepted direction (2026-05-27).** The operator has accepted consolidating the MCP transport into the kernel daemon as the canonical direction. The open questions in §6 are resolved during implementation; implementation PRs reference this RFC. ADR promotion follows once consolidation lands.

---

## Summary

The CogOS kernel daemon (`cogos serve`) and the MCP subprocess (spawned per-client from
`/Users/slowbro/go/bin/cogos mcp serve`) are today two separate processes on the same node.
This RFC proposes making the daemon the single node-root process: MCP becomes an HTTP/SSE
transport on the daemon's existing port (`:6931`), not a spawned subprocess. Both the CLI
wrapper (`./scripts/cog`) and the MCP surface become *projections* of the same kernel
operation surface. The stdio subcommand (`cogos mcp serve`) is retained as a documented
fallback for non-daemon contexts.

---

## 1. Motivation

### 1a. The v0.9.0 binary-skew incident

On 2026-05-16, the v0.9.0 release updated `~/.cog/bin/cogos` (the daemon binary, managed by
the node manifest) but did not update `/Users/slowbro/go/bin/cogos` (the path `.mcp.json`
spawns from). The result: the kernel daemon ran v0.9.0 with the `peer.utterance` event type
registered, but the MCP subprocess Claude Code spawned was still `dev/unknown` without it.
Two binaries, two update paths, silent version skew. Every tool call that relied on the new
event type failed from the MCP surface while working from the HTTP surface.

This is not a release-process failure. It is a structural failure: the binary-split model
has two install paths by design, so any release that does not update both paths produces
skew. Discipline cannot reliably prevent this; the two-process model guarantees it eventually
recurs.

### 1b. The CFT-visibility crisis: tool-surface projection gap

On 2026-05-18, the user-level Claude Code seat was discovered to have zero visibility into
the Cognitive Field Theory (CFT) corpus — roughly 25+ semantic-sector cogdocs developed in
the cog workspace since 2026-03-06 — despite the daemon being healthy and the CFT material
being the load-bearing research program of the workspace.

Root-cause analysis identified a structural projection gap between the CLI surface and the
MCP surface:

| CLI (workspace-local) | MCP (any seat) |
|---|---|
| `cog memory search` | `cog_search_memory` |
| `cog memory read` | `cog_read_cogdoc` |
| `cog memory write` | `cog_write_cogdoc` |
| `cog memory toc` | `cog_memory_toc` |
| `cog memory index` | `cog_memory_index` |
| `cog memory list` | *(no equivalent)* |
| — | `cog_patch_frontmatter` |
| — | `cog_query_field` |
| — | `cog_grep_files` |

Naming convention is inconsistent: some tools use `cog_memory_*`, others use
`cog_<verb>_cogdoc`. `cog memory list` has no MCP equivalent. Discoverability via
ToolSearch is degraded because the tools lack semantic grouping. When the MCP connection
dropped during the v0.9.0 incident, even partial coverage became zero.

The CFT corpus was invisible to the user-level agent not because the agent failed to look,
but because the tool surface it had access to was structurally incomplete relative to the CLI
surface.

### 1c. Identity attribution and the `from_session` workaround

PR #270 added an optional `from_session` parameter to `cog_emit_event` to work around the
MCP subprocess having no sender context: the kernel would mint a random session ID for any
event emitted via MCP because the subprocess carried no identity binding. The workaround is
a symptom of the structural problem. Identity should come from the connection context (the
daemon's live session registry), not be tacked on by the caller.

### 1d. Felt architectural confusion

From a 2026-05-16 operator reflection, after the architectural argument was stated: *"I
think honestly this has probably been behind the confusion I've been feeling about overall
node kernel runtime's identity. There has always felt like there's some disconnect there
that shouldn't be. The MCP configs have all gotten really messy, and part of that is because
we're not properly consolidating things."*

The MCP config sprawl (`.mcp.json` mixing subprocess-spawned and HTTP-endpoint entries,
per-client session isolation, multiple version paths) is a visible artifact of a wrong
architectural shape. Configuration is trying to paper over a structural fragmentation.

---

## 2. Current architecture (problem shape)

```
┌─────────────────────────────────────────────────┐
│  Node                                            │
│                                                  │
│  ┌──────────────────────┐                        │
│  │  cogos daemon        │  ~/.cog/bin/cogos      │
│  │  HTTP :6931          │  managed by launchd    │
│  │  - session registry  │  v0.9.0                │
│  │  - ledger            │                        │
│  │  - reconcile loop    │                        │
│  │  - identity layer    │                        │
│  │  /mcp (SSE) ← ALREADY IMPLEMENTED             │
│  └──────────────────────┘                        │
│                                                  │
│  ┌────────────────────────┐  × N MCP clients     │
│  │  MCP subprocess        │  ~/go/bin/cogos       │
│  │  stdio transport       │  NOT managed by daemon│
│  │  spawned per-client    │  may be stale version │
│  │  by .mcp.json          │                      │
│  └────────────────────────┘                      │
│                                                  │
└─────────────────────────────────────────────────┘
```

**Failure modes:**
1. Version skew — releases update one path, not the other (witnessed at v0.9.0)
2. Multi-master risk — each MCP subprocess could develop local caching or identity drift
3. Broken identity attribution — per-client subprocess has no first-class identity binding
4. Resource cost — N cogos processes per node (one per MCP client)
5. Lifecycle complexity — daemon restart does not refresh MCP for existing clients
6. Violates "one node, one substrate presence" — N processes claiming one identity

---

## 3. Proposed design

### 3a. Single-process node-root

The kernel daemon is the **one** node-root presence. MCP is a transport on that daemon,
not a separate process. `.mcp.json` points at the existing HTTP/SSE endpoint on `:6931/mcp`
rather than spawning a subprocess.

This is already architecturally prepared: `internal/engine/mcp_server.go` mounts the MCP
handler at `/mcp`; `docs/MCP-SPEC.md` documents `http://localhost:6931/mcp` as the primary
transport. The gap is `.mcp.json` migration and `.mcp.json` client-config documentation.

The consolidated shape:

```
┌─────────────────────────────────────────────────┐
│  Node                                            │
│                                                  │
│  ┌──────────────────────────────────────────┐    │
│  │  cogos daemon                            │    │
│  │  ~/.cog/bin/cogos — one binary, one ver  │    │
│  │                                          │    │
│  │  HTTP :6931           (kernel ops)       │    │
│  │  /mcp  (HTTP/SSE)     (MCP transport)    │    │
│  │                                          │    │
│  │  - session registry  (all transports)   │    │
│  │  - ledger            (all transports)   │    │
│  │  - reconcile loop    (daemon-resident)  │    │
│  │  - identity layer    (connection-bound) │    │
│  └──────────────────────────────────────────┘    │
│                                                  │
│  .mcp.json → { "url": "http://localhost:6931/mcp" }
│                                                  │
└─────────────────────────────────────────────────┘
```

### 3b. Transports as projections

CLI wrapper (`./scripts/cog`) and MCP surface are both *projections* of the kernel's full
operation surface. The invariant: adding an operation to the kernel means exposing it on
both surfaces in the same commit.

A "projection" here is the same sense as the existing Reconcilable projection primitives
(ADR-094, ADR-097, ADR-098): the kernel is the authoritative source; CLI and MCP are derived
views. The difference is that CLI and MCP projections are synchronous (a CLI call or MCP
tool call directly invokes the kernel handler), while Reconcilable projections are
asynchronous (the Reconcilable brings a filesystem surface into sync over time).

### 3c. Tool-surface isomorphism

An explicit feature parity requirement applies across transports. The following naming
convention is proposed for the `cog memory` family:

| Kernel operation | CLI surface | MCP surface |
|---|---|---|
| Search memory | `cog memory search <query>` | `cog_memory_search` |
| Read a cogdoc | `cog memory read <path>` | `cog_memory_read` |
| Write a cogdoc | `cog memory write <path> <title>` | `cog_memory_write` |
| Table of contents | `cog memory toc <path>` | `cog_memory_toc` |
| Index cogdocs | `cog memory index [<path>]` | `cog_memory_index` |
| List cogdocs | `cog memory list [<sector>]` | `cog_memory_list` ← add |

Naming convention: `cog_memory_<verb>` for the memory family. The currently non-canonical
names (`cog_read_cogdoc`, `cog_write_cogdoc`, `cog_search_memory`) are candidates for
aliasing or renaming; this RFC does not prescribe which approach, as implementation will
surface the right tradeoffs (backward-compat aliases vs. clean rename + migration note).

The same parity principle applies to all kernel operation families, not just memory. Any
operation accessible via CLI should be accessible via MCP with a predictable name derivable
from the CLI verb structure.

### 3d. Identity from connection context

Per the `identity-role-orthogonal-axes` and `dispatch-transport-by-default` substrate
memos, MCP client identity should derive from the connection context — the daemon's live
session registry knows which session is active for which MCP connection — not from a
parameter the caller must supply.

The `from_session` parameter added in PR #270 becomes **structural** under consolidation:
the daemon binds each MCP SSE connection to the calling session on connection establishment,
making identity attribution automatic. The `from_session` workaround was necessary because
the subprocess had no session registry access; the daemon does.

This applies the same per-connection-state model already used by the WebSocket/SSE bus
broker: each connected client has a session binding maintained by the daemon for the
lifetime of the connection.

Implementation note: the `from_session` parameter should remain available as an explicit
override for cases where the inferred identity is wrong or for automated tooling that
needs to act under a specific session. It becomes an override, not the primary mechanism.

### 3e. Fallback path

`cogos mcp serve` stdio subcommand (`internal/engine/cli_mcp.go`) is retained as a
documented fallback. Use cases:

- CI pipelines that run without a live daemon (smoke tests, pre-release validation)
- Headless dev containers where launchd is not available
- MCP host software that does not support HTTP transport and requires stdio

The fallback is not deprecated. It is documented as "non-default for normal node usage."
Normal node usage means `cogos serve` is running; `.mcp.json` points at HTTP.

### 3f. Skills projection (deferred to sibling RFC)

The same projection-from-canonical-source pattern applies at the skills layer: substrate
skills at `.cog/skills/` should project to `~/.claude/skills/` (and vice versa), just as
memory cogdocs project bidirectionally via ADR-097. The symptom: `~/.claude/skills/src-theory/` is
empty despite the workspace having `src-theory` content.

This RFC names the gap and defers the resolution to a sibling RFC (or to ADR-098's
`SkillProjectionReconciler` follow-up work if it naturally absorbs the `cog://skills/`
→ `~/.claude/skills/` direction). The present RFC scopes to the transport and tool-surface
layers; skills projection is a different Reconcilable.

---

## 4. Composition with existing ADRs

This RFC does not restate the following ADR decisions; it builds on them:

| ADR | Relevant invariant |
|---|---|
| ADR-073 (Node Control Plane) | The daemon is the one node presence; HTTP is the primary interface. This RFC makes MCP a transport on that interface, not a peer process. |
| ADR-091 (Substrate as Named Layer) | Substrate / Kernel / Module trichotomy. MCP as a transport on the daemon is Module-layer (a projection modality), not a Kernel-layer concern. |
| ADR-092 (Substrate Contracts) | Ledger-first rule. Any event emitted via MCP must append to the same ledger as events emitted via HTTP. No separate ledger per transport. |
| ADR-095 (Daemon Reconcile Loop Driver) | ReconcileDaemon drives registered Reconcilables. MCP transport registration does not add a new Reconcilable; it is a static mount. |
| ADR-096 (WorktreeReconciler) | Substrate resource lifecycle as Reconcilable. Not directly related, but the general pattern of "one ledger, one source of truth" motivates both. |

Also composes with the sibling projection Reconcilables (ADR-097 MemoryProjectionReconciler,
ADR-098 SkillProjectionReconciler): both address surface-projection gaps; this RFC addresses
the transport-projection gap. The three gaps are the same architectural shape applied at
different layers.

Also composes with **RFC-025 (Cogdoc Substrate Unity — Unified cog memory Tooling)** in the
cog workspace corpus. RFC-025 proposes a unified `cog memory` CLI surface (no per-type
command proliferation); §3c of this RFC proposes that the MCP surface is isomorphic to that
CLI surface. The two proposals do not conflict: RFC-025 works on the CLI layer; this RFC adds
the invariant that the MCP layer must match it. If RFC-025 lands, the naming convention it
ratifies should be the naming convention used on the MCP surface.

---

## 5. Alternatives considered

**A. Keep the subprocess, fix version management.** Implement a wrapper that checks the
subprocess binary version on each spawn, auto-updates if stale. Rejected: the discipline
fix is fragile (requires the wrapper to know the canonical version source, run before every
spawn, handle update failures). The root problem is structural multiplicity; the fix is
structural unity.

**B. Replace stdio with a dedicated HTTP sidecar.** Run a separate `cogos-mcp` process
that speaks HTTP/SSE and proxies to the daemon. Rejected: adds a third process and a proxy
boundary that could drift. The daemon already has the HTTP stack; MCP is a handler, not a
new process.

**C. Keep stdio as the primary transport; expose HTTP as the optional path.** Current MCP
spec design predates the daemon-as-one-process recognition. The MCP-SPEC.md already
designates HTTP/SSE as primary. This option would reverse that documented intent without a
reason to do so. Rejected.

**D. Keep separate processes per the existing design; add process-coupling to prevent
version skew.** E.g., have the daemon spawn the MCP subprocess and manage its lifecycle,
ensuring same-binary version parity. Rejected: process coupling at the parent-child level
means a crash in MCP handling still isolates from the daemon, but adds substantial lifecycle
complexity (spawn management, PID tracking, crash recovery across the boundary). ADR-092 §1
single-writer-per-session would require cross-process coordination. The blast-radius benefit
does not outweigh the complexity cost.

---

## 6. Open questions

These are explicitly unresolved and will be addressed by implementation:

1. **Port vs. sub-path.** Is `/mcp` the right mount path on `:6931`, or should MCP use a
   sibling port (e.g., `:6932`) to allow per-transport firewall rules? The MCP-SPEC.md
   documents `/mcp` on `:6931`; this RFC adopts that without requiring it. Implementation
   will validate whether port-cohabitation causes practical issues (CORS headers, TLS scope,
   client discovery).

2. **Connection-context identity binding.** The mechanism for binding an MCP SSE connection
   to a session in the session registry is not specified here. Implementation must decide:
   does the client pass a session token in a header on connection, or does the daemon derive
   the session from connection metadata? The `from_session` override parameter stays
   regardless; the question is the default binding mechanism.

3. **Backward-compat for MCP tool renaming.** The naming-convention proposal (§3c) requires
   renaming `cog_read_cogdoc`, `cog_write_cogdoc`, and `cog_search_memory`. Whether these
   become aliases (old names stay, new names added) or a versioned rename with a migration
   window is an implementation decision. Both approaches have different implications for
   existing `.mcp.json` configurations and for MCP clients that have cached tool schemas.

4. **Tool grouping for discoverability.** MCP tool discovery via ToolSearch currently
   surfaces `cog_memory_*` and `cog_<verb>_cogdoc` without semantic grouping. Whether
   semantic grouping is an MCP-server metadata feature, a ToolSearch-side convention, or
   a documentation-only fix is open.

5. **Crash blast radius.** Consolidating MCP into the daemon widens the crash blast radius:
   a panic in MCP handler code that escapes the panic-recovery boundary would bring down the
   daemon. The standard Go mitigation (per-handler `recover()`) is already in use in the
   engine. Whether additional isolation (e.g., per-connection goroutine recovery with
   connection reset) is warranted is an implementation judgment.

6. **Headless deployment compatibility.** Some MCP hosts (notably some versions of Claude
   Desktop, older Cursor builds) do not speak HTTP transport and require stdio. The fallback
   subcommand covers this. Whether the fallback needs a version-parity check (to ensure
   the stdio binary matches the running daemon version when both are present) is open.

---

## 7. Implementation tracks

Three separable tracks. Each should be a focused PR referencing this RFC.

**Track A: .mcp.json migration and documentation.** Migrate the `cogos` entry in `.mcp.json`
from subprocess-spawn to HTTP endpoint. Update `docs/MCP-SPEC.md` client configuration
section. Update operator documentation for node setup. Low risk; entirely client-config
and documentation. Can ship independently of B and C.

**Track B: Tool-surface parity audit and `cog_memory_list` addition.** Audit the full CLI
surface vs. MCP surface. Add `cog_memory_list`. Resolve the naming convention for the
memory family (aliases or rename). Add any other missing operations discovered in the audit.
Medium risk; additive for new operations, potentially breaking for renames.

**Track C: Connection-context identity binding.** Implement per-MCP-SSE-connection session
binding in the daemon. Wire to the session registry. Make `from_session` an override rather
than the primary mechanism. Depends on the identity-binding mechanism decided in Open
Question 2. Higher complexity; touches session registry and MCP connection lifecycle.

Track A can ship first. B and C are independent of each other after A.

---

## 8. Provenance

- Primary motivation: `feedback_consolidate_mcp_into_kernel_daemon.md` (2026-05-16) — v0.9.0 incident, architectural memo.
- Secondary motivation: `feedback_cog_memory_tooling_mcp_projection_gap.md` (2026-05-18) — CFT-visibility crisis, tool-surface gap.
- Prior art in this repo: `docs/archival/2026-04-21-mcp-always-on.md` — removal of `mcpserver` build tag; MCP made always-on in the daemon. This RFC continues that direction.
- Prior art in this repo: `docs/MCP-SPEC.md` — already designates HTTP/SSE on `:6931/mcp` as the primary transport. This RFC makes that designation operational.
- Identity and transport design: `feedback_dispatch_transport_by_default.md`, `feedback_identity_role_orthogonal_axes.md`.
- Substrate metaphysics: `project_kernel_as_observatory_constitutive.md`, `project_identity_is_the_distinction.md`, `project_reconciliation_is_the_process.md`.

---

## Discussion log

### 2026-05-18: Initial draft

- RFC authored by Cog (RFC author seat). This is an in-progress design document.
- The v0.9.0 binary-skew incident and the CFT-visibility crisis are the two concrete
  motivating failures; the RFC should be evaluated against both.
- No decisions in this RFC are promoted to ADR-level. Implementation PRs will reference
  this RFC. ADR promotion happens after consolidation and implementation-driven refinement.
- Three implementation tracks identified (A: .mcp.json migration, B: tool-surface parity,
  C: identity binding). Track A is the lowest-risk starting point.

---

## Decision gates

For this RFC to move to `accepted`:

1. At least Track A (`.mcp.json` migration) has shipped and validated in production.
2. Open Questions 1–3 (port/sub-path, identity binding, tool naming) have concrete answers
   from implementation experience, not just design preference.
3. A companion ADR exists that distills the ratified invariants (single-process node-root,
   transport-as-projection, tool-surface isomorphism contract). The ADR will cite this RFC
   with `rel: decided-by`.

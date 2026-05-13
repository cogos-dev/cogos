# RFC-0005: `cog_fork_session` — Substrate-Native Session Forking Primitive

| Field    | Value                                                               |
|----------|---------------------------------------------------------------------|
| Status   | Draft                                                               |
| Author   | @chazmaniandinkle                                                   |
| Tracking | [#202](https://github.com/myrgic/cogos/issues/202)                 |
| Target   | `v0.5.0`                                                            |
| Requires | RFC-0003 (CogBlock topology refinements)                            |

## Summary

`cog_fork_session(parent_session_hash, child_config_overlay) -> child_session_id` is the
minimum primitive beneath several apparently-distinct substrate features: `/btw`-style
asides, multi-CLI conductors, agent spawning, parallel exploration tracks, time-travel
via self-fork, and counterfactual backtracking. All reduce to one operation: pick a
content-addressed session state, declare an overlay, register a child session rooted
at that state.

This RFC specifies the `session.fork` CogBlock Kind, the overlay schema, the MCP tool
and HTTP endpoint, the ledger walk projection for lineage queries, and the garbage
collection policy.

## Background

The substrate already provides the primitives this RFC assembles:

- **Content-addressing**: every session state-at-T is already hashed in the ledger.
- **Hash chaining**: parent→child provenance is structural, not bookkeeping.
- **CogBlock envelope** (ADR-059): `V/Seq/Ts/From/To/Type/Payload/Prev/PrevHash/Hash/Merkle/Sig/Size`
  already covers fork events without schema changes to the envelope itself.
- **Block-mesh architecture** (design seed): identity, role, context, tools, and KV
  cache are the five overlay-able layers.

A fork event is therefore a small addition: a new `session.fork` CogBlock Kind, a body
declaring the parent hash and overlay manifest, and downstream consumers interpreting it.

The SRC reading: the session's state-at-T, observed from its own perspective, hashes to
a canonical reference — `φ(s*) = s*` applied to sessions. A fork is publishing a marker
that says "this point is now reference-able." The substrate needs no separate fork-table
or snapshot store; the ledger is already hash-chained.

## `session.fork` CogBlock Kind

### Kind constant

```go
// In internal/engine/cogblock_kinds.go (or wherever Kind constants live)
const KindSessionFork CogBlockKind = "session.fork"
```

### Body schema

```go
// SessionForkBody is the Payload body for a session.fork CogBlock.
type SessionForkBody struct {
    // ParentSessionHash is the content-addressed hash of the parent session
    // state at the fork point. This is the ledger entry hash, not a session ID.
    ParentSessionHash string `json:"parent_session_hash"`

    // ParentSessionID is the human-readable session ID of the parent, for
    // lineage queries. Denormalized from the hash for query efficiency.
    ParentSessionID string `json:"parent_session_id"`

    // ChildSessionID is the session ID minted for the child.
    ChildSessionID string `json:"child_session_id"`

    // ForkPoint is the ledger sequence number in the parent at which the fork
    // occurred. Used for time-travel and counterfactual queries.
    ForkPoint int64 `json:"fork_point"`

    // Overlay carries the layer-by-layer config override applied to the child.
    // Nil layers inherit the parent's value unchanged.
    Overlay SessionOverlay `json:"overlay"`

    // PinnedUntil is an optional expiry override. When set, the GC policy
    // will not collect the fork reference before this timestamp.
    PinnedUntil *time.Time `json:"pinned_until,omitempty"`
}
```

## Overlay schema

The overlay schema is grounded in the block-mesh design seed. Five layers; each is
independently overridable. Layer ordering follows Helm-style "later shadows earlier"
semantics for v1.

```go
// SessionOverlay declares which of the five block-mesh layers to override
// in the child session. Nil fields inherit the parent's value unchanged.
type SessionOverlay struct {
    // Identity overrides the identity card (who the child session runs as).
    // If nil, child inherits parent's identity.
    Identity *IdentityOverlay `json:"identity,omitempty"`

    // Role overrides the role context (what the child session is doing).
    // If nil, child inherits parent's role.
    Role *RoleOverlay `json:"role,omitempty"`

    // Context overrides the foveal context (what the child session sees).
    // If nil, child inherits parent's context at fork point.
    Context *ContextOverlay `json:"context,omitempty"`

    // Tools overrides the tool manifest (what the child session can do).
    // If nil, child inherits parent's tool grants.
    Tools *ToolsOverlay `json:"tools,omitempty"`

    // KVCache overrides the KV cache layer (what the child session's
    // inference channel is warm on). Composable with the vLLM provider
    // (RFC-0006); nil means child starts from a cold cache.
    KVCache *KVCacheOverlay `json:"kv_cache,omitempty"`
}

// IdentityOverlay partially overrides the identity layer.
type IdentityOverlay struct {
    // IdentityRef is a cog:// URI pointing to the identity CRD to use.
    // Overrides the parent's identity card entirely if set.
    IdentityRef string `json:"identity_ref,omitempty"`
}

// RoleOverlay partially overrides the role layer.
type RoleOverlay struct {
    // Role is the role name (e.g. "assistant", "btw-aside", "agent-worker").
    Role string `json:"role,omitempty"`
    // Constraints adds role-level capability constraints on top of identity.
    Constraints []string `json:"constraints,omitempty"`
}

// ContextOverlay partially overrides the context layer.
type ContextOverlay struct {
    // SeedRefs are cog:// URIs of cogdocs to inject as context seeds
    // for the child session. These are appended to (not replacing) the
    // parent's context at fork point.
    SeedRefs []string `json:"seed_refs,omitempty"`
    // ClearParentContext, if true, starts the child with a blank context
    // window rather than inheriting the parent's foveal state.
    ClearParentContext bool `json:"clear_parent_context,omitempty"`
}

// ToolsOverlay partially overrides the tools layer.
type ToolsOverlay struct {
    // GrantAdditional adds tool names to the child's grants.
    GrantAdditional []string `json:"grant_additional,omitempty"`
    // Revoke removes tool names from the child's inherited grants.
    Revoke []string `json:"revoke,omitempty"`
    // ReplaceAll, if true, replaces the entire tool grant list with
    // GrantAdditional. Use for strongly-restricted child sessions.
    ReplaceAll bool `json:"replace_all,omitempty"`
}

// KVCacheOverlay specifies the KV cache state the child inherits.
// Composable with the vLLM PagedAttention provider (RFC-0006).
type KVCacheOverlay struct {
    // InheritParentKV, if true, the child session hints to the inference
    // channel that it should warm its KV cache from the parent's block hash.
    // The channel honors this only if it supports block-addressed caching
    // (i.e. the vLLM provider; others degrade gracefully to a cold start).
    InheritParentKV bool `json:"inherit_parent_kv,omitempty"`
    // ParentKVBlockHash is the content-addressed hash of the parent's
    // KV state at fork point, supplied by the vLLM provider if available.
    ParentKVBlockHash string `json:"parent_kv_block_hash,omitempty"`
}
```

## Layer ordering (Helm-style)

When overlays compose (e.g. a child forks again), the rule is "later shadows earlier":
each layer in the child overlay fully shadows the corresponding parent layer. There is
no typed merge within a layer in v1. If a child's `Tools` overlay is non-nil, it
entirely replaces the parent's tools layer (modulo `GrantAdditional`/`Revoke` semantics
within the overlay itself). Typed merge per layer kind is deferred to a follow-up RFC
after real usage patterns are observed.

## Garbage collection

**Policy**: time-bounded retention. The default retention window is **7 days from the
fork point**. After 7 days, the ledger entry for the `session.fork` CogBlock is eligible
for GC; the parent session state it references is GC'd only when no other reference
(fork or otherwise) pins it.

**Parent-reference integrity**: the fork registry holds a GC root on each parent
session's ledger state for as long as any unexpired child fork exists. Parent ledger
gc-eligibility is computed as: `max(child_fork_expiry) over children + retention_margin`.
This prevents premature parent gc and ensures `ForkAncestors` walks succeed for all
live forks.

**Pinning**: the `PinnedUntil` field in `SessionForkBody` overrides GC. Setting it
explicitly marks the fork as pinned until the given timestamp. The `cog_fork_session`
tool accepts a `pin_duration` input (ISO 8601 duration string, e.g. `"P30D"`) to set
a custom retention at fork time.

**Cross-workspace forks**: deferred to constellation federation work. v1 is
intra-workspace only. Cross-workspace fork requests return `501 Not Implemented`.

## MCP tool: `cog_fork_session`

```go
// Tool registration in internal/engine/mcp_sessions.go
mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
    Name: "cog_fork_session",
    Description: "Fork an existing session at a specific ledger state, " +
        "producing a child session with an optional layer overlay. " +
        "Required: parent_session_id. Optional: parent_state_hash " +
        "(defaults to current HEAD), overlay (JSON object per SessionOverlay " +
        "schema), pin_duration (ISO 8601 duration, default P7D). " +
        "Returns: child_session_id, fork_block_hash, fork_point.",
}), withToolObserver(m, "cog_fork_session", m.toolForkSession))

// Input type
type forkSessionInput struct {
    ParentSessionID  string          `json:"parent_session_id"`
    ParentStateHash  string          `json:"parent_state_hash,omitempty"`
    Overlay          *SessionOverlay `json:"overlay,omitempty"`
    PinDuration      string          `json:"pin_duration,omitempty"` // ISO 8601
    ChildSessionID   string          `json:"child_session_id,omitempty"` // caller-supplied or minted
}

// Output type
type forkSessionOutput struct {
    ChildSessionID string `json:"child_session_id"`
    ForkBlockHash  string `json:"fork_block_hash"`
    ForkPoint      int64  `json:"fork_point"`
    PinnedUntil    string `json:"pinned_until,omitempty"` // RFC3339
}
```

## HTTP endpoint

`POST /api/sessions/{parent_session_id}/fork`

Request body: `forkSessionInput` JSON (with `parent_session_id` from the path param).
Response: `forkSessionOutput` JSON, 201 Created on success.
Errors: 404 if parent session unknown; 400 if overlay schema invalid; 501 if
cross-workspace fork requested.

Wired in `internal/engine/serve_kernel.go` alongside existing session endpoints.

## Ledger walk projection: fork lineage

Two projection functions expose the fork graph:

```go
// ForkChildren returns the direct child sessions forked from parentSessionID.
// Walks the ledger for session.fork CogBlocks with matching ParentSessionID.
func (s *SessionRegistry) ForkChildren(ctx context.Context, parentSessionID string) ([]string, error)

// ForkAncestors returns the lineage chain from childSessionID back to the
// root session (the session with no parent fork). Each element is a
// (sessionID, forkPoint, forkBlockHash) tuple.
func (s *SessionRegistry) ForkAncestors(ctx context.Context, childSessionID string) ([]ForkAncestor, error)

type ForkAncestor struct {
    SessionID     string `json:"session_id"`
    ForkPoint     int64  `json:"fork_point"`
    ForkBlockHash string `json:"fork_block_hash"`
}
```

Exposed via MCP as `cog_fork_children` and `cog_fork_ancestors` tools (thin wrappers
over the projection functions). Also accessible via HTTP:

- `GET /api/sessions/{session_id}/fork/children`
- `GET /api/sessions/{session_id}/fork/ancestors`

## Cross-RFC integration: KVBlockHashProvider

The fork handler queries the inference provider for the parent's KV block hash when
`KVCacheOverlay.InheritParentKV` is true. The integration point is the
`KVBlockHashProvider` interface, called by this RFC's fork handler and implemented by
the vLLM provider specified in RFC-0006.

```go
// KVBlockHashProvider is implemented by inference providers that expose
// a content-addressed KV-block layer (e.g., vLLM PagedAttention).
// The fork-session handler queries this when forking over a kvcache layer
// to obtain the parent's KV block hash.
type KVBlockHashProvider interface {
    // ParentKVBlockHash returns the hash of the KV block representing
    // the session's KV state at the given message ID, or an error if
    // the block is no longer addressable (evicted, runtime restarted).
    ParentKVBlockHash(ctx context.Context, sessionID string, atMessageID string) (BlockHash, error)
}
```

The vLLM provider (RFC-0006) implements this interface; the fork handler in
`cog_fork_session` calls it. When no provider implementing `KVBlockHashProvider` is
active, the fork handler degrades gracefully: `ParentKVBlockHash` returns an empty
hash and the child starts cold.

## Consumer skill: `/btw`

The proof-of-concept consumer is substrate-native `/btw` — a skill that forks the
current session, runs a parenthetical aside in the child, then returns.

Skill location: `~/.claude/skills/btw/SKILL.md` (cog-workspace repo path, not cogos).

The skill's procedure:

1. Call `cog_fork_session` with `parent_session_id` = current session ID and a
   `role` overlay of `"btw-aside"`.
2. Complete the aside in the child session context.
3. Optionally call `cog_register_session` to mark the child as ended.
4. Resume the parent session.

The `/btw` skill is the first demonstration that `cog_fork_session` composes with
the session-management surface without additional kernel changes.

## Compose-fits

- **RFC-0003** (CogBlock topology): `session.fork` is a new Kind in the topology;
  it fits the existing envelope without schema changes.
- **RFC-0006** (vLLM PagedAttention): the `KVCacheOverlay` field is the integration
  point. When the vLLM provider is active, `InheritParentKV: true` warm-starts the
  child's inference channel from the parent's block hash. When the vLLM provider is
  absent, the overlay is accepted and silently ignored (cold start).
- **ADR-059** (Five Systems, One Structure): fork extends the session system to six
  systems — the fork ledger is the sixth.
- **ADR-079** (CogDocs become CogBlocks): `session.fork` follows the CogBlock
  envelope exactly.
- **ADR-089** (pointer-envelope for external content): the `KVCacheOverlay.ParentKVBlockHash`
  field is an ADR-089 pointer — the envelope persists in the ledger; the pointed-to
  KV bytes may be evicted.

## Acceptance criteria

- [ ] RFC merged (this document).
- [ ] `session.fork` CogBlock Kind constant + `SessionForkBody` + `SessionOverlay`
      types committed to `internal/engine/`.
- [ ] Prerequisite (separate sub-issue before implementing `session.fork` Kind):
      Refactor existing Kind dispatch from switch-based to registry pattern so that
      adding the `session.fork` Kind requires no modification to Kind-handler switch
      statements; the Kind infrastructure dispatches via registry, not switch.
- [ ] `cog_fork_session` MCP tool registered and functional.
- [ ] `POST /api/sessions/{id}/fork` HTTP endpoint wired.
- [ ] `ForkChildren` and `ForkAncestors` projection functions implemented.
- [ ] `/btw` consumer skill committed to the cog-workspace skills directory.
- [ ] Unit tests cover: fork creation, overlay merge, GC eligibility calculation,
      lineage projection (parent→children, child→ancestors).
- [ ] Cross-workspace fork returns `501 Not Implemented`.
- [ ] Integration test: fork a session, resume child, verify lineage projection.
- [ ] All `cog_fork_session` operations emit structured logs per the kernel log
      convention with fields: `operation` (=`fork`), `parent_session_id`,
      `child_session_id`, `overlay_layers` (comma-separated), `ts`. Ledger-walk
      projection tools follow the same convention.

## Out of scope

- Typed merge per layer kind (deferred; follow-up RFC after usage patterns emerge).
- Cross-workspace forks (deferred to constellation federation work).
- Re-merge of child session into parent (possible via `cog_ingest`; no new primitive
  needed for v1).
- Fork-over-kvcache full hardware validation (blocked on Linux/CUDA target; the
  `KVCacheOverlay` wiring is specified here and stubbed in RFC-0006 scaffolding).

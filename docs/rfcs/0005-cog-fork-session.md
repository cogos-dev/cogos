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

The session's state-at-T, observed from its own perspective, hashes to a canonical
reference. A fork is publishing a marker that says "this point is now reference-able."
The substrate needs no separate fork-table or snapshot store; the ledger is already
hash-chained.

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

### Parent-reference coupling and lifetime semantics

This section specifies precisely what holds a reference to a parent session and when a
parent becomes GC-eligible. The defaults are intentionally conservative to minimize
surprise; the explicit-disown escape hatch handles the case where early release is
desired.

**What holds a parent reference:**

1. **Child sessions (direct)**: every active child session holds a strong reference to
   its parent via the `ParentSessionID` field in `SessionForkBody`. While any child
   session's `session.fork` CogBlock is within its retention window (i.e.
   `child_fork_expiry > now`), the parent session's ledger state is pinned and
   GC-ineligible. Children are the primary reference holders.

2. **Message handlers (transitive)**: if a child session's message handler is
   currently executing (i.e. the child session has an inflight MCP call or HTTP
   request in-flight), the handler holds a transitive reference to the parent via the
   child's `ParentSessionID`. This prevents the parent from being GC'd during an
   active cross-session lineage walk. The transitive reference is released when the
   handler returns.

3. **`PinnedUntil` timestamp (explicit)**: a fork with a non-nil `PinnedUntil` in its
   `SessionForkBody` holds a reference until that timestamp, regardless of child
   activity. This is the mechanism for long-running or deliberately-preserved fork
   points.

**When a parent becomes GC-eligible:**

A parent session's ledger state becomes GC-eligible when ALL of the following are true:

- No child session's `session.fork` CogBlock has `child_fork_expiry > now` (i.e. all
  children have expired or been explicitly disowned).
- No message handler is executing with a transitive reference to the parent.
- No active `PinnedUntil` timestamp on any child's `SessionForkBody` extends beyond now.
- The parent's own session retention window has elapsed (separate from child coupling;
  the parent is a session record in its own right with its own retention policy).

The parent-reference coupling adds an additional floor on top of the parent's own
retention: the parent cannot be GC'd before its last child's expiry even if the
parent's own retention window is shorter.

**Default: parent persists while any child is active**

The defensive default is: **a parent session persists in the ledger for as long as any
child fork is within its retention window**. This means:

- Forking is always safe to do on long-lived parents; the parent will not disappear
  while a child is using it.
- `ForkAncestors` walks always succeed for live forks.
- The cost is that long-lived children extend parent retention. This is intentional
  and correct: a fork over a long-lived parent's KV state is only coherent if the
  parent's ledger is intact.

**Explicit disown: `cog_fork_disown`**

When a child session is known-complete but its retention window has not yet elapsed,
the caller may release the parent reference early by calling `cog_fork_disown`:

```go
// forkDisownInput releases a child's reference to its parent,
// making the parent immediately GC-eligible (subject to other references).
// Does not delete the child session; only releases the parent reference hold.
type forkDisownInput struct {
    ChildSessionID string `json:"child_session_id"`
}
```

`cog_fork_disown` is a voluntary compact: the caller asserts it will no longer use
the parent-child lineage for this child. The fork registry removes the child's GC root
on the parent. If no other child holds a reference and no `PinnedUntil` is active, the
parent becomes immediately GC-eligible.

`cog_fork_disown` does NOT retroactively expire the child's `session.fork` CogBlock;
it is not a delete. The ledger entry remains readable until its own retention window
expires. Only the live GC root is released.

**Summary table:**

| Reference holder          | Holds parent until                          | Release mechanism              |
|---------------------------|---------------------------------------------|--------------------------------|
| Child session (active)    | `child_fork_expiry > now`                   | Expiry or `cog_fork_disown`    |
| In-flight message handler | Handler returns                             | Automatic (handler completion) |
| `PinnedUntil` timestamp   | `PinnedUntil > now`                         | Expiry or explicit update      |
| Parent's own retention    | Parent's own session retention window       | Expiry                         |

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

`POST /v1/sessions/{parent_session_id}/fork`

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

The projection functions are internal to v0.5.0. The `cog_fork_children` and
`cog_fork_ancestors` MCP tools and their HTTP endpoints are deferred to post-v0.5.0
(see §Future scope).

## Cross-RFC integration: KVBlockHashProvider

The fork handler queries the inference provider for the parent's KV block hash when
`KVCacheOverlay.InheritParentKV` is true. The integration point is the
`KVBlockHashProvider` interface, called by this RFC's fork handler and implemented by
the vLLM provider specified in RFC-0006.

### Interface definition

```go
// KVBlockHashProvider is implemented by inference providers that expose
// a content-addressed KV-block layer (e.g., vLLM PagedAttention).
// The fork-session handler queries this when forking over a kvcache layer
// to obtain the parent's KV block hash.
//
// Package: internal/engine (canonical definition; RFC-0006 implements it)
type KVBlockHashProvider interface {
    // ParentKVBlockHash returns the content-addressed hash of the KV block
    // representing the session's KV state at the given message ID.
    //
    // sessionID: the parent session whose KV state is being queried.
    // atMessageID: the message ID at the fork point (the last message in the
    //   parent's context window to be inherited by the child). Providers use
    //   this to identify which KV block covers the token range up to this message.
    //
    // Returns:
    //   - (BlockHash, nil) on success: the child can carry this hash as a warm-start
    //     hint to the inference channel.
    //   - ("", ErrKVBlockEvicted) if the block existed but has been evicted from the
    //     vLLM block manager. The fork handler treats this as a recoverable miss:
    //     the fork proceeds with a cold start.
    //   - ("", ErrKVProviderUnavailable) if the inference channel is unreachable or
    //     has restarted. Recoverable: fork proceeds cold.
    //   - ("", ErrKVBlockNotFound) if no KV block exists for the given session and
    //     message ID (session never reached a KV-addressable state). Recoverable.
    //   - ("", err) for any other error: treated as fatal by the fork handler —
    //     the fork is aborted and the error is returned to the caller.
    ParentKVBlockHash(ctx context.Context, sessionID string, atMessageID string) (BlockHash, error)
}

// BlockHash is a hex-encoded SHA-256 content-addressed identifier for a KV block.
// An empty BlockHash ("") signals absence; it is not a valid block reference.
type BlockHash string

// Sentinel errors returned by KVBlockHashProvider implementations.
// The fork handler checks these explicitly to distinguish recoverable from fatal.
var (
    // ErrKVBlockEvicted: block existed but was evicted from the block manager.
    // The fork handler degrades to cold start; this is not an error condition.
    ErrKVBlockEvicted = errors.New("kv block evicted")

    // ErrKVProviderUnavailable: the inference channel is unreachable.
    // The fork handler degrades to cold start.
    ErrKVProviderUnavailable = errors.New("kv provider unavailable")

    // ErrKVBlockNotFound: no KV block for this session+message combination.
    // The fork handler degrades to cold start.
    ErrKVBlockNotFound = errors.New("kv block not found")
)
```

### Ownership and lifecycle

**Who defines the interface**: `internal/engine` owns the canonical
`KVBlockHashProvider` interface definition. It lives alongside the fork handler, not
in the provider package, so that the fork handler does not import the provider.

**Who implements the interface**: RFC-0006's `internal/providers/vllm` package
implements `KVBlockHashProvider` on its `KVCacheProvider` struct. No other provider
in v0.5.0 implements the interface; all others degrade gracefully (see below).

**Who creates the provider**: the kernel's provider registry at startup. The
`KVCacheProvider` is registered into the provider registry by the vLLM provider
init path. The fork handler obtains it via a registry lookup at fork time — it does
not hold a long-lived reference to the provider instance.

**Who calls the interface**: the fork handler in `toolForkSession`
(`internal/engine/mcp_sessions.go`). The call happens exactly once per fork invocation,
inside the fork transaction, before the `session.fork` CogBlock is written to the
ledger.

**Lifecycle**: the `KVBlockHashProvider` implementation is active for as long as the
vLLM inference channel is registered in the provider registry. If the vLLM channel
is removed (e.g. runtime restart, explicit unregister), the registry lookup returns
nil and the fork handler degrades to cold start for the duration of the channel
absence. No dangling references: the fork handler does not cache the provider pointer
across calls.

### Error semantics

| Error returned                | Recoverable? | Fork handler action              |
|-------------------------------|--------------|----------------------------------|
| `ErrKVBlockEvicted`           | Yes          | Degrade to cold start; log warn  |
| `ErrKVProviderUnavailable`    | Yes          | Degrade to cold start; log warn  |
| `ErrKVBlockNotFound`          | Yes          | Degrade to cold start; log info  |
| `nil` provider (no vLLM)      | Yes          | Degrade to cold start; no log    |
| Any other non-nil error       | No (fatal)   | Abort fork; return error to caller |

"Degrade to cold start" means: `KVCacheOverlay.ParentKVBlockHash` is set to `""` in
the `session.fork` body; the child session starts without a KV warm-start hint. The
fork itself succeeds; only the KV warm-start is lost.

### Example invocation (fork handler)

```go
// Inside toolForkSession, after resolving the fork point and before writing
// the session.fork CogBlock to the ledger:

var kvBlockHash BlockHash
if input.Overlay != nil && input.Overlay.KVCache != nil && input.Overlay.KVCache.InheritParentKV {
    provider, ok := m.registry.LookupKVBlockHashProvider()
    if ok {
        hash, err := provider.ParentKVBlockHash(ctx, input.ParentSessionID, forkPoint.LastMessageID)
        switch {
        case err == nil:
            kvBlockHash = hash
        case errors.Is(err, ErrKVBlockEvicted),
             errors.Is(err, ErrKVProviderUnavailable),
             errors.Is(err, ErrKVBlockNotFound):
            // Recoverable: proceed with cold start.
            m.logger.Warn("kv block unavailable for fork; degrading to cold start",
                "session", input.ParentSessionID,
                "message", forkPoint.LastMessageID,
                "err", err)
        default:
            // Fatal: surface to caller.
            return nil, fmt.Errorf("fork aborted: KVBlockHashProvider error: %w", err)
        }
    }
    // No provider registered: silent cold start (no log; expected when vLLM absent).
}
input.Overlay.KVCache.ParentKVBlockHash = string(kvBlockHash)
```

### Graceful degradation when provider is absent

When no vLLM channel is registered (e.g. local dev with mlx-engine only), the
provider registry returns `ok = false` from `LookupKVBlockHashProvider()`. The fork
handler silently sets `ParentKVBlockHash = ""` and proceeds. The child session starts
cold. This is the correct and expected behavior for all non-vLLM inference channels.

The `KVCacheOverlay.InheritParentKV = true` field is accepted and written into the
`session.fork` CogBlock regardless of provider availability — it records the caller's
intent. The empty `ParentKVBlockHash` signals that the intent could not be satisfied
at fork time.

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
- [ ] `cog_fork_disown` MCP tool registered and functional: accepts `child_session_id`,
      removes the child's GC root on the parent in `ForkRegistry`, returns
      `{"status": "disowned", "child_session_id": "<id>"}`. Must be nil-safe when
      `ForkRegistry` is not wired (returns a clean "not configured" error).
- [ ] `POST /v1/sessions/{id}/fork` HTTP endpoint wired.
- [ ] `ForkChildren` and `ForkAncestors` projection functions implemented.
- [ ] `/btw` consumer skill committed to the cog-workspace skills directory.
- [ ] Unit tests cover: fork creation, overlay merge, GC eligibility calculation,
      lineage projection (parent→children, child→ancestors).
- [ ] Unit test for `cog_fork_disown`: disown removes GC root; parent becomes
      GC-eligible immediately when no other child holds a reference and no
      `PinnedUntil` is active; child's `session.fork` CogBlock remains readable.
- [ ] Cross-workspace fork returns `501 Not Implemented`.
- [ ] Integration test: fork a session, resume child, verify lineage projection.
- [ ] Integration test: fork a session, call `cog_fork_disown`, verify parent GC
      eligibility without requiring retention window expiry.
- [ ] All `cog_fork_session` and `cog_fork_disown` operations emit structured logs
      per the kernel log convention with fields: `operation` (=`fork` or `disown`),
      `parent_session_id`, `child_session_id`, `overlay_layers` (comma-separated,
      fork only), `ts`. Ledger-walk projection tools follow the same convention.

## Future scope (post-v0.5.0)

Lineage query tools deferred until the `/btw` consumer proves the primitive in
practice; a follow-up RFC will introduce `cog_fork_children` and `cog_fork_ancestors`
once usage patterns surface.

When ready, the MCP tools are thin wrappers over the projection functions already
implemented in v0.5.0, exposed via HTTP:

- `GET /v1/sessions/{session_id}/fork/children`
- `GET /v1/sessions/{session_id}/fork/ancestors`

## Out of scope

- Typed merge per layer kind (deferred; follow-up RFC after usage patterns emerge).
- Cross-workspace forks (deferred to constellation federation work).
- Re-merge of child session into parent (possible via `cog_ingest`; no new primitive
  needed for v1).
- Fork-over-kvcache full hardware validation (blocked on Linux/CUDA target; the
  `KVCacheOverlay` wiring is specified here and stubbed in RFC-0006 scaffolding).

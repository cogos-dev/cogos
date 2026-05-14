// session_fork_types.go — types for RFC-0005 session forking primitive.
//
// Defines the five-layer SessionOverlay (block-mesh grounded), SessionForkBody
// (the CogBlock payload for Kind="session.fork"), and ForkAncestor (lineage
// projection tuple). All JSON-serializable; intended to land in bus_sessions
// events and the ledger unchanged.
//
// Layer ordering follows Helm-style "later shadows earlier" semantics. There
// is no typed merge within a layer in v1; a non-nil layer fully shadows the
// corresponding parent layer. See RFC-0005 §Layer ordering.
package engine

import (
	"context"
	"time"
)

// ─── overlay schema ──────────────────────────────────────────────────────────

// SessionOverlay declares which of the five block-mesh layers to override in
// the child session. Nil fields inherit the parent's value unchanged.
//
// Helm-style merge rule for v1: each non-nil field in this struct fully
// replaces the corresponding parent layer (modulo GrantAdditional/Revoke
// semantics inside ToolsOverlay). Typed per-field merge is deferred until
// real usage patterns are observed.
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

	// KVCache overrides the KV cache layer (what the child session's inference
	// channel is warm on). Composable with the vLLM provider (RFC-0006); nil
	// means child starts from a cold cache.
	KVCache *KVCacheOverlay `json:"kv_cache,omitempty"`
}

// OverlayLayers returns a comma-separated string of the non-nil layer names.
// Used in structured logs (RFC-0005 §Structured logs acceptance criterion).
func (o *SessionOverlay) OverlayLayers() string {
	if o == nil {
		return ""
	}
	names := make([]string, 0, 5)
	if o.Identity != nil {
		names = append(names, "identity")
	}
	if o.Role != nil {
		names = append(names, "role")
	}
	if o.Context != nil {
		names = append(names, "context")
	}
	if o.Tools != nil {
		names = append(names, "tools")
	}
	if o.KVCache != nil {
		names = append(names, "kvcache")
	}
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += n
	}
	return out
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
	// SeedRefs are cog:// URIs of cogdocs to inject as context seeds for the
	// child session. These are appended to (not replacing) the parent's
	// context at fork point.
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
	// ParentKVBlockHash is the content-addressed hash of the parent's KV
	// state at fork point, supplied by the vLLM provider if available.
	ParentKVBlockHash string `json:"parent_kv_block_hash,omitempty"`
}

// ─── fork body ───────────────────────────────────────────────────────────────

// SessionForkBody is the Payload body for a session.fork CogBlock.
// Embedded in CogBlock.RawPayload when Kind == KindSessionFork.
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

// ─── lineage projection ───────────────────────────────────────────────────────

// ForkAncestor is one element in the lineage chain returned by ForkAncestors.
// Each tuple describes one step from a child session back toward the root.
type ForkAncestor struct {
	SessionID     string `json:"session_id"`
	ForkPoint     int64  `json:"fork_point"`
	ForkBlockHash string `json:"fork_block_hash"`
}

// ─── KV-block provider interface ─────────────────────────────────────────────

// BlockHash is the content-addressed hash of a KV block (e.g. "sha256:<hex>").
// Defined here so the fork handler can reference it without importing the vLLM
// provider package (which does not exist yet in v0.5.0).
type BlockHash string

// KVBlockHashProvider is implemented by inference providers that expose a
// content-addressed KV-block layer (e.g. vLLM PagedAttention, RFC-0006).
// The fork-session handler queries this when forking with a KVCacheOverlay to
// obtain the parent's KV block hash at the fork point.
//
// When no provider implementing KVBlockHashProvider is active, the fork
// handler degrades gracefully: it sets KVCacheOverlay.ParentKVBlockHash to ""
// and the child starts cold.
type KVBlockHashProvider interface {
	// ParentKVBlockHash returns the hash of the KV block representing the
	// session's KV state at the given message ID, or an error if the block is
	// no longer addressable (evicted, runtime restarted).
	ParentKVBlockHash(ctx context.Context, sessionID string, atMessageID string) (BlockHash, error)
}

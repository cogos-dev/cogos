package cogblock

import (
	"encoding/json"
	"time"
)

// CogBlock is the canonical unit of interaction in the CogOS substrate.
// Every inbound interaction is normalized into a CogBlock before routing,
// context assembly, or inference. Every significant kernel action may emit one.
type CogBlock struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"session_id,omitempty"`
	ThreadID  string    `json:"thread_id,omitempty"`

	// Source identification.
	SourceChannel   string `json:"source_channel"`
	SourceTransport string `json:"source_transport"`
	SourceIdentity  string `json:"source_identity,omitempty"`

	// Target.
	TargetIdentity string `json:"target_identity,omitempty"`
	WorkspaceID    string `json:"workspace_id,omitempty"`

	// Content.
	Kind         CogBlockKind    `json:"kind"`
	RawPayload   json.RawMessage `json:"raw_payload,omitempty"`
	SystemPrompt string          `json:"system_prompt,omitempty"`

	// Messages is kept as raw JSON to avoid coupling to any specific
	// provider message format. The kernel decodes this into its own
	// ProviderMessage type; external consumers can decode as needed.
	Messages json.RawMessage `json:"messages,omitempty"`

	// Provenance.
	Provenance   BlockProvenance `json:"provenance"`
	TrustContext TrustContext    `json:"trust_context"`

	// Ledger linkage.
	LedgerRef string `json:"ledger_ref,omitempty"`

	// Artifacts produced from processing this block.
	Artifacts []BlockArtifact `json:"artifacts,omitempty"`

	// CanonForm identifies the canonicalization algorithm used to hash this block.
	// RFC-0003 Refinement 4 — canonicalization algorithm versioning.
	//
	// New blocks default to "rfc8785-v1". Existing blocks without CanonForm are
	// treated as "rfc8785-v1" at read time (no hash change; the algorithm has not
	// changed). A future "rfc8785-v2" would be declared here when a
	// canonicalization edge case is fixed; blocks with different CanonForm values
	// coexist in the ledger and are hashed independently under their declared
	// algorithm. See docs/rfcs/0003-cogblock-topology-refinements.md §Refinement 4.
	CanonForm string `json:"canon_form,omitempty"`
}

// CanonFormRFC8785V1 is the default canonicalization algorithm for CogBlock.
// It corresponds to RFC 8785 (JSON Canonicalization Scheme) with the
// initial implementation in pkg/cogblock/ledger.go. Declared as a named
// constant so callers can reference it without embedding a string literal.
// RFC-0003 Refinement 4.
const CanonFormRFC8785V1 = "rfc8785-v1"

// CogBlockKind identifies the type of interaction a CogBlock represents.
type CogBlockKind string

const (
	BlockMessage     CogBlockKind = "message"
	BlockToolCall    CogBlockKind = "tool_call"
	BlockToolResult  CogBlockKind = "tool_result"
	BlockImport      CogBlockKind = "import"
	BlockAttention   CogBlockKind = "attention"
	BlockSystemEvent CogBlockKind = "system_event"

	// ADR-059 block-type vocabulary.
	//
	// Document types (CogBlocks at rest).
	BlockDocInsight   CogBlockKind = "doc.insight"
	BlockDocEpisode   CogBlockKind = "doc.episode"
	BlockDocProcedure CogBlockKind = "doc.procedure"

	// Bus types (CogBlocks in flight).
	BlockBusMessage    CogBlockKind = "bus.message"
	BlockBusAck        CogBlockKind = "bus.ack"
	BlockBusCheckpoint CogBlockKind = "bus.checkpoint"

	// Session types.
	BlockSessionTurn CogBlockKind = "session.turn"

	// Inference cache types (RFC-0006).
	//
	// KindCacheKVBlock represents a single PagedAttention block in the vLLM
	// block manager — a fixed-size chunk of key-value tensors addressable by
	// content hash. Each block is registered in the Kind registry via
	// engine.RegisterKindHandler in internal/engine/kinds_vllm.go.
	KindCacheKVBlock CogBlockKind = "cache.kv_block"

	// Worktree lifecycle types (ADR-096).
	//
	// These Kinds are emitted to the per-session ledger by SpawnWorktree and
	// WorktreeReconciler.ApplyPlan. They bind every substrate-spawned worktree
	// to a dispatch identity from its first byte, closing the orphan-by-design
	// gap (ADR-096 §2). See pkg/cogblock/kinds.go for the canonical comment;
	// the constant definitions live here alongside the rest of the Kind set.
	//
	// BlockWorktreeCreated is written by SpawnWorktree BEFORE the underlying
	// `git worktree add` call (ADR-091 §5 ledger-first rule). Required payload
	// fields: worktree_id, dispatch_id, repo_root, worktree_path, branch, base,
	// created_at.
	BlockWorktreeCreated CogBlockKind = "worktree.created"

	// BlockWorktreeTerminal records that the dispatch bound to a worktree has
	// reached a terminal state. Required payload fields: worktree_id,
	// dispatch_id, reason (one of: "merged", "abandoned", "exited").
	BlockWorktreeTerminal CogBlockKind = "worktree.terminal"

	// BlockWorktreePruned is written by WorktreeReconciler.ApplyPlan after a
	// successful `git worktree remove --force`. Required payload fields:
	// worktree_id, worktree_path, pruned_at.
	BlockWorktreePruned CogBlockKind = "worktree.pruned"

	// BlockWorktreeAlarm is written by WorktreeReconciler.ApplyPlan when a
	// worktree is classified `alarm-uncommitted-on-terminal-dispatch` or
	// `alarm-unknown-binding`. The reconciler does NOT mutate the filesystem
	// on alarm; operator intervention is required (ADR-096 §4). Required
	// payload fields: worktree_path, classification; optional: dispatch_id,
	// branch, diagnostic details.
	BlockWorktreeAlarm CogBlockKind = "worktree.alarm"

	// BlockWorktreeRebind extends the lifecycle of an existing worktree by
	// associating it with a new dispatch identity (ADR-096 §4 Option C).
	// Required payload fields: worktree_id, old_dispatch_id, new_dispatch_id,
	// rebound_at.
	BlockWorktreeRebind CogBlockKind = "worktree.rebind"
)

// BlockProvenance records the origin and ingestion metadata of a CogBlock.
type BlockProvenance struct {
	OriginSession string    `json:"origin_session,omitempty"`
	OriginChannel string    `json:"origin_channel,omitempty"`
	IngestedAt    time.Time `json:"ingested_at"`
	NormalizedBy  string    `json:"normalized_by"`
}

// TrustContext captures authentication and authorization state for a CogBlock.
type TrustContext struct {
	Authenticated bool    `json:"authenticated"`
	TrustScore    float64 `json:"trust_score"`
	Scope         string  `json:"scope"`
}

// BlockArtifact references an output produced from processing a CogBlock.
type BlockArtifact struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

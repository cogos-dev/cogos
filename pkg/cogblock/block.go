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

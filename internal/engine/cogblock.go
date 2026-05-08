package engine

import (
	"encoding/json"
	"time"

	"github.com/myrgic/cogos/pkg/cogblock"
)

// CogBlock is the engine-local CogBlock that includes typed Messages.
// The canonical type definitions (CogBlockKind, BlockProvenance, TrustContext,
// BlockArtifact) live in pkg/cogblock and are re-exported below.
//
// This struct mirrors cogblock.CogBlock but replaces the raw Messages field
// with the engine's typed []ProviderMessage for internal processing.
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
	Kind         CogBlockKind      `json:"kind"`
	RawPayload   json.RawMessage   `json:"raw_payload,omitempty"`
	Messages     []ProviderMessage `json:"messages,omitempty"`
	SystemPrompt string            `json:"system_prompt,omitempty"`

	// Provenance.
	Provenance   BlockProvenance `json:"provenance"`
	TrustContext TrustContext    `json:"trust_context"`

	// Ledger linkage.
	LedgerRef string `json:"ledger_ref,omitempty"`

	// Artifacts produced from processing this block.
	Artifacts []BlockArtifact `json:"artifacts,omitempty"`

	// Sections carries the sub-block index for section-level content
	// addressing (ADR-059 Phase 2). Each entry describes one addressable
	// sub-unit of this block's payload by title, anchor, content hash,
	// and byte size. Empty for blocks without sub-blocks.
	Sections []Section `json:"sections,omitempty"`

	// Prev carries the V2 DAG predecessor hashes (ADR-059).
	//
	// V1 chain semantics use a single string predecessor (historically
	// PrevHash on sibling wire formats such as BusEvent/UCPMessage).
	// V2 DAG semantics use Prev []string: one or more predecessors.
	// Linear chain → len(Prev)==1; merge block → len(Prev)>1; genesis
	// → empty. ADR-059 Phase 1: both shapes accepted on the wire; hash
	// computation omits Prev (and any V1 PrevHash sibling field) from
	// the canonical form to keep the hash stable across chain topology
	// changes.
	Prev []string `json:"prev,omitempty"`
}

// Section describes an addressable sub-block of a CogBlock's payload,
// used for section-level content addressing and delta sync (ADR-059).
// The Hash field contains "sha256:<hex>" of the canonicalized section
// content; Size is the byte length of that content.
type Section struct {
	Title  string `json:"title,omitempty"`
	Anchor string `json:"anchor,omitempty"` // fragment identifier (e.g. "#section-1")
	Hash   string `json:"hash,omitempty"`   // sha256:<hex> of canonicalized section content
	Size   int    `json:"size,omitempty"`   // byte length of section content
}

// Re-export shared types from pkg/cogblock.
// These are type aliases so existing code compiles without changes.
type CogBlockKind = cogblock.CogBlockKind
type BlockProvenance = cogblock.BlockProvenance
type TrustContext = cogblock.TrustContext
type BlockArtifact = cogblock.BlockArtifact

const (
	BlockMessage     = cogblock.BlockMessage
	BlockToolCall    = cogblock.BlockToolCall
	BlockToolResult  = cogblock.BlockToolResult
	BlockImport      = cogblock.BlockImport
	BlockAttention   = cogblock.BlockAttention
	BlockSystemEvent = cogblock.BlockSystemEvent
)

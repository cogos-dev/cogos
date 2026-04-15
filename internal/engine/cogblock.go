package engine

import (
	"encoding/json"
	"time"

	"github.com/cogos-dev/cogos/pkg/cogblock"
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

package main

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

type CogBlockKind string

const (
	BlockMessage     CogBlockKind = "message"
	BlockToolCall    CogBlockKind = "tool_call"
	BlockToolResult  CogBlockKind = "tool_result"
	BlockImport      CogBlockKind = "import"
	BlockAttention   CogBlockKind = "attention"
	BlockSystemEvent CogBlockKind = "system_event"
)

type BlockProvenance struct {
	OriginSession string    `json:"origin_session,omitempty"`
	OriginChannel string    `json:"origin_channel,omitempty"`
	IngestedAt    time.Time `json:"ingested_at"`
	NormalizedBy  string    `json:"normalized_by"`
}

type TrustContext struct {
	Authenticated bool    `json:"authenticated"`
	TrustScore    float64 `json:"trust_score"`
	Scope         string  `json:"scope"`
}

type BlockArtifact struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

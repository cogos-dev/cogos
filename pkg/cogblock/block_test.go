package cogblock

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCogBlockKindConstants(t *testing.T) {
	tests := []struct {
		kind CogBlockKind
		want string
	}{
		{BlockMessage, "message"},
		{BlockToolCall, "tool_call"},
		{BlockToolResult, "tool_result"},
		{BlockImport, "import"},
		{BlockAttention, "attention"},
		{BlockSystemEvent, "system_event"},
	}

	for _, tt := range tests {
		if string(tt.kind) != tt.want {
			t.Errorf("CogBlockKind = %q; want %q", tt.kind, tt.want)
		}
	}
}

func TestCogBlock_JSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)

	original := CogBlock{
		ID:              "block-001",
		Timestamp:       now,
		SessionID:       "session-abc",
		ThreadID:        "thread-xyz",
		SourceChannel:   "claude-code",
		SourceTransport: "jsonrpc",
		SourceIdentity:  "user-1",
		TargetIdentity:  "cog",
		WorkspaceID:     "ws-42",
		Kind:            BlockMessage,
		RawPayload:      json.RawMessage(`{"text":"hello"}`),
		SystemPrompt:    "You are helpful.",
		Messages:        json.RawMessage(`[{"role":"user","content":"hi"}]`),
		Provenance: BlockProvenance{
			OriginSession: "session-abc",
			OriginChannel: "claude-code",
			IngestedAt:    now,
			NormalizedBy:  "tailer_claudecode",
		},
		TrustContext: TrustContext{
			Authenticated: true,
			TrustScore:    0.95,
			Scope:         "workspace",
		},
		LedgerRef: "ledger-ref-001",
		Artifacts: []BlockArtifact{
			{Kind: "memory_write", Ref: "mem/semantic/insight.md"},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded CogBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Verify key fields survived round-trip
	if decoded.ID != original.ID {
		t.Errorf("ID = %q; want %q", decoded.ID, original.ID)
	}
	if decoded.Kind != original.Kind {
		t.Errorf("Kind = %q; want %q", decoded.Kind, original.Kind)
	}
	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID = %q; want %q", decoded.SessionID, original.SessionID)
	}
	if decoded.SourceChannel != original.SourceChannel {
		t.Errorf("SourceChannel = %q; want %q", decoded.SourceChannel, original.SourceChannel)
	}
	if !decoded.Timestamp.Equal(original.Timestamp) {
		t.Errorf("Timestamp = %v; want %v", decoded.Timestamp, original.Timestamp)
	}
	if decoded.Provenance.NormalizedBy != original.Provenance.NormalizedBy {
		t.Errorf("Provenance.NormalizedBy = %q; want %q", decoded.Provenance.NormalizedBy, original.Provenance.NormalizedBy)
	}
	if decoded.TrustContext.TrustScore != original.TrustContext.TrustScore {
		t.Errorf("TrustContext.TrustScore = %v; want %v", decoded.TrustContext.TrustScore, original.TrustContext.TrustScore)
	}
	if len(decoded.Artifacts) != 1 || decoded.Artifacts[0].Kind != "memory_write" {
		t.Errorf("Artifacts round-trip failed: %+v", decoded.Artifacts)
	}
	if string(decoded.Messages) != string(original.Messages) {
		t.Errorf("Messages = %s; want %s", decoded.Messages, original.Messages)
	}
}

func TestCogBlock_MinimalJSON(t *testing.T) {
	// A block with only required fields should marshal/unmarshal cleanly
	b := CogBlock{
		Kind: BlockToolCall,
	}

	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded CogBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.Kind != BlockToolCall {
		t.Errorf("Kind = %q; want %q", decoded.Kind, BlockToolCall)
	}
}

func TestCogBlockKind_JSONRoundTrip(t *testing.T) {
	type wrapper struct {
		Kind CogBlockKind `json:"kind"`
	}

	original := wrapper{Kind: BlockAttention}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Should serialize as the string value
	expected := `{"kind":"attention"}`
	if string(data) != expected {
		t.Errorf("JSON = %s; want %s", data, expected)
	}

	var decoded wrapper
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.Kind != BlockAttention {
		t.Errorf("Kind = %q; want %q", decoded.Kind, BlockAttention)
	}
}

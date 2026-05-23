package session

import (
	"encoding/json"
	"testing"
)

// TestTrackingRoundtrip verifies that Tracking marshals to the wire
// shape kernel consumers expect (camelCase JSON tags, omitempty on
// EndedAt and Status) and unmarshals back to an equal struct.
func TestTrackingRoundtrip(t *testing.T) {
	ended := "2026-05-23T12:00:00Z"
	status := "ended"
	in := Tracking{
		SessionID:     "feat/adr-100-step3d",
		Branch:        "feat/adr-100-step3d",
		StartedAt:     "2026-05-23T10:00:00Z",
		EndedAt:       &ended,
		Status:        &status,
		RootAgent:     "root",
		SpawnedAgents: []string{"a1", "a2"},
		ActiveAgents:  []string{"a1"},
		ReapedAgents:  []string{"a2"},
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out Tracking
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.SessionID != in.SessionID {
		t.Errorf("SessionID: got %q want %q", out.SessionID, in.SessionID)
	}
	if out.Branch != in.Branch {
		t.Errorf("Branch: got %q want %q", out.Branch, in.Branch)
	}
	if out.StartedAt != in.StartedAt {
		t.Errorf("StartedAt: got %q want %q", out.StartedAt, in.StartedAt)
	}
	if out.EndedAt == nil || *out.EndedAt != *in.EndedAt {
		t.Errorf("EndedAt mismatch")
	}
	if out.Status == nil || *out.Status != *in.Status {
		t.Errorf("Status mismatch")
	}
	if out.RootAgent != in.RootAgent {
		t.Errorf("RootAgent: got %q want %q", out.RootAgent, in.RootAgent)
	}
	if len(out.SpawnedAgents) != 2 || out.SpawnedAgents[0] != "a1" || out.SpawnedAgents[1] != "a2" {
		t.Errorf("SpawnedAgents: %v", out.SpawnedAgents)
	}
	if len(out.ActiveAgents) != 1 || out.ActiveAgents[0] != "a1" {
		t.Errorf("ActiveAgents: %v", out.ActiveAgents)
	}
	if len(out.ReapedAgents) != 1 || out.ReapedAgents[0] != "a2" {
		t.Errorf("ReapedAgents: %v", out.ReapedAgents)
	}
}

// TestTrackingOmitEmpty verifies that EndedAt and Status are omitted from
// JSON output when nil — preserving the wire shape that the existing
// .cog/status/.session readers expect.
func TestTrackingOmitEmpty(t *testing.T) {
	in := Tracking{
		SessionID:     "s",
		Branch:        "b",
		StartedAt:     "t",
		RootAgent:     "root",
		SpawnedAgents: []string{},
		ActiveAgents:  []string{},
		ReapedAgents:  []string{},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["endedAt"]; ok {
		t.Errorf("endedAt should be omitted when nil")
	}
	if _, ok := raw["status"]; ok {
		t.Errorf("status should be omitted when nil")
	}
}

// TestTrackingFieldNames pins the JSON field-name surface so the
// kernel↔substrate boundary cannot drift silently.
func TestTrackingFieldNames(t *testing.T) {
	in := Tracking{
		SessionID:     "s",
		Branch:        "b",
		StartedAt:     "t",
		RootAgent:     "root",
		SpawnedAgents: []string{},
		ActiveAgents:  []string{},
		ReapedAgents:  []string{},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, want := range []string{"sessionId", "branch", "startedAt", "rootAgent", "spawnedAgents", "activeAgents", "reapedAgents"} {
		if _, ok := raw[want]; !ok {
			t.Errorf("missing field %q in JSON output", want)
		}
	}
}

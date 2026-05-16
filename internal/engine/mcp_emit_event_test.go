// mcp_emit_event_test.go — unit tests for the cog_emit_event MCP tool.
//
// Covers:
//   - Existing four event types still accepted (regression guard).
//   - peer.utterance: valid and all rejection cases.
//   - from_session: recorded correctly; rejection on unregistered session;
//     rejection on mismatch with payload.from.
//   - Unknown event type is rejected.
package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// makeServerWithSessions creates an MCPServer with a live Process and a
// pre-wired session registry. Returns the server and registry so tests can
// seed sessions before calling toolEmitEvent.
func makeServerWithSessions(t *testing.T) (*MCPServer, *SessionRegistry) {
	t.Helper()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	bus := NewBusSessionManager(root)
	registry := NewSessionRegistry()
	handoffs := NewHandoffRegistry()
	server.SetSessionsBackend(bus, registry, handoffs)

	return server, registry
}

// registerTestSession seeds the registry with a live session for use in tests.
func registerTestSession(t *testing.T, registry *SessionRegistry, sessionID string) {
	t.Helper()
	now := time.Now().UTC()
	state := SessionState{
		SessionID:    sessionID,
		Workspace:    "/tmp/test",
		Role:         "peer-cog",
		RegisteredAt: now,
		LastSeen:     now,
	}
	_, _, err := registry.ApplyRegister(state, defaultActiveWithinSeconds*time.Second, now, nil)
	if err != nil {
		t.Fatalf("registerTestSession %q: %v", sessionID, err)
	}
}

// callEmitEvent is a thin wrapper that calls toolEmitEvent and returns the
// decoded text content of the first result element. If the result is a JSON
// object it is returned as-is; if it is a plain error message string, that
// string is returned. The caller can inspect it for expected substrings.
func callEmitEvent(t *testing.T, server *MCPServer, input emitEventInput) string {
	t.Helper()
	result, _, err := server.toolEmitEvent(context.Background(), nil, input)
	if err != nil {
		t.Fatalf("toolEmitEvent returned Go error (unexpected): %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("toolEmitEvent returned nil/empty result")
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] type = %T; want *mcp.TextContent", result.Content[0])
	}
	return tc.Text
}

// ─── existing event types (regression guard) ─────────────────────────────────

func TestToolEmitEvent_ExistingTypesAccepted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   emitEventInput
		wantKey string // key expected in JSON response
	}{
		{
			name:    "attention.boost",
			input:   emitEventInput{Type: "attention.boost", Payload: map[string]any{"uri": "cog://mem/semantic/test.cog.md", "weight": 1.5}},
			wantKey: "emitted",
		},
		{
			name:    "session.marker",
			input:   emitEventInput{Type: "session.marker", Payload: map[string]any{"label": "checkpoint"}},
			wantKey: "emitted",
		},
		{
			name:    "insight.captured",
			input:   emitEventInput{Type: "insight.captured", Payload: map[string]any{"summary": "test insight", "tags": []string{"test"}}},
			wantKey: "emitted",
		},
		{
			name:    "decision.made",
			input:   emitEventInput{Type: "decision.made", Payload: map[string]any{"decision": "use Go", "rationale": "performance"}},
			wantKey: "emitted",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, _ := makeServerWithSessions(t)
			got := callEmitEvent(t, server, tc.input)
			var resp map[string]any
			if err := json.Unmarshal([]byte(got), &resp); err != nil {
				t.Fatalf("response is not JSON: %q", got)
			}
			if _, ok := resp[tc.wantKey]; !ok {
				t.Errorf("response missing key %q; got %v", tc.wantKey, resp)
			}
		})
	}
}

// ─── unknown event type ───────────────────────────────────────────────────────

func TestToolEmitEvent_UnknownTypeRejected(t *testing.T) {
	t.Parallel()
	server, _ := makeServerWithSessions(t)
	got := callEmitEvent(t, server, emitEventInput{Type: "banana.split"})
	if !strings.Contains(got, "unknown event type") {
		t.Errorf("expected 'unknown event type' in response; got %q", got)
	}
}

func TestToolEmitEvent_EmptyTypeRejected(t *testing.T) {
	t.Parallel()
	server, _ := makeServerWithSessions(t)
	got := callEmitEvent(t, server, emitEventInput{Type: ""})
	if !strings.Contains(got, "event type is required") {
		t.Errorf("expected 'event type is required' in response; got %q", got)
	}
}

// ─── peer.utterance: valid ───────────────────────────────────────────────────

func TestToolEmitEvent_PeerUtteranceValid(t *testing.T) {
	t.Parallel()
	server, registry := makeServerWithSessions(t)
	registerTestSession(t, registry, "opus-cog-darkstar")
	registerTestSession(t, registry, "gemma-cog-eclipse")

	input := emitEventInput{
		Type:        "peer.utterance",
		FromSession: "opus-cog-darkstar",
		Payload: map[string]any{
			"from":    "opus-cog-darkstar",
			"to":      "gemma-cog-eclipse",
			"content": "What is the eigenform?",
			"turn":    float64(1),
		},
	}
	got := callEmitEvent(t, server, input)
	var resp map[string]any
	if err := json.Unmarshal([]byte(got), &resp); err != nil {
		t.Fatalf("expected JSON response; got %q", got)
	}
	if emitted, _ := resp["emitted"].(bool); !emitted {
		t.Errorf("expected emitted=true; got %v", resp)
	}
	if resp["from_session"] != "opus-cog-darkstar" {
		t.Errorf("from_session = %v; want opus-cog-darkstar", resp["from_session"])
	}
}

// ─── peer.utterance: rejection cases ─────────────────────────────────────────

func TestToolEmitEvent_PeerUtterance_MissingFromSession(t *testing.T) {
	t.Parallel()
	server, registry := makeServerWithSessions(t)
	registerTestSession(t, registry, "opus-cog-darkstar")
	registerTestSession(t, registry, "gemma-cog-eclipse")

	input := emitEventInput{
		Type: "peer.utterance",
		// FromSession intentionally omitted
		Payload: map[string]any{
			"from":    "opus-cog-darkstar",
			"to":      "gemma-cog-eclipse",
			"content": "Hello",
			"turn":    float64(1),
		},
	}
	got := callEmitEvent(t, server, input)
	if !strings.Contains(got, "requires from_session") {
		t.Errorf("expected 'requires from_session' in response; got %q", got)
	}
}

func TestToolEmitEvent_PeerUtterance_UnregisteredFrom(t *testing.T) {
	t.Parallel()
	server, registry := makeServerWithSessions(t)
	// Register only the "to" session; "from" is not registered.
	registerTestSession(t, registry, "gemma-cog-eclipse")

	input := emitEventInput{
		Type:        "peer.utterance",
		FromSession: "opus-cog-darkstar", // not registered
		Payload: map[string]any{
			"from":    "opus-cog-darkstar",
			"to":      "gemma-cog-eclipse",
			"content": "Hello",
			"turn":    float64(1),
		},
	}
	got := callEmitEvent(t, server, input)
	if !strings.Contains(got, "not a registered session") {
		t.Errorf("expected 'not a registered session' in response; got %q", got)
	}
}

func TestToolEmitEvent_PeerUtterance_UnregisteredTo(t *testing.T) {
	t.Parallel()
	server, registry := makeServerWithSessions(t)
	registerTestSession(t, registry, "opus-cog-darkstar")
	// "to" session not registered

	input := emitEventInput{
		Type:        "peer.utterance",
		FromSession: "opus-cog-darkstar",
		Payload: map[string]any{
			"from":    "opus-cog-darkstar",
			"to":      "gemma-cog-eclipse", // not registered
			"content": "Hello",
			"turn":    float64(1),
		},
	}
	got := callEmitEvent(t, server, input)
	if !strings.Contains(got, "not a registered session") {
		t.Errorf("expected 'not a registered session' in response; got %q", got)
	}
}

func TestToolEmitEvent_PeerUtterance_FromSessionMismatch(t *testing.T) {
	t.Parallel()
	server, registry := makeServerWithSessions(t)
	registerTestSession(t, registry, "opus-cog-darkstar")
	registerTestSession(t, registry, "gemma-cog-eclipse")
	registerTestSession(t, registry, "haiku-cog-aux")

	input := emitEventInput{
		Type:        "peer.utterance",
		FromSession: "haiku-cog-aux", // differs from payload.from
		Payload: map[string]any{
			"from":    "opus-cog-darkstar",
			"to":      "gemma-cog-eclipse",
			"content": "Hello",
			"turn":    float64(1),
		},
	}
	got := callEmitEvent(t, server, input)
	if !strings.Contains(got, "must match payload.from") {
		t.Errorf("expected 'must match payload.from' in response; got %q", got)
	}
}

func TestToolEmitEvent_PeerUtterance_EmptyContent(t *testing.T) {
	t.Parallel()
	server, registry := makeServerWithSessions(t)
	registerTestSession(t, registry, "opus-cog-darkstar")
	registerTestSession(t, registry, "gemma-cog-eclipse")

	input := emitEventInput{
		Type:        "peer.utterance",
		FromSession: "opus-cog-darkstar",
		Payload: map[string]any{
			"from":    "opus-cog-darkstar",
			"to":      "gemma-cog-eclipse",
			"content": "", // empty
			"turn":    float64(1),
		},
	}
	got := callEmitEvent(t, server, input)
	if !strings.Contains(got, "non-empty 'content'") {
		t.Errorf("expected content error; got %q", got)
	}
}

func TestToolEmitEvent_PeerUtterance_MissingTurn(t *testing.T) {
	t.Parallel()
	server, registry := makeServerWithSessions(t)
	registerTestSession(t, registry, "opus-cog-darkstar")
	registerTestSession(t, registry, "gemma-cog-eclipse")

	input := emitEventInput{
		Type:        "peer.utterance",
		FromSession: "opus-cog-darkstar",
		Payload: map[string]any{
			"from":    "opus-cog-darkstar",
			"to":      "gemma-cog-eclipse",
			"content": "Hello",
			// turn missing
		},
	}
	got := callEmitEvent(t, server, input)
	if !strings.Contains(got, "turn") {
		t.Errorf("expected turn error; got %q", got)
	}
}

func TestToolEmitEvent_PeerUtterance_NonPositiveTurn(t *testing.T) {
	t.Parallel()
	server, registry := makeServerWithSessions(t)
	registerTestSession(t, registry, "opus-cog-darkstar")
	registerTestSession(t, registry, "gemma-cog-eclipse")

	input := emitEventInput{
		Type:        "peer.utterance",
		FromSession: "opus-cog-darkstar",
		Payload: map[string]any{
			"from":    "opus-cog-darkstar",
			"to":      "gemma-cog-eclipse",
			"content": "Hello",
			"turn":    float64(0), // invalid: must be >= 1
		},
	}
	got := callEmitEvent(t, server, input)
	if !strings.Contains(got, "positive integer") {
		t.Errorf("expected positive integer error; got %q", got)
	}
}

// ─── from_session: other event types ─────────────────────────────────────────

func TestToolEmitEvent_FromSession_RecordedOnInsightCaptured(t *testing.T) {
	t.Parallel()
	server, registry := makeServerWithSessions(t)
	registerTestSession(t, registry, "opus-cog-darkstar")

	input := emitEventInput{
		Type:        "insight.captured",
		FromSession: "opus-cog-darkstar",
		Payload:     map[string]any{"summary": "binding event is the smallest unit of maintenance"},
	}
	got := callEmitEvent(t, server, input)
	var resp map[string]any
	if err := json.Unmarshal([]byte(got), &resp); err != nil {
		t.Fatalf("expected JSON response; got %q", got)
	}
	if resp["from_session"] != "opus-cog-darkstar" {
		t.Errorf("from_session = %v; want opus-cog-darkstar", resp["from_session"])
	}
	if emitted, _ := resp["emitted"].(bool); !emitted {
		t.Errorf("emitted = %v; want true", resp["emitted"])
	}
}

func TestToolEmitEvent_FromSession_UnregisteredSessionRejected(t *testing.T) {
	t.Parallel()
	server, _ := makeServerWithSessions(t)
	// Registry is empty — no sessions registered.

	input := emitEventInput{
		Type:        "session.marker",
		FromSession: "opus-cog-darkstar", // not registered
		Payload:     map[string]any{"label": "checkpoint"},
	}
	got := callEmitEvent(t, server, input)
	if !strings.Contains(got, "not a registered session") {
		t.Errorf("expected 'not a registered session' in response; got %q", got)
	}
}

func TestToolEmitEvent_FromSession_OmittedKeepsLegacyBehavior(t *testing.T) {
	t.Parallel()
	server, _ := makeServerWithSessions(t)

	input := emitEventInput{
		Type:    "session.marker",
		Payload: map[string]any{"label": "checkpoint"},
	}
	got := callEmitEvent(t, server, input)
	var resp map[string]any
	if err := json.Unmarshal([]byte(got), &resp); err != nil {
		t.Fatalf("expected JSON response; got %q", got)
	}
	if emitted, _ := resp["emitted"].(bool); !emitted {
		t.Errorf("emitted = %v; want true", resp["emitted"])
	}
	if _, has := resp["from_session"]; has {
		t.Errorf("from_session should be absent when not provided; got %v", resp["from_session"])
	}
}

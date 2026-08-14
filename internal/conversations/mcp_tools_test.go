// mcp_tools_test.go — targeted tests for the MCP tool response shape.
package conversations

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestListConversations_SingleThreadSessionOmitsThreadsBlock verifies that
// cog_list_conversations only emits the threads[] array for genuinely
// multi-thread sessions (N>1 roots). A one-thread session's threads[] entry
// duplicates turn_count/message_count for zero benefit, and — multiplied
// across every session in the default (unbounded) response — was the #557
// review's HIGH finding on cog_list_conversations output being destroyed by
// per-session thread bloat.
func TestListConversations_SingleThreadSessionOmitsThreadsBlock(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	singleTurns := []Turn{
		{UUID: "s-u1", SessionID: "single-thread-session", TurnIndex: 0, Role: RoleUser, Timestamp: now, Text: "hi"},
		{UUID: "s-a1", ParentUUID: "s-u1", SessionID: "single-thread-session", TurnIndex: 1, Role: RoleAssistant, Timestamp: now.Add(time.Minute), Text: "hello"},
	}
	singleThreads := PartitionThreads(singleTurns, nil)
	if len(singleThreads) != 1 {
		t.Fatalf("fixture setup: want 1 thread, got %d", len(singleThreads))
	}

	multiTurns := []Turn{
		{UUID: "m-u1", SessionID: "multi-thread-session", TurnIndex: 0, Role: RoleUser, Timestamp: now, Text: "hi"},
		{UUID: "m-a1", ParentUUID: "m-u1", SessionID: "multi-thread-session", TurnIndex: 1, Role: RoleAssistant, Timestamp: now.Add(time.Minute), Text: "hello"},
		{UUID: "m-sub-u1", SessionID: "multi-thread-session", TurnIndex: 2, Role: RoleUser, Timestamp: now.Add(2 * time.Minute), Text: "sidechain", IsSidechain: true},
		{UUID: "m-sub-a1", ParentUUID: "m-sub-u1", SessionID: "multi-thread-session", TurnIndex: 3, Role: RoleAssistant, Timestamp: now.Add(3 * time.Minute), Text: "sidechain reply", IsSidechain: true},
	}
	multiThreads := PartitionThreads(multiTurns, nil)
	if len(multiThreads) != 2 {
		t.Fatalf("fixture setup: want 2 threads, got %d", len(multiThreads))
	}

	idx := &Index{
		sessions: map[string]SessionMeta{
			"single-thread-session": {
				SessionID: "single-thread-session",
				TurnCount: len(singleTurns),
				Threads:   singleThreads,
			},
			"multi-thread-session": {
				SessionID: "multi-thread-session",
				TurnCount: len(multiTurns),
				Threads:   multiThreads,
			},
		},
		turns: map[string][]Turn{
			"single-thread-session": singleTurns,
			"multi-thread-session":  multiTurns,
		},
	}

	p := &Provider{index: idx}
	handler := makeListConversationsHandler(p, defaultConvMaxBytes)

	result, _, err := handler(context.Background(), nil, listConversationsInput{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("want 1 content block, got %d", len(result.Content))
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("want *mcp.TextContent, got %T", result.Content[0])
	}

	var parsed struct {
		Sessions []struct {
			SessionID string           `json:"session_id"`
			TurnCount int              `json:"turn_count"`
			Threads   []map[string]any `json:"threads,omitempty"`
		} `json:"sessions"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(tc.Text), &parsed); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, tc.Text)
	}
	if parsed.Count != 2 {
		t.Fatalf("want 2 sessions in response, got %d", parsed.Count)
	}

	byID := map[string][]map[string]any{}
	for _, s := range parsed.Sessions {
		byID[s.SessionID] = s.Threads
	}

	if threads, ok := byID["single-thread-session"]; !ok {
		t.Fatalf("single-thread-session missing from response")
	} else if threads != nil {
		t.Errorf("single-thread session must omit threads[] entirely, got %+v", threads)
	}

	if threads, ok := byID["multi-thread-session"]; !ok {
		t.Fatalf("multi-thread-session missing from response")
	} else if len(threads) != 2 {
		t.Errorf("multi-thread session should still emit its 2 threads, got %+v", threads)
	}
}

// TestListConversations_DefaultLimitCapsUnboundedCall is the round-2 HIGH
// regression: previously input.Limit was applied only when >0, so an
// operator calling cog_list_conversations with no limit at all got an
// unbounded response that could exceed maxBytes and get truncated by
// capConvOutput mid-array — an invalid-JSON, silently-partial result. A
// corpus larger than defaultConvListLimit must now come back as a complete,
// valid, EXPLICITLY-marked-partial document instead.
func TestListConversations_DefaultLimitCapsUnboundedCall(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	const nSessions = defaultConvListLimit + 50

	sessions := make(map[string]SessionMeta, nSessions)
	turns := make(map[string][]Turn, nSessions)
	for i := 0; i < nSessions; i++ {
		sid := fmt.Sprintf("session-%03d", i)
		ts := make([]Turn, 1)
		ts[0] = Turn{UUID: sid + "-u1", SessionID: sid, TurnIndex: 0, Role: RoleUser, Timestamp: now.Add(time.Duration(i) * time.Second), Text: "hi"}
		turns[sid] = ts
		sessions[sid] = SessionMeta{SessionID: sid, TurnCount: 1, FirstTurnAt: ts[0].Timestamp, LastTurnAt: ts[0].Timestamp}
	}

	idx := &Index{sessions: sessions, turns: turns}
	p := &Provider{index: idx}
	handler := makeListConversationsHandler(p, defaultConvMaxBytes)

	result, _, err := handler(context.Background(), nil, listConversationsInput{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("want *mcp.TextContent, got %T", result.Content[0])
	}

	var parsed struct {
		Sessions  []map[string]any `json:"sessions"`
		Count     int              `json:"count"`
		Total     int              `json:"total"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(tc.Text), &parsed); err != nil {
		t.Fatalf("unmarshal response (must be valid JSON, not a mid-array cut): %v\nraw: %s", err, tc.Text)
	}
	if parsed.Count != defaultConvListLimit {
		t.Errorf("count: want %d (default limit), got %d", defaultConvListLimit, parsed.Count)
	}
	if parsed.Total != nSessions {
		t.Errorf("total: want %d (all matching sessions, uncapped), got %d", nSessions, parsed.Total)
	}
	if !parsed.Truncated {
		t.Errorf("truncated: want true (total > count), got false")
	}
	if len(parsed.Sessions) != defaultConvListLimit {
		t.Errorf("sessions array length: want %d, got %d", defaultConvListLimit, len(parsed.Sessions))
	}
}

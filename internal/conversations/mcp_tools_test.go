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
// cog_list_conversations only emits thread_count for genuinely multi-thread
// sessions (N>1 roots). A one-thread session's thread_count would duplicate
// turn_count for zero benefit, and — multiplied across every session in the
// default (unbounded) response — a per-session threads[] array (the original
// #557 review's HIGH finding) was destroying cog_list_conversations output;
// round 3 found that shape still exceeded the byte cap on the real corpus
// even after being limited to 100 sessions, so the per-session detail is now
// a single integer (thread_count) rather than an array of per-thread
// records — O(1) regardless of how many threads a session has.
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
			SessionID   string `json:"session_id"`
			TurnCount   int    `json:"turn_count"`
			ThreadCount int    `json:"thread_count,omitempty"`
		} `json:"sessions"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(tc.Text), &parsed); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, tc.Text)
	}
	if parsed.Count != 2 {
		t.Fatalf("want 2 sessions in response, got %d", parsed.Count)
	}

	byID := map[string]int{}
	for _, s := range parsed.Sessions {
		byID[s.SessionID] = s.ThreadCount
	}

	if count, ok := byID["single-thread-session"]; !ok {
		t.Fatalf("single-thread-session missing from response")
	} else if count != 0 {
		t.Errorf("single-thread session must omit thread_count entirely, got %d", count)
	}

	if count, ok := byID["multi-thread-session"]; !ok {
		t.Fatalf("multi-thread-session missing from response")
	} else if count != 2 {
		t.Errorf("multi-thread session should report thread_count 2, got %d", count)
	}
}

// TestListConversations_DefaultLimitCapsUnboundedCall is the round-2 HIGH
// regression, strengthened per round-3's finding that the original fixture
// (150 one-turn sessions with no Threads, no Title, no Identity/Entrypoint)
// was too thin to ever approach defaultConvMaxBytes, so it stayed green
// while the real corpus (which has titles, identities, entrypoints, and
// multi-thread sessions) still produced an invalid-JSON mid-array cut. Round
// 3 measured that corpus directly: a limit-100 response WITH a per-thread
// threads[] array was 41,494 bytes (invalid JSON once capConvOutput cut it),
// the identical response WITHOUT that array was 27,132 bytes (valid, all 100
// sessions present) — so this fixture mirrors that shape: every session
// carries an entrypoint and realistic timestamps, roughly a third carry a
// title, a quarter carry an identity, and a third are genuinely multi-thread
// (only the presence of >1 Threads entries matters now — see ThreadCount's
// doc comment in mcp_tools.go — not how many). It asserts both JSON validity
// and that the returned text stays within maxBytes, not just that it
// happens to parse.
func TestListConversations_DefaultLimitCapsUnboundedCall(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	const nSessions = defaultConvListLimit + 50
	titles := []string{
		"Fix the OAuth refresh bug",
		"Repair conversations thread-DAG",
		"Debug BEP ping reflection storm",
		"Review PR #557 round 3",
		"Investigate LMS retry loop",
	}

	sessions := make(map[string]SessionMeta, nSessions)
	turns := make(map[string][]Turn, nSessions)
	for i := 0; i < nSessions; i++ {
		sid := fmt.Sprintf("%08x-1234-5678-9abc-def012345%03d", i, i)
		ts := make([]Turn, 1)
		ts[0] = Turn{UUID: sid + "-u1", SessionID: sid, TurnIndex: 0, Role: RoleUser, Timestamp: now.Add(time.Duration(i) * time.Second), Text: "hi"}
		turns[sid] = ts

		meta := SessionMeta{
			SessionID:   sid,
			TurnCount:   i%12 + 1,
			FirstTurnAt: ts[0].Timestamp,
			LastTurnAt:  ts[0].Timestamp,
			IndexedAt:   now,
			Entrypoint:  "cli",
		}
		if i%3 == 0 {
			meta.Title = titles[i%len(titles)]
		}
		if i%4 == 0 {
			meta.Identity = "chazmaniandinkle"
		}
		// A third of sessions are genuinely multi-thread (the shape that
		// used to serialize as a per-thread threads[] array before the
		// round-3 fix switched to a plain thread_count integer — the count
		// of Threads entries here is deliberately > 1 to exercise that
		// path; how many entries doesn't matter to output size any more,
		// which is exactly the property this test guards).
		if i%3 == 0 {
			meta.Threads = make([]ThreadMeta, 0, 6)
			for j := 0; j < 6; j++ {
				meta.Threads = append(meta.Threads, ThreadMeta{
					ThreadID:     fmt.Sprintf("%s-thread-%d", sid, j),
					Role:         ThreadRoleUnknownFork,
					MessageCount: 12 + j,
				})
			}
		}
		sessions[sid] = meta
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

	if len(tc.Text) > defaultConvMaxBytes {
		t.Errorf("response text: %d bytes exceeds defaultConvMaxBytes (%d) — the default call must stay within the cap, not merely parse after being cut", len(tc.Text), defaultConvMaxBytes)
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

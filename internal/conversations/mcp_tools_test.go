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

// TestListConversations_ByteBudgetCutStaysValidAndHonest is the #557
// round-4 review HIGH regression fixture for the byte-cut path itself
// (distinct from the limit cut exercised above): round 4 measured that an
// explicit limit large enough to exceed maxBytes produced a response whose
// bytes exceeded the cap, whose JSON did not parse, and whose "count" field
// overstated how many session objects actually survived the cut — with
// "truncated" absent entirely, since truncatedByLimit only ever reflected
// the limit slice, not the byte cut. This fixture uses a small maxBytes and
// a limit comfortably larger than what fits, and asserts the response is
// always valid JSON within the budget, with count == len(sessions) == what
// was actually emitted, and truncated == true.
func TestListConversations_ByteBudgetCutStaysValidAndHonest(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	const nSessions = 200
	// A title long enough (45 chars, matching round-4's own measurement)
	// that only a fraction of nSessions fits under a deliberately small
	// maxBytes.
	longTitle := "Investigate the OAuth session refresh contention bug"

	sessions := make(map[string]SessionMeta, nSessions)
	turns := make(map[string][]Turn, nSessions)
	for i := 0; i < nSessions; i++ {
		sid := fmt.Sprintf("%08x-1234-5678-9abc-def012345%03d", i, i)
		ts := []Turn{{UUID: sid + "-u1", SessionID: sid, TurnIndex: 0, Role: RoleUser, Timestamp: now.Add(time.Duration(i) * time.Second), Text: "hi"}}
		turns[sid] = ts
		sessions[sid] = SessionMeta{
			SessionID:   sid,
			Title:       longTitle,
			TurnCount:   i%12 + 1,
			FirstTurnAt: ts[0].Timestamp,
			LastTurnAt:  ts[0].Timestamp,
			IndexedAt:   now,
			Entrypoint:  "cli",
			Identity:    "chazmaniandinkle",
		}
	}

	idx := &Index{sessions: sessions, turns: turns}
	p := &Provider{index: idx}
	const smallMaxBytes = minConvMaxBytes // 4 KiB — forces the byte cut well before Limit is reached
	handler := makeListConversationsHandler(p, smallMaxBytes)

	result, _, err := handler(context.Background(), nil, listConversationsInput{Limit: nSessions})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("want *mcp.TextContent, got %T", result.Content[0])
	}

	if len(tc.Text) > smallMaxBytes {
		t.Fatalf("response text: %d bytes exceeds the %d-byte budget — the byte cut must never let the "+
			"response exceed maxBytes", len(tc.Text), smallMaxBytes)
	}

	var parsed struct {
		Sessions  []map[string]any `json:"sessions"`
		Count     int              `json:"count"`
		Total     int              `json:"total"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(tc.Text), &parsed); err != nil {
		t.Fatalf("unmarshal response (must be valid JSON — round 4's own regression was an invalid mid-array "+
			"cut): %v\nraw: %s", err, tc.Text)
	}

	if parsed.Total != nSessions {
		t.Errorf("total: want %d (all matching sessions, uncapped), got %d", nSessions, parsed.Total)
	}
	if len(parsed.Sessions) == 0 {
		t.Fatalf("expected at least some sessions to fit under the budget, got 0")
	}
	if len(parsed.Sessions) >= nSessions {
		t.Fatalf("test setup: expected the byte cut to bite before all %d sessions fit, got %d",
			nSessions, len(parsed.Sessions))
	}
	// The load-bearing assertion: count must equal what actually survived
	// the cut, not the limit the caller asked for and not the pre-cut
	// candidate count.
	if parsed.Count != len(parsed.Sessions) {
		t.Errorf("count (%d) != actual sessions in response (%d) — count must reflect exactly what was "+
			"emitted, not an overstated claim", parsed.Count, len(parsed.Sessions))
	}
	if !parsed.Truncated {
		t.Errorf("truncated: want true (the byte cut, not just the limit, dropped sessions), got false")
	}
}

// TestGetConversationTurn_IncludesSessionThreadList is the #557 round-4
// review MEDIUM regression fixture: after cog_list_conversations dropped
// its per-session threads[] array in favor of a bare thread_count integer,
// no MCP surface exposed a session's thread list (thread_id, role,
// message_count) or any thread's message_count at all — the comment
// claiming cog_get_conversation_turn already covered this was false (it
// only ever returned the ONE addressed turn's own thread_id/thread_role).
// This fixture asserts cog_get_conversation_turn's response now carries
// the addressed session's full "threads" list alongside that per-turn
// thread_id/thread_role.
func TestGetConversationTurn_IncludesSessionThreadList(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	turns := []Turn{
		{UUID: "m-u1", SessionID: "multi-thread-session", TurnIndex: 0, Role: RoleUser, Timestamp: now, Text: "hi"},
		{UUID: "m-a1", ParentUUID: "m-u1", SessionID: "multi-thread-session", TurnIndex: 1, Role: RoleAssistant, Timestamp: now.Add(time.Minute), Text: "hello"},
		{UUID: "m-sub-u1", SessionID: "multi-thread-session", TurnIndex: 2, Role: RoleUser, Timestamp: now.Add(2 * time.Minute), Text: "sidechain", IsSidechain: true},
		{UUID: "m-sub-a1", ParentUUID: "m-sub-u1", SessionID: "multi-thread-session", TurnIndex: 3, Role: RoleAssistant, Timestamp: now.Add(3 * time.Minute), Text: "sidechain reply", IsSidechain: true},
	}
	threads := PartitionThreads(turns, nil)
	if len(threads) != 2 {
		t.Fatalf("fixture setup: want 2 threads, got %d", len(threads))
	}

	idx := &Index{
		sessions: map[string]SessionMeta{
			"multi-thread-session": {SessionID: "multi-thread-session", TurnCount: len(turns), Threads: threads},
		},
		turns: map[string][]Turn{"multi-thread-session": turns},
	}
	p := &Provider{index: idx}
	handler := makeGetConversationTurnHandler(p, defaultConvMaxBytes)

	result, _, err := handler(context.Background(), nil, getConversationTurnInput{SessionID: "multi-thread-session", TurnIndex: 0})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("want *mcp.TextContent, got %T", result.Content[0])
	}

	var parsed struct {
		ThreadID   string `json:"thread_id"`
		ThreadRole string `json:"thread_role"`
		Threads    []struct {
			ThreadID     string `json:"thread_id"`
			Role         string `json:"role"`
			MessageCount int    `json:"message_count"`
		} `json:"threads"`
	}
	if err := json.Unmarshal([]byte(tc.Text), &parsed); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, tc.Text)
	}

	if parsed.ThreadID != "m-u1" {
		t.Errorf("thread_id: want m-u1 (the addressed turn's own thread), got %q", parsed.ThreadID)
	}
	if parsed.ThreadRole != string(ThreadRoleMain) {
		t.Errorf("thread_role: want main, got %q", parsed.ThreadRole)
	}
	if len(parsed.Threads) != 2 {
		t.Fatalf("threads: want 2 entries (the whole session's thread list), got %d: %+v", len(parsed.Threads), parsed.Threads)
	}
	byID := map[string]struct {
		Role  string
		Count int
	}{}
	for _, tm := range parsed.Threads {
		byID[tm.ThreadID] = struct {
			Role  string
			Count int
		}{tm.Role, tm.MessageCount}
	}
	if got, ok := byID["m-u1"]; !ok || got.Role != string(ThreadRoleMain) || got.Count != 2 {
		t.Errorf("threads[m-u1]: want role=main count=2, got %+v (present=%v)", got, ok)
	}
	if got, ok := byID["m-sub-u1"]; !ok || got.Role != string(ThreadRoleSubagentSidechain) || got.Count != 2 {
		t.Errorf("threads[m-sub-u1]: want role=subagent-sidechain count=2, got %+v (present=%v)", got, ok)
	}
}

// TestSliceToMap_EmitsSessionsThreadIndexNotApplicable is the #557 round-5
// review LOW regression fixture: ResolvedSlice.SessionsThreadIndexNotApplicable
// is documented (uri_resolver.go) as existing precisely so a caller watching
// sessions_missing_thread_index trend to zero can tell "still pending" apart
// from "will never resolve" — but sliceToMap only ever emitted
// sessions_missing_thread_index. Both cog_search_conversations (uri mode)
// and cog_get_conversation_turn (uri mode) go through sliceToMap, so the
// distinction never reached either MCP surface it was written for.
func TestSliceToMap_EmitsSessionsThreadIndexNotApplicable(t *testing.T) {
	slice := &ResolvedSlice{
		URI:                              "cog:conversations?thread_role=main",
		ResolvedAt:                       time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		Count:                            3,
		Sources:                          []string{"cc"},
		Bounded:                          true,
		SessionsMissingThreadIndex:       2,
		SessionsThreadIndexNotApplicable: 5,
		Turns:                            nil,
	}

	m := sliceToMap(slice)

	got, ok := m["sessions_thread_index_not_applicable"]
	if !ok {
		t.Fatalf("sliceToMap output missing key %q entirely: %+v", "sessions_thread_index_not_applicable", m)
	}
	if got != 5 {
		t.Errorf("sessions_thread_index_not_applicable: want 5, got %v", got)
	}
	// The existing counter must still be present and unaffected.
	if m["sessions_missing_thread_index"] != 2 {
		t.Errorf("sessions_missing_thread_index: want 2, got %v", m["sessions_missing_thread_index"])
	}
}

// TestSliceToMap_OmitsSessionsThreadIndexNotApplicableWhenZero verifies the
// omitempty-style behavior sliceToMap gives every other optional counter:
// a zero SessionsThreadIndexNotApplicable must not appear in the output map
// at all (matching sessions_missing_thread_index's existing > 0 guard).
func TestSliceToMap_OmitsSessionsThreadIndexNotApplicableWhenZero(t *testing.T) {
	slice := &ResolvedSlice{
		URI:        "cog:conversations",
		ResolvedAt: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		Count:      1,
		Sources:    []string{"cc"},
		Bounded:    true,
	}

	m := sliceToMap(slice)

	if _, ok := m["sessions_thread_index_not_applicable"]; ok {
		t.Errorf("sessions_thread_index_not_applicable: want absent when zero, got %v",
			m["sessions_thread_index_not_applicable"])
	}
}

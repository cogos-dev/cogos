// provider_test.go — integration tests for the Conversations Observatory provider.
//
// Covers:
//   1. Ingesting a 1-session JSONL fixture; assert turn count, identity, time bounds
//   2. Streaming a large fixture (512KB equivalent) without OOM
//   3. Drift detection: session modified after index, re-projection runs
//   4. MCP tool query path: search for a known string, assert hit
//   5. Idempotency: re-run ApplyPlan, no double-projection
//   6. LoadConfig with empty config (default source dirs)
//   7. FetchLive round-trip (write then read)
//   8. BuildState structure
//   9. Health() status transitions
package conversations

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── JSONL fixture helpers ────────────────────────────────────────────────────

// sessionUUID is a fixed UUID for test sessions.
const sessionUUID = "11111111-2222-3333-4444-555555555555"

// makeUserRecord returns a JSON line representing a user message.
func makeUserRecord(uuid, parentUUID, sessionID, text, ts string) string {
	content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	msg, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": json.RawMessage(content),
	})
	rec := map[string]any{
		"type":       "user",
		"uuid":       uuid,
		"parentUuid": parentUUID,
		"sessionId":  sessionID,
		"timestamp":  ts,
		"message":    json.RawMessage(msg),
		"userType":   "external",
		"entrypoint": "cli",
	}
	b, _ := json.Marshal(rec)
	return string(b)
}

// makeAssistantRecord returns a JSON line representing an assistant message.
func makeAssistantRecord(uuid, parentUUID, sessionID, text, ts string) string {
	content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	msg, _ := json.Marshal(map[string]any{
		"role":    "assistant",
		"model":   "claude-sonnet-4-6",
		"content": json.RawMessage(content),
	})
	rec := map[string]any{
		"type":       "assistant",
		"uuid":       uuid,
		"parentUuid": parentUUID,
		"sessionId":  sessionID,
		"timestamp":  ts,
		"message":    json.RawMessage(msg),
		"userType":   "external",
		"entrypoint": "cli",
	}
	b, _ := json.Marshal(rec)
	return string(b)
}

// makeAITitleRecord returns a JSON line for an ai-title record.
func makeAITitleRecord(sessionID, title string) string {
	rec := map[string]any{
		"type":      "ai-title",
		"sessionId": sessionID,
		"title":     title,
	}
	b, _ := json.Marshal(rec)
	return string(b)
}

// writeJSONLFixture writes a JSONL fixture file to dir/<sessionID>.jsonl.
func writeJSONLFixture(t *testing.T, dir, sessionID string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// ─── Test helpers ─────────────────────────────────────────────────────────────

// newTestProvider creates a Provider backed by a temp workspace.
func newTestProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	root := t.TempDir()
	p := NewProvider()
	return p, root
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestProviderIngest_SingleSession asserts that a single-session JSONL is
// parsed correctly: turn count, identity attribution, and time bounds.
func TestProviderIngest_SingleSession(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()

	ts1 := "2026-05-01T10:00:00Z"
	ts2 := "2026-05-01T10:01:00Z"
	ts3 := "2026-05-01T10:02:00Z"

	lines := []string{
		makeAITitleRecord(sessionUUID, "Test Session Title"),
		makeUserRecord("uuid-u1", "", sessionUUID, "what is the harness attestation policy?", ts1),
		makeAssistantRecord("uuid-a1", "uuid-u1", sessionUUID, "The harness attestation policy is...", ts2),
		makeUserRecord("uuid-u2", "uuid-a1", sessionUUID, "tell me more about operator identity", ts3),
	}
	writeJSONLFixture(t, srcDir, sessionUUID, lines)

	// Write a custom observatory.yaml pointing at our temp source dir.
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	liveAny, err := p.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}

	plan, err := p.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	if plan.Summary.Creates != 1 {
		t.Errorf("expected 1 create, got %d", plan.Summary.Creates)
	}

	results, err := p.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != "succeeded" {
		t.Errorf("expected succeeded, got %s: %s", results[0].Status, results[0].Error)
	}

	// Re-fetch live to confirm the session was indexed.
	liveAny2, err := p.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive 2: %v", err)
	}
	ls, ok := liveAny2.(*liveState)
	if !ok {
		t.Fatal("expected *liveState")
	}
	entry, ok := ls.Entries[sessionUUID]
	if !ok {
		t.Fatalf("session %s not in live state", sessionUUID)
	}

	// 2 user turns + 1 assistant turn = 3 total turns indexed.
	if entry.Meta.TurnCount != 3 {
		t.Errorf("expected 3 turns, got %d", entry.Meta.TurnCount)
	}
	if entry.Meta.Title != "Test Session Title" {
		t.Errorf("expected title 'Test Session Title', got %q", entry.Meta.Title)
	}
	if entry.Meta.Entrypoint != "cli" {
		t.Errorf("expected entrypoint 'cli', got %q", entry.Meta.Entrypoint)
	}

	// Check time bounds.
	wantFirst, _ := time.Parse(time.RFC3339, ts1)
	wantLast, _ := time.Parse(time.RFC3339, ts3)
	if !entry.Meta.FirstTurnAt.Equal(wantFirst) {
		t.Errorf("FirstTurnAt: want %v, got %v", wantFirst, entry.Meta.FirstTurnAt)
	}
	if !entry.Meta.LastTurnAt.Equal(wantLast) {
		t.Errorf("LastTurnAt: want %v, got %v", wantLast, entry.Meta.LastTurnAt)
	}
}

// TestProviderIngest_LargeSession verifies that indexing a large synthetic
// session (many turns totalling ~1MB) completes without OOM and indexes
// a sensible turn count.
func TestProviderIngest_LargeSession(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()

	const largeSID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const nTurns = 2000
	lines := make([]string, 0, nTurns+1)
	lines = append(lines, makeAITitleRecord(largeSID, "Large Session"))

	for i := 0; i < nTurns; i++ {
		ts := time.Now().UTC().Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		text := strings.Repeat("a", 400) // 400 bytes per turn × 2000 = ~800KB of content
		if i%2 == 0 {
			lines = append(lines, makeUserRecord(
				fmt.Sprintf("uuid-u%d", i), "", largeSID, text, ts,
			))
		} else {
			lines = append(lines, makeAssistantRecord(
				fmt.Sprintf("uuid-a%d", i), fmt.Sprintf("uuid-u%d", i-1), largeSID, text, ts,
			))
		}
	}
	writeJSONLFixture(t, srcDir, largeSID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan, _ := p.ComputePlan(cfgAny, liveAny, nil)
	results, err := p.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) == 0 || results[0].Status != "succeeded" {
		t.Fatalf("expected success, got: %v", results)
	}

	liveAny2, _ := p.FetchLive(ctx, cfgAny)
	ls := liveAny2.(*liveState)
	entry := ls.Entries[largeSID]

	if entry.Meta.TurnCount != nTurns {
		t.Errorf("expected %d turns, got %d", nTurns, entry.Meta.TurnCount)
	}
}

// TestProviderDriftDetection verifies that a modified session (mtime/size
// change) triggers an update action on the next plan cycle.
func TestProviderDriftDetection(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	const driftSID = "dddddddd-dddd-dddd-dddd-dddddddddddd"

	// Initial index.
	lines := []string{
		makeUserRecord("u1", "", driftSID, "first message", "2026-05-01T10:00:00Z"),
	}
	fixturePath := writeJSONLFixture(t, srcDir, driftSID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan, _ := p.ComputePlan(cfgAny, liveAny, nil)
	_, _ = p.ApplyPlan(ctx, plan)

	// Confirm first cycle: skip (just indexed).
	cfgAny, _ = p.LoadConfig(root)
	liveAny, _ = p.FetchLive(ctx, cfgAny)
	plan2, _ := p.ComputePlan(cfgAny, liveAny, nil)
	if plan2.Summary.Updates != 0 || plan2.Summary.Creates != 0 {
		t.Error("expected skip after initial index, got changes")
	}

	// Modify the file (add a new line to change size + mtime).
	additional := "\n" + makeUserRecord("u2", "u1", driftSID, "second message", "2026-05-01T10:01:00Z")
	f, err := os.OpenFile(fixturePath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open fixture for append: %v", err)
	}
	_, err = f.WriteString(additional)
	f.Close()
	if err != nil {
		t.Fatalf("append to fixture: %v", err)
	}

	// Ensure mtime is different (filesystem resolution can be 1s).
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(fixturePath, future, future); err != nil {
		t.Logf("chtimes: %v (non-fatal)", err)
	}

	// Re-plan: should detect update.
	cfgAny, _ = p.LoadConfig(root)
	liveAny, _ = p.FetchLive(ctx, cfgAny)
	plan3, err := p.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan 3: %v", err)
	}
	if plan3.Summary.Updates != 1 {
		t.Errorf("expected 1 update after file modification, got %d", plan3.Summary.Updates)
	}

	// Apply the update and verify new turn count.
	results, _ := p.ApplyPlan(ctx, plan3)
	if len(results) == 0 || results[0].Status != "succeeded" {
		t.Fatalf("apply update failed: %v", results)
	}
	liveAny4, _ := p.FetchLive(ctx, cfgAny)
	ls := liveAny4.(*liveState)
	if ls.Entries[driftSID].Meta.TurnCount != 2 {
		t.Errorf("expected 2 turns after update, got %d", ls.Entries[driftSID].Meta.TurnCount)
	}
}

// TestProviderIdempotency verifies that running ApplyPlan twice on the same
// session produces a skip on the second pass (no double-indexing).
func TestProviderIdempotency(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	const idemSID = "ffffffff-ffff-ffff-ffff-ffffffffffff"

	lines := []string{
		makeUserRecord("u1", "", idemSID, "idempotency test message", "2026-05-01T10:00:00Z"),
	}
	writeJSONLFixture(t, srcDir, idemSID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()

	// First pass: create.
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan1, _ := p.ComputePlan(cfgAny, liveAny, nil)
	_, _ = p.ApplyPlan(ctx, plan1)

	// Second pass: should skip.
	cfgAny, _ = p.LoadConfig(root)
	liveAny, _ = p.FetchLive(ctx, cfgAny)
	plan2, err := p.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan 2: %v", err)
	}
	if plan2.Summary.Creates != 0 || plan2.Summary.Updates != 0 {
		t.Errorf("expected skip on second pass; got creates=%d updates=%d",
			plan2.Summary.Creates, plan2.Summary.Updates)
	}
	if plan2.Summary.Skipped != 1 {
		t.Errorf("expected 1 skip, got %d", plan2.Summary.Skipped)
	}
}

// TestMCPSearchConversations verifies the MCP tool query path: index a session
// with a known string, then search for it and assert a hit.
func TestMCPSearchConversations(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	const searchSID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const needle = "harness attestation policy"

	lines := []string{
		makeUserRecord("u1", "", searchSID,
			"what is the "+needle+"?", "2026-05-10T12:00:00Z"),
		makeAssistantRecord("a1", "u1", searchSID,
			"The attestation model is defined in ADR-073.", "2026-05-10T12:01:00Z"),
	}
	writeJSONLFixture(t, srcDir, searchSID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan, _ := p.ComputePlan(cfgAny, liveAny, nil)
	_, _ = p.ApplyPlan(ctx, plan)

	// Direct index search (bypasses MCP layer, tests the query logic).
	p.mu.Lock()
	idx := p.index
	p.mu.Unlock()

	if idx == nil {
		t.Fatal("index is nil after ApplyPlan")
	}

	hits := idx.Search(needle, time.Time{}, time.Time{}, "", "", 10)
	if len(hits) == 0 {
		t.Fatalf("expected at least 1 hit for %q, got 0", needle)
	}
	if hits[0].SessionID != searchSID {
		t.Errorf("expected session %s, got %s", searchSID, hits[0].SessionID)
	}
	if hits[0].Role != RoleUser {
		t.Errorf("expected user role, got %s", hits[0].Role)
	}
	if !strings.Contains(strings.ToLower(hits[0].Excerpt), strings.ToLower(needle)) {
		t.Errorf("excerpt %q does not contain needle %q", hits[0].Excerpt, needle)
	}
}

// TestBuildState verifies that BuildState produces a well-formed reconcile.State.
func TestBuildState(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	const bsSID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	lines := []string{
		makeUserRecord("u1", "", bsSID, "build state test", "2026-05-01T10:00:00Z"),
	}
	writeJSONLFixture(t, srcDir, bsSID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan, _ := p.ComputePlan(cfgAny, liveAny, nil)
	_, _ = p.ApplyPlan(ctx, plan)

	liveAny2, _ := p.FetchLive(ctx, cfgAny)
	state, err := p.BuildState(cfgAny, liveAny2, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if state.ResourceType != "conversations" {
		t.Errorf("expected resource_type 'conversations', got %q", state.ResourceType)
	}
	if len(state.Resources) != 1 {
		t.Errorf("expected 1 resource, got %d", len(state.Resources))
	}
	r := state.Resources[0]
	if r.ExternalID != bsSID {
		t.Errorf("expected external_id %s, got %s", bsSID, r.ExternalID)
	}
}

// TestHealthTransitions verifies Health() returns correct statuses.
func TestHealthTransitions(t *testing.T) {
	p := NewProvider()

	// Before any plan: no changes → Synced.
	h := p.Health()
	if h.Sync != "Synced" {
		t.Errorf("initial sync: want Synced, got %s", h.Sync)
	}
	if h.Health != "Healthy" {
		t.Errorf("initial health: want Healthy, got %s", h.Health)
	}

	// Simulate a plan with creates.
	p.mu.Lock()
	p.lastPlanSummary.Creates = 3
	p.mu.Unlock()

	h2 := p.Health()
	if h2.Sync != "OutOfSync" {
		t.Errorf("after creates: want OutOfSync, got %s", h2.Sync)
	}

	// Simulate errors.
	p.mu.Lock()
	p.lastErrors = []string{"session x failed"}
	p.mu.Unlock()

	h3 := p.Health()
	if h3.Health != "Degraded" {
		t.Errorf("after errors: want Degraded, got %s", h3.Health)
	}
}

// TestParserSystemReminderStripped verifies that <system-reminder> blocks
// are stripped from user turns and do not produce empty turns.
func TestParserSystemReminderStripped(t *testing.T) {
	sysReminder := "<system-reminder>This is a system note.</system-reminder>"
	realText := "what should I do next?"

	// A user record where content is a text block with a system-reminder prefix.
	content, _ := json.Marshal([]map[string]string{
		{"type": "text", "text": sysReminder + "\n" + realText},
	})
	msg, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": json.RawMessage(content),
	})
	rec := map[string]any{
		"type":       "user",
		"uuid":       "test-uuid",
		"parentUuid": "",
		"sessionId":  "test-session",
		"timestamp":  "2026-05-01T10:00:00Z",
		"message":    json.RawMessage(msg),
		"userType":   "external",
		"entrypoint": "cli",
	}
	b, _ := json.Marshal(rec)
	jsonl := string(b) + "\n"

	var meta SessionMeta
	var turns []Turn
	err := ParseSession(strings.NewReader(jsonl), "test-session", 8192, &meta, func(t Turn) bool {
		turns = append(turns, t)
		return true
	})
	if err != nil {
		t.Fatalf("ParseSession: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	if strings.Contains(turns[0].Text, "system-reminder") {
		t.Errorf("system-reminder not stripped: %q", turns[0].Text)
	}
	if !strings.Contains(turns[0].Text, realText) {
		t.Errorf("real text missing from turn: %q", turns[0].Text)
	}
}

// TestParserSkipsToolResults verifies that tool_result content blocks in
// user records are skipped (they are tool outputs, not operator utterances).
func TestParserSkipsToolResults(t *testing.T) {
	content, _ := json.Marshal([]map[string]any{
		{"type": "tool_result", "tool_use_id": "toolu_abc", "content": "tool output"},
		{"type": "text", "text": "here is the real question"},
	})
	msg, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": json.RawMessage(content),
	})
	rec := map[string]any{
		"type":       "user",
		"uuid":       "test-uuid-2",
		"parentUuid": "",
		"sessionId":  "test-session-2",
		"timestamp":  "2026-05-01T10:00:00Z",
		"message":    json.RawMessage(msg),
		"userType":   "external",
		"entrypoint": "cli",
	}
	b, _ := json.Marshal(rec)
	jsonl := string(b) + "\n"

	var meta SessionMeta
	var turns []Turn
	_ = ParseSession(strings.NewReader(jsonl), "test-session-2", 8192, &meta, func(t Turn) bool {
		turns = append(turns, t)
		return true
	})
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	if strings.Contains(turns[0].Text, "tool output") {
		t.Error("tool_result content leaked into turn text")
	}
	if !strings.Contains(turns[0].Text, "real question") {
		t.Errorf("real text missing: %q", turns[0].Text)
	}
}

// ─── Fixture helpers ─────────────────────────────────────────────────────────

// writeObservatoryConfig writes a minimal .cog/config/observatory.yaml that
// points at srcDirs.
func writeObservatoryConfig(t *testing.T, root string, srcDirs []string) {
	t.Helper()
	cfgDir := filepath.Join(root, ".cog", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}

	var lines []string
	lines = append(lines, "source_dirs:")
	for _, d := range srcDirs {
		lines = append(lines, "  - "+d)
	}
	lines = append(lines, "include_patterns:")
	lines = append(lines, "  - \"*.jsonl\"")

	content := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(cfgDir, "observatory.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write observatory.yaml: %v", err)
	}
}

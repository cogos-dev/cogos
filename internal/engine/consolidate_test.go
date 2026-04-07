package engine

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestConsolidationProducesSummaryCogBlocks(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	action := ConsolidationAction{WorkspaceRoot: root, MaxEvents: 40}

	sessionA := testSessionID(t, "alpha")
	sessionB := testSessionID(t, "beta")
	base := mustTime(t, "2026-01-01T00:00:00Z")

	for i := 0; i < 11; i++ {
		appendFixtureEvent(t, root, sessionA, "thread-a", base.Add(time.Duration(i)*time.Minute), "http", fmt.Sprintf("memory topic alpha %d", i))
	}
	for i := 0; i < 12; i++ {
		appendFixtureEvent(t, root, sessionB, "thread-b", base.Add(time.Duration(i+20)*time.Minute), "cli", fmt.Sprintf("memory topic beta %d", i))
	}

	count, err := action.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if count != 2 {
		t.Fatalf("consolidated session count = %d; want 2", count)
	}

	if len(findConsolidationEvents(t, root, sessionA)) != 1 {
		t.Fatalf("session %s should have exactly one consolidation summary", sessionA)
	}
	if len(findConsolidationEvents(t, root, sessionB)) != 1 {
		t.Fatalf("session %s should have exactly one consolidation summary", sessionB)
	}
	if got, err := filepath.Glob(filepath.Join(root, ".cog", "ledger", "*", "events.jsonl")); err != nil || len(got) == 0 {
		t.Fatalf("expected ledger fixture to exist, glob=%v err=%v", got, err)
	}
}

func TestConsolidationSkipsGroupsBelowThreshold(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	action := ConsolidationAction{WorkspaceRoot: root, MaxEvents: 30}

	sessionID := testSessionID(t, "split")
	base := mustTime(t, "2026-02-01T00:00:00Z")

	for i := 0; i < 6; i++ {
		appendFixtureEvent(t, root, sessionID, "thread-a", base.Add(time.Duration(i)*time.Minute), "http", fmt.Sprintf("focus thread a %d", i))
	}
	for i := 0; i < 6; i++ {
		appendFixtureEvent(t, root, sessionID, "thread-b", base.Add(time.Duration(i+10)*time.Minute), "http", fmt.Sprintf("focus thread b %d", i))
	}

	count, err := action.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if count != 0 {
		t.Fatalf("consolidated session count = %d; want 0", count)
	}
	if len(findConsolidationEvents(t, root, sessionID)) != 0 {
		t.Fatal("expected no consolidation summaries for groups under threshold")
	}
}

func TestConsolidationSummaryIncludesMetadata(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	action := ConsolidationAction{WorkspaceRoot: root, MaxEvents: 20}

	sessionID := testSessionID(t, "metadata")
	threadID := "thread-meta"
	base := mustTime(t, "2026-03-01T12:00:00Z")

	for i := 0; i < 11; i++ {
		source := "http"
		if i%2 == 1 {
			source = "kernel-v3"
		}
		appendFixtureEvent(t, root, sessionID, threadID, base.Add(time.Duration(i)*time.Minute), source, fmt.Sprintf("memory archive planning planning %d", i))
	}

	count, err := action.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if count != 1 {
		t.Fatalf("consolidated session count = %d; want 1", count)
	}

	consolidations := findConsolidationEvents(t, root, sessionID)
	if len(consolidations) != 1 {
		t.Fatalf("consolidation events len = %d; want 1", len(consolidations))
	}

	data := consolidations[0].HashedPayload.Data
	if got, _ := data["kind"].(string); got != string(BlockSystemEvent) {
		t.Fatalf("kind = %q; want %q", got, BlockSystemEvent)
	}
	if got, _ := data["provenance_normalized_by"].(string); got != "consolidation" {
		t.Fatalf("provenance_normalized_by = %q; want consolidation", got)
	}

	summary, ok := data["summary"].(map[string]interface{})
	if !ok {
		t.Fatal("summary payload missing")
	}
	if got, _ := summary["first_timestamp"].(string); got != "2026-03-01T12:00:00Z" {
		t.Fatalf("first_timestamp = %q; want 2026-03-01T12:00:00Z", got)
	}
	if got, _ := summary["last_timestamp"].(string); got != "2026-03-01T12:10:00Z" {
		t.Fatalf("last_timestamp = %q; want 2026-03-01T12:10:00Z", got)
	}
	if got, _ := summary["event_count"].(float64); got != 11 {
		t.Fatalf("event_count = %v; want 11", got)
	}
	if got, _ := summary["thread_id"].(string); got != threadID {
		t.Fatalf("thread_id = %q; want %q", got, threadID)
	}

	uniqueSources := stringSlice(summary["unique_sources"])
	sort.Strings(uniqueSources)
	if strings.Join(uniqueSources, ",") != "http,kernel-v3" {
		t.Fatalf("unique_sources = %v; want [http kernel-v3]", uniqueSources)
	}

	keyTopics := stringSlice(summary["key_topics"])
	if len(keyTopics) == 0 || keyTopics[0] != "planning" {
		t.Fatalf("key_topics = %v; want planning ranked first", keyTopics)
	}
}

func TestArchivedSessionsRecordedAfterConsolidation(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	action := ConsolidationAction{WorkspaceRoot: root, MaxEvents: 40}

	sessionA := testSessionID(t, "archive-a")
	sessionB := testSessionID(t, "archive-b")
	base := mustTime(t, "2026-03-15T08:00:00Z")

	for i := 0; i < 11; i++ {
		appendFixtureEvent(t, root, sessionA, "thread-a", base.Add(time.Duration(i)*time.Minute), "http", fmt.Sprintf("archive alpha %d", i))
	}
	for i := 0; i < 12; i++ {
		appendFixtureEvent(t, root, sessionB, "thread-b", base.Add(time.Duration(i+20)*time.Minute), "cli", fmt.Sprintf("archive beta %d", i))
	}

	if _, err := action.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	archived, err := ArchivedSessions(root, sessionA)
	if err != nil {
		t.Fatalf("ArchivedSessions: %v", err)
	}
	if _, ok := archived[sessionA]; !ok {
		t.Fatalf("archived sessions missing %s", sessionA)
	}
	if _, ok := archived[sessionB]; !ok {
		t.Fatalf("archived sessions missing %s", sessionB)
	}
}

func TestHeartbeatTriggersDormantConsolidation(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.ConsolidationInterval = 3600
	p := NewProcess(cfg, makeNucleus("Cog", "tester"))
	p.lastConsolidation = time.Now().Add(-2 * time.Hour)

	sessionID := testSessionID(t, "heartbeat")
	base := mustTime(t, "2026-04-01T09:00:00Z")
	for i := 0; i < 11; i++ {
		appendFixtureEvent(t, root, sessionID, "thread-heartbeat", base.Add(time.Duration(i)*time.Minute), "http", fmt.Sprintf("sleep memory rehearsal %d", i))
	}

	p.emitHeartbeat()

	if len(findConsolidationEvents(t, root, sessionID)) != 1 {
		t.Fatal("expected heartbeat to trigger one consolidation summary")
	}
}

func appendFixtureEvent(t *testing.T, root, sessionID, threadID string, timestamp time.Time, source, content string) {
	t.Helper()
	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      "user.message",
			Timestamp: timestamp.UTC().Format(time.RFC3339),
			SessionID: sessionID,
			Data: map[string]interface{}{
				"thread_id": threadID,
				"content":   content,
			},
		},
		Metadata: EventMetadata{Source: source},
	}
	if err := AppendEvent(root, sessionID, env); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

func findConsolidationEvents(t *testing.T, root, sessionID string) []EventEnvelope {
	t.Helper()
	var summaries []EventEnvelope
	for _, event := range mustReadAllEvents(t, root, sessionID) {
		if event.HashedPayload.Type == "memory.consolidation" {
			summaries = append(summaries, event)
		}
	}
	return summaries
}

func stringSlice(value interface{}) []string {
	items, _ := value.([]interface{})
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func testSessionID(t *testing.T, suffix string) string {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "-")
	return strings.ToLower(name + "-" + suffix)
}

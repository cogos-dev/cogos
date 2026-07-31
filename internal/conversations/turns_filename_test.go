// turns_filename_test.go — #489: session keys reaching turnsFilename must
// not carry NTFS-illegal characters (a colon in particular) onto disk.
package conversations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTurnsFilename_ColonKey(t *testing.T) {
	got := turnsFilename("http:cog")
	if strings.Contains(got, ":") {
		t.Fatalf("turnsFilename(%q) = %q, still contains a colon", "http:cog", got)
	}
	want := "http%3Acog.json"
	if got != want {
		t.Fatalf("turnsFilename(%q) = %q, want %q", "http:cog", got, want)
	}
}

// TestTurnsFilename_SlashConventionPreserved locks in the pre-existing
// composite-key behavior ("<source>/<session_id>" -> "__"-joined flat file)
// so the #489 fix (routing the result through pathsafe.SanitizeComponent)
// doesn't regress it.
func TestTurnsFilename_SlashConventionPreserved(t *testing.T) {
	got := turnsFilename("claude-code/abc-123")
	want := "claude-code__abc-123.json"
	if got != want {
		t.Fatalf("turnsFilename(%q) = %q, want %q", "claude-code/abc-123", got, want)
	}
}

// TestTurnsFilename_ColonAndSlashCombined covers a composite key whose
// session-id half also needs NTFS sanitizing (e.g. a channel-scoped source
// key like "http:cog" ingested under a "channel/" source prefix).
func TestTurnsFilename_ColonAndSlashCombined(t *testing.T) {
	got := turnsFilename("channel/http:cog")
	if strings.Contains(got, ":") {
		t.Fatalf("turnsFilename(%q) = %q, still contains a colon", "channel/http:cog", got)
	}
	if strings.Contains(got, "/") {
		t.Fatalf("turnsFilename(%q) = %q, still contains a slash (would create a subdirectory)", "channel/http:cog", got)
	}
}

// TestUpsertSessionColonSessionID drives the real Index.UpsertSession /
// Index.GetTurn path (not just the filename helper) with a colon-bearing
// session ID end-to-end, and asserts the projection file that lands on disk
// is NTFS-legal.
func TestUpsertSessionColonSessionID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	sessionID := "http:cog"
	now := time.Now()
	meta := SessionMeta{SessionID: sessionID, TurnCount: 1, FirstTurnAt: now, LastTurnAt: now}
	turns := []Turn{{UUID: sessionID, SessionID: sessionID, TurnIndex: 0, Role: "user", Timestamp: now, Text: "hello"}}

	if err := idx.UpsertSession(meta, turns); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	got, ok := idx.GetTurn(sessionID, 0)
	if !ok {
		t.Fatalf("GetTurn(%q, 0) not found", sessionID)
	}
	if got.Text != "hello" {
		t.Errorf("Text = %q, want %q", got.Text, "hello")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ":") {
			t.Errorf("projection dir entry %q contains a colon", e.Name())
		}
		if e.Name() == filepath.Base(idx.turnsPath(sessionID)) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected turns file %q among projDir entries %v", filepath.Base(idx.turnsPath(sessionID)), entries)
	}
}

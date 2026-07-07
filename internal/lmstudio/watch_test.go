// watch_test.go — unit tests for conversation-file discovery and the
// emitted-state drift tracking that makes repeated Run() calls incremental.
package lmstudio

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoverConversationFiles_MissingRootIsNotError(t *testing.T) {
	files, err := DiscoverConversationFiles(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("DiscoverConversationFiles on missing root: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("len(files) = %d, want 0", len(files))
	}
}

func TestDiscoverConversationFiles_FindsNestedConversationJSON(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "Folder A", "111.conversation.json")
	writeFixture(t, root, "Folder B", "222.conversation.json")

	// Non-matching files must be ignored.
	if err := os.WriteFile(filepath.Join(root, "Folder A", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "Folder A", "other.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write decoy json: %v", err)
	}

	files, err := DiscoverConversationFiles(root)
	if err != nil {
		t.Fatalf("DiscoverConversationFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2; files=%+v", len(files), files)
	}
	for _, f := range files {
		if filepath.Ext(f.Path) != ".json" || !hasConversationSuffix(f.Path) {
			t.Errorf("discovered non-conversation file: %s", f.Path)
		}
	}
}

func TestEmittedState_NeedsEmit(t *testing.T) {
	st := &EmittedState{Files: make(map[string]ConversationFile)}
	f := ConversationFile{Path: "/a/b.conversation.json", Size: 100, Mtime: time.Now()}

	if !st.NeedsEmit(f) {
		t.Error("new file should need emit")
	}
	st.Record(f)
	if st.NeedsEmit(f) {
		t.Error("unchanged file should not need emit after Record")
	}

	grown := f
	grown.Size = 200
	if !st.NeedsEmit(grown) {
		t.Error("size change should need emit")
	}

	touched := f
	touched.Mtime = f.Mtime.Add(10 * time.Second)
	if !st.NeedsEmit(touched) {
		t.Error("mtime drift beyond tolerance should need emit")
	}

	jitter := f
	jitter.Mtime = f.Mtime.Add(1 * time.Second)
	if st.NeedsEmit(jitter) {
		t.Error("mtime jitter within 2s tolerance should NOT need emit")
	}
}

func TestEmittedState_SaveAndLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state", "emitted.json")

	st := &EmittedState{Files: make(map[string]ConversationFile)}
	f := ConversationFile{Path: "/a/b.conversation.json", Size: 42, Mtime: time.Now().Truncate(time.Second)}
	st.Record(f)

	if err := st.Save(statePath); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadEmittedState(statePath)
	if err != nil {
		t.Fatalf("LoadEmittedState: %v", err)
	}
	got, ok := loaded.Files[f.Path]
	if !ok {
		t.Fatalf("loaded state missing recorded file %s", f.Path)
	}
	if got.Size != f.Size {
		t.Errorf("loaded size = %d, want %d", got.Size, f.Size)
	}
	if !got.Mtime.Equal(f.Mtime) {
		t.Errorf("loaded mtime = %v, want %v", got.Mtime, f.Mtime)
	}
}

func TestLoadEmittedState_MissingFileIsEmpty(t *testing.T) {
	st, err := LoadEmittedState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("LoadEmittedState missing file: %v", err)
	}
	if len(st.Files) != 0 {
		t.Errorf("expected empty state, got %d entries", len(st.Files))
	}
}

func TestDefaultLMStudioDir_HonorsEnvOverride(t *testing.T) {
	t.Setenv("LMSTUDIO_CONVERSATIONS_DIR", "/custom/path")
	got, err := DefaultLMStudioDir()
	if err != nil {
		t.Fatalf("DefaultLMStudioDir: %v", err)
	}
	if got != "/custom/path" {
		t.Errorf("DefaultLMStudioDir() = %q, want /custom/path", got)
	}
}

func TestDefaultLMStudioDir_FallsBackToHome(t *testing.T) {
	t.Setenv("LMSTUDIO_CONVERSATIONS_DIR", "")
	got, err := DefaultLMStudioDir()
	if err != nil {
		t.Fatalf("DefaultLMStudioDir: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".lmstudio", "conversations")
	if got != want {
		t.Errorf("DefaultLMStudioDir() = %q, want %q", got, want)
	}
}

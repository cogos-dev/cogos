// watch.go — discovers LM Studio conversation files and tracks which have
// already been emitted, so repeated runs are incremental (only re-parse
// files that are new or have changed size/mtime since the last run).
//
// This mirrors the drift-detection approach in
// internal/conversations.isDrift (size is the definitive signal; mtime with
// a small tolerance is the secondary signal), but state is tracked
// independently in this package's own state file — this observer does not
// read or write the observatory's own index/state.
package lmstudio

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DefaultLMStudioDir returns the default LM Studio conversations directory:
// $HOME/.lmstudio/conversations. Honors the LMSTUDIO_CONVERSATIONS_DIR
// environment variable when set, so the path is configurable without code
// changes (and never hardcodes a specific user's home directory).
func DefaultLMStudioDir() (string, error) {
	if dir := os.Getenv("LMSTUDIO_CONVERSATIONS_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("lmstudio: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".lmstudio", "conversations"), nil
}

// ConversationFile describes one discovered *.conversation.json file.
type ConversationFile struct {
	Path  string    `json:"path"`
	Mtime time.Time `json:"mtime"`
	Size  int64     `json:"size"`
}

// DiscoverConversationFiles walks root recursively and returns every file
// matching *.conversation.json, sorted by path for deterministic output.
// A missing root is not an error — LM Studio may not be installed, or the
// directory may not exist yet on a fresh machine — it returns an empty slice.
func DiscoverConversationFiles(root string) ([]ConversationFile, error) {
	var out []ConversationFile

	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("lmstudio: stat %s: %w", root, err)
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable entries rather than aborting the whole walk.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".json" || !hasConversationSuffix(path) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		out = append(out, ConversationFile{
			Path:  path,
			Mtime: info.ModTime(),
			Size:  info.Size(),
		})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("lmstudio: walk %s: %w", root, walkErr)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// hasConversationSuffix reports whether path ends in ".conversation.json".
func hasConversationSuffix(path string) bool {
	const suffix = ".conversation.json"
	if len(path) < len(suffix) {
		return false
	}
	return path[len(path)-len(suffix):] == suffix
}

// ─── Emitted-state tracking ─────────────────────────────────────────────────

// EmittedState records, per source file, the (size, mtime) that was last
// successfully emitted. Used to skip unchanged files on subsequent runs.
type EmittedState struct {
	// Files maps absolute conversation file path -> last-emitted snapshot.
	Files map[string]ConversationFile `json:"files"`
}

// LoadEmittedState reads the state file at path. A missing file is not an
// error — returns an empty state (first run).
func LoadEmittedState(path string) (*EmittedState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &EmittedState{Files: make(map[string]ConversationFile)}, nil
		}
		return nil, fmt.Errorf("lmstudio: read state %s: %w", path, err)
	}
	var st EmittedState
	if jsonErr := json.Unmarshal(data, &st); jsonErr != nil {
		return nil, fmt.Errorf("lmstudio: parse state %s: %w", path, jsonErr)
	}
	if st.Files == nil {
		st.Files = make(map[string]ConversationFile)
	}
	return &st, nil
}

// Save writes the state atomically (write to temp file, then rename) so a
// crash mid-write never leaves a truncated/corrupt state file.
func (st *EmittedState) Save(path string) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("lmstudio: marshal state: %w", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		return fmt.Errorf("lmstudio: mkdir for state %s: %w", path, mkErr)
	}
	tmp := path + ".tmp"
	if wErr := os.WriteFile(tmp, data, 0o644); wErr != nil {
		return fmt.Errorf("lmstudio: write temp state %s: %w", tmp, wErr)
	}
	if rErr := os.Rename(tmp, path); rErr != nil {
		return fmt.Errorf("lmstudio: rename state into place %s: %w", path, rErr)
	}
	return nil
}

// NeedsEmit reports whether f is new or has drifted (size or mtime changed
// beyond a 2s tolerance, mirroring internal/conversations.isDrift) since the
// last recorded emission.
func (st *EmittedState) NeedsEmit(f ConversationFile) bool {
	prev, ok := st.Files[f.Path]
	if !ok {
		return true
	}
	if prev.Size != f.Size {
		return true
	}
	diff := prev.Mtime.Sub(f.Mtime)
	if diff < 0 {
		diff = -diff
	}
	return diff > 2*time.Second
}

// Record marks f as emitted at its current (size, mtime).
func (st *EmittedState) Record(f ConversationFile) {
	if st.Files == nil {
		st.Files = make(map[string]ConversationFile)
	}
	st.Files[f.Path] = f
}

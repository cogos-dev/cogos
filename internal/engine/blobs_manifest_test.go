package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestBlobsManifestCommand exercises the manifest builder that backs the
// `cogos blobs manifest <model-dir>` CLI subcommand. It writes two files into a
// temp model dir, builds the manifest, and asserts the shard count, the
// content-addressed hashes, and the model_id default.
func TestBlobsManifestCommand(t *testing.T) {
	dir := t.TempDir()

	files := map[string][]byte{
		"model-00001-of-00002.safetensors": []byte("shard one bytes"),
		"config.json":                      []byte(`{"hidden_size": 4096}`),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	manifest, err := buildModelManifest(dir, "")
	if err != nil {
		t.Fatalf("buildModelManifest: %v", err)
	}

	// Type field is the cross-node contract marker.
	if manifest.Type != "model.manifest" {
		t.Errorf("type = %q, want %q", manifest.Type, "model.manifest")
	}

	// model_id defaults to the basename of the dir.
	wantID := filepath.Base(dir)
	if manifest.ModelID != wantID {
		t.Errorf("model_id = %q, want %q", manifest.ModelID, wantID)
	}

	// Two regular files → two shards.
	if len(manifest.Shards) != 2 {
		t.Fatalf("shards = %d, want 2", len(manifest.Shards))
	}

	// Each shard hash must equal the sha256 of the file content, and size/path
	// must match.
	byPath := map[string]ManifestShard{}
	for _, s := range manifest.Shards {
		byPath[s.Path] = s
	}
	for name, content := range files {
		s, ok := byPath[name]
		if !ok {
			t.Errorf("missing shard for %s", name)
			continue
		}
		sum := sha256.Sum256(content)
		wantHash := hex.EncodeToString(sum[:])
		if s.Hash != wantHash {
			t.Errorf("%s hash = %q, want %q", name, s.Hash, wantHash)
		}
		if len(s.Hash) != 64 {
			t.Errorf("%s hash length = %d, want 64", name, len(s.Hash))
		}
		if s.Size != int64(len(content)) {
			t.Errorf("%s size = %d, want %d", name, s.Size, len(content))
		}
	}
}

// TestBlobsManifestModelIDOverride confirms the --model-id override path.
func TestBlobsManifestModelIDOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	manifest, err := buildModelManifest(dir, "llama-3-8b")
	if err != nil {
		t.Fatalf("buildModelManifest: %v", err)
	}
	if manifest.ModelID != "llama-3-8b" {
		t.Errorf("model_id = %q, want %q", manifest.ModelID, "llama-3-8b")
	}
}

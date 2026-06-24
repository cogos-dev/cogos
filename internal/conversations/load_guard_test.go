package conversations

import (
	"os"
	"testing"
)

// TestLoadIfChanged_SkipsWhenSoleWriter verifies the common case: after this
// process writes the index (UpsertSession), LoadIfChanged sees no external
// change and skips the expensive full reload while keeping the in-memory state.
func TestLoadIfChanged_SkipsWhenSoleWriter(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertSession(
		SessionMeta{SessionID: "s1"},
		[]Turn{{UUID: "u1", SessionID: "s1", Text: "hi"}},
	); err != nil {
		t.Fatal(err)
	}

	reloaded, err := idx.LoadIfChanged()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded {
		t.Error("LoadIfChanged reloaded despite no external write (sole-writer case)")
	}
	if _, ok := idx.GetMeta("s1"); !ok {
		t.Error("session s1 missing after skipped reload")
	}
}

// TestLoadIfChanged_ReloadsOnExternalWrite verifies correctness is preserved: a
// second Index over the same directory (standing in for the cog CLI) appends a
// session, changing _meta.json on disk; LoadIfChanged must detect that and
// reload so the new session is picked up.
func TestLoadIfChanged_ReloadsOnExternalWrite(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertSession(SessionMeta{SessionID: "s1"}, []Turn{{UUID: "u1", SessionID: "s1"}}); err != nil {
		t.Fatal(err)
	}

	// External writer over the same projection directory.
	other, err := NewIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Load(); err != nil {
		t.Fatal(err)
	}
	if err := other.UpsertSession(SessionMeta{SessionID: "s2"}, []Turn{{UUID: "u2", SessionID: "s2"}}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := idx.LoadIfChanged()
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded {
		t.Fatal("LoadIfChanged did not reload after an external write")
	}
	if _, ok := idx.GetMeta("s2"); !ok {
		t.Error("externally-added session s2 not picked up after reload")
	}
}

// TestLoadIfChanged_ReloadsOnSameSizeSameMtimeRewrite is the adversarial case: an
// external in-place rewrite that keeps _meta.json the same byte size AND the same
// mtime. A (mtime, size)-only guard would wrongly skip the reload; the content
// hash must still catch it.
func TestLoadIfChanged_ReloadsOnSameSizeSameMtimeRewrite(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertSession(SessionMeta{SessionID: "s1"}, []Turn{{UUID: "u1", SessionID: "s1", Text: "aaaa"}}); err != nil {
		t.Fatal(err)
	}

	metaPath := idx.metaPath()
	orig, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	// Same length, different content: flip one byte.
	mutated := append([]byte(nil), orig...)
	mutated[len(mutated)/2] ^= 0x20
	if len(mutated) != len(orig) {
		t.Fatalf("mutation changed length (%d != %d)", len(mutated), len(orig))
	}
	if err := os.WriteFile(metaPath, mutated, 0o644); err != nil {
		t.Fatal(err)
	}

	// Force the mtime back to what the index recorded, defeating the (mtime,size)
	// pre-filter so only the content hash can detect the change.
	idx.mu.RLock()
	recorded := idx.lastMetaMtime
	idx.mu.RUnlock()
	if err := os.Chtimes(metaPath, recorded, recorded); err != nil {
		t.Fatal(err)
	}

	reloaded, err := idx.LoadIfChanged()
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded {
		t.Error("missed a same-size, same-mtime in-place rewrite — content-hash tiebreaker failed")
	}
}

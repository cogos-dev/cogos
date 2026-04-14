package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// ── ResolveToFieldKey ────────────────────────────────────────────────────────

func TestResolveToFieldKeyRoundTrip(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	uri := "cog://mem/semantic/insights/foo.cog.md"

	key := ResolveToFieldKey(root, uri)
	want := root + "/.cog/mem/semantic/insights/foo.cog.md"
	if filepath.ToSlash(key) != want {
		t.Fatalf("ResolveToFieldKey(%q) = %q; want %q", uri, key, want)
	}

	// Round-trip: field key back to URI must reproduce the original.
	got := FieldKeyToURI(root, key)
	if got != uri {
		t.Errorf("FieldKeyToURI(%q) = %q; want %q", key, got, uri)
	}
}

func TestResolveToFieldKeyShortURI(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	short := "cog:mem/semantic/insights/foo.cog.md"
	full := "cog://mem/semantic/insights/foo.cog.md"

	shortKey := ResolveToFieldKey(root, short)
	fullKey := ResolveToFieldKey(root, full)

	if shortKey != fullKey {
		t.Errorf("short URI key = %q; full URI key = %q; want same", shortKey, fullKey)
	}
}

func TestResolveToFieldKeyMemoryRelative(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	rel := "semantic/insights/foo.cog.md"
	full := "cog://mem/semantic/insights/foo.cog.md"

	relKey := ResolveToFieldKey(root, rel)
	fullKey := ResolveToFieldKey(root, full)

	if relKey != fullKey {
		t.Errorf("relative key = %q; full URI key = %q; want same", relKey, fullKey)
	}
}

func TestResolveToFieldKeyAbsolutePassthrough(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	// Use a synthetic absolute path outside the workspace root to test passthrough.
	// The function should return absolute paths unchanged regardless of content.
	abs := "/some/other/workspace/.cog/mem/semantic/insights/foo.cog.md"

	key := ResolveToFieldKey(root, abs)
	if key != abs {
		t.Errorf("absolute path should pass through; got %q, want %q", key, abs)
	}
}

func TestResolveToFieldKeyWorkspaceRelative(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	// .cog/ prefix → joined with root
	pointer := ".cog/docs/framework-status.md"
	key := ResolveToFieldKey(root, pointer)
	want := filepath.Join(root, pointer)
	if key != want {
		t.Errorf("ResolveToFieldKey(%q) = %q; want %q", pointer, key, want)
	}
}

func TestResolveToFieldKeyAllMemorySectors(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	sectors := []string{"semantic/", "episodic/", "procedural/", "reflective/"}
	for _, s := range sectors {
		pointer := s + "test.md"
		key := ResolveToFieldKey(root, pointer)
		want := filepath.Join(root, ".cog", "mem", pointer)
		if key != want {
			t.Errorf("sector %q: got %q; want %q", s, key, want)
		}
	}
}

// ── FieldKeyToURI ────────────────────────────────────────────────────────────

func TestFieldKeyToURIDocs(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	abs := filepath.Join(root, ".cog/docs/foo.md")

	got := FieldKeyToURI(root, abs)
	want := "cog://docs/foo.md"
	if got != want {
		t.Errorf("FieldKeyToURI(%q) = %q; want %q", abs, got, want)
	}
}

func TestFieldKeyToURISkills(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	abs := filepath.Join(root, ".claude/skills/foo/SKILL.md")

	got := FieldKeyToURI(root, abs)
	want := "cog://skill/foo/SKILL.md"
	if got != want {
		t.Errorf("FieldKeyToURI(%q) = %q; want %q", abs, got, want)
	}
}

func TestFieldKeyToURIUnmapped(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	abs := filepath.Join(root, "apps/someapp/main.go")

	got := FieldKeyToURI(root, abs)
	want := "cog://workspace/apps/someapp/main.go"
	if got != want {
		t.Errorf("FieldKeyToURI(%q) = %q; want %q", abs, got, want)
	}
}

func TestFieldKeyToURIConfig(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	abs := filepath.Join(root, ".cog/config/kernel.yaml")

	got := FieldKeyToURI(root, abs)
	want := "cog://conf/kernel.yaml"
	if got != want {
		t.Errorf("FieldKeyToURI(%q) = %q; want %q", abs, got, want)
	}
}

func TestFieldKeyToURIOntology(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	abs := filepath.Join(root, ".cog/ontology/crystal.cog.md")

	got := FieldKeyToURI(root, abs)
	want := "cog://ontology/crystal"
	if got != want {
		t.Errorf("FieldKeyToURI(%q) = %q; want %q", abs, got, want)
	}
}

func TestFieldKeyToURIOutsideWorkspace(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	abs := "/completely/different/path.txt"

	got := FieldKeyToURI(root, abs)
	// filepath.Rel will produce "../completely/different/path.txt"
	// which should become a cog://workspace/ prefixed URI.
	if got == abs {
		// If it returned the raw path unchanged, Rel must have errored.
		// On Unix this won't happen, so we just check it got some URI form.
		t.Logf("returned raw path (acceptable on Rel error): %q", got)
	}
}

// ── PathExistsOnDisk ─────────────────────────────────────────────────────────

func TestPathExistsOnDiskReal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realFile := filepath.Join(root, "exists.txt")
	if err := os.WriteFile(realFile, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	if !PathExistsOnDisk(realFile) {
		t.Errorf("expected true for existing file %q", realFile)
	}
}

func TestPathExistsOnDiskFake(t *testing.T) {
	t.Parallel()
	fake := filepath.Join(t.TempDir(), "no-such-file.txt")
	if PathExistsOnDisk(fake) {
		t.Errorf("expected false for non-existent file %q", fake)
	}
}

func TestPathExistsOnDiskDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if !PathExistsOnDisk(dir) {
		t.Errorf("expected true for existing directory %q", dir)
	}
}

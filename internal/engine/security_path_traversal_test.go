// security_path_traversal_test.go — regression coverage for the path-traversal
// holes closed in the "security: close path-traversal holes #396 missed" change.
package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFileForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSearchMemoryGrep_SectorContainment verifies the grep fallback rejects a
// caller-supplied sector that escapes the memory root, while allowing a normal
// sector.
func TestSearchMemoryGrep_SectorContainment(t *testing.T) {
	root := t.TempDir()
	writeFileForTest(t, filepath.Join(root, ".cog", "mem", "semantic", "a.md"), "hello secret")
	writeFileForTest(t, filepath.Join(root, "outside", "b.md"), "hello secret")

	if _, err := searchMemoryGrep(root, "secret", 10, "semantic"); err != nil {
		t.Fatalf("valid sector errored: %v", err)
	}
	for _, bad := range []string{"../../outside", "../outside", "../../../etc", "a/../../outside"} {
		if _, err := searchMemoryGrep(root, "secret", 10, bad); err == nil {
			t.Errorf("searchMemoryGrep sector %q: expected containment error, got nil", bad)
		}
	}
}

// TestBuildMemoryIndexFromFS_SectorContainment is the same guard on the index
// fallback path.
func TestBuildMemoryIndexFromFS_SectorContainment(t *testing.T) {
	root := t.TempDir()
	writeFileForTest(t, filepath.Join(root, ".cog", "mem", "episodic", "a.md"), "x")
	writeFileForTest(t, filepath.Join(root, "outside", "b.md"), "x")

	if _, err := buildMemoryIndexFromFS(root, "episodic"); err != nil {
		t.Fatalf("valid sector errored: %v", err)
	}
	for _, bad := range []string{"../../outside", "../../../etc"} {
		if _, err := buildMemoryIndexFromFS(root, bad); err == nil {
			t.Errorf("buildMemoryIndexFromFS sector %q: expected containment error, got nil", bad)
		}
	}
}

func TestValidClaudeProjectName(t *testing.T) {
	for _, ok := range []string{"my-project", "-Users-foo-bar", "abc123", "a.b-c"} {
		if !validClaudeProjectName(ok) {
			t.Errorf("validClaudeProjectName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"..", "../x", "a/b", "../../tmp", "x/..", "/abs"} {
		if validClaudeProjectName(bad) {
			t.Errorf("validClaudeProjectName(%q) = true, want false", bad)
		}
	}
}

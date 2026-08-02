//go:build !windows

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NOT t.Parallel: these tests point $HOME at a t.TempDir, and $HOME is process-
// global. Combining t.Parallel with a global os.Setenv("HOME", …) leaked the
// fake home to every concurrently-running test — which stayed invisible only
// while nothing else resolved paths from $HOME. Node identity is now machine-
// anchored (see node_identity.go), so a concurrent test that boots a Process
// would materialize <tmp>/.cog/node inside this test's TempDir and break its
// cleanup. t.Setenv is the correct primitive and refuses to run under
// t.Parallel, which is exactly the constraint being honored here.
func TestAddCogBinToPathWritesRCLine(t *testing.T) {
	// Use a temp home with a pre-existing .bashrc so detectShellRC finds it.
	tmp := t.TempDir()
	bashrc := filepath.Join(tmp, ".bashrc")
	if err := os.WriteFile(bashrc, []byte("# existing\n"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", tmp)
	t.Setenv("SHELL", "/bin/bash")

	if err := addCogBinToPath(); err != nil {
		t.Fatalf("addCogBinToPath: %v", err)
	}

	content, err := os.ReadFile(bashrc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), ".cog/bin") {
		t.Errorf("expected .cog/bin in %s; got:\n%s", bashrc, string(content))
	}
}

func TestAddCogBinToPathIdempotent(t *testing.T) {
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, ".cog", "bin")
	bashrc := filepath.Join(tmp, ".bashrc")
	// Pre-seed with the export line.
	seed := "export PATH=\"" + binDir + ":$PATH\"\n"
	if err := os.WriteFile(bashrc, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", tmp)
	t.Setenv("SHELL", "/bin/bash")

	if err := addCogBinToPath(); err != nil {
		t.Fatalf("addCogBinToPath (second call): %v", err)
	}

	// File should be unchanged (no duplicate line added).
	content, err := os.ReadFile(bashrc)
	if err != nil {
		t.Fatal(err)
	}
	count := strings.Count(string(content), binDir)
	if count != 1 {
		t.Errorf("expected exactly 1 occurrence of binDir in %s; got %d:\n%s", bashrc, count, string(content))
	}
}

func TestDetectShellRCZsh(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHELL", "/bin/zsh")
	rc := detectShellRC()
	if !strings.HasSuffix(rc, ".zshrc") {
		t.Errorf("expected .zshrc; got %s", rc)
	}
}

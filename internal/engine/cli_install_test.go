//go:build !windows

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddCogBinToPathWritesRCLine(t *testing.T) {
	t.Parallel()

	// Use a temp home with a pre-existing .bashrc so detectShellRC finds it.
	tmp := t.TempDir()
	bashrc := filepath.Join(tmp, ".bashrc")
	if err := os.WriteFile(bashrc, []byte("# existing\n"), 0644); err != nil {
		t.Fatal(err)
	}

	origHome := os.Getenv("HOME")
	origShell := os.Getenv("SHELL")
	t.Cleanup(func() {
		os.Setenv("HOME", origHome)
		os.Setenv("SHELL", origShell)
	})
	os.Setenv("HOME", tmp)
	os.Setenv("SHELL", "/bin/bash")

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
	t.Parallel()

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, ".cog", "bin")
	bashrc := filepath.Join(tmp, ".bashrc")
	// Pre-seed with the export line.
	seed := "export PATH=\"" + binDir + ":$PATH\"\n"
	if err := os.WriteFile(bashrc, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	origHome := os.Getenv("HOME")
	origShell := os.Getenv("SHELL")
	t.Cleanup(func() {
		os.Setenv("HOME", origHome)
		os.Setenv("SHELL", origShell)
	})
	os.Setenv("HOME", tmp)
	os.Setenv("SHELL", "/bin/bash")

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
	orig := os.Getenv("HOME")
	origShell := os.Getenv("SHELL")
	t.Cleanup(func() {
		os.Setenv("HOME", orig)
		os.Setenv("SHELL", origShell)
	})
	os.Setenv("HOME", tmp)
	os.Setenv("SHELL", "/bin/zsh")
	rc := detectShellRC()
	if !strings.HasSuffix(rc, ".zshrc") {
		t.Errorf("expected .zshrc; got %s", rc)
	}
}

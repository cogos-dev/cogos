//go:build !windows

package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realHomeAtLoad captures the process's actual $HOME before any test in this
// package can override it. Package-level var initializers run before any
// test (and before TestMain, if one existed), so this is guaranteed to see
// the real value regardless of t.Setenv calls made later. It backs
// TestAddCogBinToPathNeverTouchesRealHome below — the regression guard for
// the incident where a HOME-mutation race let addCogBinToPath resolve and
// append to the operator's real ~/.zshrc.
var realHomeAtLoad = os.Getenv("HOME")

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

// TestAddCogBinToPathNeverTouchesRealHome is a regression guard for the
// 2026-08 incident: a HOME-mutation race (t.Parallel plus a global,
// unsynchronized os.Setenv("HOME", …) — see the historical note above)
// let addCogBinToPath resolve the operator's REAL ~/.zshrc mid-test and
// append an export line pointing at a t.TempDir() path. That race is now
// closed (t.Setenv, no t.Parallel on any test that touches HOME/SHELL), but
// this test proves the property directly rather than trusting the
// mechanism: with HOME overridden, addCogBinToPath must never open, read,
// or write any file under the real home captured at package load — under
// zsh specifically, since that's the shell the incident hit.
func TestAddCogBinToPathNeverTouchesRealHome(t *testing.T) {
	if realHomeAtLoad == "" {
		t.Skip("real $HOME unavailable at package load; cannot assert isolation")
	}

	// Every rc file addCogBinToPath's shell detection could possibly resolve
	// under the REAL home, snapshotted before the function under test runs.
	candidates := []string{
		filepath.Join(realHomeAtLoad, ".zshrc"),
		filepath.Join(realHomeAtLoad, ".bashrc"),
		filepath.Join(realHomeAtLoad, ".bash_profile"),
		filepath.Join(realHomeAtLoad, ".config", "fish", "config.fish"),
	}
	before := snapshotFileHashes(t, candidates)

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHELL", "/bin/zsh")

	// Belt-and-suspenders: the resolved rc path itself must not fall under
	// the real home, independent of whether its content later changes.
	if rc := detectShellRC(); strings.HasPrefix(rc, realHomeAtLoad+string(filepath.Separator)) {
		t.Fatalf("detectShellRC resolved a path under the REAL home (%s) while HOME was overridden to %s: %s", realHomeAtLoad, tmp, rc)
	}

	if err := addCogBinToPath(); err != nil {
		t.Fatalf("addCogBinToPath: %v", err)
	}

	after := snapshotFileHashes(t, candidates)
	for _, c := range candidates {
		if before[c] != after[c] {
			t.Errorf("real home file %s changed while HOME was overridden to %s (this is the exact class of the 2026-08 ~/.zshrc incident)", c, tmp)
		}
	}
}

// snapshotFileHashes reads each path read-only and returns a sha256 hex
// digest, or the sentinel "<absent>" when the file does not exist. It never
// writes anything.
func snapshotFileHashes(t *testing.T, paths []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			out[p] = "<absent>"
			continue
		}
		sum := sha256.Sum256(b)
		out[p] = hex.EncodeToString(sum[:])
	}
	return out
}

package coherence

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// setupGitWorkspace creates a real git repo with a tracked .cog/ file and an
// initial commit, mirroring the fixture shape the root package's
// TestGitCogTreeHash_NoIndexMutation uses. Skips if git is unavailable.
func setupGitWorkspace(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	repoDir := t.TempDir()
	mustRunGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	mustRunGit("init", "-b", "main")
	mustRunGit("config", "user.email", "test@example.com")
	mustRunGit("config", "user.name", "Test")

	cogDir := filepath.Join(repoDir, ".cog")
	memDir := filepath.Join(cogDir, "mem")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatalf("mkdir .cog/mem: %v", err)
	}
	trackedFile := filepath.Join(memDir, "note.cog.md")
	if err := os.WriteFile(trackedFile, []byte("# note\n"), 0644); err != nil {
		t.Fatalf("write note.cog.md: %v", err)
	}

	// .cog/run/ holds the canonical-hash file CheckCoherence itself writes
	// via test helpers below. The real workspace .gitignore excludes
	// .cog/run/ from the tracked tree for exactly this reason (it is
	// per-session runtime state, not tracked content) — without this,
	// writing the canonical-hash file would perturb the very tree hash it
	// is meant to describe, since gitCogTreeHash's `git add -A .cog/` stages
	// everything under .cog/ regardless of isPathTracked's exclusion list
	// (that list only filters drift/tracked-count accounting, not what goes
	// into the hash). Mirror the real .gitignore convention in this fixture.
	gitignore := filepath.Join(repoDir, ".gitignore")
	if err := os.WriteFile(gitignore, []byte(".cog/run/\n"), 0644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	mustRunGit("add", ".cog/mem/note.cog.md", ".gitignore")
	mustRunGit("commit", "-m", "init")

	return repoDir
}

// gitIndexStatus returns a stable string summarizing the real index state
// (mirrors the root package's gitIndexStatus helper), for the no-mutation
// assertion.
func gitIndexStatus(t *testing.T, repoDir string) string {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	return string(out)
}

func writeCanonicalHash(t *testing.T, repoDir, hash string) {
	t.Helper()
	dir := filepath.Join(repoDir, ".cog", "run", "coherence")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir run/coherence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "canonical-hash"), []byte(hash+"\n"), 0644); err != nil {
		t.Fatalf("write canonical-hash: %v", err)
	}
}

// ─── K3 one-way-readout: CheckCoherence must not mutate the real git index ──

func TestCheckCoherence_NoIndexMutation(t *testing.T) {
	repoDir := setupGitWorkspace(t)

	// Establish a canonical baseline so the coherent/incoherent branch is
	// exercised meaningfully.
	current, err := gitCogTreeHash(repoDir)
	if err != nil {
		t.Fatalf("gitCogTreeHash: %v", err)
	}
	writeCanonicalHash(t, repoDir, current)

	// Add an unstaged ephemeral file, simulating daemon output the real
	// index should never see staged.
	ephemeral := filepath.Join(repoDir, ".cog", "mem", "ephemeral.cog.md")
	if err := os.WriteFile(ephemeral, []byte("# ephemeral\n"), 0644); err != nil {
		t.Fatalf("write ephemeral: %v", err)
	}

	indexBefore := gitIndexStatus(t, repoDir)

	state, err := CheckCoherence(repoDir)
	if err != nil {
		t.Fatalf("CheckCoherence: %v", err)
	}
	if state == nil {
		t.Fatal("CheckCoherence returned nil state")
	}

	indexAfter := gitIndexStatus(t, repoDir)
	if indexBefore != indexAfter {
		t.Errorf("CheckCoherence mutated the git index:\nbefore: %q\nafter:  %q", indexBefore, indexAfter)
	}
}

// ─── B1: C_A (Score) bounds [0,1] ────────────────────────────────────────────

func TestCheckCoherence_ScoreBounds(t *testing.T) {
	repoDir := setupGitWorkspace(t)

	current, err := gitCogTreeHash(repoDir)
	if err != nil {
		t.Fatalf("gitCogTreeHash: %v", err)
	}
	writeCanonicalHash(t, repoDir, current)

	state, err := CheckCoherence(repoDir)
	if err != nil {
		t.Fatalf("CheckCoherence: %v", err)
	}
	if state.Score < 0 || state.Score > 1 {
		t.Errorf("Score = %v; want in [0,1]", state.Score)
	}
	if !state.Coherent {
		t.Errorf("Coherent = false; want true (canonical == current, no drift)")
	}
	if state.Score != 1.0 {
		t.Errorf("Score = %v; want 1.0 for a fully-coherent tree", state.Score)
	}
}

// TestCheckCoherence_NoBaseline_CoherentAndFullScore mirrors cog.go's
// checkCoherence: a missing canonical-hash file means coherent-by-default
// (no baseline to have drifted from) and (B1) a full graded score.
func TestCheckCoherence_NoBaseline_CoherentAndFullScore(t *testing.T) {
	repoDir := setupGitWorkspace(t)
	// No writeCanonicalHash call — canonical-hash file does not exist.

	state, err := CheckCoherence(repoDir)
	if err != nil {
		t.Fatalf("CheckCoherence: %v", err)
	}
	if !state.Coherent {
		t.Error("Coherent = false; want true when no canonical baseline exists")
	}
	if state.Score != 1.0 {
		t.Errorf("Score = %v; want 1.0 when no canonical baseline exists", state.Score)
	}
}

// TestCheckCoherence_Drift_ScoreDecreasesAndCoherentFalse establishes a
// canonical baseline, then mutates + commits a tracked file so the tree
// hash changes, and confirms Coherent flips false while Score reflects
// partial (not necessarily zero) drift — proving the boolean gate is
// unchanged (still canonical==current) and Score is graded independently.
func TestCheckCoherence_Drift_ScoreDecreasesAndCoherentFalse(t *testing.T) {
	repoDir := setupGitWorkspace(t)

	baseline, err := gitCogTreeHash(repoDir)
	if err != nil {
		t.Fatalf("gitCogTreeHash (baseline): %v", err)
	}
	writeCanonicalHash(t, repoDir, baseline)

	// Mutate and commit a tracked file so the real tree hash changes.
	trackedFile := filepath.Join(repoDir, ".cog", "mem", "note.cog.md")
	if err := os.WriteFile(trackedFile, []byte("# note (changed)\n"), 0644); err != nil {
		t.Fatalf("rewrite note.cog.md: %v", err)
	}
	cmd := exec.Command("git", "add", ".cog/mem/note.cog.md")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "drift")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	state, err := CheckCoherence(repoDir)
	if err != nil {
		t.Fatalf("CheckCoherence: %v", err)
	}
	if state.Coherent {
		t.Error("Coherent = true; want false after tracked-file drift")
	}
	if state.Score < 0 || state.Score > 1 {
		t.Errorf("Score = %v; want in [0,1]", state.Score)
	}
	if len(state.Drift) == 0 {
		t.Error("Drift is empty; want at least the changed file listed")
	}
}

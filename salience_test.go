package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ── Decay model tests ─────────────────────────────────────────────────────

func TestComputeDecay(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model    string
		daysAgo  int
		halfLife int
		wantMin  float64
		wantMax  float64
	}{
		// At t=0, all non-unknown models return 1.0.
		{"exponential", 0, 30, 1.0, 1.0},
		{"linear", 0, 30, 1.0, 1.0},
		{"step", 0, 30, 1.0, 1.0},
		// At t=halfLife, exponential ≈ 0.368 (1/e).
		{"exponential", 30, 30, 0.36, 0.38},
		// At t=2*halfLife, linear returns 0.
		{"linear", 60, 30, 0.0, 0.0},
		// Step: before → 1.0, at/after → 0.0.
		{"step", 29, 30, 1.0, 1.0},
		{"step", 30, 30, 0.0, 0.0},
		// Logarithmic: strictly between 0 and 1 at any positive time.
		{"logarithmic", 30, 30, 0.0, 1.0},
		// Unknown model returns 0.
		{"unknown", 0, 30, 0.0, 0.0},
		// Negative daysAgo treated as 0.
		{"exponential", -5, 30, 1.0, 1.0},
	}

	for _, tc := range cases {
		got := computeDecay(tc.model, tc.daysAgo, tc.halfLife)
		if got < tc.wantMin || got > tc.wantMax {
			t.Errorf("computeDecay(%q, %d, %d) = %.4f; want [%.4f, %.4f]",
				tc.model, tc.daysAgo, tc.halfLife, got, tc.wantMin, tc.wantMax)
		}
	}
}

// ── Default config ────────────────────────────────────────────────────────

func TestDefaultSalienceConfig(t *testing.T) {
	t.Parallel()
	cfg := DefaultSalienceConfig()
	if cfg == nil {
		t.Fatal("DefaultSalienceConfig returned nil")
	}
	total := cfg.WeightRecency + cfg.WeightFrequency + cfg.WeightChurn + cfg.WeightAuthorship
	if total < 0.99 || total > 1.01 {
		t.Errorf("weights sum = %.4f; want ≈ 1.0", total)
	}
	if cfg.HalfLife <= 0 {
		t.Errorf("HalfLife = %d; want > 0", cfg.HalfLife)
	}
	if cfg.DecayModel == "" {
		t.Error("DecayModel is empty")
	}
}

// ── Missing file ──────────────────────────────────────────────────────────

func TestComputeFileSalienceMissingFile(t *testing.T) {
	t.Parallel()
	cfg := DefaultSalienceConfig()
	_, err := ComputeFileSalience("/tmp", "/nonexistent/path/file.md", 90, cfg)
	if err == nil {
		t.Error("expected error for missing file")
	}
}

// ── Real git repo ─────────────────────────────────────────────────────────

func TestComputeFileSalienceRealRepo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Initialise a real git repo.
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	// Create a file and commit it.
	filePath := filepath.Join(root, "test.md")
	if err := os.WriteFile(filePath, []byte("# Test\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := wt.Add("test.md"); err != nil {
		t.Fatalf("git add: %v", err)
	}

	sig := &object.Signature{
		Name:  "Test Author",
		Email: "test@cogos-v3.test",
		When:  time.Now(),
	}
	if _, err := wt.Commit("feat: initial test commit", &git.CommitOptions{
		Author:    sig,
		Committer: sig,
	}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	// Salience should be non-zero.
	cfg := DefaultSalienceConfig()
	score, err := ComputeFileSalience(root, filePath, 90, cfg)
	if err != nil {
		t.Fatalf("ComputeFileSalience: %v", err)
	}
	if score == nil {
		t.Fatal("expected non-nil score")
	}
	if score.CommitCount != 1 {
		t.Errorf("CommitCount = %d; want 1", score.CommitCount)
	}
	if score.Total <= 0 {
		t.Errorf("Total = %.4f; want > 0", score.Total)
	}
	if score.UniqueAuthors != 1 {
		t.Errorf("UniqueAuthors = %d; want 1", score.UniqueAuthors)
	}
}

func TestComputeFileSalienceNoCommitsInWindow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Initialise repo with a commit, then query with a 0-day window
	// so the commit falls outside the window.
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	filePath := filepath.Join(root, "old.md")
	if err := os.WriteFile(filePath, []byte("old\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	wt, _ := repo.Worktree()
	wt.Add("old.md") //nolint:errcheck

	sig := &object.Signature{
		Name:  "Author",
		Email: "a@b.c",
		When:  time.Now(),
	}
	wt.Commit("old commit", &git.CommitOptions{Author: sig, Committer: sig}) //nolint:errcheck

	// Window = 0 days → commit is outside window → score should be zero.
	cfg := DefaultSalienceConfig()
	score, err := ComputeFileSalience(root, filePath, 0, cfg)
	if err != nil {
		t.Fatalf("ComputeFileSalience: %v", err)
	}
	if score == nil {
		t.Fatal("expected non-nil score")
	}
	if score.Total != 0 {
		t.Errorf("Total = %.4f; want 0.0 (no commits in window)", score.Total)
	}
}

func TestRankFilesBySalienceEmptyDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := DefaultSalienceConfig()

	scores, err := RankFilesBySalience(root, root, 0, 90, cfg)
	if err != nil {
		t.Fatalf("RankFilesBySalience on empty dir: %v", err)
	}
	if len(scores) != 0 {
		t.Errorf("got %d scores; want 0", len(scores))
	}
}

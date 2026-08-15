package engine

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
		Email: "test@cogos.test",
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

// ── Churn (COG_SALIENCE_CHURN) parity between the single-file and batch
// paths ──────────────────────────────────────────────────────────────────
//
// #563 unified computeFileSalienceWithRepo (single-file) and
// RankFilesBySalience (batch, via batchCollectStats/batchComputeScores) onto
// the same underlying walk. Before this, batch mode hardcoded churn to 0
// regardless of COG_SALIENCE_CHURN ("Churn requires c.Stats() which is
// expensive; skip in batch mode") while the single-file path honored the
// env var. These tests pin the unified behavior: both paths agree exactly,
// including churn, when the env var is set — and both stay at 0 by default.

func makeChurnRepo(t *testing.T) (root, filePath string) {
	t.Helper()
	root = t.TempDir()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	filePath = filepath.Join(root, "churn.md")
	write := func(content string) {
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	commit := func(msg string) {
		if _, err := wt.Add("churn.md"); err != nil {
			t.Fatalf("git add: %v", err)
		}
		sig := &object.Signature{Name: "Test", Email: "test@cogos.test", When: time.Now()}
		if _, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
			t.Fatalf("git commit: %v", err)
		}
	}

	write("line1\n")
	commit("init")
	write("line1\nline2\nline3\nline4\nline5\n")
	commit("grow")

	return root, filePath
}

func TestChurnEnabled_BatchAndSingleFileAgree(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("COG_SALIENCE_CHURN", "1")
	root, filePath := makeChurnRepo(t)
	cfg := DefaultSalienceConfig()

	single, err := ComputeFileSalience(root, filePath, 90, cfg)
	if err != nil {
		t.Fatalf("ComputeFileSalience: %v", err)
	}
	if single.TotalChanges <= 0 {
		t.Errorf("single-file TotalChanges = %d; want > 0 with COG_SALIENCE_CHURN=1", single.TotalChanges)
	}
	if single.Churn <= 0 {
		t.Errorf("single-file Churn = %.4f; want > 0", single.Churn)
	}

	scores, err := RankFilesBySalience(root, root, 0, 90, cfg)
	if err != nil {
		t.Fatalf("RankFilesBySalience: %v", err)
	}
	var batchScore float64
	found := false
	for _, fs := range scores {
		if fs.Path == filePath {
			batchScore, found = fs.Score, true
		}
	}
	if !found {
		t.Fatalf("churn.md missing from RankFilesBySalience results: %+v", scores)
	}

	const eps = 1e-9
	if diff := batchScore - single.Total; diff < -eps || diff > eps {
		t.Errorf("batch score = %.9f; want %.9f (must match the single-file path exactly, including churn)",
			batchScore, single.Total)
	}
}

func TestChurnDisabledByDefault(t *testing.T) {
	t.Parallel()
	root, filePath := makeChurnRepo(t)
	cfg := DefaultSalienceConfig()

	single, err := ComputeFileSalience(root, filePath, 90, cfg)
	if err != nil {
		t.Fatalf("ComputeFileSalience: %v", err)
	}
	if single.TotalChanges != 0 {
		t.Errorf("TotalChanges = %d; want 0 (COG_SALIENCE_CHURN unset)", single.TotalChanges)
	}
	if single.Churn != 0 {
		t.Errorf("Churn = %.4f; want 0 (COG_SALIENCE_CHURN unset)", single.Churn)
	}
}

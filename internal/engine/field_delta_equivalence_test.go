// field_delta_equivalence_test.go — #563 correctness coverage.
//
// The #563 fix reroutes AttentionalField.deltaUpdate through the same
// batchCollectStats walk RankFilesBySalience's full scan already uses,
// instead of computeFileSalienceWithRepo's now-retired per-path walk. This
// file asserts that rerouting did not change any observable score: a field
// that takes the delta path (Update -> deltaUpdate, HEAD changed since last
// scan) must agree exactly with a freshly constructed field that only ever
// takes the full-scan path (Update on empty state) against the same
// final HEAD — for both the base (chat-read) and observer (inbox-boosted)
// views, including a file the delta left untouched.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestFieldDeltaUpdateMatchesFullScan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	memDir := filepath.Join(root, ".cog", "mem")
	if err := os.MkdirAll(filepath.Join(memDir, "inbox"), 0o755); err != nil {
		t.Fatalf("mkdir mem: %v", err)
	}

	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(memDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	commitAll := func(msg string, when time.Time) {
		t.Helper()
		if _, err := wt.Add("."); err != nil {
			t.Fatalf("git add: %v", err)
		}
		sig := &object.Signature{Name: "Test", Email: "test@cogos.test", When: when}
		if _, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
			t.Fatalf("git commit: %v", err)
		}
	}

	now := time.Now()

	// Initial commit: two integrated docs.
	write("a.cog.md", "---\nstatus: integrated\n---\nA v1\n")
	write("b.cog.md", "---\nstatus: integrated\n---\nB v1\n")
	commitAll("init", now.AddDate(0, 0, -10))

	cfg := makeConfig(t, root)
	cfg.SalienceDaysWindow = 90
	f := NewAttentionalField(cfg)
	if err := f.Update(); err != nil {
		t.Fatalf("initial Update: %v", err)
	}
	headAfterInit := f.lastHEAD
	if headAfterInit == "" {
		t.Fatal("lastHEAD not set after initial Update")
	}

	// Delta commit: modify a.cog.md, add an inbox item, leave b.cog.md untouched.
	write("a.cog.md", "---\nstatus: integrated\n---\nA v2, updated\n")
	write("inbox/c.cog.md", "---\nstatus: raw\n---\nC, new\n")
	commitAll("update", now)

	if err := f.Update(); err != nil {
		t.Fatalf("delta Update: %v", err)
	}
	if f.lastHEAD == headAfterInit {
		t.Fatal("lastHEAD did not advance — delta Update did not see the second commit")
	}

	// Ground truth: a brand-new field that has never seen headAfterInit, so
	// its only path to the current HEAD is a full scan (Mode 3).
	freshCfg := makeConfig(t, root)
	freshCfg.SalienceDaysWindow = 90
	fresh := NewAttentionalField(freshCfg)
	if err := fresh.Update(); err != nil {
		t.Fatalf("fresh full-scan Update: %v", err)
	}

	const eps = 1e-9
	compare := func(label string, got, want map[string]float64) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: len = %d; want %d (got=%v want=%v)", label, len(got), len(want), got, want)
		}
		for path, wantScore := range want {
			gotScore, ok := got[path]
			if !ok {
				t.Errorf("%s: missing path %s (want score %.6f)", label, path, wantScore)
				continue
			}
			if diff := gotScore - wantScore; diff < -eps || diff > eps {
				t.Errorf("%s[%s] = %.9f; want %.9f (delta-path vs full-scan diverge)", label, path, gotScore, wantScore)
			}
		}
		for path := range got {
			if _, ok := want[path]; !ok {
				t.Errorf("%s: delta-path has extra path %s not in full-scan ground truth", label, path)
			}
		}
	}

	compare("base", f.AllBaseScores(), fresh.AllBaseScores())
	compare("observer", f.AllScores(), fresh.AllScores())

	// Sanity: the untouched file, the delta-modified file, and the new
	// inbox item are all actually present and distinguishable, so this test
	// would catch a wiring bug (e.g. a dropped file) rather than vacuously
	// passing on an empty comparison.
	aPath := filepath.Join(memDir, "a.cog.md")
	bPath := filepath.Join(memDir, "b.cog.md")
	cPath := filepath.Join(memDir, "inbox", "c.cog.md")
	if f.Score(aPath) <= 0 {
		t.Errorf("a.cog.md base score = %.4f; want > 0 (recently modified)", f.Score(aPath))
	}
	if f.Score(bPath) <= 0 {
		t.Errorf("b.cog.md base score = %.4f; want > 0 (untouched by delta, but still in window)", f.Score(bPath))
	}
	if delta := f.ObserverScore(cPath) - f.Score(cPath); delta < inboxRawBoost-eps || delta > inboxRawBoost+eps {
		t.Errorf("inbox item c.cog.md: ObserverScore-Score = %.6f; want inboxRawBoost (%.6f)", delta, inboxRawBoost)
	}
}

// TestFieldDeltaUpdateRaceWithConcurrentReaders exercises deltaUpdate's
// shared-state writes against real concurrent readers under -race.
//
// Lock discipline (single lock, no nesting): f.mu (sync.RWMutex) guards all
// four of f.base, f.observer, f.lastUpdated, and f.lastHEAD together — there
// is no per-field locking and no second lock, so there is no ordering to
// get wrong. #563's change to deltaUpdate does not alter this discipline;
// it only changes how many times the writer acquires f.mu.Lock() per call
// (previously once per changed file; now once for the batch of scored
// results, once per deleted file, and once for the lastHEAD/lastUpdated
// bookkeeping) and moves all file I/O (readInboxStatus) outside every
// critical section, same as before. Readers take f.mu.RLock() via
// Score/ObserverScore/Fovea/AllScores/Len and never block a writer's
// progress on their own I/O.
func TestFieldDeltaUpdateRaceWithConcurrentReaders(t *testing.T) {
	root := t.TempDir()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	memDir := filepath.Join(root, ".cog", "mem")
	if err := os.MkdirAll(filepath.Join(memDir, "inbox"), 0o755); err != nil {
		t.Fatalf("mkdir mem: %v", err)
	}
	commitAll := func(msg string) {
		t.Helper()
		if _, err := wt.Add("."); err != nil {
			t.Fatalf("git add: %v", err)
		}
		sig := &object.Signature{Name: "Test", Email: "test@cogos.test", When: time.Now()}
		if _, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
			t.Fatalf("git commit: %v", err)
		}
	}

	// Seed with a handful of files so early deltas have something to touch.
	for i := 0; i < 3; i++ {
		p := filepath.Join(memDir, fmt.Sprintf("seed%d.cog.md", i))
		if err := os.WriteFile(p, []byte("---\nstatus: integrated\n---\nseed\n"), 0o644); err != nil {
			t.Fatalf("write seed: %v", err)
		}
	}
	commitAll("seed")

	cfg := makeConfig(t, root)
	cfg.SalienceDaysWindow = 90
	f := NewAttentionalField(cfg)
	if err := f.Update(); err != nil {
		t.Fatalf("initial Update: %v", err)
	}

	const rounds = 15
	const readers = 8

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: repeatedly commit a change and call Update(), which resolves
	// to deltaUpdate on every round after the first (lastHEAD is always set
	// by then).
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < rounds; i++ {
			target := filepath.Join(memDir, fmt.Sprintf("seed%d.cog.md", i%3))
			content := fmt.Sprintf("---\nstatus: integrated\n---\nround %d\n", i)
			if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
				t.Errorf("write round %d: %v", i, err)
				return
			}
			if i%4 == 0 {
				inbox := filepath.Join(memDir, "inbox", fmt.Sprintf("item%d.cog.md", i))
				if err := os.WriteFile(inbox, []byte("---\nstatus: raw\n---\nnew\n"), 0o644); err != nil {
					t.Errorf("write inbox round %d: %v", i, err)
					return
				}
			}
			commitAll(fmt.Sprintf("round %d", i))
			if err := f.Update(); err != nil {
				t.Errorf("Update round %d: %v", i, err)
				return
			}
			// Also exercise Boost, which takes the same write lock from a
			// different call site.
			f.Boost(target, 0.01)
		}
	}()

	// Readers: hammer every read entry point concurrently with the writer.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = f.Score(filepath.Join(memDir, "seed0.cog.md"))
				_ = f.ObserverScore(filepath.Join(memDir, "seed1.cog.md"))
				_ = f.Fovea(5)
				_ = f.BaseFovea(5)
				_ = f.AllScores()
				_ = f.AllBaseScores()
				_ = f.Len()
				_ = f.LastUpdated()
			}
		}()
	}

	wg.Wait()
}

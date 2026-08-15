// salience_walk_bound_test.go — regression coverage for #563.
//
// #563: computeFileSalienceWithRepo (and, transitively, AttentionalField's
// deltaUpdate) walked the commit graph via go-git's commitPathIter looking
// for the *next* commit that touched a given path, diffing every
// intervening commit along the way, before ever consulting daysWindow. A
// file touched once far in the past and once recently forced a walk of
// nearly the entire remaining history to confirm there were no more
// (in-window) matches after the old one, even though everything past the
// recent touch was already known to be irrelevant to the score.
//
// These tests build a repo where that shape is real (a file with one very
// old touch and one recent touch, separated by a long run of commits that
// never touch it) and assert the walk is bounded by daysWindow, not by
// total history depth — via a hard wall-clock deadline enforced through a
// goroutine + select, so an unbounded walk fails the test outright rather
// than just making it slow.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// objSetter is the subset of the repo's storer this file needs.
type objSetter interface {
	SetEncodedObject(plumbing.EncodedObject) (plumbing.Hash, error)
}

// blobHash stores content as a git blob and returns its hash.
func blobHash(t *testing.T, st objSetter, content []byte) plumbing.Hash {
	t.Helper()
	b := &plumbing.MemoryObject{}
	b.SetType(plumbing.BlobObject)
	w, err := b.Writer()
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("blob write: %v", err)
	}
	h, err := st.SetEncodedObject(b)
	if err != nil {
		t.Fatalf("set blob: %v", err)
	}
	return h
}

// buildTree writes a (possibly nested, "/"-separated) flat path->blob map as
// a tree of git tree objects and returns the root tree's hash. Recurses one
// directory level at a time so paths like ".cog/mem/target.md" become real
// nested trees, not a single flat entry.
func buildTree(t *testing.T, st objSetter, flat map[string]plumbing.Hash) plumbing.Hash {
	t.Helper()
	direct := map[string]plumbing.Hash{}
	dirs := map[string]map[string]plumbing.Hash{}
	for path, h := range flat {
		if idx := strings.IndexByte(path, '/'); idx >= 0 {
			dir, rest := path[:idx], path[idx+1:]
			if dirs[dir] == nil {
				dirs[dir] = map[string]plumbing.Hash{}
			}
			dirs[dir][rest] = h
		} else {
			direct[path] = h
		}
	}

	entries := make([]object.TreeEntry, 0, len(direct)+len(dirs))
	for name, h := range direct {
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h})
	}
	for name, sub := range dirs {
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: buildTree(t, st, sub)})
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name < entries[b].Name })

	tree := &object.Tree{Entries: entries}
	obj := &plumbing.MemoryObject{}
	if err := tree.Encode(obj); err != nil {
		t.Fatalf("encode tree: %v", err)
	}
	h, err := st.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("set tree: %v", err)
	}
	return h
}

// deepHistoryRepo builds a repo of the shape described in the file comment,
// using go-git's low-level plumbing (bypassing Worktree.Commit, which
// re-scans the working tree and rewrites the index on every call — too slow
// to build repos with thousands of commits).
//
// Layout:
//   - commit[0]:          creates .cog/mem/target.md (old content) plus a
//     fixed 5-file noise set, dated far outside any reasonable daysWindow.
//   - commit[1..N]:        each modifies exactly one of the 5 noise files
//     (round-robin, so tree size — and therefore diff cost — stays O(1) per
//     commit instead of O(commit_index)); dated evenly across the days
//     between the root and the final commit. None of these touch target.md.
//   - commit[N+1] (HEAD):  modifies .cog/mem/target.md again ("today"), so
//     target.md has exactly one recent touch and one ancient touch with
//     nothing in between.
//
// target.md lives under .cog/mem/ (not repo root) so it satisfies
// AttentionalField.deltaUpdate's memory-file path prefix check.
//
// Returns the repo root and the final HEAD hash.
func deepHistoryRepo(t *testing.T, noiseCommits int) (root string, head plumbing.Hash) {
	t.Helper()
	root = t.TempDir()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	st := repo.Storer

	const targetPath = ".cog/mem/target.md"
	files := map[string]plumbing.Hash{}

	commit := func(parent plumbing.Hash, when time.Time) plumbing.Hash {
		t.Helper()
		treeHash := buildTree(t, st, files)

		var parents []plumbing.Hash
		if !parent.IsZero() {
			parents = []plumbing.Hash{parent}
		}
		sig := object.Signature{Name: "Test", Email: "test@cogos.test", When: when}
		c := &object.Commit{
			Author:       sig,
			Committer:    sig,
			Message:      "c",
			TreeHash:     treeHash,
			ParentHashes: parents,
		}
		commitObj := &plumbing.MemoryObject{}
		if err := c.Encode(commitObj); err != nil {
			t.Fatalf("encode commit: %v", err)
		}
		commitHash, err := st.SetEncodedObject(commitObj)
		if err != nil {
			t.Fatalf("set commit: %v", err)
		}
		return commitHash
	}

	total := noiseCommits + 2 // root + noise commits + final
	now := time.Now()

	// commit[0]: root, dated far outside any reasonable daysWindow.
	files[targetPath] = blobHash(t, st, []byte("old content"))
	for i := 0; i < 5; i++ {
		files[fmt.Sprintf("noise%d.md", i)] = blobHash(t, st, []byte("init"))
	}
	parent := commit(plumbing.ZeroHash, now.AddDate(0, 0, -total-1))

	// commit[1..noiseCommits]: touch only the rotating noise set. None of
	// these touch target.md.
	for i := 1; i <= noiseCommits; i++ {
		name := fmt.Sprintf("noise%d.md", i%5)
		files[name] = blobHash(t, st, []byte(fmt.Sprintf("v%d", i)))
		parent = commit(parent, now.AddDate(0, 0, -(total-i)))
	}

	// commit[N+1] = HEAD: touches target.md again, "today". This is the
	// only touch inside any small daysWindow.
	files[targetPath] = blobHash(t, st, []byte("new content"))
	head = commit(parent, now)

	branchRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), head)
	if err := st.SetReference(branchRef); err != nil {
		t.Fatalf("set branch ref: %v", err)
	}
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))
	if err := st.SetReference(headRef); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}

	// Mirror target.md onto disk at the same relative path, since deltaUpdate
	// also os.Stats the file to detect deletions independent of git history.
	absTarget := filepath.Join(root, filepath.FromSlash(targetPath))
	if err := os.MkdirAll(filepath.Dir(absTarget), 0o755); err != nil {
		t.Fatalf("mkdir mem: %v", err)
	}
	if err := os.WriteFile(absTarget, []byte("new content"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	return root, head
}

// runWithDeadline runs fn in a goroutine and fails the test outright if it
// has not returned within deadline, rather than merely reporting a slow
// elapsed time. An unbounded walk over deepHistoryRepo's history hangs this
// well past any bound a fixed, date-limited walk would need.
func runWithDeadline(t *testing.T, deadline time.Duration, fn func()) time.Duration {
	t.Helper()
	done := make(chan struct{})
	start := time.Now()
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		return time.Since(start)
	case <-time.After(deadline):
		t.Fatalf("did not complete within %v — walk is not bounded by daysWindow", deadline)
		return 0
	}
}

// TestComputeFileSalience_BoundedBySparseHistory reproduces #563 directly
// against computeFileSalienceWithRepo via the exported ComputeFileSalience
// single-file API: target.md has one recent touch and one touch 1,500+
// days in the past, separated by 1,500 commits that never touch it. Before
// the fix, finding "no more matches after the recent one, within window"
// required diffing effectively all 1,500 intervening commits per call.
func TestComputeFileSalience_BoundedBySparseHistory(t *testing.T) {
	const noiseCommits = 6000
	root, _ := deepHistoryRepo(t, noiseCommits)

	cfg := DefaultSalienceConfig()
	var score *SalienceScore
	var scoreErr error

	elapsed := runWithDeadline(t, 3*time.Second, func() {
		score, scoreErr = ComputeFileSalience(root, filepath.Join(root, ".cog", "mem", "target.md"), 30, cfg)
	})
	if scoreErr != nil {
		t.Fatalf("ComputeFileSalience: %v", scoreErr)
	}
	t.Logf("ComputeFileSalience over %d-commit history: %v", noiseCommits+2, elapsed)

	// Correctness: only the recent (in-window) touch should count. The old
	// touch is 1,500+ days before a 30-day window and must not contribute.
	if score.CommitCount != 1 {
		t.Errorf("CommitCount = %d; want 1 (only the recent touch is in-window)", score.CommitCount)
	}
	if score.Total <= 0 {
		t.Errorf("Total = %.4f; want > 0", score.Total)
	}

	// The walk must be bounded by the window, not by history depth: on this
	// machine a few dozen in-window commits should resolve in low
	// milliseconds. 500ms leaves generous headroom for slow CI while still
	// being ~1-2 orders of magnitude below what walking 1,500 commits costs
	// (pre-fix, this reliably exceeds the 3s hard deadline above).
	if elapsed > 500*time.Millisecond {
		t.Errorf("ComputeFileSalience took %v against a sparse-history file; "+
			"want well under 500ms — walk depth looks proportional to total "+
			"history rather than to daysWindow", elapsed)
	}
}

// TestFieldDeltaUpdate_BoundedBySparseHistory reproduces #563 via the actual
// profiled call chain: AttentionalField.Update -> deltaUpdate. oldHEAD/newHEAD
// are set to the last two commits, so filesChangedBetweenWithRepo reports
// exactly .cog/mem/target.md as changed (that's what the final commit
// touched) — deltaUpdate must then score it without walking the full history
// behind it.
func TestFieldDeltaUpdate_BoundedBySparseHistory(t *testing.T) {
	const noiseCommits = 6000
	root, head := deepHistoryRepo(t, noiseCommits)

	repo, err := git.PlainOpen(root)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	headCommit, err := repo.CommitObject(head)
	if err != nil {
		t.Fatalf("head commit: %v", err)
	}
	parentCommit, err := headCommit.Parent(0)
	if err != nil {
		t.Fatalf("parent commit: %v", err)
	}

	cfg := makeConfig(t, root)
	cfg.SalienceDaysWindow = 30
	f := NewAttentionalField(cfg)

	var updated int
	var deltaErr error
	elapsed := runWithDeadline(t, 3*time.Second, func() {
		updated, deltaErr = f.deltaUpdate(parentCommit.Hash.String(), head.String())
	})
	t.Logf("deltaUpdate over %d-commit history: %v (updated=%d)", noiseCommits+2, elapsed, updated)
	if deltaErr != nil {
		t.Fatalf("deltaUpdate: %v", deltaErr)
	}
	if updated != 1 {
		t.Errorf("updated = %d; want 1 (only target.md changed between the last two commits)", updated)
	}

	gotScore := f.Score(filepath.Join(root, ".cog", "mem", "target.md"))
	if gotScore <= 0 {
		t.Errorf("Score(target.md) = %.4f; want > 0 (recent touch is in-window)", gotScore)
	}

	if elapsed > 500*time.Millisecond {
		t.Errorf("deltaUpdate took %v against a sparse-history changed file; "+
			"want well under 500ms", elapsed)
	}
}

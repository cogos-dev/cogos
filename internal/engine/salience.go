// salience.go — CogOS v3 git-derived salience scoring
//
// Ported from apps/cogos/salience.go (v2.4.0).
// CLI command functions removed; core computation preserved.
//
// Implements ADR-018: Salience System (Git-Derived Attention).
// Performance target: <5ms per file via go-git (vs 80ms in shell version).
package engine

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// SalienceConfig holds weights and decay parameters for salience computation.
type SalienceConfig struct {
	WeightRecency    float64
	WeightFrequency  float64
	WeightChurn      float64
	WeightAuthorship float64
	DecayModel       string
	HalfLife         int // days
}

// DefaultSalienceConfig returns sensible defaults.
func DefaultSalienceConfig() *SalienceConfig {
	return &SalienceConfig{
		WeightRecency:    0.4,
		WeightFrequency:  0.3,
		WeightChurn:      0.2,
		WeightAuthorship: 0.1,
		DecayModel:       "exponential",
		HalfLife:         30,
	}
}

// SalienceScore holds the computed salience breakdown for a file.
type SalienceScore struct {
	Recency       float64
	Frequency     float64
	Churn         float64
	Authorship    float64
	Total         float64
	CommitCount   int
	TotalChanges  int
	UniqueAuthors int
	DaysAgo       int
}

// FileScore pairs a file path with its total salience score.
type FileScore struct {
	Path  string
	Score float64
}

// computeDecay returns a [0,1] time-decay factor given model and elapsed days.
func computeDecay(model string, daysAgo, halfLife int) float64 {
	if daysAgo < 0 {
		daysAgo = 0
	}
	switch model {
	case "exponential":
		return math.Exp(-float64(daysAgo) / float64(halfLife))
	case "linear":
		v := 1.0 - float64(daysAgo)/(2.0*float64(halfLife))
		if v < 0 {
			return 0
		}
		return v
	case "step":
		if daysAgo < halfLife {
			return 1.0
		}
		return 0.0
	case "logarithmic":
		t := 1.0 + float64(daysAgo)/float64(halfLife)
		return 1.0 / (1.0 + math.Log(t))
	default:
		return 0.0
	}
}

// ComputeFileSalience computes salience for a single file from its git history.
// Returns a zero-score result (not nil) if the file has no commits in the window.
//
// NOTE: For batch scoring (many files), use RankFilesBySalience which opens the
// repo once. This function opens the repo per call and is only suitable for
// single-file queries or tests.
func ComputeFileSalience(repoPath, filePath string, daysWindow int, cfg *SalienceConfig) (*SalienceScore, error) {
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("file not found: %s", filePath)
	}

	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("open repo: %w", err)
	}

	return computeFileSalienceWithRepo(repo, repoPath, filePath, daysWindow, cfg)
}

// salienceChurnEnabled reports whether COG_SALIENCE_CHURN opts into the
// per-commit line-churn signal (an extra c.Stats() patch computation per
// commit visited). Off by default — churn contributes 0 to the score unless
// this is set.
func salienceChurnEnabled() bool {
	v := strings.ToLower(os.Getenv("COG_SALIENCE_CHURN"))
	return v == "1" || v == "true"
}

// computeFileSalienceWithRepo is the inner implementation that accepts a pre-opened
// repo handle. This avoids re-parsing pack indexes for every file in a batch.
//
// #563: this used to run its own repo.Log(PathFilter) walk via go-git's
// commitPathIter, which diffs every commit between HEAD and the *next
// path-matching commit* before ever consulting daysWindow — so a file with
// no touches in the window (or a sparse touch history) forced a walk of the
// full remaining commit graph, repeated per file, per consolidation pass.
// It now delegates to batchCollectStats (a single date-bounded walk, cutoff
// checked before any per-commit diff) with a one-entry scope, so a single
// call costs at most one full walk of the commits within daysWindow rather
// than an unbounded walk of the whole history. See RankFilesBySalience for
// the same bound applied across many files in one shared walk.
func computeFileSalienceWithRepo(repo *git.Repository, repoPath, filePath string, daysWindow int, cfg *SalienceConfig) (*SalienceScore, error) {
	relPath := filePath
	if filepath.IsAbs(filePath) {
		if rel, err := filepath.Rel(repoPath, filePath); err == nil {
			clean := filepath.Clean(rel)
			if clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				relPath = clean
			}
		}
	}
	relPath = filepath.ToSlash(relPath)

	stats, err := batchCollectStats(repo, map[string]string{relPath: filePath}, daysWindow)
	if err != nil {
		return nil, fmt.Errorf("iterate commits: %w", err)
	}

	acc := stats[relPath]
	if acc == nil || acc.commitCount == 0 {
		return &SalienceScore{}, nil
	}

	daysAgo := int(time.Since(acc.lastTimestamp).Hours() / 24)
	recency := computeDecay(cfg.DecayModel, daysAgo, cfg.HalfLife)

	frequency := float64(acc.commitCount) / 10.0
	if frequency > 1.0 {
		frequency = 1.0
	}

	avgChanges := float64(acc.totalChanges) / float64(acc.commitCount)
	churn := avgChanges / 100.0
	if churn > 1.0 {
		churn = 1.0
	}

	authorship := float64(len(acc.authors)) / 5.0
	if authorship > 1.0 {
		authorship = 1.0
	}

	total := cfg.WeightRecency*recency +
		cfg.WeightFrequency*frequency +
		cfg.WeightChurn*churn +
		cfg.WeightAuthorship*authorship

	return &SalienceScore{
		Recency:       recency,
		Frequency:     frequency,
		Churn:         churn,
		Authorship:    authorship,
		Total:         total,
		CommitCount:   acc.commitCount,
		TotalChanges:  acc.totalChanges,
		UniqueAuthors: len(acc.authors),
		DaysAgo:       daysAgo,
	}, nil
}

// fileAccum accumulates git history stats for a single file during a batch log walk.
type fileAccum struct {
	commitCount   int
	totalChanges  int // line churn (additions+deletions); only populated when salienceChurnEnabled()
	lastTimestamp time.Time
	authors       map[string]bool
}

// RankFilesBySalience walks scope and returns all .md/.cog.md files sorted by score.
//
// Uses a single-pass commit walk: iterates the git log once and records which
// scope-files each commit touched. This is O(commits × files_per_commit) instead
// of the old O(files × commits) approach that ran a filtered log per file.
func RankFilesBySalience(repoPath, scope string, limit, daysWindow int, cfg *SalienceConfig) ([]FileScore, error) {
	// Collect file paths and build a lookup set of relative paths we care about.
	relToAbs := make(map[string]string) // slash-relative → absolute
	err := filepath.Walk(scope, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		base := path[:len(path)-len(ext)]
		if ext != ".md" && filepath.Ext(base) != ".cog" {
			return nil
		}
		if rel, e := filepath.Rel(repoPath, path); e == nil {
			relToAbs[filepath.ToSlash(filepath.Clean(rel))] = path
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", scope, err)
	}
	if len(relToAbs) == 0 {
		return nil, nil
	}

	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("open repo: %w", err)
	}

	stats, err := batchCollectStats(repo, relToAbs, daysWindow)
	if err != nil {
		return nil, fmt.Errorf("batch stats: %w", err)
	}

	scores := batchComputeScores(stats, relToAbs, cfg)

	sort.Slice(scores, func(i, j int) bool {
		return scores[i].Score > scores[j].Score
	})
	if limit > 0 && len(scores) > limit {
		scores = scores[:limit]
	}
	return scores, nil
}

// batchCollectStats walks the git log once and accumulates per-file stats for
// all files in the scope set. Only examines commits within the daysWindow —
// the cutoff is checked before any per-commit diff runs, so the walk never
// descends past daysWindow regardless of how sparsely (or never) the scope
// files were touched before that point.
//
// When salienceChurnEnabled() is true, also accumulates per-file line churn
// via one c.Stats() call per commit in the window, shared across every file
// in scope — one patch computation per commit total, not one per file.
func batchCollectStats(repo *git.Repository, relToAbs map[string]string, daysWindow int) (map[string]*fileAccum, error) {
	cutoff := time.Now().AddDate(0, 0, -daysWindow)
	stats := make(map[string]*fileAccum)
	enableChurn := salienceChurnEnabled()

	iter, err := repo.Log(&git.LogOptions{
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	defer iter.Close()

	err = iter.ForEach(func(c *object.Commit) error {
		if c.Author.When.Before(cutoff) {
			return storer.ErrStop
		}

		// Get files changed in this commit by diffing against first parent.
		changed := commitChangedFiles(c)
		author := c.Author.Email
		when := c.Author.When

		var churn map[string]int
		if enableChurn {
			if fileStats, err := c.Stats(); err == nil {
				churn = make(map[string]int, len(fileStats))
				for _, s := range fileStats {
					churn[s.Name] = s.Addition + s.Deletion
				}
			}
		}

		for _, relPath := range changed {
			if _, inScope := relToAbs[relPath]; !inScope {
				continue
			}
			acc := stats[relPath]
			if acc == nil {
				acc = &fileAccum{authors: make(map[string]bool)}
				stats[relPath] = acc
			}
			acc.commitCount++
			acc.authors[author] = true
			if acc.lastTimestamp.IsZero() || when.After(acc.lastTimestamp) {
				acc.lastTimestamp = when
			}
			if churn != nil {
				acc.totalChanges += churn[relPath]
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate commits: %w", err)
	}

	return stats, nil
}

// commitChangedFiles returns the list of file paths changed by a commit
// (relative to its first parent, or all files for root commits).
// Uses tree diffing which is cheaper than c.Stats() (no line counting).
func commitChangedFiles(c *object.Commit) []string {
	commitTree, err := c.Tree()
	if err != nil {
		return nil
	}

	var parentTree *object.Tree
	if c.NumParents() > 0 {
		parent, err := c.Parent(0)
		if err == nil {
			parentTree, _ = parent.Tree()
		}
	}

	changes, err := object.DiffTree(parentTree, commitTree)
	if err != nil {
		return nil
	}

	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		name := change.To.Name
		if name == "" {
			name = change.From.Name // deletion
		}
		if name != "" {
			paths = append(paths, name)
		}
	}
	return paths
}

// batchComputeScores converts accumulated stats into scored FileScore entries.
// Files that exist on disk but have no commits in the window get a zero score
// and are still included (they exist in the memory corpus).
func batchComputeScores(stats map[string]*fileAccum, relToAbs map[string]string, cfg *SalienceConfig) []FileScore {
	scores := make([]FileScore, 0, len(relToAbs))
	for relPath, absPath := range relToAbs {
		acc := stats[relPath]
		if acc == nil || acc.commitCount == 0 {
			scores = append(scores, FileScore{Path: absPath, Score: 0})
			continue
		}

		daysAgo := int(time.Since(acc.lastTimestamp).Hours() / 24)
		recency := computeDecay(cfg.DecayModel, daysAgo, cfg.HalfLife)

		frequency := float64(acc.commitCount) / 10.0
		if frequency > 1.0 {
			frequency = 1.0
		}

		// acc.totalChanges is only populated when salienceChurnEnabled();
		// otherwise this is 0, same as before batchCollectStats gained
		// churn support.
		avgChanges := float64(acc.totalChanges) / float64(acc.commitCount)
		churn := avgChanges / 100.0
		if churn > 1.0 {
			churn = 1.0
		}

		authorship := float64(len(acc.authors)) / 5.0
		if authorship > 1.0 {
			authorship = 1.0
		}

		total := cfg.WeightRecency*recency +
			cfg.WeightFrequency*frequency +
			cfg.WeightChurn*churn +
			cfg.WeightAuthorship*authorship

		scores = append(scores, FileScore{Path: absPath, Score: total})
	}
	return scores
}

// GetHotFiles returns paths with salience above threshold.
func GetHotFiles(repoPath, scope string, limit int, threshold float64, daysWindow int, cfg *SalienceConfig) ([]string, error) {
	ranked, err := RankFilesBySalience(repoPath, scope, 100, daysWindow, cfg)
	if err != nil {
		return nil, err
	}
	var hot []string
	for _, fs := range ranked {
		if fs.Score > threshold {
			hot = append(hot, fs.Path)
			if limit > 0 && len(hot) >= limit {
				break
			}
		}
	}
	return hot, nil
}

// field.go — CogOS v3 attentional field
//
// The attentional field is the continuous salience map over the memory corpus.
// Every memory file gets a float64 score. The "fovea" is the top-N files by
// score that fit in the context window.
//
// In v2, salience was computed once per session at context assembly time.
// In v3, the field is updated continuously by the process loop, decoupled
// from any external request.
package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	// inboxRawBoost is the salience bonus for inbox items with status: raw.
	// New, unprocessed items should attract the observer's attention so the
	// enrichment pipeline picks them up promptly.
	inboxRawBoost = 0.5

	// inboxEnrichedBoost is a smaller bonus for enriched but not-yet-integrated
	// items. They still need attention (integration step), but less urgently.
	inboxEnrichedBoost = 0.2

	// inboxPathFragment is the path substring that identifies inbox items.
	inboxPathFragment = "/inbox/"
)

// AttentionalField holds the current salience map for the memory corpus.
// It is safe for concurrent reads (serve goroutine) and periodic writes
// (consolidation goroutine).
type AttentionalField struct {
	mu sync.RWMutex

	// scores maps absolute file path → salience score.
	scores map[string]float64

	// lastUpdated is when the field was last fully recomputed.
	lastUpdated time.Time

	// lastHEAD is the HEAD commit hash at last successful update.
	// Used to skip expensive recomputation when nothing has changed.
	lastHEAD string

	// cfg holds the workspace configuration.
	cfg *Config

	// salCfg holds the salience computation parameters.
	salCfg *SalienceConfig
}

// NewAttentionalField constructs an empty field. Call Update() to populate it.
func NewAttentionalField(cfg *Config) *AttentionalField {
	return &AttentionalField{
		scores: make(map[string]float64),
		cfg:    cfg,
		salCfg: DefaultSalienceConfig(),
	}
}

// Update recomputes salience for memory files.
//
// Three modes, selected automatically:
//  1. HEAD unchanged + scores exist → no-op (instant)
//  2. Previous HEAD known + new HEAD → delta scan (only new commits)
//  3. No previous state → full scan (startup)
func (f *AttentionalField) Update() error {
	currentHEAD := resolveHEAD(f.cfg.WorkspaceRoot)
	f.mu.RLock()
	cached := f.lastHEAD
	hasScores := len(f.scores) > 0
	f.mu.RUnlock()

	// Mode 1: nothing changed.
	if currentHEAD != "" && currentHEAD == cached && hasScores {
		slog.Debug("field: HEAD unchanged, skipping", "head", currentHEAD[:12])
		return nil
	}

	// Mode 2: delta scan — only rescore files touched since lastHEAD.
	if cached != "" && currentHEAD != "" && cached != currentHEAD && hasScores {
		updated, err := f.deltaUpdate(cached, currentHEAD)
		if err != nil {
			slog.Warn("field: delta update failed, falling through to full scan", "err", err)
		} else {
			slog.Info("field: delta update", "changed_files", updated, "head", currentHEAD[:12])
			return nil
		}
	}

	// Mode 3: full scan.
	slog.Info("field: full scan starting")
	memDir := fmt.Sprintf("%s/.cog/mem", f.cfg.WorkspaceRoot)
	ranked, err := RankFilesBySalience(
		f.cfg.WorkspaceRoot,
		memDir,
		0,
		f.cfg.SalienceDaysWindow,
		f.salCfg,
	)
	if err != nil {
		return fmt.Errorf("rank files: %w", err)
	}

	fresh := make(map[string]float64, len(ranked))
	for _, fs := range ranked {
		fresh[fs.Path] = fs.Score
	}
	applyInboxBoosts(fresh)

	f.mu.Lock()
	f.scores = fresh
	f.lastUpdated = time.Now()
	f.lastHEAD = currentHEAD
	f.mu.Unlock()

	slog.Info("field: full scan complete", "files", len(fresh))
	return nil
}

// deltaUpdate rescores only files changed between oldHEAD and newHEAD.
// Opens the repo exactly once and reuses the handle for both diffing and scoring.
// Returns the number of files updated.
func (f *AttentionalField) deltaUpdate(oldHEAD, newHEAD string) (int, error) {
	repo, err := git.PlainOpen(f.cfg.WorkspaceRoot)
	if err != nil {
		return 0, fmt.Errorf("open repo: %w", err)
	}

	changed, err := filesChangedBetweenWithRepo(repo, oldHEAD, newHEAD)
	if err != nil {
		return 0, err
	}
	if len(changed) == 0 {
		f.mu.Lock()
		f.lastHEAD = newHEAD
		f.lastUpdated = time.Now()
		f.mu.Unlock()
		return 0, nil
	}

	memPrefix := fmt.Sprintf("%s/.cog/mem/", f.cfg.WorkspaceRoot)
	updated := 0

	for _, relPath := range changed {
		absPath := filepath.Join(f.cfg.WorkspaceRoot, filepath.FromSlash(relPath))
		if !strings.HasPrefix(absPath, memPrefix) {
			continue // not a memory file
		}
		if _, err := os.Stat(absPath); os.IsNotExist(err) {
			f.mu.Lock()
			delete(f.scores, absPath)
			f.mu.Unlock()
			updated++
			continue
		}
		score, err := computeFileSalienceWithRepo(repo, f.cfg.WorkspaceRoot, absPath, f.cfg.SalienceDaysWindow, f.salCfg)
		if err != nil || score == nil {
			continue
		}
		val := score.Total
		if strings.Contains(absPath, inboxPathFragment) {
			switch readInboxStatus(absPath) {
			case "raw":
				val += inboxRawBoost
			case "enriched":
				val += inboxEnrichedBoost
			}
		}
		f.mu.Lock()
		f.scores[absPath] = val
		f.mu.Unlock()
		updated++
	}

	f.mu.Lock()
	f.lastHEAD = newHEAD
	f.lastUpdated = time.Now()
	f.mu.Unlock()

	return updated, nil
}

// filesChangedBetweenWithRepo returns relative file paths changed between two
// commits, using a pre-opened repo handle.
func filesChangedBetweenWithRepo(repo *git.Repository, oldHash, newHash string) ([]string, error) {
	oldCommit, err := repo.CommitObject(plumbing.NewHash(oldHash))
	if err != nil {
		return nil, fmt.Errorf("resolve old commit: %w", err)
	}
	newCommit, err := repo.CommitObject(plumbing.NewHash(newHash))
	if err != nil {
		return nil, fmt.Errorf("resolve new commit: %w", err)
	}

	oldTree, err := oldCommit.Tree()
	if err != nil {
		return nil, err
	}
	newTree, err := newCommit.Tree()
	if err != nil {
		return nil, err
	}

	changes, err := object.DiffTree(oldTree, newTree)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	for _, c := range changes {
		if c.To.Name != "" {
			seen[c.To.Name] = true
		}
		if c.From.Name != "" {
			seen[c.From.Name] = true
		}
	}

	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	return paths, nil
}

// resolveHEAD returns the current HEAD commit hash, or "" on error.
func resolveHEAD(workspaceRoot string) string {
	repo, err := git.PlainOpen(workspaceRoot)
	if err != nil {
		return ""
	}
	ref, err := repo.Head()
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}

// Fovea returns the top n files by salience score (the "focal" context).
// If n <= 0, all files are returned.
func (f *AttentionalField) Fovea(n int) []FileScore {
	f.mu.RLock()
	defer f.mu.RUnlock()

	all := make([]FileScore, 0, len(f.scores))
	for path, score := range f.scores {
		all = append(all, FileScore{Path: path, Score: score})
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Score > all[j].Score
	})
	if n > 0 && len(all) > n {
		return all[:n]
	}
	return all
}

// Score returns the current salience score for a single file.
// Returns 0.0 if the file is not in the field.
func (f *AttentionalField) Score(path string) float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.scores[path]
}

// Len returns the number of files currently in the field.
func (f *AttentionalField) Len() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.scores)
}

// LastUpdated returns when the field was last recomputed.
func (f *AttentionalField) LastUpdated() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastUpdated
}

// Boost adds delta to the score for path. Used by attention signals to
// apply a transient recency boost without a full field recomputation.
// The boost is overwritten on the next Update() call.
func (f *AttentionalField) Boost(path string, delta float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scores[path] += delta
}

// AllScores returns a copy of the full path→score map.
// Safe for external iteration (callers get a snapshot, not a live map).
func (f *AttentionalField) AllScores() map[string]float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]float64, len(f.scores))
	for k, v := range f.scores {
		out[k] = v
	}
	return out
}

// ── Inbox awareness ──────────────────────────────────────────────────────────

// applyInboxBoosts scans the score map for files whose path contains
// /inbox/ and applies a salience bonus based on their frontmatter status.
//
//   - status: raw      → +inboxRawBoost      (needs enrichment)
//   - status: enriched → +inboxEnrichedBoost  (needs integration)
//   - status: integrated or missing → no boost (already processed)
//
// This ensures newly ingested items spike the attentional field so the
// observer loop notices them without requiring a separate registration step.
func applyInboxBoosts(scores map[string]float64) {
	for path, score := range scores {
		if !strings.Contains(path, inboxPathFragment) {
			continue
		}
		status := readInboxStatus(path)
		switch status {
		case "raw":
			scores[path] = score + inboxRawBoost
		case "enriched":
			scores[path] = score + inboxEnrichedBoost
		}
	}
}

// readInboxStatus reads just enough of a file's YAML frontmatter to extract
// the status field. Returns "" if the file can't be read or has no status.
//
// Only reads up to 20 lines to keep it fast — frontmatter is always at the
// top, and inbox CogDocs have small headers.
func readInboxStatus(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)

	// First line must be "---"
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return ""
	}

	// Scan up to 20 lines looking for status: and the closing ---
	for i := 0; i < 20 && scanner.Scan(); i++ {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		if strings.HasPrefix(line, "status:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "status:"))
			// Strip optional YAML quotes
			val = strings.Trim(val, "\"'")
			return val
		}
	}
	return ""
}

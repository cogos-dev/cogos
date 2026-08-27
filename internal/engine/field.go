// field.go — CogOS v3 attentional field
//
// The attentional field is the continuous salience map over the memory corpus.
// Every memory file gets a float64 score. The "fovea" is the top-N files by
// score that fit in the context window.
//
// In v2, salience was computed once per session at context assembly time.
// In v3, the field is updated continuously by the process loop, decoupled
// from any external request.
package engine

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
//
// The field maintains two parallel views over the same set of paths:
//
//   - base:     pure salience score, no inbox-status boosts.
//     This is the chat-read view — what the foveated assembler sees
//     when it asks "is this CogDoc relevant to the current query?".
//     Bulk-imported inbox files do not get a free pass into chat context.
//
//   - observer: base + inbox boosts (raw / enriched).
//     This is the observer-loop view — "what needs the substrate's
//     attention?". Inbox items deliberately spike here so the
//     enrichment/integration pipeline notices them.
//
// Both views are populated in the same scan (Update / deltaUpdate). Transient
// recency boosts via Boost() apply to both views — a freshly-read or
// freshly-attended CogDoc is relevant to chat *and* observer.
type AttentionalField struct {
	mu sync.RWMutex

	// base maps absolute file path → salience score with no inbox boosts.
	// This is the chat-read view.
	base map[string]float64

	// observer maps absolute file path → base + inbox boosts.
	// This is the observer-loop view.
	observer map[string]float64

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
		base:     make(map[string]float64),
		observer: make(map[string]float64),
		cfg:      cfg,
		salCfg:   DefaultSalienceConfig(),
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
	hasScores := len(f.base) > 0
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

	freshBase := make(map[string]float64, len(ranked))
	for _, fs := range ranked {
		freshBase[fs.Path] = fs.Score
	}
	freshObserver := make(map[string]float64, len(ranked))
	for path, score := range freshBase {
		freshObserver[path] = score
	}
	applyInboxBoosts(freshObserver)

	f.mu.Lock()
	f.base = freshBase
	f.observer = freshObserver
	f.lastUpdated = time.Now()
	f.lastHEAD = currentHEAD
	f.mu.Unlock()

	slog.Info("field: full scan complete", "files", len(freshBase))
	return nil
}

// deltaUpdate rescores only files changed between oldHEAD and newHEAD.
// Opens the repo exactly once and reuses the handle for both diffing and scoring.
// Returns the number of files updated.
//
// #563: this used to call computeFileSalienceWithRepo once per changed file,
// each call running its own independent commit-graph walk — O(changed_files
// x commits) and, per-file, unbounded by daysWindow (see the note on
// computeFileSalienceWithRepo). With consolidation running continuously,
// that was ~21% of kernel CPU on a live node. It now collects stats for
// every changed memory file in a single date-bounded walk (batchCollectStats,
// the same walk RankFilesBySalience's full scan already uses), so a delta
// touching N files costs one walk of the commits-in-window, not N.
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

	// Partition changed paths: files that no longer exist are deleted from
	// the field immediately; files that still exist go into the scope set
	// for the shared batch walk below.
	relToAbs := make(map[string]string, len(changed))
	updated := 0
	for _, relPath := range changed {
		absPath := filepath.Join(f.cfg.WorkspaceRoot, filepath.FromSlash(relPath))
		if !strings.HasPrefix(absPath, memPrefix) {
			continue // not a memory file
		}
		if _, err := os.Stat(absPath); os.IsNotExist(err) {
			f.mu.Lock()
			delete(f.base, absPath)
			delete(f.observer, absPath)
			f.mu.Unlock()
			updated++
			continue
		}
		relToAbs[relPath] = absPath
	}

	if len(relToAbs) > 0 {
		stats, err := batchCollectStats(repo, relToAbs, f.cfg.SalienceDaysWindow)
		if err != nil {
			return 0, fmt.Errorf("delta batch stats: %w", err)
		}
		scores := batchComputeScores(stats, relToAbs, f.salCfg)

		// Compute inbox-boosted observer values outside the lock (readInboxStatus
		// does file I/O), then apply all of them under a single lock acquisition.
		type scored struct {
			path     string
			base     float64
			observer float64
		}
		results := make([]scored, 0, len(scores))
		for _, fs := range scores {
			baseVal := fs.Score
			observerVal := baseVal
			if strings.Contains(fs.Path, inboxPathFragment) {
				switch readInboxStatus(fs.Path) {
				case "raw":
					observerVal += inboxRawBoost
				case "enriched":
					observerVal += inboxEnrichedBoost
				}
			}
			results = append(results, scored{fs.Path, baseVal, observerVal})
		}

		f.mu.Lock()
		for _, r := range results {
			f.base[r.path] = r.base
			f.observer[r.path] = r.observer
		}
		f.mu.Unlock()
		updated += len(results)
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

// Fovea returns the top n files by observer salience score (the "focal"
// context — what currently has the substrate's attention, including inbox
// urgency). Surfaces this view via constellation/fovea, MCP field resources,
// and the substrate health panels.
//
// For the chat-read view (no inbox boosts), use BaseFovea.
//
// If n <= 0, all files are returned.
func (f *AttentionalField) Fovea(n int) []FileScore {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return topN(f.observer, n)
}

// BaseFovea returns the top n files by base salience (no inbox boosts),
// the chat-read view. Currently unused outside tests/diagnostics, but
// kept symmetric with Fovea for callers that want the unboosted ranking.
func (f *AttentionalField) BaseFovea(n int) []FileScore {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return topN(f.base, n)
}

func topN(scores map[string]float64, n int) []FileScore {
	all := make([]FileScore, 0, len(scores))
	for path, score := range scores {
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

// Score returns the base salience score for a single file (no inbox boosts).
// This is the chat-read view: the foveated assembler uses Score to decide
// which CogDocs are relevant to the user's current query, without bulk
// inbox imports flooding the context. Returns 0.0 if the file is not in
// the field.
//
// For the observer view (base + inbox boosts), use ObserverScore.
func (f *AttentionalField) Score(path string) float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.base[path]
}

// ObserverScore returns the observer salience score for a single file
// (base + inbox boosts). This is the observer-loop view: "what needs
// the substrate's attention right now?". Inbox items with status: raw
// or status: enriched receive a flat-add boost so the enrichment and
// integration pipelines notice them. Returns 0.0 if the file is not
// in the field.
func (f *AttentionalField) ObserverScore(path string) float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.observer[path]
}

// Len returns the number of files currently in the field.
func (f *AttentionalField) Len() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.base)
}

// LastUpdated returns when the field was last recomputed.
func (f *AttentionalField) LastUpdated() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastUpdated
}

// Boost adds delta to the score for path in both the base and observer
// views. Used by attention signals (CogDoc reads, MCP attention emits,
// observer warming/attenuation) to apply a transient recency boost
// without a full field recomputation. The boost is overwritten on the
// next Update() call.
//
// Recency reflects actual user/agent activity, so it should influence
// chat-read salience just as it does observer salience — a freshly-read
// CogDoc is relevant to the current turn.
func (f *AttentionalField) Boost(path string, delta float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.base[path] += delta
	f.observer[path] += delta
}

// AllScores returns a copy of the full path→observer-score map (the
// observer view, including inbox boosts). Used by the observer loop
// and substrate-health surfaces (MCP field resources, adjacency).
//
// For the chat-read view (no inbox boosts), use AllBaseScores.
//
// Safe for external iteration (callers get a snapshot, not a live map).
func (f *AttentionalField) AllScores() map[string]float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return copyScoreMap(f.observer)
}

// AllBaseScores returns a copy of the full path→base-score map (no
// inbox boosts). Currently unused outside tests/diagnostics, but kept
// symmetric with AllScores.
func (f *AttentionalField) AllBaseScores() map[string]float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return copyScoreMap(f.base)
}

func copyScoreMap(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
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
// This ensures newly ingested items spike the observer view of the
// attentional field so the enrichment/integration pipeline notices them
// without requiring a separate registration step.
//
// Caller MUST pass the observer map only. The base map (chat-read view)
// is intentionally left untouched — chat assemblers should not see bulk
// inbox imports as high-salience candidates.
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

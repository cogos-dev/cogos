// provider.go — Reconcilable implementation for the Conversations Observatory.
//
// Implements pkg/reconcile.Reconcilable with resource type "conversations".
//
// Method summary:
//   Type()        — "conversations"
//   LoadConfig()  — scan source dirs for JSONL files; load .cog/config/observatory.yaml
//   FetchLive()   — load index from .cog/state/conversations/_meta.json
//   ComputePlan() — diff source files vs index entries; plan create/update/delete/skip
//   ApplyPlan()   — stream-parse new/changed sessions into the index
//   BuildState()  — construct Terraform-style state from live index entries
//   Health()      — Healthy/Degraded/OutOfSync based on index drift and errors
package conversations

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
	"gopkg.in/yaml.v3"
)

// Provider implements reconcile.Reconcilable for the Conversations Observatory.
type Provider struct {
	mu sync.Mutex

	// index is the in-memory queryable index. Populated by ApplyPlan.
	// nil until first LoadConfig call resolves projDir.
	index *Index

	// state populated during the reconcile loop.
	root            string
	lastPlanSummary reconcile.Summary
	lastErrors      []string
	operation       reconcile.OperationPhase
}

// NewProvider constructs a ConversationsProvider. The root workspace path
// is set by LoadConfig. Tests may call LoadConfig directly with a temp dir.
func NewProvider() *Provider {
	return &Provider{
		operation: reconcile.OperationIdle,
	}
}

// Type returns the resource type identifier.
func (p *Provider) Type() string { return "conversations" }

// ─── LoadConfig ──────────────────────────────────────────────────────────────

// LoadConfig reads .cog/config/observatory.yaml (if present), discovers JSONL
// source files in each configured SourceDir, and initialises the index if not
// already loaded.
func (p *Provider) LoadConfig(root string) (any, error) {
	p.mu.Lock()
	p.root = root
	p.mu.Unlock()

	obs, err := loadObservatoryConfig(root)
	if err != nil {
		return nil, fmt.Errorf("conversations: load observatory config: %w", err)
	}

	// Initialise the index (idempotent — only creates if nil).
	projDir := filepath.Join(root, ".cog", "state", "conversations")
	p.mu.Lock()
	if p.index == nil {
		idx, idxErr := NewIndex(projDir)
		if idxErr != nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("conversations: init index: %w", idxErr)
		}
		p.index = idx
	}
	p.mu.Unlock()

	// Discover source JSONL files.
	files, err := discoverSourceFiles(obs)
	if err != nil {
		return nil, fmt.Errorf("conversations: discover source files: %w", err)
	}

	return &providerConfig{
		Root:        root,
		Observatory: obs,
		SourceFiles: files,
	}, nil
}

// ─── FetchLive ───────────────────────────────────────────────────────────────

// FetchLive loads (or refreshes) the index from disk and returns the current
// set of indexed entries.
func (p *Provider) FetchLive(_ context.Context, _ any) (any, error) {
	p.mu.Lock()
	idx := p.index
	p.mu.Unlock()

	if idx == nil {
		// Index not yet initialised — nothing indexed yet.
		return &liveState{Entries: make(map[string]IndexEntry)}, nil
	}

	// Reload from disk to pick up changes from other processes.
	if err := idx.Load(); err != nil {
		return nil, fmt.Errorf("conversations: load index: %w", err)
	}

	metas := idx.ListSessions(time.Time{}, time.Time{}, "")
	entries := make(map[string]IndexEntry, len(metas))
	for _, m := range metas {
		entries[m.SessionID] = IndexEntry{
			Meta:  m,
			Depth: DepthFull,
		}
	}
	return &liveState{Entries: entries}, nil
}

// ─── ComputePlan ─────────────────────────────────────────────────────────────

// ComputePlan diffs the set of source JSONL files against the live index:
//   - source present, not indexed          → create
//   - source present, indexed but stale    → update (mtime or size changed)
//   - source absent, indexed               → delete
//   - source present, indexed, up-to-date  → skip
func (p *Provider) ComputePlan(config any, live any, _ *reconcile.State) (*reconcile.Plan, error) {
	cfg, ok := config.(*providerConfig)
	if !ok {
		return nil, fmt.Errorf("conversations: expected *providerConfig, got %T", config)
	}
	ls, ok := live.(*liveState)
	if !ok {
		return nil, fmt.Errorf("conversations: expected *liveState, got %T", live)
	}

	plan := &reconcile.Plan{
		ResourceType: "conversations",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath:   filepath.Join(cfg.Root, ".cog", "config", "observatory.yaml"),
	}

	sourceSet := make(map[string]sourceFileInfo, len(cfg.SourceFiles))
	for _, f := range cfg.SourceFiles {
		sourceSet[f.SessionID] = f
	}

	// Walk source files.
	for _, f := range cfg.SourceFiles {
		existing, indexed := ls.Entries[f.SessionID]
		switch {
		case !indexed:
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionCreate,
				ResourceType: "conversations",
				Name:         f.SessionID,
				Details: map[string]any{
					"source_path":   f.Path,
					"mtime":         f.Mtime.Format(time.RFC3339),
					"size":          f.Size,
					"is_ingest":     f.IsIngest,
					"ingest_source": f.IngestSource,
				},
			})
			plan.Summary.Creates++

		case isDrift(existing.Meta, f):
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionUpdate,
				ResourceType: "conversations",
				Name:         f.SessionID,
				Details: map[string]any{
					"source_path":   f.Path,
					"mtime":         f.Mtime.Format(time.RFC3339),
					"size":          f.Size,
					"prev_mtime":    existing.Meta.SourceMtime.Format(time.RFC3339),
					"prev_size":     existing.Meta.SourceSize,
					"prev_turns":    existing.Meta.TurnCount,
					"is_ingest":     f.IsIngest,
					"ingest_source": f.IngestSource,
				},
			})
			plan.Summary.Updates++

		default:
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionSkip,
				ResourceType: "conversations",
				Name:         f.SessionID,
				Details: map[string]any{
					"reason": "in sync",
					"turns":  existing.Meta.TurnCount,
				},
			})
			plan.Summary.Skipped++
		}
	}

	// Sessions in index but no longer in source.
	for sid := range ls.Entries {
		if _, inSource := sourceSet[sid]; !inSource {
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionDelete,
				ResourceType: "conversations",
				Name:         sid,
				Details: map[string]any{
					"reason": "source removed",
				},
			})
			plan.Summary.Deletes++
		}
	}

	sort.Slice(plan.Actions, func(i, j int) bool {
		if plan.Actions[i].Action != plan.Actions[j].Action {
			return plan.Actions[i].Action < plan.Actions[j].Action
		}
		return plan.Actions[i].Name < plan.Actions[j].Name
	})

	p.mu.Lock()
	p.lastPlanSummary = plan.Summary
	p.mu.Unlock()

	return plan, nil
}

// ─── ApplyPlan ───────────────────────────────────────────────────────────────

// ApplyPlan executes the plan. For create/update, stream-parses the source
// JSONL and writes the result to the index. For delete, removes the session.
func (p *Provider) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	if plan == nil {
		return nil, fmt.Errorf("conversations: nil plan")
	}

	p.mu.Lock()
	p.operation = reconcile.OperationSyncing
	idx := p.index
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.operation = reconcile.OperationIdle
		p.mu.Unlock()
	}()

	if idx == nil {
		return nil, fmt.Errorf("conversations: index not initialised (LoadConfig not called)")
	}

	var results []reconcile.Result
	var errs []string

	for _, action := range plan.Actions {
		if action.Action == reconcile.ActionSkip {
			continue
		}

		res := reconcile.Result{
			Phase:  "conversations",
			Action: string(action.Action),
			Name:   action.Name,
		}

		switch action.Action {
		case reconcile.ActionCreate, reconcile.ActionUpdate:
			sourcePath, _ := action.Details["source_path"].(string)
			if sourcePath == "" {
				res.Status = reconcile.ApplyFailed
				res.Error = "missing source_path in plan action"
				results = append(results, res)
				errs = append(errs, res.Error)
				continue
			}
			isIngest, _ := action.Details["is_ingest"].(bool)
			ingestSource, _ := action.Details["ingest_source"].(string)
			sf := sourceFileInfo{
				Path:         sourcePath,
				SessionID:    action.Name,
				IsIngest:     isIngest,
				IngestSource: ingestSource,
			}

			meta, turns, err := indexSession(sourcePath, action.Name, defaultMaxTurnLen, sf)
			if err != nil {
				res.Status = reconcile.ApplyFailed
				res.Error = fmt.Sprintf("index session %s: %v", action.Name, err)
				results = append(results, res)
				errs = append(errs, res.Error)
				continue
			}

			if upsertErr := idx.UpsertSession(meta, turns); upsertErr != nil {
				res.Status = reconcile.ApplyFailed
				res.Error = fmt.Sprintf("upsert session %s: %v", action.Name, upsertErr)
				results = append(results, res)
				errs = append(errs, res.Error)
				continue
			}

			res.Status = reconcile.ApplySucceeded
			res.CreatedID = action.Name
			results = append(results, res)

		case reconcile.ActionDelete:
			if delErr := idx.DeleteSession(action.Name); delErr != nil {
				res.Status = reconcile.ApplyFailed
				res.Error = delErr.Error()
				errs = append(errs, res.Error)
			} else {
				res.Status = reconcile.ApplySucceeded
			}
			results = append(results, res)

		default:
			res.Status = reconcile.ApplySkipped
			results = append(results, res)
		}
	}

	p.mu.Lock()
	p.lastErrors = errs
	p.mu.Unlock()

	return results, nil
}

// ─── BuildState ──────────────────────────────────────────────────────────────

// BuildState constructs a Terraform-style state from live index entries.
func (p *Provider) BuildState(_ any, live any, existing *reconcile.State) (*reconcile.State, error) {
	ls, ok := live.(*liveState)
	if !ok {
		return nil, fmt.Errorf("conversations: expected *liveState, got %T", live)
	}

	state := &reconcile.State{
		Version:      1,
		ResourceType: "conversations",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Resources:    []reconcile.Resource{},
		Metadata:     map[string]any{},
	}

	if existing != nil && existing.Lineage != "" {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	} else {
		state.Lineage = "conversations-" + uuid.New().String()
		state.Serial = 1
	}

	// Sort by session_id for deterministic state output.
	sids := make([]string, 0, len(ls.Entries))
	for sid := range ls.Entries {
		sids = append(sids, sid)
	}
	sort.Strings(sids)

	now := time.Now().UTC().Format(time.RFC3339)
	for _, sid := range sids {
		entry := ls.Entries[sid]
		m := entry.Meta
		state.Resources = append(state.Resources, reconcile.Resource{
			Address:       "conversations." + sid,
			Type:          "conversations",
			Mode:          reconcile.ModeData,
			ExternalID:    sid,
			Name:          sid,
			LastRefreshed: now,
			Attributes: map[string]any{
				"source_path":   m.SourcePath,
				"source":        m.Source,
				"turn_count":    m.TurnCount,
				"first_turn_at": m.FirstTurnAt.Format(time.RFC3339),
				"last_turn_at":  m.LastTurnAt.Format(time.RFC3339),
				"indexed_at":    m.IndexedAt.Format(time.RFC3339),
				"source_mtime":  m.SourceMtime.Format(time.RFC3339),
				"source_size":   m.SourceSize,
				"identity":      m.Identity,
				"entrypoint":    m.Entrypoint,
				"title":         m.Title,
				"depth":         string(entry.Depth),
			},
		})
	}

	// Summary metadata.
	state.Metadata["indexed_sessions"] = len(ls.Entries)
	totalTurns := 0
	for _, e := range ls.Entries {
		totalTurns += e.Meta.TurnCount
	}
	state.Metadata["total_turns"] = totalTurns

	return state, nil
}

// ─── Health ───────────────────────────────────────────────────────────────────

// Health returns three-axis status:
//   Sync      — Synced when last plan had no non-skip actions
//   Health    — Degraded when ApplyPlan had errors; Healthy otherwise
//   Operation — Syncing while ApplyPlan is running
func (p *Provider) Health() reconcile.ResourceStatus {
	p.mu.Lock()
	summary := p.lastPlanSummary
	errs := len(p.lastErrors)
	op := p.operation
	p.mu.Unlock()

	sync := reconcile.SyncStatusSynced
	if summary.HasChanges() {
		sync = reconcile.SyncStatusOutOfSync
	}

	health := reconcile.HealthHealthy
	msg := ""
	if errs > 0 {
		health = reconcile.HealthDegraded
		msg = fmt.Sprintf("%d session(s) failed to index", errs)
	}

	return reconcile.ResourceStatus{
		Sync:      sync,
		Health:    health,
		Operation: op,
		Message:   msg,
	}
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

const defaultMaxTurnLen = 8192

// defaultSourceDirs returns the default JSONL source directories to scan.
// Prefers ~/.claude/projects/-Users-slowbro; falls back gracefully.
func defaultSourceDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	candidate := filepath.Join(home, ".claude", "projects", "-Users-slowbro")
	if _, err := os.Stat(candidate); err == nil {
		return []string{candidate}
	}
	// Also check the generic projects dir.
	generic := filepath.Join(home, ".claude", "projects")
	if _, err := os.Stat(generic); err == nil {
		return []string{generic}
	}
	return nil
}

// loadObservatoryConfig reads .cog/config/observatory.yaml. Missing file is
// not an error — returns defaults.
func loadObservatoryConfig(root string) (ObservatoryConfig, error) {
	path := filepath.Join(root, ".cog", "config", "observatory.yaml")
	var obs ObservatoryConfig

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Default config.
			obs.SourceDirs = defaultSourceDirs()
			obs.IngestDirs = defaultIngestDirs(root)
			obs.IncludePatterns = []string{"*.jsonl"}
			obs.MaxTurnLength = defaultMaxTurnLen
			return obs, nil
		}
		return obs, fmt.Errorf("read observatory.yaml: %w", err)
	}

	if err := yaml.Unmarshal(data, &obs); err != nil {
		return obs, fmt.Errorf("parse observatory.yaml: %w", err)
	}

	// Fill in defaults for zero values.
	if len(obs.SourceDirs) == 0 {
		obs.SourceDirs = defaultSourceDirs()
	}
	if len(obs.IngestDirs) == 0 {
		obs.IngestDirs = defaultIngestDirs(root)
	}
	if len(obs.IncludePatterns) == 0 {
		obs.IncludePatterns = []string{"*.jsonl"}
	}
	if obs.MaxTurnLength <= 0 {
		obs.MaxTurnLength = defaultMaxTurnLen
	}

	return obs, nil
}

// defaultIngestDirs returns the default ingest root directory for the given
// workspace root: <root>/.cog/observatory/ingest. Returns nil if absent.
func defaultIngestDirs(root string) []string {
	candidate := filepath.Join(root, ".cog", "observatory", "ingest")
	if _, err := os.Stat(candidate); err == nil {
		return []string{candidate}
	}
	return nil
}

// discoverSourceFiles scans SourceDirs (CC UUID JSONL) and IngestDirs
// (normalized ingest surface) and returns the union as sourceFileInfo entries.
func discoverSourceFiles(obs ObservatoryConfig) ([]sourceFileInfo, error) {
	var files []sourceFileInfo
	seen := make(map[string]struct{})

	// ── CC source_dirs path ──────────────────────────────────────────────────
	for _, dir := range obs.SourceDirs {
		expanded := expandHome(dir)
		entries, err := os.ReadDir(expanded)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read dir %s: %w", expanded, err)
		}

		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !matchesPatterns(name, obs.IncludePatterns) {
				continue
			}
			if matchesPatterns(name, obs.ExcludePatterns) {
				continue
			}
			// Only accept UUID-named JSONL files (Claude Code session format).
			sid := sessionIDFromFilename(name)
			if sid == "" {
				continue
			}
			absPath := filepath.Join(expanded, name)
			if _, dup := seen[absPath]; dup {
				continue
			}
			seen[absPath] = struct{}{}

			fi, err := e.Info()
			if err != nil {
				continue
			}
			files = append(files, sourceFileInfo{
				Path:      absPath,
				SessionID: sid,
				Mtime:     fi.ModTime(),
				Size:      fi.Size(),
			})
		}
	}

	// ── Normalized ingest path ───────────────────────────────────────────────
	ingestFiles, err := discoverIngestFiles(obs)
	if err != nil {
		return nil, err
	}
	for _, f := range ingestFiles {
		if _, dup := seen[f.Path]; dup {
			continue
		}
		seen[f.Path] = struct{}{}
		files = append(files, f)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].SessionID < files[j].SessionID
	})
	return files, nil
}

// discoverIngestFiles scans IngestDirs for <source>/*.jsonl files conforming
// to the normalized ingest surface schema. Returns one sourceFileInfo per file
// with IsIngest=true and SessionID keyed as "<source>/<filename_stem>".
func discoverIngestFiles(obs ObservatoryConfig) ([]sourceFileInfo, error) {
	var files []sourceFileInfo

	for _, ingestRoot := range obs.IngestDirs {
		expanded := expandHome(ingestRoot)
		sourceDirs, err := os.ReadDir(expanded)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read ingest dir %s: %w", expanded, err)
		}

		for _, sourceEntry := range sourceDirs {
			if !sourceEntry.IsDir() {
				continue
			}
			sourceName := sourceEntry.Name()
			sourceDir := filepath.Join(expanded, sourceName)

			jsonlEntries, err := os.ReadDir(sourceDir)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("read ingest source dir %s: %w", sourceDir, err)
			}

			for _, je := range jsonlEntries {
				if je.IsDir() {
					continue
				}
				name := je.Name()
				if !strings.HasSuffix(name, ".jsonl") {
					continue
				}
				stem := strings.TrimSuffix(name, ".jsonl")
				// Composite index key: "<source>/<filename_stem>".
				indexKey := indexKeyForIngest(sourceName, stem)
				absPath := filepath.Join(sourceDir, name)

				fi, err := je.Info()
				if err != nil {
					continue
				}
				files = append(files, sourceFileInfo{
					Path:         absPath,
					SessionID:    indexKey,
					Mtime:        fi.ModTime(),
					Size:         fi.Size(),
					IsIngest:     true,
					IngestSource: sourceName,
				})
			}
		}
	}
	return files, nil
}

// sessionIDFromFilename extracts the UUID from a filename like
// "3f9a1234-....jsonl". Returns "" if the filename is not UUID-format.
func sessionIDFromFilename(name string) string {
	base := strings.TrimSuffix(name, ".jsonl")
	if base == name {
		return "" // no .jsonl suffix
	}
	if _, err := uuid.Parse(base); err != nil {
		return "" // not a UUID
	}
	return base
}

// matchesPatterns returns true if name matches any of the glob patterns.
// An empty patterns list returns false (nothing matches).
func matchesPatterns(name string, patterns []string) bool {
	for _, pat := range patterns {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// expandHome replaces a leading ~ with the user home directory.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}

// isDrift returns true when the indexed meta is stale compared to f.
// Primary signal: size change (definitive — any content change changes size).
// Secondary signal: mtime difference > 1s (guards against same-size rewrites).
//
// mtime is compared with 2-second tolerance to guard against filesystem mtime
// resolution differences between `os.ReadDir` calls on fast-write filesystems
// (e.g. Linux tmpfs in CI runners). A 1-byte change always changes size, so
// the 2s tolerance only matters for truly same-size content changes (rare in
// practice for session JSONLs, which grow monotonically).
func isDrift(meta SessionMeta, f sourceFileInfo) bool {
	// Size change is the definitive fast path.
	if meta.SourceSize != f.Size {
		return true
	}
	// mtime with 2s tolerance for filesystem-level mtime granularity jitter.
	diff := meta.SourceMtime.Sub(f.Mtime)
	if diff < 0 {
		diff = -diff
	}
	return diff > 2*time.Second
}

// indexSession opens sourcePath, streams turns, and returns the resulting
// SessionMeta and Turn slice. Does not hold the file open after return.
//
// When sf.IsIngest is true the normalized ingest parser (ingest_parser.go) is
// used instead of the CC JSONL parser; sf.IngestSource is populated into the
// returned SessionMeta.Source.
func indexSession(sourcePath, sessionID string, maxTurnLen int, sf sourceFileInfo) (SessionMeta, []Turn, error) {
	fi, err := os.Stat(sourcePath)
	if err != nil {
		return SessionMeta{}, nil, fmt.Errorf("stat %s: %w", sourcePath, err)
	}

	f, err := os.Open(sourcePath)
	if err != nil {
		return SessionMeta{}, nil, fmt.Errorf("open %s: %w", sourcePath, err)
	}
	defer f.Close()

	meta := SessionMeta{
		SessionID:   sessionID,
		Source:      sf.IngestSource,
		SourcePath:  sourcePath,
		IndexedAt:   time.Now().UTC(),
		SourceMtime: fi.ModTime(),
		SourceSize:  fi.Size(),
	}

	var turns []Turn

	if sf.IsIngest {
		rejected, parseErr := ParseIngestSession(f, sessionID, maxTurnLen, &meta, func(t Turn) bool {
			turns = append(turns, t)
			return true
		})
		if parseErr != nil {
			return meta, turns, fmt.Errorf("parse ingest %s: %w", sourcePath, parseErr)
		}
		if rejected > 0 {
			// Non-fatal: we already logged per-record rejections.
			_ = rejected
		}
		return meta, turns, nil
	}

	err = ParseSession(f, sessionID, maxTurnLen, &meta, func(t Turn) bool {
		turns = append(turns, t)
		return true
	})
	if err != nil {
		return meta, turns, fmt.Errorf("parse %s: %w", sourcePath, err)
	}

	return meta, turns, nil
}

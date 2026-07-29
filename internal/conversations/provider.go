// provider.go — Reconcilable implementation for the Conversations Observatory.
//
// Implements pkg/reconcile.Reconcilable with resource type "conversations".
//
// Method summary:
//
//	Type()        — "conversations"
//	LoadConfig()  — scan source dirs for JSONL files; load .cog/config/observatory.yaml
//	FetchLive()   — load index from .cog/state/conversations/_meta.json
//	ComputePlan() — diff source files vs index entries; plan create/update/delete/skip
//	ApplyPlan()   — stream-parse new/changed sessions into the index
//	BuildState()  — construct Terraform-style state from live index entries
//	Health()      — Healthy/Degraded/OutOfSync based on index drift and errors
package conversations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/myrgic/cogos/pkg/filelock"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
	"gopkg.in/yaml.v3"
)

// Provider implements reconcile.Reconcilable for the Conversations Observatory.
type Provider struct {
	mu sync.Mutex

	// applyMu serializes ApplyPlan calls against this Provider instance. See
	// ApplyPlan's doc comment (issue #494 remedy 4) for why this is needed
	// even though the on-disk index already has its own cross-process
	// filelock: flock(2) does not serialize two callers from within the
	// SAME process, so without this, the reconcile daemon and the autonomic
	// ticker's self-heal could both enter ApplyPlan for this provider at
	// once and deadlock each other out on that same flock. Deliberately a
	// plain sync.Mutex used only via TryLock (never Lock) — the loser skips
	// its cycle rather than queueing, which is the point: queuing would
	// still serialize the two callers' large applies back-to-back and keep
	// the provider busy for their combined duration.
	applyMu sync.Mutex

	// index is the in-memory queryable index. Populated by ApplyPlan.
	// nil until first LoadConfig call resolves projDir.
	index *Index

	// state populated during the reconcile loop.
	root            string
	lastPlanSummary reconcile.Summary
	lastErrors      []string
	operation       reconcile.OperationPhase

	// planApplied reports whether the plan described by lastPlanSummary has
	// since been applied. ComputePlan clears it; ApplyPlan sets it. It is the
	// difference between "changes are pending" (genuine, momentary divergence)
	// and "changes were planned and then resolved" (the reconciler working
	// normally). Without it, any actively-appending source holds the provider
	// in OutOfSync forever — see Health.
	planApplied bool

	// applyFailures counts actions in the last apply that failed. Non-zero
	// means planned work is not resolving, which is the real definition of
	// OutOfSync.
	applyFailures int

	// applyBackpressure counts actions in the last apply that failed solely
	// because an on-disk index lock could not be acquired in time. These are
	// deliberately NOT counted as indexing errors: losing a race for the meta
	// lock means another writer (the reconcile daemon, self-heal, or another
	// process) currently holds it, so the work is contended, not corrupt.
	// Reporting contention as Degraded closed a feedback loop — Degraded
	// re-armed self-heal, self-heal raced the daemon, the race produced
	// another timeout (#482).
	applyBackpressure int

	// ontology is the loaded L1+L2 ontology set. nil when enforcement is
	// disabled (ontology_dir not set or absent from the workspace).
	ontology *LoadedOntology

	// quarantine is the writer for quarantined records.
	quarantine *QuarantineWriter

	// coverage accumulates per-source coverage metrics across reconcile runs.
	coverage *CoverageTracker

	// coverageCache holds the last computed coverage per ingest source so that
	// unchanged (ActionSkip) sources can be served from cache instead of being
	// re-parsed every reconcile cycle. Guarded by p.mu. ActionSkip is itself the
	// drift-free signal, so a cached entry is valid whenever present.
	coverageCache map[string]SourceCoverage
}

// NewProvider constructs a ConversationsProvider. The root workspace path
// is set by LoadConfig. Tests may call LoadConfig directly with a temp dir.
func NewProvider() *Provider {
	return &Provider{
		operation:     reconcile.OperationIdle,
		coverage:      NewCoverageTracker(),
		coverageCache: make(map[string]SourceCoverage),
	}
}

// Coverage returns a snapshot of the current per-source coverage metrics.
// The returned map is a copy and safe to read without holding the provider lock.
func (p *Provider) Coverage() map[string]SourceCoverage {
	p.mu.Lock()
	cov := p.coverage
	p.mu.Unlock()
	if cov == nil {
		return make(map[string]SourceCoverage)
	}
	return cov.All()
}

// Ontology returns the loaded ontology (may be nil when enforcement is disabled).
func (p *Provider) Ontology() *LoadedOntology {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ontology
}

// Type returns the resource type identifier.
func (p *Provider) Type() string { return "conversations" }

// ResolveURI resolves a cog:conversations/… URI against the live index.
// Returns ErrURIUnknownParam (or a parse error) when the URI is invalid.
// Returns an error when the index is not yet initialised.
// This method is the entry point used by engine.ConversationsResolver.
func (p *Provider) ResolveURI(uri string) (*ResolvedSlice, error) {
	p.mu.Lock()
	idx := p.index
	ont := p.ontology
	p.mu.Unlock()

	if idx == nil {
		return nil, fmt.Errorf("conversations index not initialised — run cog reconcile conversations first")
	}
	return ResolveConversationURIWithOntology(uri, idx, ont)
}

// ─── LoadConfig ──────────────────────────────────────────────────────────────

// LoadConfig reads .cog/config/observatory.yaml (if present), discovers JSONL
// source files in each configured SourceDir, initialises the index if not
// already loaded, and loads the ontology from the ontology_dir when present.
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

	// Load ontology + mappings from ontology_dir (always attempt; missing dir is ok).
	ontologyDir := obs.OntologyDir
	if ontologyDir == "" {
		ontologyDir = defaultOntologyDir(root)
	}
	lo, ontErr := LoadOntologyDir(expandHome(ontologyDir))
	if ontErr != nil {
		return nil, fmt.Errorf("conversations: load ontology: %w", ontErr)
	}

	// Set up quarantine writer and coverage tracker.
	quarantineDir := filepath.Join(root, QuarantineDir)

	p.mu.Lock()
	p.ontology = lo
	p.quarantine = NewQuarantineWriter(quarantineDir)
	if p.coverage == nil {
		p.coverage = NewCoverageTracker()
	}
	if p.coverageCache == nil {
		p.coverageCache = make(map[string]SourceCoverage)
	}
	p.mu.Unlock()

	// Discover CC source JSONL files.
	files, err := discoverSourceFiles(obs)
	if err != nil {
		return nil, fmt.Errorf("conversations: discover source files: %w", err)
	}

	// Discover normalized ingest sources.
	ingestSources, err := discoverIngestSources(obs)
	if err != nil {
		return nil, fmt.Errorf("conversations: discover ingest sources: %w", err)
	}

	return &providerConfig{
		Root:          root,
		Observatory:   obs,
		SourceFiles:   files,
		IngestSources: ingestSources,
		Ontology:      lo,
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

	// Reload from disk only if an external process changed the index since our
	// last load/write. When the daemon is the sole writer (the common case) the
	// in-memory index already reflects every change via UpsertSession, so this
	// skips the full reload that otherwise dominates the reconcile cycle.
	if _, err := idx.LoadIfChanged(); err != nil {
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

	// Split live entries: CC sessions have Meta.Source == ""; normalized
	// ingest sessions carry their observer source id.
	ingestEntriesBySource := make(map[string][]IndexEntry)
	for _, entry := range ls.Entries {
		if entry.Meta.Source != "" {
			ingestEntriesBySource[entry.Meta.Source] = append(ingestEntriesBySource[entry.Meta.Source], entry)
		}
	}

	// ── CC source files ──────────────────────────────────────────────────────
	for _, f := range cfg.SourceFiles {
		existing, indexed := ls.Entries[f.SessionID]
		switch {
		case !indexed:
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionCreate,
				ResourceType: "conversations",
				Name:         f.SessionID,
				Details: map[string]any{
					"source_path": f.Path,
					"mtime":       f.Mtime.Format(time.RFC3339),
					"size":        f.Size,
				},
			})
			plan.Summary.Creates++

		case isDrift(existing.Meta, f):
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionUpdate,
				ResourceType: "conversations",
				Name:         f.SessionID,
				Details: map[string]any{
					"source_path": f.Path,
					"mtime":       f.Mtime.Format(time.RFC3339),
					"size":        f.Size,
					"prev_mtime":  existing.Meta.SourceMtime.Format(time.RFC3339),
					"prev_size":   existing.Meta.SourceSize,
					"prev_turns":  existing.Meta.TurnCount,
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

	// ── Normalized ingest sources (one action per SOURCE, not per file) ──────
	configuredIngest := make(map[string]struct{}, len(cfg.IngestSources))
	for _, src := range cfg.IngestSources {
		configuredIngest[src.Source] = struct{}{}
		indexed := ingestEntriesBySource[src.Source]
		details := map[string]any{
			"is_ingest":    true,
			"source_dir":   src.Dir,
			"ingest_files": src.Files,
			"total_size":   src.TotalSize,
			"latest_mtime": src.LatestMtime.Format(time.RFC3339),
		}

		switch {
		case len(indexed) == 0:
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionCreate,
				ResourceType: "conversations",
				Name:         src.Source,
				Details:      details,
			})
			plan.Summary.Creates++

		case isIngestDrift(indexed, src):
			details["prev_sessions"] = len(indexed)
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionUpdate,
				ResourceType: "conversations",
				Name:         src.Source,
				Details:      details,
			})
			plan.Summary.Updates++

		default:
			// Include is_ingest + ingest_files in skip actions so that
			// ApplyPlan can re-accumulate coverage for unchanged sources
			// (coverage must be recomputed every cycle, not accumulated).
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionSkip,
				ResourceType: "conversations",
				Name:         src.Source,
				Details: map[string]any{
					"reason":       "in sync",
					"sessions":     len(indexed),
					"is_ingest":    true,
					"ingest_files": src.Files,
				},
			})
			plan.Summary.Skipped++
		}
	}

	// ── Deletes ──────────────────────────────────────────────────────────────
	// CC sessions in index but no longer in source; ingest sessions whose
	// source is no longer configured. Stale sessions within a still-configured
	// ingest source are pruned by ApplyPlan after re-parse.
	for sid, entry := range ls.Entries {
		var stale bool
		if entry.Meta.Source == "" {
			_, inSource := sourceSet[sid]
			stale = !inSource
		} else {
			_, configured := configuredIngest[entry.Meta.Source]
			stale = !configured
		}
		if stale {
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
	// A newly computed plan has not been applied yet. Health reads this to
	// distinguish pending work from work already resolved.
	p.planApplied = false
	// A plan with no changes is itself proof that the corpus is converged —
	// whatever failed in an earlier apply is no longer outstanding. Without
	// this reset, applyFailures latches: ApplyPlan is the only other writer,
	// and the autonomic ticker skips ApplyPlan whenever the plan has no
	// changes, so a single past failure would pin Sync at OutOfSync forever.
	// That is the same permanent-divergence bug class this fix exists to
	// remove, so it must not be reintroduced through the failure path.
	if !plan.Summary.HasChanges() {
		p.applyFailures = 0
		p.applyBackpressure = 0
	}
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

	// Issue #494 remedy 4: serialize ApplyPlan per Provider instance. The
	// reconcile daemon (every 30s) and the autonomic ticker's self-heal
	// (every 30min, whenever Health() is non-Healthy) both drive the SAME
	// registered Provider — both eventually reach writeMetaFileLocked's
	// filelock.Acquire(metaLockPath, metaLockTimeout). flock(2) locks attach
	// to the open file description, not the owning process, so two
	// independent os.OpenFile calls made from the SAME process still block
	// each other; metaLockTimeout cannot save either caller here because
	// both are willing to sit out the full timeout waiting on themselves.
	// Confirmed live: every apply_failed cycle in the reconcile daemon's log
	// was preceded by a self-heal kickoff ~77s earlier, exactly the window a
	// large apply (pre-remedy-1, up to ~104s) takes to run. Once that
	// happens, the failed apply never reaches InSync, which re-arms the next
	// self-heal tick — a self-sustaining loop. TryLock (rather than
	// blocking) means the losing caller's cycle is cheap and it does not
	// fabricate an ApplyFailed result: the in-flight apply will report
	// accurate Health() shortly, and remedies 1–3 make that "shortly" close
	// to instant even for the largest source.
	if !p.applyMu.TryLock() {
		slog.Info("conversations: ApplyPlan skipped — another apply already in flight for this provider")
		return []reconcile.Result{{
			Phase:  "conversations",
			Action: "apply",
			Name:   "provider",
			Status: reconcile.ApplySkipped,
			Error:  "another ApplyPlan is already in flight for this provider",
		}}, nil
	}
	defer p.applyMu.Unlock()

	p.mu.Lock()
	p.operation = reconcile.OperationSyncing
	idx := p.index
	ont := p.ontology
	qw := p.quarantine
	cov := p.coverage
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.operation = reconcile.OperationIdle
		p.mu.Unlock()
	}()

	if idx == nil {
		return nil, fmt.Errorf("conversations: index not initialised (LoadConfig not called)")
	}

	// Reset coverage at the start of each reconcile cycle so counts reflect
	// the current corpus state rather than accumulating across cycles.
	// Skipped ingest sources are re-accumulated via a coverage-only parse pass
	// immediately below so all sources appear in the output regardless of drift.
	if cov != nil {
		cov.Reset()
	}

	var results []reconcile.Result
	var errs []string
	// backpressure counts actions that failed only because an index lock was
	// contended. See isLockBackpressure.
	var backpressure int
	// liveSources collects ingest source names present this cycle, used to
	// prune coverageCache for removed sources after the action loop.
	liveSources := make(map[string]struct{})

	// Issue #494 remedy 2: compute the source -> session IDs grouping ONCE
	// for the whole cycle (see SessionIDsBySource's doc comment), instead of
	// each applyIngestSource call re-deriving it via a full sorted
	// idx.ListSessions(...) scan of the entire index.
	bySource := idx.SessionIDsBySource()

	for _, action := range plan.Actions {
		if action.Action == reconcile.ActionSkip {
			// For skipped ingest sources the data has NOT drifted (ActionSkip is
			// itself the drift-free signal — ComputePlan already verified the
			// index matches the source). Serve coverage from cache to avoid a
			// full re-parse of every file each cycle (the CPU hot-loop fix).
			if isIngest, _ := action.Details["is_ingest"].(bool); isIngest {
				// Always record the source as live, even when cov is nil, so the
				// prune pass below never evicts a source that is still configured.
				liveSources[action.Name] = struct{}{}
				if cov != nil {
					p.mu.Lock()
					cached, warm := p.coverageCache[action.Name]
					p.mu.Unlock()

					if warm {
						// Cache hit: restore without parsing.
						cov.SetSource(action.Name, cached)
					} else {
						// Cold cache (e.g. first cycle after a restart): fall back to
						// a one-time coverage-only parse, then prime the cache.
						if covErr := accumulateCoverage(action, ont, cov); covErr != nil {
							// Non-fatal: log but don't fail the action.
							errs = append(errs, fmt.Sprintf("coverage-only pass for %s: %v", action.Name, covErr))
						} else {
							snap := cov.All()[action.Name]
							p.mu.Lock()
							p.coverageCache[action.Name] = snap
							p.mu.Unlock()
						}
					}
				}
			}
			continue
		}

		res := reconcile.Result{
			Phase:  "conversations",
			Action: string(action.Action),
			Name:   action.Name,
		}

		switch action.Action {
		case reconcile.ActionCreate, reconcile.ActionUpdate:
			if isIngest, _ := action.Details["is_ingest"].(bool); isIngest {
				// Normalized ingest: action.Name is the SOURCE; re-parse all
				// of its files and rebuild every session of that source.
				// Record the source as live before parsing so a transient parse
				// error does not drop it from liveSources and cause a spurious
				// cache eviction on the same cycle.
				liveSources[action.Name] = struct{}{}
				if applyErr := applyIngestSource(idx, action, ont, qw, cov, bySource[action.Name]); applyErr != nil {
					res.Status = reconcile.ApplyFailed
					res.Error = fmt.Sprintf("index ingest source %s: %v", action.Name, applyErr)
					results = append(results, res)
					if isLockBackpressure(applyErr) {
						backpressure++
					} else {
						errs = append(errs, res.Error)
					}
					continue
				}
				// applyIngestSource parsed + populated cov for this source;
				// snapshot it into the cache so subsequent skip cycles can
				// serve it without re-parsing.
				if cov != nil {
					snap := cov.All()[action.Name]
					p.mu.Lock()
					p.coverageCache[action.Name] = snap
					p.mu.Unlock()
				}
				res.Status = reconcile.ApplySucceeded
				res.CreatedID = action.Name
				results = append(results, res)
				continue
			}

			sourcePath, _ := action.Details["source_path"].(string)
			if sourcePath == "" {
				res.Status = reconcile.ApplyFailed
				res.Error = "missing source_path in plan action"
				results = append(results, res)
				errs = append(errs, res.Error)
				continue
			}

			meta, turns, err := indexSession(sourcePath, action.Name, defaultMaxTurnLen)
			if err != nil {
				res.Status = reconcile.ApplyFailed
				res.Error = fmt.Sprintf("index session %s: %v", action.Name, err)
				results = append(results, res)
				if isLockBackpressure(err) {
					backpressure++
				} else {
					errs = append(errs, res.Error)
				}
				continue
			}

			if upsertErr := idx.UpsertSession(meta, turns); upsertErr != nil {
				res.Status = reconcile.ApplyFailed
				res.Error = fmt.Sprintf("upsert session %s: %v", action.Name, upsertErr)
				results = append(results, res)
				if isLockBackpressure(upsertErr) {
					backpressure++
				} else {
					errs = append(errs, res.Error)
				}
				continue
			}

			res.Status = reconcile.ApplySucceeded
			res.CreatedID = action.Name
			results = append(results, res)

		case reconcile.ActionDelete:
			if delErr := idx.DeleteSession(action.Name); delErr != nil {
				res.Status = reconcile.ApplyFailed
				res.Error = delErr.Error()
				if isLockBackpressure(delErr) {
					backpressure++
				} else {
					errs = append(errs, res.Error)
				}
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
	// Removed ingest sources emit no create/update/skip action this cycle, so
	// any cache key absent from liveSources is a source that is gone.
	for src := range p.coverageCache {
		if _, ok := liveSources[src]; !ok {
			delete(p.coverageCache, src)
		}
	}
	p.lastErrors = errs
	// The plan has now been applied. Count genuinely failed actions: a plan
	// that applied cleanly leaves the provider converged even though the plan
	// itself contained changes.
	p.planApplied = true
	// Lock-contention failures are excluded from applyFailures for the same
	// reason they are excluded from lastErrors: the action did not land
	// because a concurrent writer held the index lock and is performing that
	// same work, so it is contention, not unresolved divergence. Counting it
	// re-armed self-heal, which produced more contention (#482).
	p.applyFailures = 0
	for _, r := range results {
		if r.Status == reconcile.ApplyFailed {
			p.applyFailures++
		}
	}
	// Every backpressure action is also an ApplyFailed result, so subtracting
	// the contended subset leaves the genuine failures. backpressure can never
	// exceed the ApplyFailed count because it is only incremented on paths
	// that also append an ApplyFailed result.
	p.applyFailures -= backpressure
	p.applyBackpressure = backpressure
	p.mu.Unlock()

	return results, nil
}

// isLockBackpressure reports whether err is (or wraps) a filelock acquisition
// timeout. Such a failure means another writer holds the on-disk index lock —
// backpressure from concurrency, not corruption of the corpus — so it must not
// contribute to the Degraded health axis. Matching is done with errors.Is
// against the filelock.ErrLockTimeout sentinel; every index write path wraps
// the acquisition error with %w, so the chain is intact.
func isLockBackpressure(err error) bool {
	return err != nil && errors.Is(err, filelock.ErrLockTimeout)
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
//
//	Sync      — OutOfSync only when work is genuinely unresolved: a computed
//	            plan with changes that has not been applied yet, or an applied
//	            plan whose actions failed. A plan that contained changes and
//	            applied cleanly is Synced.
//	Health    — Degraded when ApplyPlan had genuine indexing errors; Healthy
//	            otherwise. Lock-acquisition timeouts are explicitly excluded:
//	            they mean another writer holds the on-disk index lock, which is
//	            backpressure from concurrency, not corpus corruption. Counting
//	            them as Degraded closed a feedback loop with the autonomic
//	            ticker's self-heal (#482): Degraded re-armed self-heal,
//	            self-heal raced the reconcile daemon on the same provider, and
//	            the race produced another lock timeout, forever.
//	Operation — Syncing while ApplyPlan is running
//
// Sync deliberately does NOT key on "the last plan contained changes" alone.
// The observatory watches conversation transcripts that are appended to while
// the kernel runs, so on a live node there is nearly always a create or update
// in flight. Treating that as OutOfSync pinned the provider out-of-sync
// permanently and made the autonomic ticker's self-heal fire every 60s forever
// (#433) — which both burned CPU and destroyed the signal, since a status that
// is always OutOfSync cannot report actual divergence. Progress is normal
// operation; only unresolved work is divergence.
func (p *Provider) Health() reconcile.ResourceStatus {
	p.mu.Lock()
	summary := p.lastPlanSummary
	errs := len(p.lastErrors)
	op := p.operation
	applied := p.planApplied
	failures := p.applyFailures
	contended := p.applyBackpressure
	p.mu.Unlock()

	sync := reconcile.SyncStatusSynced
	switch {
	case summary.HasChanges() && !applied:
		// Work is planned but not yet carried out — genuine, momentary
		// divergence. This is the state self-heal exists to resolve.
		sync = reconcile.SyncStatusOutOfSync
	case failures > 0:
		// Work was attempted and did not land. Still divergent.
		sync = reconcile.SyncStatusOutOfSync
	}

	health := reconcile.HealthHealthy
	msg := ""
	if errs > 0 {
		health = reconcile.HealthDegraded
		msg = fmt.Sprintf("%d session(s) failed to index", errs)
	} else if contended > 0 {
		// Informational only — health stays Healthy and sync stays Synced.
		msg = fmt.Sprintf("%d action(s) deferred on index lock contention", contended)
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

// defaultOntologyDir returns the default ontology directory for the given
// workspace root: <root>/.cog/observatory/ontology. Returns "" if absent.
func defaultOntologyDir(root string) string {
	return filepath.Join(root, ".cog", "observatory", "ontology")
}

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

// discoverSourceFiles scans SourceDirs and returns matching CC JSONL files.
func discoverSourceFiles(obs ObservatoryConfig) ([]sourceFileInfo, error) {
	var files []sourceFileInfo
	seen := make(map[string]struct{})

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

	sort.Slice(files, func(i, j int) bool {
		return files[i].SessionID < files[j].SessionID
	})
	return files, nil
}

// discoverIngestSources scans IngestDirs for <source>/*.jsonl files and
// returns one ingestSourceInfo per source directory, aggregating file list,
// total size, and latest mtime. Sources appearing under multiple ingest roots
// are merged into one entry.
func discoverIngestSources(obs ObservatoryConfig) ([]ingestSourceInfo, error) {
	bySource := make(map[string]*ingestSourceInfo)

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

			src, ok := bySource[sourceName]
			if !ok {
				src = &ingestSourceInfo{Source: sourceName, Dir: sourceDir}
				bySource[sourceName] = src
			}

			for _, je := range jsonlEntries {
				if je.IsDir() {
					continue
				}
				name := je.Name()
				if !strings.HasSuffix(name, ".jsonl") {
					continue
				}
				fi, err := je.Info()
				if err != nil {
					continue
				}
				src.Files = append(src.Files, filepath.Join(sourceDir, name))
				src.TotalSize += fi.Size()
				if fi.ModTime().After(src.LatestMtime) {
					src.LatestMtime = fi.ModTime()
				}
			}
		}
	}

	var out []ingestSourceInfo
	for _, src := range bySource {
		if len(src.Files) == 0 {
			continue
		}
		sort.Strings(src.Files)
		out = append(out, *src)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
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
func indexSession(sourcePath, sessionID string, maxTurnLen int) (SessionMeta, []Turn, error) {
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
		SourcePath:  sourcePath,
		IndexedAt:   time.Now().UTC(),
		SourceMtime: fi.ModTime(),
		SourceSize:  fi.Size(),
	}

	var turns []Turn
	err = ParseSession(f, sessionID, maxTurnLen, &meta, func(t Turn) bool {
		turns = append(turns, t)
		return true
	})
	if err != nil {
		return meta, turns, fmt.Errorf("parse %s: %w", sourcePath, err)
	}

	return meta, turns, nil
}

// isIngestDrift returns true when the indexed sessions of a source are stale
// compared to the source's current file aggregate. All sessions of a source
// share the same stored (SourceSize, SourceMtime) aggregate values from the
// last apply, so checking one entry suffices; we check all defensively.
func isIngestDrift(indexed []IndexEntry, src ingestSourceInfo) bool {
	for _, entry := range indexed {
		if entry.Meta.SourceSize != src.TotalSize {
			return true
		}
		diff := entry.Meta.SourceMtime.Sub(src.LatestMtime)
		if diff < 0 {
			diff = -diff
		}
		if diff > 2*time.Second {
			return true
		}
	}
	return false
}

// applyIngestSource re-parses every file of an ingest source (action.Name),
// upserts every resulting session in one batch, and prunes — also in one
// batch — index sessions of that source that no longer appear in the parse
// result.
//
// ont, qw, cov may be nil; when non-nil, ontology enforcement, quarantine
// routing, and coverage tracking are applied during ConsumeFile.
//
// existingSourceSessionIDs is this source's slice of the map ApplyPlan
// computed once via idx.SessionIDsBySource() before its action loop — see
// that method's doc comment (issue #494 remedy 2) for why this is passed in
// rather than called here: idx.ListSessions(...) sorts the *entire* index by
// LastTurnAt, an ordering the prune pass below never uses, and doing that
// once per ingest source in a cycle with several sources drifting re-sorted
// the same ~7,500 sessions redundantly. Hoisting the (unsorted, ID-only)
// computation up to ApplyPlan turns that into one full-index walk shared by
// every source in the cycle.
func applyIngestSource(idx *Index, action reconcile.Action, ont *LoadedOntology, qw *QuarantineWriter, cov *CoverageTracker, existingSourceSessionIDs []string) error {
	sourceDir, _ := action.Details["source_dir"].(string)
	files := stringSliceDetail(action.Details["ingest_files"])
	if len(files) == 0 {
		return fmt.Errorf("no ingest_files in plan action")
	}

	// Aggregate stats for drift bookkeeping on each session meta.
	var totalSize int64
	var latestMtime time.Time
	acc := newIngestAccumulator(defaultMaxTurnLen)
	acc.Ontology = ont
	acc.Quarantine = qw
	acc.Coverage = cov
	for _, path := range files {
		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		totalSize += fi.Size()
		if fi.ModTime().After(latestMtime) {
			latestMtime = fi.ModTime()
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		consumeErr := acc.ConsumeFile(f)
		f.Close()
		if consumeErr != nil {
			return fmt.Errorf("parse %s: %w", path, consumeErr)
		}
	}

	now := time.Now().UTC()
	sessions := acc.Sessions()
	parsed := make(map[string]struct{}, len(sessions))
	batch := make([]SessionAndTurns, 0, len(sessions))
	for _, sess := range sessions {
		sess.Meta.SourcePath = sourceDir
		sess.Meta.IndexedAt = now
		sess.Meta.SourceMtime = latestMtime
		sess.Meta.SourceSize = totalSize
		batch = append(batch, SessionAndTurns{Meta: sess.Meta, Turns: sess.Turns})
		parsed[sess.Meta.SessionID] = struct{}{}
	}
	// Issue #494 remedy 1: one UpsertSessions call writes every turns file
	// under its own per-session lock, then commits ALL of this source's meta
	// in a single writeMetaFileLocked round trip, instead of one full
	// _meta.json rewrite per session.
	if len(batch) > 0 {
		if err := idx.UpsertSessions(batch); err != nil {
			return fmt.Errorf("upsert sessions for source %s: %w", action.Name, err)
		}
	}

	// Prune sessions of this source that vanished from the parse result
	// (observer files are append-only, so this is rare — defensive only).
	// Batched via DeleteSessions for the same reason as the upsert above.
	var stale []string
	for _, sid := range existingSourceSessionIDs {
		if _, ok := parsed[sid]; !ok {
			stale = append(stale, sid)
		}
	}
	if len(stale) > 0 {
		if err := idx.DeleteSessions(stale); err != nil {
			return fmt.Errorf("prune stale sessions for source %s: %w", action.Name, err)
		}
	}

	return nil
}

// accumulateCoverage parses the ingest files for the given action's source and
// accumulates coverage metrics into cov without touching the index.  Used on
// ActionSkip to ensure coverage is recomputed every cycle even when the source
// data has not changed.
//
// ont may be nil (returns without error when nil — no coverage to accumulate).
func accumulateCoverage(action reconcile.Action, ont *LoadedOntology, cov *CoverageTracker) error {
	if ont == nil || cov == nil {
		return nil
	}
	files := stringSliceDetail(action.Details["ingest_files"])
	if len(files) == 0 {
		return nil
	}
	acc := newIngestAccumulator(defaultMaxTurnLen)
	acc.Ontology = ont
	acc.Coverage = cov
	// Quarantine is intentionally nil — we are not writing quarantine records,
	// only re-counting coverage metrics.
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		consumeErr := acc.ConsumeFile(f)
		f.Close()
		if consumeErr != nil {
			return fmt.Errorf("parse %s: %w", path, consumeErr)
		}
	}
	return nil
}

// stringSliceDetail coerces a plan-action detail value into []string.
// Handles both the in-process []string form and the []any form a JSON
// round-trip would produce.
func stringSliceDetail(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

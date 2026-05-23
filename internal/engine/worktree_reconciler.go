// worktree_reconciler.go — substrate-canonical Reconcilable for git worktree lifecycle.
//
// ADR-096: every substrate-spawned worktree is bound to a ledger entry from its
// first byte. WorktreeReconciler maintains the substrate's declared worktree
// set against the live filesystem + git worktree registry, classifying each
// known worktree into one of four states and producing the corresponding plan
// action.
//
// Classification (per ADR-096 §3):
//
//	alive                                  -> leave
//	removable-clean                        -> prune (git worktree remove --force)
//	alarm-uncommitted-on-terminal-dispatch -> alarm (no filesystem mutation)
//	alarm-unknown-binding                  -> alarm (no filesystem mutation)
//
// Hard rule (ADR-096 §4): the reconciler NEVER auto-prunes
// `alarm-unknown-binding` worktrees. Auto-pruning hand-created or
// harness-managed worktrees would be a data-loss bug.
//
// Composition:
//   - ADR-091 §5: ledger-first rule (SpawnWorktree writes the ledger entry
//     before `git worktree add`).
//   - ADR-092 §3: ApplyPlan is idempotent (existence check before removal;
//     double-prune is a no-op).
//   - ADR-093: composes with ManagedSession — terminal-dispatch state is
//     observed via the ledger, not via a process-table probe.
//   - ADR-095: registered with ReconcileDaemon at boot; one instance per
//     managed repo root.

package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ─── Classification ───────────────────────────────────────────────────────────

// WorktreeClassification is the four-state classification from ADR-096 §3.
type WorktreeClassification string

const (
	// ClassAlive: bound dispatch is not terminal; or terminal but branch
	// is not yet merged or abandoned. Plan action: leave.
	ClassAlive WorktreeClassification = "alive"

	// ClassRemovableClean: dispatch terminal AND branch merged/abandoned,
	// no uncommitted changes. Plan action: prune.
	ClassRemovableClean WorktreeClassification = "removable-clean"

	// ClassAlarmUncommittedOnTerminalDispatch: dispatch terminal AND
	// worktree has uncommitted changes or unmerged local-only commits.
	// Plan action: alarm (operator decision required). NEVER auto-prune.
	ClassAlarmUncommittedOnTerminalDispatch WorktreeClassification = "alarm-uncommitted-on-terminal-dispatch"

	// ClassAlarmUnknownBinding: worktree exists on disk but no matching
	// `worktree.created` ledger event. Plan action: alarm. NEVER auto-prune.
	// This is the expected classification for pre-substrate worktrees and
	// for harness-managed worktrees created outside SpawnWorktree.
	ClassAlarmUnknownBinding WorktreeClassification = "alarm-unknown-binding"
)

// ─── Ledger projections ───────────────────────────────────────────────────────

// WorktreeCreatedEvent is the parsed projection of a `worktree.created`
// ledger event for use by ComputePlan.
type WorktreeCreatedEvent struct {
	WorktreeID   string
	DispatchID   string
	RepoRoot     string
	WorktreePath string
	Branch       string
	Base         string
	CreatedAt    time.Time
}

// TerminalReason is one of the allowed `reason` values in a `worktree.terminal`
// ledger event.
type TerminalReason string

const (
	TerminalReasonMerged    TerminalReason = "merged"
	TerminalReasonAbandoned TerminalReason = "abandoned"
	TerminalReasonExited    TerminalReason = "exited"
)

// WorktreeTerminalEvent is the parsed projection of a `worktree.terminal`
// ledger event.
type WorktreeTerminalEvent struct {
	WorktreeID string
	DispatchID string
	Reason     TerminalReason
}

// WorktreePrunedEvent records that a worktree was already pruned. The
// reconciler uses this to short-circuit double-prune attempts across
// daemon restarts (ADR-092 §3 crash recovery).
type WorktreePrunedEvent struct {
	WorktreeID string
}

// LedgerReader is the read-side dependency the reconciler uses to load
// `worktree.*` events for its repo root. Production implementations scan
// `.cog/ledger/*/events.jsonl`; test implementations return an in-memory list.
type LedgerReader interface {
	// ReadWorktreeEvents returns all worktree.* events for the given repo root,
	// in append order. The reconciler filters by RepoRoot itself if the reader
	// returns events for multiple repos.
	ReadWorktreeEvents(ctx context.Context, repoRoot string) ([]WorktreeLedgerEvent, error)
}

// WorktreeLedgerEvent is a typed projection of any `worktree.*` ledger event.
// Exactly one of the *Event fields is non-nil per record.
type WorktreeLedgerEvent struct {
	Created  *WorktreeCreatedEvent
	Terminal *WorktreeTerminalEvent
	Pruned   *WorktreePrunedEvent
}

// LedgerWriter is the write-side dependency the reconciler uses to emit
// `worktree.pruned` and `worktree.alarm` events from ApplyPlan. Production
// implementations call AppendEvent on the per-session ledger.
type LedgerWriter interface {
	EmitWorktreeEvent(ctx context.Context, eventType CogBlockKind, data map[string]interface{}) error
}

// ─── Git adapter ──────────────────────────────────────────────────────────────

// LiveWorktree is the observed state of one worktree on disk.
type LiveWorktree struct {
	// Path is the absolute filesystem path of the worktree.
	Path string
	// Branch is the branch checked out in the worktree (empty for detached HEAD).
	Branch string
	// Detached is true if HEAD is detached.
	Detached bool
	// HeadSHA is the commit SHA at HEAD.
	HeadSHA string
	// Locked is true if the worktree is locked (admin-state).
	Locked bool
	// HasUncommittedChanges is true if `git status --porcelain` is non-empty.
	HasUncommittedChanges bool
	// HasUnmergedCommits is true if the branch has local commits not present
	// on the configured upstream (empty if no upstream configured; treated
	// as "unknown" rather than "merged").
	HasUnmergedCommits bool
	// UpstreamConfigured is true if the branch tracks a remote.
	UpstreamConfigured bool
}

// GitAdapter wraps the `git` CLI surface the reconciler needs. The interface
// keeps tests hermetic; production uses the real-git implementation.
type GitAdapter interface {
	// ListWorktrees returns the set of worktrees registered with the given
	// repo root (via `git worktree list --porcelain`).
	ListWorktrees(ctx context.Context, repoRoot string) ([]LiveWorktree, error)

	// RemoveWorktree runs `git worktree remove --force <path>` against
	// repoRoot. Idempotent in spirit: callers should check path existence
	// before invoking, but the adapter does not enforce that itself.
	RemoveWorktree(ctx context.Context, repoRoot, path string) error
}

// ─── WorktreeReconciler ───────────────────────────────────────────────────────

// WorktreeReconciler implements reconcile.Reconcilable for the worktree
// lifecycle. One instance per managed repo root.
type WorktreeReconciler struct {
	RepoRoot     string
	LedgerReader LedgerReader
	LedgerWriter LedgerWriter
	GitAdapter   GitAdapter

	mu     sync.Mutex
	health reconcile.ResourceStatus
}

// NewWorktreeReconciler constructs a WorktreeReconciler for the given repo
// root using the supplied adapters. Pass nil adapters in v0 when registering
// via init(); the reconciler will use defaults wired in LoadConfig once the
// workspace root is known.
func NewWorktreeReconciler(repoRoot string, reader LedgerReader, writer LedgerWriter, git GitAdapter) *WorktreeReconciler {
	return &WorktreeReconciler{
		RepoRoot:     repoRoot,
		LedgerReader: reader,
		LedgerWriter: writer,
		GitAdapter:   git,
		health: reconcile.NewResourceStatus(
			reconcile.SyncStatusUnknown,
			reconcile.HealthProgressing,
		),
	}
}

// ─── Reconcilable interface ───────────────────────────────────────────────────

// Type returns the provider type identifier. Per ADR-096 §5, one provider per
// repo root; the type string includes a hash-free path token so multi-repo
// deployments register distinct types.
func (r *WorktreeReconciler) Type() string {
	return "worktree-reconciler:" + r.RepoRoot
}

// worktreeConfig is what LoadConfig returns: the declared worktree set
// derived from the ledger (worktrees with a creation event whose lifecycle is
// not yet pruned).
type worktreeConfig struct {
	RepoRoot        string
	CreatedByPath   map[string]*WorktreeCreatedEvent  // keyed by worktree path
	TerminalByID    map[string]*WorktreeTerminalEvent // keyed by worktree_id
	PrunedByID      map[string]struct{}               // set of pruned worktree IDs
}

// LoadConfig reads all worktree.* ledger events for this RepoRoot and derives
// the declared worktree set. Read-only; idempotent.
func (r *WorktreeReconciler) LoadConfig(workspaceRoot string) (any, error) {
	if r.LedgerReader == nil {
		// v0 fallback: workspace-root-scoped FilesystemLedgerReader.
		r.LedgerReader = NewFilesystemLedgerReader(workspaceRoot)
	}
	if r.LedgerWriter == nil {
		r.LedgerWriter = NewFilesystemLedgerWriter(workspaceRoot)
	}
	if r.GitAdapter == nil {
		r.GitAdapter = NewCLIGitAdapter()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	events, err := r.LedgerReader.ReadWorktreeEvents(ctx, r.RepoRoot)
	if err != nil {
		return nil, fmt.Errorf("worktree-reconciler: read ledger: %w", err)
	}

	cfg := &worktreeConfig{
		RepoRoot:      r.RepoRoot,
		CreatedByPath: make(map[string]*WorktreeCreatedEvent),
		TerminalByID:  make(map[string]*WorktreeTerminalEvent),
		PrunedByID:    make(map[string]struct{}),
	}
	for _, ev := range events {
		switch {
		case ev.Created != nil:
			c := ev.Created
			cfg.CreatedByPath[c.WorktreePath] = c
		case ev.Terminal != nil:
			t := ev.Terminal
			cfg.TerminalByID[t.WorktreeID] = t
		case ev.Pruned != nil:
			cfg.PrunedByID[ev.Pruned.WorktreeID] = struct{}{}
		}
	}
	return cfg, nil
}

// FetchLive runs `git worktree list --porcelain` against the repo root and
// queries per-worktree git state. Read-only.
func (r *WorktreeReconciler) FetchLive(ctx context.Context, _ any) (any, error) {
	live, err := r.GitAdapter.ListWorktrees(ctx, r.RepoRoot)
	if err != nil {
		r.setHealth(reconcile.SyncStatusUnknown, reconcile.HealthDegraded, fmt.Sprintf("git list: %v", err))
		return nil, fmt.Errorf("worktree-reconciler: git list: %w", err)
	}
	return live, nil
}

// classifiedWorktree carries a live worktree plus its classification and
// supporting decision context for downstream alarm/prune actions.
type classifiedWorktree struct {
	Live           LiveWorktree
	Classification WorktreeClassification
	Created        *WorktreeCreatedEvent  // nil for unknown-binding
	Terminal       *WorktreeTerminalEvent // nil if no terminal event
	Reason         string                 // human-readable diagnostic detail
}

// ComputePlan classifies every live worktree against the declared config and
// produces a plan of leave/prune/alarm actions. Pure function: deterministic
// given (config, live).
func (r *WorktreeReconciler) ComputePlan(config any, live any, _ *reconcile.State) (*reconcile.Plan, error) {
	cfg, ok := config.(*worktreeConfig)
	if !ok {
		return nil, fmt.Errorf("worktree-reconciler: unexpected config type %T", config)
	}
	livews, ok := live.([]LiveWorktree)
	if !ok {
		return nil, fmt.Errorf("worktree-reconciler: unexpected live type %T", live)
	}

	classified := make([]classifiedWorktree, 0, len(livews))
	for _, w := range livews {
		// The main worktree of the repo (RepoRoot itself) is never substrate-
		// owned; skip it from classification to avoid emitting alarms on the
		// host repo. `git worktree list` always reports the main repo as the
		// first entry.
		if isSamePath(w.Path, cfg.RepoRoot) {
			continue
		}

		c := classifyWorktree(w, cfg)
		classified = append(classified, c)
	}

	// Deterministic order: by path.
	sort.Slice(classified, func(i, j int) bool {
		return classified[i].Live.Path < classified[j].Live.Path
	})

	plan := &reconcile.Plan{
		ResourceType: r.Type(),
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath:   cfg.RepoRoot,
	}

	for _, c := range classified {
		action := reconcile.Action{
			ResourceType: r.Type(),
			Name:         c.Live.Path,
			Details: map[string]any{
				"path":           c.Live.Path,
				"classification": string(c.Classification),
				"branch":         c.Live.Branch,
				"locked":         c.Live.Locked,
				"reason":         c.Reason,
			},
		}
		if c.Created != nil {
			action.Details["worktree_id"] = c.Created.WorktreeID
			action.Details["dispatch_id"] = c.Created.DispatchID
		}

		switch c.Classification {
		case ClassRemovableClean:
			// Idempotency guard: if a prior `worktree.pruned` event exists for
			// this worktree ID, skip the action. This handles the case where
			// the previous tick pruned the path but the filesystem still
			// reflects the old `git worktree list` cache (rare but possible
			// across crash recovery).
			if c.Created != nil {
				if _, alreadyPruned := cfg.PrunedByID[c.Created.WorktreeID]; alreadyPruned {
					action.Action = reconcile.ActionSkip
					action.Details["reason"] = "already pruned per ledger"
					plan.Summary.Skipped++
					plan.Actions = append(plan.Actions, action)
					continue
				}
			}
			action.Action = reconcile.ActionDelete
			plan.Summary.Deletes++
		case ClassAlarmUncommittedOnTerminalDispatch, ClassAlarmUnknownBinding:
			// Alarm is encoded as ActionUpdate with classification=alarm.
			// The reconcile.Action vocabulary lacks a first-class "alarm"
			// verb; we reuse Update + the classification detail field to
			// signal alarm-only (no filesystem mutation) to ApplyPlan.
			action.Action = reconcile.ActionUpdate
			plan.Summary.Updates++
		default:
			action.Action = reconcile.ActionSkip
			plan.Summary.Skipped++
		}
		plan.Actions = append(plan.Actions, action)
	}

	return plan, nil
}

// classifyWorktree applies the four-state classifier from ADR-096 §3.
//
// Decision order:
//  1. No `worktree.created` event matching the path -> alarm-unknown-binding.
//  2. No `worktree.terminal` event for this worktree_id -> alive.
//  3. Terminal event present, classification depends on:
//     - any uncommitted changes -> alarm-uncommitted-on-terminal-dispatch
//     - reason=merged or reason=abandoned and no uncommitted -> removable-clean
//     - reason=exited (process died) and branch not yet merged/abandoned ->
//       alive (operator still owns the lifecycle decision)
func classifyWorktree(w LiveWorktree, cfg *worktreeConfig) classifiedWorktree {
	created, hasBinding := cfg.CreatedByPath[w.Path]
	if !hasBinding {
		return classifiedWorktree{
			Live:           w,
			Classification: ClassAlarmUnknownBinding,
			Reason:         "no worktree.created ledger event for this path",
		}
	}

	terminal, hasTerminal := cfg.TerminalByID[created.WorktreeID]
	if !hasTerminal {
		return classifiedWorktree{
			Live:           w,
			Classification: ClassAlive,
			Created:        created,
			Reason:         "bound dispatch not yet terminal",
		}
	}

	// Terminal dispatch. Check for uncommitted/unmerged work first — this is
	// the alarm condition that supersedes "merged/abandoned" classification.
	if w.HasUncommittedChanges || w.HasUnmergedCommits {
		return classifiedWorktree{
			Live:           w,
			Classification: ClassAlarmUncommittedOnTerminalDispatch,
			Created:        created,
			Terminal:       terminal,
			Reason: fmt.Sprintf(
				"dispatch terminal (reason=%s) but worktree has uncommitted=%t unmerged=%t",
				terminal.Reason, w.HasUncommittedChanges, w.HasUnmergedCommits,
			),
		}
	}

	switch terminal.Reason {
	case TerminalReasonMerged, TerminalReasonAbandoned:
		return classifiedWorktree{
			Live:           w,
			Classification: ClassRemovableClean,
			Created:        created,
			Terminal:       terminal,
			Reason:         fmt.Sprintf("dispatch terminal (reason=%s) and worktree clean", terminal.Reason),
		}
	default:
		// reason=exited or unknown reason: defer the decision to the operator.
		return classifiedWorktree{
			Live:           w,
			Classification: ClassAlive,
			Created:        created,
			Terminal:       terminal,
			Reason:         fmt.Sprintf("dispatch terminal (reason=%s) but branch lifecycle not resolved", terminal.Reason),
		}
	}
}

// ApplyPlan executes the plan's actions. Per ADR-096 §3:
//   - ActionDelete on a removable-clean worktree: `git worktree remove --force`
//     followed by emit `worktree.pruned`.
//   - ActionUpdate on an alarm classification: emit `worktree.alarm` only; no
//     filesystem mutation. Hard rule: NEVER auto-prune `alarm-unknown-binding`.
//   - ActionSkip: no-op.
//
// Idempotent (ADR-092 §3): existence check before removal; double-emit of
// alarm produces a second ledger entry but no filesystem side effect.
func (r *WorktreeReconciler) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	var results []reconcile.Result

	for _, action := range plan.Actions {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		path, _ := action.Details["path"].(string)
		classification, _ := action.Details["classification"].(string)
		worktreeID, _ := action.Details["worktree_id"].(string)
		dispatchID, _ := action.Details["dispatch_id"].(string)
		branch, _ := action.Details["branch"].(string)
		reason, _ := action.Details["reason"].(string)

		switch action.Action {
		case reconcile.ActionSkip:
			results = append(results, reconcile.Result{
				Phase:  "apply",
				Action: string(action.Action),
				Name:   action.Name,
				Status: reconcile.ApplySkipped,
			})

		case reconcile.ActionUpdate:
			// Alarm path.
			if !isAlarmClassification(classification) {
				results = append(results, reconcile.Result{
					Phase:  "apply",
					Action: string(action.Action),
					Name:   action.Name,
					Status: reconcile.ApplyFailed,
					Error:  fmt.Sprintf("Update action without alarm classification: %q", classification),
				})
				continue
			}
			data := map[string]interface{}{
				"worktree_path":  path,
				"classification": classification,
				"repo_root":      r.RepoRoot,
				"reason":         reason,
				"alarmed_at":     time.Now().UTC().Format(time.RFC3339),
			}
			if worktreeID != "" {
				data["worktree_id"] = worktreeID
			}
			if dispatchID != "" {
				data["dispatch_id"] = dispatchID
			}
			if branch != "" {
				data["branch"] = branch
			}
			if err := r.LedgerWriter.EmitWorktreeEvent(ctx, BlockWorktreeAlarm, data); err != nil {
				results = append(results, reconcile.Result{
					Phase:  "apply",
					Action: string(action.Action),
					Name:   action.Name,
					Status: reconcile.ApplyFailed,
					Error:  fmt.Sprintf("emit alarm: %v", err),
				})
				continue
			}
			results = append(results, reconcile.Result{
				Phase:  "apply",
				Action: string(action.Action),
				Name:   action.Name,
				Status: reconcile.ApplySucceeded,
			})

		case reconcile.ActionDelete:
			// Removable-clean only. Hard rule: never reach this branch for
			// alarm-unknown-binding (ComputePlan classifies those as Update).
			if classification != string(ClassRemovableClean) {
				results = append(results, reconcile.Result{
					Phase:  "apply",
					Action: string(action.Action),
					Name:   action.Name,
					Status: reconcile.ApplyFailed,
					Error:  fmt.Sprintf("Delete action for non-removable classification: %q", classification),
				})
				continue
			}

			// Idempotency: skip if path no longer exists.
			if _, err := os.Stat(path); err != nil {
				if os.IsNotExist(err) {
					results = append(results, reconcile.Result{
						Phase:  "apply",
						Action: string(action.Action),
						Name:   action.Name,
						Status: reconcile.ApplySkipped,
					})
					continue
				}
				results = append(results, reconcile.Result{
					Phase:  "apply",
					Action: string(action.Action),
					Name:   action.Name,
					Status: reconcile.ApplyFailed,
					Error:  fmt.Sprintf("stat: %v", err),
				})
				continue
			}

			if err := r.GitAdapter.RemoveWorktree(ctx, r.RepoRoot, path); err != nil {
				results = append(results, reconcile.Result{
					Phase:  "apply",
					Action: string(action.Action),
					Name:   action.Name,
					Status: reconcile.ApplyFailed,
					Error:  fmt.Sprintf("git remove: %v", err),
				})
				continue
			}

			data := map[string]interface{}{
				"worktree_id":   worktreeID,
				"worktree_path": path,
				"repo_root":     r.RepoRoot,
				"pruned_at":     time.Now().UTC().Format(time.RFC3339),
			}
			if err := r.LedgerWriter.EmitWorktreeEvent(ctx, BlockWorktreePruned, data); err != nil {
				// Filesystem mutation already succeeded; surface the ledger
				// write failure but the action result is failed (the ledger
				// is the authority).
				results = append(results, reconcile.Result{
					Phase:  "apply",
					Action: string(action.Action),
					Name:   action.Name,
					Status: reconcile.ApplyFailed,
					Error:  fmt.Sprintf("emit pruned: %v (filesystem already removed)", err),
				})
				continue
			}
			results = append(results, reconcile.Result{
				Phase:  "apply",
				Action: string(action.Action),
				Name:   action.Name,
				Status: reconcile.ApplySucceeded,
			})

		default:
			results = append(results, reconcile.Result{
				Phase:  "apply",
				Action: string(action.Action),
				Name:   action.Name,
				Status: reconcile.ApplyFailed,
				Error:  fmt.Sprintf("unsupported action %q", action.Action),
			})
		}
	}

	// Health: degraded if any alarm fired; otherwise healthy.
	anyAlarm := false
	anyError := false
	for _, res := range results {
		if res.Status == reconcile.ApplyFailed {
			anyError = true
		}
	}
	for _, a := range plan.Actions {
		if cls, _ := a.Details["classification"].(string); isAlarmClassification(cls) {
			anyAlarm = true
		}
	}
	switch {
	case anyError:
		r.setHealth(reconcile.SyncStatusOutOfSync, reconcile.HealthDegraded, "apply errors")
	case anyAlarm:
		r.setHealth(reconcile.SyncStatusOutOfSync, reconcile.HealthDegraded, "alarm classification present")
	default:
		r.setHealth(reconcile.SyncStatusSynced, reconcile.HealthHealthy, "")
	}

	return results, nil
}

// BuildState constructs the reconciler state from live data + plan results.
// Pure function (besides clock).
func (r *WorktreeReconciler) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	livews, ok := live.([]LiveWorktree)
	if !ok {
		return nil, fmt.Errorf("worktree-reconciler: unexpected live type %T", live)
	}
	cfg, ok := config.(*worktreeConfig)
	if !ok {
		return nil, fmt.Errorf("worktree-reconciler: unexpected config type %T", config)
	}

	state := reconcile.NewState(r.Type())
	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for _, w := range livews {
		if isSamePath(w.Path, cfg.RepoRoot) {
			continue
		}
		c := classifyWorktree(w, cfg)
		attrs := map[string]any{
			"classification":            string(c.Classification),
			"branch":                    w.Branch,
			"detached":                  w.Detached,
			"locked":                    w.Locked,
			"has_uncommitted_changes":   w.HasUncommittedChanges,
			"has_unmerged_commits":      w.HasUnmergedCommits,
		}
		if c.Created != nil {
			attrs["worktree_id"] = c.Created.WorktreeID
			attrs["dispatch_id"] = c.Created.DispatchID
		}
		state.Resources = append(state.Resources, reconcile.Resource{
			Address:       "worktree." + w.Path,
			Type:          "worktree",
			Mode:          reconcile.ModeManaged,
			Name:          w.Path,
			ExternalID:    w.Path,
			Attributes:    attrs,
			LastRefreshed: now,
		})
	}

	return state, nil
}

// Health returns the current three-axis status.
func (r *WorktreeReconciler) Health() reconcile.ResourceStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.health
}

func (r *WorktreeReconciler) setHealth(sync reconcile.SyncStatus, health reconcile.HealthStatus, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health = reconcile.ResourceStatus{
		Sync:      sync,
		Health:    health,
		Operation: reconcile.OperationIdle,
		Message:   msg,
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func isAlarmClassification(c string) bool {
	return c == string(ClassAlarmUncommittedOnTerminalDispatch) ||
		c == string(ClassAlarmUnknownBinding)
}

// isSamePath returns true if a and b refer to the same filesystem path after
// cleaning. Does not follow symlinks (intentional: we treat /a/b and a symlink
// to /a/b as distinct identities for ledger purposes).
func isSamePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// ─── Production adapters ──────────────────────────────────────────────────────

// CLIGitAdapter is the production GitAdapter that shells out to `git`.
type CLIGitAdapter struct{}

func NewCLIGitAdapter() *CLIGitAdapter { return &CLIGitAdapter{} }

func (a *CLIGitAdapter) ListWorktrees(ctx context.Context, repoRoot string) ([]LiveWorktree, error) {
	cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}

	wts := parseWorktreeListPorcelain(string(out))

	// Enrich each non-main worktree with status (uncommitted) and upstream
	// state. We skip the main worktree itself (it always lives at repoRoot).
	for i := range wts {
		if isSamePath(wts[i].Path, repoRoot) {
			continue
		}
		uncommitted, err := gitHasUncommittedChanges(ctx, wts[i].Path)
		if err == nil {
			wts[i].HasUncommittedChanges = uncommitted
		}
		if !wts[i].Detached && wts[i].Branch != "" {
			unmerged, upstream, err := gitHasUnmergedCommits(ctx, wts[i].Path)
			if err == nil {
				wts[i].HasUnmergedCommits = unmerged
				wts[i].UpstreamConfigured = upstream
			}
		}
	}
	return wts, nil
}

func (a *CLIGitAdapter) RemoveWorktree(ctx context.Context, repoRoot, path string) error {
	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", path)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// parseWorktreeListPorcelain parses `git worktree list --porcelain` output.
// Each worktree is a stanza of `key value` lines separated by blank lines.
func parseWorktreeListPorcelain(s string) []LiveWorktree {
	var out []LiveWorktree
	var cur *LiveWorktree
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			cur = &LiveWorktree{Path: val}
		case "HEAD":
			if cur != nil {
				cur.HeadSHA = val
			}
		case "branch":
			if cur != nil {
				// "refs/heads/foo" -> "foo"
				cur.Branch = strings.TrimPrefix(val, "refs/heads/")
			}
		case "detached":
			if cur != nil {
				cur.Detached = true
			}
		case "locked":
			if cur != nil {
				cur.Locked = true
			}
		}
	}
	flush()
	return out
}

func gitHasUncommittedChanges(ctx context.Context, worktreePath string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// gitHasUnmergedCommits returns (unmerged, upstreamConfigured, err). Unmerged is
// true if the branch has commits not present on its configured upstream.
// upstreamConfigured is false if no upstream is set; in that case we treat the
// worktree as "unknown" (not unmerged) so we don't falsely alarm on local-only
// scratch branches.
func gitHasUnmergedCommits(ctx context.Context, worktreePath string) (bool, bool, error) {
	// Check upstream tracking.
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "@{upstream}")
	cmd.Dir = worktreePath
	upstream, err := cmd.Output()
	if err != nil {
		return false, false, nil
	}
	upstreamRef := strings.TrimSpace(string(upstream))
	if upstreamRef == "" {
		return false, false, nil
	}

	// Count commits ahead of upstream.
	cmd2 := exec.CommandContext(ctx, "git", "rev-list", "--count", upstreamRef+"..HEAD")
	cmd2.Dir = worktreePath
	out, err := cmd2.Output()
	if err != nil {
		return false, true, nil
	}
	count := strings.TrimSpace(string(out))
	return count != "0" && count != "", true, nil
}

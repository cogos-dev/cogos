// decision_lineage_reconciler.go — the decision-lineage projection provider.
//
// This is the missing sibling to the convergence-map projection. Where
// convergence-map projects the *theoretical antecedent* lineage (von Foerster /
// Kauffman / Friston, read from .cog/mem/semantic/lineage/nodes/), this projects
// the *decision manifold*: the gravity / inertia field over the corpus's own
// architectural decisions (ADRs, RFCs, ratified decision-insights and their
// `rel:` edges).
//
// It implements the same pkg/reconcile.Reconcilable seven-method contract as
// ProjectionReconciler and registers under the projection-kind namespace as
//
//	lineage-projection-decision-lineage
//
// but reads a different corpus (DecisionCorpusDirs) and writes a different
// projection file (decision-lineage.md). Distinct corpus, same projection shape.
//
// ADR-094 §3: projection generation is a substrate primitive.
// ADR-092 §4: implements the seven-method Reconcilable contract.
package engine

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// DecisionLineageConfig is the declared config: the decision-corpus source
// directories and the output projection path.
type DecisionLineageConfig struct {
	CorpusDirs    []string
	ProjectionDir string
}

// DecisionLineageReconciler generates and maintains the decision-lineage
// projection (the spine manifold) from the decision corpus.
type DecisionLineageReconciler struct {
	mu     sync.Mutex
	phase  ProjectionPhase
	health reconcile.ResourceStatus

	// nowFn is the clock used for age/inertia; overridable in tests for
	// deterministic output. Defaults to time.Now().UTC.
	nowFn func() time.Time
}

// NewDecisionLineageReconciler constructs the provider.
func NewDecisionLineageReconciler() *DecisionLineageReconciler {
	return &DecisionLineageReconciler{
		phase:  ProjectionStarting,
		nowFn:  func() time.Time { return time.Now().UTC() },
		health: reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthProgressing),
	}
}

// Type returns the provider type identifier:
// "lineage-projection-decision-lineage".
func (r *DecisionLineageReconciler) Type() string {
	return "lineage-projection-" + string(ProjectionDecisionLineage)
}

// LoadConfig discovers the decision-corpus directories under root. When none of
// them exist (fresh install / non-cog workspace) it logs at DEBUG and returns
// nil so the cycle exits cleanly rather than WARNing every tick — matching the
// theoretical-lineage reconciler's quiet-when-absent behaviour.
func (r *DecisionLineageReconciler) LoadConfig(root string) (any, error) {
	dirs := DecisionCorpusDirs(root)
	anyExists := false
	for _, d := range dirs {
		if _, err := os.Stat(d); err == nil {
			anyExists = true
			break
		}
	}
	if !anyExists {
		slog.Debug("decision-lineage: no decision-corpus dirs present, skipping", "root", root)
		return nil, nil
	}

	lineageBase := filepath.Join(root, ".cog", "mem", "semantic", "lineage")
	projDir := filepath.Join(lineageBase, "projections")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		return nil, fmt.Errorf("create projections dir: %w", err)
	}

	return &DecisionLineageConfig{
		CorpusDirs:    dirs,
		ProjectionDir: projDir,
	}, nil
}

// FetchLive loads and parses the decision corpus. Read-only observation.
func (r *DecisionLineageReconciler) FetchLive(ctx context.Context, config any) (any, error) {
	if config == nil {
		return []Decision{}, nil
	}
	cfg, ok := config.(*DecisionLineageConfig)
	if !ok {
		return nil, fmt.Errorf("decision-lineage: unexpected config type %T", config)
	}
	// CorpusDirs all share the same workspace root; derive it from the first.
	root := deriveRootFromCorpusDir(cfg.CorpusDirs)
	decisions, err := LoadDecisionCorpus(root)
	if err != nil {
		r.mu.Lock()
		r.phase = ProjectionDetached
		r.mu.Unlock()
		return nil, fmt.Errorf("load decision corpus: %w", err)
	}
	return decisions, nil
}

// ComputePlan compares the current projection file against what the manifold
// would render. Deterministic given (config, live, state).
func (r *DecisionLineageReconciler) ComputePlan(config any, live any, state *reconcile.State) (*reconcile.Plan, error) {
	if config == nil {
		return &reconcile.Plan{ResourceType: r.Type()}, nil
	}
	cfg, ok := config.(*DecisionLineageConfig)
	if !ok {
		return nil, fmt.Errorf("decision-lineage ComputePlan: unexpected config type %T", config)
	}
	decisions, ok := live.([]Decision)
	if !ok {
		return nil, fmt.Errorf("decision-lineage ComputePlan: unexpected live type %T", live)
	}

	manifold := ComputeManifold(decisions, r.now())
	projected := renderDecisionLineageProjection(manifold)
	projPath := filepath.Join(cfg.ProjectionDir, string(ProjectionDecisionLineage)+".md")

	plan := &reconcile.Plan{
		ResourceType: r.Type(),
		GeneratedAt:  r.now().Format(time.RFC3339),
		ConfigPath:   cfg.ProjectionDir,
	}

	existing, err := os.ReadFile(projPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading projection %s: %w", projPath, err)
	}

	// Compare on a content basis that ignores the volatile "Last generated"
	// timestamp line, so an unchanged corpus does not churn every cycle.
	if err != nil || !equalIgnoringTimestamp(existing, []byte(projected)) {
		action := reconcile.ActionCreate
		if err == nil {
			action = reconcile.ActionUpdate
		}
		plan.Actions = append(plan.Actions, reconcile.Action{
			Action:       action,
			ResourceType: r.Type(),
			Name:         string(ProjectionDecisionLineage),
			Details: map[string]any{
				"path":           projPath,
				"decision_count": len(decisions),
				"kind":           string(ProjectionDecisionLineage),
				// Carry the already-rendered projection so ApplyPlan writes it
				// directly instead of re-loading + re-rendering the whole corpus.
				"content": projected,
			},
		})
		if action == reconcile.ActionCreate {
			plan.Summary.Creates++
		} else {
			plan.Summary.Updates++
		}
	} else {
		plan.Actions = append(plan.Actions, reconcile.Action{
			Action:       reconcile.ActionSkip,
			ResourceType: r.Type(),
			Name:         string(ProjectionDecisionLineage),
			Details:      map[string]any{"reason": "content unchanged"},
		})
		plan.Summary.Skipped++
	}

	return plan, nil
}

// ApplyPlan writes the projection file. Idempotent: re-derives content from the
// corpus and writes atomically (tmp + rename).
func (r *DecisionLineageReconciler) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	var results []reconcile.Result

	for _, action := range plan.Actions {
		if action.Action == reconcile.ActionSkip {
			results = append(results, reconcile.Result{
				Phase: "apply", Action: string(action.Action), Name: action.Name,
				Status: reconcile.ApplySkipped,
			})
			continue
		}

		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		projPath, _ := action.Details["path"].(string)
		if projPath == "" {
			results = append(results, reconcile.Result{
				Phase: "apply", Action: string(action.Action), Name: action.Name,
				Status: reconcile.ApplyFailed, Error: "missing path in action details",
			})
			continue
		}

		// Use the projection ComputePlan already rendered this cycle. Only fall
		// back to re-loading + re-rendering the whole decision corpus (the
		// redundant O(corpus) work FetchLive/ComputePlan already did) if a
		// caller built the plan without content — e.g. a test or a hand-built
		// plan. projPath = <root>/.cog/mem/semantic/lineage/projections/decision-lineage.md.
		content, _ := action.Details["content"].(string)
		if content == "" {
			root := deriveRootFromProjectionPath(projPath)
			decisions, derr := LoadDecisionCorpus(root)
			if derr != nil {
				results = append(results, reconcile.Result{
					Phase: "apply", Action: string(action.Action), Name: action.Name,
					Status: reconcile.ApplyFailed, Error: fmt.Sprintf("reload corpus: %v", derr),
				})
				continue
			}
			content = renderDecisionLineageProjection(ComputeManifold(decisions, r.now()))
		}

		tmp := projPath + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
			results = append(results, reconcile.Result{
				Phase: "apply", Action: string(action.Action), Name: action.Name,
				Status: reconcile.ApplyFailed, Error: fmt.Sprintf("write tmp: %v", err),
			})
			continue
		}
		if err := os.Rename(tmp, projPath); err != nil {
			os.Remove(tmp)
			results = append(results, reconcile.Result{
				Phase: "apply", Action: string(action.Action), Name: action.Name,
				Status: reconcile.ApplyFailed, Error: fmt.Sprintf("rename: %v", err),
			})
			continue
		}

		r.mu.Lock()
		r.phase = ProjectionLive
		r.health = reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
		r.mu.Unlock()

		results = append(results, reconcile.Result{
			Phase: "apply", Action: string(action.Action), Name: action.Name,
			Status: reconcile.ApplySucceeded,
		})
		decisionCount, _ := action.Details["decision_count"].(int)
		log.Printf("[decision-lineage-reconciler] wrote %s (%d decisions)", projPath, decisionCount)
	}

	return results, nil
}

// BuildState records each decision as a tracked resource plus the projection
// file itself. Pure — no side effects.
func (r *DecisionLineageReconciler) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	if config == nil {
		return reconcile.NewState(r.Type()), nil
	}
	cfg, ok := config.(*DecisionLineageConfig)
	if !ok {
		return nil, fmt.Errorf("decision-lineage BuildState: unexpected config type %T", config)
	}
	decisions, ok := live.([]Decision)
	if !ok {
		return nil, fmt.Errorf("decision-lineage BuildState: unexpected live type %T", live)
	}

	state := reconcile.NewState(r.Type())
	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial
	}

	manifold := ComputeManifold(decisions, r.now())
	for _, d := range decisions {
		v := manifold.Vertebrae[d.ID]
		state.Resources = append(state.Resources, reconcile.Resource{
			Address:    "decision." + d.ID,
			Type:       "decision",
			Mode:       reconcile.ModeManaged,
			Name:       d.ID,
			ExternalID: d.Path,
			Attributes: map[string]any{
				"kind":         d.Kind,
				"status":       d.Status,
				"gravity":      v.Gravity,
				"inertia":      v.Inertia,
				"cost_to_move": v.CostToMove,
				"basin":        v.Basin,
			},
			LastRefreshed: r.now().Format(time.RFC3339),
		})
	}

	projPath := filepath.Join(cfg.ProjectionDir, string(ProjectionDecisionLineage)+".md")
	state.Resources = append(state.Resources, reconcile.Resource{
		Address:       "lineage-projection." + string(ProjectionDecisionLineage),
		Type:          "lineage-projection",
		Mode:          reconcile.ModeManaged,
		Name:          string(ProjectionDecisionLineage),
		ExternalID:    projPath,
		Attributes:    map[string]any{"kind": string(ProjectionDecisionLineage)},
		LastRefreshed: r.now().Format(time.RFC3339),
	})

	return state, nil
}

// Health returns the current three-axis status.
func (r *DecisionLineageReconciler) Health() reconcile.ResourceStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.health
}

// now returns the reconciler clock (overridable in tests).
func (r *DecisionLineageReconciler) now() time.Time {
	if r.nowFn != nil {
		return r.nowFn()
	}
	return time.Now().UTC()
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// deriveRootFromCorpusDir reduces any corpus dir back to the workspace root.
// All DecisionCorpusDirs are <root>/.cog/...; we cut at the ".cog" segment.
func deriveRootFromCorpusDir(dirs []string) string {
	for _, d := range dirs {
		if i := lastIndexOfSegment(d, ".cog"); i >= 0 {
			return d[:i]
		}
	}
	return ""
}

// deriveRootFromProjectionPath reduces the projection file path to the root.
func deriveRootFromProjectionPath(p string) string {
	if i := lastIndexOfSegment(p, ".cog"); i >= 0 {
		return p[:i]
	}
	return ""
}

// lastIndexOfSegment returns the byte index where a path segment named seg
// begins (preceded by a separator), or -1. Returns the index of the separator
// so the returned prefix excludes the trailing separator.
func lastIndexOfSegment(path, seg string) int {
	clean := filepath.ToSlash(path)
	needle := "/" + seg + "/"
	if i := lastIndex(clean, needle); i >= 0 {
		return i
	}
	// Handle path that ends exactly at the segment.
	if suffix := "/" + seg; len(clean) >= len(suffix) && clean[len(clean)-len(suffix):] == suffix {
		return len(clean) - len(suffix)
	}
	return -1
}

// lastIndex is strings.LastIndex without importing strings into the hot path
// signature; kept tiny and obvious.
func lastIndex(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// equalIgnoringTimestamp compares two projection byte slices for equality,
// ignoring any line beginning with "_Last generated:". This keeps an unchanged
// corpus from producing a spurious diff on every reconcile cycle.
func equalIgnoringTimestamp(a, b []byte) bool {
	return stripTimestampLine(a) == stripTimestampLine(b)
}

func stripTimestampLine(data []byte) string {
	var out bytes.Buffer
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("_Last generated:")) {
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.String()
}

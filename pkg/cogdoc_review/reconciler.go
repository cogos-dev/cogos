// reconciler.go
// CogdocReviewReconciler — Layer A: always-on substrate guarantee.
//
// Implements pkg/reconcile.Reconcilable. The Reconciler runs before
// constellation indexing on every proposed cogdoc. It enforces the
// deterministic review pipeline: similarity search + forced acknowledgment.
//
// RFC-008 Reconcilable contract methods:
//   - Type()        → "cogdoc_review"
//   - LoadConfig()  → reads CogdocReviewClass from hook-config.yaml
//   - FetchLive()   → scans corpus for existing cogdocs + reads provenance fields
//   - ComputePlan() → determines if a proposed cogdoc has satisfied the gate
//   - ApplyPlan()   → blocks (returns error) if gate not satisfied; writes state
//   - BuildState()  → snapshots the current review corpus state
//   - Health()      → reports Healthy/Degraded/Missing based on Ollama reachability
//
// See also: pkg/substrate/reconcile/cogdoc_review_types.go (types)
// See also: pkg/cogdoc_review/similarity.go (search primitive)

package cogdoc_review

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

const (
	reconcilerType = "cogdoc_review"
)

// hookConfigYAML is the shape of the cogdoc_review section in hook-config.yaml.
// This matches the T08 config knob design from the schema cogdoc.
type hookConfigYAML struct {
	CogdocReview struct {
		Enabled        bool     `yaml:"enabled"`
		Threshold      float64  `yaml:"threshold"`
		TopN           int      `yaml:"top_n"`
		EmbedModel     string   `yaml:"embed_model"`
		OllamaEndpoint string   `yaml:"ollama_endpoint"`
		CorpusPaths    []string `yaml:"corpus_paths"`
	} `yaml:"cogdoc_review"`
}

// liveState is the "live" system state fetched by FetchLive.
// For the cogdoc review Reconciler, live state is:
// - the set of cogdocs in the corpus that have provenance fields
// - the set that lack provenance fields (potential gate violations)
type liveState struct {
	// ProvenancedCogdocs are cogdoc IDs that have a valid provenance field.
	ProvenancedCogdocs []string

	// UnreviewedCogdocs are cogdoc IDs in the corpus without provenance fields.
	// These are not necessarily violations — they predate the pipeline.
	UnreviewedCogdocs []string

	// CorpusSize is the total number of cogdocs scanned.
	CorpusSize int

	// OllamaReachable indicates whether Ollama embed endpoint is up.
	OllamaReachable bool
}

// CogdocReviewReconciler implements reconcile.Reconcilable.
// It is the Layer A enforcement of the deterministic review pipeline.
type CogdocReviewReconciler struct {
	// WorkspaceRoot is the absolute path to the workspace.
	// Set before registration.
	WorkspaceRoot string

	health reconcile.ResourceStatus
}

// NewCogdocReviewReconciler creates a new reconciler for the given workspace root.
func NewCogdocReviewReconciler(workspaceRoot string) *CogdocReviewReconciler {
	return &CogdocReviewReconciler{
		WorkspaceRoot: workspaceRoot,
		health:        reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthProgressing),
	}
}

// Type returns "cogdoc_review".
func (r *CogdocReviewReconciler) Type() string {
	return reconcilerType
}

// LoadConfig reads the CogdocReviewClass from .cog/hooks/hook-config.yaml.
// Returns a *CogdocReviewClass. Returns default class if config file not found
// or cogdoc_review section is absent.
func (r *CogdocReviewReconciler) LoadConfig(root string) (any, error) {
	configPath := filepath.Join(root, ".cog", "hooks", "hook-config.yaml")

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No config file: return default class (enabled with defaults).
			def := reconcile.DefaultCogdocReviewClass()
			return &def, nil
		}
		return nil, fmt.Errorf("read hook-config.yaml: %w", err)
	}

	var cfg hookConfigYAML
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse hook-config.yaml: %w", err)
	}

	class := reconcile.CogdocReviewClass{
		Enabled:             cfg.CogdocReview.Enabled,
		SimilarityThreshold: cfg.CogdocReview.Threshold,
		TopN:                cfg.CogdocReview.TopN,
		EmbedModel:          cfg.CogdocReview.EmbedModel,
		OllamaEndpoint:      cfg.CogdocReview.OllamaEndpoint,
		CorpusPaths:         cfg.CogdocReview.CorpusPaths,
	}

	// Apply defaults for zero values.
	if class.SimilarityThreshold == 0 {
		class.SimilarityThreshold = 0.70
	}
	if class.TopN == 0 {
		class.TopN = 5
	}

	return &class, nil
}

// FetchLive scans the corpus and reports which cogdocs have provenance fields
// and which don't. Also probes Ollama reachability.
func (r *CogdocReviewReconciler) FetchLive(ctx context.Context, config any) (any, error) {
	class, ok := config.(*reconcile.CogdocReviewClass)
	if !ok {
		return nil, fmt.Errorf("cogdoc_review FetchLive: expected *CogdocReviewClass, got %T", config)
	}

	// When the pipeline is disabled, skip the O(corpus) WalkDir+YAML parse and
	// the Ollama HTTP probe entirely. ComputePlan short-circuits on !Enabled and
	// only reads CorpusSize/OllamaReachable for plan metadata, so a zero-value
	// liveState is sufficient. Without this guard a disabled reconciler still
	// walked the whole corpus and made a 3s Ollama probe on every ~30s tick.
	if !class.Enabled {
		return &liveState{}, nil
	}

	corpus, err := walkCorpus(r.WorkspaceRoot, class.CorpusPaths)
	if err != nil {
		return nil, fmt.Errorf("walk corpus: %w", err)
	}

	live := &liveState{CorpusSize: len(corpus)}

	for _, entry := range corpus {
		if hasProvenanceField(filepath.Join(r.WorkspaceRoot, entry.FilePath)) {
			live.ProvenancedCogdocs = append(live.ProvenancedCogdocs, entry.FM.ID)
		} else {
			live.UnreviewedCogdocs = append(live.UnreviewedCogdocs, entry.FM.ID)
		}
	}

	// Probe Ollama.
	endpoint := class.OllamaEndpoint
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	live.OllamaReachable = probeOllama(ctx, endpoint)

	return live, nil
}

// ComputePlan compares declared config against live state.
// For the cogdoc review Reconciler, "plan" means: which (if any) candidate
// proposals are blocked by missing provenance fields.
//
// In the context of the full pipeline, ComputePlan is called by the kernel's
// reconcile loop, not by the skill or the hook directly. It produces the
// kernel-side view of review compliance.
func (r *CogdocReviewReconciler) ComputePlan(config any, live any, state *reconcile.State) (*reconcile.Plan, error) {
	class, ok := config.(*reconcile.CogdocReviewClass)
	if !ok {
		return nil, fmt.Errorf("ComputePlan: expected *CogdocReviewClass")
	}

	lv, ok := live.(*liveState)
	if !ok {
		return nil, fmt.Errorf("ComputePlan: expected *liveState")
	}

	plan := &reconcile.Plan{
		ResourceType: reconcilerType,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath:   ".cog/hooks/hook-config.yaml",
		Metadata: map[string]any{
			"pipeline_enabled":    class.Enabled,
			"corpus_size":         lv.CorpusSize,
			"ollama_reachable":    lv.OllamaReachable,
			"unreviewed_count":    len(lv.UnreviewedCogdocs),
			"provenanced_count":   len(lv.ProvenancedCogdocs),
		},
	}

	if !class.Enabled {
		plan.Actions = []reconcile.Action{{
			Action:       reconcile.ActionSkip,
			ResourceType: reconcilerType,
			Name:         "pipeline",
			Details:      map[string]any{"reason": "cogdoc_review.enabled is false"},
		}}
		plan.Summary.Skipped = 1
		return plan, nil
	}

	if !lv.OllamaReachable {
		plan.Warnings = append(plan.Warnings,
			"Ollama embed endpoint is unreachable; similarity search disabled until it recovers")
	}

	// All unreviewed cogdocs predate the pipeline — they are not violations,
	// just unreviewed. No forced remediation of the existing corpus.
	// The Reconciler only gates NEW cogdocs (handled by the hook + skill).
	// Kernel-side: report corpus health but take no forced action on old docs.
	plan.Actions = []reconcile.Action{{
		Action:       reconcile.ActionSkip,
		ResourceType: reconcilerType,
		Name:         "existing-corpus",
		Details: map[string]any{
			"unreviewed": len(lv.UnreviewedCogdocs),
			"provenanced": len(lv.ProvenancedCogdocs),
			"note": "Existing corpus predates pipeline; no forced remediation",
		},
	}}
	plan.Summary.Skipped = 1
	return plan, nil
}

// ApplyPlan executes the planned changes.
// For the cogdoc review Reconciler, Apply updates health status only.
// Gate enforcement is done at the hook layer (pre-commit) and skill layer.
func (r *CogdocReviewReconciler) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	ollamaReachable, _ := plan.Metadata["ollama_reachable"].(bool)
	if ollamaReachable {
		r.health = reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
	} else {
		r.health = reconcile.ResourceStatus{
			Sync:    reconcile.SyncStatusSynced,
			Health:  reconcile.HealthDegraded,
			Message: "Ollama embed endpoint unreachable; similarity search degraded",
		}
	}

	return []reconcile.Result{{
		Phase:  "apply",
		Action: "update",
		Name:   "cogdoc_review.health",
		Status: reconcile.ApplySucceeded,
	}}, nil
}

// BuildState constructs a state snapshot from live data.
func (r *CogdocReviewReconciler) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	lv, ok := live.(*liveState)
	if !ok {
		return nil, fmt.Errorf("BuildState: expected *liveState")
	}

	serial := 1
	lineage := "cogdoc-review-pipeline"
	if existing != nil {
		serial = existing.Serial + 1
		lineage = existing.Lineage
	}

	resources := make([]reconcile.Resource, 0, len(lv.ProvenancedCogdocs)+1)

	resources = append(resources, reconcile.Resource{
		Address:     "cogdoc_review.corpus",
		Type:        reconcilerType,
		Mode:        reconcile.ModeManaged,
		Name:        "corpus",
		LastRefreshed: time.Now().UTC().Format(time.RFC3339),
		Attributes: map[string]any{
			"corpus_size":       lv.CorpusSize,
			"provenanced_count": len(lv.ProvenancedCogdocs),
			"unreviewed_count":  len(lv.UnreviewedCogdocs),
			"ollama_reachable":  lv.OllamaReachable,
		},
	})

	return &reconcile.State{
		Version:      1,
		Lineage:      lineage,
		Serial:       serial,
		ResourceType: reconcilerType,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Resources:    resources,
	}, nil
}

// Health returns the current three-axis status.
func (r *CogdocReviewReconciler) Health() reconcile.ResourceStatus {
	return r.health
}

// --- Gate API (used by hook and skill, not the kernel reconcile loop) ---

// CheckProposal is the primary gate API for Layer B (hook) and Layer C (skill).
// Given a proposed cogdoc query text, it runs the similarity search and returns
// the candidates that require acknowledgment.
//
// Returns empty slice if:
//   - Pipeline is disabled in config
//   - No candidates above threshold
//   - Ollama is unreachable (gate degrades gracefully: logs warning, passes)
func (r *CogdocReviewReconciler) CheckProposal(
	ctx context.Context,
	class *reconcile.CogdocReviewClass,
	proposal reconcile.CogdocProposal,
) ([]reconcile.SimilarityCandidate, error) {
	if !class.Enabled {
		return nil, nil
	}

	cfg := SimilaritySearchConfig{
		WorkspaceRoot:  r.WorkspaceRoot,
		OllamaEndpoint: class.OllamaEndpoint,
		EmbedModel:     class.EmbedModel,
		Threshold:      class.SimilarityThreshold,
		TopN:           class.TopN,
		CorpusPaths:    class.CorpusPaths,
		ExcludeFile:    proposal.FilePath,
	}

	return SearchSimilarFromProposal(ctx, cfg, proposal)
}

// ValidateProvenance checks whether a cogdoc file's provenance field covers
// all required candidates. Returns nil if the gate is satisfied.
func (r *CogdocReviewReconciler) ValidateProvenance(
	cogdocPath string,
	requiredCandidateIDs []string,
) error {
	absPath := cogdocPath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(r.WorkspaceRoot, cogdocPath)
	}

	prov, err := readProvenanceField(absPath)
	if err != nil {
		return fmt.Errorf("read provenance: %w", err)
	}
	if prov == nil {
		return fmt.Errorf("cogdoc %s has no provenance field; review pipeline gate not satisfied", cogdocPath)
	}

	// Check all required candidates are acknowledged.
	acked := map[string]bool{}
	for _, ack := range prov.Acknowledgments {
		acked[ack.CogdocID] = true
	}
	for _, id := range requiredCandidateIDs {
		if !acked[id] {
			return fmt.Errorf("cogdoc %s missing acknowledgment for candidate %s", cogdocPath, id)
		}
	}
	return nil
}

// --- helpers ---

// hasProvenanceField returns true if the cogdoc at absPath has a provenance field.
func hasProvenanceField(absPath string) bool {
	p, err := readProvenanceField(absPath)
	return err == nil && p != nil
}

// provenanceFrontmatter is used only for checking whether the field exists.
type provenanceFrontmatter struct {
	Provenance *reconcile.ProvenanceRecord `yaml:"provenance"`
}

// readProvenanceField reads the provenance field from a cogdoc frontmatter.
// Returns nil if no provenance field present. Does not error on missing field.
func readProvenanceField(absPath string) (*reconcile.ProvenanceRecord, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}

	// Extract YAML frontmatter.
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return nil, nil // no frontmatter
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return nil, nil
	}
	fmYAML := s[4 : 4+end]

	var pfm provenanceFrontmatter
	if err := yaml.Unmarshal([]byte(fmYAML), &pfm); err != nil {
		return nil, nil // malformed frontmatter, treat as no provenance
	}
	return pfm.Provenance, nil
}

// probeOllama returns true if the Ollama endpoint responds with HTTP 200
// on a lightweight health check (GET /api/tags).
func probeOllama(ctx context.Context, endpoint string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, endpoint+"/api/tags", nil)
	if err != nil {
		return false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

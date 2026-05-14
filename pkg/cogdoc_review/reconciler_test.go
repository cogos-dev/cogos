// reconciler_test.go
// T06: Unit tests for CogdocReviewReconciler lifecycle.
//
// Tests cover:
//   - LoadConfig with defaults (no config file)
//   - LoadConfig from hook-config.yaml with cogdoc_review section
//   - ComputePlan with pipeline disabled
//   - ComputePlan with pipeline enabled (no violations)
//   - BuildState shape
//   - Health initial state
//   - CheckProposal: gate passes when disabled
//   - ValidateProvenance: gate passes when provenance field present
//   - ValidateProvenance: gate fails when provenance field absent
//
// Note: Tests that require Ollama (similarity search) are integration tests
// in reconciler_integration_test.go (T09) and require a build tag.
// These unit tests mock or bypass the embed call.

package cogdoc_review_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/myrgic/cogos/pkg/cogdoc_review"
	"github.com/myrgic/cogos/pkg/reconcile"
)

// makeWorkspace creates a temp directory with a minimal workspace structure.
func makeWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cogDir := filepath.Join(dir, ".cog", "mem", "semantic", "insights")
	if err := os.MkdirAll(cogDir, 0755); err != nil {
		t.Fatal(err)
	}
	adrDir := filepath.Join(dir, ".cog", "adr")
	if err := os.MkdirAll(adrDir, 0755); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(dir, ".cog", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeCogdoc writes a minimal .cog.md file with optional provenance field.
func writeCogdoc(t *testing.T, dir, relPath, id, title string, withProvenance bool) {
	t.Helper()
	absPath := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		t.Fatal(err)
	}

	fm := map[string]any{
		"type":  "insight",
		"id":    id,
		"title": title,
	}
	if withProvenance {
		fm["provenance"] = map[string]any{
			"pipeline_version":    "v0.1.0",
			"proposed_at":        time.Now().UTC().Format(time.RFC3339),
			"reviewed_at":        time.Now().UTC().Format(time.RFC3339),
			"candidates_surfaced": 0,
			"acknowledgments":    []any{},
			"action_taken":       "authored",
		}
	}

	fmBytes, err := yaml.Marshal(fm)
	if err != nil {
		t.Fatal(err)
	}

	content := "---\n" + string(fmBytes) + "---\n\n# " + title + "\n\nBody text.\n"
	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// writeHookConfig writes a hook-config.yaml with cogdoc_review section.
func writeHookConfig(t *testing.T, dir string, enabled bool, threshold float64, topN int) {
	t.Helper()
	cfg := map[string]any{
		"cogdoc_review": map[string]any{
			"enabled":   enabled,
			"threshold": threshold,
			"top_n":     topN,
		},
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".cog", "hooks", "hook-config.yaml")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadConfig_Defaults verifies defaults are returned when no config file exists.
func TestLoadConfig_Defaults(t *testing.T) {
	dir := makeWorkspace(t)
	r := cogdoc_review.NewCogdocReviewReconciler(dir)

	cfg, err := r.LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	class, ok := cfg.(*reconcile.CogdocReviewClass)
	if !ok {
		t.Fatalf("expected *CogdocReviewClass, got %T", cfg)
	}

	if !class.Enabled {
		t.Error("default Enabled should be true")
	}
	if class.SimilarityThreshold != 0.70 {
		t.Errorf("default Threshold: got %v, want 0.70", class.SimilarityThreshold)
	}
	if class.TopN != 5 {
		t.Errorf("default TopN: got %v, want 5", class.TopN)
	}
}

// TestLoadConfig_FromFile verifies config is read from hook-config.yaml.
func TestLoadConfig_FromFile(t *testing.T) {
	dir := makeWorkspace(t)
	writeHookConfig(t, dir, false, 0.80, 3)

	r := cogdoc_review.NewCogdocReviewReconciler(dir)
	cfg, err := r.LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	class, ok := cfg.(*reconcile.CogdocReviewClass)
	if !ok {
		t.Fatalf("expected *CogdocReviewClass, got %T", cfg)
	}

	if class.Enabled {
		t.Error("expected Enabled=false from config")
	}
	if class.SimilarityThreshold != 0.80 {
		t.Errorf("threshold: got %v, want 0.80", class.SimilarityThreshold)
	}
	if class.TopN != 3 {
		t.Errorf("top_n: got %v, want 3", class.TopN)
	}
}

// TestComputePlan_Disabled verifies plan is a skip when pipeline disabled.
func TestComputePlan_Disabled(t *testing.T) {
	dir := makeWorkspace(t)
	r := cogdoc_review.NewCogdocReviewReconciler(dir)

	class := &reconcile.CogdocReviewClass{Enabled: false}
	live := &struct{}{} // not used when disabled

	// Use FetchLive to get proper live type.
	// For disabled test, we need to use the actual liveState.
	// Load from FetchLive with disabled class.
	ctx := context.Background()
	lv, err := r.FetchLive(ctx, class)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	_ = live

	plan, err := r.ComputePlan(class, lv, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	if plan.Summary.Skipped != 1 {
		t.Errorf("disabled plan should have 1 skip, got %v", plan.Summary.Skipped)
	}
	if plan.Summary.HasChanges() {
		t.Error("disabled plan should have no changes")
	}
}

// TestComputePlan_EmptyCorpus verifies plan on an empty corpus.
func TestComputePlan_EmptyCorpus(t *testing.T) {
	dir := makeWorkspace(t)
	r := cogdoc_review.NewCogdocReviewReconciler(dir)

	class := &reconcile.CogdocReviewClass{
		Enabled:             true,
		SimilarityThreshold: 0.70,
		TopN:                5,
	}

	ctx := context.Background()
	lv, err := r.FetchLive(ctx, class)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}

	plan, err := r.ComputePlan(class, lv, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	if plan.ResourceType != "cogdoc_review" {
		t.Errorf("ResourceType: got %q, want %q", plan.ResourceType, "cogdoc_review")
	}
}

// TestBuildState_Shape verifies BuildState returns a valid state struct.
func TestBuildState_Shape(t *testing.T) {
	dir := makeWorkspace(t)
	// Add two cogdocs: one with provenance, one without.
	writeCogdoc(t, dir, ".cog/mem/semantic/insights/reviewed.cog.md", "reviewed-1", "Reviewed Insight", true)
	writeCogdoc(t, dir, ".cog/mem/semantic/insights/unreviewed.cog.md", "unreviewed-1", "Unreviewed Insight", false)

	r := cogdoc_review.NewCogdocReviewReconciler(dir)
	class := &reconcile.CogdocReviewClass{Enabled: true, SimilarityThreshold: 0.70, TopN: 5}

	ctx := context.Background()
	lv, err := r.FetchLive(ctx, class)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}

	state, err := r.BuildState(class, lv, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}

	if state.ResourceType != "cogdoc_review" {
		t.Errorf("ResourceType: got %q", state.ResourceType)
	}
	if state.Serial != 1 {
		t.Errorf("Serial: got %d, want 1", state.Serial)
	}
	if len(state.Resources) == 0 {
		t.Error("Resources should not be empty")
	}

	// Verify the corpus resource attributes.
	corpus := state.Resources[0]
	if corpus.Name != "corpus" {
		t.Errorf("first resource name: got %q, want %q", corpus.Name, "corpus")
	}
	corpusSize, _ := corpus.Attributes["corpus_size"].(int)
	if corpusSize != 2 {
		t.Errorf("corpus_size: got %v, want 2", corpus.Attributes["corpus_size"])
	}
}

// TestHealth_Initial verifies the initial health state.
func TestHealth_Initial(t *testing.T) {
	dir := makeWorkspace(t)
	r := cogdoc_review.NewCogdocReviewReconciler(dir)

	h := r.Health()
	// Initial state before any Apply: should be Progressing (not Healthy or Degraded).
	if h.Health == reconcile.HealthHealthy {
		t.Error("initial health should not be Healthy before first Apply")
	}
}

// TestCheckProposal_Disabled verifies gate passes (empty result) when disabled.
func TestCheckProposal_Disabled(t *testing.T) {
	dir := makeWorkspace(t)
	r := cogdoc_review.NewCogdocReviewReconciler(dir)

	class := &reconcile.CogdocReviewClass{Enabled: false}
	proposal := reconcile.CogdocProposal{
		ProposedID: "test-proposal",
		Title:      "Test Proposal",
		ProposedAt: time.Now().UTC(),
	}

	ctx := context.Background()
	candidates, err := r.CheckProposal(ctx, class, proposal)
	if err != nil {
		t.Fatalf("CheckProposal: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("disabled gate should return 0 candidates, got %d", len(candidates))
	}
}

// TestValidateProvenance_Present verifies gate passes when provenance field exists.
func TestValidateProvenance_Present(t *testing.T) {
	dir := makeWorkspace(t)
	relPath := ".cog/mem/semantic/insights/with-provenance.cog.md"
	writeCogdoc(t, dir, relPath, "with-prov", "With Provenance", true)

	r := cogdoc_review.NewCogdocReviewReconciler(dir)
	err := r.ValidateProvenance(filepath.Join(dir, relPath), nil)
	if err != nil {
		t.Errorf("ValidateProvenance with provenance: expected nil, got %v", err)
	}
}

// TestValidateProvenance_Absent verifies gate fails when provenance field missing.
func TestValidateProvenance_Absent(t *testing.T) {
	dir := makeWorkspace(t)
	relPath := ".cog/mem/semantic/insights/no-provenance.cog.md"
	writeCogdoc(t, dir, relPath, "no-prov", "No Provenance", false)

	r := cogdoc_review.NewCogdocReviewReconciler(dir)
	err := r.ValidateProvenance(filepath.Join(dir, relPath), nil)
	if err == nil {
		t.Error("ValidateProvenance without provenance: expected error, got nil")
	}
}

// TestType verifies the reconciler type string.
func TestType(t *testing.T) {
	r := cogdoc_review.NewCogdocReviewReconciler("/tmp")
	if got := r.Type(); got != "cogdoc_review" {
		t.Errorf("Type(): got %q, want %q", got, "cogdoc_review")
	}
}

// TestProvenanceRecord_AllDistinct verifies the AllDistinct helper.
func TestProvenanceRecord_AllDistinct(t *testing.T) {
	prov := reconcile.ProvenanceRecord{
		Acknowledgments: []reconcile.CandidateAcknowledgment{
			{CogdocID: "a", Decision: reconcile.AckReadDistinct},
			{CogdocID: "b", Decision: reconcile.AckReadDistinct},
		},
	}
	if !prov.AllDistinct() {
		t.Error("AllDistinct: expected true")
	}

	prov.Acknowledgments = append(prov.Acknowledgments, reconcile.CandidateAcknowledgment{
		CogdocID: "c",
		Decision: reconcile.AckAmendInstead,
	})
	if prov.AllDistinct() {
		t.Error("AllDistinct with amend-instead: expected false")
	}
}

// TestProvenanceRecord_HasAmendDecision verifies the HasAmendDecision helper.
func TestProvenanceRecord_HasAmendDecision(t *testing.T) {
	prov := reconcile.ProvenanceRecord{
		Acknowledgments: []reconcile.CandidateAcknowledgment{
			{CogdocID: "a", Decision: reconcile.AckReadDistinct},
		},
	}
	if prov.HasAmendDecision() {
		t.Error("HasAmendDecision: expected false with only read+distinct")
	}

	prov.Acknowledgments = append(prov.Acknowledgments, reconcile.CandidateAcknowledgment{
		CogdocID: "b",
		Decision: reconcile.AckAmendInstead,
	})
	if !prov.HasAmendDecision() {
		t.Error("HasAmendDecision: expected true with amend-instead present")
	}
}

// TestCogdocProposal_QueryText verifies the query text composition.
func TestCogdocProposal_QueryText(t *testing.T) {
	p := reconcile.CogdocProposal{Title: "My Title"}
	if got := p.QueryText(); got != "My Title" {
		t.Errorf("QueryText (no abstract): got %q", got)
	}

	p.Abstract = "My abstract."
	want := "My Title\n\nMy abstract."
	if got := p.QueryText(); got != want {
		t.Errorf("QueryText (with abstract): got %q, want %q", got, want)
	}
}

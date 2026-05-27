// decision_lineage_reconciler_test.go — tests for the decision-lineage
// projection provider, its corpus loader, and the rendered CLI views.
package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// adrDoc returns a minimal ADR cogdoc with the given slug, status, date, and
// refs (rel→targetSlug pairs).
func adrDoc(slug, status, created string, refs map[string]string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("type: adr\n")
	b.WriteString("id: \"" + slug + "\"\n")
	b.WriteString("title: 'ADR: " + slug + "'\n")
	b.WriteString("status: " + status + "\n")
	b.WriteString("created: " + created + "\n")
	b.WriteString("slug: " + slug + "\n")
	if len(refs) > 0 {
		b.WriteString("refs:\n")
		for rel, target := range refs {
			b.WriteString("- uri: cog://architecture/adrs/" + target + "\n")
			b.WriteString("  rel: " + rel + "\n")
		}
	}
	b.WriteString("---\n\n# " + slug + "\n\nBody.\n")
	return b.String()
}

// makeDecisionWorkspace writes ADR fixtures into <root>/.cog/architecture/adrs/.
func makeDecisionWorkspace(t *testing.T, adrs map[string]string) (root string) {
	t.Helper()
	root = t.TempDir()
	adrDir := filepath.Join(root, ".cog", "architecture", "adrs")
	if err := os.MkdirAll(adrDir, 0755); err != nil {
		t.Fatalf("mkdir adrs: %v", err)
	}
	for name, content := range adrs {
		if err := os.WriteFile(filepath.Join(adrDir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func TestLoadDecisionCorpus_ParsesEdges(t *testing.T) {
	root := makeDecisionWorkspace(t, map[string]string{
		"ADR-block.cog.md": adrDoc("block", "accepted", "2025-12-01", nil),
		"ADR-leaf.cog.md": adrDoc("leaf", "accepted", "2026-04-01", map[string]string{
			"builds-on": "block",
		}),
	})

	decisions, err := LoadDecisionCorpus(root)
	if err != nil {
		t.Fatalf("LoadDecisionCorpus: %v", err)
	}
	if len(decisions) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(decisions))
	}

	byID := map[string]Decision{}
	for _, d := range decisions {
		byID[d.ID] = d
	}
	leaf, ok := byID["leaf"]
	if !ok {
		t.Fatal("leaf decision not loaded (slug key mismatch)")
	}
	if len(leaf.Edges) != 1 {
		t.Fatalf("leaf edges = %d, want 1", len(leaf.Edges))
	}
	if leaf.Edges[0].Rel != "builds-on" || leaf.Edges[0].Target != "block" {
		t.Errorf("leaf edge = %+v, want builds-on→block", leaf.Edges[0])
	}
	if byID["block"].Status != "accepted" {
		t.Errorf("block status = %q, want accepted", byID["block"].Status)
	}
}

func TestResolveEdgeTarget(t *testing.T) {
	cases := map[string]string{
		"cog://architecture/adrs/cogblock-protocol": "cogblock-protocol",
		"cog://architecture/rfcs/display-numbers":   "display-numbers",
		"bare-slug":     "bare-slug",
		"cog://adr/078": "078",
	}
	for in, want := range cases {
		if got := resolveEdgeTarget(in); got != want {
			t.Errorf("resolveEdgeTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecisionLineageReconciler_FullCycle(t *testing.T) {
	root := makeDecisionWorkspace(t, map[string]string{
		"ADR-block.cog.md": adrDoc("block", "accepted", "2025-12-01", nil),
		"ADR-fossil.cog.md": adrDoc("fossil", "superseded", "2025-12-05", map[string]string{
			"superseded-by": "block",
		}),
		"ADR-leaf.cog.md": adrDoc("leaf", "accepted", "2026-04-01", map[string]string{
			"builds-on": "block",
		}),
	})

	rec := NewDecisionLineageReconciler()
	rec.nowFn = func() time.Time { return fixtureNow } // deterministic
	ctx := context.Background()

	cfgAny, err := rec.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfgAny == nil {
		t.Fatal("LoadConfig returned nil despite corpus present")
	}

	liveAny, err := rec.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	decisions := liveAny.([]Decision)
	if len(decisions) != 3 {
		t.Fatalf("FetchLive: expected 3 decisions, got %d", len(decisions))
	}

	// First plan → create.
	plan, err := rec.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 1 {
		t.Errorf("expected 1 create, got %+v", plan.Summary)
	}

	results, err := rec.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	for _, r := range results {
		if r.Status == reconcile.ApplyFailed {
			t.Errorf("apply failed: %s", r.Error)
		}
	}

	// Projection file written, with the spine content.
	cfg := cfgAny.(*DecisionLineageConfig)
	projPath := filepath.Join(cfg.ProjectionDir, "decision-lineage.md")
	data, err := os.ReadFile(projPath)
	if err != nil {
		t.Fatalf("projection not written: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"Decision Lineage — The Spine Manifold",
		"Centre of Mass",
		"`block`",
		"Accretion Over Time",
		"Orbits / Basins",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("projection missing %q", want)
		}
	}

	// Idempotency: second plan → skip (no churn from timestamp).
	liveAny2, _ := rec.FetchLive(ctx, cfgAny)
	plan2, err := rec.ComputePlan(cfgAny, liveAny2, nil)
	if err != nil {
		t.Fatalf("second ComputePlan: %v", err)
	}
	if plan2.Summary.HasChanges() {
		t.Errorf("expected no changes on second plan, got %+v", plan2.Summary)
	}

	// BuildState records each decision + the projection file.
	state, err := rec.BuildState(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if state.ResourceType != rec.Type() {
		t.Errorf("state type = %q, want %q", state.ResourceType, rec.Type())
	}
	// 3 decisions + 1 projection file.
	if len(state.Resources) != 4 {
		t.Errorf("state resources = %d, want 4", len(state.Resources))
	}

	if rec.Health().Sync != reconcile.SyncStatusSynced {
		t.Errorf("health = %s, want Synced", rec.Health().Sync)
	}
}

func TestDecisionLineageReconciler_AbsentCorpus(t *testing.T) {
	// Empty workspace with no .cog/architecture dir → LoadConfig returns nil,
	// the cycle exits cleanly with no drift (quiet-when-absent contract).
	root := t.TempDir()
	rec := NewDecisionLineageReconciler()

	cfgAny, err := rec.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfgAny != nil {
		t.Errorf("LoadConfig should return nil for absent corpus, got %T", cfgAny)
	}
	// Downstream methods must tolerate nil config.
	liveAny, err := rec.FetchLive(context.Background(), cfgAny)
	if err != nil {
		t.Fatalf("FetchLive(nil): %v", err)
	}
	plan, err := rec.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan(nil): %v", err)
	}
	if plan.Summary.HasChanges() {
		t.Errorf("absent corpus should yield no changes, got %+v", plan.Summary)
	}
}

func TestDecisionLineageReconciler_Type(t *testing.T) {
	rec := NewDecisionLineageReconciler()
	if rec.Type() != "lineage-projection-decision-lineage" {
		t.Errorf("Type() = %q, want lineage-projection-decision-lineage", rec.Type())
	}
}

func TestDecisionLineageReconciler_Registered(t *testing.T) {
	RegisterProjectionProviders()
	if !reconcile.HasProvider("lineage-projection-decision-lineage") {
		t.Error("decision-lineage provider not registered")
	}
}

func TestRootDerivation(t *testing.T) {
	// Round-trip: a corpus dir and a projection path must both derive the root.
	root := "/tmp/ws"
	dirs := DecisionCorpusDirs(root)
	if got := deriveRootFromCorpusDir(dirs); got != root {
		t.Errorf("deriveRootFromCorpusDir = %q, want %q", got, root)
	}
	projPath := filepath.Join(root, ".cog", "mem", "semantic", "lineage", "projections", "decision-lineage.md")
	if got := deriveRootFromProjectionPath(projPath); got != root {
		t.Errorf("deriveRootFromProjectionPath = %q, want %q", got, root)
	}
}

func TestSpineCLIViews(t *testing.T) {
	m := ComputeManifold(fixtureCorpus(), fixtureNow)

	overview := renderSpineOverview(m)
	for _, want := range []string{"THE SPINE", "CENTRE OF MASS", "BASINS", "ACCRETION ERAS", "block"} {
		if !strings.Contains(overview, want) {
			t.Errorf("overview missing %q", want)
		}
	}

	detail := renderSpineDecision(m, m.Vertebrae["block"])
	for _, want := range []string{"VERTEBRA: block", "GRAVITY:", "INERTIA:", "COST-TO-MOVE:", "INCOMING EDGES"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q", want)
		}
	}
	// block has structural incoming edges → the structural marker must appear.
	if !strings.Contains(detail, "*") {
		t.Error("detail should mark structural edges with *")
	}

	orbits := renderSpineOrbits(m)
	if !strings.Contains(orbits, "ORBITAL BASINS") || !strings.Contains(orbits, "WELL: block") {
		t.Error("orbits view missing expected content")
	}

	accretion := renderSpineAccretion(m)
	if !strings.Contains(accretion, "ACCRETION TIMELINE") {
		t.Error("accretion view missing header")
	}
}

// projection_reconciler_test.go
// Integration tests for ProjectionReconciler and ProjectionWatcher.
//
// D6 — "Integration test: change a node's frontmatter → projection regenerates
// within bounded window."
//
// Tests:
//   - TestProjectionReconciler_FullCycle: complete LoadConfig/FetchLive/ComputePlan/ApplyPlan/BuildState cycle
//   - TestProjectionReconciler_Idempotent: applying same plan twice produces same result
//   - TestProjectionReconciler_AllSixProjections: all six kinds produce non-empty output
//   - TestProjectionWatcher_TriggerOnWrite: watcher calls trigger after node file write
//   - TestProjectionReconciler_Registration: RegisterProjectionProviders registers 6 providers

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/reconcile"
)

// ─── Test helpers ─────────────────────────────────────────────────────────────

// makeTestWorkspace creates a temporary workspace with a lineage nodes directory
// and the given node cogdoc files. Returns the workspace root and a cleanup func.
func makeTestWorkspace(t *testing.T, nodeFiles map[string]string) (root string, cleanup func()) {
	t.Helper()
	root = t.TempDir()

	nodesDir := filepath.Join(root, ".cog", "mem", "semantic", "lineage", "nodes")
	if err := os.MkdirAll(nodesDir, 0755); err != nil {
		t.Fatalf("create nodes dir: %v", err)
	}

	for name, content := range nodeFiles {
		if err := os.WriteFile(filepath.Join(nodesDir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write node file %s: %v", name, err)
		}
	}

	return root, func() { os.RemoveAll(root) }
}

// titleCase converts "foo-bar-baz" to "Foo Bar Baz" without using strings.Title (deprecated).
func titleCase(s string) string {
	words := strings.Fields(strings.ReplaceAll(s, "-", " "))
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// sampleNode returns a minimal valid lineage node cogdoc.
func sampleNode(id, tier, exposure string) string {
	return `---
id: ` + id + `
kind: lineage-node
tier: ` + tier + `
title: "` + titleCase(id) + `"
public_exposure_risk: ` + exposure + `
demotion_template: "Test demotion template for ` + id + `."
corpus_depth: medium
refs:
  - rel: grounds
    uri: cog://mem/semantic/lineage/nodes/antecedent-test
    note: Test ref
created: 2026-05-16T00:00:00Z
updated: 2026-05-16T00:00:00Z
---

# ` + titleCase(id) + `

Test node body.
`
}

// ─── Tests ────────────────────────────────────────────────────────────────────

func TestProjectionReconciler_FullCycle(t *testing.T) {
	root, cleanup := makeTestWorkspace(t, map[string]string{
		"antecedent-pattee.cog.md": sampleNode("antecedent-pattee", "1", "low"),
		"thread-b-pattee.cog.md":   sampleNode("thread-b-pattee", "2", "medium"),
		"thread-h-cft.cog.md":      sampleNode("thread-h-cft", "3", "high"),
	})
	defer cleanup()

	rec := NewProjectionReconciler(ProjectionBibliography)

	// LoadConfig
	cfgAny, err := rec.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg, ok := cfgAny.(*ProjectionConfig)
	if !ok {
		t.Fatalf("LoadConfig: expected *ProjectionConfig, got %T", cfgAny)
	}
	if cfg.NodesDir == "" {
		t.Error("LoadConfig: NodesDir is empty")
	}

	// FetchLive
	ctx := context.Background()
	liveAny, err := rec.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	nodes, ok := liveAny.([]LineageNode)
	if !ok {
		t.Fatalf("FetchLive: expected []LineageNode, got %T", liveAny)
	}
	if len(nodes) != 3 {
		t.Errorf("FetchLive: expected 3 nodes, got %d", len(nodes))
	}

	// ComputePlan (first run — projection file does not exist yet → should create)
	plan, err := rec.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if !plan.Summary.HasChanges() {
		t.Error("ComputePlan: expected changes on first run, got none")
	}
	if plan.Summary.Creates != 1 {
		t.Errorf("ComputePlan: expected 1 create, got %+v", plan.Summary)
	}

	// ApplyPlan
	results, err := rec.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("ApplyPlan: no results")
	}
	for _, r := range results {
		if r.Status == reconcile.ApplyFailed {
			t.Errorf("ApplyPlan: result failed: %s", r.Error)
		}
	}

	// Verify projection file was created.
	projPath := filepath.Join(cfg.ProjectionDir, string(ProjectionBibliography)+".md")
	if _, err := os.Stat(projPath); err != nil {
		t.Fatalf("projection file not created at %s: %v", projPath, err)
	}

	// BuildState
	state, err := rec.BuildState(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if state.ResourceType != rec.Type() {
		t.Errorf("BuildState: wrong ResourceType %s", state.ResourceType)
	}
	// 3 nodes + 1 projection file = 4 resources
	if len(state.Resources) != 4 {
		t.Errorf("BuildState: expected 4 resources, got %d", len(state.Resources))
	}

	// Health
	h := rec.Health()
	if h.Sync != reconcile.SyncStatusSynced {
		t.Errorf("Health: expected Synced after apply, got %s", h.Sync)
	}
}

func TestProjectionReconciler_Idempotent(t *testing.T) {
	root, cleanup := makeTestWorkspace(t, map[string]string{
		"node-a.cog.md": sampleNode("node-a", "1", "none"),
	})
	defer cleanup()

	rec := NewProjectionReconciler(ProjectionBibliography)
	ctx := context.Background()

	cfgAny, _ := rec.LoadConfig(root)
	liveAny, _ := rec.FetchLive(ctx, cfgAny)
	plan, _ := rec.ComputePlan(cfgAny, liveAny, nil)

	// Apply once.
	results1, err := rec.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Apply again — should produce the same result (idempotent).
	results2, err := rec.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}

	if len(results1) != len(results2) {
		t.Errorf("idempotency: result count changed: %d vs %d", len(results1), len(results2))
	}

	// Second ComputePlan should show no changes (synced).
	liveAny2, _ := rec.FetchLive(ctx, cfgAny)
	plan2, _ := rec.ComputePlan(cfgAny, liveAny2, nil)
	if plan2.Summary.HasChanges() {
		t.Errorf("idempotency: ComputePlan still shows changes after two applies: %+v", plan2.Summary)
	}
}

func TestProjectionReconciler_AllSixProjections(t *testing.T) {
	root, cleanup := makeTestWorkspace(t, map[string]string{
		"antecedent-rovelli.cog.md": sampleNode("antecedent-rovelli", "1", "none"),
		"thread-g-ln2.cog.md":       sampleNode("thread-g-ln2", "2", "medium"),
		"thread-h-cft.cog.md":       sampleNode("thread-h-cft", "3", "high"),
	})
	defer cleanup()

	ctx := context.Background()

	for _, kind := range AllProjectionKinds {
		t.Run(string(kind), func(t *testing.T) {
			rec := NewProjectionReconciler(kind)
			cfgAny, err := rec.LoadConfig(root)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			liveAny, err := rec.FetchLive(ctx, cfgAny)
			if err != nil {
				t.Fatalf("FetchLive: %v", err)
			}
			plan, err := rec.ComputePlan(cfgAny, liveAny, nil)
			if err != nil {
				t.Fatalf("ComputePlan: %v", err)
			}
			_, err = rec.ApplyPlan(ctx, plan)
			if err != nil {
				t.Fatalf("ApplyPlan: %v", err)
			}

			// Verify the projection file was written.
			cfg := cfgAny.(*ProjectionConfig)
			projPath := filepath.Join(cfg.ProjectionDir, string(kind)+".md")
			data, err := os.ReadFile(projPath)
			if err != nil {
				t.Fatalf("projection file missing: %v", err)
			}
			if len(data) == 0 {
				t.Error("projection file is empty")
			}
			if !strings.Contains(string(data), "Generated by ProjectionReconciler") {
				t.Error("projection file missing generated-by header")
			}
		})
	}
}

func TestProjectionWatcher_TriggerOnWrite(t *testing.T) {
	root, cleanup := makeTestWorkspace(t, map[string]string{
		"initial.cog.md": sampleNode("initial", "1", "none"),
	})
	defer cleanup()

	nodesDir := filepath.Join(root, ".cog", "mem", "semantic", "lineage", "nodes")

	triggered := make(chan struct{}, 1)
	trigger := func(ctx context.Context) error {
		select {
		case triggered <- struct{}{}:
		default:
		}
		return nil
	}

	watcher := NewProjectionWatcher(nodesDir, trigger, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := watcher.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer watcher.Stop()

	// Write a new node file to trigger the watcher.
	newNodePath := filepath.Join(nodesDir, "new-node.cog.md")
	if err := os.WriteFile(newNodePath, []byte(sampleNode("new-node", "2", "low")), 0644); err != nil {
		t.Fatalf("write new node: %v", err)
	}

	// Wait for trigger (debounce: 500ms + fsnotify/polling latency; allow 3s).
	select {
	case <-triggered:
		// success
	case <-time.After(3 * time.Second):
		t.Error("watcher did not trigger within 3s of file write")
	}
}

func TestProjectionReconciler_Registration(t *testing.T) {
	// Reset registry state (this test verifies that RegisterProjectionProviders
	// registers exactly 6 providers with the correct names).
	// We cannot reset the global registry here without the test helper,
	// but we can verify the providers exist after init() runs.
	for _, kind := range AllProjectionKinds {
		providerName := "lineage-projection-" + string(kind)
		if !reconcile.HasProvider(providerName) {
			t.Errorf("provider %q not registered (init should have called RegisterProjectionProviders)", providerName)
		}
	}
}

func TestProjectionReconciler_Type(t *testing.T) {
	for _, kind := range AllProjectionKinds {
		rec := NewProjectionReconciler(kind)
		expected := "lineage-projection-" + string(kind)
		if rec.Type() != expected {
			t.Errorf("Type(): expected %s, got %s", expected, rec.Type())
		}
	}
}

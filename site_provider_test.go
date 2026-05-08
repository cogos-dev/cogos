// site_provider_test.go
// Covers: SiteProvider LoadConfig, FetchLive, ComputePlan, ApplyPlan, BuildState.
// Does NOT modify production code. Defects found are documented with t.Skip("FOLLOWUP: ...").

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── Mock DeployStrategy ────────────────────────────────────────────────────────

// mockDeployStrategy is a test-only implementation of DeployStrategy.
type mockDeployStrategy struct {
	FetchLiveCallCount int
	DeployCallCount    int
	FetchLiveReturn    LiveSiteState
	FetchLiveError     error
	DeployError        error
	DeployedArtifacts  []string // artifact dirs received by Deploy
}

func (m *mockDeployStrategy) FetchLive(_ context.Context, _ SiteCRD) (LiveSiteState, error) {
	m.FetchLiveCallCount++
	return m.FetchLiveReturn, m.FetchLiveError
}

func (m *mockDeployStrategy) Deploy(_ context.Context, _ SiteCRD, artifactDir string) error {
	m.DeployCallCount++
	m.DeployedArtifacts = append(m.DeployedArtifacts, artifactDir)
	return m.DeployError
}

// ─── Fixtures ───────────────────────────────────────────────────────────────────

// validSiteYAML returns a minimal valid site.yaml content.
func validSiteYAML(name, domain string) string {
	return fmt.Sprintf(`apiVersion: cogos.myrgic.io/v1alpha1
kind: Site
metadata:
  name: %s
spec:
  domain: %s
  deploy:
    strategy: gh-pages
    target:
      repo: myrgic/%s
`, name, domain, name)
}

// invalidSiteYAML returns a YAML that parses but fails Validate (missing domain).
func invalidSiteYAML(name string) string {
	return fmt.Sprintf(`apiVersion: cogos.myrgic.io/v1alpha1
kind: Site
metadata:
  name: %s
spec:
  deploy:
    strategy: gh-pages
    target:
      repo: myrgic/%s
`, name, name)
}

// buildAppsDir builds a <root>/apps/<name>/site.yaml directory tree.
// yamlContents maps app name → yaml content; empty content signals: skip creating.
func buildAppsDir(t *testing.T, root string, yamlContents map[string]string) {
	t.Helper()
	appsDir := filepath.Join(root, "apps")
	if err := os.MkdirAll(appsDir, 0755); err != nil {
		t.Fatalf("MkdirAll apps: %v", err)
	}
	for name, content := range yamlContents {
		appDir := filepath.Join(appsDir, name)
		if err := os.MkdirAll(appDir, 0755); err != nil {
			t.Fatalf("MkdirAll app %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(appDir, "site.yaml"), []byte(content), 0644); err != nil {
			t.Fatalf("write site.yaml for %s: %v", name, err)
		}
	}
}

// newProviderWithMock returns a SiteProvider with a mock strategy registered as "gh-pages".
func newProviderWithMock(mock *mockDeployStrategy) *SiteProvider {
	sp := &SiteProvider{strategies: map[string]DeployStrategy{
		"gh-pages": mock,
	}}
	return sp
}

// makeCRD produces a SiteCRD for testing.
func makeCRD(name, domain string) SiteCRD {
	return SiteCRD{
		APIVersion: "cogos.myrgic.io/v1alpha1",
		Kind:       "Site",
		Metadata:   SiteMetadata{Name: name},
		Spec: SiteSpec{
			Domain: domain,
			Deploy: DeploySpec{
				Strategy: "gh-pages",
				Target:   map[string]any{"repo": "myrgic/" + name},
			},
		},
	}
}

// ─── LoadConfig tests ───────────────────────────────────────────────────────────

func TestSiteProvider_LoadConfig_Valid5Apps(t *testing.T) {
	root := t.TempDir()
	names := []string{"app-a", "app-b", "app-c", "app-d", "app-e"}
	contents := make(map[string]string, len(names))
	for i, n := range names {
		contents[n] = validSiteYAML(n, fmt.Sprintf("app%d.example.com", i))
	}
	buildAppsDir(t, root, contents)

	sp := &SiteProvider{strategies: defaultStrategies()}
	result, err := sp.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: unexpected error: %v", err)
	}
	crds, ok := result.([]SiteCRD)
	if !ok {
		t.Fatalf("LoadConfig: unexpected return type %T", result)
	}
	if len(crds) != 5 {
		t.Errorf("LoadConfig: got %d CRDs, want 5", len(crds))
	}
}

func TestSiteProvider_LoadConfig_EmptyApps(t *testing.T) {
	root := t.TempDir()
	// Create the apps dir but leave it empty
	if err := os.MkdirAll(filepath.Join(root, "apps"), 0755); err != nil {
		t.Fatal(err)
	}

	sp := &SiteProvider{strategies: defaultStrategies()}
	result, err := sp.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig empty apps: unexpected error: %v", err)
	}
	crds, ok := result.([]SiteCRD)
	if !ok {
		t.Fatalf("LoadConfig: unexpected return type %T", result)
	}
	if len(crds) != 0 {
		t.Errorf("LoadConfig: got %d CRDs, want 0", len(crds))
	}
}

// TestSiteProvider_LoadConfig_InvalidYAML verifies that a YAML parse error (not a
// validation error) causes LoadConfig to return an error.
func TestSiteProvider_LoadConfig_InvalidYAML(t *testing.T) {
	root := t.TempDir()
	buildAppsDir(t, root, map[string]string{
		"broken": "not: valid: yaml: [[[",
	})

	sp := &SiteProvider{strategies: defaultStrategies()}
	_, err := sp.LoadConfig(root)
	if err == nil {
		t.Fatal("LoadConfig: want error for unparseable YAML, got nil")
	}
}

// TestSiteProvider_LoadConfig_InvalidValidation verifies behavior when a CRD
// parses but fails validation (missing domain). siteLoadCRDs logs a warning but
// does NOT fail-fast on validation errors — it returns all CRDs including invalid ones.
// This is intentional: the planner surfaces the warning; the apply won't deploy them.
func TestSiteProvider_LoadConfig_InvalidValidation(t *testing.T) {
	root := t.TempDir()
	buildAppsDir(t, root, map[string]string{
		"no-domain": invalidSiteYAML("no-domain"),
	})

	sp := &SiteProvider{strategies: defaultStrategies()}
	result, err := sp.LoadConfig(root)
	// siteLoadCRDs logs but does not return a validation error
	if err != nil {
		t.Fatalf("LoadConfig: unexpected error (validation warnings should not fail LoadConfig): %v", err)
	}
	crds, ok := result.([]SiteCRD)
	if !ok {
		t.Fatalf("LoadConfig: unexpected return type %T", result)
	}
	// The CRD is still returned (invalid CRDs are logged, not dropped)
	if len(crds) != 1 {
		t.Errorf("LoadConfig: got %d CRDs, want 1 (invalid CRDs returned with warning)", len(crds))
	}
}

// TestSiteProvider_LoadConfig_MixedValidAndInvalid verifies that a mix of valid and
// validation-failing CRDs all get returned. Parse failures still hard-fail.
func TestSiteProvider_LoadConfig_MixedValidAndInvalid(t *testing.T) {
	root := t.TempDir()
	buildAppsDir(t, root, map[string]string{
		"good-app":  validSiteYAML("good-app", "good.example.com"),
		"bad-app":   invalidSiteYAML("bad-app"),
	})

	sp := &SiteProvider{strategies: defaultStrategies()}
	result, err := sp.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: unexpected error: %v", err)
	}
	crds, ok := result.([]SiteCRD)
	if !ok {
		t.Fatalf("unexpected return type %T", result)
	}
	if len(crds) != 2 {
		t.Errorf("LoadConfig: got %d CRDs, want 2 (both valid and invalid returned)", len(crds))
	}
}

// ─── FetchLive tests ─────────────────────────────────────────────────────────────

func TestSiteProvider_FetchLive_MockCalledPerApp(t *testing.T) {
	mock := &mockDeployStrategy{
		FetchLiveReturn: LiveSiteState{
			ArtifactSHA:        "abc123",
			CNAMEContent:       "example.com",
			ResolvedFromTarget: true,
		},
	}
	sp := newProviderWithMock(mock)

	crds := []SiteCRD{
		makeCRD("app-a", "a.example.com"),
		makeCRD("app-b", "b.example.com"),
	}

	result, err := sp.FetchLive(context.Background(), crds)
	if err != nil {
		t.Fatalf("FetchLive: unexpected error: %v", err)
	}
	liveMap, ok := result.(map[string]LiveSiteState)
	if !ok {
		t.Fatalf("FetchLive: unexpected return type %T", result)
	}
	if mock.FetchLiveCallCount != 2 {
		t.Errorf("FetchLive call count: got %d, want 2", mock.FetchLiveCallCount)
	}
	if len(liveMap) != 2 {
		t.Errorf("FetchLive: map has %d entries, want 2", len(liveMap))
	}
	for _, name := range []string{"app-a", "app-b"} {
		state, ok := liveMap[name]
		if !ok {
			t.Errorf("FetchLive: missing entry for %q", name)
			continue
		}
		if state.ArtifactSHA != "abc123" {
			t.Errorf("FetchLive[%s].ArtifactSHA: got %q, want abc123", name, state.ArtifactSHA)
		}
	}
}

func TestSiteProvider_FetchLive_StrategyLookupError(t *testing.T) {
	// Provider has no strategies registered
	sp := &SiteProvider{strategies: map[string]DeployStrategy{}}

	crds := []SiteCRD{
		makeCRD("app-a", "a.example.com"),
	}

	// FetchLive logs the error and returns an empty LiveSiteState for the app;
	// it does NOT return a top-level error.
	result, err := sp.FetchLive(context.Background(), crds)
	if err != nil {
		t.Fatalf("FetchLive: expected no top-level error for unknown strategy, got: %v", err)
	}
	liveMap, ok := result.(map[string]LiveSiteState)
	if !ok {
		t.Fatalf("FetchLive: unexpected return type %T", result)
	}
	state, ok := liveMap["app-a"]
	if !ok {
		t.Fatal("FetchLive: missing entry for app-a")
	}
	if state.ResolvedFromTarget {
		t.Error("FetchLive: ResolvedFromTarget should be false for unknown strategy")
	}
}

func TestSiteProvider_FetchLive_StrategyError(t *testing.T) {
	mock := &mockDeployStrategy{
		FetchLiveError: fmt.Errorf("network timeout"),
	}
	sp := newProviderWithMock(mock)
	crds := []SiteCRD{makeCRD("app-a", "a.example.com")}

	result, err := sp.FetchLive(context.Background(), crds)
	if err != nil {
		t.Fatalf("FetchLive: expected no top-level error for per-app strategy error, got: %v", err)
	}
	liveMap := result.(map[string]LiveSiteState)
	state := liveMap["app-a"]
	if state.ResolvedFromTarget {
		t.Error("FetchLive: ResolvedFromTarget should be false when strategy errors")
	}
}

// ─── ComputePlan tests ───────────────────────────────────────────────────────────

func TestSiteProvider_ComputePlan_AllSynced(t *testing.T) {
	mock := &mockDeployStrategy{}
	sp := newProviderWithMock(mock)
	sp.root = "/fake/root"

	crds := []SiteCRD{makeCRD("app-a", "a.example.com")}
	liveMap := map[string]LiveSiteState{
		"app-a": {ArtifactSHA: "deadbeef", ResolvedFromTarget: true},
	}

	plan, err := sp.ComputePlan(crds, liveMap, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("ComputePlan: got %d actions, want 1", len(plan.Actions))
	}
	if plan.Actions[0].Action != ActionSkip {
		t.Errorf("ComputePlan: got action %q, want Skip", plan.Actions[0].Action)
	}
	if plan.Summary.Skipped != 1 {
		t.Errorf("ComputePlan: Summary.Skipped = %d, want 1", plan.Summary.Skipped)
	}
}

func TestSiteProvider_ComputePlan_EmptyArtifactSHA_IsUpdate(t *testing.T) {
	mock := &mockDeployStrategy{}
	sp := newProviderWithMock(mock)
	sp.root = "/fake/root"

	crds := []SiteCRD{makeCRD("app-a", "a.example.com")}
	liveMap := map[string]LiveSiteState{
		"app-a": {ArtifactSHA: "", ResolvedFromTarget: true}, // deployed but SHA unknown
	}

	plan, err := sp.ComputePlan(crds, liveMap, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("ComputePlan: got %d actions, want 1", len(plan.Actions))
	}
	if plan.Actions[0].Action != ActionUpdate {
		t.Errorf("ComputePlan: got action %q, want Update", plan.Actions[0].Action)
	}
	if plan.Summary.Updates != 1 {
		t.Errorf("ComputePlan: Summary.Updates = %d, want 1", plan.Summary.Updates)
	}
}

func TestSiteProvider_ComputePlan_NotDeployed_IsCreate(t *testing.T) {
	mock := &mockDeployStrategy{}
	sp := newProviderWithMock(mock)
	sp.root = "/fake/root"

	crds := []SiteCRD{makeCRD("app-a", "a.example.com")}
	liveMap := map[string]LiveSiteState{
		"app-a": {ResolvedFromTarget: false}, // not deployed yet
	}

	plan, err := sp.ComputePlan(crds, liveMap, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("ComputePlan: got %d actions, want 1", len(plan.Actions))
	}
	if plan.Actions[0].Action != ActionCreate {
		t.Errorf("ComputePlan: got action %q, want Create", plan.Actions[0].Action)
	}
	if plan.Summary.Creates != 1 {
		t.Errorf("ComputePlan: Summary.Creates = %d, want 1", plan.Summary.Creates)
	}
}

func TestSiteProvider_ComputePlan_MissingFromLive_IsCreate(t *testing.T) {
	mock := &mockDeployStrategy{}
	sp := newProviderWithMock(mock)
	sp.root = "/fake/root"

	crds := []SiteCRD{makeCRD("app-a", "a.example.com")}
	// app-a not present in liveMap at all → zero-value LiveSiteState, ResolvedFromTarget=false
	liveMap := map[string]LiveSiteState{}

	plan, err := sp.ComputePlan(crds, liveMap, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Actions[0].Action != ActionCreate {
		t.Errorf("ComputePlan: got action %q, want Create for missing live entry", plan.Actions[0].Action)
	}
}

func TestSiteProvider_ComputePlan_OrphanedState_IsDelete(t *testing.T) {
	mock := &mockDeployStrategy{}
	sp := newProviderWithMock(mock)
	sp.root = "/fake/root"

	// No CRDs — app-a was removed from config
	crds := []SiteCRD{}
	liveMap := map[string]LiveSiteState{}

	// State still has app-a
	state := &ReconcileState{
		Resources: []ReconcileResource{
			{Name: "app-a", Type: "site"},
		},
	}

	plan, err := sp.ComputePlan(crds, liveMap, state)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("ComputePlan: got %d actions, want 1", len(plan.Actions))
	}
	if plan.Actions[0].Action != ActionDelete {
		t.Errorf("ComputePlan: got action %q, want Delete", plan.Actions[0].Action)
	}
	if plan.Summary.Deletes != 1 {
		t.Errorf("ComputePlan: Summary.Deletes = %d, want 1", plan.Summary.Deletes)
	}
}

func TestSiteProvider_ComputePlan_SummaryCounts(t *testing.T) {
	mock := &mockDeployStrategy{}
	sp := newProviderWithMock(mock)
	sp.root = "/fake/root"

	crds := []SiteCRD{
		makeCRD("app-skip", "skip.example.com"),
		makeCRD("app-create", "create.example.com"),
		makeCRD("app-update", "update.example.com"),
	}
	liveMap := map[string]LiveSiteState{
		"app-skip":   {ArtifactSHA: "sha1", ResolvedFromTarget: true},
		"app-create": {ResolvedFromTarget: false},
		"app-update": {ArtifactSHA: "", ResolvedFromTarget: true},
	}

	plan, err := sp.ComputePlan(crds, liveMap, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Skipped != 1 {
		t.Errorf("Summary.Skipped = %d, want 1", plan.Summary.Skipped)
	}
	if plan.Summary.Creates != 1 {
		t.Errorf("Summary.Creates = %d, want 1", plan.Summary.Creates)
	}
	if plan.Summary.Updates != 1 {
		t.Errorf("Summary.Updates = %d, want 1", plan.Summary.Updates)
	}
}

// ─── ApplyPlan tests ─────────────────────────────────────────────────────────────

// buildFakeAppDir creates a minimal app directory with a dist/ subdir for ApplyPlan.
// The build step (siteBuild) runs build.sh; we skip by using action.Details["app_dir"]
// pointing to a dir with a real build.sh stub. Actually ApplyPlan calls siteBuild
// which runs bash build.sh — we need to provide a real build.sh or mock the build.
// Since we cannot mock siteBuild easily, we create a real build.sh that just succeeds.
func buildFakeAppDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	distDir := filepath.Join(dir, "dist")
	if err := os.MkdirAll(distDir, 0755); err != nil {
		t.Fatalf("MkdirAll dist: %v", err)
	}
	// Write a trivial build.sh that creates/touches the dist dir
	buildSH := "#!/bin/bash\nmkdir -p dist\n"
	buildPath := filepath.Join(dir, "build.sh")
	if err := os.WriteFile(buildPath, []byte(buildSH), 0755); err != nil {
		t.Fatalf("write build.sh: %v", err)
	}
	// Write a placeholder file in dist so Deploy has something to work with
	if err := os.WriteFile(filepath.Join(distDir, "index.html"), []byte("<html/>"), 0644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	return dir
}

func TestSiteProvider_ApplyPlan_DeployCalledForCreateAndUpdate(t *testing.T) {
	mock := &mockDeployStrategy{}
	sp := newProviderWithMock(mock)
	sp.root = t.TempDir()

	// Build real app dirs (ApplyPlan calls siteBuild which runs bash build.sh)
	appCreateDir := buildFakeAppDir(t)
	appUpdateDir := buildFakeAppDir(t)

	// Write minimal site.yaml into the provider root so siteCRDByName works after reload
	buildAppsDir(t, sp.root, map[string]string{
		"app-create": validSiteYAML("app-create", "create.example.com"),
		"app-update": validSiteYAML("app-update", "update.example.com"),
	})

	plan := &ReconcilePlan{
		ResourceType: "site",
		Actions: []ReconcileAction{
			{
				Action:       ActionCreate,
				ResourceType: "site",
				Name:         "app-create",
				Details: map[string]any{
					"app_dir":  appCreateDir,
					"strategy": "gh-pages",
					"domain":   "create.example.com",
					"target":   map[string]any{"repo": "myrgic/app-create"},
				},
			},
			{
				Action:       ActionUpdate,
				ResourceType: "site",
				Name:         "app-update",
				Details: map[string]any{
					"app_dir":  appUpdateDir,
					"strategy": "gh-pages",
					"domain":   "update.example.com",
					"target":   map[string]any{"repo": "myrgic/app-update"},
				},
			},
		},
	}

	results, err := sp.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if mock.DeployCallCount != 2 {
		t.Errorf("Deploy call count: got %d, want 2", mock.DeployCallCount)
	}
	if len(results) != 2 {
		t.Errorf("ApplyPlan: got %d results, want 2", len(results))
	}
	for _, r := range results {
		if r.Status != ApplySucceeded {
			t.Errorf("ApplyPlan result %s: status %v, want ApplySucceeded; err: %v", r.Name, r.Status, r.Error)
		}
	}
}

func TestSiteProvider_ApplyPlan_SkipNotDeployed(t *testing.T) {
	mock := &mockDeployStrategy{}
	sp := newProviderWithMock(mock)
	sp.root = t.TempDir()

	plan := &ReconcilePlan{
		ResourceType: "site",
		Actions: []ReconcileAction{
			{
				Action:       ActionSkip,
				ResourceType: "site",
				Name:         "app-skip",
				Details:      map[string]any{"app_dir": "/some/dir"},
			},
		},
	}

	results, err := sp.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if mock.DeployCallCount != 0 {
		t.Errorf("Deploy call count: got %d, want 0 (skip actions should not deploy)", mock.DeployCallCount)
	}
	// Skip actions produce no result entries
	if len(results) != 0 {
		t.Errorf("ApplyPlan: got %d results for skip-only plan, want 0", len(results))
	}
}

func TestSiteProvider_ApplyPlan_PerActionErrorDoesNotAbort(t *testing.T) {
	mock := &mockDeployStrategy{
		DeployError: fmt.Errorf("network failure"),
	}
	sp := newProviderWithMock(mock)
	sp.root = t.TempDir()

	appDir1 := buildFakeAppDir(t)
	appDir2 := buildFakeAppDir(t)

	buildAppsDir(t, sp.root, map[string]string{
		"app-1": validSiteYAML("app-1", "1.example.com"),
		"app-2": validSiteYAML("app-2", "2.example.com"),
	})

	plan := &ReconcilePlan{
		ResourceType: "site",
		Actions: []ReconcileAction{
			{
				Action: ActionCreate, Name: "app-1",
				Details: map[string]any{
					"app_dir": appDir1, "strategy": "gh-pages",
					"domain": "1.example.com",
					"target": map[string]any{"repo": "myrgic/app-1"},
				},
			},
			{
				Action: ActionCreate, Name: "app-2",
				Details: map[string]any{
					"app_dir": appDir2, "strategy": "gh-pages",
					"domain": "2.example.com",
					"target": map[string]any{"repo": "myrgic/app-2"},
				},
			},
		},
	}

	results, err := sp.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: unexpected top-level error: %v", err)
	}
	// Both results should be present — apply loop does not abort on per-action failure
	if len(results) != 2 {
		t.Errorf("ApplyPlan: got %d results, want 2 (per-action error should not abort)", len(results))
	}
	for _, r := range results {
		if r.Status != ApplyFailed {
			t.Errorf("result %s: expected ApplyFailed, got %v", r.Name, r.Status)
		}
		if r.Error == "" {
			t.Errorf("result %s: expected non-empty error", r.Name)
		}
	}
}

// ─── BuildState tests ────────────────────────────────────────────────────────────

func TestSiteProvider_BuildState_PreservesLineage(t *testing.T) {
	sp := &SiteProvider{strategies: defaultStrategies()}

	liveMap := map[string]LiveSiteState{
		"app-a": {ArtifactSHA: "sha123", CNAMEContent: "a.example.com", ResolvedFromTarget: true},
	}

	existing := &ReconcileState{
		Version:      1,
		ResourceType: "site",
		Lineage:      "site-existing-lineage",
		Serial:       5,
	}

	state, err := sp.BuildState(nil, liveMap, existing)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if state.Lineage != "site-existing-lineage" {
		t.Errorf("BuildState: lineage = %q, want %q", state.Lineage, "site-existing-lineage")
	}
	if state.Serial != 6 {
		t.Errorf("BuildState: serial = %d, want 6", state.Serial)
	}
	if len(state.Resources) != 1 {
		t.Fatalf("BuildState: got %d resources, want 1", len(state.Resources))
	}
	res := state.Resources[0]
	if res.Name != "app-a" {
		t.Errorf("BuildState: resource name = %q, want app-a", res.Name)
	}
	if res.ExternalID != "sha123" {
		t.Errorf("BuildState: ExternalID = %q, want sha123", res.ExternalID)
	}
}

func TestSiteProvider_BuildState_NilExistingCreatesNewLineage(t *testing.T) {
	sp := &SiteProvider{strategies: defaultStrategies()}
	liveMap := map[string]LiveSiteState{}

	state, err := sp.BuildState(nil, liveMap, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if state.Lineage == "" {
		t.Error("BuildState: lineage should not be empty for new state")
	}
	if !strings.HasPrefix(state.Lineage, "site-") {
		t.Errorf("BuildState: lineage %q should start with 'site-'", state.Lineage)
	}
	if state.Serial != 0 {
		t.Errorf("BuildState: serial = %d, want 0 for new state", state.Serial)
	}
}

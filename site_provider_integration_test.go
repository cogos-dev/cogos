// site_provider_integration_test.go
// Integration tests for SiteProvider against the real ~/workspaces/myrgic/sites/ monorepo.
//
// All tests skip gracefully when the monorepo is absent (CI without monorepo, etc.).
// Each test uses a mockDeployStrategy from site_provider_test.go (same package).
//
// Run with:
//   go test -run TestSiteProviderIntegration -v ./...

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// skipIfNoMonorepo checks for the monorepo and returns its root path.
// It calls t.Skip if the monorepo is absent so CI without the monorepo passes cleanly.
func skipIfNoMonorepo(t *testing.T) string {
	t.Helper()
	sitesPath := os.ExpandEnv("$HOME/workspaces/myrgic/sites")
	if _, err := os.Stat(filepath.Join(sitesPath, "apps")); err != nil {
		t.Skipf("integration test requires monorepo at %s (got %v)", sitesPath, err)
	}
	return sitesPath
}

// expectedSiteNames are the 5 apps currently declared in the monorepo.
var expectedSiteNames = []string{
	"myrgic-com",
	"myrgic-dev",
	"myrgic-ai",
	"myrgic-net",
	"myrgic-org",
}

// TestSiteProviderIntegration_LoadConfig loads the real monorepo and verifies
// that all 5 site CRDs are present and structurally correct.
func TestSiteProviderIntegration_LoadConfig(t *testing.T) {
	sitesPath := skipIfNoMonorepo(t)

	sp := &SiteProvider{strategies: defaultStrategies()}
	result, err := sp.LoadConfig(sitesPath)
	if err != nil {
		t.Fatalf("LoadConfig(%s): %v", sitesPath, err)
	}

	crds, ok := result.([]SiteCRD)
	if !ok {
		t.Fatalf("LoadConfig: unexpected return type %T", result)
	}

	// Must return exactly 5 CRDs.
	if len(crds) != 5 {
		t.Errorf("LoadConfig: got %d CRDs, want 5; names: %v", len(crds), crdNames(crds))
	}

	// Build a name→CRD index for assertions below.
	byName := make(map[string]SiteCRD, len(crds))
	for _, crd := range crds {
		byName[crd.Metadata.Name] = crd
	}

	// All 5 expected names must be present.
	for _, name := range expectedSiteNames {
		if _, ok := byName[name]; !ok {
			t.Errorf("LoadConfig: expected CRD %q not found; got names: %v", name, crdNames(crds))
		}
	}

	// Exactly 1 canonical site.
	canonicalCount := 0
	for _, crd := range crds {
		if crd.Spec.Canonical {
			canonicalCount++
			if crd.Metadata.Name != "myrgic-com" {
				t.Errorf("LoadConfig: canonical site is %q, want myrgic-com", crd.Metadata.Name)
			}
		}
	}
	if canonicalCount != 1 {
		t.Errorf("LoadConfig: got %d canonical sites, want exactly 1", canonicalCount)
	}

	// All sites must use the gh-pages strategy.
	for _, crd := range crds {
		if crd.Spec.Deploy.Strategy != "gh-pages" {
			t.Errorf("LoadConfig: %s has strategy %q, want gh-pages", crd.Metadata.Name, crd.Spec.Deploy.Strategy)
		}
	}

	// All sites must have a non-empty deploy target repo.
	for _, crd := range crds {
		repo, _ := crd.Spec.Deploy.Target["repo"].(string)
		if repo == "" {
			t.Errorf("LoadConfig: %s has empty deploy.target.repo", crd.Metadata.Name)
		}
	}

	// The 4 redirect sites must have redirect_to == "https://myrgic.com/".
	redirectSites := []string{"myrgic-dev", "myrgic-ai", "myrgic-net", "myrgic-org"}
	for _, name := range redirectSites {
		crd, ok := byName[name]
		if !ok {
			continue // already reported above
		}
		if crd.Spec.RedirectTo == nil {
			t.Errorf("LoadConfig: %s has nil redirect_to, want \"https://myrgic.com/\"", name)
		} else if *crd.Spec.RedirectTo != "https://myrgic.com/" {
			t.Errorf("LoadConfig: %s redirect_to = %q, want \"https://myrgic.com/\"", name, *crd.Spec.RedirectTo)
		}
	}
}

// TestSiteProviderIntegration_PlanShape exercises LoadConfig → FetchLive → ComputePlan
// against the real monorepo, using a mock strategy that simulates fresh (undeployed) targets.
// All 5 sites should appear as Creates when no live state is resolved.
func TestSiteProviderIntegration_PlanShape(t *testing.T) {
	sitesPath := skipIfNoMonorepo(t)

	// mockDeployStrategy returns ResolvedFromTarget=false for all (fresh deploy targets).
	mock := &mockDeployStrategy{
		FetchLiveReturn: LiveSiteState{ResolvedFromTarget: false},
	}
	sp := newProviderWithMock(mock)

	// Load real config.
	configAny, err := sp.LoadConfig(sitesPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	crds, ok := configAny.([]SiteCRD)
	if !ok {
		t.Fatalf("LoadConfig: unexpected type %T", configAny)
	}
	if len(crds) == 0 {
		t.Skip("integration test: no CRDs loaded, skipping plan shape test")
	}

	// FetchLive with mock (no real GitHub calls).
	liveAny, err := sp.FetchLive(context.Background(), configAny)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}

	// ComputePlan.
	plan, err := sp.ComputePlan(configAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	// All 5 sites are undeployed → 5 Creates.
	if plan.Summary.Creates != 5 {
		t.Errorf("ComputePlan: Summary.Creates = %d, want 5", plan.Summary.Creates)
	}

	// Plan has changes.
	if !plan.Summary.HasChanges() {
		t.Error("ComputePlan: HasChanges() should be true for 5 creates")
	}

	// Each action must target one of the 5 expected site names.
	expectedSet := make(map[string]bool, len(expectedSiteNames))
	for _, n := range expectedSiteNames {
		expectedSet[n] = true
	}

	actionNames := make(map[string]bool, len(plan.Actions))
	for _, action := range plan.Actions {
		if action.Action != ActionCreate {
			t.Errorf("ComputePlan: unexpected action %q for %s (want Create)", action.Action, action.Name)
		}
		if !expectedSet[action.Name] {
			t.Errorf("ComputePlan: action targets unexpected site %q", action.Name)
		}
		actionNames[action.Name] = true
	}

	// Every expected site must have an action.
	for _, name := range expectedSiteNames {
		if !actionNames[name] {
			t.Errorf("ComputePlan: no action for expected site %q", name)
		}
	}
}

// TestSiteProviderIntegration_AppDirsExecutable verifies that each app's build.sh
// has the executable bit set. Catches chmod drift that would silently break builds.
func TestSiteProviderIntegration_AppDirsExecutable(t *testing.T) {
	sitesPath := skipIfNoMonorepo(t)

	appsDir := filepath.Join(sitesPath, "apps")
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", appsDir, err)
	}

	if len(entries) == 0 {
		t.Skip("integration test: no apps found in monorepo")
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		buildSH := filepath.Join(appsDir, name, "build.sh")
		info, err := os.Stat(buildSH)
		if err != nil {
			// FOLLOWUP: some apps may not have build.sh yet (e.g. static redirects).
			// For now, skip missing build.sh rather than fail — flag for wave 5 review.
			t.Logf("FOLLOWUP: %s/build.sh not found (%v); verify build.sh is required for all site types", name, err)
			continue
		}
		if info.Mode()&0111 == 0 {
			t.Errorf("%s/build.sh is not executable (mode %v)", name, info.Mode())
		}
	}
}

// crdNames extracts metadata names from a slice of SiteCRDs for error messages.
func crdNames(crds []SiteCRD) []string {
	names := make([]string, len(crds))
	for i, c := range crds {
		names[i] = c.Metadata.Name
	}
	return names
}

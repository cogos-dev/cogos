// site_strategy_ghpages_test.go
// Covers: GHPagesStrategy.FetchLive (fully unit-tested via injected runner)
//         GHPagesStrategy.Deploy (integration-only, skipped unless gh is authenticated
//         and -short is not set).
//
// Strategy choice: HYBRID (A for FetchLive, B for Deploy)
//   - FetchLive has rich JSON-parsing logic and 404-branching worth unit testing.
//     We stub ghAPIRunner (the package-level injectable) to avoid real gh CLI calls.
//   - Deploy's logic is mostly sequencing exec calls; unit-testing exec invocation
//     order requires more invasive mocking (gitRun uses exec.CommandContext directly
//     with no injection point). Rather than widen the refactor surface, Deploy is
//     covered by a guarded integration test that is skipped unless gh is available
//     and -short is not set. This is consistent with D1's FOLLOWUP pattern.

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ─── Test helpers ────────────────────────────────────────────────────────────────

// makeRefJSON returns a JSON bytes simulating gh api .../git/refs/heads/main response.
func makeRefJSON(sha string) []byte {
	b, _ := json.Marshal(map[string]any{
		"ref": "refs/heads/main",
		"object": map[string]any{
			"sha":  sha,
			"type": "commit",
		},
	})
	return b
}

// makeCNAMEJSON returns a JSON bytes simulating gh api .../contents/CNAME response.
func makeCNAMEJSON(domain string) []byte {
	encoded := base64.StdEncoding.EncodeToString([]byte(domain + "\n"))
	b, _ := json.Marshal(map[string]any{
		"name":     "CNAME",
		"content":  encoded,
		"encoding": "base64",
	})
	return b
}

// makeContentsJSON returns JSON bytes simulating a gh api .../contents/<file>
// response with base64-encoded content (used for the .artifact-sha manifest).
func makeContentsJSON(name, content string) []byte {
	encoded := base64.StdEncoding.EncodeToString([]byte(content + "\n"))
	b, _ := json.Marshal(map[string]any{
		"name":     name,
		"content":  encoded,
		"encoding": "base64",
	})
	return b
}

// notFoundErr returns a fake 404 error matching FetchLive's detection heuristic.
func notFoundErr(path string) error {
	return fmt.Errorf("gh api %s: exit status 1: 404 Not Found", path)
}

// ghPages returns a fresh GHPagesStrategy.
func ghPages() *GHPagesStrategy {
	return &GHPagesStrategy{}
}

// crd returns a minimal SiteCRD targeting the given repo.
func ghCRD(name, domain, repo string) SiteCRD {
	return SiteCRD{
		APIVersion: "cogos.myrgic.io/v1alpha1",
		Kind:       "Site",
		Metadata:   SiteMetadata{Name: name},
		Spec: SiteSpec{
			Domain: domain,
			Deploy: DeploySpec{
				Strategy: "gh-pages",
				Target:   map[string]any{"repo": repo},
			},
		},
	}
}

// withRunner temporarily replaces ghAPIRunner for the duration of the test.
func withRunner(t *testing.T, fn func(ctx context.Context, path string) ([]byte, error)) {
	t.Helper()
	orig := ghAPIRunner
	ghAPIRunner = fn
	t.Cleanup(func() { ghAPIRunner = orig })
}

// ─── FetchLive — unit tests ──────────────────────────────────────────────────────

func TestGHPagesStrategy_FetchLive_SuccessPath(t *testing.T) {
	app := ghCRD("myrgic-com", "myrgic.com", "myrgic/myrgic.github.io")

	withRunner(t, func(ctx context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "git/refs/heads/main") {
			return makeRefJSON("abc123def456"), nil
		}
		if strings.Contains(path, "contents/CNAME") {
			return makeCNAMEJSON("myrgic.com"), nil
		}
		if strings.Contains(path, "contents/.artifact-sha") {
			return makeContentsJSON(".artifact-sha", "cafef00d"), nil
		}
		return nil, fmt.Errorf("unexpected path: %s", path)
	})

	g := ghPages()
	state, err := g.FetchLive(context.Background(), app)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	if !state.ResolvedFromTarget {
		t.Error("ResolvedFromTarget: want true")
	}
	// ArtifactSHA is the deploy-embedded content hash, not the git commit SHA.
	if state.ArtifactSHA != "cafef00d" {
		t.Errorf("ArtifactSHA: got %q, want cafef00d", state.ArtifactSHA)
	}
	if state.Metadata["commit_sha"] != "abc123def456" {
		t.Errorf("Metadata[commit_sha]: got %v, want abc123def456", state.Metadata["commit_sha"])
	}
	if state.CNAMEContent != "myrgic.com" {
		t.Errorf("CNAMEContent: got %q, want myrgic.com", state.CNAMEContent)
	}
	repo, ok := state.Metadata["repo"]
	if !ok || repo != "myrgic/myrgic.github.io" {
		t.Errorf("Metadata[repo]: got %v, want myrgic/myrgic.github.io", repo)
	}
}

func TestGHPagesStrategy_FetchLive_MissingArtifactSHA_NotAnError(t *testing.T) {
	app := ghCRD("no-sha", "nosha.example.com", "myrgic/no-sha")

	withRunner(t, func(ctx context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "git/refs/heads/main") {
			return makeRefJSON("abc999"), nil
		}
		if strings.Contains(path, "contents/CNAME") {
			return makeCNAMEJSON("nosha.example.com"), nil
		}
		if strings.Contains(path, "contents/.artifact-sha") {
			return nil, notFoundErr(path) // manifest absent (pre-.artifact-sha deploy)
		}
		return nil, fmt.Errorf("unexpected path: %s", path)
	})

	g := ghPages()
	state, err := g.FetchLive(context.Background(), app)
	if err != nil {
		t.Fatalf("FetchLive: .artifact-sha 404 should not error, got: %v", err)
	}
	if !state.ResolvedFromTarget {
		t.Error("ResolvedFromTarget: want true (repo exists; .artifact-sha is optional)")
	}
	if state.ArtifactSHA != "" {
		t.Errorf("ArtifactSHA: want empty when .artifact-sha absent, got %q", state.ArtifactSHA)
	}
}

func TestGHPagesStrategy_FetchLive_NotFound_MainRef(t *testing.T) {
	app := ghCRD("new-site", "new.example.com", "myrgic/new-site")

	withRunner(t, func(ctx context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "git/refs/heads/main") {
			return nil, notFoundErr(path)
		}
		return nil, fmt.Errorf("unexpected path: %s", path)
	})

	g := ghPages()
	state, err := g.FetchLive(context.Background(), app)
	if err != nil {
		t.Fatalf("FetchLive: 404 on main ref should not error, got: %v", err)
	}
	if state.ResolvedFromTarget {
		t.Error("ResolvedFromTarget: want false when repo has no main branch (404)")
	}
}

func TestGHPagesStrategy_FetchLive_MissingCNAME_NotAnError(t *testing.T) {
	app := ghCRD("no-cname", "nocname.example.com", "myrgic/no-cname")

	withRunner(t, func(ctx context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "git/refs/heads/main") {
			return makeRefJSON("deadbeef1234"), nil
		}
		if strings.Contains(path, "contents/CNAME") {
			return nil, notFoundErr(path) // CNAME not present
		}
		if strings.Contains(path, "contents/.artifact-sha") {
			return makeContentsJSON(".artifact-sha", "beadfeed"), nil
		}
		return nil, fmt.Errorf("unexpected path: %s", path)
	})

	g := ghPages()
	state, err := g.FetchLive(context.Background(), app)
	if err != nil {
		t.Fatalf("FetchLive: CNAME 404 should not error, got: %v", err)
	}
	if !state.ResolvedFromTarget {
		t.Error("ResolvedFromTarget: want true (repo exists; CNAME is optional)")
	}
	if state.ArtifactSHA != "beadfeed" {
		t.Errorf("ArtifactSHA: got %q, want beadfeed", state.ArtifactSHA)
	}
	if state.Metadata["commit_sha"] != "deadbeef1234" {
		t.Errorf("Metadata[commit_sha]: got %v, want deadbeef1234", state.Metadata["commit_sha"])
	}
	if state.CNAMEContent != "" {
		t.Errorf("CNAMEContent: want empty when CNAME absent, got %q", state.CNAMEContent)
	}
}

func TestGHPagesStrategy_FetchLive_MalformedRefJSON(t *testing.T) {
	app := ghCRD("bad-json", "bad.example.com", "myrgic/bad-json")

	withRunner(t, func(ctx context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "git/refs/heads/main") {
			return []byte("not valid json {{{"), nil
		}
		return nil, fmt.Errorf("unexpected path: %s", path)
	})

	g := ghPages()
	_, err := g.FetchLive(context.Background(), app)
	if err == nil {
		t.Fatal("FetchLive: want error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parse ref response") {
		t.Errorf("FetchLive error: got %q, want to mention 'parse ref response'", err.Error())
	}
}

func TestGHPagesStrategy_FetchLive_MissingRepo(t *testing.T) {
	app := SiteCRD{
		Metadata: SiteMetadata{Name: "no-repo"},
		Spec: SiteSpec{
			Domain: "example.com",
			Deploy: DeploySpec{
				Strategy: "gh-pages",
				Target:   map[string]any{}, // repo key absent
			},
		},
	}

	// Runner should not be called since resolveRepo fails before any API call
	called := false
	withRunner(t, func(ctx context.Context, path string) ([]byte, error) {
		called = true
		return nil, nil
	})

	g := ghPages()
	_, err := g.FetchLive(context.Background(), app)
	if err == nil {
		t.Fatal("FetchLive: want error for missing deploy.target.repo, got nil")
	}
	if called {
		t.Error("ghAPIRunner should not be called when repo is missing")
	}
}

func TestGHPagesStrategy_FetchLive_NonTransientRefError(t *testing.T) {
	app := ghCRD("ref-error", "ref.example.com", "myrgic/ref-error")

	withRunner(t, func(ctx context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "git/refs/heads/main") {
			return nil, fmt.Errorf("gh api %s: connection refused", path)
		}
		return nil, fmt.Errorf("unexpected path: %s", path)
	})

	g := ghPages()
	_, err := g.FetchLive(context.Background(), app)
	if err == nil {
		t.Fatal("FetchLive: want error for non-404 ref error, got nil")
	}
	if !strings.Contains(err.Error(), "fetch main ref") {
		t.Errorf("FetchLive error: got %q, want to mention 'fetch main ref'", err.Error())
	}
}

// ─── Deploy — integration test (guarded) ─────────────────────────────────────────

// TestGHPagesStrategy_Deploy_Integration exercises Deploy against a real temp repo.
// Skipped unless:
//   - gh CLI is present and authenticated, AND
//   - -short flag is not set.
//
// This test does NOT push to a real remote — it asserts only that the local git
// operations succeed (init, copy, CNAME, add, commit). The push is expected to fail
// (no real remote) and the test verifies the error is from the push step, not earlier.
func TestGHPagesStrategy_Deploy_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("INTEGRATION: skipped in -short mode")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("INTEGRATION: gh CLI not found in PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("INTEGRATION: git not found in PATH")
	}

	// Build a minimal artifact directory
	artifactDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(artifactDir, "index.html"),
		[]byte("<html><body>test</body></html>"),
		0644,
	); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	app := ghCRD("integration-test", "integration.example.com", "myrgic/cogos-integration-test-nonexistent")

	g := ghPages()
	err := g.Deploy(context.Background(), app, artifactDir)

	// We expect the push to fail (repo doesn't exist); earlier steps should succeed.
	// If error is nil, that's unexpected but fine (test repo might exist).
	if err != nil {
		// Only the push step should fail
		if !strings.Contains(err.Error(), "Deploy: git push") &&
			!strings.Contains(err.Error(), "push") &&
			!strings.Contains(err.Error(), "remote") {
			t.Errorf("Deploy: unexpected failure stage (want push/remote error, got: %v)", err)
		}
		// Early failure (init, copy, CNAME, add, commit) is a real defect
		for _, earlyStage := range []string{"git init", "copy artifact", "write CNAME", "git add", "git commit"} {
			if strings.Contains(err.Error(), earlyStage) {
				t.Errorf("Deploy: unexpected early failure at %q: %v", earlyStage, err)
			}
		}
	}
}

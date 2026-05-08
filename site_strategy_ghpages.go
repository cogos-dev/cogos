// site_strategy_ghpages.go — GitHub Pages deploy strategy.
//
// GHPagesStrategy implements DeployStrategy for sites deployed via GitHub Pages.
// It uses the gh CLI for API queries (FetchLive) and git for branch-push deploys.
//
// Deployment model: the target repo's main branch is treated as the deploy branch.
// Each deploy replaces the branch HEAD entirely (force-push). History is intentionally
// replaced — the deploy target is auto-managed artifact storage, not source history.
//
// Prerequisites: gh CLI authenticated, git configured with push access to the target repo.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// GHPagesStrategy deploys sites to GitHub Pages via the gh CLI and git.
type GHPagesStrategy struct {
	mu sync.Mutex
}

// init self-registers GHPagesStrategy with the global SiteProvider.
// site_provider.go's init() runs first (alphabetical file order within the package),
// so GetProvider("site") is guaranteed to find the SiteProvider before this fires.
func init() {
	p, err := GetProvider("site")
	if err != nil {
		// Should never happen in normal operation; log and continue rather than panic.
		return
	}
	sp, ok := p.(*SiteProvider)
	if !ok {
		return
	}
	sp.RegisterStrategy("gh-pages", &GHPagesStrategy{})
}

// resolveRepo extracts the "repo" key from the CRD's deploy target map.
func (g *GHPagesStrategy) resolveRepo(app SiteCRD) (string, error) {
	raw, ok := app.Spec.Deploy.Target["repo"]
	if !ok {
		return "", fmt.Errorf("gh-pages: deploy.target.repo is required")
	}
	repo, ok := raw.(string)
	if !ok || repo == "" {
		return "", fmt.Errorf("gh-pages: deploy.target.repo must be a non-empty string")
	}
	return repo, nil
}

// ghAPI runs: gh api <path> and returns stdout bytes.
func ghAPI(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api %s: %w: %s", path, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// ─── FetchLive ──────────────────────────────────────────────────────────────────

// FetchLive retrieves the current deployed state from the GitHub Pages target repo.
// It queries:
//   - /repos/<repo>/git/refs/heads/main → ArtifactSHA (current HEAD)
//   - /repos/<repo>/contents/CNAME → CNAMEContent (optional; 404 = empty)
func (g *GHPagesStrategy) FetchLive(ctx context.Context, app SiteCRD) (LiveSiteState, error) {
	repo, err := g.resolveRepo(app)
	if err != nil {
		return LiveSiteState{}, err
	}

	state := LiveSiteState{
		ResolvedFromTarget: true,
		Metadata:           make(map[string]any),
	}

	// Query main branch SHA
	refData, err := ghAPI(ctx, fmt.Sprintf("/repos/%s/git/refs/heads/main", repo))
	if err != nil {
		// If the repo doesn't exist yet or has no main branch, treat as not deployed
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not Found") {
			state.ResolvedFromTarget = false
			return state, nil
		}
		return LiveSiteState{}, fmt.Errorf("FetchLive: fetch main ref: %w", err)
	}

	var refResp struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(refData, &refResp); err != nil {
		return LiveSiteState{}, fmt.Errorf("FetchLive: parse ref response: %w", err)
	}
	state.ArtifactSHA = refResp.Object.SHA

	// Query CNAME (optional — 404 is normal)
	cnameData, err := ghAPI(ctx, fmt.Sprintf("/repos/%s/contents/CNAME", repo))
	if err == nil {
		var cnameResp struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := json.Unmarshal(cnameData, &cnameResp); err == nil && cnameResp.Encoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(
				strings.ReplaceAll(cnameResp.Content, "\n", ""))
			if err == nil {
				state.CNAMEContent = strings.TrimSpace(string(decoded))
			}
		}
	}
	// CNAME fetch errors are silently ignored — it's optional

	state.Metadata["repo"] = repo
	return state, nil
}

// ─── Deploy ─────────────────────────────────────────────────────────────────────

// Deploy publishes the artifact directory to the GitHub Pages target repo.
// It creates a temp git repo, copies the artifact, writes CNAME, and force-pushes.
func (g *GHPagesStrategy) Deploy(ctx context.Context, app SiteCRD, artifactDir string) error {
	repo, err := g.resolveRepo(app)
	if err != nil {
		return err
	}

	// Determine source SHA for the commit message (set by ApplyPlan via env)
	sourceSHA := os.Getenv("__SOURCE_SHA")
	if sourceSHA == "" {
		sourceSHA = "unknown"
	}
	if len(sourceSHA) > 12 {
		sourceSHA = sourceSHA[:12]
	}

	// Create a temp directory for the ephemeral deploy repo
	tmpDir, err := os.MkdirTemp("", "cogos-ghpages-*")
	if err != nil {
		return fmt.Errorf("Deploy: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// git init
	if err := gitRun(ctx, tmpDir, "init"); err != nil {
		return fmt.Errorf("Deploy: git init: %w", err)
	}

	// Copy artifact contents into temp dir
	if err := copyDir(artifactDir, tmpDir); err != nil {
		return fmt.Errorf("Deploy: copy artifact: %w", err)
	}

	// Write CNAME file (domain without trailing newline)
	cnamePath := filepath.Join(tmpDir, "CNAME")
	if err := os.WriteFile(cnamePath, []byte(app.Spec.Domain), 0644); err != nil {
		return fmt.Errorf("Deploy: write CNAME: %w", err)
	}

	// Stage all files
	if err := gitRun(ctx, tmpDir, "add", "."); err != nil {
		return fmt.Errorf("Deploy: git add: %w", err)
	}

	// Commit
	msg := fmt.Sprintf("deploy: %s @ %s", app.Spec.Domain, sourceSHA)
	if err := gitRun(ctx, tmpDir, "-c", "user.email=cog@myrgic.io",
		"-c", "user.name=CogOS", "commit", "-m", msg); err != nil {
		return fmt.Errorf("Deploy: git commit: %w", err)
	}

	// Set remote origin using HTTPS form (gh CLI auth handles credentials via git-credential-gh).
	// gh CLI configures git-credential-helper on install, so no token embedding is needed.
	remote := fmt.Sprintf("https://github.com/%s.git", repo)
	if err := gitRun(ctx, tmpDir, "remote", "add", "origin", remote); err != nil {
		return fmt.Errorf("Deploy: git remote add: %w", err)
	}

	// Force-push to main — deploy repo is auto-managed; history is replaced per deploy.
	// This is the correct semantic for a gh-pages deploy target: no source lineage needed,
	// only the latest artifact matters.
	if err := gitRun(ctx, tmpDir, "push", "--force", "origin", "HEAD:main"); err != nil {
		return fmt.Errorf("Deploy: git push: %w", err)
	}

	return nil
}

// ─── Helpers ────────────────────────────────────────────────────────────────────

// gitRun executes a git command in dir and returns combined output on error.
func gitRun(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// copyDir recursively copies src directory contents into dst (not src itself).
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target, info.Mode())
	})
}

// copyFile copies a single file preserving its mode.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// Package site provides the Reconcilable provider for static-site deployments.
//
// This package makes SiteProvider and GHPagesStrategy available to the daemon
// binary (cmd/cogos) which cannot import the workspace-root package main.
//
// The types and logic mirror site_*.go in the workspace root. The workspace-root
// copies remain canonical for the cog CLI; this package is the importable
// counterpart used by cmd/cogos/providers_wire.go.
//
// Registration: importing this package (even as a blank import) triggers init(),
// which registers "site" with pkg/reconcile. GHPagesStrategy is self-registered
// in the same init() chain.
package site

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
	"gopkg.in/yaml.v3"
)

func init() {
	sp := newSiteProvider()
	reconcile.RegisterProvider("site", sp)
	sp.RegisterStrategy("gh-pages", &ghPagesStrategy{})
}

// ─── SiteCRD types ───────────────────────────────────────────────────────────

var knownStrategies = map[string]bool{
	"gh-pages":          true,
	"gitlab-pages":      true,
	"s3":                true,
	"k8s-ingress":       true,
	"self-hosted-rsync": true,
}

// ErrInvalidSiteCRD is returned by Validate when the CRD is inconsistent.
var ErrInvalidSiteCRD = errors.New("invalid SiteCRD")

// ErrUnsupportedStrategy is returned when the requested strategy is not registered.
var ErrUnsupportedStrategy = errors.New("unsupported deploy strategy")

type siteCRD struct {
	APIVersion string       `yaml:"apiVersion" json:"apiVersion"`
	Kind       string       `yaml:"kind"       json:"kind"`
	Metadata   siteMetadata `yaml:"metadata"   json:"metadata"`
	Spec       siteSpec     `yaml:"spec"       json:"spec"`
}

type siteMetadata struct {
	Name string `yaml:"name" json:"name"`
}

type siteSpec struct {
	Domain     string     `yaml:"domain"      json:"domain"`
	Canonical  bool       `yaml:"canonical"   json:"canonical"`
	Source     sourceSpec `yaml:"source"      json:"source"`
	Build      buildSpec  `yaml:"build"       json:"build"`
	HTTPS      httpsSpec  `yaml:"https"       json:"https"`
	Deploy     deploySpec `yaml:"deploy"      json:"deploy"`
	RedirectTo *string    `yaml:"redirect_to,omitempty" json:"redirect_to,omitempty"`
}

type sourceSpec struct{ Path string `yaml:"path" json:"path"` }
type buildSpec struct {
	Command string `yaml:"command" json:"command"`
	Dist    string `yaml:"dist"    json:"dist"`
}
type httpsSpec struct{ Required bool `yaml:"required" json:"required"` }
type deploySpec struct {
	Strategy string         `yaml:"strategy" json:"strategy"`
	Target   map[string]any `yaml:"target"   json:"target"`
}

func (s siteCRD) Validate() error {
	if s.Metadata.Name == "" {
		return fmt.Errorf("%w: metadata.name is required", ErrInvalidSiteCRD)
	}
	if s.Spec.Domain == "" {
		return fmt.Errorf("%w: spec.domain is required", ErrInvalidSiteCRD)
	}
	if s.Spec.Canonical && s.Spec.RedirectTo != nil {
		return fmt.Errorf("%w: canonical site %q must not set redirect_to", ErrInvalidSiteCRD, s.Metadata.Name)
	}
	if s.Spec.Deploy.Strategy == "" {
		return fmt.Errorf("%w: spec.deploy.strategy is required", ErrInvalidSiteCRD)
	}
	if !knownStrategies[s.Spec.Deploy.Strategy] {
		return fmt.Errorf("%w: unknown deploy strategy %q", ErrInvalidSiteCRD, s.Spec.Deploy.Strategy)
	}
	return nil
}

// ─── DeployStrategy ──────────────────────────────────────────────────────────

type liveSiteState struct {
	ArtifactSHA        string         `json:"artifact_sha"`
	CNAMEContent       string         `json:"cname_content"`
	ResolvedFromTarget bool           `json:"resolved_from_target"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

type deployStrategy interface {
	FetchLive(ctx context.Context, app siteCRD) (liveSiteState, error)
	Deploy(ctx context.Context, app siteCRD, artifactDir string) error
}

// ─── SiteProvider ────────────────────────────────────────────────────────────

type siteProvider struct {
	mu         sync.Mutex
	root       string
	strategies map[string]deployStrategy
}

func newSiteProvider() *siteProvider {
	return &siteProvider{strategies: map[string]deployStrategy{}}
}

func (s *siteProvider) RegisterStrategy(name string, strat deployStrategy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strategies[name] = strat
}

func (s *siteProvider) lookupStrategy(name string) (deployStrategy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	strat, ok := s.strategies[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedStrategy, name)
	}
	return strat, nil
}

func (s *siteProvider) Type() string { return "site" }

func (s *siteProvider) Health() reconcile.ResourceStatus {
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthProgressing)
}

func (s *siteProvider) LoadConfig(root string) (any, error) {
	s.mu.Lock()
	s.root = root
	s.mu.Unlock()
	crds, err := loadCRDs(root)
	if err != nil {
		return nil, err
	}
	if crds == nil {
		crds = []siteCRD{}
	}
	return crds, nil
}

func (s *siteProvider) FetchLive(ctx context.Context, config any) (any, error) {
	crds, ok := config.([]siteCRD)
	if !ok {
		return nil, fmt.Errorf("site provider: FetchLive: unexpected config type %T", config)
	}
	live := make(map[string]liveSiteState, len(crds))
	for _, crd := range crds {
		strat, err := s.lookupStrategy(crd.Spec.Deploy.Strategy)
		if err != nil {
			log.Printf("[site] warning: FetchLive %s: %v", crd.Metadata.Name, err)
			live[crd.Metadata.Name] = liveSiteState{}
			continue
		}
		state, err := strat.FetchLive(ctx, crd)
		if err != nil {
			log.Printf("[site] warning: FetchLive %s: %v", crd.Metadata.Name, err)
			live[crd.Metadata.Name] = liveSiteState{}
			continue
		}
		live[crd.Metadata.Name] = state
	}
	return live, nil
}

func (s *siteProvider) ComputePlan(config any, live any, state *reconcile.State) (*reconcile.Plan, error) {
	crds, ok := config.([]siteCRD)
	if !ok {
		return nil, fmt.Errorf("site provider: ComputePlan: unexpected config type %T", config)
	}
	liveMap, ok := live.(map[string]liveSiteState)
	if !ok {
		return nil, fmt.Errorf("site provider: ComputePlan: unexpected live type %T", live)
	}

	s.mu.Lock()
	root := s.root
	s.mu.Unlock()

	plan := &reconcile.Plan{
		ResourceType: "site",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath:   root + "/apps",
	}

	declared := make(map[string]bool, len(crds))
	for _, crd := range crds {
		name := crd.Metadata.Name
		declared[name] = true
		appDir := root + "/apps/" + name

		liveState := liveMap[name]
		details := map[string]any{
			"domain":   crd.Spec.Domain,
			"strategy": crd.Spec.Deploy.Strategy,
			"target":   crd.Spec.Deploy.Target,
			"app_dir":  appDir,
		}

		if _, err := s.lookupStrategy(crd.Spec.Deploy.Strategy); err != nil {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("%s: strategy %q not registered — deploy will fail", name, crd.Spec.Deploy.Strategy))
		}

		if !liveState.ResolvedFromTarget {
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionCreate,
				ResourceType: "site",
				Name:         name,
				Details:      details,
			})
			plan.Summary.Creates++
			continue
		}

		if liveState.ArtifactSHA == "" {
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionUpdate,
				ResourceType: "site",
				Name:         name,
				Details:      details,
			})
			plan.Summary.Updates++
			continue
		}

		plan.Actions = append(plan.Actions, reconcile.Action{
			Action:       reconcile.ActionSkip,
			ResourceType: "site",
			Name:         name,
			Details:      details,
		})
		plan.Summary.Skipped++
	}

	if state != nil {
		for _, res := range state.Resources {
			if !declared[res.Name] {
				plan.Actions = append(plan.Actions, reconcile.Action{
					Action:       reconcile.ActionDelete,
					ResourceType: "site",
					Name:         res.Name,
					Details:      map[string]any{"reason": "no matching site CRD"},
				})
				plan.Summary.Deletes++
			}
		}
	}

	return plan, nil
}

func (s *siteProvider) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	var results []reconcile.Result
	for _, action := range plan.Actions {
		if action.Action == reconcile.ActionSkip {
			continue
		}
		results = append(results, s.applyAction(ctx, action))
	}
	return results, nil
}

func (s *siteProvider) applyAction(ctx context.Context, action reconcile.Action) reconcile.Result {
	base := reconcile.Result{
		Phase:  "site",
		Action: string(action.Action),
		Name:   action.Name,
	}
	if action.Action == reconcile.ActionDelete {
		log.Printf("[site] delete %s: no-op in v0.0.1 (manual teardown required)", action.Name)
		base.Status = reconcile.ApplySucceeded
		return base
	}
	appDir, _ := action.Details["app_dir"].(string)
	if appDir == "" {
		base.Status = reconcile.ApplyFailed
		base.Error = "no app_dir in action details"
		return base
	}
	if err := siteBuild(ctx, appDir, action.Name); err != nil {
		base.Status = reconcile.ApplyFailed
		base.Error = fmt.Sprintf("build: %v", err)
		return base
	}
	stratName, _ := action.Details["strategy"].(string)
	strat, err := s.lookupStrategy(stratName)
	if err != nil {
		base.Status = reconcile.ApplyFailed
		base.Error = fmt.Sprintf("strategy lookup: %v", err)
		return base
	}
	s.mu.Lock()
	root := s.root
	s.mu.Unlock()
	crds, err := loadCRDs(root)
	if err != nil {
		base.Status = reconcile.ApplyFailed
		base.Error = fmt.Sprintf("reload config: %v", err)
		return base
	}
	crd, found := crdByName(crds, action.Name)
	if !found {
		base.Status = reconcile.ApplyFailed
		base.Error = fmt.Sprintf("CRD not found after reload: %s", action.Name)
		return base
	}
	if err := strat.Deploy(ctx, crd, appDir+"/dist"); err != nil {
		base.Status = reconcile.ApplyFailed
		base.Error = fmt.Sprintf("deploy: %v", err)
		return base
	}
	base.Status = reconcile.ApplySucceeded
	base.CreatedID = "site:" + action.Name
	return base
}

func (s *siteProvider) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	liveMap, ok := live.(map[string]liveSiteState)
	if !ok {
		return nil, fmt.Errorf("site provider: BuildState: unexpected live type %T", live)
	}
	state := &reconcile.State{
		Version:      1,
		ResourceType: "site",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	} else {
		state.Lineage = "site-" + time.Now().UTC().Format("20060102T150405Z")
	}
	for name, liveState := range liveMap {
		state.Resources = append(state.Resources, reconcile.Resource{
			Address:       "site." + name,
			Type:          "site",
			Mode:          reconcile.ModeManaged,
			ExternalID:    liveState.ArtifactSHA,
			Name:          name,
			LastRefreshed: time.Now().UTC().Format(time.RFC3339),
			Attributes: map[string]any{
				"artifact_sha":         liveState.ArtifactSHA,
				"cname_content":        liveState.CNAMEContent,
				"resolved_from_target": liveState.ResolvedFromTarget,
			},
		})
	}
	return state, nil
}

// ─── Config helpers ──────────────────────────────────────────────────────────

func loadCRDs(root string) ([]siteCRD, error) {
	pattern := filepath.Join(root, "apps", "*", "site.yaml")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("site provider: glob %q: %w", pattern, err)
	}
	var crds []siteCRD
	for _, yamlPath := range matches {
		data, err := os.ReadFile(yamlPath)
		if err != nil {
			return nil, fmt.Errorf("site provider: read %s: %w", yamlPath, err)
		}
		var crd siteCRD
		if err := yaml.Unmarshal(data, &crd); err != nil {
			return nil, fmt.Errorf("site provider: parse %s: %w", yamlPath, err)
		}
		if err := crd.Validate(); err != nil {
			log.Printf("[site] warning: %s validation: %v", yamlPath, err)
		}
		crds = append(crds, crd)
	}
	return crds, nil
}

func crdByName(crds []siteCRD, name string) (siteCRD, bool) {
	for _, c := range crds {
		if c.Metadata.Name == name {
			return c, true
		}
	}
	return siteCRD{}, false
}

func siteBuild(ctx context.Context, appDir, name string) error {
	cmd := exec.CommandContext(ctx, "bash", "build.sh")
	cmd.Dir = appDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build.sh for %s: %w\n%s", name, err, out)
	}
	log.Printf("[site] build %s: %s", name, out)
	return nil
}

// ─── GHPagesStrategy ─────────────────────────────────────────────────────────

type ghPagesStrategy struct{ mu sync.Mutex }

func (g *ghPagesStrategy) resolveRepo(app siteCRD) (string, error) {
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

func (g *ghPagesStrategy) FetchLive(ctx context.Context, app siteCRD) (liveSiteState, error) {
	repo, err := g.resolveRepo(app)
	if err != nil {
		return liveSiteState{}, err
	}
	state := liveSiteState{ResolvedFromTarget: true, Metadata: make(map[string]any)}

	refData, err := ghAPI(ctx, fmt.Sprintf("/repos/%s/git/refs/heads/main", repo))
	if err != nil {
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not Found") {
			state.ResolvedFromTarget = false
			return state, nil
		}
		return liveSiteState{}, fmt.Errorf("FetchLive: fetch main ref: %w", err)
	}
	var refResp struct {
		Object struct{ SHA string `json:"sha"` } `json:"object"`
	}
	if err := json.Unmarshal(refData, &refResp); err != nil {
		return liveSiteState{}, fmt.Errorf("FetchLive: parse ref response: %w", err)
	}
	state.ArtifactSHA = refResp.Object.SHA

	cnameData, err := ghAPI(ctx, fmt.Sprintf("/repos/%s/contents/CNAME", repo))
	if err == nil {
		var cnameResp struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := json.Unmarshal(cnameData, &cnameResp); err == nil && cnameResp.Encoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(cnameResp.Content, "\n", ""))
			if err == nil {
				state.CNAMEContent = strings.TrimSpace(string(decoded))
			}
		}
	}
	state.Metadata["repo"] = repo
	return state, nil
}

func (g *ghPagesStrategy) Deploy(ctx context.Context, app siteCRD, artifactDir string) error {
	repo, err := g.resolveRepo(app)
	if err != nil {
		return err
	}
	sourceSHA := os.Getenv("__SOURCE_SHA")
	if sourceSHA == "" {
		sourceSHA = "unknown"
	}
	if len(sourceSHA) > 12 {
		sourceSHA = sourceSHA[:12]
	}
	tmpDir, err := os.MkdirTemp("", "cogos-ghpages-*")
	if err != nil {
		return fmt.Errorf("Deploy: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := gitRun(ctx, tmpDir, "init"); err != nil {
		return fmt.Errorf("Deploy: git init: %w", err)
	}
	if err := copyDir(artifactDir, tmpDir); err != nil {
		return fmt.Errorf("Deploy: copy artifact: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "CNAME"), []byte(app.Spec.Domain), 0644); err != nil {
		return fmt.Errorf("Deploy: write CNAME: %w", err)
	}
	if err := gitRun(ctx, tmpDir, "add", "."); err != nil {
		return fmt.Errorf("Deploy: git add: %w", err)
	}
	msg := fmt.Sprintf("deploy: %s @ %s", app.Spec.Domain, sourceSHA)
	if err := gitRun(ctx, tmpDir, "-c", "user.email=cog@myrgic.io",
		"-c", "user.name=CogOS", "commit", "-m", msg); err != nil {
		return fmt.Errorf("Deploy: git commit: %w", err)
	}
	remote := fmt.Sprintf("https://github.com/%s.git", repo)
	if err := gitRun(ctx, tmpDir, "remote", "add", "origin", remote); err != nil {
		return fmt.Errorf("Deploy: git remote add: %w", err)
	}
	if err := gitRun(ctx, tmpDir, "push", "--force", "origin", "HEAD:main"); err != nil {
		return fmt.Errorf("Deploy: git push: %w", err)
	}
	return nil
}

func gitRun(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

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

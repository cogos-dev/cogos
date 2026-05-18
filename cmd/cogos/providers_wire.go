// providers_wire.go — wires Reconcilable provider registration, workspace
// context, and MCP tool extensions into the kernel daemon at boot.
//
// A named import of internal/providers/daemon triggers that package's init(),
// which registers all 10 production providers ("agent", "component", "discord",
// "eval", "identity", "mcp-tools", "openclaw-agents", "openclaw-cron",
// "openclaw-gateway", "service") with pkg/reconcile before engine.Main()
// starts the HTTP server.
//
// The identity provider (Wave 6b) registers a daemon-side stub via
// internal/providers/daemon. The full plan/apply implementation lives in
// workspace-root identity_wiring.go (cog CLI binary).
//
// engine.RegisterProviders is set here so the registration call happens inside
// runServe() (after the logger is up) rather than in a file-level init() that
// might fire before tracing/logging infrastructure is ready.
//
// engine.SetProvidersWorkspace is set here so that after LoadConfig resolves
// cfg.WorkspaceRoot, the daemon-side providers receive the workspace path and
// can perform real filesystem Health() checks rather than reporting
// "workspace not yet configured".
//
// engine.RegisterMCPExtensions wires the four eval MCP tools
// (cog_run_experiment, cog_list_experiments, cog_get_experiment_status,
// cog_pin_baseline) onto the kernel's MCP server so they are accessible
// from both the cog CLI binary and the daemon.
//
// workspace.LoadConfig and workspace.GitRoot are wired here so that
// internal/workspace.ResolveWorkspace() can resolve the active workspace
// from ~/.cog/node/global.yaml and from the enclosing git repository, in
// addition to the COG_ROOT environment variable. Without this wiring the
// daemon binary skips tiers 2-4 of the resolution algorithm silently.
// Previously this wiring lived only in cog.go:init() (the CLI binary).
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/internal/eval"
	"github.com/myrgic/cogos/internal/providers/component"
	"github.com/myrgic/cogos/internal/providers/daemon"
	_ "github.com/myrgic/cogos/internal/providers/site" // registers "site" with pkg/reconcile
	"github.com/myrgic/cogos/internal/workspace"
	"gopkg.in/yaml.v3"
)

// daemonGlobalConfig is the minimal shape of ~/.cog/node/global.yaml that
// workspace resolution requires. Only the two fields used by
// workspace.ConfigProvider are declared; other keys round-trip cleanly via
// yaml.v3's default behaviour.
//
// ADR-085 rule 7: duplicate small helpers rather than exporting them from
// the root CLI package.
type daemonGlobalConfig struct {
	CurrentWorkspace string                           `yaml:"current-workspace,omitempty"`
	Workspaces       map[string]*daemonWorkspaceEntry `yaml:"workspaces,omitempty"`
}

type daemonWorkspaceEntry struct {
	Path string `yaml:"path"`
}

func (c *daemonGlobalConfig) CurrentWorkspaceValue() string { return c.CurrentWorkspace }
func (c *daemonGlobalConfig) WorkspacePath(name string) (string, bool) {
	e, ok := c.Workspaces[name]
	if !ok || e == nil {
		return "", false
	}
	return e.Path, ok
}

// daemonConfigProvider wraps *daemonGlobalConfig to implement workspace.ConfigProvider.
type daemonConfigProvider struct{ cfg *daemonGlobalConfig }

func (p daemonConfigProvider) CurrentWorkspace() string { return p.cfg.CurrentWorkspace }
func (p daemonConfigProvider) WorkspacePath(name string) (string, bool) {
	return p.cfg.WorkspacePath(name)
}

// loadDaemonGlobalConfig reads ~/.cog/node/global.yaml, falling back to
// ~/.cog/config (the legacy path used before issue #161). Returns an empty
// config on ENOENT so the daemon starts even on a fresh node.
//
// Local duplicate of cog.go's loadGlobalConfig / globalConfigPath per
// ADR-085 rule 7.
func loadDaemonGlobalConfig() (workspace.ConfigProvider, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return daemonConfigProvider{cfg: &daemonGlobalConfig{
			Workspaces: make(map[string]*daemonWorkspaceEntry),
		}}, nil
	}
	newPath := filepath.Join(home, ".cog", "node", "global.yaml")
	oldPath := filepath.Join(home, ".cog", "config")

	path := newPath
	if _, statErr := os.Stat(newPath); os.IsNotExist(statErr) {
		if _, statErr2 := os.Stat(oldPath); statErr2 == nil {
			path = oldPath
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return daemonConfigProvider{cfg: &daemonGlobalConfig{
				Workspaces: make(map[string]*daemonWorkspaceEntry),
			}}, nil
		}
		return nil, fmt.Errorf("daemon: read global config: %w", err)
	}

	var cfg daemonGlobalConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("daemon: parse global config: %w", err)
	}
	if cfg.Workspaces == nil {
		cfg.Workspaces = make(map[string]*daemonWorkspaceEntry)
	}
	return daemonConfigProvider{cfg: &cfg}, nil
}

// daemonGitRoot returns the top-level git repository containing the process
// working directory. Local duplicate of cog.go's gitRoot per ADR-085 rule 7.
func daemonGitRoot() (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("not a git repository")
	}
	return strings.TrimSpace(out.String()), nil
}

// uriRegistryLocatorAdapter adapts engine.ResolveWorkspacePath to satisfy the
// pin.WorkspaceLocator interface. This avoids exporting the engine's unexported
// uriResolver type while still allowing the pin provider to consult the global
// workspace registry for target path resolution.
type uriRegistryLocatorAdapter struct{}

func (a *uriRegistryLocatorAdapter) LocateWorkspace(ctx context.Context, name string) (string, error) {
	return engine.ResolveWorkspacePath(ctx, name)
}

// daemonEvalProvider is the daemon-side EvalProvider instance passed to the
// eval MCP tools. The daemon does not run plan/apply; it only exposes the
// four read/trigger tools whose state effects (trigger files, baseline pins)
// are read by the CLI's reconcile loop.
var daemonEvalProvider = eval.New(nil, nil)

func init() {
	// Wire workspace.LoadConfig and workspace.GitRoot so that
	// internal/workspace.ResolveWorkspace() can resolve workspaces via the
	// global registry (~/.cog/node/global.yaml) and via git root detection,
	// in addition to the COG_ROOT env var that always works unconditionally.
	//
	// These DI seams were previously only wired in cog.go:init() (the CLI
	// binary). Moving them here closes the gap for the daemon binary.
	workspace.LoadConfig = func() (workspace.ConfigProvider, error) {
		return loadDaemonGlobalConfig()
	}
	workspace.GitRoot = daemonGitRoot

	// The named import of internal/providers/daemon above already triggered
	// daemon.init() (and component.init() via daemon's blank import), which
	// called reconcile.RegisterProvider for all 10 providers (including the
	// Wave 6b identity stub). engine.RegisterProviders is set to a no-op rather
	// than left nil so runServe() logs "providers registered" when it calls the hook.
	engine.RegisterProviders = func() {
		// providers already registered by internal/providers/daemon init()
	}

	// Wire workspace context into daemon-side providers. runServe() calls this
	// after LoadConfig resolves cfg.WorkspaceRoot. Until then, workspaceRoot
	// is "" and Health() returns "workspace not yet configured" — acceptable
	// because no Health() is called until the autonomic ticker or foveated
	// handler fires.
	engine.SetProvidersWorkspace = func(workspaceRoot string) {
		daemon.SetWorkspaceRoot(workspaceRoot)
		component.SetWorkspaceRoot(workspaceRoot)
		// Wire URIRegistry into the pin provider's WorkspaceLocator so FetchLive
		// can consult the global workspace registry for target path resolution
		// (two-step chain: registry → sibling-dir fallback). Safe to call every
		// time SetProvidersWorkspace fires — SetPinWorkspaceLocator is idempotent.
		daemon.SetPinWorkspaceLocator(&uriRegistryLocatorAdapter{})
		// Prime the daemon EvalProvider root so its MCP tools can resolve
		// the workspace-relative state files (eval-dispatch-triggers.json,
		// eval-baselines.json). LoadConfig is idempotent — safe to call here.
		if workspaceRoot != "" {
			_, _ = daemonEvalProvider.LoadConfig(workspaceRoot)
		}
	}

	// Wire the four eval MCP tools onto the kernel's MCP server. The daemon
	// EvalProvider is minimal (no dispatcher/emitter) — the tools read/write
	// the sidecar state files that the CLI's reconcile loop consumes.
	// The root path is set lazily: cog_run_experiment calls LoadConfig, which
	// sets e.root before writeDispatchTrigger uses it. For a fully configured
	// workspace, the tools work end-to-end; for a fresh smoke workspace they
	// return a "not configured" error that still exercises the code path.
	engine.RegisterMCPExtensions = func(srv *engine.MCPServer) {
		eval.RegisterEvalTools(srv.Server(), daemonEvalProvider)
	}
}

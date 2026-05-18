// Package daemon registers all Reconcilable providers that the kernel daemon
// (cmd/cogos) needs to surface in proprioception health blocks.
//
// Background: The production providers (agent, component, discord, identity,
// mcp-tools, openclaw-agents, openclaw-cron, openclaw-gateway, service) were
// originally defined in the workspace-root package main of the cog CLI.
// Because package main cannot be imported, the daemon binary (built from
// cmd/cogos) could not reach those registrations and pkg/reconcile.ListProviders()
// always returned empty.
//
// This package provides daemon-safe provider structs whose Health()
// implementations replicate the workspace-root logic using only importable
// packages (os, filepath, pkg/reconcile).
// The non-health methods (LoadConfig, FetchLive, ComputePlan, ApplyPlan,
// BuildState) return errors — the daemon only exercises Health() through the
// proprioception block.
//
// Workspace context is injected at boot via SetWorkspaceRoot(), called by
// engine.SetProvidersWorkspace after LoadConfig resolves cfg.WorkspaceRoot.
// This avoids the workspace.ResolveWorkspace() dependency-injection seams
// (LoadConfig/GitRoot func vars) that are only wired in the cog CLI's main
// package, not in cmd/cogos.
//
// The component provider is already fully extracted to
// internal/providers/component and is wired here via blank import.
// The pin provider (internal/providers/pin) is fully extracted and registered
// here directly — its Health() delegates to the extracted package.
// The identity provider (Wave 6b) is registered here as a stub; the full
// plan/apply wiring lives in the workspace-root identity_wiring.go.
// The other eight are implemented as minimal structs below.
//
// cmd/cogos/providers_wire.go imports this package (triggering init()) and
// wires both engine.RegisterProviders and engine.SetProvidersWorkspace so
// the full seam is operational before the HTTP server starts serving requests.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/myrgic/cogos/internal/providers/pin"
	"github.com/myrgic/cogos/pkg/reconcile"

	// Trigger internal/providers/component's init() which registers "component".
	_ "github.com/myrgic/cogos/internal/providers/component"
)

// workspaceRoot is set at daemon boot by SetWorkspaceRoot (called from
// engine.SetProvidersWorkspace after LoadConfig resolves the workspace path).
// It is read by resolveRoot() on every Health() probe. Zero value means
// "not yet wired" — callers get a clear error rather than a silent bad result.
var (
	workspaceRootMu sync.RWMutex
	workspaceRoot   string
)

// SetWorkspaceRoot injects the resolved workspace path into this package so
// that all daemon-side provider Health() implementations can resolve their
// filesystem checks without depending on workspace.ResolveWorkspace(), whose
// dependency-injection seams (LoadConfig/GitRoot) are not wired in the
// cmd/cogos binary.
//
// Must be called before any provider Health() invocation — engine.runServe()
// calls it via engine.SetProvidersWorkspace immediately after LoadConfig
// resolves cfg.WorkspaceRoot.
func SetWorkspaceRoot(root string) {
	workspaceRootMu.Lock()
	defer workspaceRootMu.Unlock()
	workspaceRoot = root
}

// globalPinProvider is the singleton pinProvider registered in init().
// Exposed so providers_wire.go can inject the workspace locator after the
// engine's URIRegistry is initialised.
var globalPinProvider = &pinProvider{stubMethods: stubMethods{name: "pin"}}

// SetPinWorkspaceLocator wires a WorkspaceLocator into the pin provider so that
// FetchLive can consult the global workspace registry. Called from
// providers_wire.go after engine.URIRegistry is available.
func SetPinWorkspaceLocator(loc pin.WorkspaceLocator) {
	globalPinProvider.mu.Lock()
	defer globalPinProvider.mu.Unlock()
	if globalPinProvider.impl == nil {
		globalPinProvider.impl = pin.New(nil)
	}
	globalPinProvider.impl.SetWorkspaceLocator(loc)
}

// pinProvider is the daemon-side wrapper around the fully-extracted pin provider.
// It delegates Health() to pin.New() after running the read-only refresh cycle
// (LoadConfig → FetchLive → ComputePlan, no ApplyPlan) so that pinStates is
// populated before Health() is called. Without the read-only cycle, pinStates is
// always empty and Health() always reports green ("no pins declared") even when
// pin files declare drift.
//
// The refresh is time-bounded to 10 s to avoid blocking the proprioception tick.
// staleAfter controls how long a prior cycle result is reused to avoid hammering
// the filesystem on every foveated-context refresh.
//
// Concurrency: mu is held from the staleness check through the entire refresh
// so that concurrent Health() calls serialise on the lock. Only the first
// stale caller runs the refresh; subsequent callers reuse the result already
// being computed (they block on mu.Lock() and see a fresh lastProbe when they
// acquire it). This eliminates parallel RefreshState invocations and the
// associated parallel bumpPinFile race.
type pinProvider struct {
	stubMethods
	mu        sync.Mutex
	impl      *pin.Provider
	lastProbe time.Time
}

const pinProbeStaleAfter = 30 * time.Second

func (p *pinProvider) Type() string { return "pin" }

func (p *pinProvider) Health() reconcile.ResourceStatus {
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}

	// Hold mu for the entire probe so that concurrent Health() calls serialise
	// here. The first caller that finds a stale result runs RefreshState; any
	// other concurrent callers block on Lock() and re-check freshness once they
	// acquire it (finding the result already fresh).
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.impl == nil {
		p.impl = pin.New(nil)
	}

	if time.Since(p.lastProbe) >= pinProbeStaleAfter {
		// Run the read-only refresh cycle (no ApplyPlan, no filesystem writes).
		// A 10 s context prevents blocking the proprioception tick indefinitely.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := p.impl.RefreshState(ctx, root); err != nil {
			return reconcile.ResourceStatus{
				Sync:      reconcile.SyncStatusUnknown,
				Health:    reconcile.HealthDegraded,
				Operation: reconcile.OperationIdle,
				Message:   fmt.Sprintf("pin refresh: %v", err),
			}
		}
		p.lastProbe = time.Now()
	}

	return p.impl.Health()
}

func init() {
	reconcile.RegisterProvider("agent", &agentProvider{})
	reconcile.RegisterProvider("discord", &discordProvider{})
	reconcile.RegisterProvider("eval", &evalProvider{})
	reconcile.RegisterProvider("identity", &identityProvider{stubMethods: stubMethods{name: "identity"}})
	reconcile.RegisterProvider("mcp-tools", &mcpToolsProvider{})
	reconcile.RegisterProvider("openclaw-agents", &openclawAgentsProvider{})
	reconcile.RegisterProvider("openclaw-cron", &openclawCronProvider{})
	reconcile.RegisterProvider("openclaw-gateway", &openclawGatewayProvider{})
	reconcile.RegisterProvider("pin", globalPinProvider)
	reconcile.RegisterProvider("service", &serviceProvider{})
}

// resolveRoot returns the workspace root or an error status.
// It reads the package-level workspaceRoot set by SetWorkspaceRoot, bypassing
// workspace.ResolveWorkspace() whose DI seams are not wired in cmd/cogos.
func resolveRoot() (string, *reconcile.ResourceStatus) {
	workspaceRootMu.RLock()
	root := workspaceRoot
	workspaceRootMu.RUnlock()
	if root == "" {
		s := reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "workspace not yet configured",
		}
		return "", &s
	}
	return root, nil
}

// stubMethods satisfies the non-Health parts of reconcile.Reconcilable.
// All operations return "daemon: operation not available" — the daemon only
// calls Health() through the proprioception block.
type stubMethods struct{ name string }

func (s *stubMethods) LoadConfig(_ string) (any, error) {
	return nil, fmt.Errorf("daemon: LoadConfig not available for %s provider", s.name)
}
func (s *stubMethods) FetchLive(_ context.Context, _ any) (any, error) {
	return nil, fmt.Errorf("daemon: FetchLive not available for %s provider", s.name)
}
func (s *stubMethods) ComputePlan(_ any, _ any, _ *reconcile.State) (*reconcile.Plan, error) {
	return nil, fmt.Errorf("daemon: ComputePlan not available for %s provider", s.name)
}
func (s *stubMethods) ApplyPlan(_ context.Context, _ *reconcile.Plan) ([]reconcile.Result, error) {
	return nil, fmt.Errorf("daemon: ApplyPlan not available for %s provider", s.name)
}
func (s *stubMethods) BuildState(_ any, _ any, _ *reconcile.State) (*reconcile.State, error) {
	return nil, fmt.Errorf("daemon: BuildState not available for %s provider", s.name)
}

// ─── agent ────────────────────────────────────────────────────────────────────

type agentProvider struct{ stubMethods }

func (p *agentProvider) Type() string { return "agent" }

func (p *agentProvider) Health() reconcile.ResourceStatus {
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}
	agentsDir := filepath.Join(root, ".cog", "bin", "agents")
	info, err := os.Stat(agentsDir)
	if err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("agents directory missing: %v", err),
		}
	}
	if !info.IsDir() {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message:   "agents path exists but is not a directory",
		}
	}
	registryPath := filepath.Join(agentsDir, "registry.yaml")
	if _, err := os.Stat(registryPath); err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message:   "registry.yaml missing",
		}
	}
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusUnknown,
		Health:    reconcile.HealthHealthy,
		Operation: reconcile.OperationIdle,
		Message:   fmt.Sprintf("agents directory readable (%s)", agentsDir),
	}
}

// ─── discord ──────────────────────────────────────────────────────────────────

type discordProvider struct{ stubMethods }

func (p *discordProvider) Type() string { return "discord" }

func (p *discordProvider) Health() reconcile.ResourceStatus {
	// Token presence mirrors the workspace-root DiscordProvider.Health() check.
	if os.Getenv("DISCORD_BOT_TOKEN") == "" {
		// Check .cog/config/discord/config.hcl for token field.
		root, bad := resolveRoot()
		if bad != nil {
			return *bad
		}
		hclPath := filepath.Join(root, ".cog", "config", "discord", "config.hcl")
		if _, err := os.Stat(hclPath); err != nil {
			return reconcile.ResourceStatus{
				Sync:      reconcile.SyncStatusUnknown,
				Health:    reconcile.HealthMissing,
				Operation: reconcile.OperationIdle,
				Message:   "no bot token configured",
			}
		}
	}
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthHealthy)
}

// ─── mcp-tools ────────────────────────────────────────────────────────────────

type mcpToolsProvider struct{ stubMethods }

func (p *mcpToolsProvider) Type() string { return "mcp-tools" }

func (p *mcpToolsProvider) Health() reconcile.ResourceStatus {
	if os.Getenv("OPENCLAW_URL") == "" {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthSuspended,
			Operation: reconcile.OperationIdle,
			Message:   "OPENCLAW_URL not set — bridge not available",
		}
	}
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}
	statePath := filepath.Join(root, ".cog", "config", "mcp-tools", ".state.json")
	if _, err := os.Stat(statePath); err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthProgressing,
			Operation: reconcile.OperationIdle,
			Message:   "no state file — tools not yet discovered",
		}
	}
	return reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
}

// ─── openclaw-agents ─────────────────────────────────────────────────────────

type openclawAgentsProvider struct{ stubMethods }

func (p *openclawAgentsProvider) Type() string { return "openclaw-agents" }

func (p *openclawAgentsProvider) Health() reconcile.ResourceStatus {
	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	if _, err := os.Stat(configPath); err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "openclaw.json not found",
		}
	}
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthHealthy)
}

// ─── openclaw-cron ───────────────────────────────────────────────────────────

type openclawCronProvider struct{ stubMethods }

func (p *openclawCronProvider) Type() string { return "openclaw-cron" }

func (p *openclawCronProvider) Health() reconcile.ResourceStatus {
	home, _ := os.UserHomeDir()
	cronPath := filepath.Join(home, ".openclaw", "cron", "jobs.json")
	if _, err := os.Stat(cronPath); err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "jobs.json not found (will be created on first apply)",
		}
	}
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthHealthy)
}

// ─── openclaw-gateway ────────────────────────────────────────────────────────

type openclawGatewayProvider struct{ stubMethods }

func (p *openclawGatewayProvider) Type() string { return "openclaw-gateway" }

func (p *openclawGatewayProvider) Health() reconcile.ResourceStatus {
	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	if _, err := os.Stat(configPath); err != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "openclaw.json not found",
		}
	}
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthHealthy)
}

// ─── eval ─────────────────────────────────────────────────────────────────────

// evalProvider surfaces eval harness health for the daemon's proprioception block.
//
// Health() reads two state files from .cog/state/ that the eval harness writes:
//   - eval-baselines.json        — which experiments have a pinned baseline
//   - eval-dispatch-triggers.json — pending on-demand run requests
//
// Full plan/apply lives in the workspace-root CLI binary (eval_wiring.go);
// the daemon only needs Health() to contribute to the foveated context block.
type evalProvider struct{ stubMethods }

func (p *evalProvider) Type() string { return "eval" }

func (p *evalProvider) Health() reconcile.ResourceStatus {
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}

	stateDir := filepath.Join(root, ".cog", "state")

	// Read baseline pins — presence indicates experiments are being tracked.
	pinsPath := filepath.Join(stateDir, "eval-baselines.json")
	pinnedCount := 0
	if data, err := os.ReadFile(pinsPath); err == nil {
		var pins map[string]string
		if json.Unmarshal(data, &pins) == nil {
			pinnedCount = len(pins)
		}
	}

	// Read pending dispatch triggers — non-empty means a run was requested but
	// the reconcile cycle hasn't consumed it yet.
	triggersPath := filepath.Join(stateDir, "eval-dispatch-triggers.json")
	pendingCount := 0
	if data, err := os.ReadFile(triggersPath); err == nil {
		var triggers map[string]bool
		if json.Unmarshal(data, &triggers) == nil {
			pendingCount = len(triggers)
		}
	}

	// Check for the experiments directory — its presence signals eval is configured.
	experimentsDir := filepath.Join(root, ".cog", "mem", "semantic", "architecture", "tournament", "experiments")
	_, expDirErr := os.Stat(experimentsDir)

	switch {
	case expDirErr != nil:
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "no tournament experiments directory — eval not configured",
		}
	case pendingCount > 0:
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthProgressing,
			Operation: reconcile.OperationSyncing,
			Message:   fmt.Sprintf("%d pending trigger(s), %d pinned baseline(s)", pendingCount, pinnedCount),
		}
	default:
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("%d pinned baseline(s); full plan/apply via cog CLI", pinnedCount),
		}
	}
}

// ─── identity ────────────────────────────────────────────────────────────────

// identityProvider surfaces identity CRD health for the daemon's proprioception
// block. Health() checks for the presence of the identity config directory
// (.cog/config/identities/) and counts declared CRDs.
//
// Full plan/apply lives in the workspace-root CLI binary (identity_wiring.go);
// the daemon only needs Health() to contribute to the foveated context block.
//
// Wave 6b: stub-only. Wave 6c will wire the real IdentityProvider once the
// Constellation DB layer is extractable from the workspace-root package.
type identityProvider struct{ stubMethods }

func (p *identityProvider) Type() string { return "identity" }

func (p *identityProvider) Health() reconcile.ResourceStatus {
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}
	identitiesDir := filepath.Join(root, ".cog", "config", "identities")
	entries, err := os.ReadDir(identitiesDir)
	if err != nil {
		// No identities directory — treat as healthy (no identities declared).
		// Presence of the directory is optional; the provider operates on an
		// empty set if it does not exist.
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusSynced,
			Health:    reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   "no identities directory — no identities declared",
		}
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".yaml" {
			count++
		}
	}
	if count == 0 {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusSynced,
			Health:    reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   "identities directory present, no CRDs declared",
		}
	}
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusUnknown,
		Health:    reconcile.HealthHealthy,
		Operation: reconcile.OperationIdle,
		Message:   fmt.Sprintf("%d identity CRD(s) declared; runtime status requires CLI", count),
	}
}

// ─── service ─────────────────────────────────────────────────────────────────

// serviceProvider checks for service CRD yaml files under
// .cog/config/services/. Full Docker container-status checks require the
// workspace-root CLI (cog plan service) — the daemon reports structural
// presence only.
type serviceProvider struct{ stubMethods }

func (p *serviceProvider) Type() string { return "service" }

func (p *serviceProvider) Health() reconcile.ResourceStatus {
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}
	servicesDir := filepath.Join(root, ".cog", "config", "services")
	entries, err := os.ReadDir(servicesDir)
	if err != nil {
		// No services directory — treat as healthy (no services declared).
		return reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".yaml" {
			count++
		}
	}
	if count == 0 {
		return reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
	}
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusUnknown,
		Health:    reconcile.HealthHealthy,
		Operation: reconcile.OperationIdle,
		Message:   fmt.Sprintf("%d service CRD(s) declared; runtime status requires CLI", count),
	}
}

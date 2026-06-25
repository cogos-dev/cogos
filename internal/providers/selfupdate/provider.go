// Package selfupdate implements the self-update Reconcilable for the CogOS
// kernel daemon.
//
// The provider observes the latest applicable release on GitHub (throttled to
// at most one query per check_interval) and, when auto_apply is enabled on a
// darwin host, spawns a DETACHED `cogos self-update` subprocess that performs
// the download → verify → atomic-swap → restart → health → rollback sequence.
//
// Safety invariants enforced here:
//   - FetchLive never hits GitHub more than once per check_interval (throttle
//     lives in ReleaseResolver).
//   - ComputePlan is PURE: no I/O, no network, no mutation of provider state.
//   - The running daemon NEVER swaps its own binary in-process. ApplyPlan only
//     ever spawns the detached updater; the destructive work happens in that
//     separate process (which survives the daemon restart it triggers).
//   - The auto path never downgrades (Gate F); only an explicit pin may move to
//     an older tag.
//   - Non-darwin hosts are notify-only (Gate H), never erroring.
//
// The shipped default is DISABLED (opt-in via .cog/config/self-update.yaml).
package selfupdate

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// runningVersion is the build version of the currently running daemon. It is
// injected by the engine via SetRunningVersion at boot (the engine cannot be
// imported here without an import cycle: engine's CLI imports this package).
// Defaults to "dev" so a test or un-wired caller gets the same inert behaviour
// as an actual dev build.
var (
	runningVersionMu sync.RWMutex
	runningVersion   = "dev"
)

// SetRunningVersion injects the daemon's build version (e.g. "v0.16.4"). Called
// from the engine at boot. Also stamps the GitHub User-Agent version. Both
// writes happen under runningVersionMu so the User-Agent read in github.go's
// getJSON (also guarded by runningVersionMu) is race-free.
func SetRunningVersion(v string) {
	runningVersionMu.Lock()
	defer runningVersionMu.Unlock()
	runningVersion = v
	if v != "" {
		runningVersionForUA = v
	}
}

// currentRunningVersion returns the injected daemon build version.
func currentRunningVersion() string {
	runningVersionMu.RLock()
	defer runningVersionMu.RUnlock()
	return runningVersion
}

// userAgentVersion returns the version string for the GitHub User-Agent under
// runningVersionMu, so it can be read concurrently with SetRunningVersion
// without a data race. The variable itself lives in github.go.
func userAgentVersion() string {
	runningVersionMu.RLock()
	defer runningVersionMu.RUnlock()
	return runningVersionForUA
}

// defaultHealthPort is the kernel HTTP API port used when none is configured.
const defaultHealthPort = 6931

// spawnFn is the seam used by ApplyPlan to launch the detached updater. It
// defaults to the real spawnDetachedUpdater and is overridden in tests so the
// plan→apply contract can be asserted without forking a process.
var spawnFn = spawnDetachedUpdater

// inProgressWatchdog caps how long the in-process inProgress flag is trusted
// before being force-cleared. It is a ceiling, not a completion signal — the
// real cross-process guard is the updater lockfile. It MUST exceed the detached
// updater's total context timeout (5 minutes in run()); otherwise the watchdog
// clears inProgress while the updater is still downloading and the reconcile
// loop re-enters ApplyPlan and forks redundant subprocesses that each lose the
// lockfile race and exit. 6 minutes leaves headroom over the 5-minute updater.
const inProgressWatchdog = 6 * time.Minute

// ─── Provider ────────────────────────────────────────────────────────────────

// Provider implements reconcile.Reconcilable for the "self-update" resource type.
//
// Thread safety: mu guards all mutable fields. The reconcile harness sequences
// LoadConfig/FetchLive/ComputePlan/ApplyPlan, but Health() is called
// concurrently by the foveated-context block, so the lock is load-bearing.
type Provider struct {
	mu         sync.Mutex
	resolver   *ReleaseResolver
	root       string
	port       int
	inProgress bool
	status     reconcile.ResourceStatus
}

// New constructs a Provider with a fresh resolver and an unknown/missing status.
func New() *Provider {
	return &Provider{
		resolver: &ReleaseResolver{},
		port:     defaultHealthPort,
		status:   reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthMissing),
	}
}

// global is the singleton registered with the reconcile registry. SetWorkspaceRoot
// and SetPort mutate it before the first reconcile tick.
var global = New()

// Registered returns the singleton provider for registration in the daemon init().
func Registered() *Provider { return global }

// SetWorkspaceRoot injects the workspace root so LoadConfig and the updater log
// path can be resolved. Called from daemon.SetWorkspaceRoot at boot.
func SetWorkspaceRoot(root string) {
	global.mu.Lock()
	global.root = root
	global.mu.Unlock()
}

// SetPort injects the health-endpoint port the spawned updater should poll.
func SetPort(port int) {
	if port <= 0 {
		return
	}
	global.mu.Lock()
	global.port = port
	global.mu.Unlock()
}

// Type returns the resource type identifier.
func (p *Provider) Type() string { return "self-update" }

// ─── Config / Live bundles ───────────────────────────────────────────────────

// selfUpdateLive is the FetchLive output. It carries the running version plus
// either the resolved target, a Disabled marker, or a soft fetch error string.
type selfUpdateLive struct {
	Disabled       bool
	RunningVersion string
	FetchErr       string
	Target         *resolvedRelease
}

// ─── LoadConfig ──────────────────────────────────────────────────────────────

// LoadConfig reads <root>/.cog/config/self-update.yaml. An absent file yields
// the disabled default (no error, no GitHub traffic).
func (p *Provider) LoadConfig(root string) (any, error) {
	p.mu.Lock()
	p.root = root
	p.mu.Unlock()
	return loadSelfUpdateConfig(root)
}

// ─── FetchLive ───────────────────────────────────────────────────────────────

// FetchLive resolves the live target release. When disabled it returns
// immediately with no network traffic. A resolver error is soft: it is recorded
// in FetchErr rather than failing the reconcile cycle. The throttle lives in
// ReleaseResolver.Resolve.
func (p *Provider) FetchLive(ctx context.Context, config any) (any, error) {
	cfg, ok := config.(*SelfUpdateConfig)
	if !ok {
		return nil, fmt.Errorf("self-update: FetchLive expected *SelfUpdateConfig, got %T", config)
	}

	running := currentRunningVersion()

	if !cfg.Enabled {
		p.setStatus(reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusSynced,
			Health:    reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   "self-update disabled",
		})
		return &selfUpdateLive{Disabled: true, RunningVersion: running}, nil
	}

	rel, err := p.resolver.Resolve(ctx, cfg)
	if err != nil {
		p.setStatus(reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("cannot resolve target: %v", err),
		})
		return &selfUpdateLive{RunningVersion: running, FetchErr: err.Error()}, nil
	}

	// Refresh status knowledge (not a mutation gate — purely informational).
	pinned := cfg.Pin != ""
	p.refreshStatusFromLive(cfg, running, rel, pinned)
	return &selfUpdateLive{RunningVersion: running, Target: rel}, nil
}

// ─── ComputePlan (PURE) ──────────────────────────────────────────────────────

// ComputePlan compares the running version against the resolved target and emits
// exactly one Action (skip or update). It performs NO I/O and mutates NO provider
// state — purity is verified by a unit test. The destructive work is deferred to
// ApplyPlan, which only spawns the detached updater.
//
// Gates (in order):
//
//	B  disabled                                   → skip(disabled)
//	C  fetch error                                → skip(fetch_error) + warning
//	D  running version "" (dev/unknown)           → skip(dev_build)   [never auto-update dev]
//	E  versionEqual(target, running)              → skip(up_to_date)
//	F  !pinned && !versionAfter(target, running)  → skip(running_ahead) [no downgrade]
//	   pinned                                       → allow any direction (operator override)
//	else                                          → update{from,to,auto_apply,os,arch,repo,pinned}
func (p *Provider) ComputePlan(config any, live any, _ *reconcile.State) (*reconcile.Plan, error) {
	cfg, ok := config.(*SelfUpdateConfig)
	if !ok {
		return nil, fmt.Errorf("self-update: ComputePlan expected *SelfUpdateConfig, got %T", config)
	}
	lv, ok := live.(*selfUpdateLive)
	if !ok {
		return nil, fmt.Errorf("self-update: ComputePlan expected *selfUpdateLive, got %T", live)
	}

	plan := &reconcile.Plan{
		ResourceType: "self-update",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath:   selfUpdateConfigPath(cfg.Root()),
	}

	skip := func(reason, name string, details map[string]any) (*reconcile.Plan, error) {
		d := map[string]any{"reason": reason}
		for k, v := range details {
			d[k] = v
		}
		plan.Actions = append(plan.Actions, reconcile.Action{
			Action:       reconcile.ActionSkip,
			ResourceType: "self-update",
			Name:         name,
			Details:      d,
		})
		plan.Summary.Skipped++
		return plan, nil
	}

	// GATE B — disabled.
	if lv.Disabled {
		return skip("disabled", "cogos", nil)
	}

	// GATE C — fetch error (soft).
	if lv.FetchErr != "" {
		plan.Warnings = append(plan.Warnings, "self-update: "+lv.FetchErr)
		return skip("fetch_error", "cogos", map[string]any{"error": lv.FetchErr})
	}

	running := lv.RunningVersion
	var target string
	if lv.Target != nil {
		target = lv.Target.Tag
	}
	pinned := cfg.Pin != ""

	// GATE D — dev/unknown build never auto-updated (runs BEFORE the pin branch
	// so a dev build with a pin set stays inert on the auto path).
	if normVersion(running) == "" {
		return skip("dev_build", "cogos", map[string]any{"running": running, "target": target})
	}

	// GATE E — already on target.
	if versionEqual(target, running) {
		return skip("up_to_date", "cogos", map[string]any{"running": running, "target": target})
	}

	// GATE F — no downgrade on the auto path unless pinned.
	if !pinned && !versionAfter(target, running) {
		return skip("running_ahead", "cogos", map[string]any{"running": running, "target": target})
	}

	// Update action.
	plan.Actions = append(plan.Actions, reconcile.Action{
		Action:       reconcile.ActionUpdate,
		ResourceType: "self-update",
		Name:         "cogos",
		Details: map[string]any{
			"from":       running,
			"to":         target,
			"auto_apply": cfg.AutoApply,
			"os":         runtime.GOOS,
			"arch":       runtime.GOARCH,
			"repo":       cfg.Repo,
			"pinned":     pinned,
		},
	})
	plan.Summary.Updates++
	return plan, nil
}

// ─── ApplyPlan ───────────────────────────────────────────────────────────────

// ApplyPlan executes update actions by spawning the detached updater. It never
// swaps the binary in-process.
//
// Gates:
//
//	G  auto_apply == false   → skipped (notify-only)
//	H  GOOS != "darwin"      → skipped (auto-apply unsupported; notify-only)
//	I  inProgress            → skipped (in-process dup-spawn guard)
//	else                     → spawn detached updater, return succeeded
func (p *Provider) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	results := make([]reconcile.Result, 0, len(plan.Actions))

	for _, action := range plan.Actions {
		if action.Action != reconcile.ActionUpdate {
			continue
		}
		toTag, _ := action.Details["to"].(string)
		repo, _ := action.Details["repo"].(string)
		autoApply, _ := action.Details["auto_apply"].(bool)

		// GATE G — notify-only when auto_apply is off.
		if !autoApply {
			p.setStatus(reconcile.ResourceStatus{
				Sync:      reconcile.SyncStatusOutOfSync,
				Health:    reconcile.HealthHealthy,
				Operation: reconcile.OperationIdle,
				Message:   fmt.Sprintf("update available: %s (auto_apply off)", toTag),
			})
			results = append(results, skipResult("cogos", "auto_apply disabled — notify only"))
			continue
		}

		// GATE H — auto-apply unsupported off darwin.
		if runtime.GOOS != "darwin" {
			p.setStatus(reconcile.ResourceStatus{
				Sync:      reconcile.SyncStatusOutOfSync,
				Health:    reconcile.HealthHealthy,
				Operation: reconcile.OperationIdle,
				Message:   fmt.Sprintf("update available: %s (auto-apply unsupported on %s)", toTag, runtime.GOOS),
			})
			results = append(results, skipResult("cogos", "auto-apply unsupported on "+runtime.GOOS))
			continue
		}

		// GATE I — in-process dup-spawn guard.
		p.mu.Lock()
		if p.inProgress {
			p.mu.Unlock()
			results = append(results, skipResult("cogos", "update already in progress"))
			continue
		}
		p.inProgress = true
		root := p.root
		port := p.port
		p.mu.Unlock()

		p.setStatus(reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthProgressing,
			Operation: reconcile.OperationSyncing,
			Message:   fmt.Sprintf("updating to %s", toTag),
		})

		if err := spawnFn(root, toTag, repo, port); err != nil {
			p.mu.Lock()
			p.inProgress = false
			p.mu.Unlock()
			p.setStatus(reconcile.ResourceStatus{
				Sync:      reconcile.SyncStatusOutOfSync,
				Health:    reconcile.HealthDegraded,
				Operation: reconcile.OperationIdle,
				Message:   fmt.Sprintf("failed to spawn updater: %v", err),
			})
			results = append(results, reconcile.Result{
				Phase:  "apply",
				Action: string(reconcile.ActionUpdate),
				Name:   "cogos",
				Status: reconcile.ApplyFailed,
				Error:  err.Error(),
			})
			continue
		}

		// The updater now owns the swap+restart. inProgress is cleared either by
		// the daemon restart (fresh process) or by this watchdog ceiling. The
		// real cross-process guard is the updater lockfile, not this flag.
		p.armWatchdog()

		results = append(results, reconcile.Result{
			Phase:  "apply",
			Action: string(reconcile.ActionUpdate),
			Name:   "cogos",
			Status: reconcile.ApplySucceeded,
		})
	}
	return results, nil
}

// armWatchdog clears inProgress after inProgressWatchdog if the daemon has not
// been restarted out from under us by then.
func (p *Provider) armWatchdog() {
	time.AfterFunc(inProgressWatchdog, func() {
		p.mu.Lock()
		p.inProgress = false
		p.mu.Unlock()
	})
}

func skipResult(name, reason string) reconcile.Result {
	return reconcile.Result{
		Phase:  "apply",
		Action: string(reconcile.ActionUpdate),
		Name:   name,
		Status: reconcile.ApplySkipped,
		Error:  reason,
	}
}

// ─── BuildState ──────────────────────────────────────────────────────────────

// BuildState projects the current self-update knowledge into reconcile State.
func (p *Provider) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	lv, ok := live.(*selfUpdateLive)
	if !ok {
		return nil, fmt.Errorf("self-update: BuildState expected *selfUpdateLive, got %T", live)
	}

	state := reconcile.NewState("self-update")
	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	}
	state.GeneratedAt = time.Now().UTC().Format(time.RFC3339)

	target := ""
	if lv.Target != nil {
		target = lv.Target.Tag
	}
	externalID := target
	if externalID == "" {
		externalID = lv.RunningVersion
	}

	p.mu.Lock()
	inProgress := p.inProgress
	p.mu.Unlock()

	state.Resources = append(state.Resources, reconcile.Resource{
		Address:    "self-update.cogos",
		Type:       "self-update",
		Mode:       reconcile.ModeManaged,
		Name:       "cogos",
		ExternalID: externalID,
		Attributes: map[string]any{
			"running_version": lv.RunningVersion,
			"target_tag":      target,
			"disabled":        lv.Disabled,
			"in_progress":     inProgress,
		},
		LastRefreshed: time.Now().UTC().Format(time.RFC3339),
	})
	return state, nil
}

// ─── Health ──────────────────────────────────────────────────────────────────

// Health returns the last-computed three-axis status. Status is updated only in
// FetchLive (knowledge refresh) and ApplyPlan (progressing/failed) — never in the
// pure ComputePlan.
func (p *Provider) Health() reconcile.ResourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.root == "" {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "workspace not configured",
		}
	}
	if p.inProgress {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthProgressing,
			Operation: reconcile.OperationSyncing,
			Message:   p.status.Message,
		}
	}
	return p.status
}

// ─── status helpers ──────────────────────────────────────────────────────────

func (p *Provider) setStatus(s reconcile.ResourceStatus) {
	p.mu.Lock()
	p.status = s
	p.mu.Unlock()
}

// refreshStatusFromLive computes the informational Health status from a resolved
// target. Called only from FetchLive — never from ComputePlan.
func (p *Provider) refreshStatusFromLive(cfg *SelfUpdateConfig, running string, rel *resolvedRelease, pinned bool) {
	target := rel.Tag

	switch {
	case normVersion(running) == "":
		p.setStatus(reconcile.ResourceStatus{
			Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("dev build (%s); self-update inert", running),
		})
	case versionEqual(target, running):
		p.setStatus(reconcile.ResourceStatus{
			Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("running %s is current", running),
		})
	case !pinned && !versionAfter(target, running):
		p.setStatus(reconcile.ResourceStatus{
			Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("running %s, stable is %s", running, target),
		})
	case cfg.AutoApply && runtime.GOOS == "darwin":
		p.setStatus(reconcile.ResourceStatus{
			Sync: reconcile.SyncStatusOutOfSync, Health: reconcile.HealthProgressing,
			Operation: reconcile.OperationSyncing,
			Message:   fmt.Sprintf("update to %s queued", target),
		})
	default:
		p.setStatus(reconcile.ResourceStatus{
			Sync: reconcile.SyncStatusOutOfSync, Health: reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("update available: %s (auto_apply off)", target),
		})
	}
}

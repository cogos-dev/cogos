// service_provider.go — Reconciliation provider for container services.
//
// Implements Reconcilable to manage container lifecycle through the standard
// plan/apply/state reconciliation loop. Compares declared ServiceCRDs against
// live Docker container state and produces create/update/skip actions.
//
// State file: .cog/config/services/.state.json

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// healthCheckClient is a dedicated HTTP client for service health checks.
var healthCheckClient = &http.Client{Timeout: 15 * time.Second}

// checkServiceHealth performs an HTTP health check against a ServiceCRD's
// declared health endpoint. Returns "healthy", "unhealthy", "error", or "—"
// when no health endpoint is configured.
func checkServiceHealth(ctx context.Context, crd *ServiceCRD) string {
	if crd.Spec.Health.Endpoint == "" || crd.Spec.Health.Port == 0 {
		return "—"
	}

	timeout, _ := ParseServiceDuration(crd.Spec.Health.Timeout)
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	healthCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("http://localhost:%d%s", crd.Spec.Health.Port, crd.Spec.Health.Endpoint)
	req, err := http.NewRequestWithContext(healthCtx, "GET", url, nil)
	if err != nil {
		return "error"
	}

	resp, err := healthCheckClient.Do(req)
	if err != nil {
		return "unhealthy"
	}
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return "healthy"
	}
	return "unhealthy"
}

// ServiceProvider implements Reconcilable for container service management.
type ServiceProvider struct {
	mu      sync.Mutex
	root    string
	runtime *DockerClient

	// Health() result cache. computeHealth issues a Docker Ping + one
	// ContainerList per declared service; Health() is called on every
	// /v1/infer, every autonomic tick, and after every reconcile cycle, so the
	// live probe is memoized for serviceHealthTTL. Guarded by mu.
	cachedHealth ResourceStatus
	healthAt     time.Time
	healthValid  bool
}

// serviceHealthTTL bounds how stale a cached Health() result may be. Service
// liveness changes are picked up within this window (and at the latest by the
// next ~30s reconcile cycle), which is well inside the Reconcilable contract's
// "Health() must be near-zero-cost" expectation.
const serviceHealthTTL = 15 * time.Second

func init() {
	RegisterProvider("service", &ServiceProvider{})
}

func (s *ServiceProvider) Type() string { return "service" }

// ─── LoadConfig ─────────────────────────────────────────────────────────────────

// LoadConfig loads all service CRD definitions from .cog/config/services/.
func (s *ServiceProvider) LoadConfig(root string) (any, error) {
	s.mu.Lock()
	s.root = root
	s.mu.Unlock()
	crds, err := ListServiceCRDs(root)
	if err != nil {
		return nil, fmt.Errorf("service provider: load config: %w", err)
	}
	if crds == nil {
		crds = []ServiceCRD{}
	}
	return crds, nil
}

// ─── FetchLive ──────────────────────────────────────────────────────────────────

// ServiceLiveState holds the runtime state of managed services across both
// execution modes. `Available` reports whether the container runtime is up;
// local-mode reconciliation works regardless.
type ServiceLiveState struct {
	Available  bool                      // container runtime reachable
	Containers map[string]*ContainerInfo // keyed by service name
	LocalProcs map[string]*LocalProcess  // keyed by service name
}

// FetchLive queries both the Docker daemon and local PID files for the
// current live state of all services.
func (s *ServiceProvider) FetchLive(ctx context.Context, config any) (any, error) {
	s.mu.Lock()
	if s.runtime == nil {
		s.runtime = NewDockerClient("")
	}
	root := s.root
	s.mu.Unlock()

	live := &ServiceLiveState{
		Containers: make(map[string]*ContainerInfo),
		LocalProcs: make(map[string]*LocalProcess),
	}

	// Build a name→CRD lookup so ListLocalProcessesWithCRDs can adopt legacy
	// PID files whose argv matches the current CRD's expected command. The
	// reconcile harness passes the LoadConfig output through as `config`;
	// when it's the expected shape we forward it, otherwise we fall back to
	// the strict (non-adopting) scan.
	var crdMap map[string]*ServiceCRD
	if crds, ok := config.([]ServiceCRD); ok {
		crdMap = make(map[string]*ServiceCRD, len(crds))
		for i := range crds {
			crdMap[crds[i].Metadata.Name] = &crds[i]
		}
	}

	// Local processes are discoverable whether or not Docker is up.
	if procs, err := ListLocalProcessesWithCRDs(root, crdMap); err == nil {
		live.LocalProcs = procs
	} else {
		log.Printf("[service] warning: list local processes: %v", err)
	}

	if err := s.runtime.Ping(ctx); err != nil {
		// Runtime not available — return with locals populated but no containers.
		live.Available = false
		return live, nil
	}
	live.Available = true

	entries, err := s.runtime.ListManagedContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("service provider: list containers: %w", err)
	}

	for _, entry := range entries {
		serviceName := entry.Labels[labelService]
		if serviceName == "" {
			continue
		}
		info, err := s.runtime.ContainerInspect(ctx, entry.ID)
		if err != nil {
			log.Printf("[service] warning: inspect %s failed: %v", entry.ID[:12], err)
			continue
		}
		live.Containers[serviceName] = info
	}

	return live, nil
}

// ─── Execution-mode selection ───────────────────────────────────────────────────

// serviceMode returns the effective execution mode for a CRD given current
// runtime availability. Docker is preferred when both an image is declared
// and the runtime is reachable; local is the fallback. "none" indicates the
// CRD cannot be executed in the current environment.
const (
	modeDocker = "docker"
	modeLocal  = "local"
	modeNone   = "none"
)

func serviceMode(crd *ServiceCRD, runtimeAvailable bool) string {
	if crd.Spec.Image != "" && runtimeAvailable {
		return modeDocker
	}
	if crd.Spec.Local != nil {
		return modeLocal
	}
	return modeNone
}

// ─── ComputePlan ────────────────────────────────────────────────────────────────

// ComputePlan compares declared CRDs against live state (containers + local
// processes) and produces per-service create/update/skip/delete actions.
// Each CRD resolves to exactly one mode; orphans in either mode get cleaned up.
func (s *ServiceProvider) ComputePlan(config any, live any, state *ReconcileState) (*ReconcilePlan, error) {
	crds := config.([]ServiceCRD)
	liveState := live.(*ServiceLiveState)

	plan := &ReconcilePlan{
		ResourceType: "service",
		GeneratedAt:  nowISO(),
		ConfigPath:   ".cog/config/services/",
	}

	if !liveState.Available {
		plan.Warnings = append(plan.Warnings, "container runtime not available — docker-mode services will be skipped")
	}

	// Sort CRDs by name for deterministic output
	sort.Slice(crds, func(i, j int) bool {
		return crds[i].Metadata.Name < crds[j].Metadata.Name
	})

	seenContainer := make(map[string]bool)
	seenLocal := make(map[string]bool)

	for _, crd := range crds {
		name := crd.Metadata.Name
		mode := serviceMode(&crd, liveState.Available)

		switch mode {
		case modeDocker:
			seenContainer[name] = true
			s.planDockerService(plan, &crd, liveState.Containers[name])
		case modeLocal:
			seenLocal[name] = true
			s.planLocalService(plan, &crd, liveState.LocalProcs[name])
		case modeNone:
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionSkip,
				ResourceType: "service",
				Name:         name,
				Details:      map[string]any{"reason": "no image and no local spec (or runtime unavailable)"},
			})
			plan.Summary.Skipped++
		}
	}

	// Orphaned managed containers (not declared by any CRD as docker-mode).
	for name, info := range liveState.Containers {
		if !seenContainer[name] {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionDelete,
				ResourceType: "service",
				Name:         name,
				Details: map[string]any{
					"reason":       "no matching service CRD (docker)",
					"container_id": info.ID[:12],
					"mode":         modeDocker,
				},
			})
			plan.Summary.Deletes++
		}
	}

	// Orphaned local processes — includes "declared CRD is docker-mode but
	// a local process is running under the same name" (stale from a prior
	// mode), which we want to stop.
	for name, proc := range liveState.LocalProcs {
		if !seenLocal[name] {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionDelete,
				ResourceType: "service",
				Name:         name,
				Details: map[string]any{
					"reason": "no matching service CRD (local)",
					"pid":    proc.PID,
					"mode":   modeLocal,
				},
			})
			plan.Summary.Deletes++
		}
	}

	return plan, nil
}

// planDockerService produces the action for a docker-mode CRD.
func (s *ServiceProvider) planDockerService(plan *ReconcilePlan, crd *ServiceCRD, info *ContainerInfo) {
	name := crd.Metadata.Name
	if info == nil {
		plan.Actions = append(plan.Actions, ReconcileAction{
			Action:       ActionCreate,
			ResourceType: "service",
			Name:         name,
			Details: map[string]any{
				"reason": "no container found",
				"image":  crd.Spec.Image,
				"mode":   modeDocker,
			},
		})
		plan.Summary.Creates++
		return
	}
	drifted, reasons := detectServiceDrift(crd, info)
	if drifted {
		plan.Actions = append(plan.Actions, ReconcileAction{
			Action:       ActionUpdate,
			ResourceType: "service",
			Name:         name,
			Details: map[string]any{
				"reason":       "drift detected: " + joinReasons(reasons),
				"container_id": info.ID[:12],
				"mode":         modeDocker,
			},
		})
		plan.Summary.Updates++
		return
	}
	plan.Actions = append(plan.Actions, ReconcileAction{
		Action:       ActionSkip,
		ResourceType: "service",
		Name:         name,
		Details: map[string]any{
			"reason":       "in sync",
			"container_id": info.ID[:12],
			"status":       info.State.Status,
			"mode":         modeDocker,
		},
	})
	plan.Summary.Skipped++
}

// planLocalService produces the action for a local-mode CRD.
func (s *ServiceProvider) planLocalService(plan *ReconcilePlan, crd *ServiceCRD, proc *LocalProcess) {
	name := crd.Metadata.Name
	if proc == nil || !proc.Running {
		plan.Actions = append(plan.Actions, ReconcileAction{
			Action:       ActionCreate,
			ResourceType: "service",
			Name:         name,
			Details: map[string]any{
				"reason": "no local process running",
				"mode":   modeLocal,
			},
		})
		plan.Summary.Creates++
		return
	}
	// Drift: compare cmdHash of declared spec vs recorded spec.
	expected := localCmdHash(crd.Spec.Local, resolveWorkdir(s.root, crd.Spec.Local))
	if expected != proc.CmdHash {
		plan.Actions = append(plan.Actions, ReconcileAction{
			Action:       ActionUpdate,
			ResourceType: "service",
			Name:         name,
			Details: map[string]any{
				"reason": "declared spec differs from running cmd_hash",
				"pid":    proc.PID,
				"mode":   modeLocal,
			},
		})
		plan.Summary.Updates++
		return
	}
	plan.Actions = append(plan.Actions, ReconcileAction{
		Action:       ActionSkip,
		ResourceType: "service",
		Name:         name,
		Details: map[string]any{
			"reason": "in sync",
			"pid":    proc.PID,
			"mode":   modeLocal,
		},
	})
	plan.Summary.Skipped++
}

// detectServiceDrift compares a CRD against a live container.
func detectServiceDrift(crd *ServiceCRD, info *ContainerInfo) (bool, []string) {
	var reasons []string

	// Check if container is not running
	if !info.State.Running {
		reasons = append(reasons, fmt.Sprintf("container not running (state: %s)", info.State.Status))
	}

	// Check image drift
	liveImage := info.Config.Image
	if !strings.Contains(liveImage, crd.Spec.Image) && !strings.Contains(crd.Spec.Image, liveImage) {
		// Do a more lenient check — sometimes the tag resolves differently
		declaredBase := strings.Split(crd.Spec.Image, ":")[0]
		liveBase := strings.Split(liveImage, ":")[0]
		if declaredBase != liveBase {
			reasons = append(reasons, fmt.Sprintf("image: declared=%s live=%s", crd.Spec.Image, liveImage))
		}
	}

	// Check port bindings
	for _, port := range crd.Spec.Ports {
		proto := port.Protocol
		if proto == "" {
			proto = "tcp"
		}
		key := fmt.Sprintf("%d/%s", port.Container, proto)
		bindings, ok := info.HostConfig.PortBindings[key]
		if !ok || len(bindings) == 0 {
			reasons = append(reasons, fmt.Sprintf("port %d not bound", port.Container))
			continue
		}
		expectedHost := fmt.Sprintf("%d", port.Host)
		if bindings[0].HostPort != expectedHost {
			reasons = append(reasons, fmt.Sprintf("port %d: host=%s expected=%s",
				port.Container, bindings[0].HostPort, expectedHost))
		}
	}

	// Check environment variables
	liveEnv := make(map[string]string)
	for _, e := range info.Config.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			liveEnv[parts[0]] = parts[1]
		}
	}
	for _, e := range crd.Spec.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			if liveVal, ok := liveEnv[parts[0]]; !ok || liveVal != parts[1] {
				reasons = append(reasons, fmt.Sprintf("env %s changed", parts[0]))
			}
		}
	}

	return len(reasons) > 0, reasons
}

// ─── ApplyPlan ──────────────────────────────────────────────────────────────────

// ApplyPlan executes planned service changes, dispatching per action to the
// docker or local handler based on the `mode` detail key attached by the
// planner.
func (s *ServiceProvider) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	s.mu.Lock()
	if s.runtime == nil {
		s.runtime = NewDockerClient("")
	}
	root := s.root
	s.mu.Unlock()

	var results []ReconcileResult
	for _, action := range plan.Actions {
		if action.Action == ActionSkip {
			continue
		}
		mode, _ := action.Details["mode"].(string)
		if mode == "" {
			mode = modeDocker // backward-compat default
		}

		switch mode {
		case modeLocal:
			results = append(results, s.applyLocal(root, action))
		default:
			results = append(results, s.applyDocker(ctx, action))
		}
	}
	return results, nil
}

// applyDocker dispatches create/update/delete for docker-mode services.
func (s *ServiceProvider) applyDocker(ctx context.Context, action ReconcileAction) ReconcileResult {
	switch action.Action {
	case ActionCreate:
		return s.applyCreate(ctx, action.Name)
	case ActionUpdate:
		return s.applyUpdate(ctx, action.Name)
	case ActionDelete:
		return s.applyDelete(ctx, action.Name)
	}
	return ReconcileResult{Phase: "service", Action: string(action.Action), Name: action.Name, Status: ApplySkipped}
}

// applyLocal dispatches create/update/delete for local-mode services.
func (s *ServiceProvider) applyLocal(root string, action ReconcileAction) ReconcileResult {
	switch action.Action {
	case ActionCreate:
		return s.applyLocalCreate(root, action.Name)
	case ActionUpdate:
		// Stop then recreate; cheapest way to pick up new argv/env.
		if err := LocalStop(root, action.Name); err != nil {
			return ReconcileResult{
				Phase: "service", Action: "update", Name: action.Name,
				Status: ApplyFailed, Error: fmt.Sprintf("stop before update: %v", err),
			}
		}
		result := s.applyLocalCreate(root, action.Name)
		result.Action = "update"
		return result
	case ActionDelete:
		if err := LocalStop(root, action.Name); err != nil {
			return ReconcileResult{
				Phase: "service", Action: "delete", Name: action.Name,
				Status: ApplyFailed, Error: err.Error(),
			}
		}
		return ReconcileResult{
			Phase: "service", Action: "delete", Name: action.Name,
			Status: ApplySucceeded,
		}
	}
	return ReconcileResult{Phase: "service", Action: string(action.Action), Name: action.Name, Status: ApplySkipped}
}

func (s *ServiceProvider) applyLocalCreate(root, name string) ReconcileResult {
	crd, err := LoadServiceCRD(root, name)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: err.Error(),
		}
	}
	proc, err := LocalStart(root, crd)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: err.Error(),
		}
	}
	return ReconcileResult{
		Phase: "service", Action: "create", Name: name,
		Status: ApplySucceeded, CreatedID: fmt.Sprintf("pid:%d", proc.PID),
	}
}

func (s *ServiceProvider) applyCreate(ctx context.Context, name string) ReconcileResult {
	crd, err := LoadServiceCRD(s.root, name)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: err.Error(),
		}
	}

	// Pull image
	if err := s.runtime.ImagePull(ctx, crd.Spec.Image, crd.Spec.Platform, nil); err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("pull failed: %v", err),
		}
	}

	// Build config and create
	config, err := BuildContainerConfig(s.root, crd)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("config: %v", err),
		}
	}

	containerName := ManagedContainerName(name)
	containerID, err := s.runtime.ContainerCreate(ctx, containerName, config)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("create: %v", err),
		}
	}

	// Start
	if err := s.runtime.ContainerStart(ctx, containerID); err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("start: %v", err),
		}
	}

	return ReconcileResult{
		Phase: "service", Action: "create", Name: name,
		Status: ApplySucceeded, CreatedID: containerID,
	}
}

func (s *ServiceProvider) applyUpdate(ctx context.Context, name string) ReconcileResult {
	// Stop and remove existing container
	entry, err := s.runtime.FindManagedContainer(ctx, name)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "update", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("find container: %v", err),
		}
	}
	if entry != nil {
		if err := s.runtime.ContainerStop(ctx, entry.ID, 10); err != nil {
			log.Printf("[service] warning: stop %s before update: %v", entry.ID[:12], err)
		}
		if err := s.runtime.ContainerRemove(ctx, entry.ID, true); err != nil {
			return ReconcileResult{
				Phase: "service", Action: "update", Name: name,
				Status: ApplyFailed, Error: fmt.Sprintf("remove old container: %v", err),
			}
		}
	}

	// Recreate
	result := s.applyCreate(ctx, name)
	result.Action = "update"
	return result
}

func (s *ServiceProvider) applyDelete(ctx context.Context, name string) ReconcileResult {
	entry, err := s.runtime.FindManagedContainer(ctx, name)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "delete", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("find container: %v", err),
		}
	}
	if entry == nil {
		return ReconcileResult{
			Phase: "service", Action: "delete", Name: name,
			Status: ApplySucceeded,
		}
	}

	if entry.State == "running" {
		if err := s.runtime.ContainerStop(ctx, entry.ID, 10); err != nil {
			log.Printf("[service] warning: stop %s before delete: %v", entry.ID[:12], err)
		}
	}
	if err := s.runtime.ContainerRemove(ctx, entry.ID, true); err != nil {
		return ReconcileResult{
			Phase: "service", Action: "delete", Name: name,
			Status: ApplyFailed, Error: err.Error(),
		}
	}

	return ReconcileResult{
		Phase: "service", Action: "delete", Name: name,
		Status: ApplySucceeded,
	}
}

// ─── BuildState ─────────────────────────────────────────────────────────────────

// BuildState constructs reconcile state from live container data.
func (s *ServiceProvider) BuildState(config any, live any, existing *ReconcileState) (*ReconcileState, error) {
	liveState := live.(*ServiceLiveState)

	state := &ReconcileState{
		Version:      1,
		ResourceType: "service",
		GeneratedAt:  nowISO(),
	}

	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	} else {
		state.Lineage = "service-" + nowISO()
	}

	for name, info := range liveState.Containers {
		health := "unknown"
		if info.State.Health != nil {
			health = info.State.Health.Status
		}

		resource := ReconcileResource{
			Address:       "service." + name,
			Type:          "container",
			Mode:          ModeManaged,
			ExternalID:    info.ID,
			Name:          name,
			LastRefreshed: nowISO(),
			Attributes: map[string]any{
				"image":   info.Config.Image,
				"status":  info.State.Status,
				"running": info.State.Running,
				"health":  health,
				"mode":    modeDocker,
			},
		}
		state.Resources = append(state.Resources, resource)
	}

	for name, proc := range liveState.LocalProcs {
		resource := ReconcileResource{
			Address:       "service." + name,
			Type:          "local",
			Mode:          ModeManaged,
			ExternalID:    fmt.Sprintf("pid:%d", proc.PID),
			Name:          name,
			LastRefreshed: nowISO(),
			Attributes: map[string]any{
				"pid":        proc.PID,
				"started_at": proc.StartedAt,
				"workdir":    proc.Workdir,
				"cmd_hash":   proc.CmdHash,
				"running":    proc.Running,
				"log_path":   proc.LogPath,
				"mode":       modeLocal,
			},
		}
		state.Resources = append(state.Resources, resource)
	}

	// Sort by address for deterministic output
	sort.Slice(state.Resources, func(i, j int) bool {
		return state.Resources[i].Address < state.Resources[j].Address
	})

	return state, nil
}

// ─── Health ─────────────────────────────────────────────────────────────────────

// Health returns the three-axis status of the service subsystem, served from a
// short-TTL cache so the live Docker probe runs at most once per
// serviceHealthTTL no matter how many callers hit it.
func (s *ServiceProvider) Health() ResourceStatus {
	s.mu.Lock()
	if s.healthValid && time.Since(s.healthAt) < serviceHealthTTL {
		h := s.cachedHealth
		s.mu.Unlock()
		return h
	}
	s.mu.Unlock()

	h := s.computeHealth()

	s.mu.Lock()
	s.cachedHealth = h
	s.healthAt = time.Now()
	s.healthValid = true
	s.mu.Unlock()
	return h
}

// computeHealth performs the live probe: a Docker Ping plus a per-service
// container/PID-liveness check. Called by Health() at most once per
// serviceHealthTTL.
func (s *ServiceProvider) computeHealth() ResourceStatus {
	s.mu.Lock()
	root := s.root
	s.mu.Unlock()
	if root == "" {
		var err error
		root, _, err = ResolveWorkspace()
		if err != nil {
			return ResourceStatus{
				Sync: SyncStatusUnknown, Health: HealthMissing, Operation: OperationIdle,
				Message: "workspace not found",
			}
		}
	}

	crds, err := ListServiceCRDs(root)
	if err != nil || len(crds) == 0 {
		return NewResourceStatus(SyncStatusSynced, HealthHealthy)
	}

	runtime := NewDockerClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runtimeUp := runtime.Ping(ctx) == nil

	down := 0
	for _, crd := range crds {
		switch serviceMode(&crd, runtimeUp) {
		case modeDocker:
			entry, _ := runtime.FindManagedContainer(ctx, crd.Metadata.Name)
			if entry == nil || entry.State != "running" {
				down++
			}
		case modeLocal:
			proc, _ := LocalStatus(root, crd.Metadata.Name)
			if proc == nil || !proc.Running {
				down++
			}
		case modeNone:
			down++
		}
	}

	if down == len(crds) {
		return ResourceStatus{
			Sync: SyncStatusOutOfSync, Health: HealthDegraded, Operation: OperationIdle,
			Message: fmt.Sprintf("all %d services down", down),
		}
	}
	if down > 0 {
		return ResourceStatus{
			Sync: SyncStatusOutOfSync, Health: HealthDegraded, Operation: OperationIdle,
			Message: fmt.Sprintf("%d/%d services down", down, len(crds)),
		}
	}

	return NewResourceStatus(SyncStatusSynced, HealthHealthy)
}

// ─── Health Monitor ─────────────────────────────────────────────────────────────

const (
	BlockServiceHealth       = "service.health"
	BlockServiceCapabilities = "service.capabilities"
	serviceHealthBusID       = "bus_chat_system_capabilities"
)

// ServiceHealthMonitor periodically polls service health and emits bus events.
type ServiceHealthMonitor struct {
	root    string
	mgr     *busSessionManager
	runtime *DockerClient
	stopCh  chan struct{}
	done   chan struct{}
}

// NewServiceHealthMonitor creates a new health monitor.
func NewServiceHealthMonitor(root string, mgr *busSessionManager) *ServiceHealthMonitor {
	return &ServiceHealthMonitor{
		root:    root,
		mgr:     mgr,
		runtime: NewDockerClient(""),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start begins the health monitor polling loop.
func (m *ServiceHealthMonitor) Start() {
	go m.run()
}

// Stop halts the health monitor.
func (m *ServiceHealthMonitor) Stop() {
	close(m.stopCh)
	<-m.done
}

func (m *ServiceHealthMonitor) run() {
	defer close(m.done)

	// Initial check after 15s delay
	select {
	case <-time.After(15 * time.Second):
	case <-m.stopCh:
		return
	}

	m.checkAndEmit()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.checkAndEmit()
		case <-m.stopCh:
			return
		}
	}
}

func (m *ServiceHealthMonitor) checkAndEmit() {
	crds, err := ListServiceCRDs(m.root)
	if err != nil || len(crds) == 0 {
		return
	}

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	runtimeUp := m.runtime.Ping(pingCtx) == nil

	for _, crd := range crds {
		svcCtx, svcCancel := context.WithTimeout(context.Background(), 10*time.Second)

		status := "not_running"
		health := "unknown"
		mode := serviceMode(&crd, runtimeUp)

		switch mode {
		case modeDocker:
			entry, _ := m.runtime.FindManagedContainer(svcCtx, crd.Metadata.Name)
			if entry != nil {
				status = entry.State
				if entry.State == "running" {
					health = checkServiceHealth(svcCtx, &crd)
				}
			}
		case modeLocal:
			proc, _ := LocalStatus(m.root, crd.Metadata.Name)
			if proc != nil && proc.Running {
				status = "running"
				health = checkServiceHealth(svcCtx, &crd)
			}
		}
		svcCancel()

		if m.mgr != nil {
			payload := map[string]interface{}{
				"service": crd.Metadata.Name,
				"status":  status,
				"health":  health,
				"image":   crd.Spec.Image,
				"mode":    mode,
			}
			if _, err := m.mgr.appendBusEvent(serviceHealthBusID, BlockServiceHealth, "kernel:cogos", payload); err != nil {
				log.Printf("[svc-health] emit event for %s: %v", crd.Metadata.Name, err)
			}
		}
	}
}

// ─── Service Capability Advertiser ──────────────────────────────────────────────

// AdvertiseServiceCapabilities posts service.capabilities events on the bus
// for services with spec.bus.advertise == true.
func AdvertiseServiceCapabilities(root string, mgr *busSessionManager) error {
	crds, err := ListServiceCRDs(root)
	if err != nil {
		return fmt.Errorf("advertise service capabilities: %w", err)
	}

	for _, crd := range crds {
		if !crd.Spec.Bus.Advertise || len(crd.Spec.Tools) == 0 {
			continue
		}

		tools := make([]map[string]interface{}, len(crd.Spec.Tools))
		for i, t := range crd.Spec.Tools {
			tools[i] = map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"endpoint":    t.Endpoint,
				"method":      t.Method,
			}
		}

		ports := make([]map[string]interface{}, len(crd.Spec.Ports))
		for i, p := range crd.Spec.Ports {
			ports[i] = map[string]interface{}{
				"host":      p.Host,
				"container": p.Container,
				"protocol":  p.Protocol,
			}
		}

		payload := map[string]interface{}{
			"service":      crd.Metadata.Name,
			"image":        crd.Spec.Image,
			"tools":        tools,
			"ports":        ports,
			"advertisedAt": time.Now().UTC().Format(time.RFC3339Nano),
		}

		mgr.appendBusEvent(serviceHealthBusID, BlockServiceCapabilities, "kernel:cogos", payload)
		log.Printf("[svc-cap] advertised service=%s tools=%d", crd.Metadata.Name, len(crd.Spec.Tools))
	}
	return nil
}

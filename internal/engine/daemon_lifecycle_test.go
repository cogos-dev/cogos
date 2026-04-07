package engine

import (
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"
)

type fakeRuntime struct {
	statusByID map[string]ContainerStatus
}

func (f *fakeRuntime) Start(_ string, _ ContainerConfig) (string, error) {
	return "fake-container", nil
}

func (f *fakeRuntime) Stop(_ string) error {
	return nil
}

func (f *fakeRuntime) Status(containerID string) (ContainerStatus, error) {
	if st, ok := f.statusByID[containerID]; ok {
		return st, nil
	}
	return ContainerStatus{}, nil
}

func (f *fakeRuntime) Logs(_ string, _ bool) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeRuntime) Exec(_ string, _ []string) ([]byte, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeRuntime) Pull(_ string) error {
	return nil
}

func TestWorkspaceSlug(t *testing.T) {
	t.Parallel()
	got := workspaceSlug("/Users/slowbro/cog workspace")
	if got != "cog-workspace" {
		t.Fatalf("workspaceSlug = %q; want %q", got, "cog-workspace")
	}
}

func TestSaveLoadDaemonState(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	pid := 4242
	want := &DaemonState{
		Mode:      daemonModeBareMetal,
		Endpoint:  "http://localhost:6931",
		Workspace: root,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		PID:       &pid,
	}
	if err := saveDaemonState(want); err != nil {
		t.Fatalf("saveDaemonState: %v", err)
	}
	got, err := loadDaemonState(root)
	if err != nil {
		t.Fatalf("loadDaemonState: %v", err)
	}
	if got == nil {
		t.Fatal("loadDaemonState returned nil")
	}
	if got.Mode != want.Mode || got.Endpoint != want.Endpoint || got.Workspace != want.Workspace {
		t.Fatalf("loaded state = %+v; want %+v", got, want)
	}
	if got.PID == nil || *got.PID != pid {
		t.Fatalf("loaded pid = %v; want %d", got.PID, pid)
	}
}

func TestPlanStartFresh(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.Port = 6931

	plan, err := planStart(cfg, &fakeRuntime{statusByID: map[string]ContainerStatus{}},
		func(string, time.Duration) (*DaemonHealth, error) { return nil, errors.New("down") },
		defaultDaemonImage)
	if err != nil {
		t.Fatalf("planStart: %v", err)
	}
	if plan.Action != startFresh {
		t.Fatalf("plan.Action = %s; want %s", plan.Action, startFresh)
	}
}

func TestPlanStartReuseSameWorkspace(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.Port = 6931
	state := &DaemonState{
		Mode:      daemonModeContainer,
		Endpoint:  endpointForPort(cfg.Port),
		Container: containerNameForWorkspace(root),
		Workspace: root,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Image:     defaultDaemonImage,
	}
	if err := saveDaemonState(state); err != nil {
		t.Fatalf("saveDaemonState: %v", err)
	}

	plan, err := planStart(cfg, &fakeRuntime{},
		func(string, time.Duration) (*DaemonHealth, error) {
			return &DaemonHealth{Status: "ok", Workspace: root}, nil
		},
		defaultDaemonImage)
	if err != nil {
		t.Fatalf("planStart: %v", err)
	}
	if plan.Action != startReuse {
		t.Fatalf("plan.Action = %s; want %s", plan.Action, startReuse)
	}
}

func TestPlanStartConflictDifferentWorkspace(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.Port = 6931

	plan, err := planStart(cfg, &fakeRuntime{},
		func(string, time.Duration) (*DaemonHealth, error) {
			return &DaemonHealth{Status: "ok", Workspace: filepath.Join(root, "other")}, nil
		},
		defaultDaemonImage)
	if err != nil {
		t.Fatalf("planStart: %v", err)
	}
	if plan.Action != startConflict {
		t.Fatalf("plan.Action = %s; want %s", plan.Action, startConflict)
	}
}

func TestPlanStartAdoptsExistingContainerWithoutStateFile(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.Port = 6931
	name := containerNameForWorkspace(root)

	plan, err := planStart(cfg, &fakeRuntime{statusByID: map[string]ContainerStatus{
		name: {Exists: true, Running: true, Status: "running"},
	}},
		func(string, time.Duration) (*DaemonHealth, error) { return nil, errors.New("down") },
		defaultDaemonImage)
	if err != nil {
		t.Fatalf("planStart: %v", err)
	}
	if plan.Action != startAdopt {
		t.Fatalf("plan.Action = %s; want %s", plan.Action, startAdopt)
	}
	if plan.AdoptState == nil || plan.AdoptState.Container != name {
		t.Fatalf("plan.AdoptState = %+v; want container %s", plan.AdoptState, name)
	}
}

func TestPlanStartReclaimsStaleState(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.Port = 6931
	name := containerNameForWorkspace(root)
	state := &DaemonState{
		Mode:      daemonModeContainer,
		Endpoint:  endpointForPort(cfg.Port),
		Container: name,
		Workspace: root,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Image:     defaultDaemonImage,
	}
	if err := saveDaemonState(state); err != nil {
		t.Fatalf("saveDaemonState: %v", err)
	}

	plan, err := planStart(cfg, &fakeRuntime{statusByID: map[string]ContainerStatus{
		name: {Exists: true, Running: false, Status: "exited"},
	}},
		func(string, time.Duration) (*DaemonHealth, error) { return nil, errors.New("down") },
		defaultDaemonImage)
	if err != nil {
		t.Fatalf("planStart: %v", err)
	}
	if plan.Action != startFresh {
		t.Fatalf("plan.Action = %s; want %s", plan.Action, startFresh)
	}
	if !plan.RemoveStateFile {
		t.Fatal("expected stale state file to be reclaimed")
	}
}

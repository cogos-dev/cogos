package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	daemonModeBareMetal = "bare-metal"
	daemonModeContainer = "container"

	defaultDaemonImage = "cogos/kernel-v3:dev"
	stateHealthTimeout = 1500 * time.Millisecond
)

type DaemonState struct {
	Mode      string `yaml:"mode"`
	Endpoint  string `yaml:"endpoint"`
	Container string `yaml:"container,omitempty"`
	Workspace string `yaml:"workspace"`
	StartedAt string `yaml:"started_at"`
	Image     string `yaml:"image,omitempty"`
	PID       *int   `yaml:"pid"`
}

type DaemonHealth struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	State     string `json:"state"`
	Identity  string `json:"identity"`
	Workspace string `json:"workspace"`
}

type ContainerConfig struct {
	Name          string
	WorkspaceRoot string
	Port          int
	Command       []string
	Env           map[string]string
	RestartPolicy string
}

type ContainerStatus struct {
	Exists  bool
	Running bool
	Status  string
}

type ContainerRuntime interface {
	Start(image string, config ContainerConfig) (containerID string, err error)
	Stop(containerID string) error
	Status(containerID string) (ContainerStatus, error)
	Logs(containerID string, follow bool) (io.ReadCloser, error)
	Exec(containerID string, command []string) ([]byte, error)
	Pull(image string) error
}

type HealthChecker func(endpoint string, timeout time.Duration) (*DaemonHealth, error)

type startAction string

const (
	startFresh    startAction = "start_fresh"
	startReuse    startAction = "reuse"
	startAdopt    startAction = "adopt"
	startConflict startAction = "conflict"
)

type startPlan struct {
	Action          startAction
	Message         string
	ContainerName   string
	DesiredEndpoint string
	AdoptState      *DaemonState
	RemoveStateFile bool
}

func daemonStatePath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".cog", "run", "daemon", "state.yaml")
}

func endpointForPort(port int) string {
	if port == 0 {
		port = 6931
	}
	return fmt.Sprintf("http://localhost:%d", port)
}

func loadDaemonState(workspaceRoot string) (*DaemonState, error) {
	path := daemonStatePath(workspaceRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read daemon state: %w", err)
	}
	var state DaemonState
	if err := yaml.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse daemon state: %w", err)
	}
	if state.Workspace == "" {
		state.Workspace = workspaceRoot
	}
	if state.Endpoint == "" {
		state.Endpoint = endpointForPort(6931)
	}
	return &state, nil
}

func saveDaemonState(state *DaemonState) error {
	path := daemonStatePath(state.Workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("mkdir daemon state dir: %w", err)
	}
	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal daemon state: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write daemon state: %w", err)
	}
	return nil
}

func removeDaemonState(workspaceRoot string) error {
	path := daemonStatePath(workspaceRoot)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove daemon state: %w", err)
	}
	return nil
}

func workspaceSlug(workspaceRoot string) string {
	base := filepath.Base(filepath.Clean(workspaceRoot))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "workspace"
	}
	re := regexp.MustCompile(`[^a-zA-Z0-9]+`)
	slug := strings.Trim(re.ReplaceAllString(base, "-"), "-")
	if slug == "" {
		slug = "workspace"
	}
	return slug
}

func containerNameForWorkspace(workspaceRoot string) string {
	return "cogos-v3-" + workspaceSlug(workspaceRoot)
}

func checkDaemonHealth(endpoint string, timeout time.Duration) (*DaemonHealth, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health status %d", resp.StatusCode)
	}
	var health DaemonHealth
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil, err
	}
	return &health, nil
}

func resolveClientEndpoint(defaultWorkspace string, defaultPort int) string {
	if cfg, err := LoadConfig(defaultWorkspace, 0); err == nil {
		if state, serr := loadDaemonState(cfg.WorkspaceRoot); serr == nil && state != nil && state.Endpoint != "" {
			return state.Endpoint
		}
		if cfg.Port != 0 {
			return endpointForPort(cfg.Port)
		}
	}
	return endpointForPort(defaultPort)
}

func desiredModeFromEnv() string {
	if mode := os.Getenv("COG_DAEMON_MODE"); mode != "" {
		return mode
	}
	return daemonModeBareMetal
}

func buildDaemonState(cfg *Config) *DaemonState {
	state := &DaemonState{
		Mode:      desiredModeFromEnv(),
		Endpoint:  endpointForPort(cfg.Port),
		Workspace: cfg.WorkspaceRoot,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if state.Mode == daemonModeContainer {
		state.Container = os.Getenv("COG_DAEMON_CONTAINER")
		state.Image = os.Getenv("COG_DAEMON_IMAGE")
	} else {
		pid := os.Getpid()
		state.PID = &pid
	}
	return state
}

func planServeState(cfg *Config, health HealthChecker) (reuse bool, message string, err error) {
	state, serr := loadDaemonState(cfg.WorkspaceRoot)
	if serr != nil {
		return false, "", serr
	}
	endpoint := endpointForPort(cfg.Port)
	if state != nil && state.Endpoint != "" {
		endpoint = state.Endpoint
	}

	live, herr := health(endpoint, stateHealthTimeout)
	if herr == nil {
		if live.Workspace != "" && live.Workspace != cfg.WorkspaceRoot {
			return false, "", fmt.Errorf("daemon for workspace %s is already listening at %s", live.Workspace, endpoint)
		}
		return true, fmt.Sprintf("daemon already running for workspace %s at %s", cfg.WorkspaceRoot, endpoint), nil
	}

	if state != nil {
		if err := removeDaemonState(cfg.WorkspaceRoot); err != nil {
			return false, "", err
		}
	}
	return false, "", nil
}

func planStart(cfg *Config, runtime ContainerRuntime, health HealthChecker, image string) (*startPlan, error) {
	desiredEndpoint := endpointForPort(cfg.Port)
	containerName := containerNameForWorkspace(cfg.WorkspaceRoot)
	state, err := loadDaemonState(cfg.WorkspaceRoot)
	if err != nil {
		return nil, err
	}

	if state != nil {
		endpoint := state.Endpoint
		if endpoint == "" {
			endpoint = desiredEndpoint
		}
		if live, herr := health(endpoint, stateHealthTimeout); herr == nil {
			if live.Workspace != "" && live.Workspace != cfg.WorkspaceRoot {
				return &startPlan{
					Action:          startConflict,
					ContainerName:   containerName,
					DesiredEndpoint: desiredEndpoint,
					Message:         fmt.Sprintf("daemon for workspace %s is already listening at %s", live.Workspace, endpoint),
				}, nil
			}
			return &startPlan{
				Action:          startReuse,
				ContainerName:   containerName,
				DesiredEndpoint: endpoint,
				Message:         fmt.Sprintf("daemon already running for workspace %s at %s", cfg.WorkspaceRoot, endpoint),
			}, nil
		}

		containerID := state.Container
		if containerID == "" {
			containerID = containerName
		}
		status, serr := runtime.Status(containerID)
		if serr != nil {
			return nil, serr
		}
		return &startPlan{
			Action:          startFresh,
			ContainerName:   containerName,
			DesiredEndpoint: desiredEndpoint,
			RemoveStateFile: !status.Running,
		}, nil
	}

	if live, herr := health(desiredEndpoint, stateHealthTimeout); herr == nil {
		if live.Workspace != "" && live.Workspace != cfg.WorkspaceRoot {
			return &startPlan{
				Action:          startConflict,
				ContainerName:   containerName,
				DesiredEndpoint: desiredEndpoint,
				Message:         fmt.Sprintf("port conflict: workspace %s already uses %s", live.Workspace, desiredEndpoint),
			}, nil
		}

		status, serr := runtime.Status(containerName)
		if serr != nil {
			return nil, serr
		}

		mode := daemonModeBareMetal
		container := ""
		img := ""
		if status.Exists && status.Running {
			mode = daemonModeContainer
			container = containerName
			img = image
		}

		return &startPlan{
			Action:          startAdopt,
			ContainerName:   containerName,
			DesiredEndpoint: desiredEndpoint,
			Message:         fmt.Sprintf("adopting existing daemon for workspace %s at %s", cfg.WorkspaceRoot, desiredEndpoint),
			AdoptState: &DaemonState{
				Mode:      mode,
				Endpoint:  desiredEndpoint,
				Container: container,
				Workspace: cfg.WorkspaceRoot,
				StartedAt: time.Now().UTC().Format(time.RFC3339),
				Image:     img,
				PID:       nil,
			},
		}, nil
	}

	status, serr := runtime.Status(containerName)
	if serr != nil {
		return nil, serr
	}
	if status.Exists && status.Running {
		return &startPlan{
			Action:          startAdopt,
			ContainerName:   containerName,
			DesiredEndpoint: desiredEndpoint,
			Message:         fmt.Sprintf("adopting existing container %s", containerName),
			AdoptState: &DaemonState{
				Mode:      daemonModeContainer,
				Endpoint:  desiredEndpoint,
				Container: containerName,
				Workspace: cfg.WorkspaceRoot,
				StartedAt: time.Now().UTC().Format(time.RFC3339),
				Image:     image,
			},
		}, nil
	}

	return &startPlan{
		Action:          startFresh,
		ContainerName:   containerName,
		DesiredEndpoint: desiredEndpoint,
	}, nil
}

func waitForDaemonHealthy(endpoint string, timeout time.Duration, health HealthChecker) (*DaemonHealth, error) {
	deadline := time.Now().Add(timeout)
	for {
		live, err := health(endpoint, stateHealthTimeout)
		if err == nil {
			return live, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemon did not become healthy at %s within %s", endpoint, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func waitForDaemonDown(endpoint string, timeout time.Duration, health HealthChecker) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := health(endpoint, stateHealthTimeout); err != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon at %s did not stop within %s", endpoint, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

type NerdctlRuntime struct {
	bin string
}

func NewNerdctlRuntime() (*NerdctlRuntime, error) {
	if bin := os.Getenv("COGOS_V3_NERDCTL"); bin != "" {
		return &NerdctlRuntime{bin: bin}, nil
	}
	if bin, err := exec.LookPath("nerdctl"); err == nil {
		return &NerdctlRuntime{bin: bin}, nil
	}
	const fallback = "/opt/homebrew/bin/nerdctl"
	if _, err := os.Stat(fallback); err == nil {
		return &NerdctlRuntime{bin: fallback}, nil
	}
	return nil, errors.New("nerdctl not found; set COGOS_V3_NERDCTL or install nerdctl/Colima")
}

func (n *NerdctlRuntime) Start(image string, config ContainerConfig) (string, error) {
	args := []string{
		"run", "-d", "--replace",
		"--name", config.Name,
		"--restart", config.RestartPolicy,
		"-p", fmt.Sprintf("%d:%d", config.Port, config.Port),
		"-v", fmt.Sprintf("%s:%s", config.WorkspaceRoot, config.WorkspaceRoot),
		"-w", config.WorkspaceRoot,
	}
	for k, v := range config.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, image)
	args = append(args, config.Command...)

	cmd := exec.Command(n.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("nerdctl run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (n *NerdctlRuntime) Stop(containerID string) error {
	cmd := exec.Command(n.bin, "stop", containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nerdctl stop %s: %w: %s", containerID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (n *NerdctlRuntime) Status(containerID string) (ContainerStatus, error) {
	cmd := exec.Command(n.bin, "inspect", containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.ToLower(string(out))
		if strings.Contains(text, "no such object") || strings.Contains(text, "no such container") || strings.Contains(text, "not found") {
			return ContainerStatus{}, nil
		}
		return ContainerStatus{}, fmt.Errorf("nerdctl inspect %s: %w: %s", containerID, err, strings.TrimSpace(string(out)))
	}

	var inspect []struct {
		State struct {
			Running bool   `json:"Running"`
			Status  string `json:"Status"`
		} `json:"State"`
	}
	if err := json.Unmarshal(out, &inspect); err != nil {
		return ContainerStatus{}, fmt.Errorf("parse inspect output: %w", err)
	}
	if len(inspect) == 0 {
		return ContainerStatus{}, nil
	}
	return ContainerStatus{
		Exists:  true,
		Running: inspect[0].State.Running,
		Status:  inspect[0].State.Status,
	}, nil
}

func (n *NerdctlRuntime) Logs(containerID string, follow bool) (io.ReadCloser, error) {
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, containerID)
	cmd := exec.Command(n.bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &commandReadCloser{ReadCloser: stdout, wait: cmd.Wait, stderr: &stderr}, nil
}

func (n *NerdctlRuntime) Exec(containerID string, command []string) ([]byte, error) {
	args := append([]string{"exec", containerID}, command...)
	cmd := exec.Command(n.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("nerdctl exec %s: %w: %s", containerID, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (n *NerdctlRuntime) Pull(image string) error {
	cmd := exec.Command(n.bin, "pull", image)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nerdctl pull %s: %w: %s", image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

type commandReadCloser struct {
	io.ReadCloser
	wait   func() error
	stderr *bytes.Buffer
}

func (c *commandReadCloser) Close() error {
	readErr := c.ReadCloser.Close()
	waitErr := c.wait()
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return fmt.Errorf("%w: %s", waitErr, strings.TrimSpace(c.stderr.String()))
	}
	return nil
}

func stopBareMetalPID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

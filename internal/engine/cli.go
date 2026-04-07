// main.go — CogOS v3 kernel entry point
//
// Starts the continuous process daemon. Three goroutines run concurrently:
//  1. process.Run(ctx)  — the cognitive loop (field updates, consolidation, heartbeat)
//  2. server.Start()    — the HTTP API
//
// Flags:
//
//	--port        API port (default 6931; v2 is 5100)
//	--workspace   path to workspace root (auto-detected from cwd if omitted)
//	--config      (reserved for future use)
package engine

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var (
	// Version is injected at build time via -ldflags (e.g. "v0.1.0").
	Version = "dev"

	// BuildTime is injected at build time via -ldflags.
	BuildTime = "unknown"
)

func Main() {
	port := flag.Int("port", 0, "HTTP API port (default 6931)")
	workspace := flag.String("workspace", "", "Workspace root path (auto-detected if empty)")
	flag.Parse()

	// Sub-commands.
	args := flag.Args()
	if len(args) > 0 {
		switch args[0] {
		case "init":
			runInitCmd(args[1:], *workspace)
			return
		case "serve":
			runServeCmd(args[1:], *workspace, *port)
			return
		case "start":
			runStartCmd(args[1:], *workspace, *port)
			return
		case "stop":
			runStopCmd(args[1:], *workspace, *port)
			return
		case "restart":
			runRestartCmd(args[1:], *workspace, *port)
			return
		case "status":
			runStatusCmd(args[1:], *workspace, *port)
			return
		case "logs":
			runLogsCmd(args[1:], *workspace, *port)
			return
		case "version":
			fmt.Printf("cogos version=%s build=%s\n", Version, BuildTime)
			return
		case "health":
			runHealthCheckCmd(args[1:], *workspace, *port)
			return
		case "chat":
			runChat(args[1:], *workspace, *port)
			return
		case "bench":
			runBenchCmd(args[1:], *workspace, *port)
			return
		case "docs":
			runDocsCmd(args[1:], *workspace)
			return
		case "blobs":
			runBlobsCmd(args[1:], *workspace)
			return
		case "experiment":
			runExperimentCmd(args[1:], *workspace, *port)
			return
		}
	}

	// Compatibility path: plain `cogos-v3` still serves in the foreground.
	runServe(*workspace, *port)
}

func runInitCmd(args []string, defaultWorkspace string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (default: current directory)")
	_ = fs.Parse(args)

	// Use positional arg if no --workspace flag.
	target := *workspace
	if target == "" && fs.NArg() > 0 {
		target = fs.Arg(0)
	}

	if err := RunInit(target); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func setupLogger() {
	// Configure structured logging.
	level := slog.LevelInfo
	if os.Getenv("COG_LOG_DEBUG") != "" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func runServeCmd(args []string, defaultWorkspace string, defaultPort int) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected if empty)")
	port := fs.Int("port", defaultPort, "HTTP API port (default 6931)")
	_ = fs.Parse(args)
	runServe(*workspace, *port)
}

func runServe(workspace string, port int) {
	setupLogger()
	slog.Info("cogos-v3: starting", "build", BuildTime)

	// Load configuration.
	cfg, err := LoadConfig(workspace, port)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	slog.Info("config loaded", "workspace", cfg.WorkspaceRoot, "port", cfg.Port)

	if reuse, msg, err := planServeState(cfg, checkDaemonHealth); err != nil {
		slog.Error("daemon lifecycle failed", "err", err)
		os.Exit(1)
	} else if reuse {
		fmt.Fprintln(os.Stderr, msg)
		return
	}

	state := buildDaemonState(cfg)
	if err := saveDaemonState(state); err != nil {
		slog.Error("daemon state write failed", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := removeDaemonState(cfg.WorkspaceRoot); err != nil {
			slog.Warn("daemon state cleanup failed", "err", err)
		}
	}()

	// Load nucleus (identity core).
	nucleus, err := LoadNucleus(cfg)
	if err != nil {
		slog.Error("nucleus load failed", "err", err)
		os.Exit(1)
	}
	slog.Info("nucleus loaded", "summary", nucleus.Summary())

	// Build the continuous process.
	process := NewProcess(cfg, nucleus)

	// Load TRM model and embedding index (optional — graceful degradation).
	if trm, embIdx := loadTRMAtStartup(cfg); trm != nil && embIdx != nil {
		process.SetTRM(trm, embIdx)
		slog.Info("trm: wired into context assembly pipeline")
	}

	// Build the inference router.
	router, err := BuildRouter(cfg)
	if err != nil {
		slog.Warn("router build failed; inference disabled", "err", err)
	}

	// Build the HTTP server.
	server := NewServer(cfg, nucleus, process)
	if router != nil {
		server.SetRouter(router)
	}

	// Initialize telemetry (traces + metrics). No-op if no collector is available.
	ctx0 := context.Background()
	shutdownTelemetry := initTelemetry(ctx0)

	// Root context cancelled on SIGINT / SIGTERM.
	ctx, cancel := signal.NotifyContext(ctx0, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	defer shutdownTelemetry(ctx0)

	// Start process goroutine.
	processDone := make(chan error, 1)
	go func() {
		processDone <- process.Run(ctx)
	}()

	// Start HTTP server goroutine.
	serverDone := make(chan error, 1)
	go func() {
		if err := server.Start(); err != nil {
			serverDone <- err
		}
	}()

	// Wait for shutdown signal or fatal error.
	select {
	case <-ctx.Done():
		slog.Info("cogos-v3: shutdown signal received")
	case err := <-serverDone:
		if err != nil {
			slog.Error("server error", "err", err)
			cancel()
		}
	case err := <-processDone:
		if err != nil {
			slog.Error("process error", "err", err)
			cancel()
		}
	}

	// Graceful shutdown.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("server shutdown error", "err", err)
	}

	// Wait for process to finish.
	select {
	case <-processDone:
	case <-shutdownCtx.Done():
		slog.Warn("process did not stop in time")
	}

	slog.Info("cogos-v3: stopped")
}

// runHealthCheck performs a quick health check and exits 0 (healthy) or 1 (unhealthy).
// Used by the Dockerfile HEALTHCHECK directive.
func runHealthCheckCmd(args []string, defaultWorkspace string, defaultPort int) {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (used to resolve runtime state)")
	port := fs.Int("port", defaultPort, "Daemon port when no runtime state exists")
	_ = fs.Parse(args)

	baseURL := resolveClientEndpoint(*workspace, *port)
	url := baseURL + "/health"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)
		os.Exit(1)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "unhealthy: status %d\n", resp.StatusCode)
		os.Exit(1)
	}
	fmt.Println("healthy")
}

func runStartCmd(args []string, defaultWorkspace string, defaultPort int) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected if empty)")
	port := fs.Int("port", defaultPort, "Daemon port")
	image := fs.String("image", defaultDaemonImage, "OCI image to run")
	_ = fs.Parse(args)

	cfg, err := LoadConfig(*workspace, *port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}

	runtime, err := NewNerdctlRuntime()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	plan, err := planStart(cfg, runtime, checkDaemonHealth, *image)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	switch plan.Action {
	case startConflict:
		fmt.Fprintln(os.Stderr, plan.Message)
		os.Exit(1)
	case startReuse:
		fmt.Fprintln(os.Stderr, plan.Message)
		return
	case startAdopt:
		if plan.AdoptState != nil {
			if err := saveDaemonState(plan.AdoptState); err != nil {
				fmt.Fprintf(os.Stderr, "error: save daemon state: %v\n", err)
				os.Exit(1)
			}
		}
		fmt.Fprintln(os.Stderr, plan.Message)
		return
	case startFresh:
		if plan.RemoveStateFile {
			if err := removeDaemonState(cfg.WorkspaceRoot); err != nil {
				fmt.Fprintf(os.Stderr, "error: remove stale daemon state: %v\n", err)
				os.Exit(1)
			}
		}
	}

	containerName := plan.ContainerName
	if containerName == "" {
		containerName = containerNameForWorkspace(cfg.WorkspaceRoot)
	}
	containerID, err := runtime.Start(*image, ContainerConfig{
		Name:          containerName,
		WorkspaceRoot: cfg.WorkspaceRoot,
		Port:          cfg.Port,
		Command:       []string{"serve", "--workspace", cfg.WorkspaceRoot, "--port", fmt.Sprintf("%d", cfg.Port)},
		RestartPolicy: "unless-stopped",
		Env: map[string]string{
			"COG_DAEMON_MODE":      daemonModeContainer,
			"COG_DAEMON_CONTAINER": containerName,
			"COG_DAEMON_IMAGE":     *image,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if _, err := waitForDaemonHealthy(endpointForPort(cfg.Port), 12*time.Second, checkDaemonHealth); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if state, _ := loadDaemonState(cfg.WorkspaceRoot); state == nil {
		fallback := &DaemonState{
			Mode:      daemonModeContainer,
			Endpoint:  endpointForPort(cfg.Port),
			Container: containerName,
			Workspace: cfg.WorkspaceRoot,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
			Image:     *image,
		}
		if err := saveDaemonState(fallback); err != nil {
			fmt.Fprintf(os.Stderr, "warning: daemon is healthy but state file was not written: %v\n", err)
		}
	}

	fmt.Fprintf(os.Stderr, "started container %s (%s)\n", containerName, strings.TrimSpace(containerID))
}

func runStopCmd(args []string, defaultWorkspace string, defaultPort int) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected if empty)")
	port := fs.Int("port", defaultPort, "Daemon port")
	_ = fs.Parse(args)

	cfg, err := LoadConfig(*workspace, *port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}

	state, err := loadDaemonState(cfg.WorkspaceRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if state == nil {
		fmt.Fprintf(os.Stderr, "no daemon state for workspace %s\n", cfg.WorkspaceRoot)
		return
	}

	switch state.Mode {
	case daemonModeContainer:
		runtime, err := NewNerdctlRuntime()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		containerID := state.Container
		if containerID == "" {
			containerID = containerNameForWorkspace(cfg.WorkspaceRoot)
		}
		if err := runtime.Stop(containerID); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case daemonModeBareMetal:
		if state.PID == nil {
			fmt.Fprintf(os.Stderr, "cannot stop unmanaged bare-metal daemon for %s\n", cfg.WorkspaceRoot)
			os.Exit(1)
		}
		if err := stopBareMetalPID(*state.PID); err != nil {
			fmt.Fprintf(os.Stderr, "error: stop pid %d: %v\n", *state.PID, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown daemon mode %q\n", state.Mode)
		os.Exit(1)
	}

	if err := waitForDaemonDown(state.Endpoint, 10*time.Second, checkDaemonHealth); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if err := removeDaemonState(cfg.WorkspaceRoot); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cleanup state file: %v\n", err)
	}
	fmt.Fprintf(os.Stderr, "stopped daemon for workspace %s\n", cfg.WorkspaceRoot)
}

func runRestartCmd(args []string, defaultWorkspace string, defaultPort int) {
	fs := flag.NewFlagSet("restart", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected if empty)")
	port := fs.Int("port", defaultPort, "Daemon port")
	image := fs.String("image", defaultDaemonImage, "OCI image to run")
	_ = fs.Parse(args)

	cfg, err := LoadConfig(*workspace, *port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}
	state, err := loadDaemonState(cfg.WorkspaceRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if state != nil && state.Mode == daemonModeBareMetal {
		fmt.Fprintln(os.Stderr, "restart is only supported for container-managed daemons; stop the foreground `serve` process and start again")
		os.Exit(1)
	}

	runtime, err := NewNerdctlRuntime()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := runtime.Pull(*image); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if state != nil {
		runStopCmd([]string{"--workspace", cfg.WorkspaceRoot, "--port", fmt.Sprintf("%d", cfg.Port)}, cfg.WorkspaceRoot, cfg.Port)
	}
	runStartCmd([]string{"--workspace", cfg.WorkspaceRoot, "--port", fmt.Sprintf("%d", cfg.Port), "--image", *image}, cfg.WorkspaceRoot, cfg.Port)
}

func runStatusCmd(args []string, defaultWorkspace string, defaultPort int) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected if empty)")
	port := fs.Int("port", defaultPort, "Daemon port")
	_ = fs.Parse(args)

	cfg, err := LoadConfig(*workspace, *port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}
	state, err := loadDaemonState(cfg.WorkspaceRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	endpoint := endpointForPort(cfg.Port)
	if state != nil && state.Endpoint != "" {
		endpoint = state.Endpoint
	}

	fmt.Fprintf(os.Stdout, "Workspace: %s\n", cfg.WorkspaceRoot)
	fmt.Fprintf(os.Stdout, "Endpoint:  %s\n", endpoint)
	if state == nil {
		fmt.Fprintln(os.Stdout, "State:     missing")
	} else {
		fmt.Fprintf(os.Stdout, "Mode:      %s\n", state.Mode)
		if state.Container != "" {
			fmt.Fprintf(os.Stdout, "Container: %s\n", state.Container)
		}
		if state.PID != nil {
			fmt.Fprintf(os.Stdout, "PID:       %d\n", *state.PID)
		}
	}

	health, err := checkDaemonHealth(endpoint, 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stdout, "Health:    unreachable (%v)\n", err)
		if state == nil {
			return
		}
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "Health:    %s\n", health.Status)
	if health.State != "" {
		fmt.Fprintf(os.Stdout, "Kernel:    %s\n", health.State)
	}
	if health.Identity != "" {
		fmt.Fprintf(os.Stdout, "Identity:  %s\n", health.Identity)
	}
}

func runLogsCmd(args []string, defaultWorkspace string, defaultPort int) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected if empty)")
	follow := fs.Bool("f", true, "Follow logs")
	_ = fs.Parse(args)

	cfg, err := LoadConfig(*workspace, defaultPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}
	state, err := loadDaemonState(cfg.WorkspaceRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if state == nil {
		fmt.Fprintf(os.Stderr, "no daemon state for workspace %s\n", cfg.WorkspaceRoot)
		os.Exit(1)
	}
	if state.Mode != daemonModeContainer {
		fmt.Fprintln(os.Stderr, "logs is only supported for container-managed daemons")
		os.Exit(1)
	}

	runtime, err := NewNerdctlRuntime()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	containerID := state.Container
	if containerID == "" {
		containerID = containerNameForWorkspace(cfg.WorkspaceRoot)
	}
	reader, err := runtime.Logs(containerID, *follow)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer reader.Close()
	if _, err := io.Copy(os.Stdout, reader); err != nil {
		fmt.Fprintf(os.Stderr, "error: copy logs: %v\n", err)
		os.Exit(1)
	}
}

// serve_daemon.go — Daemon lifecycle management (start/stop/status/enable/disable) and CLI command handler

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	sdk "github.com/cogos-dev/cogos/sdk"
)

// === LOG ROTATION ===

// rotatingLogWriter implements io.Writer with size-based log rotation.
// When the current log file exceeds maxSize bytes, it closes the file,
// shifts existing rotated files (.1 → .2, .2 → .3, etc.), renames the
// current file to .1, and opens a fresh file. Thread-safe via mutex.
type rotatingLogWriter struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	maxSize  int64
	maxFiles int
	size     int64
}

// newRotatingLogWriter opens the log file at path and returns a rotating writer.
// maxSize is the threshold in bytes before rotation occurs. maxFiles is the number
// of old rotated files to keep (e.g., 3 means .1, .2, .3).
func newRotatingLogWriter(path string, maxSize int64, maxFiles int) (*rotatingLogWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}

	// Get current file size so we rotate correctly on restart
	var size int64
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}

	return &rotatingLogWriter{
		file:     f,
		path:     path,
		maxSize:  maxSize,
		maxFiles: maxFiles,
		size:     size,
	}, nil
}

// Write implements io.Writer. It writes p to the current log file and triggers
// rotation if the file size exceeds the threshold.
func (w *rotatingLogWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err = w.file.Write(p)
	w.size += int64(n)

	if w.size >= w.maxSize {
		w.rotate()
	}

	return n, err
}

// rotate closes the current file, shifts old files, and opens a new one.
// Must be called with w.mu held.
func (w *rotatingLogWriter) rotate() {
	w.file.Close()

	// Shift existing rotated files: .2 → .3, .1 → .2
	for i := w.maxFiles; i > 1; i-- {
		oldPath := fmt.Sprintf("%s.%d", w.path, i-1)
		newPath := fmt.Sprintf("%s.%d", w.path, i)
		os.Rename(oldPath, newPath) // ignore errors — file may not exist
	}

	// Rename current file to .1
	os.Rename(w.path, w.path+".1")

	// Open fresh file
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		// Best effort: reopen the .1 file so we don't lose writes entirely
		f, _ = os.OpenFile(w.path+".1", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	}
	w.file = f
	w.size = 0
}

// Close closes the underlying file.
func (w *rotatingLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// redirectOutputToRotatingLog sets up a rotating log writer and redirects
// os.Stdout, os.Stderr, and the default logger through it. This is used when
// the process is launched by launchd (stdout is not a terminal) so that
// launchd's StandardOutPath doesn't grow unbounded. Returns the writer so the
// caller can close it on shutdown.
func redirectOutputToRotatingLog(logPath string, maxSize int64, maxFiles int) (*rotatingLogWriter, error) {
	w, err := newRotatingLogWriter(logPath, maxSize, maxFiles)
	if err != nil {
		return nil, err
	}

	// Create a pipe: writes to pw appear as reads on pr
	pr, pw, err := os.Pipe()
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("os.Pipe: %w", err)
	}

	// Replace fd 1 (stdout) and fd 2 (stderr) with the pipe's write end
	if err := syscall.Dup2(int(pw.Fd()), 1); err != nil {
		w.Close()
		return nil, fmt.Errorf("dup2 stdout: %w", err)
	}
	if err := syscall.Dup2(int(pw.Fd()), 2); err != nil {
		w.Close()
		return nil, fmt.Errorf("dup2 stderr: %w", err)
	}

	// Update Go-level references so fmt.Printf, log.Printf, etc. use the pipe
	os.Stdout = pw
	os.Stderr = pw
	log.SetOutput(pw)

	// Pump pipe reads → rotating writer in background
	go func() {
		buf := make([]byte, 32768)
		for {
			n, readErr := pr.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
			}
			if readErr != nil {
				break
			}
		}
	}()

	return w, nil
}

// isStdoutTerminal returns true if stdout is connected to a terminal (as
// opposed to a file or pipe, which is the case under launchd).
func isStdoutTerminal() bool {
	stat, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

// === DAEMON MANAGEMENT ===

// getDaemonPaths returns node-level paths for PID file and log file.
// The daemon is a node-level process (serves all workspaces), so its
// lifecycle state lives under ~/.cog/, not under any specific workspace.
func getDaemonPaths() (pidFile, logFile, stateDir string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	stateDir = filepath.Join(home, ".cog", "run")
	pidFile = filepath.Join(stateDir, "serve.pid")
	logDir := filepath.Join(home, ".cog", "var", "logs")
	logFile = filepath.Join(logDir, "serve.log")

	// Ensure directories exist
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return "", "", "", fmt.Errorf("failed to create run directory: %w", err)
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return "", "", "", fmt.Errorf("failed to create log directory: %w", err)
	}

	return pidFile, logFile, stateDir, nil
}

// getLaunchdPlistPath returns the path to the launchd plist file
func getLaunchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

// readPIDFile reads the PID from the PID file
func readPIDFile(pidFile string) (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid PID in file: %w", err)
	}
	return pid, nil
}

// isProcessRunning checks if a process with the given PID is running
func isProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. Send signal 0 to check if process exists.
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// getServerStats fetches stats from the running server's health endpoint
func getServerStats(port int) (map[string]interface{}, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/health", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var stats map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, err
	}
	return stats, nil
}

// getRequestCount fetches request count from the running server
func getRequestCount(port int) (int, int, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/v1/requests", port))
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	var data struct {
		Count int `json:"count"`
		Data  []struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0, 0, err
	}

	// Count running requests
	running := 0
	for _, entry := range data.Data {
		if entry.Status == "running" {
			running++
		}
	}
	return data.Count, running, nil
}

// isLaunchdEnabled checks if the launchd plist exists
func isLaunchdEnabled() bool {
	plistPath := getLaunchdPlistPath()
	_, err := os.Stat(plistPath)
	return err == nil
}

// getStartTimeFromPID tries to get the process start time
func getStartTimeFromPID(pid int) (time.Time, error) {
	// Use ps to get the process start time
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=") // bare-ok: instant ps lookup
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, err
	}
	// Parse the output (format: "Wed Jan  8 10:30:00 2025")
	timeStr := strings.TrimSpace(string(out))
	if timeStr == "" {
		return time.Time{}, fmt.Errorf("empty output")
	}
	// Parse in local timezone — ps lstart returns local time
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", timeStr, time.Local)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// === COMMAND HANDLER ===

func cmdServe(args []string) int {
	port := defaultServePort
	subCmd := ""

	// Parse arguments to find subcommand and flags
	var remainingArgs []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port", "-p":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &port)
				i++
			}
		case "--debug":
			DebugMode.Store(true)
		case "--help", "-h":
			printServeHelp()
			return 0
		case "start", "stop", "status", "enable", "disable":
			if subCmd == "" {
				subCmd = args[i]
			}
		default:
			remainingArgs = append(remainingArgs, args[i])
		}
	}

	// Handle subcommands
	switch subCmd {
	case "start":
		return cmdServeStart(port)
	case "stop":
		return cmdServeStop()
	case "status":
		return cmdServeStatus(port)
	case "enable":
		return cmdServeEnable(port)
	case "disable":
		return cmdServeDisable()
	default:
		// No subcommand = run in foreground (existing behavior)
		return cmdServeForeground(port)
	}
}

// cmdServeForeground runs the server in the foreground
func cmdServeForeground(port int) int {
	// When running under launchd (stdout is not a terminal), redirect all output
	// through a rotating log writer so StandardOutPath doesn't grow unbounded.
	if !isStdoutTerminal() {
		if _, logFile, _, err := getDaemonPaths(); err == nil {
			if rotLog, err := redirectOutputToRotatingLog(logFile, 100*1024*1024, 3); err == nil {
				defer rotLog.Close()
				log.Printf("[serve] Log rotation active: %s (100 MB max, 3 files)", logFile)
			} else {
				log.Printf("[serve] Warning: failed to set up log rotation: %v", err)
			}
		}
	}

	// At least one local CLI backend must be available.
	_, claudeErr := exec.LookPath(claudeCommand)
	_, codexErr := exec.LookPath(codexCommand)
	if claudeErr != nil && codexErr != nil {
		fmt.Fprintf(os.Stderr, "Error: no local CLI inference backend found in PATH\n")
		fmt.Fprintf(os.Stderr, "Install one of:\n")
		fmt.Fprintf(os.Stderr, "  npm install -g @anthropic-ai/claude-code\n")
		fmt.Fprintf(os.Stderr, "  npm install -g @openai/codex\n")
		return 1
	}

	// Initialize SDK kernel for cog:// access
	root, source, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: Running outside a cog workspace\n")
		fmt.Fprintf(os.Stderr, "SDK features (cog://, /state, /signals) will be disabled\n\n")
		fmt.Fprintf(os.Stderr, "To enable SDK features:\n")
		fmt.Fprintf(os.Stderr, "  cd /path/to/workspace && cog serve   # Run from workspace\n")
		fmt.Fprintf(os.Stderr, "  cog -w myworkspace serve             # Use registered workspace\n")
		fmt.Fprintf(os.Stderr, "  COG_WORKSPACE=name cog serve         # Use env var\n\n")
	}
	_ = source // Source is informational only here

	// Resolve secrets via envspec if .envspec exists in workspace
	if root != "" {
		envspecPath := filepath.Join(root, ".envspec")
		if _, statErr := os.Stat(envspecPath); statErr == nil {
			if cfg, cfgErr := LoadNodeConfig(root); cfgErr == nil {
				if env, resolveErr := resolveNodeSecrets(root, cfg); resolveErr == nil {
					for k, v := range env.Vars {
						os.Setenv(k, v)
					}
					log.Printf("[envspec] Resolved %d secret(s) from %s", len(env.Vars), envspecPath)
				} else {
					log.Printf("[envspec] Warning: secret resolution failed: %v", resolveErr)
				}
			}
		}
	}

	var kernel *sdk.Kernel
	if root != "" {
		kernel, err = sdk.Connect(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Could not initialize SDK: %v\n", err)
			fmt.Fprintf(os.Stderr, "SDK features will be disabled\n")
			kernel = nil
		}
	}

	server := newServeServer(port, kernel)

	// Initialize OCI auto-reload
	if root != "" {
		store := NewOCIStore(root)
		if err := store.EnsureLayout(); err == nil {
			server.ociStore = store
			server.reexecCh = make(chan string, 1)
			// Use layer digest (binary content) for comparison — manifest digest
			// changes on every push due to timestamp annotations
			if d, resolveErr := store.ResolveLayerDigest(context.Background()); resolveErr == nil && d != "" {
				server.ociDigest = d
				digestShort := d
				if len(digestShort) > 23 {
					digestShort = digestShort[:23]
				}
				log.Printf("[oci] auto-reload enabled (current digest: %s)", digestShort)
			} else {
				log.Printf("[oci] auto-reload enabled (no artifact yet — push with: make push)")
			}
		}
	}

	// Initialize bus chat event emission if we have a workspace
	if root != "" {
		server.busChat = newBusChat(root)
		server.researchMgr = newResearchManager(root, server.busChat.manager)
		// Wire SSE broker to bus event emission
		server.busChat.manager.AddEventHandler("sse-broker", func(busID string, evt *CogBlock) {
			server.busBroker.publish(busID, evt)
		})

		// Initialize consumer cursor registry (ADR-061)
		server.consumerReg = newConsumerRegistry(filepath.Join(root, ".cog", "run", "bus"))
		if err := server.consumerReg.loadFromDisk(); err != nil {
			log.Printf("[bus-cursor] Failed to load cursors from disk: %v", err)
		}
		go server.consumerReg.runLifecycle(context.Background())

		// Wire block index — append-only hash index for all bus events
		blkIndex := newBlockIndex(root)
		server.busChat.manager.AddEventHandler("block-index", func(_ string, block *CogBlock) {
			blkIndex.Append(block)
		})

		// Wire constellation bus indexer — index chat content into FTS5
		// for cross-surface search (Discord, Claude Code, HTTP, Telegram)
		server.busChat.manager.AddEventHandler("constellation-bus", newConstellationBusHandler(root))

		log.Printf("[bus-chat] initialized (taa_profile=%s, context_from_bus=%v)",
			server.busChat.config.TAAProfile, server.busChat.config.Features.ContextFromBus)

		// --- Wire Phase 5-10 components ---

		// 0. Persistent OpenClawBridge for remote tool dispatch
		var openclawBridge *OpenClawBridge
		if ocURL := os.Getenv("OPENCLAW_URL"); ocURL != "" {
			ocToken := os.Getenv("OPENCLAW_TOKEN")
			openclawBridge = NewOpenClawBridge(ocURL, ocToken, "")
			if err := openclawBridge.ProbeGateway(context.Background()); err != nil {
				log.Printf("[bridge] OpenClaw gateway not available at %s: %v", ocURL, err)
				openclawBridge = nil
			} else {
				log.Printf("[bridge] OpenClaw gateway connected at %s", ocURL)
			}
		}

		// 1. CapabilityCache: TTL-based cache for agent capability advertisements
		capCache := NewCapabilityCache()
		stopSweeper := capCache.StartExpirySweeper(60 * time.Second)
		defer stopSweeper()

		// Wire CapabilityCache as bus consumer for agent.capabilities events
		server.busChat.manager.AddEventHandler("capability-cache", func(busID string, block *CogBlock) {
			if block.Type != BlockAgentCapabilities {
				return
			}
			// Parse payload
			payloadBytes, err := json.Marshal(block.Payload)
			if err != nil {
				log.Printf("[cap-cache] failed to marshal payload: %v", err)
				return
			}
			var caps AgentCapabilitiesPayload
			if err := json.Unmarshal(payloadBytes, &caps); err != nil {
				log.Printf("[cap-cache] failed to parse capabilities from %s: %v", block.From, err)
				return
			}
			ttl := defaultCapabilityTTL
			if caps.TTL != "" {
				if parsed, parseErr := time.ParseDuration(caps.TTL); parseErr == nil {
					ttl = parsed
				}
			}
			capCache.Set(caps.AgentID, caps, ttl)
			log.Printf("[cap-cache] cached capabilities for agent=%s tools_allow=%d tools_deny=%d ttl=%s",
				caps.AgentID, len(caps.Tools.Allow), len(caps.Tools.Deny), ttl)
		})

		// 2. CapabilityResolver: wraps cache for URI resolution and tool validation
		capResolver := NewCapabilityResolver(capCache)

		// 3. ToolRouter: listens for tool.invoke events on the bus
		toolRouter := NewToolRouter(server.busChat.manager, root, openclawBridge, capResolver)
		toolRouter.Start()
		defer toolRouter.Stop()

		// 4. CapabilityAdvertiser: advertise agent capabilities on startup
		go func() {
			if err := AdvertiseAgentCapabilities(root, server.busChat.manager); err != nil {
				log.Printf("[cap-advert] startup advertise failed: %v", err)
			}
		}()

		// 5. BEPProvider: file watcher for agent CRD changes
		bepProvider := NewBEPProvider(root)
		bepProvider.OnFileChange(func(filename string) {
			log.Printf("[bep] CRD changed: %s — re-advertising capabilities", filename)
			if err := AdvertiseAgentCapabilities(root, server.busChat.manager); err != nil {
				log.Printf("[cap-advert] re-advertise after CRD change failed: %v", err)
			}
		})
		if err := bepProvider.Start(); err != nil {
			log.Printf("[bep] failed to start provider: %v", err)
		} else {
			defer bepProvider.Stop()
		}

		// 5b. BEPEngine: cross-node sync via BEP protocol (gated on cluster.enabled)
		if bepCfg, cfgErr := bepProvider.LoadConfig(); cfgErr == nil && bepCfg.Enabled {
			engine, engineErr := NewBEPEngine(root, bepCfg, bepProvider)
			if engineErr != nil {
				log.Printf("[bep-engine] failed to create: %v", engineErr)
			} else {
				engine.SetBus(server.busChat.manager)
				bepProvider.AddChangeHandler(engine.NotifyLocalChange)
				if startErr := engine.Start(); startErr != nil {
					log.Printf("[bep-engine] failed to start: %v", startErr)
				} else {
					defer engine.Stop()
				}
			}
		}

		// 6. Node identity logging
		if nodeIdent, nodeErr := LoadNodeIdentity(); nodeErr == nil {
			log.Printf("[node] %s (%s/%s)", nodeIdent.Node.ID, nodeIdent.Node.OS, nodeIdent.Node.Arch)
		}

		// 7. Active reconciliation loop
		reconciler := NewServeReconciler(root)
		reconciler.SetBus(server.busChat.manager)
		if startErr := reconciler.Start(); startErr != nil {
			log.Printf("[reconciler] failed to start: %v", startErr)
		} else {
			defer reconciler.Stop()
		}

		// 6b. Service health monitor: polls container health every 30s, emits bus events
		svcMonitor := NewServiceHealthMonitor(root, server.busChat.manager)
		svcMonitor.Start()
		defer svcMonitor.Stop()

		// 6c. Advertise service capabilities on the bus
		if err := AdvertiseServiceCapabilities(root, server.busChat.manager); err != nil {
			log.Printf("[service] capability advertisement failed: %v", err)
		}

		// 6d. Initialize modality pipeline and auto-register HTTP modules from service CRDs
		{
			pipelineSessionID := generateID()
			pipeline := NewModalityPipeline(&PipelineConfig{
				WorkspaceRoot: root,
				SessionID:     pipelineSessionID,
			})
			registerHTTPModules(root, pipeline)
			if startErr := pipeline.Start(context.Background()); startErr != nil {
				log.Printf("[modality] pipeline start failed: %v", startErr)
			} else {
				log.Printf("[modality] pipeline started (session=%s, modules=%d)", pipelineSessionID, len(pipeline.Bus.HUD()))
				server.pipeline = pipeline
				defer pipeline.Stop(context.Background())
			}
		}

		// 7. EventDiscordBridge: forward bus events to Discord (only if configured)
		if webhookURL := loadEventsWebhookURL(root); webhookURL != "" {
			bridge := NewEventDiscordBridge(webhookURL, server.busBroker)
			server.busChat.manager.AddEventHandler("event-discord-bridge", bridge.HandleEvent)
			bridge.Start()
			defer func() {
				bridge.Stop()
				server.busChat.manager.RemoveEventHandler("event-discord-bridge")
			}()
		}

		// 8. Deterministic Reactor: fires rules on matching bus events (no LLM)
		reactor := NewReactor(server.busChat.manager)

		// Rule: system.startup → notify Discord #events via OpenClaw message tool.
		// When bridge is unavailable, falls back to log-only.
		bridge := openclawBridge // capture for closure
		reactor.AddRule(ReactorRule{
			Name:      "system.startup.notify",
			EventType: BlockSystemStartup,
			Action: func(block *CogBlock) {
				shortHash := block.Hash
				if len(shortHash) > 8 {
					shortHash = shortHash[:8]
				}
				agent := block.From
				if idx := strings.Index(agent, "@"); idx > 0 {
					agent = agent[:idx]
				}

				log.Printf("[reactor] system.startup from=%s hash=%s", block.From, shortHash)

				if bridge == nil {
					log.Printf("[reactor] no OpenClaw bridge — skipping Discord notification")
					return
				}

				msg := fmt.Sprintf("[%s · %s] Gateway online.", agent, shortHash)
				_, err := bridge.ExecuteTool(context.Background(), "message", map[string]interface{}{
					"action":  "send",
					"channel": "discord",
					"target":  "channel:1476656793659768978", // #events
					"message": msg,
					"silent":  true,
				})
				if err != nil {
					log.Printf("[reactor] startup notification failed: %v", err)
				} else {
					log.Printf("[reactor] startup notification sent: %s", msg)
				}
			},
		})

		// Rule: chat.response → log agent activity across surfaces for cross-session awareness.
		// Enables peripheral awareness to show "[agent] responded on [surface] Nm ago".
		reactor.AddRule(ReactorRule{
			Name:      "agent.activity.notify",
			EventType: BlockChatResponse,
			Action: func(block *CogBlock) {
				agent, _ := block.Payload["agent"].(string)
				origin, _ := block.Payload["origin"].(string)
				if agent == "" && origin == "" {
					return
				}
				log.Printf("[reactor] agent.activity: agent=%s origin=%s bus=%s seq=%d",
					agent, origin, block.BusID, block.Seq)
			},
		})

		// Rule: chat.request → detect cross-session patterns for context bridging.
		// When requests arrive from different surfaces within a time window,
		// logs the correlation for peripheral awareness enrichment.
		reactor.AddRule(ReactorRule{
			Name:      "session.context.bridge",
			EventType: BlockChatRequest,
			Action: func(block *CogBlock) {
				origin, _ := block.Payload["origin"].(string)
				agent, _ := block.Payload["agent"].(string)
				if origin == "" {
					return
				}
				log.Printf("[reactor] session.bridge: origin=%s agent=%s bus=%s seq=%d",
					origin, agent, block.BusID, block.Seq)
			},
		})

		// Rule: component.drift → structured logging for reconciliation drift events.
		// Logs component name, action type, severity, and resource for observability.
		reactor.AddRule(ReactorRule{
			Name:      "component.drift.log",
			EventType: BlockComponentDrift,
			Action: func(block *CogBlock) {
				component, _ := block.Payload["component"].(string)
				action, _ := block.Payload["action"].(string)
				severity, _ := block.Payload["severity"].(string)
				resource, _ := block.Payload["resource"].(string)
				reason, _ := block.Payload["reason"].(string)
				log.Printf("[reactor] component.drift: component=%s action=%s severity=%s resource=%s reason=%q",
					component, action, severity, resource, reason)
			},
		})

		// CRD-generated subscription rules
		if subRules, err := GenerateSubscriptionRules(root, server.busChat.manager); err != nil {
			log.Printf("[reactor] subscription rule generation failed: %v", err)
		} else {
			for _, rule := range subRules {
				reactor.AddRule(rule)
			}
			log.Printf("[reactor] added %d CRD subscription rules", len(subRules))
		}

		reactor.Start()
		defer reactor.Stop()

		// Suppress unused variable warnings for cache (used by capability resolver)
		_ = capCache
	}

	// Load workspace registry for multi-workspace support
	server.workspaces = make(map[string]*workspaceContext)
	if globalCfg, err := loadGlobalConfig(); err == nil && len(globalCfg.Workspaces) > 0 {
		server.defaultWS = globalCfg.CurrentWorkspace
		for wsName, wsEntry := range globalCfg.Workspaces {
			wsKernel, wsErr := sdk.Connect(wsEntry.Path)
			if wsErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to connect workspace %q (%s): %v\n", wsName, wsEntry.Path, wsErr)
				continue
			}
			server.workspaces[wsName] = &workspaceContext{
				root:    wsEntry.Path,
				name:    wsName,
				kernel:  wsKernel,
				busChat: newBusChat(wsEntry.Path),
			}
		}
		fmt.Fprintf(os.Stderr, "Loaded %d workspaces\n", len(server.workspaces))
	}

	// Ensure the primary workspace uses server.busChat (which has event handlers wired).
	// Named workspaces from the registry get their own newBusChat(), but the primary
	// workspace must share server.busChat so block-index, reactor, SSE, and discord
	// bridge handlers fire for events on this workspace's buses.
	if root != "" {
		found := false
		for wsName, ws := range server.workspaces {
			if ws.root == root {
				ws.busChat = server.busChat
				found = true
				log.Printf("[workspace] linked %q to primary busChat (handlers active)", wsName)
				break
			}
		}
		if !found {
			// Primary workspace not in named registry — add by path
			server.workspaces[root] = &workspaceContext{
				root:    root,
				name:    root,
				kernel:  kernel,
				busChat: server.busChat,
			}
		}
	}

	// Initialize OpenTelemetry tracing (noop if OTEL_EXPORTER_OTLP_ENDPOINT is not set)
	tp, otelErr := initTracer()
	if otelErr != nil {
		log.Printf("[otel] failed to initialize tracer: %v", otelErr)
	} else if tp != nil {
		log.Printf("[otel] tracing enabled (endpoint=%s)", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}

	// Start OCI watcher for auto-reload
	stopOCIWatch := server.startOCIWatcher()
	defer stopOCIWatch()

	// Write PID file so `cog serve status` works regardless of how the
	// server was launched (foreground, `cog serve start`, or launchd).
	if pidFile, _, _, pidErr := getDaemonPaths(); pidErr == nil {
		if writeErr := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0644); writeErr != nil {
			log.Printf("[serve] warning: failed to write PID file: %v", writeErr)
		} else {
			defer os.Remove(pidFile)
		}
	}

	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Flush and shut down the tracer after server stops
	if tp != nil {
		shutdownTracer(tp)
	}

	return 0
}

// cmdServeStart starts the server as a background daemon
func cmdServeStart(port int) int {
	pidFile, logFile, _, err := getDaemonPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Check if already running
	if pid, err := readPIDFile(pidFile); err == nil {
		if isProcessRunning(pid) {
			fmt.Fprintf(os.Stderr, "Server already running (PID %d)\n", pid)
			return 1
		}
		// Stale PID file, remove it
		os.Remove(pidFile)
	}

	// Get the kernel binary path
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found (run from workspace or use -w flag)\n")
		return 1
	}
	cogBinary := filepath.Join(root, ".cog", "cog")

	// Open log file with rotation (100 MB max, keep 3 old files)
	logOut, err := newRotatingLogWriter(logFile, 100*1024*1024, 3)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening log file: %v\n", err)
		return 1
	}

	// Build command to run serve in foreground mode
	cmd := exec.Command(cogBinary, "serve", "--port", strconv.Itoa(port)) // bare-ok: long-running daemon process
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	cmd.Dir = root

	// Detach from parent process
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	// Start the process
	if err := cmd.Start(); err != nil {
		logOut.Close()
		fmt.Fprintf(os.Stderr, "Error starting server: %v\n", err)
		return 1
	}

	// Write PID file
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write PID file: %v\n", err)
	}

	fmt.Printf("Server started (PID %d) on port %d\n", cmd.Process.Pid, port)
	fmt.Printf("Log file: %s\n", PathToURI(root, logFile))

	// Detach - don't wait for the child process
	// The child will be orphaned and adopted by init/launchd

	return 0
}

// cmdServeStop stops the background daemon
func cmdServeStop() int {
	pidFile, _, _, err := getDaemonPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	pid, err := readPIDFile(pidFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Server not running (no PID file)\n")
		return 1
	}

	if !isProcessRunning(pid) {
		// Clean up stale PID file
		os.Remove(pidFile)
		fmt.Fprintf(os.Stderr, "Server not running (stale PID file removed)\n")
		return 1
	}

	// Send SIGTERM
	process, err := os.FindProcess(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding process: %v\n", err)
		return 1
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "Error sending signal: %v\n", err)
		return 1
	}

	// Wait for process to exit (with timeout)
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if !isProcessRunning(pid) {
			break
		}
	}

	// Check if still running
	if isProcessRunning(pid) {
		fmt.Fprintf(os.Stderr, "Warning: process did not exit gracefully, sending SIGKILL\n")
		process.Signal(syscall.SIGKILL)
	}

	// Remove PID file
	os.Remove(pidFile)

	fmt.Printf("Server stopped (PID %d)\n", pid)
	return 0
}

// cmdServeStatus shows the daemon status
func cmdServeStatus(port int) int {
	pidFile, logFile, _, err := getDaemonPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Check if running via PID file
	pid, pidErr := readPIDFile(pidFile)
	running := pidErr == nil && isProcessRunning(pid)

	// Check launchd status
	launchdEnabled := isLaunchdEnabled()

	fmt.Printf("CogOS Inference Server Status\n")
	fmt.Printf("==============================\n\n")

	if running {
		fmt.Printf("Status:      \033[32mRUNNING\033[0m\n")
		fmt.Printf("Version:     %s\n", Version)
		fmt.Printf("PID:         %d\n", pid)
		fmt.Printf("Port:        %d\n", port)

		// Get uptime
		if startTime, err := getStartTimeFromPID(pid); err == nil {
			uptime := time.Since(startTime).Round(time.Second)
			fmt.Printf("Uptime:      %s\n", uptime)
		}

		// Get request stats from server
		if total, running, err := getRequestCount(port); err == nil {
			fmt.Printf("Requests:    %d total, %d running\n", total, running)
		} else {
			fmt.Printf("Requests:    (unable to connect)\n")
		}

		// Get health status
		if stats, err := getServerStats(port); err == nil {
			if status, ok := stats["status"].(string); ok {
				fmt.Printf("Health:      %s\n", status)
			}
			if claude, ok := stats["claude"].(bool); ok {
				if claude {
					fmt.Printf("Claude CLI:  \033[32mavailable\033[0m\n")
				} else {
					fmt.Printf("Claude CLI:  \033[31munavailable\033[0m\n")
				}
			}
		}
	} else {
		fmt.Printf("Status:      \033[31mSTOPPED\033[0m\n")
		if pidErr == nil {
			fmt.Printf("Note:        Stale PID file exists (PID %d)\n", pid)
		}
	}

	fmt.Printf("\n")

	// Persistence status
	if launchdEnabled {
		fmt.Printf("Persistence: \033[32mENABLED\033[0m (launchd)\n")
		fmt.Printf("Plist:       %s\n", getLaunchdPlistPath())
	} else {
		fmt.Printf("Persistence: \033[33mDISABLED\033[0m (on-demand only)\n")
	}

	// Show workspace-internal paths as cog:// URIs
	if root, _, err := ResolveWorkspace(); err == nil {
		fmt.Printf("Log file:    %s\n", PathToURI(root, logFile))
		fmt.Printf("PID file:    %s\n", PathToURI(root, pidFile))
	} else {
		fmt.Printf("Log file:    %s\n", logFile)
		fmt.Printf("PID file:    %s\n", pidFile)
	}

	if running {
		return 0
	}
	return 1
}

// cmdServeEnable registers with launchd for auto-start
func cmdServeEnable(port int) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found (run from workspace or use -w flag)\n")
		return 1
	}

	_, logFile, _, err := getDaemonPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	cogBinary := filepath.Join(root, ".cog", "cog")
	plistPath := getLaunchdPlistPath()

	// Ensure LaunchAgents directory exists
	launchAgentsDir := filepath.Dir(plistPath)
	if err := os.MkdirAll(launchAgentsDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating LaunchAgents directory: %v\n", err)
		return 1
	}

	// Generate plist content
	plistContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>serve</string>
        <string>--port</string>
        <string>%d</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin</string>
    </dict>
</dict>
</plist>
`, launchdLabel, cogBinary, port, root, logFile, logFile)

	// Write plist file
	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing plist: %v\n", err)
		return 1
	}

	// Load with launchctl (with timeout to prevent hang)
	loadCtx, loadCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer loadCancel()
	cmd := exec.CommandContext(loadCtx, "launchctl", "load", plistPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error loading plist: %v\n", err)
		fmt.Fprintf(os.Stderr, "Plist written to: %s\n", plistPath)
		fmt.Fprintf(os.Stderr, "You can load it manually with: launchctl load %s\n", plistPath)
		return 1
	}

	fmt.Printf("Server enabled for auto-start\n")
	fmt.Printf("Plist: %s\n", plistPath)
	fmt.Printf("Port: %d\n", port)
	fmt.Printf("\nTo disable: cog serve disable\n")
	return 0
}

// cmdServeDisable removes from launchd
func cmdServeDisable() int {
	plistPath := getLaunchdPlistPath()

	if !isLaunchdEnabled() {
		fmt.Println("Server is not enabled for auto-start")
		return 0
	}

	// Unload with launchctl (with timeout to prevent hang)
	unloadCtx, unloadCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer unloadCancel()
	cmd := exec.CommandContext(unloadCtx, "launchctl", "unload", plistPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: launchctl unload failed: %v\n", err)
	}

	// Remove plist file
	if err := os.Remove(plistPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error removing plist: %v\n", err)
		return 1
	}

	fmt.Printf("Server disabled for auto-start\n")
	fmt.Printf("Removed: %s\n", plistPath)
	return 0
}

func printServeHelp() {
	fmt.Printf(`Serve - Unified CogOS HTTP API

This server provides a single entry point for all CogOS workspace access:
- OpenAI-compatible inference (wrapping Claude CLI)
- Universal cog:// URI resolution (SDK)
- Real-time WebSocket updates
- Widget state (Whirlpool)

Usage: cog serve [command] [options]

Commands:
  (none)      Run server in foreground (default)
  start       Start server as background daemon
  stop        Stop the background daemon
  status      Show server status, PID, port, uptime, request count
  enable      Register with launchd for auto-start on login
  disable     Remove from launchd

Options:
  --port, -p <port>   Port to listen on (default: %d)
  --help, -h          Show this help

Inference Endpoints (OpenAI-compatible):
  POST   /v1/chat/completions   Chat completions (streaming & non-streaming)
  GET    /v1/models             List available models
  GET    /v1/providers          List providers with status/health (ADR-046)
  GET    /v1/requests           List in-flight requests
  GET    /v1/requests/:id       Get specific request status
  DELETE /v1/requests/:id       Cancel a request
  GET    /v1/taa                TAA context visibility (debugging)
  GET    /v1/sessions           List sessions with context metadata
  GET    /v1/sessions/:id/context  Per-session context state

SDK Endpoints (universal cog:// access):
  GET    /resolve?uri=cog://... Resolve any cog:// URI
  POST   /mutate                Apply mutations (set/patch/append/delete)
  GET    /ws/watch?uri=cog://...  WebSocket for real-time updates

Widget Endpoints (Whirlpool):
  GET    /state                 Full workspace state (coherence, signals)
  GET    /signals               Signal field with decay calculations
  GET    /health                Health check

URI Examples:
  /resolve?uri=cog://mem/semantic/insights  - List memory documents
  /resolve?uri=cog://coherence                 - Get coherence state
  /resolve?uri=cog://signals                   - Get signal field
  /resolve?uri=cog://identity                  - Get workspace identity
  /resolve?uri=cog://thread                    - Get conversation thread

Examples:
  # Run server in foreground
  cog serve

  # Start as background daemon
  cog serve start

  # Resolve a cog:// URI
  curl "http://localhost:5100/resolve?uri=cog://coherence"

  # Get workspace state
  curl "http://localhost:5100/state"

  # Chat completion (non-streaming)
  curl http://localhost:5100/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"claude","messages":[{"role":"user","content":"Hello!"}]}'

  # Watch for signal changes (WebSocket)
  websocat "ws://localhost:5100/ws/watch?uri=cog://signals"

Daemon Files:
  PID file: .cog/run/serve.pid
  Log file: .cog/logs/serve.log
  Plist:    ~/Library/LaunchAgents/com.cogos.kernel.plist

Notes:
  - Requires Claude CLI installed (npm install -g @anthropic-ai/claude-code)
  - SDK features require running from within a cog-workspace
  - Supports 20+ cog:// namespaces (memory, signals, coherence, identity, etc.)
  - Converts Claude responses to OpenAI format
  - Supports both streaming (SSE) and non-streaming responses
`, defaultServePort)
}

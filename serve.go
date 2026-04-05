// serve.go — CogOS HTTP server core: struct, routes, middleware, lifecycle
//
// Split files:
//   serve_types.go      — OpenAI/Claude API types
//   serve_daemon.go     — Daemon management (start/stop/enable/disable) + CLI command handler
//   serve_providers.go  — Provider health, discovery, models (ADR-046)
//   serve_context.go    — TAA context, session observability, foveated rendering
//   serve_inference.go  — Chat completions, streaming, tool bridge, thread persistence
//   serve_bus.go        — SDK routes (cog:// resolve/mutate/watch) + consumer cursors (ADR-061)

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	sdk "github.com/cogos-dev/cogos/sdk"
	"github.com/fsnotify/fsnotify"
)

// === CONFIGURATION ===
// Port assignments are defined in cog://conf/ports (canonical registry)
// See: .cog/conf/ports.cog.md for port policy and ranges

const (
	defaultServePort = 5100 // Registered: cog://conf/ports#kernel
	claudeCommand    = "claude"
	codexCommand     = "codex"
	launchdLabel     = "com.cogos.kernel"
)

// === SERVER ===

// DebugMode controls verbose logging in inference
var DebugMode atomic.Bool

// ctxKey is a typed key for context.WithValue to avoid collisions.
type ctxKey string

const ctxWorkspaceKey ctxKey = "workspace"

// workspaceContext holds per-workspace state for multi-workspace serving.
type workspaceContext struct {
	root    string
	name    string
	kernel  *sdk.Kernel
	busChat *busChat
}

type serveServer struct {
	port          int
	kernel        *sdk.Kernel                  // default workspace kernel (backward compat)
	workspaces    map[string]*workspaceContext // name → workspace context
	defaultWS     string                       // default workspace name
	lastTAAState  *ContextState                // Most recent TAA context for debugging
	taaStateMutex sync.RWMutex
	busChat       *busChat           // Bus event emission for chat (nil if no workspace)
	busBroker     *busEventBroker    // SSE subscriber broker for bus events
	consumerReg   *consumerRegistry  // Server-side consumer cursor tracking (ADR-061)
	toolBridge    *ToolBridge        // Synchronous tool bridge for client-driven agent loops
	mcpManager    *MCPSessionManager // MCP Streamable HTTP session manager
	researchMgr   *researchManager   // Research orchestration (nil if no workspace)

	// Context engine: normalizes threads, manages sessions, builds compressed context.
	// Replaces the simple claudeSessionStore for context-aware inference.
	contextEngine *ContextEngine

	// Claude CLI session continuity (legacy path — kept for non-context-engine requests).
	claudeSessionStore   map[string]string
	claudeSessionStoreMu sync.RWMutex

	// Lifecycle manager: tracks inference sessions for hook dispatch (identity
	// injection on first turn, working memory updates after each turn).
	lifecycle *LifecycleManager

	// OCI auto-reload: kernel watches .cog/oci/index.json for new digests
	ociStore  *OCIStore   // nil if no OCI layout exists
	ociDigest string      // manifest digest at startup (for comparison)
	reexecCh  chan string // signals graceful re-exec with new digest
}

func newServeServer(port int, kernel *sdk.Kernel) *serveServer {
	root, _, _ := ResolveWorkspace()
	return &serveServer{
		port:               port,
		kernel:             kernel,
		busBroker:          newBusEventBroker(),
		toolBridge:         NewToolBridge(),
		contextEngine:      NewContextEngine(root),
		claudeSessionStore: make(map[string]string),
		lifecycle:          NewLifecycleManager(),
	}
}

// getWorkspace returns workspace context by name or path. Falls back to default.
func (s *serveServer) getWorkspace(nameOrPath string) *workspaceContext {
	if nameOrPath == "" {
		nameOrPath = s.defaultWS
	}
	// Try by name first
	if ws, ok := s.workspaces[nameOrPath]; ok {
		return ws
	}
	// Try by path
	for _, ws := range s.workspaces {
		if ws.root == nameOrPath {
			return ws
		}
	}
	// Fall back to default
	if ws, ok := s.workspaces[s.defaultWS]; ok {
		return ws
	}
	return nil
}

// workspaceMiddleware extracts workspace selection from each request and injects
// the resolved workspaceContext into the request context.
func (s *serveServer) workspaceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wsRoot string

		// Resolution order:
		// 1. X-UCP-Workspace header (Root field)
		if wsHeader := r.Header.Get("X-UCP-Workspace"); wsHeader != "" {
			var wsPacket struct {
				Root string `json:"root"`
			}
			if json.Unmarshal([]byte(wsHeader), &wsPacket) == nil && wsPacket.Root != "" {
				wsRoot = wsPacket.Root
			}
		}

		// 2. ?workspace= query parameter
		if wsRoot == "" {
			wsRoot = r.URL.Query().Get("workspace")
		}

		// Resolve to workspaceContext
		ws := s.getWorkspace(wsRoot)
		if ws == nil {
			// No workspace found at all — proceed without workspace context
			// (health, debug endpoints don't need it)
			next.ServeHTTP(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), ctxWorkspaceKey, ws)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// workspaceFromRequest returns the workspace context injected by workspaceMiddleware.
// Returns nil if no workspace context is available.
func workspaceFromRequest(r *http.Request) *workspaceContext {
	ws, _ := r.Context().Value(ctxWorkspaceKey).(*workspaceContext)
	return ws
}

// deepCopyContextState creates a deep copy of a ContextState so that the copy
// does not share any pointers, slices, or maps with the original. This prevents
// data races when the original is mutated concurrently.
func deepCopyContextState(src *ContextState) *ContextState {
	if src == nil {
		return nil
	}
	dst := *src // shallow copy of value fields

	// Deep copy pointer-to-struct fields (ContextTier)
	if src.Tier1Identity != nil {
		t := *src.Tier1Identity
		dst.Tier1Identity = &t
	}
	if src.Tier2Temporal != nil {
		t := *src.Tier2Temporal
		dst.Tier2Temporal = &t
	}
	if src.Tier3Present != nil {
		t := *src.Tier3Present
		dst.Tier3Present = &t
	}
	if src.Tier4Semantic != nil {
		t := *src.Tier4Semantic
		dst.Tier4Semantic = &t
	}

	return &dst
}

// Start begins the HTTP server
func (s *serveServer) Start() error {
	StartRegistryCleanup()

	// Initialize the harness (inference engine)
	initHarness()

	mux := http.NewServeMux()

	// Inference routes (keep custom streaming implementation)
	mux.HandleFunc("/v1/chat/completions", otelMiddleware("POST /v1/chat/completions", s.handleChatCompletions))
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/providers", s.handleProviders)     // ADR-046: Provider discovery
	mux.HandleFunc("/v1/providers/", s.handleProviderByID) // Provider activate/test
	mux.HandleFunc("/v1/requests", s.handleRequests)
	mux.HandleFunc("/v1/requests/", s.handleRequestByID)
	mux.HandleFunc("/v1/taa", s.handleTAA)                                    // TAA context visibility endpoint
	mux.HandleFunc("POST /v1/context/foveated", s.handleFoveatedContext)      // Iris-driven foveated rendering
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)                  // Per-session context list
	mux.HandleFunc("/v1/sessions/", s.handleSessionContext)                   // Per-session context detail
	mux.HandleFunc("GET /v1/card", s.handleCard)                              // ADR-048: Kernel capability card
	mux.HandleFunc("POST /v1/tool-bridge/pending", s.handleToolBridgePending) // Synchronous tool bridge
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/debug", s.handleDebug)
	mux.HandleFunc("/services", s.handleServices)

	// Bus event streaming (SSE) and REST endpoints
	mux.HandleFunc("GET /v1/events/stream", s.handleEventsStream)
	mux.HandleFunc("GET /v1/bus/events", s.handleBusEventsGlobal) // Cross-bus event search
	mux.HandleFunc("GET /v1/bus/", s.handleBusRoute)              // Catch-all: /{bus_id}/events, /events/{seq}, /stats

	// Bus messaging API (inter-workspace)
	mux.HandleFunc("POST /v1/bus/send", s.handleBusSend)
	mux.HandleFunc("POST /v1/bus/open", s.handleBusOpen)
	mux.HandleFunc("GET /v1/bus/list", s.handleBusList)

	// Consumer cursor API (ADR-061)
	mux.HandleFunc("GET /v1/bus/consumers", s.handleBusConsumers)
	mux.HandleFunc("DELETE /v1/bus/consumers/", s.handleBusConsumerDelete)
	mux.HandleFunc("POST /v1/bus/", s.handleBusAck) // catch-all POST for /v1/bus/{bus_id}/ack

	// SDK routes (universal cog:// access)
	if s.kernel != nil {
		mux.HandleFunc("GET /resolve", s.handleResolve)
		mux.HandleFunc("POST /mutate", s.handleMutate)
		mux.HandleFunc("GET /ws/watch", s.handleWatch)
		// Whirlpool endpoints via SDK
		mux.HandleFunc("GET /state", s.handleState)
		mux.HandleFunc("GET /signals", s.handleSignals)
	}

	// MCP Streamable HTTP endpoint
	mcpRoot := ""
	if ws := s.getWorkspace(""); ws != nil {
		mcpRoot = ws.root
	} else if s.kernel != nil {
		mcpRoot = s.kernel.Root()
	}
	if mcpRoot != "" {
		s.mcpManager = NewMCPSessionManager(s.workspaces, mcpRoot)
		mux.Handle("/mcp", s.mcpManager)
	}

	// CogField graph endpoint
	mux.HandleFunc("GET /api/cogfield/graph", s.handleCogFieldGraph)
	mux.HandleFunc("GET /api/cogfield/query", s.handleCogFieldQuery)
	mux.HandleFunc("/api/cogfield/sessions/", s.handleSessionDetail)
	mux.HandleFunc("/api/cogfield/buses/", s.handleBusDetail)
	mux.HandleFunc("/api/cogfield/expand/", s.handleExpandNode)
	mux.HandleFunc("/api/cogfield/documents/", s.handleDocumentDetail)

	// Research orchestration endpoints
	mux.HandleFunc("POST /v1/research/start", s.handleResearchStart)
	mux.HandleFunc("GET /v1/research/status", s.handleResearchStatus)
	mux.HandleFunc("POST /v1/research/eval", s.handleResearchEval)
	mux.HandleFunc("POST /v1/research/keep", s.handleResearchKeep)
	mux.HandleFunc("POST /v1/research/discard", s.handleResearchDiscard)
	mux.HandleFunc("POST /v1/research/pause", s.handleResearchPause)
	mux.HandleFunc("POST /v1/research/resume", s.handleResearchResume)
	mux.HandleFunc("POST /v1/research/stop", s.handleResearchStop)
	mux.HandleFunc("GET /v1/research/results", s.handleResearchResults)

	mux.HandleFunc("/", s.handleRoot)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)

	// Check port availability before printing banner
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			fmt.Fprintf(os.Stderr, "Error: Port %d is already in use\n", s.port)
			fmt.Fprintf(os.Stderr, "\nTo fix this:\n")
			fmt.Fprintf(os.Stderr, "  lsof -i :%d          # See what's using the port\n", s.port)
			fmt.Fprintf(os.Stderr, "  cog serve stop       # Stop existing cog server\n")
			fmt.Fprintf(os.Stderr, "  cog serve --port %d  # Use a different port\n", s.port+1)
			return err
		}
		return err
	}
	listener.Close()

	server := &http.Server{
		Addr:         addr,
		Handler:      s.workspaceMiddleware(s.corsMiddleware(mux)),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // Long timeout for streaming
	}

	// Start SSE connection reaper (belt: sweeps stale connections every 30s)
	reaperCtx, reaperCancel := context.WithCancel(context.Background())
	defer reaperCancel()
	s.busBroker.startReaper(reaperCtx)

	// Graceful shutdown / OCI re-exec
	done := make(chan bool, 1)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case <-quit:
			fmt.Println("\nShutting down server...")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			server.Shutdown(ctx)
			close(done)

		case newDigest := <-s.reexecCh:
			digestShort := newDigest
			if len(digestShort) > 23 {
				digestShort = digestShort[:23]
			}
			log.Printf("[oci] initiating graceful re-exec (new digest: %s)", digestShort)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			server.Shutdown(ctx)

			// Re-exec: replace this process with the new binary
			selfPath, _ := os.Executable()
			selfPath, _ = filepath.EvalSymlinks(selfPath)
			log.Printf("[oci] re-exec: %s %v", selfPath, os.Args)

			execErr := syscall.Exec(selfPath, os.Args, os.Environ())
			// If Exec fails, we're still running the old binary
			log.Printf("[oci] re-exec failed: %v (continuing with current binary)", execErr)
			close(done)
		}
	}()

	fmt.Printf("CogOS unified server starting on http://localhost:%d\n", s.port)
	fmt.Printf("\nInference (OpenAI-compatible):\n")
	fmt.Printf("  POST   /v1/chat/completions - Chat completions\n")
	fmt.Printf("  GET    /v1/models           - List models\n")
	fmt.Printf("  GET    /v1/providers        - List providers with health (ADR-046)\n")
	fmt.Printf("  GET    /v1/requests         - List in-flight requests\n")
	fmt.Printf("  DELETE /v1/requests/:id     - Cancel a request\n")
	fmt.Printf("  GET    /v1/taa              - TAA context visibility\n")
	fmt.Printf("  GET    /v1/sessions         - List sessions with context metadata\n")
	fmt.Printf("  GET    /v1/sessions/:id/context - Per-session context state\n")
	if s.kernel != nil {
		fmt.Printf("\nSDK (universal cog:// access):\n")
		fmt.Printf("  GET    /resolve?uri=cog://... - Resolve any URI\n")
		fmt.Printf("  POST   /mutate                - Apply mutations\n")
		fmt.Printf("  GET    /ws/watch?uri=cog://...  - WebSocket watch\n")
		fmt.Printf("\nWhirlpool (widget state):\n")
		fmt.Printf("  GET    /state               - Workspace state\n")
		fmt.Printf("  GET    /signals             - Signal field\n")
	}
	if s.mcpManager != nil {
		fmt.Printf("\nMCP (Streamable HTTP):\n")
		fmt.Printf("  POST   /mcp                - JSON-RPC 2.0 (tools, resources)\n")
		fmt.Printf("  DELETE /mcp                - End session\n")
	}
	fmt.Printf("\nHealth:\n")
	fmt.Printf("  GET    /health              - Health check\n")
	fmt.Println("\nPress Ctrl+C to stop")

	err = server.ListenAndServe()
	if err != http.ErrServerClosed {
		return err
	}

	<-done

	// Stop MCP session manager cleanup goroutine
	if s.mcpManager != nil {
		s.mcpManager.Stop()
	}

	return nil
}

// === OCI AUTO-RELOAD ===

// startOCIWatcher watches .cog/oci/index.json for changes and triggers re-exec
// when a new digest is detected. Returns a stop function.
func (s *serveServer) startOCIWatcher() func() {
	if s.ociStore == nil || s.reexecCh == nil {
		return func() {}
	}

	indexPath := s.ociStore.IndexPath()
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		log.Printf("[oci] no index.json yet at %s — watcher will detect creation", filepath.Dir(indexPath))
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[oci] fsnotify unavailable: %v, auto-reload disabled", err)
		return func() {}
	}

	// Watch the directory (not the file) because index.json may be atomically
	// replaced (write to tmp + rename), which creates a new inode.
	ociDir := filepath.Dir(indexPath)
	if err := watcher.Add(ociDir); err != nil {
		log.Printf("[oci] cannot watch %s: %v, auto-reload disabled", ociDir, err)
		watcher.Close()
		return func() {}
	}

	stopCh := make(chan struct{})
	go s.runOCIWatcher(watcher, stopCh)

	log.Printf("[oci] watching %s for digest changes", ociDir)
	return func() {
		close(stopCh)
		watcher.Close()
	}
}

// runOCIWatcher is the fsnotify event loop for OCI digest changes.
func (s *serveServer) runOCIWatcher(w *fsnotify.Watcher, stopCh chan struct{}) {
	const debounce = 500 * time.Millisecond
	var timer *time.Timer

	for {
		select {
		case <-stopCh:
			if timer != nil {
				timer.Stop()
			}
			return

		case event, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != "index.json" {
				continue
			}

			// Debounce: oras-go may write index.json multiple times per push
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, func() {
				s.checkOCIDigest()
			})

		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("[oci] fsnotify error: %v", err)
		}
	}
}

// checkOCIDigest compares the latest OCI digest against the running digest.
// If different, pulls the new binary and signals re-exec.
func (s *serveServer) checkOCIDigest() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Compare layer (binary) digests, not manifest digests — manifests change
	// on every push due to timestamp annotations even if the binary is identical
	newDigest, err := s.ociStore.ResolveLayerDigest(ctx)
	if err != nil {
		log.Printf("[oci] resolve failed: %v (keeping current binary)", err)
		return
	}

	if newDigest == "" || newDigest == s.ociDigest {
		return
	}

	digestShort := newDigest
	if len(digestShort) > 23 {
		digestShort = digestShort[:23]
	}
	oldShort := s.ociDigest
	if len(oldShort) > 23 {
		oldShort = oldShort[:23]
	}
	log.Printf("[oci] new binary detected: %s (was %s)", digestShort, oldShort)

	// Pull binary to self-path
	selfPath, err := os.Executable()
	if err != nil {
		log.Printf("[oci] cannot determine self path: %v", err)
		return
	}
	selfPath, err = filepath.EvalSymlinks(selfPath)
	if err != nil {
		log.Printf("[oci] cannot resolve symlinks: %v", err)
		return
	}

	pulledDigest, err := s.ociStore.Pull(ctx, selfPath)
	if err != nil {
		log.Printf("[oci] pull failed: %v (keeping current binary)", err)
		return
	}

	log.Printf("[oci] pulled new kernel to %s (layer digest: %s)", selfPath, pulledDigest[:min(23, len(pulledDigest))])

	// Signal re-exec
	select {
	case s.reexecCh <- newDigest:
	default:
		// Already signaled
	}
}

func (s *serveServer) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || strings.HasPrefix(origin, "http://localhost") || strings.HasPrefix(origin, "http://127.0.0.1") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "http://localhost:5100")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *serveServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	endpoints := []string{
		"POST /v1/chat/completions - OpenAI-compatible inference",
		"GET /v1/models - List models",
		"GET /v1/providers - List providers with health status",
		"GET /v1/requests - List in-flight requests",
		"DELETE /v1/requests/:id - Cancel request",
		"GET /v1/card - Kernel capability card",
		"GET /v1/sessions - List sessions with context metadata",
		"GET /v1/sessions/:id/context - Per-session context state",
		"GET /health - Health check",
	}

	// Add MCP endpoint if available
	if s.mcpManager != nil {
		endpoints = append(endpoints,
			"POST /mcp - MCP Streamable HTTP (JSON-RPC 2.0)",
		)
	}

	// Add SDK endpoints if kernel is available
	if s.kernel != nil {
		endpoints = append(endpoints,
			"GET /resolve?uri=cog://... - Resolve any cog:// URI",
			"POST /mutate - Apply mutations",
			"GET /ws/watch?uri=cog://... - WebSocket watch",
			"GET /state - Workspace state",
			"GET /signals - Signal field",
		)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"name":      "CogOS Unified Server",
		"version":   Version,
		"sdk":       s.kernel != nil,
		"endpoints": endpoints,
	})
}

// handleCard returns the Kernel Card — a self-describing capability manifest (ADR-048).
func (s *serveServer) handleCard(w http.ResponseWriter, r *http.Request) {
	hasMCP := s.mcpManager != nil
	hasSDK := s.kernel != nil

	endpoints := map[string]string{
		"inference": "/v1/chat/completions",
		"models":    "/v1/models",
		"providers": "/v1/providers",
		"sessions":  "/v1/sessions",
		"health":    "/health",
	}
	if hasMCP {
		endpoints["mcp"] = "/mcp"
	}
	if hasSDK {
		endpoints["resolve"] = "/resolve"
		endpoints["mutate"] = "/mutate"
	}

	// Build workspace directory with per-workspace MCP URLs
	wsDir := make(map[string]any, len(s.workspaces))
	for name := range s.workspaces {
		wsEntry := map[string]string{}
		if hasMCP {
			wsEntry["mcp"] = fmt.Sprintf("/mcp?workspace=%s", name)
		}
		wsDir[name] = wsEntry
	}

	card := map[string]any{
		"schemaVersion":    "1.0",
		"name":             "CogOS Kernel",
		"humanReadableId":  "cogos/kernel",
		"description":      "Workspace-aware inference routing with MCP tool access",
		"url":              fmt.Sprintf("http://localhost:%d", s.port),
		"version":          Version,
		"protocolVersions": []string{"mcp/2025-03-26", "openai/v1"},
		"provider": map[string]string{
			"name": "CogOS",
		},
		"capabilities": map[string]bool{
			"inference": true,
			"mcp":       hasMCP,
			"sdk":       hasSDK,
		},
		"endpoints":  endpoints,
		"workspaces": wsDir,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(card)
}

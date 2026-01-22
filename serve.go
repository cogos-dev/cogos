// Serve Module - OpenAI-compatible inference endpoint wrapping Claude CLI
//
// This module provides an HTTP server with OpenAI-compatible endpoints:
// - POST /v1/chat/completions - Chat completions (streaming & non-streaming)
// - GET /v1/models - List available models
// - GET /v1/requests - List in-flight requests
// - DELETE /v1/requests/:id - Cancel a request
// - GET /v1/taa - TAA context visibility (debugging)
// - GET /health - Health check
//
// Daemon management commands:
// - cog serve           - Run server in foreground (existing behavior)
// - cog serve start     - Start server as background process
// - cog serve stop      - Stop the background server
// - cog serve status    - Show running status, PID, port, uptime, request count
// - cog serve enable    - Register with launchd for auto-start on login
// - cog serve disable   - Remove from launchd
//
// The server uses the shared inference engine from inference.go.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	sdk "github.com/cogos-dev/cogos/sdk"
	"github.com/cogos-dev/cogos/sdk/httputil"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// === CONFIGURATION ===
// Port assignments are defined in cog://conf/ports (canonical registry)
// See: .cog/conf/ports.cog.md for port policy and ranges

const (
	defaultServePort = 5100 // Registered: cog://conf/ports#kernel
	claudeCommand    = "claude"
	launchdLabel     = "com.cogos.kernel"
)

// === DAEMON MANAGEMENT ===

// getDaemonPaths returns paths for PID file and log file
func getDaemonPaths() (pidFile, logFile, stateDir string, err error) {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return "", "", "", fmt.Errorf("no workspace found (run from workspace or use -w flag): %w", err)
	}
	stateDir = filepath.Join(root, ".cog", "run")
	pidFile = filepath.Join(stateDir, "serve.pid")
	logDir := filepath.Join(root, ".cog", "logs")
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
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, err
	}
	// Parse the output (format: "Wed Jan  8 10:30:00 2025")
	timeStr := strings.TrimSpace(string(out))
	if timeStr == "" {
		return time.Time{}, fmt.Errorf("empty output")
	}
	// Try parsing with standard format
	t, err := time.Parse("Mon Jan _2 15:04:05 2006", timeStr)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// === OPENAI API TYPES ===

// ChatCompletionRequest represents an OpenAI-format chat completion request
type ChatCompletionRequest struct {
	Model            string           `json:"model"`
	Messages         []ChatMessage    `json:"messages"`
	Stream           bool             `json:"stream,omitempty"`
	Temperature      *float64         `json:"temperature,omitempty"`
	MaxTokens        *int             `json:"max_tokens,omitempty"`
	ResponseFormat   *ResponseFormat  `json:"response_format,omitempty"`
	SystemPrompt     string           `json:"system_prompt,omitempty"` // Extension for explicit system
	TAA              json.RawMessage  `json:"taa,omitempty"`           // TAA context: false/absent=none, true=default, "name"=profile
}

// GetTAAProfile parses the TAA field and returns the profile name to use.
// Returns: ("", false) for no TAA, ("default", true) for taa:true, ("name", true) for taa:"name"
func (r *ChatCompletionRequest) GetTAAProfile() (string, bool) {
	if len(r.TAA) == 0 {
		return "", false
	}

	// Try parsing as boolean
	var boolVal bool
	if err := json.Unmarshal(r.TAA, &boolVal); err == nil {
		if boolVal {
			return "default", true
		}
		return "", false
	}

	// Try parsing as string (profile name)
	var strVal string
	if err := json.Unmarshal(r.TAA, &strVal); err == nil && strVal != "" {
		return strVal, true
	}

	return "", false
}

// GetTAAProfileWithHeader checks both the request body TAA field and the X-TAA-Profile header.
// Header takes precedence over body field if both are present.
func (r *ChatCompletionRequest) GetTAAProfileWithHeader(header string) (string, bool) {
	// Header takes precedence
	if header != "" {
		return header, true
	}
	// Fall back to body field
	return r.GetTAAProfile()
}

// ChatMessage represents a single message in the conversation
// Content can be either a string or an array of content parts (OpenAI SDK format)
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // Can be string or array
}

// GetContent extracts the text content from a ChatMessage
// Handles both string format and array-of-parts format (OpenAI SDK)
func (m *ChatMessage) GetContent() string {
	if len(m.Content) == 0 {
		return ""
	}

	// Try to unmarshal as a simple string first
	var strContent string
	if err := json.Unmarshal(m.Content, &strContent); err == nil {
		return strContent
	}

	// Try to unmarshal as an array of content parts
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var result strings.Builder
		for _, part := range parts {
			if part.Type == "text" || part.Type == "" {
				result.WriteString(part.Text)
			}
		}
		return result.String()
	}

	// Fallback: return raw content as string
	return string(m.Content)
}

// StringToRawContent converts a string to json.RawMessage for ChatMessage.Content
func StringToRawContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return json.RawMessage(b)
}

// ResponseFormat for JSON schema responses
type ResponseFormat struct {
	Type       string          `json:"type,omitempty"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

// ChatCompletionResponse represents the non-streaming response
type ChatCompletionResponse struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	Created           int64              `json:"created"`
	Model             string             `json:"model"`
	Choices           []ChatChoice       `json:"choices"`
	Usage             *UsageInfo         `json:"usage,omitempty"`
}

// ChatChoice represents a single completion choice
type ChatChoice struct {
	Index        int          `json:"index"`
	Message      *ChatMessage `json:"message,omitempty"`
	Delta        *ChatMessage `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
}

// UsageInfo represents token usage
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk represents a streaming response chunk
type StreamChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
}

// ModelListResponse represents the /v1/models response
type ModelListResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// ModelInfo represents a single model
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ErrorResponse represents an API error
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains error information
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// === CLAUDE CLI TYPES ===

// ClaudeStreamMessage represents a message from Claude's stream-json output
type ClaudeStreamMessage struct {
	Type             string           `json:"type"`
	Subtype          string           `json:"subtype,omitempty"`
	Message          *ClaudeMessage   `json:"message,omitempty"`
	Result           string           `json:"result,omitempty"`
	StructuredOutput json.RawMessage  `json:"structured_output,omitempty"`
	Usage            *ClaudeUsage     `json:"usage,omitempty"`
	ToolUseResult    *ToolUseResultEx `json:"tool_use_result,omitempty"` // For user messages with tool results
}

// ToolUseResultEx contains extended tool result info from Claude CLI
type ToolUseResultEx struct {
	Stdout      string `json:"stdout,omitempty"`
	Stderr      string `json:"stderr,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	IsImage     bool   `json:"isImage,omitempty"`
}

// ClaudeMessage represents the nested message in assistant responses
type ClaudeMessage struct {
	Content    []ClaudeContent `json:"content,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
	Usage      *ClaudeUsage    `json:"usage,omitempty"`
}

// ClaudeContent represents a content block in the message
type ClaudeContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // For tool_use blocks
	Name      string          `json:"name,omitempty"`        // For tool_use blocks (e.g., "StructuredOutput")
	Input     json.RawMessage `json:"input,omitempty"`       // For tool_use blocks - contains structured output JSON
	ToolUseID string          `json:"tool_use_id,omitempty"` // For tool_result blocks
	Content   string          `json:"content,omitempty"`     // For tool_result blocks (the result content)
	IsError   bool            `json:"is_error,omitempty"`    // For tool_result blocks
}

// ClaudeUsage represents token usage info
type ClaudeUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// === RICH STREAMING TYPES ===

// ClaudeStreamEvent represents the event wrapper for --include-partial-messages mode
type ClaudeStreamEvent struct {
	Type  string          `json:"type"`
	Event json.RawMessage `json:"event,omitempty"`
}

// StreamEventData represents the data inside a stream_event
type StreamEventData struct {
	Type         string          `json:"type"` // message_start, content_block_start, content_block_delta, etc.
	Index        int             `json:"index,omitempty"`
	ContentBlock *ContentBlock   `json:"content_block,omitempty"`
	Delta        *DeltaContent   `json:"delta,omitempty"`
	Message      json.RawMessage `json:"message,omitempty"`
}

// ContentBlock represents a content block in streaming
type ContentBlock struct {
	Type  string          `json:"type"` // text, tool_use
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`    // for tool_use
	Name  string          `json:"name,omitempty"`  // for tool_use
	Input json.RawMessage `json:"input,omitempty"` // for tool_use
}

// DeltaContent represents delta content in streaming
type DeltaContent struct {
	Type        string `json:"type"` // text_delta, input_json_delta
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

// === SERVER ===

// DebugMode controls verbose logging in inference
var DebugMode = false

type serveServer struct {
	port          int
	kernel        *sdk.Kernel
	lastTAAState  *ContextState // Most recent TAA context for debugging
	taaStateMutex sync.RWMutex
}

func newServeServer(port int, kernel *sdk.Kernel) *serveServer {
	return &serveServer{port: port, kernel: kernel}
}

// Start begins the HTTP server
func (s *serveServer) Start() error {
	mux := http.NewServeMux()

	// Inference routes (keep custom streaming implementation)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/requests", s.handleRequests)
	mux.HandleFunc("/v1/requests/", s.handleRequestByID)
	mux.HandleFunc("/v1/taa", s.handleTAA) // TAA context visibility endpoint
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/debug", s.handleDebug)
	mux.HandleFunc("/services", s.handleServices)

	// SDK routes (universal cog:// access)
	if s.kernel != nil {
		mux.HandleFunc("GET /resolve", s.handleResolve)
		mux.HandleFunc("POST /mutate", s.handleMutate)
		mux.HandleFunc("GET /ws/watch", s.handleWatch)
		// Whirlpool endpoints via SDK
		mux.HandleFunc("GET /state", s.handleState)
		mux.HandleFunc("GET /signals", s.handleSignals)
	}

	mux.HandleFunc("/", s.handleRoot)

	addr := fmt.Sprintf(":%d", s.port)

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
		Handler:      s.corsMiddleware(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // Long timeout for streaming
	}

	// Graceful shutdown
	done := make(chan bool)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		fmt.Println("\nShutting down server...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		server.Shutdown(ctx)
		close(done)
	}()

	fmt.Printf("CogOS unified server starting on http://localhost:%d\n", s.port)
	fmt.Printf("\nInference (OpenAI-compatible):\n")
	fmt.Printf("  POST   /v1/chat/completions - Chat completions\n")
	fmt.Printf("  GET    /v1/models           - List models\n")
	fmt.Printf("  GET    /v1/requests         - List in-flight requests\n")
	fmt.Printf("  DELETE /v1/requests/:id     - Cancel a request\n")
	fmt.Printf("  GET    /v1/taa              - TAA context visibility\n")
	if s.kernel != nil {
		fmt.Printf("\nSDK (universal cog:// access):\n")
		fmt.Printf("  GET    /resolve?uri=cog://... - Resolve any URI\n")
		fmt.Printf("  POST   /mutate                - Apply mutations\n")
		fmt.Printf("  GET    /ws/watch?uri=cog://...  - WebSocket watch\n")
		fmt.Printf("\nWhirlpool (widget state):\n")
		fmt.Printf("  GET    /state               - Workspace state\n")
		fmt.Printf("  GET    /signals             - Signal field\n")
	}
	fmt.Printf("\nHealth:\n")
	fmt.Printf("  GET    /health              - Health check\n")
	fmt.Println("\nPress Ctrl+C to stop")

	err = server.ListenAndServe()
	if err != http.ErrServerClosed {
		return err
	}

	<-done
	return nil
}

func (s *serveServer) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

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
		"GET /v1/requests - List in-flight requests",
		"DELETE /v1/requests/:id - Cancel request",
		"GET /health - Health check",
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
		"name":       "CogOS Unified Server",
		"version":    Version,
		"sdk":        s.kernel != nil,
		"endpoints":  endpoints,
	})
}

// handleTAA returns the TAA context state for debugging/visibility
// This allows clients like cogcode to see what context was constructed
func (s *serveServer) handleTAA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	s.taaStateMutex.RLock()
	ctx := s.lastTAAState
	s.taaStateMutex.RUnlock()

	if ctx == nil {
		json.NewEncoder(w).Encode(map[string]any{
			"status":  "no_context",
			"message": "No TAA context available (no inference requests yet)",
		})
		return
	}

	// Build tier breakdown
	tiers := make(map[string]any)
	if ctx.Tier1Identity != nil {
		tiers["tier1_identity"] = map[string]any{
			"tokens": ctx.Tier1Identity.Tokens,
			"source": ctx.Tier1Identity.Source,
		}
	}
	if ctx.Tier2Temporal != nil {
		tiers["tier2_temporal"] = map[string]any{
			"tokens": ctx.Tier2Temporal.Tokens,
			"source": ctx.Tier2Temporal.Source,
		}
	}
	if ctx.Tier3Present != nil {
		tiers["tier3_present"] = map[string]any{
			"tokens": ctx.Tier3Present.Tokens,
			"source": ctx.Tier3Present.Source,
		}
	}
	if ctx.Tier4Semantic != nil {
		tiers["tier4_semantic"] = map[string]any{
			"tokens": ctx.Tier4Semantic.Tokens,
			"source": ctx.Tier4Semantic.Source,
		}
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status":          "ok",
		"total_tokens":    ctx.TotalTokens,
		"coherence_score": ctx.CoherenceScore,
		"should_refresh":  ctx.ShouldRefresh,
		"anchor":          ctx.Anchor,
		"goal":            ctx.Goal,
		"tiers":           tiers,
		"timestamp":       nowISO(),
	})
}

func (s *serveServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Check if claude CLI is available
	_, err := exec.LookPath(claudeCommand)
	status := "healthy"
	if err != nil {
		status = "degraded"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":    status,
		"timestamp": nowISO(),
		"claude":    err == nil,
		"debug":     DebugMode,
	})
}

func (s *serveServer) handleDebug(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	switch r.Method {
	case "GET":
		// Return current debug state
		json.NewEncoder(w).Encode(map[string]interface{}{
			"debug": DebugMode,
		})
	case "POST":
		// Toggle or set debug mode
		var req struct {
			Debug *bool `json:"debug"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// Toggle if no body
			DebugMode = !DebugMode
		} else if req.Debug != nil {
			DebugMode = *req.Debug
		} else {
			DebugMode = !DebugMode
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"debug": DebugMode,
		})
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
	}
}

// handleServices provides service status and management via launchd
func (s *serveServer) handleServices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch r.Method {
	case "GET":
		// Return status of all services
		services := s.getServicesStatus()
		json.NewEncoder(w).Encode(services)

	case "POST":
		// Restart a service
		var req struct {
			Service string `json:"service"` // "kernel" or "cog-chat"
			Action  string `json:"action"`  // "restart", "start", "stop"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeError(w, http.StatusBadRequest, "Invalid JSON", "invalid_request")
			return
		}

		// Validate service name
		validServices := map[string]string{
			"kernel":   "com.cogos.kernel",
			"cog-chat": "com.cogos.cog-chat",
		}
		launchdLabel, ok := validServices[req.Service]
		if !ok {
			s.writeError(w, http.StatusBadRequest, "Unknown service: "+req.Service, "invalid_request")
			return
		}

		// Execute launchctl command
		var cmd *exec.Cmd
		uid := os.Getuid()
		switch req.Action {
		case "restart":
			cmd = exec.Command("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, launchdLabel))
		case "start":
			cmd = exec.Command("launchctl", "kickstart", fmt.Sprintf("gui/%d/%s", uid, launchdLabel))
		case "stop":
			cmd = exec.Command("launchctl", "kill", "SIGTERM", fmt.Sprintf("gui/%d/%s", uid, launchdLabel))
		default:
			s.writeError(w, http.StatusBadRequest, "Unknown action: "+req.Action, "invalid_request")
			return
		}

		output, err := cmd.CombinedOutput()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   err.Error(),
				"output":  string(output),
			})
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"service": req.Service,
			"action":  req.Action,
			"output":  string(output),
		})

	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
	}
}

// getServicesStatus checks the status of managed services
// Service ports are registered in cog://conf/ports
func (s *serveServer) getServicesStatus() map[string]interface{} {
	// Port assignments from cog://conf/ports registry
	services := []struct {
		name   string
		label  string
		port   int
		health string
	}{
		{"kernel", "com.cogos.kernel", 5100, "http://localhost:5100/health"},       // cog://conf/ports#kernel
		{"cog-chat", "com.cogos.cog-chat", 8765, "http://localhost:8765/api/health"}, // cog://conf/ports#cog-chat
	}

	result := make(map[string]interface{})
	serviceList := make([]map[string]interface{}, 0)

	for _, svc := range services {
		status := map[string]interface{}{
			"name":     svc.name,
			"label":    svc.label,
			"port":     svc.port,
			"running":  false,
			"healthy":  false,
			"launchd":  false,
			"pid":      nil,
			"exitCode": nil,
		}

		// Check launchd status
		cmd := exec.Command("launchctl", "list", svc.label)
		output, err := cmd.Output()
		if err == nil {
			status["launchd"] = true
			// Parse output: "PID\tStatus\tLabel"
			lines := strings.Split(string(output), "\n")
			if len(lines) > 0 {
				fields := strings.Fields(lines[0])
				if len(fields) >= 2 {
					if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 0 {
						status["pid"] = pid
						status["running"] = true
					}
					if exitCode, err := strconv.Atoi(fields[1]); err == nil {
						status["exitCode"] = exitCode
					}
				}
			}
		}

		// Check health endpoint
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(svc.health)
		if err == nil {
			status["healthy"] = resp.StatusCode == http.StatusOK
			resp.Body.Close()
			status["running"] = true // If health responds, it's running
		}

		serviceList = append(serviceList, status)
	}

	result["services"] = serviceList
	result["timestamp"] = time.Now().Format(time.RFC3339)

	return result
}

func (s *serveServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	response := ModelListResponse{
		Object: "list",
		Data: []ModelInfo{
			{
				ID:      "claude-opus-4-5-20250929",
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "anthropic",
			},
			{
				ID:      "claude-sonnet-4-5-20250929",
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "anthropic",
			},
			{
				ID:      "claude",
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "anthropic",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *serveServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	// Parse request
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), "invalid_request")
		return
	}

	// Parse UCP headers (Universal Context Protocol)
	workspaceRoot := s.kernel.Root()
	ucpContext, err := parseUCPHeaders(r, workspaceRoot)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid UCP headers: "+err.Error(), "invalid_request")
		return
	}

	// Log UCP packet presence
	if ucpContext != nil {
		var packets []string
		if ucpContext.Identity != nil {
			packets = append(packets, "identity")
		}
		if ucpContext.TAA != nil {
			packets = append(packets, "taa")
		}
		if ucpContext.Memory != nil {
			packets = append(packets, "memory")
		}
		if ucpContext.History != nil {
			packets = append(packets, "history")
		}
		if ucpContext.Workspace != nil {
			packets = append(packets, "workspace")
		}
		if len(packets) > 0 {
			log.Printf("[UCP] Request with %d packets: %v", len(packets), packets)
		}
	}

	// Extract system prompt and user messages
	systemPrompt := req.SystemPrompt
	var userPrompt strings.Builder

	for _, msg := range req.Messages {
		content := msg.GetContent()
		switch msg.Role {
		case "system":
			if systemPrompt == "" {
				systemPrompt = content
			} else {
				systemPrompt += "\n\n" + content
			}
		case "user":
			if userPrompt.Len() > 0 {
				userPrompt.WriteString("\n\n")
			}
			userPrompt.WriteString(content)
		case "assistant":
			// Include prior assistant messages as context
			if userPrompt.Len() > 0 {
				userPrompt.WriteString("\n\nAssistant: ")
				userPrompt.WriteString(content)
				userPrompt.WriteString("\n\nUser: ")
			}
		}
	}

	if userPrompt.Len() == 0 {
		s.writeError(w, http.StatusBadRequest, "No user message provided", "invalid_request")
		return
	}

	// Extract session ID for thread persistence and context
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = r.Header.Get("X-Eidolon-ID")
	}

	// Persist user message to thread (substrate-based memory)
	if err := s.appendToThread("user", userPrompt.String(), sessionID); err != nil {
		log.Printf("[TAA] Failed to persist user message: %v", err)
	}

	// Check if TAA context injection is requested
	// Priority: UCP-TAA header > X-TAA-Profile header > body field
	var contextState *ContextState
	var taaProfile string
	var taaEnabled bool

	if ucpContext != nil && ucpContext.TAA != nil {
		// Use UCP-TAA packet (explicit mode)
		taaProfile = ucpContext.TAA.Profile
		taaEnabled = true
		log.Printf("[UCP] Using UCP-TAA packet: profile=%s, total_tokens=%d", taaProfile, ucpContext.TAA.TotalTokens)
	} else {
		// Fall back to legacy TAA header/body field
		taaHeader := r.Header.Get("X-TAA-Profile")
		taaProfile, taaEnabled = req.GetTAAProfileWithHeader(taaHeader)
	}

	if taaEnabled {
		var err error
		contextState, err = ConstructContextStateWithProfile(req.Messages, sessionID, workspaceRoot, taaProfile)
		if err != nil {
			// Log but don't fail - context is optional enhancement
			log.Printf("[TAA] Context construction warning (profile=%s): %v", taaProfile, err)
		} else {
			log.Printf("[TAA] Context loaded: profile=%s, tokens=%d, coherence=%.2f",
				taaProfile, contextState.TotalTokens, contextState.CoherenceScore)

			// Populate UCP response metrics if UCP was used
			if ucpContext != nil && ucpContext.TAA != nil {
				constructedTokens := contextState.TotalTokens
				ucpContext.TAA.ConstructedTokens = &constructedTokens

				// Extract tier token counts from ContextTier structs
				tierBreakdown := make(map[string]int)
				if contextState.Tier1Identity != nil {
					tierBreakdown["tier1"] = contextState.Tier1Identity.Tokens
				}
				if contextState.Tier2Temporal != nil {
					tierBreakdown["tier2"] = contextState.Tier2Temporal.Tokens
				}
				if contextState.Tier3Present != nil {
					tierBreakdown["tier3"] = contextState.Tier3Present.Tokens
				}
				if contextState.Tier4Semantic != nil {
					tierBreakdown["tier4"] = contextState.Tier4Semantic.Tokens
				}
				ucpContext.TAA.TierBreakdown = tierBreakdown
			}
		}
	}

	// Build InferenceRequest using shared engine
	var schema json.RawMessage
	if req.ResponseFormat != nil && len(req.ResponseFormat.JSONSchema) > 0 {
		schema = req.ResponseFormat.JSONSchema
	}

	inferReq := &InferenceRequest{
		Prompt:       userPrompt.String(),
		SystemPrompt: systemPrompt,
		Model:        req.Model,
		Schema:       schema,
		MaxTokens:    req.MaxTokens,
		Origin:       "http",
		Stream:       req.Stream,
		Context:      r.Context(),
		ContextState: contextState,
	}

	// Set UCP response headers if UCP was used
	if ucpContext != nil {
		if err := setUCPResponseHeaders(w, ucpContext); err != nil {
			log.Printf("[UCP] Failed to set response headers: %v", err)
		}
	}

	// Handle streaming vs non-streaming
	if req.Stream {
		s.handleStreamingResponse(w, inferReq, sessionID)
	} else {
		s.handleNonStreamingResponse(w, inferReq, sessionID)
	}
}

func (s *serveServer) handleStreamingResponse(w http.ResponseWriter, inferReq *InferenceRequest, sessionID string) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Add TAA context as headers (before streaming starts, so clients can read immediately)
	if inferReq.ContextState != nil {
		ctx := inferReq.ContextState
		w.Header().Set("X-TAA-Total-Tokens", strconv.Itoa(ctx.TotalTokens))
		w.Header().Set("X-TAA-Coherence", fmt.Sprintf("%.2f", ctx.CoherenceScore))
		if ctx.Anchor != "" {
			w.Header().Set("X-TAA-Anchor", ctx.Anchor)
		}
		if ctx.Goal != "" {
			w.Header().Set("X-TAA-Goal", ctx.Goal)
		}
		if ctx.Tier1Identity != nil {
			w.Header().Set("X-TAA-Tier1-Tokens", strconv.Itoa(ctx.Tier1Identity.Tokens))
		}
		if ctx.Tier2Temporal != nil {
			w.Header().Set("X-TAA-Tier2-Tokens", strconv.Itoa(ctx.Tier2Temporal.Tokens))
		}
		if ctx.Tier3Present != nil {
			w.Header().Set("X-TAA-Tier3-Tokens", strconv.Itoa(ctx.Tier3Present.Tokens))
		}
		if ctx.Tier4Semantic != nil {
			w.Header().Set("X-TAA-Tier4-Tokens", strconv.Itoa(ctx.Tier4Semantic.Tokens))
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "Streaming not supported", "server_error")
		return
	}

	// Store TAA context for /v1/taa endpoint and emit as SSE event
	if inferReq.ContextState != nil {
		s.taaStateMutex.Lock()
		s.lastTAAState = inferReq.ContextState
		s.taaStateMutex.Unlock()
		s.emitTAAContext(w, flusher, inferReq.ContextState, inferReq.Model)
	}

	// Use shared inference engine
	chunks, err := RunInferenceStream(inferReq, GlobalRegistry)
	if err != nil {
		s.writeSSEError(w, flusher, "Failed to start inference: "+err.Error())
		return
	}

	model := inferReq.Model
	if model == "" {
		model = "claude"
	}
	created := time.Now().Unix()

	// Accumulate content for thread persistence
	var accumulatedContent strings.Builder

	// Process chunks from the inference engine
	for chunk := range chunks {
		if chunk.Error != nil {
			s.writeSSEError(w, flusher, "Inference error: "+chunk.Error.Error())
			return
		}

		// Handle rich event types
		// Note: All custom events include empty "choices" array for OpenAI SDK compatibility
		switch chunk.EventType {
		case "session_info", "session_start":
			// Emit session info as custom event
			// Note: We only send tool count, not full definitions (can be 100KB+)
			if chunk.SessionInfo != nil {
				toolCount := 0
				if chunk.SessionInfo.Tools != nil {
					toolCount = len(chunk.SessionInfo.Tools)
				}
				sessionChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{}, // Required for OpenAI SDK compatibility
					"event_type": chunk.EventType,
					"session": map[string]any{
						"session_id": chunk.SessionInfo.SessionID,
						"model":      chunk.SessionInfo.Model,
						"tool_count": toolCount,
					},
				}
				data, _ := json.Marshal(sessionChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			continue

		case "tool_use", "tool_use_start":
			// Emit tool call as custom event
			if chunk.ToolCall != nil {
				toolChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{}, // Required for OpenAI SDK compatibility
					"event_type": "tool_call",
					"tool_call": map[string]any{
						"id":        chunk.ToolCall.ID,
						"name":      chunk.ToolCall.Name,
						"arguments": chunk.ToolCall.Arguments,
					},
				}
				data, _ := json.Marshal(toolChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			continue

		case "tool_use_delta":
			// Emit tool call delta (partial args)
			if chunk.ToolCall != nil {
				deltaChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{}, // Required for OpenAI SDK compatibility
					"event_type": "tool_call_delta",
					"tool_call": map[string]any{
						"id":              chunk.ToolCall.ID,
						"name":            chunk.ToolCall.Name,
						"arguments_delta": string(chunk.ToolCall.Arguments),
					},
				}
				data, _ := json.Marshal(deltaChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			continue
		}

		// Handle tool result
		if chunk.ToolResult != nil {
			resultChunk := map[string]any{
				"id":         chunk.ID,
				"object":     "chat.completion.chunk",
				"created":    created,
				"model":      model,
				"choices":    []any{}, // Required for OpenAI SDK compatibility
				"event_type": "tool_result",
				"tool_result": map[string]any{
					"tool_call_id": chunk.ToolResult.ToolCallID,
					"content":      chunk.ToolResult.Content,
					"is_error":     chunk.ToolResult.IsError,
				},
			}
			data, _ := json.Marshal(resultChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		if chunk.Content != "" {
			// Accumulate content for thread persistence
			accumulatedContent.WriteString(chunk.Content)

			// Content chunk
			openAIChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{
					{
						Index: 0,
						Delta: &ChatMessage{
							Role:    "assistant",
							Content: StringToRawContent(chunk.Content),
						},
					},
				},
			}
			data, _ := json.Marshal(openAIChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		if chunk.Done {
			// Persist accumulated assistant response to thread
			if accumulatedContent.Len() > 0 {
				if err := s.appendToThread("assistant", accumulatedContent.String(), sessionID); err != nil {
					log.Printf("[TAA] Failed to persist streamed assistant response: %v", err)
				}
				// Check if thread needs summarization
				s.checkSummarization(sessionID)
			}

			// Emit usage stats if available
			if chunk.Usage != nil {
				usageChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{}, // Required for OpenAI SDK compatibility
					"event_type": "usage",
					"usage": map[string]any{
						"input_tokens":  chunk.Usage.InputTokens,
						"output_tokens": chunk.Usage.OutputTokens,
						"cost_usd":      chunk.Usage.CostUSD,
					},
				}
				data, _ := json.Marshal(usageChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			// Final chunk with finish reason
			finishReason := chunk.FinishReason
			if finishReason == "" {
				finishReason = "stop"
			}
			openAIChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{
					{
						Index:        0,
						Delta:        &ChatMessage{},
						FinishReason: finishReason,
					},
				},
			}
			data, _ := json.Marshal(openAIChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
	}
}

func (s *serveServer) handleNonStreamingResponse(w http.ResponseWriter, inferReq *InferenceRequest, sessionID string) {
	// Store TAA context for /v1/taa endpoint
	if inferReq.ContextState != nil {
		s.taaStateMutex.Lock()
		s.lastTAAState = inferReq.ContextState
		s.taaStateMutex.Unlock()
	}

	// Use shared inference engine
	resp, err := RunInference(inferReq, GlobalRegistry)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Inference failed: "+err.Error(), "server_error")
		return
	}

	// Persist assistant response to thread (substrate-based memory)
	if err := s.appendToThread("assistant", resp.Content, sessionID); err != nil {
		log.Printf("[TAA] Failed to persist assistant response: %v", err)
	}

	// Check if thread needs summarization
	s.checkSummarization(sessionID)

	model := inferReq.Model
	if model == "" {
		model = "claude"
	}

	usage := UsageInfo{
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		TotalTokens:      resp.PromptTokens + resp.CompletionTokens,
	}

	finishReason := resp.FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}

	// Build response
	response := ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatChoice{
			{
				Index: 0,
				Message: &ChatMessage{
					Role:    "assistant",
					Content: StringToRawContent(resp.Content),
				},
				FinishReason: finishReason,
			},
		},
		Usage: &usage,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleRequests handles GET /v1/requests - list in-flight requests
func (s *serveServer) handleRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	// Get query parameter for filtering
	statusFilter := r.URL.Query().Get("status")

	var entries []*RequestEntry
	if statusFilter == "running" {
		entries = GlobalRegistry.ListRunning()
	} else {
		entries = GlobalRegistry.List()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   entries,
		"count":  len(entries),
	})
}

// handleRequestByID handles GET/DELETE /v1/requests/:id
func (s *serveServer) handleRequestByID(w http.ResponseWriter, r *http.Request) {
	// Extract request ID from path
	path := strings.TrimPrefix(r.URL.Path, "/v1/requests/")
	requestID := strings.TrimSuffix(path, "/")

	if requestID == "" {
		s.writeError(w, http.StatusBadRequest, "Request ID required", "invalid_request")
		return
	}

	switch r.Method {
	case "GET":
		// Get specific request
		entry := GlobalRegistry.Get(requestID)
		if entry == nil {
			s.writeError(w, http.StatusNotFound, "Request not found", "not_found")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entry)

	case "DELETE":
		// Cancel request
		if GlobalRegistry.Cancel(requestID) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":        requestID,
				"cancelled": true,
			})
		} else {
			s.writeError(w, http.StatusNotFound, "Request not found or already completed", "not_found")
		}

	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
	}
}

func (s *serveServer) writeError(w http.ResponseWriter, status int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ErrorResponse{
		Error: ErrorDetail{
			Message: message,
			Type:    errType,
		},
	})
}

func (s *serveServer) writeSSEError(w http.ResponseWriter, flusher http.Flusher, message string) {
	errResp := ErrorResponse{
		Error: ErrorDetail{
			Message: message,
			Type:    "server_error",
		},
	}
	data, _ := json.Marshal(errResp)
	fmt.Fprintf(w, "data: %s\n\n", data)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// emitTAAContext emits the TAA context state as an SSE event for debugging/visibility
// This allows clients like cogcode to display the constructed context tiers
func (s *serveServer) emitTAAContext(w http.ResponseWriter, flusher http.Flusher, ctx *ContextState, model string) {
	if ctx == nil {
		return
	}

	// Build tier breakdown
	tiers := make(map[string]any)

	if ctx.Tier1Identity != nil {
		tiers["tier1_identity"] = map[string]any{
			"tokens": ctx.Tier1Identity.Tokens,
			"source": ctx.Tier1Identity.Source,
		}
	}

	if ctx.Tier2Temporal != nil {
		tiers["tier2_temporal"] = map[string]any{
			"tokens": ctx.Tier2Temporal.Tokens,
			"source": ctx.Tier2Temporal.Source,
		}
	}

	if ctx.Tier3Present != nil {
		tiers["tier3_present"] = map[string]any{
			"tokens": ctx.Tier3Present.Tokens,
			"source": ctx.Tier3Present.Source,
		}
	}

	if ctx.Tier4Semantic != nil {
		tiers["tier4_semantic"] = map[string]any{
			"tokens": ctx.Tier4Semantic.Tokens,
			"source": ctx.Tier4Semantic.Source,
		}
	}

	// Build TAA context event
	taaEvent := map[string]any{
		"id":         fmt.Sprintf("taa-%d", time.Now().UnixNano()),
		"object":     "chat.completion.chunk",
		"created":    time.Now().Unix(),
		"model":      model,
		"choices":    []any{}, // Required for OpenAI SDK compatibility
		"event_type": "taa_context",
		"taa": map[string]any{
			"total_tokens":    ctx.TotalTokens,
			"coherence_score": ctx.CoherenceScore,
			"tiers":           tiers,
			"anchor":          ctx.Anchor,
			"goal":            ctx.Goal,
		},
	}

	data, _ := json.Marshal(taaEvent)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// === THREAD PERSISTENCE ===

// appendToThread appends a message to the conversation thread via SDK.
// This enables substrate-based memory: threads persist across sessions and devices.
func (s *serveServer) appendToThread(role, content, sessionID string) error {
	if s.kernel == nil {
		return nil // Thread persistence requires SDK
	}

	// Construct message
	msg := map[string]interface{}{
		"role":      role,
		"content":   content,
		"timestamp": time.Now().Format(time.RFC3339),
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Determine thread URI
	threadURI := "cog://thread/current"
	if sessionID != "" {
		threadURI = "cog://thread/" + sessionID
	}

	// Append via SDK
	mutation := sdk.NewAppendMutation(msgBytes)
	return s.kernel.MutateContext(context.Background(), threadURI, mutation)
}

// checkSummarization checks if thread needs summarization.
// Logs a recommendation when thread exceeds 12 messages (6 conversational turns).
// Actual summarization is handled by async background tasks.
func (s *serveServer) checkSummarization(sessionID string) {
	if s.kernel == nil {
		return
	}

	// Load thread
	threadURI := "cog://thread/current"
	if sessionID != "" {
		threadURI = "cog://thread/" + sessionID
	}

	resource, err := s.kernel.ResolveContext(context.Background(), threadURI)
	if err != nil {
		return // No thread yet or error loading
	}

	// Parse thread data
	var thread struct {
		Messages []interface{} `json:"messages"`
	}
	if err := json.Unmarshal(resource.Content, &thread); err != nil {
		return
	}

	// Check if we need summarization (>12 messages = >6 turns)
	if len(thread.Messages) > 12 {
		log.Printf("[TAA] Thread %s has %d messages, summarization recommended", sessionID, len(thread.Messages))
		// TODO: Trigger async summarization task
		// This will be implemented by the Memory Integration Specialist
	}
}

// === SDK ROUTES ===
// These handlers delegate to the SDK kernel for universal cog:// access.

// handleResolve handles GET /resolve?uri=cog://...
func (s *serveServer) handleResolve(w http.ResponseWriter, r *http.Request) {
	if s.kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	uri := r.URL.Query().Get("uri")
	if uri == "" {
		s.writeError(w, http.StatusBadRequest, "missing 'uri' query parameter", "invalid_request")
		return
	}

	resource, err := s.kernel.ResolveContext(r.Context(), uri)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "invalid") {
			status = http.StatusBadRequest
		}
		s.writeError(w, status, err.Error(), "resolve_error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resource)
}

// handleMutate handles POST /mutate
func (s *serveServer) handleMutate(w http.ResponseWriter, r *http.Request) {
	if s.kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	var req httputil.MutateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request")
		return
	}

	if req.URI == "" {
		s.writeError(w, http.StatusBadRequest, "missing 'uri' field", "invalid_request")
		return
	}

	// Convert to SDK mutation
	var mutation *sdk.Mutation
	content := []byte(req.Content)
	switch sdk.MutationOp(req.Op) {
	case sdk.MutationSet:
		mutation = sdk.NewSetMutation(content)
	case sdk.MutationPatch:
		mutation = sdk.NewPatchMutation(content)
	case sdk.MutationAppend:
		mutation = sdk.NewAppendMutation(content)
	case sdk.MutationDelete:
		mutation = sdk.NewDeleteMutation()
	default:
		s.writeError(w, http.StatusBadRequest, "invalid 'op' field: "+req.Op, "invalid_request")
		return
	}

	if req.Metadata != nil {
		for k, v := range req.Metadata {
			mutation.WithMetadata(k, v)
		}
	}

	if err := s.kernel.MutateContext(r.Context(), req.URI, mutation); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "read-only") {
			status = http.StatusMethodNotAllowed
		}
		s.writeError(w, status, err.Error(), "mutate_error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"uri":     req.URI,
	})
}

// handleWatch handles GET /ws/watch?uri=cog://... (WebSocket)
func (s *serveServer) handleWatch(w http.ResponseWriter, r *http.Request) {
	if s.kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	uri := r.URL.Query().Get("uri")
	if uri == "" {
		s.writeError(w, http.StatusBadRequest, "missing 'uri' query parameter", "invalid_request")
		return
	}

	// Upgrade to WebSocket
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to upgrade: "+err.Error(), "server_error")
		return
	}
	defer c.CloseNow()

	// Create watcher
	ctx := r.Context()
	watcher, err := s.kernel.WatchURI(ctx, uri)
	if err != nil {
		c.Close(websocket.StatusInternalError, err.Error())
		return
	}
	defer watcher.Close()

	// Send initial message
	wsjson.Write(ctx, c, map[string]any{
		"type":    "connected",
		"uri":     uri,
		"message": "watching for changes",
	})

	// Forward events to WebSocket
	for {
		select {
		case <-ctx.Done():
			c.Close(websocket.StatusNormalClosure, "context cancelled")
			return
		case event, ok := <-watcher.Events:
			if !ok {
				c.Close(websocket.StatusNormalClosure, "watcher closed")
				return
			}

			msg := map[string]any{
				"type":      "event",
				"uri":       event.URI,
				"eventType": event.Type,
				"timestamp": event.Timestamp,
			}
			if event.Resource != nil {
				msg["resource"] = event.Resource
			}

			if err := wsjson.Write(ctx, c, msg); err != nil {
				return
			}
		}
	}
}

// handleState returns full workspace state via SDK
func (s *serveServer) handleState(w http.ResponseWriter, r *http.Request) {
	if s.kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	// Resolve cog://status for full workspace state
	resource, err := s.kernel.Resolve("cog://status")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "resolve_error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resource)
}

// handleSignals returns signal field via SDK
func (s *serveServer) handleSignals(w http.ResponseWriter, r *http.Request) {
	if s.kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	// Resolve cog://signals for signal field
	resource, err := s.kernel.Resolve("cog://signals")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "resolve_error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resource)
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
	// Check if claude CLI is available
	if _, err := exec.LookPath(claudeCommand); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Claude CLI not found in PATH\n")
		fmt.Fprintf(os.Stderr, "Install: npm install -g @anthropic-ai/claude-code\n")
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
	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
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

	// Open log file
	logOut, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening log file: %v\n", err)
		return 1
	}

	// Build command to run serve in foreground mode
	cmd := exec.Command(cogBinary, "serve", "--port", strconv.Itoa(port))
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
	fmt.Printf("Log file: %s\n", logFile)

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

	fmt.Printf("Log file:    %s\n", logFile)
	fmt.Printf("PID file:    %s\n", pidFile)

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

	// Load with launchctl
	cmd := exec.Command("launchctl", "load", plistPath)
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

	// Unload with launchctl
	cmd := exec.Command("launchctl", "unload", plistPath)
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
  GET    /v1/requests           List in-flight requests
  GET    /v1/requests/:id       Get specific request status
  DELETE /v1/requests/:id       Cancel a request
  GET    /v1/taa                TAA context visibility (debugging)

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

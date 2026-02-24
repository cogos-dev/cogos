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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	sdk "github.com/cogos-dev/cogos/sdk"
	"github.com/cogos-dev/cogos/sdk/httputil"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
	Model          string          `json:"model"`
	Messages       []ChatMessage   `json:"messages"`
	Stream         bool            `json:"stream,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	SystemPrompt   string          `json:"system_prompt,omitempty"` // Extension for explicit system
	TAA            json.RawMessage `json:"taa,omitempty"`           // TAA context: false/absent=none, true=default, "name"=profile
	Tools          []json.RawMessage `json:"tools,omitempty"`       // OpenAI-format tool definitions
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
	Content    json.RawMessage `json:"content"`               // Can be string or array
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`  // Assistant tool call requests
	ToolCallID string          `json:"tool_call_id,omitempty"` // For role:"tool" — which call this answers
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

// GetToolCallsSummary extracts a text summary of tool calls from an assistant message.
// Returns empty string if no tool calls are present.
func (m *ChatMessage) GetToolCallsSummary() string {
	if len(m.ToolCalls) == 0 {
		return ""
	}
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, call := range calls {
		if call.Function.Name != "" {
			sb.WriteString("[Tool call: ")
			sb.WriteString(call.Function.Name)
			// Include a truncated version of args for context
			args := call.Function.Arguments
			if len(args) > 200 {
				args = args[:200] + "..."
			}
			if args != "" {
				sb.WriteString("(")
				sb.WriteString(args)
				sb.WriteString(")")
			}
			sb.WriteString("]")
		}
	}
	return sb.String()
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
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *UsageInfo   `json:"usage,omitempty"`
}

// ChatChoice represents a single completion choice
type ChatChoice struct {
	Index        int          `json:"index"`
	Message      *ChatMessage `json:"message,omitempty"`
	Delta        *ChatMessage `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
}


// UsageInfo represents token usage in the OpenAI-compatible response format.
// The cache and cost fields are Anthropic extensions — they're omitted for
// non-Claude providers, so standard OpenAI clients ignore them gracefully.
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// Anthropic extensions (omitted when zero)
	CacheReadTokens   int     `json:"cache_read_input_tokens,omitempty"`
	CacheCreateTokens int     `json:"cache_creation_input_tokens,omitempty"`
	CostUSD           float64 `json:"cost_usd,omitempty"`
}

// StreamChunk represents a streaming response chunk
type StreamChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *UsageInfo   `json:"usage,omitempty"`
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

// === PROVIDER API TYPES (ADR-046) ===

// ProviderHealth represents the health status of a provider
type ProviderHealth struct {
	LastCheck *string `json:"last_check"` // ISO8601 timestamp or null
	LatencyMs *int    `json:"latency_ms"` // Latency in ms or null
	Error     *string `json:"error"`      // Error message or null
}

// ProviderInfo represents a single provider in the API response
type ProviderInfo struct {
	ID     string               `json:"id"`
	Name   string               `json:"name"`
	Status string               `json:"status"` // "online", "offline", "unknown", "degraded"
	Active bool                 `json:"active"`
	Models []string             `json:"models"`
	Config ProviderPublicConfig `json:"config"`
	Health ProviderHealth       `json:"health"`
}

// ProviderPublicConfig represents publicly-visible provider configuration
// Note: API keys are never exposed, only has_api_key boolean
type ProviderPublicConfig struct {
	BaseURL   string `json:"base_url"`
	HasAPIKey bool   `json:"has_api_key"`
}

// ProviderListResponse represents the /v1/providers response
type ProviderListResponse struct {
	Object        string         `json:"object"`
	Data          []ProviderInfo `json:"data"`
	Active        string         `json:"active"`
	FallbackChain []string       `json:"fallback_chain"`
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
	workspaces    map[string]*workspaceContext  // name → workspace context
	defaultWS     string                       // default workspace name
	lastTAAState  *ContextState                // Most recent TAA context for debugging
	taaStateMutex sync.RWMutex
	busChat       *busChat        // Bus event emission for chat (nil if no workspace)
	busBroker     *busEventBroker // SSE subscriber broker for bus events
}

func newServeServer(port int, kernel *sdk.Kernel) *serveServer {
	return &serveServer{port: port, kernel: kernel, busBroker: newBusEventBroker()}
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
	mux.HandleFunc("/v1/taa", s.handleTAA) // TAA context visibility endpoint
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/debug", s.handleDebug)
	mux.HandleFunc("/services", s.handleServices)

	// Bus event streaming (SSE) and REST endpoints
	mux.HandleFunc("GET /v1/events/stream", s.handleEventsStream)
	mux.HandleFunc("GET /v1/bus/", s.handleBusEventsREST)

	// Bus messaging API (inter-workspace)
	mux.HandleFunc("POST /v1/bus/send", s.handleBusSend)
	mux.HandleFunc("POST /v1/bus/open", s.handleBusOpen)
	mux.HandleFunc("GET /v1/bus/list", s.handleBusList)

	// SDK routes (universal cog:// access)
	if s.kernel != nil {
		mux.HandleFunc("GET /resolve", s.handleResolve)
		mux.HandleFunc("POST /mutate", s.handleMutate)
		mux.HandleFunc("GET /ws/watch", s.handleWatch)
		// Whirlpool endpoints via SDK
		mux.HandleFunc("GET /state", s.handleState)
		mux.HandleFunc("GET /signals", s.handleSignals)
	}

	// CogField graph endpoint
	mux.HandleFunc("GET /api/cogfield/graph", s.handleCogFieldGraph)
	mux.HandleFunc("GET /api/cogfield/query", s.handleCogFieldQuery)
	mux.HandleFunc("/api/cogfield/sessions/", s.handleSessionDetail)
	mux.HandleFunc("/api/cogfield/buses/", s.handleBusDetail)
	mux.HandleFunc("/api/cogfield/expand/", s.handleExpandNode)
	mux.HandleFunc("/api/cogfield/documents/", s.handleDocumentDetail)

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

	// Graceful shutdown
	done := make(chan bool, 1)
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
	fmt.Printf("  GET    /v1/providers        - List providers with health (ADR-046)\n")
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
		origin := r.Header.Get("Origin")
		if origin == "" || strings.HasPrefix(origin, "http://localhost") || strings.HasPrefix(origin, "http://127.0.0.1") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "http://localhost:5100")
		}
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
		"GET /v1/providers - List providers with health status",
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
		"name":      "CogOS Unified Server",
		"version":   Version,
		"sdk":       s.kernel != nil,
		"endpoints": endpoints,
	})
}

// handleTAA returns the TAA context state for debugging/visibility
// This allows clients like cogcode to see what context was constructed
func (s *serveServer) handleTAA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// CORS is handled by corsMiddleware

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
		"debug":     DebugMode.Load(),
	})
}

func (s *serveServer) handleDebug(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// CORS is handled by corsMiddleware

	switch r.Method {
	case "GET":
		// Return current debug state
		json.NewEncoder(w).Encode(map[string]interface{}{
			"debug": DebugMode.Load(),
		})
	case "POST":
		// Toggle or set debug mode
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10) // 64KB limit
		var req struct {
			Debug *bool `json:"debug"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// Toggle if no body (or body too large)
			DebugMode.Store(!DebugMode.Load())
		} else if req.Debug != nil {
			DebugMode.Store(*req.Debug)
		} else {
			DebugMode.Store(!DebugMode.Load())
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"debug": DebugMode.Load(),
		})
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
	}
}

// handleServices provides service status and management via launchd
func (s *serveServer) handleServices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// CORS is handled by corsMiddleware

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
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10) // 64KB limit
		var req struct {
			Service string `json:"service"` // "kernel" or "cog-chat"
			Action  string `json:"action"`  // "restart", "start", "stop"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), "invalid_request")
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

		// Execute launchctl command with timeout to prevent kernel hang
		var cmd *exec.Cmd
		uid := os.Getuid()
		launchctlCtx, launchctlCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer launchctlCancel()
		switch req.Action {
		case "restart":
			cmd = exec.CommandContext(launchctlCtx, "launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, launchdLabel))
		case "start":
			cmd = exec.CommandContext(launchctlCtx, "launchctl", "kickstart", fmt.Sprintf("gui/%d/%s", uid, launchdLabel))
		case "stop":
			cmd = exec.CommandContext(launchctlCtx, "launchctl", "kill", "SIGTERM", fmt.Sprintf("gui/%d/%s", uid, launchdLabel))
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
		{"kernel", "com.cogos.kernel", 5100, "http://localhost:5100/health"},         // cog://conf/ports#kernel
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

		// Check launchd status (with timeout to prevent kernel hang)
		listCtx, listCancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(listCtx, "launchctl", "list", svc.label)
		output, err := cmd.Output()
		listCancel()
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

// === PROVIDER HEALTH CACHE ===

// providerHealthCache stores cached health check results
type providerHealthCache struct {
	mu      sync.RWMutex
	results map[ProviderType]*ProviderHealth
	checked map[ProviderType]time.Time
}

var healthCache = &providerHealthCache{
	results: make(map[ProviderType]*ProviderHealth),
	checked: make(map[ProviderType]time.Time),
}

// getProviderDisplayName returns a human-readable name for a provider
func getProviderDisplayName(pt ProviderType) string {
	switch pt {
	case ProviderClaude:
		return "Claude CLI"
	case ProviderOpenAI:
		return "OpenAI"
	case ProviderOpenRouter:
		return "OpenRouter"
	case ProviderOllama:
		return "Ollama (Local)"
	case ProviderLocal:
		return "Local Kernel"
	case ProviderCustom:
		return "Custom"
	default:
		return string(pt)
	}
}

// getProviderModels returns the default models for a provider
func getProviderModels(pt ProviderType) []string {
	switch pt {
	case ProviderClaude:
		return []string{"claude-opus-4-5", "claude-sonnet-4-5", "claude"}
	case ProviderOpenAI:
		return []string{"gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "gpt-3.5-turbo"}
	case ProviderOpenRouter:
		return []string{"anthropic/claude-3-haiku", "openai/gpt-4o-mini", "google/gemini-pro"}
	case ProviderOllama:
		return []string{"llama3.2", "mistral", "codellama"}
	case ProviderLocal:
		return []string{"claude"} // Routes to Claude by default
	default:
		return []string{}
	}
}

// checkProviderHealth performs a health check on a provider
// Results are cached for 60 seconds to avoid hammering providers
func checkProviderHealth(pt ProviderType, config *ProviderConfig) *ProviderHealth {
	healthCache.mu.RLock()
	if cached, ok := healthCache.results[pt]; ok {
		if checkedAt, ok := healthCache.checked[pt]; ok {
			if time.Since(checkedAt) < 60*time.Second {
				healthCache.mu.RUnlock()
				return cached
			}
		}
	}
	healthCache.mu.RUnlock()

	// Perform fresh health check
	health := &ProviderHealth{}

	// Special handling for Claude CLI - check if binary exists
	if pt == ProviderClaude {
		_, err := exec.LookPath(claudeCommand)
		now := nowISO()
		health.LastCheck = &now
		if err != nil {
			errMsg := "Claude CLI not found in PATH"
			health.Error = &errMsg
		} else {
			latency := 0 // CLI check is instant
			health.LatencyMs = &latency
		}
		healthCache.mu.Lock()
		healthCache.results[pt] = health
		healthCache.checked[pt] = time.Now()
		healthCache.mu.Unlock()
		return health
	}

	// For HTTP providers, make a lightweight request
	if config == nil || config.BaseURL == "" {
		errMsg := "no configuration"
		health.Error = &errMsg
		now := nowISO()
		health.LastCheck = &now
		// Cache the "no configuration" result to avoid repeated checks
		healthCache.mu.Lock()
		healthCache.results[pt] = health
		healthCache.checked[pt] = time.Now()
		healthCache.mu.Unlock()
		return health
	}

	// Make health check request (GET /models is usually lightweight)
	client := &http.Client{Timeout: 5 * time.Second}
	url := config.BaseURL + "/models"

	start := time.Now()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		now := nowISO()
		health.LastCheck = &now
		errMsg := err.Error()
		health.Error = &errMsg
		return health
	}

	// Add auth header if API key is set
	if config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+config.APIKey)
	}

	// OpenRouter-specific headers
	if pt == ProviderOpenRouter {
		req.Header.Set("HTTP-Referer", "https://cogos.dev")
		req.Header.Set("X-Title", "CogOS Kernel")
	}

	resp, err := client.Do(req)
	latency := int(time.Since(start).Milliseconds())
	now := nowISO()
	health.LastCheck = &now
	health.LatencyMs = &latency

	if err != nil {
		errMsg := err.Error()
		health.Error = &errMsg
	} else {
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			errMsg := "authentication failed"
			health.Error = &errMsg
		} else if resp.StatusCode >= 400 {
			errMsg := fmt.Sprintf("HTTP %d", resp.StatusCode)
			health.Error = &errMsg
		}
		// Success: health.Error remains nil
	}

	// Cache the result
	healthCache.mu.Lock()
	healthCache.results[pt] = health
	healthCache.checked[pt] = time.Now()
	healthCache.mu.Unlock()

	return health
}

// getProviderStatus determines the status string from health info
func getProviderStatus(health *ProviderHealth, hasAPIKey bool, pt ProviderType) string {
	// Claude CLI doesn't need API key check
	if pt == ProviderClaude {
		if health.Error != nil {
			return "offline"
		}
		return "online"
	}

	// For other providers, check if we've done a health check
	if health.LastCheck == nil {
		return "unknown"
	}

	if health.Error != nil {
		// Distinguish between auth errors and connection errors
		if *health.Error == "authentication failed" {
			return "degraded" // We can reach it but auth is wrong
		}
		return "offline"
	}

	return "online"
}

// handleProviders handles GET /v1/providers - list all configured providers
func (s *serveServer) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	// Get current active provider
	currentActive := getActiveProvider()

	// Get default providers from inference.go
	providers := DefaultProviders()

	// Build response
	var data []ProviderInfo

	// Always include Claude CLI as first provider
	claudeHealth := checkProviderHealth(ProviderClaude, nil)
	claudeStatus := getProviderStatus(claudeHealth, true, ProviderClaude)
	data = append(data, ProviderInfo{
		ID:     string(ProviderClaude),
		Name:   getProviderDisplayName(ProviderClaude),
		Status: claudeStatus,
		Active: currentActive == ProviderClaude,
		Models: getProviderModels(ProviderClaude),
		Config: ProviderPublicConfig{
			BaseURL:   "",   // Claude CLI doesn't use HTTP
			HasAPIKey: true, // Claude CLI uses Anthropic API key internally
		},
		Health: *claudeHealth,
	})

	// Add HTTP providers
	providerOrder := []ProviderType{ProviderOpenAI, ProviderOpenRouter, ProviderOllama, ProviderLocal}
	for _, pt := range providerOrder {
		config := providers[pt]
		if config == nil {
			continue
		}

		health := checkProviderHealth(pt, config)
		hasAPIKey := config.APIKey != ""
		status := getProviderStatus(health, hasAPIKey, pt)

		// Ollama and Local don't require API keys
		if pt == ProviderOllama || pt == ProviderLocal {
			hasAPIKey = true // Mark as "configured" since no key needed
		}

		data = append(data, ProviderInfo{
			ID:     string(pt),
			Name:   getProviderDisplayName(pt),
			Status: status,
			Active: currentActive == pt,
			Models: getProviderModels(pt),
			Config: ProviderPublicConfig{
				BaseURL:   config.BaseURL,
				HasAPIKey: hasAPIKey,
			},
			Health: *health,
		})
	}

	response := ProviderListResponse{
		Object:        "list",
		Data:          data,
		Active:        string(currentActive),
		FallbackChain: []string{string(ProviderOpenRouter), string(ProviderOllama)},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// === ACTIVE PROVIDER STATE ===

// activeProvider tracks the currently active provider (mutable at runtime)
var activeProvider = ProviderClaude
var activeProviderMu sync.RWMutex

// getActiveProvider returns the currently active provider
func getActiveProvider() ProviderType {
	activeProviderMu.RLock()
	defer activeProviderMu.RUnlock()
	return activeProvider
}

// setActiveProvider sets the active provider
func setActiveProvider(pt ProviderType) ProviderType {
	activeProviderMu.Lock()
	defer activeProviderMu.Unlock()
	prev := activeProvider
	activeProvider = pt
	return prev
}

// handleProviderByID handles POST /v1/providers/{id}/activate and /v1/providers/{id}/test
func (s *serveServer) handleProviderByID(w http.ResponseWriter, r *http.Request) {
	// Parse path: /v1/providers/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/v1/providers/")
	parts := strings.Split(path, "/")

	if len(parts) < 2 {
		s.writeError(w, http.StatusBadRequest, "Invalid path. Use /v1/providers/{id}/activate or /v1/providers/{id}/test", "invalid_request")
		return
	}

	providerID := parts[0]
	action := parts[1]

	// Validate provider ID
	var providerType ProviderType
	switch providerID {
	case "claude":
		providerType = ProviderClaude
	case "openai":
		providerType = ProviderOpenAI
	case "openrouter":
		providerType = ProviderOpenRouter
	case "ollama":
		providerType = ProviderOllama
	case "local":
		providerType = ProviderLocal
	default:
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("Provider '%s' not found", providerID), "not_found")
		return
	}

	switch action {
	case "activate":
		s.handleProviderActivate(w, r, providerType)
	case "test":
		s.handleProviderTest(w, r, providerType)
	default:
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("Unknown action: %s", action), "invalid_request")
	}
}

// handleProviderActivate sets a provider as the active default
func (s *serveServer) handleProviderActivate(w http.ResponseWriter, r *http.Request, pt ProviderType) {
	if r.Method != "POST" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	// Set the provider as active
	previous := setActiveProvider(pt)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"active":   string(pt),
		"previous": string(previous),
	})
}

// handleProviderTest performs a health check on a specific provider
func (s *serveServer) handleProviderTest(w http.ResponseWriter, r *http.Request, pt ProviderType) {
	if r.Method != "POST" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	// Get provider config
	providers := DefaultProviders()
	config := providers[pt]

	// For Claude CLI, we just check if the binary exists
	if pt == ProviderClaude {
		start := time.Now()
		_, err := exec.LookPath(claudeCommand)
		latency := int(time.Since(start).Milliseconds())

		status := "online"
		var errMsg *string
		if err != nil {
			status = "offline"
			msg := "Claude CLI not found in PATH"
			errMsg = &msg
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"provider":   string(pt),
			"status":     status,
			"latency_ms": latency,
			"test_model": "claude",
			"error":      errMsg,
		})
		return
	}

	// For HTTP providers, make a test request
	if config == nil {
		s.writeError(w, http.StatusInternalServerError, "No configuration for provider", "server_error")
		return
	}

	// Health check by hitting the models endpoint
	client := &http.Client{Timeout: 10 * time.Second}
	url := config.BaseURL + "/models"

	start := time.Now()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}

	if config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+config.APIKey)
	}

	// OpenRouter-specific headers
	if pt == ProviderOpenRouter {
		req.Header.Set("HTTP-Referer", "https://cogos.dev")
		req.Header.Set("X-Title", "CogOS Kernel")
	}

	resp, err := client.Do(req)
	latency := int(time.Since(start).Milliseconds())

	status := "online"
	var errMsg *string

	if err != nil {
		status = "offline"
		msg := err.Error()
		errMsg = &msg
	} else {
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			status = "degraded"
			msg := "authentication failed"
			errMsg = &msg
		} else if resp.StatusCode >= 400 {
			status = "offline"
			msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
			errMsg = &msg
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"provider":   string(pt),
		"status":     status,
		"latency_ms": latency,
		"test_model": config.Model,
		"error":      errMsg,
	})
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), "invalid_request")
		return
	}

	// Enrich OTEL span with request attributes
	span := trace.SpanFromContext(r.Context())
	span.SetAttributes(
		attribute.String("model", req.Model),
		attribute.Bool("stream", req.Stream),
	)

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

	// Extract system prompt and user messages.
	// Conversation history is flattened into a single prompt string because
	// Claude CLI's -p flag is one-shot. Tool call/result history from prior
	// turns is preserved as text context so the model sees what happened.
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
			// Include prior assistant messages as context.
			// Handle both text content and tool_calls (which may have null content).
			if userPrompt.Len() > 0 {
				userPrompt.WriteString("\n\nAssistant: ")
				if content != "" {
					userPrompt.WriteString(content)
				}
				// Include tool calls so the model knows what tools were used
				if toolSummary := msg.GetToolCallsSummary(); toolSummary != "" {
					if content != "" {
						userPrompt.WriteString("\n")
					}
					userPrompt.WriteString(toolSummary)
				}
				userPrompt.WriteString("\n\nUser: ")
			}
		case "tool":
			// Include tool results so the model sees the full tool interaction.
			// Format: [Tool result for <call_id>: <content>]
			if userPrompt.Len() > 0 && content != "" {
				toolResult := content
				if len(toolResult) > 500 {
					toolResult = toolResult[:500] + "...(truncated)"
				}
				if msg.ToolCallID != "" {
					userPrompt.WriteString(fmt.Sprintf("\n[Tool result (%s): %s]", msg.ToolCallID, toolResult))
				} else {
					userPrompt.WriteString(fmt.Sprintf("\n[Tool result: %s]", toolResult))
				}
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

	// Auto-derive session from origin when no explicit session provided.
	// This enables bus event emission for gateway-routed requests (e.g. OpenClaw→Discord)
	// that don't send X-Session-ID. All requests from the same origin share one bus.
	if sessionID == "" {
		origin := r.Header.Get("X-Origin")
		if origin == "" {
			origin = "http"
		}
		sessionID = origin
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

		// Try bus-sourced context first (multi-turn history)
		if s.busChat != nil && sessionID != "" {
			busCtxID := fmt.Sprintf("bus_chat_%s", sessionID)
			contextState = s.busChat.buildContextFromBus(busCtxID, userPrompt.String())
			if contextState != nil {
				log.Printf("[TAA] Using bus history: %s (%d total tokens) instead of request messages",
					busCtxID, contextState.TotalTokens)
			}
		}

		// Fall back to request-only context
		if contextState == nil {
			contextState, err = ConstructContextStateWithProfile(req.Messages, sessionID, workspaceRoot, taaProfile)
		}

		if err != nil {
			// Log but don't fail - context is optional enhancement
			log.Printf("[TAA] Context construction warning (profile=%s): %v", taaProfile, err)
		} else if contextState != nil {
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

			// Log low coherence for observability
			if contextState.ShouldRefresh {
				log.Printf("[TAA] Low coherence (%.2f) — context may be degraded", contextState.CoherenceScore)
			}
		}
	}

	// Emit bus chat.request event (side-effect for CogField visibility)
	var busID string
	var requestSeq int
	if s.busChat != nil && sessionID != "" {
		var reqEvt *BusEventData
		origin := r.Header.Get("X-Origin")
		if origin == "" {
			origin = "http"
		}
		busID, reqEvt, _ = s.busChat.emitRequest(sessionID, userPrompt.String(), origin)
		if reqEvt != nil {
			requestSeq = reqEvt.Seq
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
		Tools:        req.Tools,
	}

	// Use UCP workspace root as Claude CLI working directory when provided.
	// This lets callers (e.g., OpenClaw) specify which workspace the backend
	// should operate in, rather than always using the kernel's workspace.
	if ucpContext != nil && ucpContext.Workspace != nil && ucpContext.Workspace.Root != "" {
		inferReq.WorkspaceRoot = ucpContext.Workspace.Root
	}

	// Parse X-Allowed-Tools header for explicit tool control
	if allowedToolsHeader := r.Header.Get("X-Allowed-Tools"); allowedToolsHeader != "" {
		var tools []string
		for _, t := range strings.Split(allowedToolsHeader, ",") {
			if trimmed := strings.TrimSpace(t); trimmed != "" {
				tools = append(tools, trimmed)
			}
		}
		inferReq.AllowedTools = tools
	}

	// Parse OpenClaw bridge headers for MCP bridge mode (headers override env vars).
	// The harness auto-generates the MCP config when it sees OpenClawURL set.
	openClawURL := r.Header.Get("X-OpenClaw-URL")
	if openClawURL == "" {
		openClawURL = os.Getenv("OPENCLAW_URL")
	}
	if openClawURL != "" {
		inferReq.OpenClawURL = openClawURL
		inferReq.OpenClawToken = r.Header.Get("X-OpenClaw-Token")
		if inferReq.OpenClawToken == "" {
			inferReq.OpenClawToken = os.Getenv("OPENCLAW_TOKEN")
		}
		inferReq.SessionID = sessionID // Use already-resolved session ID from earlier parsing
	}

	// Set UCP response headers if UCP was used
	if ucpContext != nil {
		if err := setUCPResponseHeaders(w, ucpContext); err != nil {
			log.Printf("[UCP] Failed to set response headers: %v", err)
		}
	}

	// Handle streaming vs non-streaming
	startTime := time.Now()
	if req.Stream {
		s.handleStreamingResponse(w, inferReq, sessionID, busID, requestSeq, startTime)
	} else {
		s.handleNonStreamingResponse(w, inferReq, sessionID, busID, requestSeq, startTime)
	}
}

func (s *serveServer) handleStreamingResponse(w http.ResponseWriter, inferReq *InferenceRequest, sessionID, busID string, requestSeq int, startTime time.Time) {
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
	// Deep copy to prevent data races — the original may be mutated concurrently.
	if inferReq.ContextState != nil {
		s.taaStateMutex.Lock()
		s.lastTAAState = deepCopyContextState(inferReq.ContextState)
		s.taaStateMutex.Unlock()
		s.emitTAAContext(w, flusher, inferReq.ContextState, inferReq.Model)
	}

	// Delegate to harness inference engine
	chunks, err := HarnessRunInferenceStream(inferReq, GlobalRegistry)
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
			// Claude CLI handles tool calls internally. Emit as informational
			// events (empty choices) so OpenAI-compatible clients don't try to
			// execute them. CogOS-aware clients can use the event_type field.
			if chunk.ToolCall != nil {
				toolStartChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{},
					"event_type": "tool_call",
					"tool_call": map[string]any{
						"id":   chunk.ToolCall.ID,
						"name": chunk.ToolCall.Name,
					},
				}
				data, _ := json.Marshal(toolStartChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			continue

		case "tool_use_delta":
			// Informational only — tool is being executed by Claude CLI internally.
			if chunk.ToolCall != nil {
				toolDeltaChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{},
					"event_type": "tool_call_delta",
					"tool_call": map[string]any{
						"arguments": string(chunk.ToolCall.Arguments),
					},
				}
				data, _ := json.Marshal(toolDeltaChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			continue
		}

		// Handle tool result — no OpenAI standard for streaming tool results,
		// so this remains a CogOS extension. We include a choices entry with the
		// result content so SDKs don't silently drop the event.
		if chunk.ToolResult != nil {
			resultChunk := map[string]any{
				"id":      chunk.ID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"role":    "assistant",
						"content": nil,
					},
					"finish_reason": nil,
				}},
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

			// Build usage info if available
			var usageInfo *UsageInfo
			if chunk.Usage != nil {
				usageInfo = &UsageInfo{
					PromptTokens:      chunk.Usage.InputTokens,
					CompletionTokens:  chunk.Usage.OutputTokens,
					TotalTokens:       chunk.Usage.InputTokens + chunk.Usage.OutputTokens,
					CacheReadTokens:   chunk.Usage.CacheReadTokens,
					CacheCreateTokens: chunk.Usage.CacheCreateTokens,
					CostUSD:           chunk.Usage.CostUSD,
				}
			}

			// OpenAI streaming spec: finish_reason chunk first, then
			// usage-only chunk, then [DONE]. Include usage on the
			// finish_reason chunk too — some SDKs read it there.
			finishReason := chunk.FinishReason
			if finishReason == "" {
				finishReason = "stop"
			}
			finishChunk := &StreamChunk{
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
				Usage: usageInfo,
			}
			data, _ := json.Marshal(finishChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

			// Dedicated usage-only chunk (standard OpenAI stream_options
			// include_usage format). Emitted after finish_reason, before
			// [DONE], with empty choices array.
			if usageInfo != nil {
				usageChunk := &StreamChunk{
					ID:      chunk.ID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []ChatChoice{},
					Usage:   usageInfo,
				}
				data, _ = json.Marshal(usageChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

			// Emit bus chat.response event
			if s.busChat != nil && busID != "" && accumulatedContent.Len() > 0 {
				tokensUsed := 0
				if chunk.Usage != nil {
					tokensUsed = chunk.Usage.InputTokens + chunk.Usage.OutputTokens
				}
				s.busChat.emitResponse(busID, requestSeq, accumulatedContent.String(), model, time.Since(startTime).Milliseconds(), tokensUsed)
			}
		}
	}
}

func (s *serveServer) handleNonStreamingResponse(w http.ResponseWriter, inferReq *InferenceRequest, sessionID, busID string, requestSeq int, startTime time.Time) {
	// Store TAA context for /v1/taa endpoint
	// Deep copy to prevent data races — the original may be mutated concurrently.
	if inferReq.ContextState != nil {
		s.taaStateMutex.Lock()
		s.lastTAAState = deepCopyContextState(inferReq.ContextState)
		s.taaStateMutex.Unlock()
	}

	// Delegate to harness inference engine
	resp, err := HarnessRunInference(inferReq, GlobalRegistry)
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
		PromptTokens:      resp.PromptTokens,
		CompletionTokens:  resp.CompletionTokens,
		TotalTokens:       resp.PromptTokens + resp.CompletionTokens,
		CacheReadTokens:   resp.CacheReadTokens,
		CacheCreateTokens: resp.CacheCreateTokens,
		CostUSD:           resp.CostUSD,
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

	// Emit bus chat.response event
	if s.busChat != nil && busID != "" && resp.Content != "" {
		tokensUsed := resp.PromptTokens + resp.CompletionTokens
		s.busChat.emitResponse(busID, requestSeq, resp.Content, model, time.Since(startTime).Milliseconds(), tokensUsed)
	}
}

// handleRequests handles GET /v1/requests - list in-flight requests
func (s *serveServer) handleRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	// Get query parameter for filtering
	statusFilter := r.URL.Query().Get("status")

	var entries []RequestEntry
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
	// Use per-request workspace kernel, fall back to default
	kernel := s.kernel
	if ws := workspaceFromRequest(r); ws != nil {
		kernel = ws.kernel
	}
	if kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	uri := r.URL.Query().Get("uri")
	if uri == "" {
		s.writeError(w, http.StatusBadRequest, "missing 'uri' query parameter", "invalid_request")
		return
	}

	resource, err := kernel.ResolveContext(r.Context(), uri)
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
	// Use per-request workspace kernel, fall back to default
	kernel := s.kernel
	if ws := workspaceFromRequest(r); ws != nil {
		kernel = ws.kernel
	}
	if kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit
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

	if err := kernel.MutateContext(r.Context(), req.URI, mutation); err != nil {
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
	// Use per-request workspace kernel, fall back to default
	kernel := s.kernel
	if ws := workspaceFromRequest(r); ws != nil {
		kernel = ws.kernel
	}
	if kernel == nil {
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
		OriginPatterns: []string{"localhost:*", "127.0.0.1:*"},
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to upgrade: "+err.Error(), "server_error")
		return
	}
	defer c.CloseNow()

	// Create watcher
	ctx := r.Context()
	watcher, err := kernel.WatchURI(ctx, uri)
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
	// Use per-request workspace kernel, fall back to default
	kernel := s.kernel
	if ws := workspaceFromRequest(r); ws != nil {
		kernel = ws.kernel
	}
	if kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	// Resolve cog://status for full workspace state
	resource, err := kernel.Resolve("cog://status")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "resolve_error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resource)
}

// handleSignals returns signal field via SDK
func (s *serveServer) handleSignals(w http.ResponseWriter, r *http.Request) {
	// Use per-request workspace kernel, fall back to default
	kernel := s.kernel
	if ws := workspaceFromRequest(r); ws != nil {
		kernel = ws.kernel
	}
	if kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	// Resolve cog://signals for signal field
	resource, err := kernel.Resolve("cog://signals")
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

	// Initialize bus chat event emission if we have a workspace
	if root != "" {
		server.busChat = newBusChat(root)
		// Wire SSE broker to bus event emission
		server.busChat.manager.onEvent = func(busID string, evt *BusEventData) {
			server.busBroker.publish(busID, evt)
		}
		log.Printf("[bus-chat] initialized (taa_profile=%s, context_from_bus=%v)",
			server.busChat.config.TAAProfile, server.busChat.config.Features.ContextFromBus)
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

	// Register the primary workspace too (from ResolveWorkspace) if not already in registry
	if root != "" {
		if _, exists := server.workspaces[root]; !exists {
			// Use root path as key if not in named registry
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

	// Open log file
	logOut, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
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

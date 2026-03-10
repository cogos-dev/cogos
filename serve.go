// Serve Module - OpenAI-compatible inference endpoint wrapping Claude CLI
//
// This module provides an HTTP server with OpenAI-compatible endpoints:
// - POST /v1/chat/completions - Chat completions (streaming & non-streaming)
// - GET /v1/models - List available models
// - GET /v1/requests - List in-flight requests
// - DELETE /v1/requests/:id - Cancel a request
// - GET /v1/taa - TAA context visibility (debugging)
// - GET /v1/sessions - List sessions with context metadata
// - GET /v1/sessions/{session_id}/context - Per-session context state
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
	"github.com/fsnotify/fsnotify"
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
	Role       string          `json:"role,omitempty"`
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
	busBroker     *busEventBroker   // SSE subscriber broker for bus events
	consumerReg   *consumerRegistry // Server-side consumer cursor tracking (ADR-061)
	toolBridge    *ToolBridge       // Synchronous tool bridge for client-driven agent loops
	mcpManager    *MCPSessionManager // MCP Streamable HTTP session manager
	researchMgr   *researchManager   // Research orchestration (nil if no workspace)

	// OCI auto-reload: kernel watches .cog/oci/index.json for new digests
	ociStore  *OCIStore  // nil if no OCI layout exists
	ociDigest string     // manifest digest at startup (for comparison)
	reexecCh  chan string // signals graceful re-exec with new digest
}

func newServeServer(port int, kernel *sdk.Kernel) *serveServer {
	return &serveServer{port: port, kernel: kernel, busBroker: newBusEventBroker(), toolBridge: NewToolBridge()}
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
	mux.HandleFunc("/v1/taa", s.handleTAA)                    // TAA context visibility endpoint
	mux.HandleFunc("POST /v1/context/foveated", s.handleFoveatedContext) // Iris-driven foveated rendering
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)            // Per-session context list
	mux.HandleFunc("/v1/sessions/", s.handleSessionContext)             // Per-session context detail
	mux.HandleFunc("GET /v1/card", s.handleCard)              // ADR-048: Kernel capability card
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

// === PER-SESSION CONTEXT OBSERVABILITY ===

// SessionContextState captures the context state for a single session's most recent foveated request.
// This enables per-session observability via GET /v1/sessions/{session_id}/context.
type SessionContextState struct {
	SessionID      string         `json:"session_id"`
	Profile        string         `json:"profile"`
	TurnNumber     int            `json:"turn_number"`
	IrisSize       int            `json:"iris_size"`
	IrisUsed       int            `json:"iris_used"`
	IrisPressure   float64        `json:"iris_pressure"`
	TotalTokens    int            `json:"total_tokens"`
	Blocks         []ContextBlock `json:"blocks"`
	BlockCount     int            `json:"block_count"`
	CacheHits      int            `json:"cache_hits"`
	LastRequestAt  time.Time      `json:"last_request_at"`
	CoherenceScore float64        `json:"coherence_score,omitempty"`
}

var (
	sessionContextStore   = make(map[string]*SessionContextState)
	sessionContextStoreMu sync.RWMutex
)

// recordSessionContext stores the latest context state for a session.
func recordSessionContext(state *SessionContextState) {
	sessionContextStoreMu.Lock()
	defer sessionContextStoreMu.Unlock()
	sessionContextStore[state.SessionID] = state
}

// handleListSessions returns summary metadata for all known sessions.
// GET /v1/sessions
func (s *serveServer) handleListSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	sessionContextStoreMu.RLock()
	defer sessionContextStoreMu.RUnlock()

	type sessionSummary struct {
		SessionID      string    `json:"session_id"`
		Profile        string    `json:"profile"`
		TurnNumber     int       `json:"turn_number"`
		IrisPressure   float64   `json:"iris_pressure"`
		TotalTokens    int       `json:"total_tokens"`
		BlockCount     int       `json:"block_count"`
		CoherenceScore float64   `json:"coherence_score,omitempty"`
		LastRequestAt  time.Time `json:"last_request_at"`
	}

	sessions := make([]sessionSummary, 0, len(sessionContextStore))
	for _, state := range sessionContextStore {
		sessions = append(sessions, sessionSummary{
			SessionID:      state.SessionID,
			Profile:        state.Profile,
			TurnNumber:     state.TurnNumber,
			IrisPressure:   state.IrisPressure,
			TotalTokens:    state.TotalTokens,
			BlockCount:     state.BlockCount,
			CoherenceScore: state.CoherenceScore,
			LastRequestAt:  state.LastRequestAt,
		})
	}

	json.NewEncoder(w).Encode(map[string]any{
		"sessions": sessions,
		"count":    len(sessions),
	})
}

// handleSessionContext returns the full context state for a specific session.
// GET /v1/sessions/{session_id}/context
func (s *serveServer) handleSessionContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Extract session_id from URL path: /v1/sessions/{session_id}/context
	path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	// path is now "{session_id}/context" or "{session_id}"
	sessionID := strings.TrimSuffix(path, "/context")
	sessionID = strings.TrimSuffix(sessionID, "/")

	if sessionID == "" {
		s.writeError(w, http.StatusBadRequest, "session_id is required in path: /v1/sessions/{session_id}/context", "invalid_request")
		return
	}

	sessionContextStoreMu.RLock()
	state, ok := sessionContextStore[sessionID]
	sessionContextStoreMu.RUnlock()

	if !ok {
		s.writeError(w, http.StatusNotFound,
			fmt.Sprintf("No context found for session %q. Use GET /v1/sessions to list known sessions.", sessionID),
			"not_found")
		return
	}

	json.NewEncoder(w).Encode(state)
}

// handleFoveatedContext renders context at variable resolution driven by iris signals.
//
// The iris (agent's context window state) determines the effective budget,
// and score-based thresholds determine which content gets full vs. reduced resolution.
//
// POST /v1/context/foveated
// Body: { prompt, iris: { size, used }, profile, session_id, user_id }
// Response: { context, tokens, anchor, goal, coherence_score, tier_breakdown, effective_budget, iris_pressure }
func (s *serveServer) handleFoveatedContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Parse request
	r.Body = http.MaxBytesReader(w, r.Body, 1<<18) // 256KB limit
	var req struct {
		Prompt    string `json:"prompt"`
		Iris      struct {
			Size int `json:"size"`
			Used int `json:"used"`
		} `json:"iris"`
		Profile   string `json:"profile"`
		SessionID string `json:"session_id"`
		UserID    string `json:"user_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), "invalid_request")
		return
	}

	if req.Prompt == "" {
		s.writeError(w, http.StatusBadRequest, "prompt is required", "invalid_request")
		return
	}

	// Defaults
	profileName := req.Profile
	if profileName == "" {
		profileName = "default"
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = "foveated-" + nowISO()
	}

	workspaceRoot := ""
	if s.kernel != nil {
		workspaceRoot = s.kernel.Root()
	} else if ws := s.getWorkspace(""); ws != nil {
		workspaceRoot = ws.root
	}

	if workspaceRoot == "" {
		s.writeError(w, http.StatusInternalServerError, "No workspace root available", "server_error")
		return
	}

	// Build messages from prompt (minimal — the plugin sends just the prompt)
	promptJSON, _ := json.Marshal(req.Prompt)
	messages := []ChatMessage{
		{Role: "user", Content: json.RawMessage(promptJSON)},
	}

	// Compute iris pressure
	irisPressure := float64(0)
	if req.Iris.Size > 0 {
		irisPressure = float64(req.Iris.Used) / float64(req.Iris.Size)
	}

	log.Printf("[foveated] Request: iris_size=%d iris_used=%d pressure=%.1f%% profile=%s session=%s",
		req.Iris.Size, req.Iris.Used, irisPressure*100, profileName, sessionID)

	// Construct context with iris-driven budgets
	var ctx *ContextState
	var err error

	if req.Iris.Size > 0 {
		ctx, err = ConstructContextStateWithIris(messages, sessionID, workspaceRoot, profileName, req.Iris.Size, req.Iris.Used)
	} else {
		ctx, err = ConstructContextStateWithProfile(messages, sessionID, workspaceRoot, profileName)
	}

	if err != nil {
		log.Printf("[foveated] Construction error (partial result returned): %v", err)
		// Continue — partial results are still useful
	}

	if ctx == nil {
		s.writeError(w, http.StatusInternalServerError, "Context construction returned nil", "server_error")
		return
	}

	// Store as last TAA state for /v1/taa visibility
	s.taaStateMutex.Lock()
	s.lastTAAState = ctx
	s.taaStateMutex.Unlock()

	// Build response — stability-ordered blocks
	contextStr, blocks := ctx.BuildOrderedContextString()
	if contextStr == "" {
		// Fallback to legacy tier-ordered output if decomposition yields nothing
		contextStr = ctx.BuildContextString()
	}

	tierBreakdown := map[string]int{}
	if ctx.Tier1Identity != nil {
		tierBreakdown["tier1"] = ctx.Tier1Identity.Tokens
	}
	if ctx.Tier2Temporal != nil {
		tierBreakdown["tier2"] = ctx.Tier2Temporal.Tokens
	}
	if ctx.Tier3Present != nil {
		tierBreakdown["tier3"] = ctx.Tier3Present.Tokens
	}
	if ctx.Tier4Semantic != nil {
		tierBreakdown["tier4"] = ctx.Tier4Semantic.Tokens
	}

	effectiveBudget := ctx.TotalTokens
	if req.Iris.Size > 0 {
		available := req.Iris.Size - req.Iris.Used
		if available > 0 {
			effectiveBudget = available
		}
	}

	// Record per-session context state for observability
	turnNumber := 0
	cacheHits := 0
	sessionContextStoreMu.RLock()
	if prev, ok := sessionContextStore[sessionID]; ok {
		turnNumber = prev.TurnNumber + 1
		// Count cache hits: blocks whose hash matches a block in the previous state
		prevHashes := make(map[string]bool, len(prev.Blocks))
		for _, b := range prev.Blocks {
			prevHashes[b.Hash] = true
		}
		for _, b := range blocks {
			if prevHashes[b.Hash] {
				cacheHits++
			}
		}
	}
	sessionContextStoreMu.RUnlock()

	recordSessionContext(&SessionContextState{
		SessionID:      sessionID,
		Profile:        profileName,
		TurnNumber:     turnNumber,
		IrisSize:       req.Iris.Size,
		IrisUsed:       req.Iris.Used,
		IrisPressure:   irisPressure,
		TotalTokens:    ctx.TotalTokens,
		Blocks:         blocks,
		BlockCount:     len(blocks),
		CacheHits:      cacheHits,
		LastRequestAt:  time.Now(),
		CoherenceScore: ctx.CoherenceScore,
	})

	log.Printf("[foveated] Response: tokens=%d blocks=%d anchor=%q goal=%q coherence=%.2f pressure=%.1f%%",
		ctx.TotalTokens, len(blocks), ctx.Anchor, ctx.Goal, ctx.CoherenceScore, irisPressure*100)

	json.NewEncoder(w).Encode(map[string]any{
		"context":          contextStr,
		"tokens":           ctx.TotalTokens,
		"anchor":           ctx.Anchor,
		"goal":             ctx.Goal,
		"coherence_score":  ctx.CoherenceScore,
		"tier_breakdown":   tierBreakdown,
		"effective_budget": effectiveBudget,
		"iris_pressure":    irisPressure,
		"blocks":           blocks,
	})
}

func (s *serveServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Check if claude CLI is available
	_, err := exec.LookPath(claudeCommand)
	status := "healthy"
	if err != nil {
		status = "degraded"
	}

	resp := map[string]any{
		"status":    status,
		"timestamp": nowISO(),
		"claude":    err == nil,
		"debug":     DebugMode.Load(),
	}
	if s.mcpManager != nil {
		resp["mcp"] = map[string]any{
			"sessions": s.mcpManager.SessionCount(),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
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
		if ucpContext.User != nil {
			packets = append(packets, "user")
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
	var requestHash string
	if s.busChat != nil && sessionID != "" {
		origin := r.Header.Get("X-Origin")
		if origin == "" {
			origin = "http"
		}

		reqOpts := ChatRequestOpts{
			SessionID: sessionID,
			Content:   userPrompt.String(),
			Origin:    origin,
			Model:     req.Model,
			Stream:    req.Stream,
		}
		// Identity from UCP or fallback headers
		if ucpContext != nil && ucpContext.User != nil {
			reqOpts.UserID = ucpContext.User.ID
			reqOpts.UserName = ucpContext.User.DisplayName
		} else {
			reqOpts.UserID = r.Header.Get("X-OpenClaw-User-ID")
			reqOpts.UserName = r.Header.Get("X-OpenClaw-User-Name")
		}
		if ucpContext != nil && ucpContext.Identity != nil {
			reqOpts.AgentName = ucpContext.Identity.Name
		}
		// OTEL trace context from headers
		reqOpts.TraceID = r.Header.Get("X-Trace-ID")
		reqOpts.SpanID = r.Header.Get("X-Span-ID")
		// TAA context
		reqOpts.HasTAA = taaEnabled
		reqOpts.TAAProfile = taaProfile

		var reqEvt *CogBlock
		busID, reqEvt, _ = s.busChat.emitRequest(reqOpts)
		if reqEvt != nil {
			requestSeq = reqEvt.Seq
			requestHash = reqEvt.Hash
		}
	}

	// Build InferenceRequest using shared engine
	var schema json.RawMessage
	if req.ResponseFormat != nil && len(req.ResponseFormat.JSONSchema) > 0 {
		schema = req.ResponseFormat.JSONSchema
	}

	// When tools are present, use a detached context instead of the HTTP
	// request context. This prevents the CLI from being killed when Request 1's
	// handler returns (tool bridge parks the channel across HTTP boundaries).
	var inferCtx context.Context
	if len(req.Tools) > 0 {
		inferCtx = context.Background()
	} else {
		inferCtx = r.Context()
	}

	inferReq := &InferenceRequest{
		Prompt:       userPrompt.String(),
		SystemPrompt: systemPrompt,
		Model:        req.Model,
		Schema:       schema,
		MaxTokens:    req.MaxTokens,
		Origin:       "http",
		Stream:       req.Stream,
		Context:      inferCtx,
		ContextState: contextState,
		Tools:        req.Tools,
	}

	// Always set session ID for the harness (needed by MCP bridge's SESSION_ID env var)
	inferReq.SessionID = sessionID

	// Use UCP workspace root as Claude CLI working directory when provided.
	// This lets callers (e.g., OpenClaw) specify which workspace the backend
	// should operate in, rather than always using the kernel's workspace.
	if ucpContext != nil && ucpContext.Workspace != nil && ucpContext.Workspace.Root != "" {
		inferReq.WorkspaceRoot = ucpContext.Workspace.Root
	}

	// Simple workspace override: X-Workspace-Root header sets the working
	// directory without requiring a full UCP workspace packet. This is a
	// lightweight alternative for callers that only need to specify the cwd.
	if inferReq.WorkspaceRoot == "" {
		if simpleRoot := r.Header.Get("X-Workspace-Root"); simpleRoot != "" {
			inferReq.WorkspaceRoot = simpleRoot
		}
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

	// Agent-aware tool policy enforcement from CRD.
	// If no explicit X-Allowed-Tools header was provided, look up the agent's
	// CRD and apply its modelConfig.allowedTools. The UCP Identity packet
	// carries the agent name (e.g., "Sentinel", "Whirl").
	if ucpContext != nil && ucpContext.Identity != nil && ucpContext.Identity.Name != "" {
		agentName := strings.ToLower(ucpContext.Identity.Name)
		policy, err := GetAgentCRDToolPolicy(workspaceRoot, agentName)
		if err != nil {
			log.Printf("[CRD] Warning: failed to load agent CRD for %q: %v", agentName, err)
		} else if policy != nil {
			// Headless agent gate: headless agents do NOT go through inference.
			// They should only receive tool dispatch via the bus ToolRouter.
			if policy.AgentType == "headless" {
				log.Printf("[CRD] Agent %q is headless — rejecting inference request", agentName)
				http.Error(w, `{"error":"headless agents do not support inference — use bus tool.invoke"}`, http.StatusBadRequest)
				return
			}

			// CRD-defined tools — only apply if no explicit header override
			if len(inferReq.AllowedTools) == 0 && len(policy.AllowedTools) > 0 {
				inferReq.AllowedTools = policy.AllowedTools
				log.Printf("[CRD] Applied tool policy for agent %q: %v", agentName, policy.AllowedTools)
			}
		}
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

	// Extract user identity from UCP or OpenClaw fallback headers.
	// Priority: X-UCP-User header > X-OpenClaw-User-ID / X-OpenClaw-User-Name fallback
	if ucpContext != nil && ucpContext.User != nil {
		inferReq.UserID = ucpContext.User.ID
		inferReq.UserName = ucpContext.User.DisplayName
		log.Printf("[UCP] User identity: id=%s name=%s source=%s",
			ucpContext.User.ID, ucpContext.User.DisplayName, ucpContext.User.Source)
	} else {
		// Fallback: OpenClaw sends user identity via simpler headers
		if userID := r.Header.Get("X-OpenClaw-User-ID"); userID != "" {
			inferReq.UserID = userID
		}
		if userName := r.Header.Get("X-OpenClaw-User-Name"); userName != "" {
			inferReq.UserName = userName
		}
		if inferReq.UserID != "" {
			log.Printf("[UCP] User identity (fallback headers): id=%s name=%s",
				inferReq.UserID, inferReq.UserName)
		}
	}

	// Wire user memory scope if we have a user identity and an agent CRD
	if inferReq.UserID != "" && ucpContext != nil && ucpContext.Identity != nil {
		agentName := strings.ToLower(ucpContext.Identity.Name)
		if crd, err := LoadAgentCRD(workspaceRoot, agentName); err == nil {
			if scope := BuildUserScope(crd, inferReq.UserID); scope != nil {
				log.Printf("[memory-scope] user=%s agent=%s level=%s scope=%s",
					inferReq.UserID, agentName, scope.Level, scope.UserScope)
			}
		}
	}

	// Set UCP response headers if UCP was used
	if ucpContext != nil {
		if err := setUCPResponseHeaders(w, ucpContext); err != nil {
			log.Printf("[UCP] Failed to set response headers: %v", err)
		}
	}

	// Collect bus enrichment context for response/error handlers
	bctx := busEventCtx{
		BusID:       busID,
		RequestSeq:  requestSeq,
		RequestHash: requestHash,
		TraceID:     r.Header.Get("X-Trace-ID"),
		SpanID:      r.Header.Get("X-Span-ID"),
	}

	// Check for tool bridge continuation: if the request contains role:"tool"
	// messages and there's an active bridge session, deliver results and resume
	// streaming from the parked output channel instead of starting a new CLI.
	startTime := time.Now()
	if s.toolBridge != nil && s.hasToolMessages(req.Messages) {
		if sess := s.toolBridge.GetSession(sessionID); sess != nil {
			log.Printf("[tool-bridge] Continuation detected for session %s", sessionID)
			s.handleToolBridgeContinuation(w, &req, sess, sessionID, bctx, startTime)
			return
		}
	}

	// Handle streaming vs non-streaming
	if req.Stream {
		s.handleStreamingResponse(w, inferReq, sessionID, bctx, startTime)
	} else {
		s.handleNonStreamingResponse(w, inferReq, sessionID, bctx, startTime)
	}
}

// busEventCtx carries enrichment fields from the HTTP handler into streaming/non-streaming
// response handlers for bus event emission. Avoids threading the full *http.Request.
type busEventCtx struct {
	BusID       string
	RequestSeq  int
	RequestHash string
	TraceID     string
	SpanID      string
}

func (s *serveServer) handleStreamingResponse(w http.ResponseWriter, inferReq *InferenceRequest, sessionID string, bctx busEventCtx, startTime time.Time) {
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

	// Rolling write deadline — prevents the server's global WriteTimeout
	// (5 min) from killing long-running inference streams.  Each call pushes
	// the deadline forward by sseWriteWindow so the timeout becomes per-idle
	// rather than absolute.
	rc := http.NewResponseController(w)
	extendDeadline := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteWindow))
	}

	// Store TAA context for /v1/taa endpoint and emit as SSE event
	// Deep copy to prevent data races — the original may be mutated concurrently.
	if inferReq.ContextState != nil {
		s.taaStateMutex.Lock()
		s.lastTAAState = deepCopyContextState(inferReq.ContextState)
		s.taaStateMutex.Unlock()
		extendDeadline()
		s.emitTAAContext(w, flusher, inferReq.ContextState, inferReq.Model)
	}

	// Pre-create tool bridge session if external tools are present.
	// MCP bridges may POST to /v1/tool-bridge/pending before message_stop,
	// since Claude CLI invokes MCP tools eagerly during streaming.
	hasExternalTools := len(inferReq.Tools) > 0
	if hasExternalTools && s.toolBridge != nil {
		s.toolBridge.EnsureSession(sessionID)
	}

	// Delegate to harness inference engine
	chunks, err := HarnessRunInferenceStream(inferReq, GlobalRegistry)
	if err != nil {
		s.writeSSEError(w, flusher, "Failed to start inference: "+err.Error())
		if s.busChat != nil && bctx.BusID != "" {
			s.busChat.emitError(ChatErrorOpts{
				BusID:        bctx.BusID,
				RequestSeq:   bctx.RequestSeq,
				RequestHash:  bctx.RequestHash,
				ErrorMessage: err.Error(),
				ErrorType:    "inference_start",
				DurationMs:   time.Since(startTime).Milliseconds(),
				Model:        inferReq.Model,
				Stream:       true,
				TraceID:      bctx.TraceID,
				SpanID:       bctx.SpanID,
			})
		}
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
		extendDeadline()

		if chunk.Error != nil {
			s.writeSSEError(w, flusher, "Inference error: "+chunk.Error.Error())
			if s.busChat != nil && bctx.BusID != "" {
				s.busChat.emitError(ChatErrorOpts{
					BusID:        bctx.BusID,
					RequestSeq:   bctx.RequestSeq,
					RequestHash:  bctx.RequestHash,
					ErrorMessage: chunk.Error.Error(),
					ErrorType:    "inference_stream",
					DurationMs:   time.Since(startTime).Milliseconds(),
					Model:        inferReq.Model,
					Stream:       true,
					TraceID:      bctx.TraceID,
					SpanID:       bctx.SpanID,
				})
			}
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
				// Eagerly register external tool calls with the tool bridge.
				// MCP bridges may arrive before message_stop, so calls must
				// be registered as soon as content_block_stop events arrive.
				if hasExternalTools && s.toolBridge != nil && chunk.EventType == "tool_use" {
					const mcpPrefix = "mcp__cogos-bridge__"
					if strings.HasPrefix(chunk.ToolCall.Name, mcpPrefix) {
						origName := strings.TrimPrefix(chunk.ToolCall.Name, mcpPrefix)
						s.toolBridge.RegisterCall(sessionID, &ToolBridgeCall{
							ToolCallID: chunk.ToolCall.ID,
							Name:       origName,
							Arguments:  string(chunk.ToolCall.Arguments),
							ResultCh:   make(chan ToolBridgeResult, 1),
						})
					}
				}

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

		if chunk.Done && chunk.Suspended {
			// CLI is suspended waiting for external tool results.
			// Emit tool_calls to the client, park the output channel, and return.
			log.Printf("[tool-bridge] Suspended stream: %d external tool calls, parking channel for session %s",
				len(chunk.ExternalToolCalls), sessionID)

			// Emit external tool_calls as OpenAI streaming deltas
			for i, tc := range chunk.ExternalToolCalls {
				toolCallStart := map[string]any{
					"id":      chunk.ID,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []map[string]any{{
						"index": 0,
						"delta": map[string]any{
							"tool_calls": []map[string]any{{
								"index": i,
								"id":    tc.ID,
								"type":  "function",
								"function": map[string]any{
									"name":      tc.Name,
									"arguments": string(tc.Arguments),
								},
							}},
						},
					}},
				}
				data, _ := json.Marshal(toolCallStart)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			// Emit finish chunk with tool_calls reason
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
			finishChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{{
					Index:        0,
					Delta:        &ChatMessage{},
					FinishReason: "tool_calls",
				}},
				Usage: usageInfo,
			}
			data, _ := json.Marshal(finishChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

			// Park the output channel in the tool bridge for resumption.
			// The harness goroutine is still running (CLI blocked on MCP bridge).
			// When the client sends a follow-up request with tool results,
			// handleToolBridgeContinuation will resume reading from this channel.
			// Calls were already eagerly registered via tool_use events above.
			s.toolBridge.RegisterSession(sessionID, chunks, inferReq, nil)
			return
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

			// Emit external tool_calls as proper OpenAI streaming deltas.
			// Per the spec, tool_calls are emitted as delta chunks BEFORE
			// the finish_reason chunk.
			if len(chunk.ExternalToolCalls) > 0 {
				for i, tc := range chunk.ExternalToolCalls {
					// First chunk: tool call header (id, type, function name)
					toolCallStart := map[string]any{
						"id":      chunk.ID,
						"object":  "chat.completion.chunk",
						"created": created,
						"model":   model,
						"choices": []map[string]any{{
							"index": 0,
							"delta": map[string]any{
								"tool_calls": []map[string]any{{
									"index": i,
									"id":    tc.ID,
									"type":  "function",
									"function": map[string]any{
										"name":      tc.Name,
										"arguments": string(tc.Arguments),
									},
								}},
							},
						}},
					}
					data, _ := json.Marshal(toolCallStart)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
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
			if s.busChat != nil && bctx.BusID != "" && accumulatedContent.Len() > 0 {
				respOpts := ChatResponseOpts{
					BusID:        bctx.BusID,
					RequestSeq:   bctx.RequestSeq,
					Content:      accumulatedContent.String(),
					Model:        model,
					DurationMs:   time.Since(startTime).Milliseconds(),
					Stream:       true,
					RequestHash:  bctx.RequestHash,
					FinishReason: finishReason,
					TraceID:      bctx.TraceID,
					SpanID:       bctx.SpanID,
				}
				if chunk.Usage != nil {
					respOpts.PromptTokens = chunk.Usage.InputTokens
					respOpts.CompletionTokens = chunk.Usage.OutputTokens
					respOpts.TokensUsed = chunk.Usage.InputTokens + chunk.Usage.OutputTokens
					respOpts.CacheReadTokens = chunk.Usage.CacheReadTokens
					respOpts.CacheCreateTokens = chunk.Usage.CacheCreateTokens
					respOpts.CostUSD = chunk.Usage.CostUSD
				}
				// TAA context metrics
				if inferReq.ContextState != nil {
					respOpts.TAATokens = inferReq.ContextState.TotalTokens
					respOpts.TAACoherence = inferReq.ContextState.CoherenceScore
				}
				s.busChat.emitResponse(respOpts)
			}

			// Clean up pre-created session if no suspension happened
			if hasExternalTools && s.toolBridge != nil {
				s.toolBridge.CleanupSession(sessionID)
			}
		}
	}
}

func (s *serveServer) handleNonStreamingResponse(w http.ResponseWriter, inferReq *InferenceRequest, sessionID string, bctx busEventCtx, startTime time.Time) {
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
		if s.busChat != nil && bctx.BusID != "" {
			s.busChat.emitError(ChatErrorOpts{
				BusID:        bctx.BusID,
				RequestSeq:   bctx.RequestSeq,
				RequestHash:  bctx.RequestHash,
				ErrorMessage: err.Error(),
				ErrorType:    "inference_error",
				DurationMs:   time.Since(startTime).Milliseconds(),
				Model:        inferReq.Model,
				TraceID:      bctx.TraceID,
				SpanID:       bctx.SpanID,
			})
		}
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

	// Build the assistant message
	assistantMsg := &ChatMessage{
		Role:    "assistant",
		Content: StringToRawContent(resp.Content),
	}

	// Include tool_calls in the response if the model called external tools
	if len(resp.ToolCalls) > 0 {
		var toolCalls []map[string]any
		for _, tc := range resp.ToolCalls {
			toolCalls = append(toolCalls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(tc.Arguments),
				},
			})
		}
		tcJSON, _ := json.Marshal(toolCalls)
		assistantMsg.ToolCalls = json.RawMessage(tcJSON)
	}

	// Build response
	response := ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatChoice{
			{
				Index:        0,
				Message:      assistantMsg,
				FinishReason: finishReason,
			},
		},
		Usage: &usage,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)

	// Emit bus chat.response event
	if s.busChat != nil && bctx.BusID != "" && resp.Content != "" {
		respOpts := ChatResponseOpts{
			BusID:             bctx.BusID,
			RequestSeq:        bctx.RequestSeq,
			Content:           resp.Content,
			Model:             model,
			DurationMs:        time.Since(startTime).Milliseconds(),
			PromptTokens:      resp.PromptTokens,
			CompletionTokens:  resp.CompletionTokens,
			TokensUsed:        resp.PromptTokens + resp.CompletionTokens,
			CacheReadTokens:   resp.CacheReadTokens,
			CacheCreateTokens: resp.CacheCreateTokens,
			CostUSD:           resp.CostUSD,
			FinishReason:      finishReason,
			RequestHash:       bctx.RequestHash,
			TraceID:           bctx.TraceID,
			SpanID:            bctx.SpanID,
		}
		// TAA context metrics
		if inferReq.ContextState != nil {
			respOpts.TAATokens = inferReq.ContextState.TotalTokens
			respOpts.TAACoherence = inferReq.ContextState.CoherenceScore
		}
		s.busChat.emitResponse(respOpts)
	}
}

// === TOOL BRIDGE HANDLERS ===

// hasToolMessages checks if any messages in the request have role:"tool".
func (s *serveServer) hasToolMessages(messages []ChatMessage) bool {
	for _, msg := range messages {
		if msg.Role == "tool" {
			return true
		}
	}
	return false
}

// handleToolBridgePending handles POST /v1/tool-bridge/pending.
// Called by the MCP bridge subprocess when a passthrough tool is invoked.
// Blocks until the client delivers the real tool result via a follow-up request.
func (s *serveServer) handleToolBridgePending(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		ToolName  string `json:"tool_name"`
		Arguments string `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	log.Printf("[tool-bridge] MCP bridge arrived: session=%s tool=%s", req.SessionID, req.ToolName)

	// Try immediate match, or register as waiter and block until call arrives.
	// Claude CLI invokes MCP tools eagerly (before content_block_stop), so the
	// MCP bridge often arrives before the harness registers the call.
	call, waiterCh := s.toolBridge.WaitForPending(req.SessionID, req.ToolName)
	if call == nil && waiterCh == nil {
		log.Printf("[tool-bridge] No session for MCP bridge: session=%s tool=%s", req.SessionID, req.ToolName)
		http.Error(w, "No session", http.StatusNotFound)
		return
	}

	if call == nil {
		// Block until RegisterCall wakes us or timeout
		select {
		case c, ok := <-waiterCh:
			if !ok || c == nil {
				log.Printf("[tool-bridge] Waiter cancelled: session=%s tool=%s", req.SessionID, req.ToolName)
				http.Error(w, "Session cancelled", http.StatusGone)
				return
			}
			call = c
		case <-time.After(2 * time.Minute):
			log.Printf("[tool-bridge] Waiter timeout: session=%s tool=%s", req.SessionID, req.ToolName)
			http.Error(w, "Timeout waiting for call registration", http.StatusGatewayTimeout)
			return
		case <-r.Context().Done():
			log.Printf("[tool-bridge] MCP bridge disconnected: session=%s tool=%s", req.SessionID, req.ToolName)
			return
		}
	}

	log.Printf("[tool-bridge] Blocking on result for: session=%s tool=%s id=%s", req.SessionID, req.ToolName, call.ToolCallID)

	// Block until the client delivers the result (or timeout)
	select {
	case result := <-call.ResultCh:
		log.Printf("[tool-bridge] Result delivered: session=%s tool=%s id=%s (len=%d)",
			req.SessionID, req.ToolName, call.ToolCallID, len(result.Content))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	case <-time.After(10 * time.Minute):
		log.Printf("[tool-bridge] Timeout waiting for result: session=%s tool=%s", req.SessionID, req.ToolName)
		http.Error(w, "Timeout waiting for tool result", http.StatusGatewayTimeout)
	case <-r.Context().Done():
		log.Printf("[tool-bridge] Client disconnected: session=%s tool=%s", req.SessionID, req.ToolName)
	}
}

// handleToolBridgeContinuation handles a follow-up request with role:"tool" results.
// It delivers the results to the blocked MCP bridge and resumes streaming from
// the parked output channel.
func (s *serveServer) handleToolBridgeContinuation(w http.ResponseWriter, req *ChatCompletionRequest, sess *ToolBridgeSession, sessionID string, bctx busEventCtx, startTime time.Time) {
	// Set SSE headers for streaming response
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "Streaming not supported", "server_error")
		return
	}

	rc := http.NewResponseController(w)
	extendDeadline := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteWindow))
	}

	// Deliver tool results to the pending calls.
	// Only deliver results for tool_call_ids that exist in this session's CallsByID.
	// BrowserOS sends the full conversation history, so most role:"tool" messages
	// are from previous turns and should be silently skipped.
	delivered := 0
	for _, msg := range req.Messages {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			if sess.CallsByID[msg.ToolCallID] != nil {
				content := msg.GetContent()
				s.toolBridge.DeliverResult(sessionID, msg.ToolCallID, ToolBridgeResult{
					Content: content,
				})
				delivered++
			}
		}
	}
	log.Printf("[tool-bridge] Delivered %d tool results for session %s", delivered, sessionID)

	// Resume reading from the parked output channel.
	// The harness goroutine is still running and will produce new chunks
	// after the MCP bridge delivers the results to Claude CLI.
	model := req.Model
	if model == "" {
		model = "claude"
	}
	created := time.Now().Unix()
	var accumulatedContent strings.Builder

	for chunk := range sess.OutputCh {
		extendDeadline()

		if chunk.Error != nil {
			s.writeSSEError(w, flusher, "Inference error: "+chunk.Error.Error())
			s.toolBridge.CleanupSession(sessionID)
			return
		}

		// Handle rich event types (same as handleStreamingResponse)
		switch chunk.EventType {
		case "session_info", "session_start":
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
					"choices":    []any{},
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
			if chunk.ToolCall != nil {
				// Eagerly register external tool calls for re-suspension rounds
				if s.toolBridge != nil && chunk.EventType == "tool_use" {
					const mcpPrefix = "mcp__cogos-bridge__"
					if strings.HasPrefix(chunk.ToolCall.Name, mcpPrefix) {
						origName := strings.TrimPrefix(chunk.ToolCall.Name, mcpPrefix)
						s.toolBridge.RegisterCall(sessionID, &ToolBridgeCall{
							ToolCallID: chunk.ToolCall.ID,
							Name:       origName,
							Arguments:  string(chunk.ToolCall.Arguments),
							ResultCh:   make(chan ToolBridgeResult, 1),
						})
					}
				}

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

		// Tool result
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

		// Content
		if chunk.Content != "" {
			accumulatedContent.WriteString(chunk.Content)
			openAIChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{{
					Index: 0,
					Delta: &ChatMessage{
						Role:    "assistant",
						Content: StringToRawContent(chunk.Content),
					},
				}},
			}
			data, _ := json.Marshal(openAIChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		// Suspended again — nested tool calls
		if chunk.Done && chunk.Suspended {
			log.Printf("[tool-bridge] Re-suspended: %d more external tool calls", len(chunk.ExternalToolCalls))
			for i, tc := range chunk.ExternalToolCalls {
				toolCallStart := map[string]any{
					"id":      chunk.ID,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []map[string]any{{
						"index": 0,
						"delta": map[string]any{
							"tool_calls": []map[string]any{{
								"index": i,
								"id":    tc.ID,
								"type":  "function",
								"function": map[string]any{
									"name":      tc.Name,
									"arguments": string(tc.Arguments),
								},
							}},
						},
					}},
				}
				data, _ := json.Marshal(toolCallStart)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			finishChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{{
					Index:        0,
					Delta:        &ChatMessage{},
					FinishReason: "tool_calls",
				}},
			}
			data, _ := json.Marshal(finishChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

			// Calls were already eagerly registered via tool_use events above.
			return
		}

		// Final Done
		if chunk.Done {
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

			finishReason := chunk.FinishReason
			if finishReason == "" {
				finishReason = "stop"
			}
			finishChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{{
					Index:        0,
					Delta:        &ChatMessage{},
					FinishReason: finishReason,
				}},
				Usage: usageInfo,
			}
			data, _ := json.Marshal(finishChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

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

			// Clean up the bridge session — CLI has exited
			s.toolBridge.CleanupSession(sessionID)
			return
		}
	}

	// Channel closed unexpectedly
	s.toolBridge.CleanupSession(sessionID)
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

// === Consumer Cursor Handlers (ADR-061) ===

// handleBusAck handles POST /v1/bus/{bus_id}/ack — acknowledge an event.
func (s *serveServer) handleBusAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract bus_id from path: /v1/bus/{bus_id}/ack
	path := strings.TrimPrefix(r.URL.Path, "/v1/bus/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || parts[1] != "ack" || parts[0] == "" {
		http.Error(w, "Expected /v1/bus/{bus_id}/ack", http.StatusBadRequest)
		return
	}
	busID := parts[0]

	var req struct {
		ConsumerID string `json:"consumer_id"`
		Seq        int64  `json:"seq"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.ConsumerID == "" || req.Seq <= 0 {
		http.Error(w, "consumer_id and seq (>0) are required", http.StatusBadRequest)
		return
	}

	cursor, err := s.consumerReg.ack(busID, req.ConsumerID, req.Seq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cursor)
}

// handleBusConsumers handles GET /v1/bus/consumers — list all consumers.
func (s *serveServer) handleBusConsumers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	busID := r.URL.Query().Get("bus_id") // optional filter
	cursors := s.consumerReg.list(busID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"consumers": cursors,
	})
}

// handleBusConsumerDelete handles DELETE /v1/bus/consumers/{consumer_id} — remove a consumer.
func (s *serveServer) handleBusConsumerDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract consumer_id from path: /v1/bus/consumers/{consumer_id}
	consumerID := strings.TrimPrefix(r.URL.Path, "/v1/bus/consumers/")
	if consumerID == "" {
		http.Error(w, "Consumer ID required", http.StatusBadRequest)
		return
	}

	if !s.consumerReg.remove(consumerID) {
		http.Error(w, "Consumer not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
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

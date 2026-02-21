// MCP Server - Model Context Protocol implementation for CogOS
//
// This module provides an MCP server that exposes CogOS resources and tools
// via the standard JSON-RPC 2.0 based MCP protocol.
//
// Protocol: https://modelcontextprotocol.io/
//
// Modes:
//   - Standard: exposes CogOS tools (memory, coherence) via stdio
//   - Bridge (--bridge): merges CogOS tools with OpenClaw platform tools,
//     proxying openclaw_* calls to the OpenClaw gateway via HTTP
//
// Resources exposed:
//   - cog://mem/* (memory sectors)
//   - cog://identity
//   - cog://coherence
//   - cog://signals
//
// Tools exposed:
//   - cogos_memory_search
//   - cogos_memory_read
//   - cogos_memory_write
//   - cogos_coherence_check
//   - openclaw_* (bridge mode only — proxied to OpenClaw gateway)

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// === JSON-RPC 2.0 TYPES ===

// JSONRPCRequest represents an incoming JSON-RPC 2.0 request
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"` // Can be string, number, or null
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse represents an outgoing JSON-RPC 2.0 response
type JSONRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      interface{}   `json:"id,omitempty"`
	Result  interface{}   `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

// JSONRPCError represents a JSON-RPC 2.0 error
type JSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Standard JSON-RPC error codes
const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
)

// === MCP PROTOCOL TYPES ===

// MCPInitializeParams for initialize method
type MCPInitializeParams struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    MCPClientCaps     `json:"capabilities"`
	ClientInfo      MCPImplementation `json:"clientInfo"`
}

// MCPClientCaps represents client capabilities
type MCPClientCaps struct {
	Roots    *MCPRootsCaps    `json:"roots,omitempty"`
	Sampling *MCPSamplingCaps `json:"sampling,omitempty"`
}

// MCPRootsCaps represents roots capability
type MCPRootsCaps struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// MCPSamplingCaps represents sampling capability
type MCPSamplingCaps struct{}

// MCPImplementation represents client/server info
type MCPImplementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// MCPInitializeResult for initialize response
type MCPInitializeResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    MCPServerCaps     `json:"capabilities"`
	ServerInfo      MCPImplementation `json:"serverInfo"`
	Instructions    string            `json:"instructions,omitempty"`
}

// MCPServerCaps represents server capabilities
type MCPServerCaps struct {
	Resources *MCPResourcesCaps `json:"resources,omitempty"`
	Tools     *MCPToolsCaps     `json:"tools,omitempty"`
	Prompts   *MCPPromptsCaps   `json:"prompts,omitempty"`
}

// MCPResourcesCaps represents resources capability
type MCPResourcesCaps struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// MCPToolsCaps represents tools capability
type MCPToolsCaps struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// MCPPromptsCaps represents prompts capability
type MCPPromptsCaps struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// MCPResource represents a resource in resources/list
type MCPResource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// MCPResourcesListResult for resources/list response
type MCPResourcesListResult struct {
	Resources []MCPResource `json:"resources"`
}

// MCPResourceReadParams for resources/read
type MCPResourceReadParams struct {
	URI string `json:"uri"`
}

// MCPResourceContent represents resource content
type MCPResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"` // base64 encoded
}

// MCPResourceReadResult for resources/read response
type MCPResourceReadResult struct {
	Contents []MCPResourceContent `json:"contents"`
}

// MCPTool represents a tool in tools/list
type MCPTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// MCPToolsListResult for tools/list response
type MCPToolsListResult struct {
	Tools []MCPTool `json:"tools"`
}

// MCPToolCallParams for tools/call
type MCPToolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

// MCPToolCallResult for tools/call response
type MCPToolCallResult struct {
	Content []MCPToolContent `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

// MCPToolContent represents tool result content
type MCPToolContent struct {
	Type string `json:"type"` // "text" or "image" or "resource"
	Text string `json:"text,omitempty"`
}

// === OPENCLAW BRIDGE ===

// OpenClawBridge proxies tool calls to the OpenClaw gateway via HTTP
type OpenClawBridge struct {
	BaseURL    string // e.g. "http://localhost:18789"
	Token      string // Bearer token for auth
	SessionKey string // Session context for tool execution
	client     *http.Client
}

// NewOpenClawBridge creates a new bridge client
func NewOpenClawBridge(baseURL, token, sessionKey string) *OpenClawBridge {
	return &OpenClawBridge{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Token:      token,
		SessionKey: sessionKey,
		client: &http.Client{
			Timeout: 60 * time.Second, // Browser/canvas actions can take 10-30s
		},
	}
}

// openAIFunctionTool represents an OpenAI-format tool definition for deserialization.
type openAIFunctionTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description"`
		Parameters  map[string]interface{} `json:"parameters"`
	} `json:"function"`
}

// convertOpenAIToolsToMCP converts OpenAI-format tool definitions (from request body)
// into MCP tool format. This is the bridge between the standard tool convention
// (tools sent in POST /v1/chat/completions body) and the MCP protocol.
func convertOpenAIToolsToMCP(tools []json.RawMessage) []MCPTool {
	var mcpTools []MCPTool
	for _, raw := range tools {
		var oaiTool openAIFunctionTool
		if err := json.Unmarshal(raw, &oaiTool); err != nil {
			continue // skip malformed
		}
		if oaiTool.Type != "function" || oaiTool.Function.Name == "" {
			continue
		}
		schema := oaiTool.Function.Parameters
		if schema == nil {
			schema = map[string]interface{}{"type": "object"}
		}
		mcpTools = append(mcpTools, MCPTool{
			Name:        oaiTool.Function.Name,
			Description: oaiTool.Function.Description,
			InputSchema: schema,
		})
	}
	return mcpTools
}

// LoadToolRegistry reads external tool definitions from the TOOL_REGISTRY env var.
// Tools are serialized as JSON by generateMCPConfig from the request body's tools field.
// This is the push-based alternative to the old pull-based FetchToolManifest approach:
// tools flow FROM the caller's request body, not from an HTTP discovery endpoint.
func (s *MCPServer) LoadToolRegistry() {
	regJSON := os.Getenv("TOOL_REGISTRY")
	if regJSON == "" {
		return
	}
	var tools []MCPTool
	if err := json.Unmarshal([]byte(regJSON), &tools); err != nil {
		fmt.Fprintf(os.Stderr, "[MCP Bridge] Failed to parse TOOL_REGISTRY: %v\n", err)
		return
	}
	s.externalTools = tools
	fmt.Fprintf(os.Stderr, "[MCP Bridge] Loaded %d tools from registry\n", len(tools))
	for _, t := range tools {
		fmt.Fprintf(os.Stderr, "[MCP Bridge]   - %s\n", t.Name)
	}
}

// ProbeGateway verifies connectivity to the OpenClaw gateway.
// Returns nil if the gateway is reachable and authenticated.
func (b *OpenClawBridge) ProbeGateway(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "mcp.bridge.probe_gateway",
		trace.WithAttributes(
			attribute.String("openclaw.url", b.BaseURL),
		),
	)
	defer span.End()

	probeBody, _ := json.Marshal(map[string]interface{}{
		"tool":   "agents_list",
		"action": "json",
		"args":   map[string]interface{}{},
	})

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", b.BaseURL+"/tools/invoke", bytes.NewReader(probeBody))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "create probe request")
		return fmt.Errorf("create probe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "gateway unreachable")
		return fmt.Errorf("gateway probe failed: %w", err)
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	if resp.StatusCode == 401 {
		err := fmt.Errorf("gateway auth failed (401) — check OPENCLAW_TOKEN")
		span.RecordError(err)
		span.SetStatus(codes.Error, "auth failed")
		return err
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("gateway probe returned %d: %s", resp.StatusCode, string(body))
		span.RecordError(err)
		span.SetStatus(codes.Error, "probe failed")
		return err
	}

	span.SetStatus(codes.Ok, "gateway reachable")
	return nil
}

// ExecuteTool calls a tool on the OpenClaw gateway via POST /tools/invoke
func (b *OpenClawBridge) ExecuteTool(ctx context.Context, name string, args map[string]interface{}) (*MCPToolCallResult, error) {
	ctx, span := tracer.Start(ctx, "openclaw.tool.execute",
		trace.WithAttributes(
			attribute.String("tool.name", name),
			attribute.String("openclaw.url", b.BaseURL),
		),
	)
	defer span.End()

	body := map[string]interface{}{
		"tool": name,
		"args": args,
	}
	if b.SessionKey != "" {
		body["sessionKey"] = b.SessionKey
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	span.SetAttributes(attribute.Int("request.size", len(jsonBody)))

	req, err := http.NewRequestWithContext(ctx, "POST", b.BaseURL+"/tools/invoke", bytes.NewReader(jsonBody))
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}

	// Inject trace context into outgoing HTTP request for distributed tracing
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := b.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "tool invocation failed")
		return nil, fmt.Errorf("invoke tool: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("read response: %w", err)
	}

	span.SetAttributes(
		attribute.Int("http.status_code", resp.StatusCode),
		attribute.Int("response.size", len(respBody)),
	)

	if resp.StatusCode != 200 {
		span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return &MCPToolCallResult{
			Content: []MCPToolContent{
				{Type: "text", Text: fmt.Sprintf("Tool error (HTTP %d): %s", resp.StatusCode, string(respBody))},
			},
			IsError: true,
		}, nil
	}

	// Parse the OpenClaw response: { ok: true, result: ... }
	var ocResp struct {
		OK     bool        `json:"ok"`
		Result interface{} `json:"result"`
		Error  *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &ocResp); err != nil {
		return &MCPToolCallResult{
			Content: []MCPToolContent{
				{Type: "text", Text: string(respBody)},
			},
		}, nil
	}

	if !ocResp.OK && ocResp.Error != nil {
		span.SetAttributes(attribute.String("tool.error", ocResp.Error.Message))
		span.SetStatus(codes.Error, "tool returned error")
		return &MCPToolCallResult{
			Content: []MCPToolContent{
				{Type: "text", Text: fmt.Sprintf("Tool error: %s", ocResp.Error.Message)},
			},
			IsError: true,
		}, nil
	}

	span.SetStatus(codes.Ok, "tool executed")

	// Convert result to text
	resultText, err := json.MarshalIndent(ocResp.Result, "", "  ")
	if err != nil {
		resultText = respBody
	}

	return &MCPToolCallResult{
		Content: []MCPToolContent{
			{Type: "text", Text: string(resultText)},
		},
	}, nil
}

// === MCP SERVER ===

// MCPServer handles MCP protocol communication
type MCPServer struct {
	root   string // Workspace root
	reader *bufio.Reader
	writer io.Writer
	debug  bool

	// Bridge mode
	bridge        *OpenClawBridge // Non-nil when --bridge is active
	bridgeOn      bool           // Whether bridge mode is enabled
	externalTools []MCPTool      // Tools registered from external sources (e.g., OpenClaw request body)

	// Tracing — propagated from parent process via TRACEPARENT env var
	traceCtx context.Context
}

// NewMCPServer creates a new MCP server
func NewMCPServer(root string, reader io.Reader, writer io.Writer) *MCPServer {
	return &MCPServer{
		root:   root,
		reader: bufio.NewReader(reader),
		writer: writer,
		debug:  os.Getenv("MCP_DEBUG") != "",
	}
}

// EnableBridge activates bridge mode with the given OpenClaw connection
func (s *MCPServer) EnableBridge(bridge *OpenClawBridge) {
	s.bridge = bridge
	s.bridgeOn = true
}

// Run starts the MCP server main loop
func (s *MCPServer) Run() error {
	if s.bridgeOn {
		s.log("MCP bridge server starting for workspace: %s (OpenClaw: %s)", s.root, s.bridge.BaseURL)
	} else {
		s.log("MCP server starting for workspace: %s", s.root)
	}

	for {
		// Read a line (JSON-RPC message)
		line, err := s.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				s.log("Client disconnected (EOF)")
				return nil
			}
			return fmt.Errorf("read error: %w", err)
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		s.log("← %s", line)

		// Parse JSON-RPC request
		var req JSONRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.sendError(nil, ParseError, "Parse error", err.Error())
			continue
		}

		if req.JSONRPC != "2.0" {
			s.sendError(req.ID, InvalidRequest, "Invalid Request", "jsonrpc must be '2.0'")
			continue
		}

		// Handle the request
		result, rpcErr := s.handleRequest(&req)
		if rpcErr != nil {
			s.sendError(req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		} else if req.ID != nil {
			// Only send response if there was an ID (not a notification)
			s.sendResult(req.ID, result)
		}
	}
}

// handleRequest dispatches to the appropriate handler
func (s *MCPServer) handleRequest(req *JSONRPCRequest) (interface{}, *JSONRPCError) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.Params)
	case "initialized":
		// Notification, no response needed
		return nil, nil
	case "resources/list":
		return s.handleResourcesList(req.Params)
	case "resources/read":
		return s.handleResourcesRead(req.Params)
	case "tools/list":
		return s.handleToolsList(req.Params)
	case "tools/call":
		return s.handleToolsCall(req.Params)
	case "ping":
		return map[string]interface{}{}, nil
	default:
		return nil, &JSONRPCError{
			Code:    MethodNotFound,
			Message: fmt.Sprintf("Method not found: %s", req.Method),
		}
	}
}

// === MCP METHOD HANDLERS ===

// handleInitialize handles the initialize method
func (s *MCPServer) handleInitialize(params json.RawMessage) (interface{}, *JSONRPCError) {
	var p MCPInitializeParams
	if params != nil {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &JSONRPCError{Code: InvalidParams, Message: "Invalid params", Data: err.Error()}
		}
	}

	s.log("Initialize from %s %s (protocol %s)", p.ClientInfo.Name, p.ClientInfo.Version, p.ProtocolVersion)

	serverName := "cogos-mcp"
	instructions := "CogOS MCP server provides access to workspace memory, identity, and coherence state via cog:// URIs."
	if s.bridgeOn {
		serverName = "cogos-bridge"
		instructions = "CogOS MCP bridge provides unified access to both CogOS workspace tools (memory, coherence) and OpenClaw platform tools (message, web-search, sessions, etc.)."
	}

	return &MCPInitializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities: MCPServerCaps{
			Resources: &MCPResourcesCaps{
				Subscribe:   false,
				ListChanged: false,
			},
			Tools: &MCPToolsCaps{
				ListChanged: false,
			},
		},
		ServerInfo: MCPImplementation{
			Name:    serverName,
			Version: Version,
		},
		Instructions: instructions,
	}, nil
}

// handleResourcesList returns available resources
func (s *MCPServer) handleResourcesList(params json.RawMessage) (interface{}, *JSONRPCError) {
	resources := []MCPResource{
		// Memory sectors
		{
			URI:         "cog://mem/semantic",
			Name:        "Semantic Memory",
			Description: "Crystallized knowledge, architecture, research",
			MimeType:    "text/markdown",
		},
		{
			URI:         "cog://mem/episodic",
			Name:        "Episodic Memory",
			Description: "Sessions, decisions, implementations",
			MimeType:    "text/markdown",
		},
		{
			URI:         "cog://mem/procedural",
			Name:        "Procedural Memory",
			Description: "Guides, workflows, how-tos",
			MimeType:    "text/markdown",
		},
		{
			URI:         "cog://mem/reflective",
			Name:        "Reflective Memory",
			Description: "Retrospectives, consolidations",
			MimeType:    "text/markdown",
		},
		// Identity
		{
			URI:         "cog://identity",
			Name:        "Current Identity",
			Description: "Active identity card and configuration",
			MimeType:    "text/markdown",
		},
		// Coherence
		{
			URI:         "cog://coherence",
			Name:        "Workspace Coherence",
			Description: "Coherence state and drift information",
			MimeType:    "application/json",
		},
		// Context
		{
			URI:         "cog://context/current",
			Name:        "Current TAA Context",
			Description: "Full TAA context package showing what the agent sees (identity, temporal, present, semantic tiers)",
			MimeType:    "application/json",
		},
		// Capabilities
		{
			URI:         "cog://capabilities",
			Name:        "Capabilities Manifest",
			Description: "Machine-readable manifest of all workspace capabilities (CLI, HTTP, MCP, URIs)",
			MimeType:    "text/yaml",
		},
	}

	return &MCPResourcesListResult{Resources: resources}, nil
}

// handleResourcesRead reads a resource by URI
func (s *MCPServer) handleResourcesRead(params json.RawMessage) (interface{}, *JSONRPCError) {
	var p MCPResourceReadParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &JSONRPCError{Code: InvalidParams, Message: "Invalid params", Data: err.Error()}
	}

	s.log("Read resource: %s", p.URI)

	// Handle special URIs
	switch {
	case p.URI == "cog://identity":
		return s.readIdentity()
	case p.URI == "cog://coherence":
		return s.readCoherence()
	case p.URI == "cog://context/current":
		return s.readContext()
	case p.URI == "cog://capabilities":
		return s.readCapabilities()
	case strings.HasPrefix(p.URI, "cog://mem/"):
		return s.readMemory(p.URI)
	default:
		// Try generic URI resolution
		return s.readGenericURI(p.URI)
	}
}

// readIdentity reads the current identity
func (s *MCPServer) readIdentity() (interface{}, *JSONRPCError) {
	// Read identity from config
	configPath := filepath.Join(s.root, ".cog", "config", "identity.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, &JSONRPCError{Code: InternalError, Message: "Failed to read identity config", Data: err.Error()}
	}

	return &MCPResourceReadResult{
		Contents: []MCPResourceContent{
			{
				URI:      "cog://identity",
				MimeType: "text/yaml",
				Text:     string(data),
			},
		},
	}, nil
}

// readCoherence reads coherence state
func (s *MCPServer) readCoherence() (interface{}, *JSONRPCError) {
	coherencePath := filepath.Join(s.root, ".cog", "run", "coherence", "coherence.json")
	data, err := os.ReadFile(coherencePath)
	if err != nil {
		// No coherence state yet, return empty state
		emptyState := map[string]interface{}{
			"coherent":  true,
			"drift":     []string{},
			"timestamp": nowISO(),
		}
		jsonData, _ := json.Marshal(emptyState)
		return &MCPResourceReadResult{
			Contents: []MCPResourceContent{
				{
					URI:      "cog://coherence",
					MimeType: "application/json",
					Text:     string(jsonData),
				},
			},
		}, nil
	}

	return &MCPResourceReadResult{
		Contents: []MCPResourceContent{
			{
				URI:      "cog://coherence",
				MimeType: "application/json",
				Text:     string(data),
			},
		},
	}, nil
}

// readContext reads the current TAA context state
// This exposes exactly what the agent sees - all four tiers
func (s *MCPServer) readContext() (interface{}, *JSONRPCError) {
	// Generate a session ID based on current time (for this snapshot)
	sessionID := fmt.Sprintf("mcp-%d", time.Now().Unix())

	// Construct context using TAA pipeline with default profile
	// Note: We pass empty messages since this is a snapshot request,
	// not part of an active conversation
	contextState, err := ConstructContextStateWithProfile(
		[]ChatMessage{}, // No conversation history for snapshot
		sessionID,
		s.root,
		"default", // Use default TAA profile
	)

	if err != nil {
		s.log("TAA context construction partial error: %v", err)
		// Continue - partial context is still useful
	}

	if contextState == nil {
		// Return minimal context if construction failed completely
		contextState = &ContextState{
			CoherenceScore: 0.0,
			ShouldRefresh:  true,
		}
	}

	// Build response with full context visibility
	response := map[string]interface{}{
		"tiers": map[string]interface{}{
			"tier1_identity": formatTierInfo(contextState.Tier1Identity),
			"tier2_temporal": formatTierInfo(contextState.Tier2Temporal),
			"tier3_present":  formatTierInfo(contextState.Tier3Present),
			"tier4_semantic": formatTierInfo(contextState.Tier4Semantic),
		},
		"metadata": map[string]interface{}{
			"total_tokens":    contextState.TotalTokens,
			"coherence_score": contextState.CoherenceScore,
			"should_refresh":  contextState.ShouldRefresh,
			"anchor":          contextState.Anchor,
			"goal":            contextState.Goal,
			"session_id":      sessionID,
			"timestamp":       nowISO(),
		},
		"profile": "default",
	}

	jsonData, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return nil, &JSONRPCError{Code: InternalError, Message: "Failed to serialize context", Data: err.Error()}
	}

	return &MCPResourceReadResult{
		Contents: []MCPResourceContent{
			{
				URI:      "cog://context/current",
				MimeType: "application/json",
				Text:     string(jsonData),
			},
		},
	}, nil
}

// readCapabilities reads the capabilities manifest
func (s *MCPServer) readCapabilities() (interface{}, *JSONRPCError) {
	// Read the capabilities manifest from memory
	manifestPath := filepath.Join(s.root, ".cog", "mem", "semantic", "architecture", "capabilities-manifest.cog.md")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, &JSONRPCError{Code: InternalError, Message: "Failed to read capabilities manifest", Data: err.Error()}
	}

	return &MCPResourceReadResult{
		Contents: []MCPResourceContent{
			{
				URI:      "cog://capabilities",
				MimeType: "text/yaml",
				Text:     string(data),
			},
		},
	}, nil
}

// formatTierInfo creates a summary of a tier for JSON output
func formatTierInfo(tier *ContextTier) interface{} {
	if tier == nil {
		return map[string]interface{}{
			"loaded":  false,
			"tokens":  0,
			"content": nil,
		}
	}

	// For large content, truncate to first 500 chars with indicator
	contentPreview := tier.Content
	truncated := false
	if len(contentPreview) > 500 {
		contentPreview = contentPreview[:500] + "..."
		truncated = true
	}

	return map[string]interface{}{
		"loaded":            true,
		"tokens":            tier.Tokens,
		"source":            tier.Source,
		"content_length":    len(tier.Content),
		"content_preview":   contentPreview,
		"content_truncated": truncated,
	}
}

// readMemory reads a memory sector or document
func (s *MCPServer) readMemory(uri string) (interface{}, *JSONRPCError) {
	// Parse URI: cog://mem/semantic/... -> .cog/mem/semantic/...
	path := strings.TrimPrefix(uri, "cog://")
	fullPath := filepath.Join(s.root, ".cog", path)

	// Check if it's a directory or file
	info, err := os.Stat(fullPath)
	if err != nil {
		// Try with .cog.md extension
		fullPath = fullPath + ".cog.md"
		info, err = os.Stat(fullPath)
		if err != nil {
			return nil, &JSONRPCError{Code: InvalidParams, Message: "Resource not found", Data: uri}
		}
	}

	if info.IsDir() {
		// List directory contents
		return s.listDirectory(uri, fullPath)
	}

	// Read file
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, &JSONRPCError{Code: InternalError, Message: "Failed to read file", Data: err.Error()}
	}

	mimeType := "text/markdown"
	if strings.HasSuffix(fullPath, ".json") {
		mimeType = "application/json"
	} else if strings.HasSuffix(fullPath, ".yaml") || strings.HasSuffix(fullPath, ".yml") {
		mimeType = "text/yaml"
	}

	return &MCPResourceReadResult{
		Contents: []MCPResourceContent{
			{
				URI:      uri,
				MimeType: mimeType,
				Text:     string(data),
			},
		},
	}, nil
}

// listDirectory lists files in a directory as a text listing
func (s *MCPServer) listDirectory(uri, path string) (interface{}, *JSONRPCError) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, &JSONRPCError{Code: InternalError, Message: "Failed to read directory", Data: err.Error()}
	}

	var listing strings.Builder
	listing.WriteString(fmt.Sprintf("# Contents of %s\n\n", uri))

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			listing.WriteString(fmt.Sprintf("- %s/\n", name))
		} else {
			listing.WriteString(fmt.Sprintf("- %s\n", name))
		}
	}

	return &MCPResourceReadResult{
		Contents: []MCPResourceContent{
			{
				URI:      uri,
				MimeType: "text/markdown",
				Text:     listing.String(),
			},
		},
	}, nil
}

// readGenericURI reads any cog:// URI using the resolver
func (s *MCPServer) readGenericURI(uri string) (interface{}, *JSONRPCError) {
	resolved, err := resolveURI(uri)
	if err != nil {
		return nil, &JSONRPCError{Code: InvalidParams, Message: "Failed to resolve URI", Data: err.Error()}
	}

	fullPath := filepath.Join(s.root, resolved)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, &JSONRPCError{Code: InternalError, Message: "Failed to read file", Data: err.Error()}
	}

	mimeType := "text/markdown"
	if strings.HasSuffix(fullPath, ".json") {
		mimeType = "application/json"
	}

	return &MCPResourceReadResult{
		Contents: []MCPResourceContent{
			{
				URI:      uri,
				MimeType: mimeType,
				Text:     string(data),
			},
		},
	}, nil
}

// GetMCPTools returns the canonical list of CogOS-local MCP tools
// (excludes workflow_invoke in bridge mode as it's not needed)
func GetMCPTools() []MCPTool {
	return []MCPTool{
		{
			Name:        "cogos_memory_search",
			Description: "Search workspace memory for documents matching a query",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "Search query",
					},
					"sector": map[string]interface{}{
						"type":        "string",
						"description": "Memory sector to search (semantic, episodic, procedural, reflective). If omitted, searches all.",
						"enum":        []string{"semantic", "episodic", "procedural", "reflective"},
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of results (default: 10)",
						"default":     10,
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "cogos_memory_read",
			Description: "Read a specific memory document by path",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Path to document (e.g., 'semantic/architecture/kernel.cog.md')",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "cogos_memory_write",
			Description: "Write a new document to memory",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Path for new document (e.g., 'semantic/insights/discovery.cog.md')",
					},
					"title": map[string]interface{}{
						"type":        "string",
						"description": "Document title",
					},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "Document content (markdown)",
					},
					"type": map[string]interface{}{
						"type":        "string",
						"description": "Cogdoc type (insight, decision, guide, etc.)",
						"default":     "knowledge",
					},
				},
				"required": []string{"path", "title", "content"},
			},
		},
		{
			Name:        "cogos_coherence_check",
			Description: "Check workspace coherence status",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	}
}

// handleToolsList returns available tools, merging local + external in bridge mode.
// External tools come from the tool registry (populated from the request body's tools field),
// not from an HTTP discovery endpoint.
func (s *MCPServer) handleToolsList(params json.RawMessage) (interface{}, *JSONRPCError) {
	tools := GetMCPTools()

	if s.bridgeOn && len(s.externalTools) > 0 {
		for _, rt := range s.externalTools {
			// Namespace external tools with openclaw_ prefix
			namespaced := rt
			namespaced.Name = "openclaw_" + rt.Name
			tools = append(tools, namespaced)
		}
		fmt.Fprintf(os.Stderr, "[MCP Bridge] tools/list: %d local + %d external = %d total\n",
			len(GetMCPTools()), len(s.externalTools), len(tools))
	}

	return &MCPToolsListResult{Tools: tools}, nil
}

// handleToolsCall executes a tool, dispatching local vs proxy in bridge mode
func (s *MCPServer) handleToolsCall(params json.RawMessage) (interface{}, *JSONRPCError) {
	var p MCPToolCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &JSONRPCError{Code: InvalidParams, Message: "Invalid params", Data: err.Error()}
	}

	s.log("Tool call: %s with %v", p.Name, p.Arguments)

	ctx := s.traceCtx
	if ctx == nil {
		ctx = context.Background()
	}

	// In bridge mode, dispatch openclaw_* to remote
	if s.bridgeOn && strings.HasPrefix(p.Name, "openclaw_") {
		_, span := tracer.Start(ctx, "mcp.tool.call",
			trace.WithAttributes(
				attribute.String("tool.name", p.Name),
				attribute.String("tool.routing", "remote"),
			),
		)
		defer span.End()

		remoteName := strings.TrimPrefix(p.Name, "openclaw_")
		result, err := s.bridge.ExecuteTool(ctx, remoteName, p.Arguments)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "bridge call failed")
			return nil, &JSONRPCError{Code: InternalError, Message: "Bridge call failed", Data: err.Error()}
		}
		if result.IsError {
			span.SetStatus(codes.Error, "tool returned error")
		} else {
			span.SetStatus(codes.Ok, "tool executed")
		}
		return result, nil
	}

	// Handle local CogOS tools
	_, span := tracer.Start(ctx, "mcp.tool.call",
		trace.WithAttributes(
			attribute.String("tool.name", p.Name),
			attribute.String("tool.routing", "local"),
		),
	)
	defer span.End()

	var result interface{}
	var rpcErr *JSONRPCError
	switch p.Name {
	case "cogos_memory_search":
		result, rpcErr = s.toolMemorySearch(p.Arguments)
	case "cogos_memory_read":
		result, rpcErr = s.toolMemoryRead(p.Arguments)
	case "cogos_memory_write":
		result, rpcErr = s.toolMemoryWrite(p.Arguments)
	case "cogos_coherence_check":
		result, rpcErr = s.toolCoherenceCheck(p.Arguments)
	default:
		span.SetStatus(codes.Error, "unknown tool")
		return nil, &JSONRPCError{Code: MethodNotFound, Message: fmt.Sprintf("Unknown tool: %s", p.Name)}
	}

	if rpcErr != nil {
		span.SetStatus(codes.Error, rpcErr.Message)
	} else {
		span.SetStatus(codes.Ok, "tool executed")
	}
	return result, rpcErr
}

// toolMemorySearch searches memory using ripgrep
func (s *MCPServer) toolMemorySearch(args map[string]interface{}) (interface{}, *JSONRPCError) {
	query, _ := args["query"].(string)
	if query == "" {
		return nil, &JSONRPCError{Code: InvalidParams, Message: "query is required"}
	}

	sector, _ := args["sector"].(string)
	limit := 10
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}

	// Build search path
	searchPath := filepath.Join(s.root, ".cog", "mem")
	if sector != "" {
		searchPath = filepath.Join(searchPath, sector)
	}

	// Use ripgrep for fast search
	cmd := exec.Command("rg", "-l", "-i", "--max-count", "1", query, searchPath)
	output, err := cmd.Output()
	if err != nil {
		// No matches is not an error
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return &MCPToolCallResult{
				Content: []MCPToolContent{
					{Type: "text", Text: "No results found."},
				},
			}, nil
		}
		return nil, &JSONRPCError{Code: InternalError, Message: "Search failed", Data: err.Error()}
	}

	// Parse results
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) > limit {
		lines = lines[:limit]
	}

	var results strings.Builder
	results.WriteString(fmt.Sprintf("Found %d results for '%s':\n\n", len(lines), query))

	for _, line := range lines {
		if line == "" {
			continue
		}
		// Convert absolute path to cog:// URI
		relPath, _ := filepath.Rel(filepath.Join(s.root, ".cog"), line)
		uri := "cog://" + relPath
		uri = strings.TrimSuffix(uri, ".cog.md")
		results.WriteString(fmt.Sprintf("- %s\n", uri))
	}

	return &MCPToolCallResult{
		Content: []MCPToolContent{
			{Type: "text", Text: results.String()},
		},
	}, nil
}

// toolMemoryRead reads a memory document
func (s *MCPServer) toolMemoryRead(args map[string]interface{}) (interface{}, *JSONRPCError) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, &JSONRPCError{Code: InvalidParams, Message: "path is required"}
	}

	// Build full path
	fullPath := filepath.Join(s.root, ".cog", "mem", path)

	// Try with .cog.md extension if not present
	if !strings.HasSuffix(fullPath, ".md") {
		fullPath = fullPath + ".cog.md"
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, &JSONRPCError{Code: InvalidParams, Message: "Document not found", Data: path}
	}

	return &MCPToolCallResult{
		Content: []MCPToolContent{
			{Type: "text", Text: string(data)},
		},
	}, nil
}

// toolMemoryWrite writes a memory document
func (s *MCPServer) toolMemoryWrite(args map[string]interface{}) (interface{}, *JSONRPCError) {
	path, _ := args["path"].(string)
	title, _ := args["title"].(string)
	content, _ := args["content"].(string)
	docType, _ := args["type"].(string)

	if path == "" || title == "" || content == "" {
		return nil, &JSONRPCError{Code: InvalidParams, Message: "path, title, and content are required"}
	}

	if docType == "" {
		docType = "knowledge"
	}

	// Build full path
	fullPath := filepath.Join(s.root, ".cog", "mem", path)
	if !strings.HasSuffix(fullPath, ".cog.md") {
		fullPath = fullPath + ".cog.md"
	}

	// Ensure directory exists
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, &JSONRPCError{Code: InternalError, Message: "Failed to create directory", Data: err.Error()}
	}

	// Generate ID from path
	id := strings.TrimSuffix(filepath.Base(path), ".cog.md")
	id = strings.TrimSuffix(id, ".md")

	// Build document with frontmatter
	var doc strings.Builder
	doc.WriteString("---\n")
	doc.WriteString(fmt.Sprintf("type: %s\n", docType))
	doc.WriteString(fmt.Sprintf("id: %s\n", id))
	doc.WriteString(fmt.Sprintf("title: \"%s\"\n", title))
	doc.WriteString(fmt.Sprintf("created: %s\n", nowISO()[:10]))
	doc.WriteString("---\n\n")
	doc.WriteString(content)

	if err := writeAtomic(fullPath, []byte(doc.String()), 0644); err != nil {
		return nil, &JSONRPCError{Code: InternalError, Message: "Failed to write document", Data: err.Error()}
	}

	return &MCPToolCallResult{
		Content: []MCPToolContent{
			{Type: "text", Text: fmt.Sprintf("Document written to cog://mem/%s", path)},
		},
	}, nil
}

// toolCoherenceCheck checks coherence status
func (s *MCPServer) toolCoherenceCheck(args map[string]interface{}) (interface{}, *JSONRPCError) {
	state, err := recordCoherenceState(s.root)
	if err != nil {
		return nil, &JSONRPCError{Code: InternalError, Message: "Coherence check failed", Data: err.Error()}
	}

	var result strings.Builder
	if state.Coherent {
		result.WriteString("Workspace is coherent with canonical state.\n")
	} else {
		result.WriteString(fmt.Sprintf("Workspace has %d files drifted from canonical:\n\n", len(state.Drift)))
		for _, file := range state.Drift {
			result.WriteString(fmt.Sprintf("- %s\n", file))
		}
	}

	result.WriteString(fmt.Sprintf("\nCanonical hash: %s\n", state.CanonicalHash))
	result.WriteString(fmt.Sprintf("Current hash: %s\n", state.CurrentHash))

	return &MCPToolCallResult{
		Content: []MCPToolContent{
			{Type: "text", Text: result.String()},
		},
	}, nil
}

// === UTILITY METHODS ===

// sendResult sends a successful response
func (s *MCPServer) sendResult(id interface{}, result interface{}) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	s.send(resp)
}

// sendError sends an error response
func (s *MCPServer) sendError(id interface{}, code int, message string, data interface{}) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &JSONRPCError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
	s.send(resp)
}

// send writes a response to the output
func (s *MCPServer) send(resp JSONRPCResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		s.log("Failed to marshal response: %v", err)
		return
	}
	s.log("→ %s", string(data))
	fmt.Fprintln(s.writer, string(data))
}

// log writes debug output to stderr
func (s *MCPServer) log(format string, args ...interface{}) {
	if s.debug {
		fmt.Fprintf(os.Stderr, "[MCP] "+format+"\n", args...)
	}
}

// === COMMAND ENTRY POINT ===

// cmdMCP handles the `cog mcp` command
func cmdMCP(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: cog mcp serve [--bridge]")
		return 1
	}

	switch args[0] {
	case "serve":
		bridge := false
		for _, a := range args[1:] {
			if a == "--bridge" {
				bridge = true
			}
		}
		if err := runMCPServe(bridge); err != nil {
			fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown mcp subcommand: %s\n", args[0])
		return 1
	}
}

// runMCPServe runs the MCP server on stdio
func runMCPServe(bridgeMode bool) error {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("no workspace found: %w", err)
	}

	// Initialize OTEL tracing for the bridge subprocess.
	// Uses the same OTEL_EXPORTER_OTLP_ENDPOINT passed via MCP config env vars.
	tp, otelErr := initTracer()
	if otelErr != nil {
		fmt.Fprintf(os.Stderr, "[MCP Bridge] otel: failed to init tracer: %v\n", otelErr)
	} else if tp != nil {
		fmt.Fprintf(os.Stderr, "[MCP Bridge] otel: tracing enabled (endpoint=%s)\n", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
		defer shutdownTracer(tp)
	}

	// Extract parent trace context from TRACEPARENT env var (W3C Trace Context).
	// This links bridge spans to the parent CogOS serve.go trace.
	ctx := context.Background()
	if traceparent := os.Getenv("TRACEPARENT"); traceparent != "" {
		carrier := propagation.MapCarrier{"traceparent": traceparent}
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
		fmt.Fprintf(os.Stderr, "[MCP Bridge] otel: linked to parent trace via TRACEPARENT\n")
	}

	// Bridge diagnostics always go to stderr (not gated by MCP_DEBUG)
	logBridge := func(format string, args ...interface{}) {
		fmt.Fprintf(os.Stderr, "[MCP Bridge] "+format+"\n", args...)
	}

	server := NewMCPServer(root, os.Stdin, os.Stdout)
	server.traceCtx = ctx

	if bridgeMode {
		_, span := tracer.Start(ctx, "mcp.bridge.activate",
			trace.WithAttributes(
				attribute.String("openclaw.url", os.Getenv("OPENCLAW_URL")),
				attribute.Bool("bridge.mode", true),
			),
		)

		openclawURL := os.Getenv("OPENCLAW_URL")
		openclawToken := os.Getenv("OPENCLAW_TOKEN")
		sessionID := os.Getenv("SESSION_ID")

		logBridge("Activating bridge mode")
		logBridge("  OPENCLAW_URL=%s", openclawURL)
		logBridge("  OPENCLAW_TOKEN=%s", maskToken(openclawToken))
		logBridge("  SESSION_ID=%s", sessionID)
		logBridge("  COG_ROOT=%s", os.Getenv("COG_ROOT"))

		if openclawURL == "" {
			logBridge("ERROR: OPENCLAW_URL is empty — bridge cannot activate")
			logBridge("  Set OPENCLAW_URL env var or pass via MCP config")
			span.SetStatus(codes.Error, "missing OPENCLAW_URL")
			span.End()
			return fmt.Errorf("OPENCLAW_URL environment variable required for bridge mode")
		}

		bridge := NewOpenClawBridge(openclawURL, openclawToken, sessionID)

		// Probe the gateway to verify connectivity (non-blocking — tools come from registry)
		logBridge("Probing gateway at %s/tools/invoke ...", openclawURL)
		if err := bridge.ProbeGateway(ctx); err != nil {
			logBridge("WARNING: Gateway probe failed: %v", err)
			logBridge("  Bridge enabled but tool execution may fail")
		} else {
			logBridge("Gateway probe OK — tool execution endpoint reachable")
		}

		server.EnableBridge(bridge)

		// Load external tools from registry (populated by generateMCPConfig from request body)
		server.LoadToolRegistry()
		logBridge("Bridge enabled: %d external tools registered", len(server.externalTools))

		span.SetAttributes(attribute.Int("tools.external_count", len(server.externalTools)))
		span.SetStatus(codes.Ok, "bridge activated")
		span.End()
	}

	return server.Run()
}

// maskToken returns a masked version of a token for safe logging
func maskToken(token string) string {
	if token == "" {
		return "(empty)"
	}
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "..." + token[len(token)-4:]
}

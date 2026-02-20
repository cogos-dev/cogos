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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
			Timeout: 10 * time.Second,
		},
	}
}

// OpenClawToolManifest represents the response from GET /tools/list
type OpenClawToolManifest struct {
	Tools []OpenClawToolDef `json:"tools"`
}

// OpenClawToolDef represents a tool definition from OpenClaw
type OpenClawToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// FetchToolManifest retrieves the tool manifest from OpenClaw's GET /tools/list
func (b *OpenClawBridge) FetchToolManifest() ([]MCPTool, error) {
	req, err := http.NewRequest("GET", b.BaseURL+"/tools/list", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch tool manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tools/list returned %d: %s", resp.StatusCode, string(body))
	}

	var manifest OpenClawToolManifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	// Convert to MCP tools
	var tools []MCPTool
	for _, t := range manifest.Tools {
		tools = append(tools, MCPTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return tools, nil
}

// ExecuteTool calls a tool on the OpenClaw gateway via POST /tools/invoke
func (b *OpenClawBridge) ExecuteTool(name string, args map[string]interface{}) (*MCPToolCallResult, error) {
	body := map[string]interface{}{
		"tool": name,
		"args": args,
	}
	if b.SessionKey != "" {
		body["sessionKey"] = b.SessionKey
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", b.BaseURL+"/tools/invoke", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("invoke tool: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
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
		return &MCPToolCallResult{
			Content: []MCPToolContent{
				{Type: "text", Text: fmt.Sprintf("Tool error: %s", ocResp.Error.Message)},
			},
			IsError: true,
		}, nil
	}

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
	bridge    *OpenClawBridge // Non-nil when --bridge is active
	bridgeOn  bool           // Whether bridge mode is enabled
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

// bridgeExcludedTools lists OpenClaw tools excluded from bridge mode
// (Phase 3 — deep session state required)
var bridgeExcludedTools = map[string]bool{
	"browser": true,
	"canvas":  true,
}

// handleToolsList returns available tools, merging local + remote in bridge mode
func (s *MCPServer) handleToolsList(params json.RawMessage) (interface{}, *JSONRPCError) {
	tools := GetMCPTools()

	if s.bridgeOn {
		remotTools, err := s.bridge.FetchToolManifest()
		if err != nil {
			s.log("Bridge: failed to fetch remote tools: %v", err)
			// Continue with local tools only — don't fail the whole list
		} else {
			for _, rt := range remotTools {
				// Skip excluded tools
				if bridgeExcludedTools[rt.Name] {
					continue
				}
				// Prefix with openclaw_ to avoid namespace collisions
				rt.Name = "openclaw_" + rt.Name
				tools = append(tools, rt)
			}
			s.log("Bridge: merged %d remote tools (total: %d)", len(remotTools), len(tools))
		}
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

	// In bridge mode, dispatch openclaw_* to remote
	if s.bridgeOn && strings.HasPrefix(p.Name, "openclaw_") {
		remoteName := strings.TrimPrefix(p.Name, "openclaw_")
		result, err := s.bridge.ExecuteTool(remoteName, p.Arguments)
		if err != nil {
			return nil, &JSONRPCError{Code: InternalError, Message: "Bridge call failed", Data: err.Error()}
		}
		return result, nil
	}

	// Handle local CogOS tools
	switch p.Name {
	case "cogos_memory_search":
		return s.toolMemorySearch(p.Arguments)
	case "cogos_memory_read":
		return s.toolMemoryRead(p.Arguments)
	case "cogos_memory_write":
		return s.toolMemoryWrite(p.Arguments)
	case "cogos_coherence_check":
		return s.toolCoherenceCheck(p.Arguments)
	default:
		return nil, &JSONRPCError{Code: MethodNotFound, Message: fmt.Sprintf("Unknown tool: %s", p.Name)}
	}
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

	server := NewMCPServer(root, os.Stdin, os.Stdout)

	if bridgeMode {
		openclawURL := os.Getenv("OPENCLAW_URL")
		openclawToken := os.Getenv("OPENCLAW_TOKEN")
		sessionID := os.Getenv("SESSION_ID")

		if openclawURL == "" {
			return fmt.Errorf("OPENCLAW_URL environment variable required for bridge mode")
		}

		bridge := NewOpenClawBridge(openclawURL, openclawToken, sessionID)
		server.EnableBridge(bridge)
	}

	return server.Run()
}

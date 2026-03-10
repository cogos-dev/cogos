package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ToolRouter is the Capability Router for tool-as-RPC on the CogBus.
// It listens for tool.invoke events, validates permissions, dispatches tool
// execution, and posts tool.result responses back onto the bus.
// When a bridge is configured, unknown tools are dispatched to OpenClaw.
type ToolRouter struct {
	manager  *busSessionManager
	root     string               // workspace root for kernel operations
	bridge   *OpenClawBridge      // remote tool dispatch (nil = local only)
	resolver *CapabilityResolver  // agent capability lookups (nil = skip)

	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
}

// NewToolRouter creates a new ToolRouter bound to the given bus session manager
// and workspace root. If bridge is non-nil, unknown tools fall through to
// remote dispatch via the OpenClaw gateway. If resolver is non-nil, bridge
// dispatch validates agent capabilities before forwarding.
func NewToolRouter(manager *busSessionManager, root string, bridge *OpenClawBridge, resolver *CapabilityResolver) *ToolRouter {
	return &ToolRouter{
		manager:  manager,
		root:     root,
		bridge:   bridge,
		resolver: resolver,
		stopCh:   make(chan struct{}),
	}
}

// Start begins listening for tool.invoke events on all active buses.
// It registers a named event handler on the bus session manager so that
// tool.invoke events are handled as they arrive.
func (tr *ToolRouter) Start() {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	if tr.running {
		return
	}
	tr.running = true
	tr.stopCh = make(chan struct{})

	tr.manager.AddEventHandler("tool-router", func(busID string, evt *CogBlock) {
		tr.mu.Lock()
		running := tr.running
		tr.mu.Unlock()
		if !running {
			return
		}
		if evt.Type == BlockToolInvoke {
			go tr.handleToolInvokeEvent(busID, evt)
		}
	})

	log.Printf("[tool-router] started — listening for %s events", BlockToolInvoke)
}

// Stop signals the tool router to stop processing new events and removes
// its event handler from the bus session manager.
func (tr *ToolRouter) Stop() {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	if !tr.running {
		return
	}
	tr.running = false
	close(tr.stopCh)
	tr.manager.RemoveEventHandler("tool-router")
	log.Printf("[tool-router] stopped")
}

// handleToolInvokeEvent processes a single tool.invoke bus event.
func (tr *ToolRouter) handleToolInvokeEvent(busID string, evt *CogBlock) {
	start := time.Now()

	// Decode the payload into ToolInvokePayload.
	invoke, err := decodeToolInvokePayload(evt.Payload)
	if err != nil {
		log.Printf("[tool-router] bus=%s seq=%d decode error: %v", busID, evt.Seq, err)
		tr.postError(busID, "", "payload_decode_error", err.Error(), start)
		return
	}

	log.Printf("[tool-router] bus=%s seq=%d tool=%s caller=%s target=%s reqID=%s",
		busID, evt.Seq, invoke.Tool, invoke.CallerAgent, invoke.TargetAgent, invoke.RequestID)

	// Validate required fields.
	if invoke.RequestID == "" {
		tr.postError(busID, "", "validation_error", "requestId is required", start)
		return
	}
	if invoke.Tool == "" {
		tr.postError(busID, invoke.RequestID, "validation_error", "tool name is required", start)
		return
	}

	// Check caller's permissions: load caller agent CRD and verify the tool isn't denied.
	if invoke.CallerAgent != "" {
		if err := tr.checkCallerPermissions(invoke.CallerAgent, invoke.Tool); err != nil {
			log.Printf("[tool-router] bus=%s reqID=%s permission denied for caller %s: %v",
				busID, invoke.RequestID, invoke.CallerAgent, err)
			tr.postError(busID, invoke.RequestID, "permission_denied", err.Error(), start)
			return
		}
	}

	// If a target agent is specified, verify it exists and has the tool in its allow list.
	if invoke.TargetAgent != "" {
		if err := tr.checkTargetPermissions(invoke.TargetAgent, invoke.Tool); err != nil {
			log.Printf("[tool-router] bus=%s reqID=%s target agent %s check failed: %v",
				busID, invoke.RequestID, invoke.TargetAgent, err)
			tr.postError(busID, invoke.RequestID, "target_agent_error", err.Error(), start)
			return
		}
	}

	// Determine if this is a headless agent dispatch. When a target agent is
	// specified and is headless, we use the optimized path that skips inference
	// and logs the dispatch separately for observability.
	headless := invoke.TargetAgent != "" && tr.isHeadlessAgent(invoke.TargetAgent)
	executedBy := "kernel:tool-router"
	if headless {
		executedBy = "kernel:tool-router:headless"
		log.Printf("[tool-router] bus=%s reqID=%s headless dispatch for agent=%s tool=%s",
			busID, invoke.RequestID, invoke.TargetAgent, invoke.Tool)
	}

	// Dispatch tool execution.
	result, dispatch, execErr := tr.executeTool(invoke.Tool, invoke.Args, busID, invoke.CallerAgent)

	durationMs := time.Since(start).Milliseconds()

	// Build the result payload.
	resultPayload := ToolResultPayload{
		RequestID:  invoke.RequestID,
		ExecutedBy: executedBy,
		DurationMs: durationMs,
		Tool:       invoke.Tool,
		Dispatch:   dispatch,
	}
	if execErr != nil {
		resultPayload.Error = execErr.Error()
		log.Printf("[tool-router] bus=%s reqID=%s tool=%s error=%s duration=%dms",
			busID, invoke.RequestID, invoke.Tool, execErr.Error(), durationMs)
	} else {
		resultPayload.Result = result
		// Compute serialized result size for observability
		if resultBytes, err := json.Marshal(result); err == nil {
			resultPayload.ResultSize = len(resultBytes)
		}
		log.Printf("[tool-router] bus=%s reqID=%s tool=%s success duration=%dms size=%d headless=%v",
			busID, invoke.RequestID, invoke.Tool, durationMs, resultPayload.ResultSize, headless)
	}

	// Post tool.result back onto the bus.
	tr.postResult(busID, resultPayload)
}

// checkCallerPermissions loads the caller agent's CRD and verifies the tool
// is not in the deny list.
func (tr *ToolRouter) checkCallerPermissions(callerAgent, tool string) error {
	policy, err := GetAgentCRDToolPolicy(tr.root, callerAgent)
	if err != nil {
		// If we can't load the CRD at all, treat it as a non-fatal warning
		// but still allow (backward-compatible with agents that have no CRD).
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to load caller CRD: %w", err)
	}
	if policy == nil {
		return nil // No policy = unrestricted.
	}

	// Check deny list.
	for _, denied := range policy.DenyTools {
		if matchToolPattern(denied, tool) {
			return fmt.Errorf("tool %q is denied for agent %q", tool, callerAgent)
		}
	}

	return nil
}

// checkTargetPermissions verifies that the target agent exists and has the tool
// in its capability allow list.
func (tr *ToolRouter) checkTargetPermissions(targetAgent, tool string) error {
	crd, err := LoadAgentCRD(tr.root, targetAgent)
	if err != nil {
		return fmt.Errorf("target agent %q not found: %w", targetAgent, err)
	}

	// If the agent has an explicit allow list, verify the tool is in it.
	allowList := crd.Spec.Capabilities.Tools.Allow
	if len(allowList) > 0 {
		found := false
		for _, allowed := range allowList {
			if matchToolPattern(allowed, tool) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("tool %q not in allow list for agent %q", tool, targetAgent)
		}
	}

	return nil
}

// matchToolPattern checks whether a tool name matches a pattern.
// Supports exact match and glob-style wildcard suffix (e.g. "memory_*").
func matchToolPattern(pattern, tool string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(tool, prefix)
	}
	return pattern == tool
}

// executeTool dispatches to the appropriate built-in tool implementation.
// Returns (result, dispatch_path, error). dispatch is "builtin" or "bridge".
// busID and callerAgent are used to derive a session key for bridge dispatch
// so the gateway can track which session/agent initiated the call.
func (tr *ToolRouter) executeTool(tool string, args map[string]any, busID, callerAgent string) (any, string, error) {
	switch tool {
	case "memory_search":
		r, err := tr.toolMemorySearch(args)
		return r, "builtin", err
	case "memory_get":
		r, err := tr.toolMemoryGet(args)
		return r, "builtin", err
	case "memory_write":
		r, err := tr.toolMemoryWrite(args)
		return r, "builtin", err
	case "read":
		r, err := tr.toolRead(args)
		return r, "builtin", err
	default:
		// Remote dispatch via OpenClaw bridge
		if tr.bridge != nil {
			// Resolver-based capability validation: if we know who the caller
			// is and a resolver is wired, verify the agent can invoke this tool.
			if callerAgent != "" && tr.resolver != nil {
				if _, known := tr.resolver.ResolveAgent(callerAgent); known {
					if !tr.resolver.CanInvokeTool(callerAgent, tool) {
						return nil, "bridge", fmt.Errorf("agent %q is not capable of tool %q", callerAgent, tool)
					}
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Derive session key from bus event context for gateway tracking.
			// Format: "busID:callerAgent" — identifies which bus channel and
			// agent initiated this tool call.
			sessionKey := tr.bridge.SessionKey // fall back to bridge default
			if busID != "" || callerAgent != "" {
				sessionKey = busID + ":" + callerAgent
			}

			result, err := tr.bridge.ExecuteToolWithSession(ctx, tool, args, sessionKey)
			if err != nil {
				return nil, "bridge", fmt.Errorf("remote tool %q: %w", tool, err)
			}
			return result, "bridge", nil
		}
		return nil, "", fmt.Errorf("unknown tool: %s", tool)
	}
}

// ─── Headless Agent Tool Dispatch ────────────────────────────────────────────────

// HandleHeadlessTool dispatches a tool call for a headless agent without inference.
// Returns the tool result directly, bypassing the LLM pipeline. This is the public
// API for components (e.g., reactor subscriptions) that know they are targeting a
// headless agent and want direct tool-as-RPC without going through bus events.
func (tr *ToolRouter) HandleHeadlessTool(agentID, tool string, args map[string]any) (any, error) {
	// Validate the agent is headless.
	if !tr.isHeadlessAgent(agentID) {
		return nil, fmt.Errorf("agent %q is not headless or not found", agentID)
	}

	result, path, err := tr.executeTool(tool, args, "", agentID)
	if err != nil {
		return nil, fmt.Errorf("headless tool dispatch via %s: %w", path, err)
	}

	log.Printf("[tool-router] headless dispatch agent=%s tool=%s via=%s", agentID, tool, path)
	return result, nil
}

// isHeadlessAgent checks whether the given agent is of type "headless" by
// consulting the capability resolver cache first, then falling back to loading
// the agent CRD from disk. Returns false if the agent cannot be found.
func (tr *ToolRouter) isHeadlessAgent(agentID string) bool {
	// Fast path: check the in-memory capability cache.
	if tr.resolver != nil {
		if caps, ok := tr.resolver.ResolveAgent(agentID); ok {
			return caps.AgentType == "headless"
		}
	}

	// Slow path: load the CRD from disk.
	crd, err := LoadAgentCRD(tr.root, agentID)
	if err != nil {
		return false
	}
	return crd.Spec.Type == "headless"
}

// ─── Built-in Tool Implementations ──────────────────────────────────────────────

// toolMemorySearch searches CogDocs memory using the kernel MemorySearch function.
func (tr *ToolRouter) toolMemorySearch(args map[string]any) (any, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}

	limit := 10
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	sector, _ := args["sector"].(string)

	results, err := MemorySearch(tr.root, query, false, 0, 0.0, false)
	if err != nil {
		return nil, fmt.Errorf("memory search failed: %w", err)
	}

	// Filter by sector if specified.
	if sector != "" {
		filtered := make([]MemorySearchResult, 0, len(results))
		for _, r := range results {
			if strings.Contains(r.Path, string(filepath.Separator)+sector+string(filepath.Separator)) ||
				strings.HasPrefix(r.Path, sector+string(filepath.Separator)) {
				filtered = append(filtered, r)
			}
		}
		results = filtered
	}

	// Apply limit.
	if len(results) > limit {
		results = results[:limit]
	}

	// Convert to a serializable format.
	type searchHit struct {
		URI   string  `json:"uri"`
		Path  string  `json:"path"`
		Title string  `json:"title,omitempty"`
		Score float64 `json:"score"`
		Type  string  `json:"type,omitempty"`
	}
	hits := make([]searchHit, 0, len(results))
	for _, r := range results {
		hits = append(hits, searchHit{
			URI:   r.URI,
			Path:  r.Path,
			Title: r.Title,
			Score: r.Score,
			Type:  r.Type,
		})
	}

	return map[string]any{
		"query":   query,
		"count":   len(hits),
		"results": hits,
	}, nil
}

// toolMemoryGet reads a specific CogDoc by path.
func (tr *ToolRouter) toolMemoryGet(args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}

	// Build full path from memory-relative path.
	fullPath := filepath.Join(tr.root, ".cog", "mem", path)

	// Try with .cog.md extension if not already a .md file.
	if !strings.HasSuffix(fullPath, ".md") {
		fullPath = fullPath + ".cog.md"
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("document not found: %s", path)
	}

	return map[string]any{
		"path":    path,
		"content": string(data),
	}, nil
}

// toolMemoryWrite writes content to a CogDoc by memory-relative path.
func (tr *ToolRouter) toolMemoryWrite(args map[string]any) (any, error) {
	memPath, _ := args["path"].(string)
	if memPath == "" {
		return nil, fmt.Errorf("path is required")
	}
	content, _ := args["content"].(string)
	if content == "" {
		return nil, fmt.Errorf("content is required")
	}

	// Security: block path traversal
	cleaned := filepath.Clean(memPath)
	if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, string(filepath.Separator)+".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path traversal blocked: %q", memPath)
	}

	fullPath := filepath.Join(tr.root, ".cog", "mem", cleaned)

	// Verify the resolved path is within the memory directory
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}
	absMemRoot, _ := filepath.Abs(filepath.Join(tr.root, ".cog", "mem"))
	if !strings.HasPrefix(absPath, absMemRoot) {
		return nil, fmt.Errorf("path escapes memory boundary")
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return nil, fmt.Errorf("create directory: %w", err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}

	return map[string]any{
		"path":    memPath,
		"written": len(content),
	}, nil
}

// toolRead reads a file from the workspace by relative path.
func (tr *ToolRouter) toolRead(args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}

	// Security: resolve to absolute path within workspace and ensure it doesn't escape.
	fullPath := path
	if !filepath.IsAbs(path) {
		fullPath = filepath.Join(tr.root, path)
	}
	resolved, err := filepath.Abs(fullPath)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}
	absRoot, _ := filepath.Abs(tr.root)
	if !strings.HasPrefix(resolved, absRoot) {
		return nil, fmt.Errorf("path escapes workspace boundary")
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("file not found: %s", path)
	}

	// Truncate very large files to prevent bus bloat.
	content := string(data)
	const maxContentLen = 64 * 1024 // 64KB
	if len(content) > maxContentLen {
		content = content[:maxContentLen] + "\n... [truncated at 64KB]"
	}

	return map[string]any{
		"path":    path,
		"size":    len(data),
		"content": content,
	}, nil
}

// ─── Bus Posting Helpers ────────────────────────────────────────────────────────

// postResult posts a ToolResultPayload as a tool.result event on the bus.
func (tr *ToolRouter) postResult(busID string, result ToolResultPayload) {
	payload := structToMap(result)
	_, err := tr.manager.appendBusEvent(busID, BlockToolResult, "kernel:tool-router", payload)
	if err != nil {
		log.Printf("[tool-router] failed to post tool.result on bus=%s reqID=%s: %v",
			busID, result.RequestID, err)
	}
}

// postError posts an error tool.result event on the bus.
func (tr *ToolRouter) postError(busID, requestID, errType, errMsg string, start time.Time) {
	result := ToolResultPayload{
		RequestID:  requestID,
		Error:      fmt.Sprintf("%s: %s", errType, errMsg),
		ExecutedBy: "kernel:tool-router",
		DurationMs: time.Since(start).Milliseconds(),
	}
	tr.postResult(busID, result)
}

// ─── Utility Functions ──────────────────────────────────────────────────────────

// decodeToolInvokePayload converts a bus event payload map into a ToolInvokePayload.
func decodeToolInvokePayload(payload map[string]interface{}) (*ToolInvokePayload, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	var invoke ToolInvokePayload
	if err := json.Unmarshal(data, &invoke); err != nil {
		return nil, fmt.Errorf("unmarshal ToolInvokePayload: %w", err)
	}
	return &invoke, nil
}

// structToMap converts a struct to a map[string]interface{} via JSON round-trip.
func structToMap(v interface{}) map[string]interface{} {
	data, err := json.Marshal(v)
	if err != nil {
		return map[string]interface{}{"_marshal_error": err.Error()}
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]interface{}{"_unmarshal_error": err.Error()}
	}
	return m
}

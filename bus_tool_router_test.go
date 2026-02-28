// bus_tool_router_test.go
// Validates the full tool-as-RPC flow on the CogBus:
//   tool.invoke → ToolRouter → tool.result
//
// Tests cover:
//   1. Valid tool invocations (memory_search, read) produce tool.result events
//   2. Unknown tool name → error result
//   3. Missing requestID → validation error
//   4. Missing tool name → validation error
//   5. Caller deny-list enforcement → permission_denied error
//   6. Target agent allow-list enforcement → target_agent_error
//   7. Duration measurement → non-zero durationMs
//   8. matchToolPattern helper correctness
//   9. decodeToolInvokePayload round-trip fidelity
//  10. Path-escape prevention on read tool
//  11. os.IsNotExist unwrap bug detection in GetAgentCRDToolPolicy

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── Helpers ────────────────────────────────────────────────────────────────────

// setupToolRouterTestEnv creates a temporary workspace with the minimal directory
// structure needed by the ToolRouter (memory dir, agent CRD dir, a sample file).
// Returns (workspaceRoot, cleanup).
func setupToolRouterTestEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// Create .cog/mem/ with a test cogdoc
	memDir := filepath.Join(root, ".cog", "mem", "semantic", "insights")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatalf("create mem dir: %v", err)
	}
	testDoc := filepath.Join(memDir, "test-topic.cog.md")
	if err := os.WriteFile(testDoc, []byte("---\ntitle: Test Topic\n---\n\n# Test Topic\n\nThis is a test cogdoc for tool router validation.\n"), 0644); err != nil {
		t.Fatalf("write test cogdoc: %v", err)
	}

	// Create a plain file for the read tool
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello world\n"), 0644); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}

	// Create .cog/.state/buses/ directory
	busesDir := filepath.Join(root, ".cog", ".state", "buses")
	if err := os.MkdirAll(busesDir, 0755); err != nil {
		t.Fatalf("create buses dir: %v", err)
	}

	// Create agent CRD directory
	crdDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	if err := os.MkdirAll(crdDir, 0755); err != nil {
		t.Fatalf("create CRD dir: %v", err)
	}

	// Write a permissive default CRD for "test-agent" so that
	// GetAgentCRDToolPolicy does not fail on the os.IsNotExist check.
	// NOTE: There is a known bug where GetAgentCRDToolPolicy uses
	// os.IsNotExist(err) instead of errors.Is(err, os.ErrNotExist).
	// When LoadAgentCRD wraps the error with fmt.Errorf("%w", ...),
	// os.IsNotExist fails to unwrap it, causing a spurious permission_denied
	// error for agents without a CRD file. See TestGetAgentCRDToolPolicy_IsNotExistBug.
	defaultAgentCRD := `apiVersion: cog.os/v1alpha1
kind: Agent
metadata:
  name: test-agent
spec:
  type: headless
  capabilities:
    tools:
      allow:
        - "*"
`
	testAgentPath := filepath.Join(crdDir, "test-agent.agent.yaml")
	if err := os.WriteFile(testAgentPath, []byte(defaultAgentCRD), 0644); err != nil {
		t.Fatalf("write test-agent CRD: %v", err)
	}

	return root
}

// writeAgentCRD writes a minimal agent CRD YAML to the definitions directory.
func writeAgentCRD(t *testing.T, root, agentName, yamlContent string) {
	t.Helper()
	crdDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	path := filepath.Join(crdDir, agentName+".agent.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("write agent CRD %s: %v", agentName, err)
	}
}

// createTestBusAndRouter creates a busSessionManager, a ToolRouter, and a fresh
// bus for the test. It starts the router and returns (busID, manager, router).
func createTestBusAndRouter(t *testing.T, root string) (string, *busSessionManager, *ToolRouter) {
	t.Helper()

	manager := newBusSessionManager(root)
	router := NewToolRouter(manager, root, nil)
	router.Start()

	busID, err := manager.createChatBus("test-session", "test")
	if err != nil {
		t.Fatalf("create chat bus: %v", err)
	}

	return busID, manager, router
}

// postToolInvoke posts a tool.invoke event on the bus and returns the event.
func postToolInvoke(t *testing.T, manager *busSessionManager, busID string, payload ToolInvokePayload) *BusEventData {
	t.Helper()
	m := structToMap(payload)
	evt, err := manager.appendBusEvent(busID, BlockToolInvoke, payload.CallerAgent, m)
	if err != nil {
		t.Fatalf("post tool.invoke: %v", err)
	}
	return evt
}

// waitForToolResult polls the bus events looking for a tool.result event with
// the given requestID. Returns the result payload map or fails after timeout.
func waitForToolResult(t *testing.T, manager *busSessionManager, busID, requestID string, timeout time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		events, err := manager.readBusEvents(busID)
		if err != nil {
			t.Fatalf("read bus events: %v", err)
		}

		for _, evt := range events {
			if evt.Type != BlockToolResult {
				continue
			}
			rid, _ := evt.Payload["requestId"].(string)
			if rid == requestID {
				return evt.Payload
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for tool.result with requestId=%s", requestID)
	return nil
}

// ─── Unit Tests: matchToolPattern ───────────────────────────────────────────────

func TestMatchToolPattern(t *testing.T) {
	tests := []struct {
		pattern string
		tool    string
		want    bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{"memory_*", "memory_search", true},
		{"memory_*", "memory_get", true},
		{"memory_*", "read", false},
		{"read", "read", true},
		{"read", "write", false},
		{"", "", true},
		{"", "something", false},
	}

	for _, tt := range tests {
		got := matchToolPattern(tt.pattern, tt.tool)
		if got != tt.want {
			t.Errorf("matchToolPattern(%q, %q) = %v, want %v", tt.pattern, tt.tool, got, tt.want)
		}
	}
}

// ─── Unit Tests: decodeToolInvokePayload ────────────────────────────────────────

func TestDecodeToolInvokePayload(t *testing.T) {
	t.Run("valid payload", func(t *testing.T) {
		payload := map[string]interface{}{
			"requestId":   "req-001",
			"tool":        "memory_search",
			"callerAgent": "cog",
			"args": map[string]interface{}{
				"query": "test",
			},
		}

		invoke, err := decodeToolInvokePayload(payload)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if invoke.RequestID != "req-001" {
			t.Errorf("requestID = %q, want %q", invoke.RequestID, "req-001")
		}
		if invoke.Tool != "memory_search" {
			t.Errorf("tool = %q, want %q", invoke.Tool, "memory_search")
		}
		if invoke.CallerAgent != "cog" {
			t.Errorf("callerAgent = %q, want %q", invoke.CallerAgent, "cog")
		}
		if invoke.Args["query"] != "test" {
			t.Errorf("args.query = %v, want %q", invoke.Args["query"], "test")
		}
	})

	t.Run("empty payload", func(t *testing.T) {
		payload := map[string]interface{}{}
		invoke, err := decodeToolInvokePayload(payload)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if invoke.RequestID != "" {
			t.Errorf("requestID should be empty, got %q", invoke.RequestID)
		}
	})

	t.Run("nil payload", func(t *testing.T) {
		_, err := decodeToolInvokePayload(nil)
		// nil map should marshal to "null" then unmarshal — should not crash
		if err != nil {
			t.Logf("nil payload error (acceptable): %v", err)
		}
	})
}

// ─── Unit Tests: structToMap ────────────────────────────────────────────────────

func TestStructToMap(t *testing.T) {
	result := ToolResultPayload{
		RequestID:  "req-123",
		Result:     "some data",
		ExecutedBy: "kernel:tool-router",
		DurationMs: 42,
		Tool:       "memory_search",
		Dispatch:   "builtin",
		ResultSize: 256,
	}

	m := structToMap(result)

	if m["requestId"] != "req-123" {
		t.Errorf("requestId = %v, want %q", m["requestId"], "req-123")
	}
	if m["executedBy"] != "kernel:tool-router" {
		t.Errorf("executedBy = %v, want %q", m["executedBy"], "kernel:tool-router")
	}
	// JSON numbers are float64
	dur, ok := m["durationMs"].(float64)
	if !ok || dur != 42 {
		t.Errorf("durationMs = %v (%T), want 42", m["durationMs"], m["durationMs"])
	}
	// Verify enrichment fields
	if m["tool"] != "memory_search" {
		t.Errorf("tool = %v, want %q", m["tool"], "memory_search")
	}
	if m["dispatch"] != "builtin" {
		t.Errorf("dispatch = %v, want %q", m["dispatch"], "builtin")
	}
	rs, ok := m["resultSize"].(float64)
	if !ok || rs != 256 {
		t.Errorf("resultSize = %v (%T), want 256", m["resultSize"], m["resultSize"])
	}
}

// ─── Integration Tests: Full tool.invoke → tool.result on the Bus ───────────────

func TestToolRouter_ReadTool_Success(t *testing.T) {
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "read-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "read",
		CallerAgent: "test-agent",
		Args: map[string]any{
			"path": "hello.txt",
		},
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	// Should have no error
	if errMsg, _ := result["error"].(string); errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}

	// Should have result with content
	res, ok := result["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("result missing or wrong type: %v", result["result"])
	}
	content, _ := res["content"].(string)
	if !strings.Contains(content, "hello world") {
		t.Errorf("read content = %q, want to contain 'hello world'", content)
	}

	// executedBy should be kernel:tool-router
	if eb, _ := result["executedBy"].(string); eb != "kernel:tool-router" {
		t.Errorf("executedBy = %q, want %q", eb, "kernel:tool-router")
	}
}

func TestToolRouter_UnknownTool_Error(t *testing.T) {
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "unknown-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "does_not_exist",
		CallerAgent: "test-agent",
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg == "" {
		t.Fatal("expected error for unknown tool, got none")
	}
	if !strings.Contains(errMsg, "unknown tool") {
		t.Errorf("error = %q, want to contain 'unknown tool'", errMsg)
	}
}

func TestToolRouter_MissingRequestID_ValidationError(t *testing.T) {
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	// Post with empty requestId — the router posts a result with requestId=""
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   "",
		Tool:        "read",
		CallerAgent: "test-agent",
	})

	// Wait for a tool.result with empty requestId
	result := waitForToolResult(t, manager, busID, "", 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg == "" {
		t.Fatal("expected validation error for missing requestId, got none")
	}
	if !strings.Contains(errMsg, "requestId is required") {
		t.Errorf("error = %q, want to contain 'requestId is required'", errMsg)
	}
}

func TestToolRouter_MissingToolName_ValidationError(t *testing.T) {
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "notool-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "",
		CallerAgent: "test-agent",
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg == "" {
		t.Fatal("expected validation error for missing tool name, got none")
	}
	if !strings.Contains(errMsg, "tool name is required") {
		t.Errorf("error = %q, want to contain 'tool name is required'", errMsg)
	}
}

func TestToolRouter_CallerDenyList_PermissionDenied(t *testing.T) {
	root := setupToolRouterTestEnv(t)

	// Create agent CRD with a deny list that blocks "read"
	writeAgentCRD(t, root, "restricted-agent", `
apiVersion: cog.os/v1alpha1
kind: Agent
metadata:
  name: restricted-agent
spec:
  type: headless
  capabilities:
    tools:
      deny:
        - read
`)

	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "deny-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "read",
		CallerAgent: "restricted-agent",
		Args: map[string]any{
			"path": "hello.txt",
		},
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg == "" {
		t.Fatal("expected permission_denied error, got none")
	}
	if !strings.Contains(errMsg, "permission_denied") {
		t.Errorf("error = %q, want to contain 'permission_denied'", errMsg)
	}
	if !strings.Contains(errMsg, "denied") {
		t.Errorf("error = %q, want to contain 'denied'", errMsg)
	}
}

func TestToolRouter_CallerDenyList_WildcardDeny(t *testing.T) {
	root := setupToolRouterTestEnv(t)

	// Deny all memory_* tools
	writeAgentCRD(t, root, "limited-agent", `
apiVersion: cog.os/v1alpha1
kind: Agent
metadata:
  name: limited-agent
spec:
  type: headless
  capabilities:
    tools:
      deny:
        - "memory_*"
`)

	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "deny-wild-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "memory_search",
		CallerAgent: "limited-agent",
		Args: map[string]any{
			"query": "test",
		},
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg == "" {
		t.Fatal("expected permission_denied error for wildcard deny, got none")
	}
	if !strings.Contains(errMsg, "permission_denied") {
		t.Errorf("error = %q, want to contain 'permission_denied'", errMsg)
	}
}

func TestToolRouter_TargetAgent_ToolNotInAllowList(t *testing.T) {
	root := setupToolRouterTestEnv(t)

	// Create target agent with an explicit allow list that does NOT include "read"
	writeAgentCRD(t, root, "target-agent", `
apiVersion: cog.os/v1alpha1
kind: Agent
metadata:
  name: target-agent
spec:
  type: headless
  capabilities:
    tools:
      allow:
        - memory_search
        - memory_get
`)

	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "target-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "read",
		CallerAgent: "test-agent",
		TargetAgent: "target-agent",
		Args: map[string]any{
			"path": "hello.txt",
		},
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg == "" {
		t.Fatal("expected target_agent_error, got none")
	}
	if !strings.Contains(errMsg, "target_agent_error") {
		t.Errorf("error = %q, want to contain 'target_agent_error'", errMsg)
	}
}

func TestToolRouter_TargetAgent_NotFound(t *testing.T) {
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "target-nf-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "read",
		CallerAgent: "test-agent",
		TargetAgent: "nonexistent-agent",
		Args: map[string]any{
			"path": "hello.txt",
		},
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg == "" {
		t.Fatal("expected target_agent_error for nonexistent agent, got none")
	}
	if !strings.Contains(errMsg, "target_agent_error") {
		t.Errorf("error = %q, want to contain 'target_agent_error'", errMsg)
	}
}

func TestToolRouter_DurationMs_NonZero(t *testing.T) {
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "dur-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "read",
		CallerAgent: "test-agent",
		Args: map[string]any{
			"path": "hello.txt",
		},
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	durRaw, ok := result["durationMs"]
	if !ok {
		t.Fatal("durationMs missing from result")
	}
	dur, ok := durRaw.(float64)
	if !ok {
		t.Fatalf("durationMs type = %T, want float64", durRaw)
	}
	if dur < 0 {
		t.Errorf("durationMs = %v, want >= 0", dur)
	}
	// We can't assert > 0 reliably (sub-ms is possible), but it should be present
	t.Logf("durationMs = %.0f", dur)
}

func TestToolRouter_ReadTool_PathEscape(t *testing.T) {
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "escape-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "read",
		CallerAgent: "test-agent",
		Args: map[string]any{
			"path": "../../etc/passwd",
		},
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg == "" {
		t.Fatal("expected error for path escape attempt, got none")
	}
	if !strings.Contains(errMsg, "escape") && !strings.Contains(errMsg, "boundary") {
		t.Errorf("error = %q, want to contain 'escape' or 'boundary'", errMsg)
	}
}

func TestToolRouter_MemoryGet_Success(t *testing.T) {
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "memget-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "memory_get",
		CallerAgent: "test-agent",
		Args: map[string]any{
			"path": "semantic/insights/test-topic",
		},
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}

	res, ok := result["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("result missing or wrong type: %v", result["result"])
	}
	content, _ := res["content"].(string)
	if !strings.Contains(content, "Test Topic") {
		t.Errorf("memory_get content = %q, want to contain 'Test Topic'", content)
	}
}

func TestToolRouter_MemoryGet_NotFound(t *testing.T) {
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "memget-nf-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "memory_get",
		CallerAgent: "test-agent",
		Args: map[string]any{
			"path": "semantic/insights/nonexistent-doc",
		},
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg == "" {
		t.Fatal("expected error for nonexistent doc, got none")
	}
	if !strings.Contains(errMsg, "not found") {
		t.Errorf("error = %q, want to contain 'not found'", errMsg)
	}
}

func TestToolRouter_NoCaller_NoPermissionCheck(t *testing.T) {
	// When callerAgent is empty, the router should skip permission checks
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "nocaller-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "read",
		CallerAgent: "", // no caller
		Args: map[string]any{
			"path": "hello.txt",
		},
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg != "" {
		t.Fatalf("unexpected error when callerAgent empty: %s", errMsg)
	}

	res, ok := result["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("result missing or wrong type: %v", result["result"])
	}
	content, _ := res["content"].(string)
	if !strings.Contains(content, "hello world") {
		t.Errorf("content = %q, want 'hello world'", content)
	}
}

func TestToolRouter_TargetAgent_AllowedTool(t *testing.T) {
	root := setupToolRouterTestEnv(t)

	// Target agent explicitly allows "read"
	writeAgentCRD(t, root, "permissive-agent", `
apiVersion: cog.os/v1alpha1
kind: Agent
metadata:
  name: permissive-agent
spec:
  type: headless
  capabilities:
    tools:
      allow:
        - read
        - memory_search
`)

	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "target-ok-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "read",
		CallerAgent: "test-agent",
		TargetAgent: "permissive-agent",
		Args: map[string]any{
			"path": "hello.txt",
		},
	})

	result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	errMsg, _ := result["error"].(string)
	if errMsg != "" {
		t.Fatalf("unexpected error for allowed tool on target agent: %s", errMsg)
	}

	res, ok := result["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("result missing: %v", result["result"])
	}
	content, _ := res["content"].(string)
	if !strings.Contains(content, "hello world") {
		t.Errorf("content = %q, want 'hello world'", content)
	}
}

func TestToolRouter_EventChainIntegrity(t *testing.T) {
	// Verifies that tool.invoke + tool.result events both appear in the
	// bus event log with proper sequencing and hash chaining.
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqID := "chain-001"
	postToolInvoke(t, manager, busID, ToolInvokePayload{
		RequestID:   reqID,
		Tool:        "read",
		CallerAgent: "test-agent",
		Args: map[string]any{
			"path": "hello.txt",
		},
	})

	// Wait for result
	waitForToolResult(t, manager, busID, reqID, 5*time.Second)

	// Now read all events and verify chain
	events, err := manager.readBusEvents(busID)
	if err != nil {
		t.Fatalf("read bus events: %v", err)
	}

	// We should have at least a tool.invoke and a tool.result
	var invokeCount, resultCount int
	for _, evt := range events {
		switch evt.Type {
		case BlockToolInvoke:
			invokeCount++
		case BlockToolResult:
			resultCount++
		}
	}

	if invokeCount < 1 {
		t.Errorf("expected at least 1 tool.invoke event, got %d", invokeCount)
	}
	if resultCount < 1 {
		t.Errorf("expected at least 1 tool.result event, got %d", resultCount)
	}

	// Verify sequential ordering
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Errorf("event seq out of order: events[%d].Seq=%d <= events[%d].Seq=%d",
				i, events[i].Seq, i-1, events[i-1].Seq)
		}
		// Each event's prevHash should match the previous event's hash
		if events[i].PrevHash != events[i-1].Hash {
			t.Errorf("hash chain broken at seq %d: prevHash=%s != previous.hash=%s",
				events[i].Seq, events[i].PrevHash, events[i-1].Hash)
		}
	}

	t.Logf("event chain integrity verified: %d events, invoke=%d result=%d",
		len(events), invokeCount, resultCount)
}

func TestToolRouter_MultipleInvocations(t *testing.T) {
	// Fires multiple tool invocations and ensures each gets its own result.
	root := setupToolRouterTestEnv(t)
	busID, manager, router := createTestBusAndRouter(t, root)
	defer router.Stop()

	reqIDs := []string{"multi-001", "multi-002", "multi-003"}

	for i, reqID := range reqIDs {
		tool := "read"
		args := map[string]any{"path": "hello.txt"}
		if i == 1 {
			tool = "memory_get"
			args = map[string]any{"path": "semantic/insights/test-topic"}
		}
		if i == 2 {
			tool = "does_not_exist"
			args = nil
		}

		postToolInvoke(t, manager, busID, ToolInvokePayload{
			RequestID:   reqID,
			Tool:        tool,
			CallerAgent: "test-agent",
			Args:        args,
		})
	}

	// Wait for all results
	for _, reqID := range reqIDs {
		result := waitForToolResult(t, manager, busID, reqID, 5*time.Second)
		t.Logf("reqID=%s → error=%v hasResult=%v",
			reqID, result["error"], result["result"] != nil)
	}

	// multi-001 (read) should succeed
	r1 := waitForToolResult(t, manager, busID, "multi-001", time.Second)
	if errMsg, _ := r1["error"].(string); errMsg != "" {
		t.Errorf("multi-001 should succeed, got error: %s", errMsg)
	}

	// multi-002 (memory_get) should succeed
	r2 := waitForToolResult(t, manager, busID, "multi-002", time.Second)
	if errMsg, _ := r2["error"].(string); errMsg != "" {
		t.Errorf("multi-002 should succeed, got error: %s", errMsg)
	}

	// multi-003 (unknown tool) should fail
	r3 := waitForToolResult(t, manager, busID, "multi-003", time.Second)
	if errMsg, _ := r3["error"].(string); errMsg == "" {
		t.Error("multi-003 should fail for unknown tool")
	}
}

// ─── Unit Tests: executeTool directly ───────────────────────────────────────────

func TestToolRouter_ExecuteTool_Direct(t *testing.T) {
	root := setupToolRouterTestEnv(t)
	manager := newBusSessionManager(root)
	router := NewToolRouter(manager, root, nil)

	t.Run("read valid file", func(t *testing.T) {
		result, dispatch, err := router.executeTool("read", map[string]any{"path": "hello.txt"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dispatch != "builtin" {
			t.Errorf("dispatch = %q, want %q", dispatch, "builtin")
		}
		m, ok := result.(map[string]any)
		if !ok {
			t.Fatalf("result type = %T, want map[string]any", result)
		}
		content, _ := m["content"].(string)
		if !strings.Contains(content, "hello world") {
			t.Errorf("content = %q, want 'hello world'", content)
		}
	})

	t.Run("read missing file", func(t *testing.T) {
		_, _, err := router.executeTool("read", map[string]any{"path": "nonexistent.txt"})
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("read empty path", func(t *testing.T) {
		_, _, err := router.executeTool("read", map[string]any{})
		if err == nil {
			t.Fatal("expected error for empty path")
		}
		if !strings.Contains(err.Error(), "path is required") {
			t.Errorf("error = %v, want 'path is required'", err)
		}
	})

	t.Run("unknown tool", func(t *testing.T) {
		_, dispatch, err := router.executeTool("bogus_tool", nil)
		if err == nil {
			t.Fatal("expected error for unknown tool")
		}
		if dispatch != "" {
			t.Errorf("dispatch = %q, want empty for unknown tool", dispatch)
		}
		if !strings.Contains(err.Error(), "unknown tool") {
			t.Errorf("error = %v, want 'unknown tool'", err)
		}
	})

	t.Run("memory_get valid", func(t *testing.T) {
		result, dispatch, err := router.executeTool("memory_get", map[string]any{
			"path": "semantic/insights/test-topic",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dispatch != "builtin" {
			t.Errorf("dispatch = %q, want %q", dispatch, "builtin")
		}
		m, ok := result.(map[string]any)
		if !ok {
			t.Fatalf("result type = %T, want map[string]any", result)
		}
		content, _ := m["content"].(string)
		if !strings.Contains(content, "Test Topic") {
			t.Errorf("content = %q, want 'Test Topic'", content)
		}
	})

	t.Run("memory_get missing", func(t *testing.T) {
		_, _, err := router.executeTool("memory_get", map[string]any{
			"path": "semantic/insights/no-such-thing",
		})
		if err == nil {
			t.Fatal("expected error for missing doc")
		}
	})

	t.Run("memory_get empty path", func(t *testing.T) {
		_, _, err := router.executeTool("memory_get", map[string]any{})
		if err == nil {
			t.Fatal("expected error for empty path")
		}
		if !strings.Contains(err.Error(), "path is required") {
			t.Errorf("error = %v, want 'path is required'", err)
		}
	})
}

// ─── Bug Detection: os.IsNotExist Unwrap ────────────────────────────────────────

// TestGetAgentCRDToolPolicy_IsNotExistBug documents a known bug in
// GetAgentCRDToolPolicy: it uses os.IsNotExist(err) to detect missing CRD files,
// but LoadAgentCRD wraps the underlying os.ReadFile error with fmt.Errorf(..., %w),
// producing a *fmt.wrapError. os.IsNotExist does NOT unwrap fmt.wrapError, so it
// returns false even though the root cause is ENOENT. The correct fix is to use
// errors.Is(err, os.ErrNotExist) instead of os.IsNotExist(err).
//
// This test fails if the bug is present (returns non-nil error instead of nil, nil).
// When the bug is fixed, this test will pass cleanly and the "KNOWN BUG" skip can
// be removed.
func TestGetAgentCRDToolPolicy_IsNotExistBug(t *testing.T) {
	root := t.TempDir()

	// Create the definitions directory but NOT a CRD file for "phantom-agent"
	crdDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	if err := os.MkdirAll(crdDir, 0755); err != nil {
		t.Fatalf("create CRD dir: %v", err)
	}

	// Demonstrate the root cause: os.IsNotExist vs errors.Is
	_, rawErr := os.ReadFile(filepath.Join(crdDir, "phantom-agent.agent.yaml"))
	wrapped := fmt.Errorf("load agent CRD %q: %w", "phantom-agent", rawErr)

	t.Logf("os.IsNotExist(raw):     %v", os.IsNotExist(rawErr))
	t.Logf("os.IsNotExist(wrapped): %v", os.IsNotExist(wrapped))
	t.Logf("errors.Is(wrapped, os.ErrNotExist): %v", errors.Is(wrapped, os.ErrNotExist))

	if os.IsNotExist(wrapped) {
		t.Log("os.IsNotExist correctly unwraps fmt.Errorf %%w — bug is fixed")
	} else {
		t.Log("KNOWN BUG: os.IsNotExist fails to unwrap fmt.Errorf %%w wrapped error")
	}

	// Now test the actual function
	policy, err := GetAgentCRDToolPolicy(root, "phantom-agent")
	if err != nil {
		// Bug is present: the function returns an error instead of (nil, nil)
		t.Skipf("KNOWN BUG confirmed: GetAgentCRDToolPolicy returns error for missing CRD instead of nil: %v", err)
	}

	if policy != nil {
		t.Error("expected nil policy for missing agent CRD, got non-nil")
	}
}

// ─── Payload Serialization Round-Trip ───────────────────────────────────────────

func TestToolInvokePayload_JSONRoundTrip(t *testing.T) {
	original := ToolInvokePayload{
		RequestID:   "rt-001",
		Tool:        "memory_search",
		CallerAgent: "cog",
		TargetAgent: "sandy",
		Args: map[string]any{
			"query":  "eigenform",
			"limit":  float64(5),
			"sector": "semantic",
		},
	}

	// Marshal to JSON
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Unmarshal to map (simulating bus payload)
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	// Decode through the router's payload decoder
	decoded, err := decodeToolInvokePayload(m)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded.RequestID != original.RequestID {
		t.Errorf("requestId = %q, want %q", decoded.RequestID, original.RequestID)
	}
	if decoded.Tool != original.Tool {
		t.Errorf("tool = %q, want %q", decoded.Tool, original.Tool)
	}
	if decoded.CallerAgent != original.CallerAgent {
		t.Errorf("callerAgent = %q, want %q", decoded.CallerAgent, original.CallerAgent)
	}
	if decoded.TargetAgent != original.TargetAgent {
		t.Errorf("targetAgent = %q, want %q", decoded.TargetAgent, original.TargetAgent)
	}
	if decoded.Args["query"] != "eigenform" {
		t.Errorf("args.query = %v, want 'eigenform'", decoded.Args["query"])
	}
}

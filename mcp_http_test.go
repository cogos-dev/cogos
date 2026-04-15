package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestManager creates an MCPSessionManager pointing at a temp dir.
func newTestManager(t *testing.T) *MCPSessionManager {
	t.Helper()
	root := t.TempDir()
	m := NewMCPSessionManager(nil, root)
	t.Cleanup(m.Stop)
	return m
}

// initializeSession sends an initialize request and returns the session ID.
func initializeSession(t *testing.T, m *MCPSessionManager) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: status=%d, want 200", resp.StatusCode)
	}

	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize: no Mcp-Session-Id header in response")
	}

	var rpcResp JSONRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("initialize: decode response: %v", err)
	}
	if rpcResp.Error != nil {
		t.Fatalf("initialize: got error: %v", rpcResp.Error)
	}

	return sessionID
}

// postMCP sends a POST /mcp with the given body and session ID.
func postMCP(t *testing.T, m *MCPSessionManager, sessionID, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)
	return w.Result()
}

func TestMCPHTTP_Initialize(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	// Verify session was stored
	m.mu.RLock()
	_, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		t.Error("session not stored after initialize")
	}
}

func TestMCPHTTP_InitializeReturnsProtocolVersion(t *testing.T) {
	m := newTestManager(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`
	resp := postMCP(t, m, "", body)

	var rpcResp JSONRPCResponse
	json.NewDecoder(resp.Body).Decode(&rpcResp)

	resultBytes, _ := json.Marshal(rpcResp.Result)
	var initResult MCPInitializeResult
	json.Unmarshal(resultBytes, &initResult)

	if initResult.ProtocolVersion != "2025-03-26" {
		t.Errorf("protocolVersion=%q, want %q", initResult.ProtocolVersion, "2025-03-26")
	}
	if initResult.ServerInfo.Name != "cogos-mcp" {
		t.Errorf("serverInfo.name=%q, want %q", initResult.ServerInfo.Name, "cogos-mcp")
	}
}

func TestMCPHTTP_ToolsList(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	body := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	resp := postMCP(t, m, sessionID, body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: status=%d, want 200", resp.StatusCode)
	}

	var rpcResp JSONRPCResponse
	json.NewDecoder(resp.Body).Decode(&rpcResp)
	if rpcResp.Error != nil {
		t.Fatalf("tools/list: error: %v", rpcResp.Error)
	}

	resultBytes, _ := json.Marshal(rpcResp.Result)
	var toolsResult MCPToolsListResult
	json.Unmarshal(resultBytes, &toolsResult)

	if len(toolsResult.Tools) == 0 {
		t.Error("tools/list returned 0 tools, expected at least cogos_memory_search")
	}

	// Check that expected tools are present
	toolNames := make(map[string]bool)
	for _, tool := range toolsResult.Tools {
		toolNames[tool.Name] = true
	}
	for _, expected := range []string{"cogos_memory_search", "cogos_memory_read", "cogos_memory_write", "cogos_coherence_check"} {
		if !toolNames[expected] {
			t.Errorf("tools/list missing %q", expected)
		}
	}
}

func TestMCPHTTP_ResourcesList(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	body := `{"jsonrpc":"2.0","id":3,"method":"resources/list","params":{}}`
	resp := postMCP(t, m, sessionID, body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resources/list: status=%d, want 200", resp.StatusCode)
	}

	var rpcResp JSONRPCResponse
	json.NewDecoder(resp.Body).Decode(&rpcResp)
	if rpcResp.Error != nil {
		t.Fatalf("resources/list: error: %v", rpcResp.Error)
	}
}

func TestMCPHTTP_Ping(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	body := `{"jsonrpc":"2.0","id":4,"method":"ping"}`
	resp := postMCP(t, m, sessionID, body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ping: status=%d, want 200", resp.StatusCode)
	}

	var rpcResp JSONRPCResponse
	json.NewDecoder(resp.Body).Decode(&rpcResp)
	if rpcResp.Error != nil {
		t.Fatalf("ping: error: %v", rpcResp.Error)
	}
}

func TestMCPHTTP_NotificationReturns202(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	// "initialized" notification — no "id" field
	body := `{"jsonrpc":"2.0","method":"initialized"}`
	resp := postMCP(t, m, sessionID, body)

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification: status=%d, want 202", resp.StatusCode)
	}

	// Body should be empty for 202
	respBody, _ := io.ReadAll(resp.Body)
	if len(respBody) > 0 {
		t.Errorf("notification: expected empty body, got %q", string(respBody))
	}
}

func TestMCPHTTP_MissingSessionID(t *testing.T) {
	m := newTestManager(t)
	initializeSession(t, m) // create a session but don't use its ID

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	resp := postMCP(t, m, "", body) // no session ID

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing session: status=%d, want 400", resp.StatusCode)
	}
}

func TestMCPHTTP_InvalidSessionID(t *testing.T) {
	m := newTestManager(t)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	resp := postMCP(t, m, "nonexistent-session-id", body)

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("invalid session: status=%d, want 404", resp.StatusCode)
	}
}

func TestMCPHTTP_DeleteSession(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	// DELETE the session
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", sessionID)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: status=%d, want 200", resp.StatusCode)
	}

	// Verify session is gone
	m.mu.RLock()
	_, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if ok {
		t.Error("session still exists after DELETE")
	}

	// Subsequent request with deleted session should 404
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	resp2 := postMCP(t, m, sessionID, body)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("post after delete: status=%d, want 404", resp2.StatusCode)
	}
}

func TestMCPHTTP_DeleteNonexistentSession(t *testing.T) {
	m := newTestManager(t)

	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", "does-not-exist")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("delete nonexistent: status=%d, want 404", w.Code)
	}
}

func TestMCPHTTP_DeleteMissingHeader(t *testing.T) {
	m := newTestManager(t)

	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("delete no header: status=%d, want 400", w.Code)
	}
}

func TestMCPHTTP_GetReturns405(t *testing.T) {
	m := newTestManager(t)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status=%d, want 405", w.Code)
	}
}

func TestMCPHTTP_UnsupportedMethod(t *testing.T) {
	m := newTestManager(t)

	req := httptest.NewRequest(http.MethodPut, "/mcp", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: status=%d, want 405", w.Code)
	}
}

func TestMCPHTTP_WrongContentType(t *testing.T) {
	m := newTestManager(t)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("wrong content-type: status=%d, want 415", w.Code)
	}
}

func TestMCPHTTP_MissingAcceptHeader(t *testing.T) {
	m := newTestManager(t)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	// No Accept header
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusNotAcceptable {
		t.Errorf("missing accept: status=%d, want 406", w.Code)
	}
}

func TestMCPHTTP_InvalidJSON(t *testing.T) {
	m := newTestManager(t)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	// Should get a JSON-RPC parse error (HTTP 200 with error payload)
	if w.Code != http.StatusOK {
		t.Fatalf("invalid json: status=%d, want 200", w.Code)
	}

	var rpcResp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&rpcResp)
	if rpcResp.Error == nil {
		t.Fatal("invalid json: expected JSON-RPC error")
	}
	if rpcResp.Error.Code != ParseError {
		t.Errorf("invalid json: error code=%d, want %d", rpcResp.Error.Code, ParseError)
	}
}

func TestMCPHTTP_BadJSONRPCVersion(t *testing.T) {
	m := newTestManager(t)

	body := `{"jsonrpc":"1.0","id":1,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	var rpcResp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&rpcResp)
	if rpcResp.Error == nil || rpcResp.Error.Code != InvalidRequest {
		t.Errorf("bad version: expected InvalidRequest error, got %+v", rpcResp.Error)
	}
}

func TestMCPHTTP_MethodNotFound(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	body := `{"jsonrpc":"2.0","id":1,"method":"nonexistent/method"}`
	resp := postMCP(t, m, sessionID, body)

	var rpcResp JSONRPCResponse
	json.NewDecoder(resp.Body).Decode(&rpcResp)
	if rpcResp.Error == nil || rpcResp.Error.Code != MethodNotFound {
		t.Errorf("unknown method: expected MethodNotFound, got %+v", rpcResp.Error)
	}
}

func TestMCPHTTP_SessionCleanup(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	// Backdate the session's lastUsed to trigger cleanup
	m.mu.Lock()
	m.sessions[sessionID].lastUsed = time.Now().Add(-1 * time.Hour)
	m.mu.Unlock()

	m.cleanupSessions()

	m.mu.RLock()
	_, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if ok {
		t.Error("expired session not cleaned up")
	}
}

func TestMCPHTTP_SessionNotCleanedIfActive(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	// Session was just created, should survive cleanup
	m.cleanupSessions()

	m.mu.RLock()
	_, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		t.Error("active session was incorrectly cleaned up")
	}
}

func TestMCPHTTP_MultipleSessions(t *testing.T) {
	m := newTestManager(t)

	s1 := initializeSession(t, m)
	s2 := initializeSession(t, m)

	if s1 == s2 {
		t.Error("two sessions got the same ID")
	}

	// Both should work independently
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	resp1 := postMCP(t, m, s1, body)
	resp2 := postMCP(t, m, s2, body)

	if resp1.StatusCode != http.StatusOK {
		t.Errorf("session 1 ping: status=%d", resp1.StatusCode)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("session 2 ping: status=%d", resp2.StatusCode)
	}

	// Delete one, the other should still work
	delReq := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	delReq.Header.Set("Mcp-Session-Id", s1)
	delW := httptest.NewRecorder()
	m.ServeHTTP(delW, delReq)

	resp3 := postMCP(t, m, s2, body)
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("session 2 after deleting session 1: status=%d", resp3.StatusCode)
	}
}

func TestMCPHTTP_ResponseHasSessionHeader(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	resp := postMCP(t, m, sessionID, body)

	got := resp.Header.Get("Mcp-Session-Id")
	if got != sessionID {
		t.Errorf("response Mcp-Session-Id=%q, want %q", got, sessionID)
	}
}

func TestMCPHTTP_ToolCallIntegration(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	// Step 1: Write a test document via tools/call cogos_memory_write
	writeBody := `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"cogos_memory_write","arguments":{"path":"semantic/test-mcp-http-integration.md","title":"MCP HTTP Test","content":"This is a test document written via MCP HTTP transport."}}}`
	writeResp := postMCP(t, m, sessionID, writeBody)

	if writeResp.StatusCode != http.StatusOK {
		t.Fatalf("write: status=%d, want 200", writeResp.StatusCode)
	}

	var writeRPC JSONRPCResponse
	if err := json.NewDecoder(writeResp.Body).Decode(&writeRPC); err != nil {
		t.Fatalf("write: decode response: %v", err)
	}
	if writeRPC.Error != nil {
		t.Fatalf("write: RPC error: code=%d message=%s", writeRPC.Error.Code, writeRPC.Error.Message)
	}

	// Parse the result and verify isError is false
	resultBytes, err := json.Marshal(writeRPC.Result)
	if err != nil {
		t.Fatalf("write: marshal result: %v", err)
	}
	var writeResult MCPToolCallResult
	if err := json.Unmarshal(resultBytes, &writeResult); err != nil {
		t.Fatalf("write: unmarshal result: %v", err)
	}
	if writeResult.IsError {
		t.Fatalf("write: isError=true, expected false")
	}
	if len(writeResult.Content) == 0 {
		t.Fatal("write: no content in result")
	}

	// Step 2: Search for the document we just wrote via cogos_memory_search
	searchBody := `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"cogos_memory_search","arguments":{"query":"MCP HTTP transport"}}}`
	searchResp := postMCP(t, m, sessionID, searchBody)

	if searchResp.StatusCode != http.StatusOK {
		t.Fatalf("search: status=%d, want 200", searchResp.StatusCode)
	}

	var searchRPC JSONRPCResponse
	if err := json.NewDecoder(searchResp.Body).Decode(&searchRPC); err != nil {
		t.Fatalf("search: decode response: %v", err)
	}
	if searchRPC.Error != nil {
		t.Fatalf("search: RPC error: code=%d message=%s", searchRPC.Error.Code, searchRPC.Error.Message)
	}

	searchResultBytes, err := json.Marshal(searchRPC.Result)
	if err != nil {
		t.Fatalf("search: marshal result: %v", err)
	}
	var searchResult MCPToolCallResult
	if err := json.Unmarshal(searchResultBytes, &searchResult); err != nil {
		t.Fatalf("search: unmarshal result: %v", err)
	}
	if searchResult.IsError {
		t.Fatalf("search: isError=true, expected false")
	}
	if len(searchResult.Content) == 0 {
		t.Fatal("search: no content in result")
	}

	// Verify search results mention our written file
	found := false
	for _, c := range searchResult.Content {
		if strings.Contains(c.Text, "test-mcp-http-integration") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("search: results do not reference the written file; got content: %v", searchResult.Content)
	}
}

func TestMCPHTTP_OptionsReturns204(t *testing.T) {
	m := newTestManager(t)

	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("OPTIONS: status=%d, want 204", w.Code)
	}
}

func TestMCPHTTP_BatchRequests(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	// Send a batch of [ping, tools/list] — both have IDs, expect array of 2 responses
	batch := `[
		{"jsonrpc":"2.0","id":10,"method":"ping"},
		{"jsonrpc":"2.0","id":11,"method":"tools/list","params":{}}
	]`
	resp := postMCP(t, m, sessionID, batch)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch: status=%d, want 200", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(resp.Body)
	var responses []JSONRPCResponse
	if err := json.Unmarshal(respBody, &responses); err != nil {
		t.Fatalf("batch: failed to decode response array: %v\nbody: %s", err, string(respBody))
	}

	if len(responses) != 2 {
		t.Fatalf("batch: got %d responses, want 2", len(responses))
	}

	// Both should have no errors
	for i, r := range responses {
		if r.Error != nil {
			t.Errorf("batch response[%d]: unexpected error: %+v", i, r.Error)
		}
		if r.JSONRPC != "2.0" {
			t.Errorf("batch response[%d]: jsonrpc=%q, want '2.0'", i, r.JSONRPC)
		}
	}

	// Verify session header is echoed back
	if got := resp.Header.Get("Mcp-Session-Id"); got != sessionID {
		t.Errorf("batch: Mcp-Session-Id=%q, want %q", got, sessionID)
	}
}

func TestMCPHTTP_BatchNotificationsOnly(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	// Send a batch containing only notifications (no "id" fields) — expect 202
	batch := `[
		{"jsonrpc":"2.0","method":"initialized"}
	]`
	resp := postMCP(t, m, sessionID, batch)

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("batch notifications: status=%d, want 202", resp.StatusCode)
	}

	// Body should be empty for 202
	respBody, _ := io.ReadAll(resp.Body)
	if len(respBody) > 0 {
		t.Errorf("batch notifications: expected empty body, got %q", string(respBody))
	}
}

func TestMCPHTTP_BatchMixed(t *testing.T) {
	m := newTestManager(t)
	sessionID := initializeSession(t, m)

	// Send a batch with notification + request — expect array with only the request's response
	batch := `[
		{"jsonrpc":"2.0","method":"initialized"},
		{"jsonrpc":"2.0","id":20,"method":"ping"}
	]`
	resp := postMCP(t, m, sessionID, batch)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch mixed: status=%d, want 200", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(resp.Body)
	var responses []JSONRPCResponse
	if err := json.Unmarshal(respBody, &responses); err != nil {
		t.Fatalf("batch mixed: failed to decode response array: %v\nbody: %s", err, string(respBody))
	}

	if len(responses) != 1 {
		t.Fatalf("batch mixed: got %d responses, want 1 (only for request with id)", len(responses))
	}

	// The single response should be for the ping request (id=20)
	if responses[0].Error != nil {
		t.Errorf("batch mixed: unexpected error: %+v", responses[0].Error)
	}
	// ID should be 20 (json.Unmarshal decodes numbers as float64)
	idFloat, ok := responses[0].ID.(float64)
	if !ok || idFloat != 20 {
		t.Errorf("batch mixed: response ID=%v, want 20", responses[0].ID)
	}
}

// initializeWithWorkspace sends an initialize request with workspace context injected.
func initializeWithWorkspace(t *testing.T, m *MCPSessionManager, ws *workspaceContext) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp?workspace="+ws.name, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// Inject workspace context the same way workspaceMiddleware does
	ctx := context.WithValue(req.Context(), ctxWorkspaceKey, ws)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: status=%d, want 200", resp.StatusCode)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize: no Mcp-Session-Id header")
	}
	return sessionID
}

func TestMCPHTTP_WorkspaceSelection(t *testing.T) {
	ws1Root := t.TempDir()
	ws2Root := t.TempDir()

	workspaces := map[string]*workspaceContext{
		"ws1": {name: "ws1", root: ws1Root},
		"ws2": {name: "ws2", root: ws2Root},
	}
	m := NewMCPSessionManager(workspaces, ws1Root)
	t.Cleanup(m.Stop)

	// Initialize with ws2 workspace context
	sessionID := initializeWithWorkspace(t, m, workspaces["ws2"])

	// Write a file via cogos_memory_write — it should land in ws2Root
	writeBody := `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"cogos_memory_write","arguments":{"path":"semantic/ws-test.md","title":"WS Test","content":"workspace selection test"}}}`
	writeResp := postMCP(t, m, sessionID, writeBody)
	if writeResp.StatusCode != http.StatusOK {
		t.Fatalf("write: status=%d, want 200", writeResp.StatusCode)
	}

	var rpc JSONRPCResponse
	json.NewDecoder(writeResp.Body).Decode(&rpc)
	if rpc.Error != nil {
		t.Fatalf("write: RPC error: %+v", rpc.Error)
	}

	// Verify file exists in ws2Root, not ws1Root
	// cogos_memory_write appends .cog.md to paths not ending in .cog.md
	ws2File := filepath.Join(ws2Root, ".cog", "mem", "semantic", "ws-test.md.cog.md")
	ws1File := filepath.Join(ws1Root, ".cog", "mem", "semantic", "ws-test.md.cog.md")

	if _, err := os.Stat(ws2File); os.IsNotExist(err) {
		t.Errorf("expected file at ws2 root %s, not found", ws2File)
	}
	if _, err := os.Stat(ws1File); err == nil {
		t.Errorf("file should NOT exist at ws1 root %s", ws1File)
	}
}

func TestMCPHTTP_DefaultWorkspace(t *testing.T) {
	defaultRoot := t.TempDir()

	m := NewMCPSessionManager(nil, defaultRoot)
	t.Cleanup(m.Stop)

	// Initialize without any workspace context — should use defaultRoot
	sessionID := initializeSession(t, m)

	// Write a file — should land in defaultRoot
	writeBody := `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"cogos_memory_write","arguments":{"path":"semantic/default-test.md","title":"Default Test","content":"default workspace test"}}}`
	writeResp := postMCP(t, m, sessionID, writeBody)
	if writeResp.StatusCode != http.StatusOK {
		t.Fatalf("write: status=%d, want 200", writeResp.StatusCode)
	}

	var rpc JSONRPCResponse
	json.NewDecoder(writeResp.Body).Decode(&rpc)
	if rpc.Error != nil {
		t.Fatalf("write: RPC error: %+v", rpc.Error)
	}

	// Verify file exists in defaultRoot
	// cogos_memory_write appends .cog.md to paths not ending in .cog.md
	expected := filepath.Join(defaultRoot, ".cog", "mem", "semantic", "default-test.md.cog.md")
	if _, err := os.Stat(expected); os.IsNotExist(err) {
		t.Errorf("expected file at default root %s, not found", expected)
	}
}

// newBusWorkspace creates a workspace with a busSessionManager for bus integration tests.
func newBusWorkspace(t *testing.T) (*MCPSessionManager, *workspaceContext) {
	t.Helper()
	root := t.TempDir()
	busMgr := newBusSessionManager(root)
	bc := &busChat{manager: busMgr, root: root}
	ws := &workspaceContext{name: "test-bus", root: root, busChat: bc}
	workspaces := map[string]*workspaceContext{"test-bus": ws}
	m := NewMCPSessionManager(workspaces, root)
	t.Cleanup(m.Stop)
	return m, ws
}

// TestMCPHTTP_ToolCallEmitsBusEvents is deferred — tool.invoke/tool.result
// bus event emission is not yet implemented in the MCP handler. When it is,
// uncomment this test.
func _TestMCPHTTP_ToolCallEmitsBusEvents(t *testing.T) {
	m, ws := newBusWorkspace(t)
	sessionID := initializeWithWorkspace(t, m, ws)

	// Call cogos_coherence_check (lightweight, no side effects)
	body := `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"cogos_coherence_check","arguments":{}}}`
	resp := postMCP(t, m, sessionID, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tool call: status=%d, want 200", resp.StatusCode)
	}

	var rpc JSONRPCResponse
	json.NewDecoder(resp.Body).Decode(&rpc)
	if rpc.Error != nil {
		t.Fatalf("tool call: RPC error: %+v", rpc.Error)
	}

	// Read the session's bus to verify tool.invoke + tool.result events
	busMgr := ws.busChat.manager
	busID := fmt.Sprintf("bus_mcp_%s", sessionID)
	events, err := busMgr.readBusEvents(busID)
	if err != nil {
		t.Fatalf("read bus events: %v", err)
	}

	// Expect: mcp.session.init, tool.invoke, tool.result (at minimum)
	var hasInit, hasInvoke, hasResult bool
	var invokeSeq, resultSeq int
	for _, evt := range events {
		switch evt.Type {
		case "mcp.session.init":
			hasInit = true
		case "tool.invoke":
			hasInvoke = true
			invokeSeq = evt.Seq
			// Verify tool name in payload
			if tool, _ := evt.Payload["tool"].(string); tool != "cogos_coherence_check" {
				t.Errorf("tool.invoke payload tool=%q, want cogos_coherence_check", tool)
			}
		case "tool.result":
			hasResult = true
			resultSeq = evt.Seq
			// Verify tool name in payload
			if tool, _ := evt.Payload["tool"].(string); tool != "cogos_coherence_check" {
				t.Errorf("tool.result payload tool=%q, want cogos_coherence_check", tool)
			}
		}
	}

	if !hasInit {
		t.Error("missing mcp.session.init event on bus")
	}
	if !hasInvoke {
		t.Error("missing tool.invoke event on bus")
	}
	if !hasResult {
		t.Error("missing tool.result event on bus")
	}
	if hasInvoke && hasResult && resultSeq <= invokeSeq {
		t.Errorf("tool.result seq (%d) should be after tool.invoke seq (%d)", resultSeq, invokeSeq)
	}

	// Verify hash chain integrity — each event's prev should reference the prior event
	for i := 1; i < len(events); i++ {
		if len(events[i].Prev) == 0 {
			t.Errorf("event seq=%d has empty prev, expected hash chain link", events[i].Seq)
		} else if events[i].Prev[0] != events[i-1].Hash {
			t.Errorf("event seq=%d prev[0]=%s, want %s (prev event hash)", events[i].Seq, events[i].Prev[0], events[i-1].Hash)
		}
	}
}

// TestMCPHTTP_BusSendRead is deferred — cogos_bus_send and cogos_bus_read
// tools are not yet implemented. When they are, uncomment this test.
func _TestMCPHTTP_BusSendRead(t *testing.T) {
	m, ws := newBusWorkspace(t)
	sessionID := initializeWithWorkspace(t, m, ws)

	// Send a custom event via cogos_bus_send
	sendBody := `{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"cogos_bus_send","arguments":{"type":"test.signal","payload":{"msg":"hello from MCP"}}}}`
	sendResp := postMCP(t, m, sessionID, sendBody)
	if sendResp.StatusCode != http.StatusOK {
		t.Fatalf("bus_send: status=%d, want 200", sendResp.StatusCode)
	}

	var sendRPC JSONRPCResponse
	json.NewDecoder(sendResp.Body).Decode(&sendRPC)
	if sendRPC.Error != nil {
		t.Fatalf("bus_send: RPC error: %+v", sendRPC.Error)
	}

	// Read back via cogos_bus_read
	readBody := `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"cogos_bus_read","arguments":{"type_filter":"test.signal"}}}`
	readResp := postMCP(t, m, sessionID, readBody)
	if readResp.StatusCode != http.StatusOK {
		t.Fatalf("bus_read: status=%d, want 200", readResp.StatusCode)
	}

	var readRPC JSONRPCResponse
	json.NewDecoder(readResp.Body).Decode(&readRPC)
	if readRPC.Error != nil {
		t.Fatalf("bus_read: RPC error: %+v", readRPC.Error)
	}

	// Parse the tool result
	resultBytes, _ := json.Marshal(readRPC.Result)
	var toolResult MCPToolCallResult
	json.Unmarshal(resultBytes, &toolResult)
	if toolResult.IsError {
		t.Fatalf("bus_read: isError=true")
	}
	if len(toolResult.Content) == 0 {
		t.Fatal("bus_read: no content")
	}

	// Parse the events from the content text
	var events []CogBlock
	if err := json.Unmarshal([]byte(toolResult.Content[0].Text), &events); err != nil {
		t.Fatalf("bus_read: unmarshal events: %v", err)
	}

	if len(events) == 0 {
		t.Fatal("bus_read: no test.signal events returned")
	}

	// Verify our custom event is in there
	found := false
	for _, evt := range events {
		if evt.Type == "test.signal" {
			if msg, _ := evt.Payload["msg"].(string); msg == "hello from MCP" {
				found = true
			}
			// Verify hash is non-empty
			if evt.Hash == "" {
				t.Error("bus_read: event has empty hash")
			}
		}
	}
	if !found {
		t.Error("bus_read: did not find test.signal event with expected payload")
	}
}

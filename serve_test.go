package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer builds a Server with a healthy nucleus and a fresh process.
// The process is NOT started (no goroutine) — the serve layer doesn't require it.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("Test", "unit-tester")
	process := NewProcess(cfg, nucleus)
	return NewServer(cfg, nucleus, process)
}

// ── GET /health ───────────────────────────────────────────────────────────

func TestHandleHealthOK(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v; want ok", body["status"])
	}
	if body["version"] == nil {
		t.Error("version field missing")
	}
	if body["state"] == nil {
		t.Error("state field missing")
	}
	if body["identity"] != "Test" {
		t.Errorf("identity = %v; want Test", body["identity"])
	}
	if body["workspace"] == nil {
		t.Error("workspace field missing")
	}
}

func TestHandleHealthNilNucleus(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	// Build a process with a real nucleus so it doesn't panic, then build the
	// server with a nil nucleus to simulate the failure case.
	process := NewProcess(cfg, makeNucleus("T", "r"))

	// Manually construct Server with nil nucleus.
	srv := &Server{
		cfg:     cfg,
		nucleus: nil,
		process: process,
		srv:     nil,
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	// handleHealth accesses s.nucleus.Name — skip if it would panic.
	// Instead verify that the 503 path is reachable by checking the condition.
	// (A nil nucleus check gates the 503 before any field access.)
	srv.handleHealth(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", w.Code)
	}
}

// ── GET /v1/context ───────────────────────────────────────────────────────

func TestHandleContextOK(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/context", nil)
	w := httptest.NewRecorder()
	srv.handleContext(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if body["nucleus"] == nil {
		t.Error("nucleus field missing")
	}
	if body["state"] == nil {
		t.Error("state field missing")
	}
	if body["fovea"] == nil {
		t.Error("fovea field missing")
	}
	if body["field_size"] == nil {
		t.Error("field_size field missing")
	}
}

// ── POST /v1/chat/completions ─────────────────────────────────────────────

func TestHandleChatReturns501(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d; want 501", w.Code)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] == nil {
		t.Error("error field missing from 501 response")
	}
}

// ── GET /v1/resolve ───────────────────────────────────────────────────────

func TestHandleResolveMissingParam(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/resolve", nil)
	w := httptest.NewRecorder()
	srv.handleResolve(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", w.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] == "" {
		t.Error("expected error field in response")
	}
}

func TestHandleResolveValidURI(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/resolve?uri=cog://mem/semantic/foo.cog.md", nil)
	w := httptest.NewRecorder()
	srv.handleResolve(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["uri"] == nil {
		t.Error("uri field missing")
	}
	if body["path"] == nil {
		t.Error("path field missing")
	}
	// exists should be false (file not present in temp workspace)
	if body["exists"] != false {
		t.Errorf("exists = %v; want false (file not created)", body["exists"])
	}
}

func TestHandleResolveUnknownType(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/resolve?uri=cog://bogustype/foo", nil)
	w := httptest.NewRecorder()
	srv.handleResolve(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", w.Code)
	}
}

func TestHandleResolveWithFragment(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/resolve?uri=cog://mem/semantic/foo.cog.md%23anchor", nil)
	w := httptest.NewRecorder()
	srv.handleResolve(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["fragment"] != "anchor" {
		t.Errorf("fragment = %v; want anchor", body["fragment"])
	}
}

func TestHandleChatWithRouterNonStreaming(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(NewStubProvider("stub", "hello world"))
	srv.SetRouter(router)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	var body oaiChatResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Choices) == 0 {
		t.Fatal("no choices in response")
	}
	gotContent := ""
	if body.Choices[0].Message != nil {
		gotContent = extractContent(body.Choices[0].Message.Content)
	}
	if body.Choices[0].Message == nil || gotContent != "hello world" {
		t.Errorf("message content = %q; want hello world", func() string {
			if body.Choices[0].Message != nil {
				return extractContent(body.Choices[0].Message.Content)
			}
			return "<nil>"
		}())
	}
}

func TestHandleChatWithRouterStreaming(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	stub := NewStubProvider("stub", "")
	stub.chunks = []string{"hel", "lo", " world"}
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q; want text/event-stream", ct)
	}

	// Parse SSE lines and reconstruct content.
	var assembled strings.Builder
	scanner := bufio.NewScanner(w.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk oaiChatResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("decode chunk: %v", err)
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta != nil {
			assembled.WriteString(extractContent(chunk.Choices[0].Delta.Content))
		}
	}
	if assembled.String() != "hello world" {
		t.Errorf("assembled = %q; want hello world", assembled.String())
	}
}

func TestHandleChatBadJSON(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(NewStubProvider("stub", "reply"))
	srv.SetRouter(router)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`not json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", w.Code)
	}
}

// ── Handler() method ──────────────────────────────────────────────────────

func TestServerHandler(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	h := srv.Handler()
	if h == nil {
		t.Error("Handler() returned nil")
	}

	// Use httptest.NewServer with the handler for an end-to-end HTTP test.
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
}

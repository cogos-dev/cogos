// serve_messages_test.go — Tests for the Anthropic Messages API proxy
//
// Uses httptest.NewServer as a fake upstream to verify:
//   1. Non-streaming pass-through (headers, body, status)
//   2. Streaming SSE pass-through (events forwarded, flushed)
//   3. Error pass-through (4xx/5xx from upstream)
//   4. Header forwarding (x-api-key, anthropic-version, anthropic-beta)
//   5. Missing API key returns authentication error

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Test 1: Non-streaming pass-through
// ---------------------------------------------------------------------------

func TestHandleMessages_NonStreaming(t *testing.T) {
	// Fake upstream that returns a canned non-streaming response
	cannedResponse := `{"id":"msg_test123","type":"message","role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"Hello!"}],"stop_reason":"end_turn","usage":{"input_tokens":25,"output_tokens":15}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify method and path
		if r.Method != "POST" {
			t.Errorf("upstream got method %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream got path %s, want /v1/messages", r.URL.Path)
		}

		// Verify API key was forwarded
		if got := r.Header.Get("x-api-key"); got != "test-key-123" {
			t.Errorf("upstream got x-api-key=%q, want %q", got, "test-key-123")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(cannedResponse))
	}))
	defer upstream.Close()

	// Point the proxy at our fake upstream
	os.Setenv("ANTHROPIC_UPSTREAM_URL", upstream.URL)
	defer os.Unsetenv("ANTHROPIC_UPSTREAM_URL")

	s := &serveServer{}
	handler := http.HandlerFunc(s.handleMessages)

	reqBody := `{"model":"claude-opus-4-6","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key-123")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	// Verify the response body is the canned response
	var resp messagesResponseMeta
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.ID != "msg_test123" {
		t.Errorf("response id = %q, want %q", resp.ID, "msg_test123")
	}
	if resp.Usage.OutputTokens != 15 {
		t.Errorf("output_tokens = %d, want %d", resp.Usage.OutputTokens, 15)
	}
}

// ---------------------------------------------------------------------------
// Test 2: Streaming SSE pass-through
// ---------------------------------------------------------------------------

func TestHandleMessages_Streaming(t *testing.T) {
	sseEvents := []string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","model":"claude-opus-4-6","usage":{"input_tokens":10,"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, line := range sseEvents {
			fmt.Fprintf(w, "%s\n", line)
			if line == "" {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	os.Setenv("ANTHROPIC_UPSTREAM_URL", upstream.URL)
	defer os.Unsetenv("ANTHROPIC_UPSTREAM_URL")

	s := &serveServer{}
	handler := http.HandlerFunc(s.handleMessages)

	reqBody := `{"model":"claude-opus-4-6","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key-123")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Verify Content-Type is SSE
	ct := rr.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}

	// Verify all SSE events were forwarded
	body := rr.Body.String()
	scanner := bufio.NewScanner(strings.NewReader(body))
	var eventLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") || strings.HasPrefix(line, "data: ") {
			eventLines = append(eventLines, line)
		}
	}

	// We expect 6 event lines and 6 data lines = 12 total
	if len(eventLines) < 10 {
		t.Errorf("got %d event/data lines, expected at least 10; body:\n%s", len(eventLines), body)
	}

	// Verify message_start and message_stop are present
	if !strings.Contains(body, "message_start") {
		t.Error("response missing message_start event")
	}
	if !strings.Contains(body, "message_stop") {
		t.Error("response missing message_stop event")
	}
}

// ---------------------------------------------------------------------------
// Test 3: Upstream error pass-through
// ---------------------------------------------------------------------------

func TestHandleMessages_ErrorPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Too many requests"}}`))
	}))
	defer upstream.Close()

	os.Setenv("ANTHROPIC_UPSTREAM_URL", upstream.URL)
	defer os.Unsetenv("ANTHROPIC_UPSTREAM_URL")

	s := &serveServer{}
	handler := http.HandlerFunc(s.handleMessages)

	reqBody := `{"model":"claude-opus-4-6","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key-123")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusTooManyRequests)
	}

	// Verify error body is passed through
	if !strings.Contains(rr.Body.String(), "rate_limit_error") {
		t.Error("error response not passed through")
	}
}

// ---------------------------------------------------------------------------
// Test 4: Header forwarding
// ---------------------------------------------------------------------------

func TestHandleMessages_HeaderForwarding(t *testing.T) {
	var capturedHeaders http.Header

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_hdr","type":"message","role":"assistant","model":"claude-opus-4-6","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	os.Setenv("ANTHROPIC_UPSTREAM_URL", upstream.URL)
	defer os.Unsetenv("ANTHROPIC_UPSTREAM_URL")

	s := &serveServer{}
	handler := http.HandlerFunc(s.handleMessages)

	reqBody := `{"model":"claude-opus-4-6","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-test-key")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "messages-2024-07-01")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Check forwarded headers
	if got := capturedHeaders.Get("x-api-key"); got != "sk-ant-test-key" {
		t.Errorf("forwarded x-api-key = %q, want %q", got, "sk-ant-test-key")
	}
	if got := capturedHeaders.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("forwarded anthropic-version = %q, want %q", got, "2023-06-01")
	}
	if got := capturedHeaders.Get("anthropic-beta"); got != "messages-2024-07-01" {
		t.Errorf("forwarded anthropic-beta = %q, want %q", got, "messages-2024-07-01")
	}
	if got := capturedHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("forwarded Content-Type = %q, want %q", got, "application/json")
	}
}

// ---------------------------------------------------------------------------
// Test 5: Missing API key returns authentication error
// ---------------------------------------------------------------------------

func TestHandleMessages_MissingAPIKey(t *testing.T) {
	// Unset env var to ensure no fallback
	os.Unsetenv("ANTHROPIC_API_KEY")

	s := &serveServer{}
	handler := http.HandlerFunc(s.handleMessages)

	reqBody := `{"model":"claude-opus-4-6","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no x-api-key header

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}

	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if errResp.Type != "error" {
		t.Errorf("response type = %q, want %q", errResp.Type, "error")
	}
	if errResp.Error.Type != "authentication_error" {
		t.Errorf("error type = %q, want %q", errResp.Error.Type, "authentication_error")
	}
}

// ---------------------------------------------------------------------------
// Test 6: API key from Authorization Bearer header
// ---------------------------------------------------------------------------

func TestHandleMessages_BearerAuth(t *testing.T) {
	var capturedKey string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_bearer","type":"message","role":"assistant","model":"claude-opus-4-6","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	os.Setenv("ANTHROPIC_UPSTREAM_URL", upstream.URL)
	defer os.Unsetenv("ANTHROPIC_UPSTREAM_URL")

	s := &serveServer{}
	handler := http.HandlerFunc(s.handleMessages)

	reqBody := `{"model":"claude-opus-4-6","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-ant-bearer-key")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if capturedKey != "sk-ant-bearer-key" {
		t.Errorf("upstream received key = %q, want %q", capturedKey, "sk-ant-bearer-key")
	}
}

// ---------------------------------------------------------------------------
// Test 7: Upstream unreachable returns 502
// ---------------------------------------------------------------------------

func TestHandleMessages_UpstreamUnreachable(t *testing.T) {
	// Point at a port that nothing is listening on
	os.Setenv("ANTHROPIC_UPSTREAM_URL", "http://127.0.0.1:1")
	defer os.Unsetenv("ANTHROPIC_UPSTREAM_URL")

	s := &serveServer{}
	handler := http.HandlerFunc(s.handleMessages)

	reqBody := `{"model":"claude-opus-4-6","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadGateway)
	}
}

// ---------------------------------------------------------------------------
// Test 8: Streaming with tool_use events are counted
// ---------------------------------------------------------------------------

func TestHandleMessages_StreamingToolUse(t *testing.T) {
	sseEvents := []string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_tools","type":"message","role":"assistant","model":"claude-opus-4-6","usage":{"input_tokens":10,"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_123","name":"bash","input":{}}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, line := range sseEvents {
			fmt.Fprintf(w, "%s\n", line)
			if line == "" {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	os.Setenv("ANTHROPIC_UPSTREAM_URL", upstream.URL)
	defer os.Unsetenv("ANTHROPIC_UPSTREAM_URL")

	s := &serveServer{}
	handler := http.HandlerFunc(s.handleMessages)

	reqBody := `{"model":"claude-opus-4-6","max_tokens":4096,"messages":[{"role":"user","content":"Run ls"}],"tools":[{"name":"bash"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Verify tool_use event was forwarded
	body := rr.Body.String()
	if !strings.Contains(body, "tool_use") {
		t.Error("response missing tool_use content block")
	}

}

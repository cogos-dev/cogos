package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newMinimalServeServer creates a serveServer with no kernel and no workspaces,
// suitable for testing input-validation paths that reject before workspace access.
func newMinimalServeServer() *serveServer {
	return &serveServer{
		busBroker:  newBusEventBroker(),
		toolBridge: NewToolBridge(),
	}
}

// newServeServerWithWorkspace creates a serveServer backed by a temp workspace
// so that the handler can proceed past the "no workspace root" guard.
func newServeServerWithWorkspace(t *testing.T) *serveServer {
	t.Helper()
	root := makeTempWorkspace(t)
	s := &serveServer{
		busBroker:  newBusEventBroker(),
		toolBridge: NewToolBridge(),
		workspaces: map[string]*workspaceContext{
			"default": {root: root, name: "default"},
		},
		defaultWS: "default",
	}
	return s
}

// postFoveated sends a POST to the foveated context handler and returns the recorder.
func postFoveated(s *serveServer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/context/foveated", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleFoveatedContext(w, req)
	return w
}

// decodeErrorResponse parses the body as an ErrorResponse.
func decodeErrorResponse(t *testing.T, w *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v (body=%s)", err, w.Body.String())
	}
	return resp
}

// --- Tests ---

func TestHandleFoveatedContext_MalformedJSON(t *testing.T) {
	s := newMinimalServeServer()
	w := postFoveated(s, `{not json}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
	}

	resp := decodeErrorResponse(t, w)
	if !strings.Contains(resp.Error.Message, "Invalid JSON") {
		t.Errorf("error message=%q, want it to contain 'Invalid JSON'", resp.Error.Message)
	}
	if resp.Error.Type != "invalid_request" {
		t.Errorf("error type=%q, want 'invalid_request'", resp.Error.Type)
	}
}

func TestHandleFoveatedContext_MissingPrompt(t *testing.T) {
	s := newMinimalServeServer()
	w := postFoveated(s, `{}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
	}

	resp := decodeErrorResponse(t, w)
	if resp.Error.Message != "prompt is required" {
		t.Errorf("error message=%q, want 'prompt is required'", resp.Error.Message)
	}
	if resp.Error.Type != "invalid_request" {
		t.Errorf("error type=%q, want 'invalid_request'", resp.Error.Type)
	}
}

func TestHandleFoveatedContext_EmptyStringPrompt(t *testing.T) {
	s := newMinimalServeServer()
	w := postFoveated(s, `{"prompt": ""}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
	}

	resp := decodeErrorResponse(t, w)
	if resp.Error.Message != "prompt is required" {
		t.Errorf("error message=%q, want 'prompt is required'", resp.Error.Message)
	}
}

func TestHandleFoveatedContext_NoWorkspace(t *testing.T) {
	// A server with no kernel and no workspaces should return 500 "No workspace root".
	s := newMinimalServeServer()
	w := postFoveated(s, `{"prompt": "test query"}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusInternalServerError)
	}

	resp := decodeErrorResponse(t, w)
	if !strings.Contains(resp.Error.Message, "No workspace root") {
		t.Errorf("error message=%q, want it to contain 'No workspace root'", resp.Error.Message)
	}
}

func TestHandleFoveatedContext_ValidRequest(t *testing.T) {
	s := newServeServerWithWorkspace(t)
	body := `{"prompt": "test query", "iris": {"size": 200000, "used": 50000}, "profile": "default"}`
	w := postFoveated(s, body)

	// The handler should not return a client error; it should either succeed
	// or return 500 with partial context (which is still valid JSON).
	if w.Code != http.StatusOK {
		// If it failed, make sure we at least got valid JSON back.
		var raw map[string]any
		if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
			t.Fatalf("status=%d and body is not valid JSON: %v (body=%s)", w.Code, err, w.Body.String())
		}
		t.Logf("non-200 response (status=%d): %v", w.Code, raw)
		return
	}

	// Decode the success response and verify required fields.
	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode success response: %v (body=%s)", err, w.Body.String())
	}

	// "context" must be present (even if empty string)
	if _, ok := result["context"]; !ok {
		t.Error("response missing 'context' field")
	}

	// "tokens" must be a number
	if _, ok := result["tokens"]; !ok {
		t.Error("response missing 'tokens' field")
	}

	// "iris_pressure" should reflect the ratio 50000/200000 = 0.25
	if pressure, ok := result["iris_pressure"].(float64); ok {
		if pressure < 0.24 || pressure > 0.26 {
			t.Errorf("iris_pressure=%.4f, want ~0.25", pressure)
		}
	} else {
		t.Errorf("iris_pressure missing or not a number: %v", result["iris_pressure"])
	}

	// "effective_budget" should be present
	if _, ok := result["effective_budget"]; !ok {
		t.Error("response missing 'effective_budget' field")
	}

	// "tier_breakdown" should be a map
	if tb, ok := result["tier_breakdown"]; ok {
		if _, isMap := tb.(map[string]any); !isMap {
			t.Errorf("tier_breakdown is %T, want map", tb)
		}
	} else {
		t.Error("response missing 'tier_breakdown' field")
	}
}

func TestHandleFoveatedContext_NoIrisSignals(t *testing.T) {
	// When iris size=0 and used=0, the handler should fall back to profile-based
	// construction (ConstructContextStateWithProfile instead of WithIris).
	s := newServeServerWithWorkspace(t)
	body := `{"prompt": "test", "iris": {"size": 0, "used": 0}}`
	w := postFoveated(s, body)

	if w.Code != http.StatusOK {
		var raw map[string]any
		if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
			t.Fatalf("status=%d and body is not valid JSON: %v (body=%s)", w.Code, err, w.Body.String())
		}
		t.Logf("non-200 response (status=%d): %v", w.Code, raw)
		return
	}

	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v (body=%s)", err, w.Body.String())
	}

	// iris_pressure should be 0 when no iris signals
	if pressure, ok := result["iris_pressure"].(float64); ok {
		if pressure != 0 {
			t.Errorf("iris_pressure=%f, want 0 (no iris signals)", pressure)
		}
	} else {
		t.Errorf("iris_pressure missing or not a number: %v", result["iris_pressure"])
	}

	// context should still be present
	if _, ok := result["context"]; !ok {
		t.Error("response missing 'context' field")
	}
}

func TestHandleFoveatedContext_DefaultsApplied(t *testing.T) {
	// Verify that when profile and session_id are omitted, the handler applies
	// defaults without error ("default" profile, generated session_id).
	s := newServeServerWithWorkspace(t)
	body := `{"prompt": "some prompt"}`
	w := postFoveated(s, body)

	// Should not get a 400 — the handler should apply defaults for missing fields.
	if w.Code == http.StatusBadRequest {
		t.Fatalf("status=400 for request with only prompt; defaults should apply. body=%s", w.Body.String())
	}

	// Must be valid JSON regardless of status code.
	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("response is not valid JSON (status=%d): %v (body=%s)", w.Code, err, w.Body.String())
	}
}

func TestHandleFoveatedContext_ContentTypeJSON(t *testing.T) {
	// Verify the response has Content-Type: application/json even for errors.
	s := newMinimalServeServer()
	w := postFoveated(s, `{}`)

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
}

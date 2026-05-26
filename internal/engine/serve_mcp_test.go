// serve_mcp_test.go — tests for registerMCPRoutes helpers (issue #317).
package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMCPGetHandler_BareGet verifies that a bare GET /mcp (no Mcp-Session-Id,
// no text/event-stream Accept) returns 405 with a one-liner hint body rather
// than a cryptic 400. Acceptance criterion from issue #317.
func TestMCPGetHandler_BareGet(t *testing.T) {
	t.Parallel()

	sentinel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := mcpGetHandler(sentinel)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "POST /mcp") {
		t.Errorf("response body should mention POST /mcp; got: %q", body)
	}
	if !strings.Contains(body, "text/event-stream") {
		t.Errorf("response body should mention text/event-stream; got: %q", body)
	}
}

// TestMCPGetHandler_SSEPassthrough verifies that a GET with the proper SSE
// Accept header passes through to the underlying MCP handler unchanged.
func TestMCPGetHandler_SSEPassthrough(t *testing.T) {
	t.Parallel()

	var called bool
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := mcpGetHandler(sentinel)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !called {
		t.Error("underlying handler was not called for SSE GET; want pass-through")
	}
}

// TestMCPGetHandler_SessionIDPassthrough verifies that a GET with an
// Mcp-Session-Id header (SSE stream resume) passes through unchanged.
func TestMCPGetHandler_SessionIDPassthrough(t *testing.T) {
	t.Parallel()

	var called bool
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := mcpGetHandler(sentinel)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", "test-session-abc")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !called {
		t.Error("underlying handler was not called when Mcp-Session-Id is present; want pass-through")
	}
}

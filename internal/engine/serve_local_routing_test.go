// serve_local_routing_test.go — tests for model="local" routing semantics
// and /v1/models advertisement.
//
// Covers myrgic/cogos#75:
//   - model="local" pins to the "ollama" provider when one is registered.
//   - model="local" with no local provider falls through to default routing
//     and emits a structured warning.
//   - /v1/models advertises claude-opus-4-7, not the stale claude-opus-4-6.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// capturingHandler is a minimal slog.Handler that records logged messages so
// tests can assert on structured warnings without touching stderr.
type capturingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *capturingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r)
	return nil
}
func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h // attrs ignored for test purposes
}
func (h *capturingHandler) WithGroup(name string) slog.Handler {
	return h
}

// messages returns all logged message strings (snapshot).
func (h *capturingHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.recs))
	for i, r := range h.recs {
		out[i] = r.Message
	}
	return out
}

// hasWarnContaining returns true if any captured Warn-level record's message
// contains the given substring.
func (h *capturingHandler) hasWarnContaining(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, sub) {
			return true
		}
	}
	return false
}

// newTestServerWithRouter creates a Server wired with the provided router.
func newTestServerWithRouter(t *testing.T, router Router) *Server {
	t.Helper()
	srv := newTestServer(t)
	srv.SetRouter(router)
	return srv
}

// newCloudStub creates a StubProvider that is NOT local (IsLocal=false) so it
// won't satisfy a FirstLocalProvider() walk.
func newCloudStub(name, response string) *StubProvider {
	s := NewStubProvider(name, response)
	s.capabilities.IsLocal = false
	return s
}

// TestModelLocal_PinsToOllamaWhenAvailable verifies that a chat request with
// model="local" routes to the "ollama" provider when one is registered,
// regardless of any other providers in the pool.
func TestModelLocal_PinsToOllamaWhenAvailable(t *testing.T) {
	t.Parallel()

	ollamaStub := NewStubProvider("ollama", "ollama response")
	cloudStub := newCloudStub("claude-code", "cloud response")

	router := NewSimpleRouter(RoutingConfig{Default: "claude-code"})
	router.RegisterProvider(cloudStub)
	router.RegisterProvider(ollamaStub)

	srv := newTestServerWithRouter(t, router)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"hello"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}

	// The ollama stub should have received the request — its lastRequest is set
	// only when it is actually invoked by the provider.
	if ollamaStub.lastRequest == nil {
		t.Error("ollama stub was not called; model=local did not route to ollama")
	}
	if cloudStub.lastRequest != nil {
		t.Error("cloud stub was unexpectedly called; model=local should prefer ollama")
	}

	// Confirm the response content came from ollama.
	var body oaiChatResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Choices) == 0 {
		t.Fatal("no choices in response")
	}
	got := ""
	if body.Choices[0].Message != nil {
		got = extractContent(body.Choices[0].Message.Content)
	}
	if got != "ollama response" {
		t.Errorf("response content = %q; want %q", got, "ollama response")
	}
}

// TestModelLocal_FallsThroughWhenNoLocalProvider verifies that when no local
// provider is registered, model="local" falls through to default routing and
// emits a structured warning so operators can observe the mismatch.
func TestModelLocal_FallsThroughWhenNoLocalProvider(t *testing.T) {
	t.Parallel()

	// Install a capturing logger so we can assert on the warning without
	// touching the global default permanently.
	handler := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cloudStub := newCloudStub("claude-code", "cloud fallback")

	router := NewSimpleRouter(RoutingConfig{Default: "claude-code"})
	router.RegisterProvider(cloudStub)

	srv := newTestServerWithRouter(t, router)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"ping"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	// Should not fail — fall-through to default routing returns a valid response.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (fall-through to default routing)", w.Code)
	}

	// The cloud stub should have been called as the default.
	if cloudStub.lastRequest == nil {
		t.Error("cloud stub was not called on fallthrough; default routing broken")
	}

	// A structured warning must have been emitted mentioning "local".
	if !handler.hasWarnContaining("model=local") {
		t.Errorf("expected a Warn-level log containing %q; got messages: %v",
			"model=local", handler.messages())
	}
}

// TestModelsEndpoint_AdvertisesOpus47 verifies that /v1/models includes
// claude-opus-4-7 and does NOT include the stale claude-opus-4-6.
func TestModelsEndpoint_AdvertisesOpus47(t *testing.T) {
	t.Parallel()

	// Wire a frontier provider so the claude-* entries appear (gated on availability).
	frontierStub := newCloudStub("anthropic", "frontier response")
	router := NewSimpleRouter(RoutingConfig{Default: "anthropic"})
	router.RegisterProvider(frontierStub)
	srv := newTestServerWithRouter(t, router)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.handleModels(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}

	body := w.Body.Bytes()

	// Must advertise the canonical Opus 4.7 ID.
	if !bytes.Contains(body, []byte("claude-opus-4-7")) {
		t.Errorf("/v1/models response does not contain claude-opus-4-7; body: %s", body)
	}

	// Must NOT advertise the stale Opus 4.6 ID.
	if bytes.Contains(body, []byte("claude-opus-4-6")) {
		t.Errorf("/v1/models response still contains stale claude-opus-4-6; body: %s", body)
	}
}

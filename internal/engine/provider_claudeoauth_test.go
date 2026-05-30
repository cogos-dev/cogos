// provider_claudeoauth_test.go — ClaudeOAuthProvider unit tests
//
// All tests use httptest.NewServer to mock the Anthropic API and the
// OAuth token endpoint. No real API calls or credential store reads are made.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// stubCredentialSource is a CredentialSource for tests that returns a
// pre-configured credential and records WriteBack calls.
type stubCredentialSource struct {
	cred       OAuthCredential
	resolveErr error
	writebacks []OAuthCredential
}

func (s *stubCredentialSource) Resolve() (OAuthCredential, error) {
	return s.cred, s.resolveErr
}

func (s *stubCredentialSource) WriteBack(cred OAuthCredential) error {
	s.writebacks = append(s.writebacks, cred)
	return nil
}

// newTestOAuthProvider creates a ClaudeOAuthProvider wired to a test server
// and a stub lifecycle, bypassing all real credential stores.
func newTestOAuthProvider(t *testing.T, endpoint string, cred OAuthCredential) *ClaudeOAuthProvider {
	t.Helper()
	src := &stubCredentialSource{cred: cred}
	lc := NewCredentialLifecycle(src, func(_ context.Context, rt string) (OAuthCredential, error) {
		return OAuthCredential{}, fmt.Errorf("no refresh configured in test")
	})
	p := &ClaudeOAuthProvider{
		name:      "claude-oauth",
		model:     "claude-opus-4-6-20250514",
		endpoint:  strings.TrimRight(endpoint, "/"),
		maxTokens: anthropicDefaultMaxToks,
		timeout:   5 * time.Second,
		client:    &http.Client{Timeout: 5 * time.Second},
		lc:        lc,
	}
	return p
}

// freshCred returns a credential that is valid for 60 minutes.
func freshCred() OAuthCredential {
	return OAuthCredential{
		AccessToken:  "cc-test-access-token",
		RefreshToken: "cc-test-refresh-token",
		ExpiresAtMS:  time.Now().Add(60 * time.Minute).UnixMilli(),
	}
}

// ── setOAuthHeaders ───────────────────────────────────────────────────────────

func TestSetOAuthHeaders(t *testing.T) {
	t.Parallel()
	p := &ClaudeOAuthProvider{name: "test"}
	req, _ := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	p.setOAuthHeaders(req, "my-token", false)

	if auth := req.Header.Get("Authorization"); auth != "Bearer my-token" {
		t.Errorf("Authorization = %q; want 'Bearer my-token'", auth)
	}
	if v := req.Header.Get("anthropic-version"); v != anthropicAPIVersion {
		t.Errorf("anthropic-version = %q; want %q", v, anthropicAPIVersion)
	}
	betaHeader := req.Header.Get("anthropic-beta")
	for _, b := range claudeCodeOAuthOnlyBetas {
		if !strings.Contains(betaHeader, b) {
			t.Errorf("anthropic-beta should contain %q; got %q", b, betaHeader)
		}
	}
	for _, b := range claudeCodeCommonBetas {
		if !strings.Contains(betaHeader, b) {
			t.Errorf("anthropic-beta should contain common beta %q; got %q", b, betaHeader)
		}
	}
	if strings.Contains(betaHeader, claudeCodeContext1MBeta) {
		t.Error("1M context beta should NOT be in default headers")
	}
	ua := req.Header.Get("User-Agent")
	if !strings.HasPrefix(ua, "claude-cli/") {
		t.Errorf("User-Agent = %q; want prefix 'claude-cli/'", ua)
	}
	if req.Header.Get("x-app") != "cli" {
		t.Errorf("x-app = %q; want 'cli'", req.Header.Get("x-app"))
	}
}

func TestSetOAuthHeadersWith1MBeta(t *testing.T) {
	t.Parallel()
	p := &ClaudeOAuthProvider{name: "test"}
	req, _ := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	p.setOAuthHeaders(req, "tok", true)

	betaHeader := req.Header.Get("anthropic-beta")
	if !strings.Contains(betaHeader, claudeCodeContext1MBeta) {
		t.Errorf("1M context beta should be present when with1MBeta=true; got %q", betaHeader)
	}
}

// ── buildOAuthSystem ──────────────────────────────────────────────────────────

// The OAuth path keeps the system field canonical-only (Anthropic scores it for
// "is this genuine Claude Code") and relocates the operator/agent content to the
// user turn. assertCanonicalOnly checks the system field is exactly the prefix.
func assertCanonicalOnly(t *testing.T, blocks []anthropicSystemBlock) {
	t.Helper()
	if len(blocks) != 1 || blocks[0].Text != claudeCodeSystemPrefix {
		t.Fatalf("system field must be exactly the canonical Claude Code prefix; got %+v", blocks)
	}
}

func TestBuildOAuthSystemCanonicalOnly_PromptRelocated(t *testing.T) {
	t.Parallel()
	blocks, injected := buildOAuthSystem(&CompletionRequest{
		SystemPrompt: "Additional instructions.",
	})
	assertCanonicalOnly(t, blocks)
	if !strings.Contains(injected, "Additional instructions.") {
		t.Errorf("caller's system prompt must be in the relocated string; got %q", injected)
	}
}

func TestBuildOAuthSystemPrefixOnlyWhenNoPrompt(t *testing.T) {
	t.Parallel()
	blocks, injected := buildOAuthSystem(&CompletionRequest{})
	assertCanonicalOnly(t, blocks)
	if injected != "" {
		t.Errorf("want empty relocated string when no prompt/context; got %q", injected)
	}
}

func TestBuildOAuthSystemWithContext_Relocated(t *testing.T) {
	t.Parallel()
	blocks, injected := buildOAuthSystem(&CompletionRequest{
		SystemPrompt: "Nucleus.",
		Context: []ContextItem{
			{ID: "cog://mem/note", Zone: ZoneFoveal, Salience: 0.9, Content: "note content"},
		},
	})
	assertCanonicalOnly(t, blocks)
	if !strings.Contains(injected, "note content") {
		t.Error("context item should be in the relocated string")
	}
	if !strings.Contains(injected, "Nucleus.") {
		t.Error("system prompt should be in the relocated string")
	}
	// Order within the relocated content: context items precede the system prompt.
	if strings.Index(injected, "note content") > strings.Index(injected, "Nucleus.") {
		t.Error("order should be: context → system prompt")
	}
}

func TestPrependOAuthSystemToUserTurn(t *testing.T) {
	t.Parallel()
	// Relocated content is inserted as a leading user message; the wire-format
	// normalizer (normalizeAnthropicMessages) then guarantees user-first ordering,
	// alternation, and tool_result-block-0 on the final structure.
	p := &anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: "hi"}}}
	prependOAuthSystemToUserTurn(p, "IDENTITY")
	if len(p.Messages) != 2 || p.Messages[0].Role != "user" || p.Messages[0].Content.(string) != "IDENTITY" {
		t.Fatalf("want a separate leading IDENTITY user message; got %+v", p.Messages)
	}
	if p.Messages[1].Content.(string) != "hi" {
		t.Errorf("original user message must be untouched; got %+v", p.Messages[1])
	}
	// Regression (the live bug): a first user message carrying a tool_result must
	// stay block-0 with no text prepended ahead of it.
	p2 := &anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: []anthropicContentBlock{{Type: "tool_result", ToolUseID: "a", Content: "out"}}}}}
	prependOAuthSystemToUserTurn(p2, "IDENTITY")
	blocks := p2.Messages[1].Content.([]anthropicContentBlock)
	if len(blocks) != 1 || blocks[0].Type != "tool_result" {
		t.Errorf("tool_result must remain the first block of its message; got %+v", blocks)
	}
	// empty injected: no-op.
	p4 := &anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: "hi"}}}
	prependOAuthSystemToUserTurn(p4, "")
	if len(p4.Messages) != 1 || p4.Messages[0].Content.(string) != "hi" {
		t.Error("empty relocated content must be a no-op")
	}
}

// ── Capabilities ──────────────────────────────────────────────────────────────

func TestClaudeOAuthCapabilities(t *testing.T) {
	t.Parallel()
	p := NewClaudeOAuthProvider("claude-oauth", ProviderConfig{
		Model:     "claude-opus-4-6-20250514",
		MaxTokens: 8192,
	}, nil)
	caps := p.Capabilities()

	if caps.IsLocal {
		t.Error("IsLocal should be false (cloud provider)")
	}
	for _, cap := range []Capability{CapStreaming, CapToolUse, CapVision, CapLongContext, CapJSON, CapCaching} {
		if !caps.HasCapability(cap) {
			t.Errorf("missing capability: %s", cap)
		}
	}
	if caps.MaxContextTokens != 1_000_000 {
		t.Errorf("MaxContextTokens = %d; want 1_000_000", caps.MaxContextTokens)
	}
}

// ── Name / Model ──────────────────────────────────────────────────────────────

func TestClaudeOAuthName(t *testing.T) {
	t.Parallel()
	p := NewClaudeOAuthProvider("my-oauth", ProviderConfig{Model: "m"}, nil)
	if p.Name() != "my-oauth" {
		t.Errorf("Name() = %q; want my-oauth", p.Name())
	}
	if p.Model() != "m" {
		t.Errorf("Model() = %q; want m", p.Model())
	}
}

// ── Available ─────────────────────────────────────────────────────────────────

func TestClaudeOAuthAvailable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer srv.Close()

	p := newTestOAuthProvider(t, srv.URL, freshCred())
	if !p.Available(context.Background()) {
		t.Error("Available() = false; want true when /v1/models returns 200")
	}
}

func TestClaudeOAuthAvailableNoCredential(t *testing.T) {
	t.Parallel()
	// No credential available — Available should return false.
	src := &stubCredentialSource{resolveErr: fmt.Errorf("no credential")}
	lc := NewCredentialLifecycle(src, func(_ context.Context, _ string) (OAuthCredential, error) {
		return OAuthCredential{}, fmt.Errorf("no refresh")
	})
	p := &ClaudeOAuthProvider{
		name:   "claude-oauth",
		model:  "m",
		client: &http.Client{Timeout: 5 * time.Second},
		lc:     lc,
	}
	if p.Available(context.Background()) {
		t.Error("Available() = true; want false when no credential")
	}
}

func TestClaudeOAuthAvailableReactive401(t *testing.T) {
	t.Parallel()
	// First /v1/models call returns 401; refresh succeeds; retry returns 200.
	var callCount atomic.Int32

	// A second, updated token is returned after refresh.
	newToken := "cc-refreshed-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		n := callCount.Add(1)
		if n == 1 {
			// First call: 401 to trigger reactive refresh.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Second call (after refresh): check that the new token is used.
		if r.Header.Get("Authorization") != "Bearer "+newToken {
			http.Error(w, "wrong token", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer srv.Close()

	refreshed := OAuthCredential{
		AccessToken: newToken,
		ExpiresAtMS: time.Now().Add(60 * time.Minute).UnixMilli(),
	}
	src := &stubCredentialSource{cred: freshCred()}
	lc := NewCredentialLifecycle(src, func(_ context.Context, _ string) (OAuthCredential, error) {
		return refreshed, nil
	})
	p := &ClaudeOAuthProvider{
		name:     "claude-oauth",
		model:    "m",
		endpoint: strings.TrimRight(srv.URL, "/"),
		timeout:  5 * time.Second,
		client:   &http.Client{Timeout: 5 * time.Second},
		lc:       lc,
	}
	if !p.Available(context.Background()) {
		t.Error("Available() = false; want true after reactive refresh")
	}
}

// ── Ping ──────────────────────────────────────────────────────────────────────

func TestClaudeOAuthPing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer srv.Close()

	p := newTestOAuthProvider(t, srv.URL, freshCred())
	lat, err := p.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if lat <= 0 {
		t.Errorf("latency = %v; want > 0", lat)
	}
}

// ── Complete ──────────────────────────────────────────────────────────────────

func TestClaudeOAuthComplete(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		// Verify OAuth-specific headers are present.
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-api-key") != "" {
			http.Error(w, "x-api-key should not be set for OAuth", http.StatusBadRequest)
			return
		}
		// Verify system is a block array whose first block is the canonical
		// Claude Code prefix (the OAuth wire shape; decodes to []any of maps).
		var reqBody anthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		sysArr, ok := reqBody.System.([]any)
		if !ok || len(sysArr) == 0 {
			http.Error(w, "system should be a non-empty block array", http.StatusBadRequest)
			return
		}
		first, _ := sysArr[0].(map[string]any)
		if first["text"] != claudeCodeSystemPrefix {
			http.Error(w, "block[0] must be the Claude Code prefix", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(anthropicResponseBody("OAuth response"))
	}))
	defer srv.Close()

	p := newTestOAuthProvider(t, srv.URL, freshCred())
	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "OAuth response" {
		t.Errorf("Content = %q; want 'OAuth response'", resp.Content)
	}
}

// 429 then 200: the provider honours retry-after (0s here), backs off, and
// retries to success.
func TestClaudeOAuthComplete429ThenSuccess(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("retry-after", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(anthropicResponseBody("pong"))
	}))
	defer srv.Close()

	p := newTestOAuthProvider(t, srv.URL, freshCred())
	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "pong" {
		t.Errorf("Content = %q; want pong", resp.Content)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server calls = %d; want 2 (429 then 200)", n)
	}
}

// 429 exhausted with a fallback configured: the provider delegates to the CLI
// fallback, which serves the request (here a stub standing in for claude-code).
func TestClaudeOAuthComplete429FallsBackToCLI(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("retry-after", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	p := newTestOAuthProvider(t, srv.URL, freshCred())
	p.fallback = NewStubProvider("cli-fallback", "fallback-served")

	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete (with fallback): %v", err)
	}
	if resp.Content != "fallback-served" {
		t.Errorf("Content = %q; want 'fallback-served' (CLI fallback)", resp.Content)
	}
}

// 429 exhausted with no fallback: returns an error that names the 429.
func TestClaudeOAuthComplete429NoFallbackErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("retry-after", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	}))
	defer srv.Close()

	p := newTestOAuthProvider(t, srv.URL, freshCred())
	_, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error on 429 with no fallback")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error = %v; want mention of 429", err)
	}
}

func TestClaudeOAuthCompleteReactive401(t *testing.T) {
	t.Parallel()

	newToken := "cc-refreshed"
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		n := callCount.Add(1)
		if n == 1 {
			// First call: 401.
			http.Error(w, "expired token", http.StatusUnauthorized)
			return
		}
		// Second call (after refresh): must use the new token.
		if r.Header.Get("Authorization") != "Bearer "+newToken {
			http.Error(w, "wrong token after refresh", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(anthropicResponseBody("After refresh"))
	}))
	defer srv.Close()

	refreshed := OAuthCredential{
		AccessToken: newToken,
		ExpiresAtMS: time.Now().Add(60 * time.Minute).UnixMilli(),
	}
	src := &stubCredentialSource{cred: freshCred()}
	lc := NewCredentialLifecycle(src, func(_ context.Context, _ string) (OAuthCredential, error) {
		return refreshed, nil
	})
	p := &ClaudeOAuthProvider{
		name:      "claude-oauth",
		model:     "claude-opus-4-6-20250514",
		endpoint:  strings.TrimRight(srv.URL, "/"),
		maxTokens: anthropicDefaultMaxToks,
		timeout:   5 * time.Second,
		client:    &http.Client{Timeout: 5 * time.Second},
		lc:        lc,
	}

	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "After refresh" {
		t.Errorf("Content = %q; want 'After refresh'", resp.Content)
	}
	if callCount.Load() != 2 {
		t.Errorf("server call count = %d; want 2 (original + retry)", callCount.Load())
	}
}

func TestClaudeOAuthCompleteHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"overloaded"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := newTestOAuthProvider(t, srv.URL, freshCred())
	_, err := p.Complete(context.Background(), &CompletionRequest{})
	if err == nil {
		t.Error("expected error for 503 response")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}

// ── Stream ────────────────────────────────────────────────────────────────────

func TestClaudeOAuthStream(t *testing.T) {
	t.Parallel()

	events := []anthropicSSEEvent{
		{Type: "message_start", Message: &anthropicSSEMsg{Usage: anthropicUsage{InputTokens: 5}}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthropicContent{Type: "text"}},
		{Type: "content_block_delta", Index: 0, Delta: &anthropicSSEDelta{Type: "text_delta", Text: "OAuth stream"}},
		{Type: "content_block_stop", Index: 0},
		{Type: "message_delta", Delta: &anthropicSSEDelta{StopReason: "end_turn"}, Usage: &anthropicSSEUsage{OutputTokens: 3}},
		{Type: "message_stop"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		// Verify OAuth auth (not x-api-key).
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseLines(events))
	}))
	defer srv.Close()

	p := newTestOAuthProvider(t, srv.URL, freshCred())
	ch, err := p.Stream(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var content strings.Builder
	var done bool
	for sc := range ch {
		if sc.Error != nil {
			t.Fatalf("stream error: %v", sc.Error)
		}
		content.WriteString(sc.Delta)
		if sc.Done {
			done = true
		}
	}
	if content.String() != "OAuth stream" {
		t.Errorf("streamed content = %q; want 'OAuth stream'", content.String())
	}
	if !done {
		t.Error("last chunk should have Done=true")
	}
}

func TestClaudeOAuthStreamReactive401(t *testing.T) {
	t.Parallel()

	newToken := "cc-stream-refresh"
	var callCount atomic.Int32
	events := []anthropicSSEEvent{
		{Type: "message_start", Message: &anthropicSSEMsg{Usage: anthropicUsage{InputTokens: 3}}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthropicContent{Type: "text"}},
		{Type: "content_block_delta", Index: 0, Delta: &anthropicSSEDelta{Type: "text_delta", Text: "after-refresh"}},
		{Type: "content_block_stop", Index: 0},
		{Type: "message_delta", Delta: &anthropicSSEDelta{StopReason: "end_turn"}, Usage: &anthropicSSEUsage{OutputTokens: 2}},
		{Type: "message_stop"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		n := callCount.Add(1)
		if n == 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseLines(events))
	}))
	defer srv.Close()

	refreshed := OAuthCredential{
		AccessToken: newToken,
		ExpiresAtMS: time.Now().Add(60 * time.Minute).UnixMilli(),
	}
	src := &stubCredentialSource{cred: freshCred()}
	lc := NewCredentialLifecycle(src, func(_ context.Context, _ string) (OAuthCredential, error) {
		return refreshed, nil
	})
	p := &ClaudeOAuthProvider{
		name:      "claude-oauth",
		model:     "claude-opus-4-6-20250514",
		endpoint:  strings.TrimRight(srv.URL, "/"),
		maxTokens: anthropicDefaultMaxToks,
		timeout:   5 * time.Second,
		client:    &http.Client{Timeout: 5 * time.Second},
		lc:        lc,
	}

	ch, err := p.Stream(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var content strings.Builder
	for sc := range ch {
		if sc.Error != nil {
			t.Fatalf("stream error: %v", sc.Error)
		}
		content.WriteString(sc.Delta)
	}
	if content.String() != "after-refresh" {
		t.Errorf("content = %q; want 'after-refresh'", content.String())
	}
}

func TestClaudeOAuthStreamHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"overloaded"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := newTestOAuthProvider(t, srv.URL, freshCred())
	_, err := p.Stream(context.Background(), &CompletionRequest{})
	if err == nil {
		t.Error("expected error for 503 before stream starts")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention 503, got: %v", err)
	}
}

// ── claudeCodeCredentialSource.WriteBack ──────────────────────────────────────

func TestClaudeCodeCredentialSourceWriteBack(t *testing.T) {
	// Not parallel: uses a temp dir.
	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")

	src := &claudeCodeCredentialSource{credPath: credPath}

	// Seed an existing file with other top-level fields.
	initial := map[string]any{
		"primaryApiKey": "sk-ant-test",
		"claudeAiOauth": map[string]any{
			"accessToken":  "old",
			"refreshToken": "old-ref",
			"expiresAt":    12345,
			"scopes":       []string{"user:inference"},
		},
	}
	raw, _ := json.Marshal(initial)
	_ = os.WriteFile(credPath, raw, 0600)

	newCred := OAuthCredential{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		ExpiresAtMS:  99999,
	}
	if err := src.WriteBack(newCred); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}

	// Read back and verify.
	data, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Other top-level fields must be preserved.
	if _, ok := doc["primaryApiKey"]; !ok {
		t.Error("primaryApiKey should be preserved")
	}

	// claudeAiOauth must be updated.
	var oauth claudeAiOauthJSON
	if err := json.Unmarshal(doc["claudeAiOauth"], &oauth); err != nil {
		t.Fatalf("Unmarshal claudeAiOauth: %v", err)
	}
	if oauth.AccessToken != "new-access" {
		t.Errorf("accessToken = %q; want new-access", oauth.AccessToken)
	}
	if oauth.RefreshToken != "new-refresh" {
		t.Errorf("refreshToken = %q; want new-refresh", oauth.RefreshToken)
	}
	if oauth.ExpiresAt != 99999 {
		t.Errorf("expiresAt = %d; want 99999", oauth.ExpiresAt)
	}
	// Scopes from old record should be preserved.
	if len(oauth.Scopes) == 0 {
		t.Error("scopes should be preserved from prior record")
	}

	// File permissions must be 0600.
	fi, _ := os.Stat(credPath)
	if fi.Mode().Perm() != 0600 {
		t.Errorf("file mode = %o; want 0600", fi.Mode().Perm())
	}
}

func TestClaudeCodeCredentialSourceWriteBackNoExisting(t *testing.T) {
	// Not parallel: uses a temp dir.
	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")
	src := &claudeCodeCredentialSource{credPath: credPath}

	newCred := OAuthCredential{
		AccessToken:  "tok",
		RefreshToken: "ref",
		ExpiresAtMS:  12345,
	}
	if err := src.WriteBack(newCred); err != nil {
		t.Fatalf("WriteBack on new file: %v", err)
	}

	data, _ := os.ReadFile(credPath)
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(data, &doc)
	var oauth claudeAiOauthJSON
	_ = json.Unmarshal(doc["claudeAiOauth"], &oauth)
	if oauth.AccessToken != "tok" {
		t.Errorf("accessToken = %q; want tok", oauth.AccessToken)
	}
}

// ── makeProvider integration ──────────────────────────────────────────────────

func TestMakeProviderClaudeOAuth(t *testing.T) {
	t.Parallel()
	p, err := makeProvider("claude-oauth", ProviderConfig{
		Type:  "claude-oauth",
		Model: "claude-opus-4-6-20250514",
	}, nil)
	if err != nil {
		t.Fatalf("makeProvider claude-oauth: %v", err)
	}
	if p.Name() != "claude-oauth" {
		t.Errorf("Name() = %q; want claude-oauth", p.Name())
	}
	if _, ok := p.(*ClaudeOAuthProvider); !ok {
		t.Errorf("provider type = %T; want *ClaudeOAuthProvider", p)
	}
}

// min is available as a builtin since Go 1.21; kept as local for test-file clarity.
// (shadowing the builtin is fine inside a function, but a package-level redeclaration
//  is a compilation error in Go 1.21+)

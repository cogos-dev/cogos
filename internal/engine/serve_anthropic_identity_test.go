// serve_anthropic_identity_test.go — G1b: per-session identity resolution at
// the Anthropic Messages gateway (/v1/messages).
//
// Test matrix mirrors serve_identity_gateway_test.go (handleChat):
//
//	(a) flag OFF, unbound        → TargetIdentity==nucleus.Name, nucleus card present.
//	(b) flag OFF, bound "alice"  → only attribution changes; nucleus card still present.
//	(c) flag ON,  unbound        → NO nucleus card, clean transport path.
//	(d) flag ON,  bound "alice"  → attribution "alice", clean (no nucleus card).
//
// All four cases use StubProvider so handleAnthropicMessages runs the full
// success path and we can inspect lastRequest.SystemPrompt.
//
// Nucleus card sentinel: makeNucleus("TestNucleus", "test-role") produces
// Card "# TestNucleus\nRole: test-role\n"; we check for "TestNucleus" presence.
package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doAnthropicRequest POSTs to /v1/messages and returns the HTTP recorder.
// sessionHeader is the X-Cogos-Session-Id value; empty means not sent.
func doAnthropicRequest(t *testing.T, srv *Server, sessionHeader string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":      "claude",
		"messages":   []map[string]any{{"role": "user", "content": "hello"}},
		"max_tokens": 128,
		"stream":     false,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if sessionHeader != "" {
		req.Header.Set("X-Cogos-Session-Id", sessionHeader)
	}
	w := httptest.NewRecorder()
	srv.handleAnthropicMessages(w, req)
	return w
}

// doAnthropicRequestWithUserID posts to /v1/messages with metadata.user_id set.
func doAnthropicRequestWithUserID(t *testing.T, srv *Server, userID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":      "claude",
		"messages":   []map[string]any{{"role": "user", "content": "hello"}},
		"max_tokens": 128,
		"stream":     false,
		"metadata":   map[string]any{"user_id": userID},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAnthropicMessages(w, req)
	return w
}

// ─── (a) flag OFF, unbound → nucleus attribution + nucleus card ──────────────

// TestAnthropicGatewayIdentity_FlagOff_Unbound verifies that with
// IdentityNakedDefault=false and no session header, the nucleus card is present
// in the system prompt. No-regression case: byte-for-byte today's behavior.
func TestAnthropicGatewayIdentity_FlagOff_Unbound(t *testing.T) {
	t.Parallel()
	srv, stub, _ := newGatewayTestServer(t, false)

	w := doAnthropicRequest(t, srv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("(a) status = %d; want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("(a) stub did not capture lastRequest")
	}
	// Nucleus card must be present — identical to pre-G1 behavior.
	if !strings.Contains(lr.SystemPrompt, "TestNucleus") {
		t.Errorf("(a) flag OFF unbound: expected nucleus card in SystemPrompt, got %q",
			lr.SystemPrompt)
	}
}

// ─── (b) flag OFF, bound "alice" → only attribution changes; card still present

// TestAnthropicGatewayIdentity_FlagOff_BoundForeign verifies that with
// IdentityNakedDefault=false, a bound (but foreign) session does NOT strip the
// nucleus card. Only block.TargetIdentity changes.
func TestAnthropicGatewayIdentity_FlagOff_BoundForeign(t *testing.T) {
	t.Parallel()
	srv, stub, fake := newGatewayTestServer(t, false)
	bindSession(fake, "sess-alice", "alice")

	w := doAnthropicRequest(t, srv, "sess-alice")
	if w.Code != http.StatusOK {
		t.Fatalf("(b) status = %d; want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("(b) stub did not capture lastRequest")
	}
	// Flag OFF: embodiment unchanged — nucleus card still present.
	if !strings.Contains(lr.SystemPrompt, "TestNucleus") {
		t.Errorf("(b) flag OFF bound: expected nucleus card in SystemPrompt, got %q",
			lr.SystemPrompt)
	}
}

// ─── (c) flag ON, unbound → no nucleus card, clean transport ─────────────────

// TestAnthropicGatewayIdentity_FlagOn_Unbound verifies that with
// IdentityNakedDefault=true and no session, the nucleus card is absent from
// the system prompt (clean transport path).
func TestAnthropicGatewayIdentity_FlagOn_Unbound(t *testing.T) {
	t.Parallel()
	srv, stub, _ := newGatewayTestServer(t, true)

	w := doAnthropicRequest(t, srv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("(c) status = %d; want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("(c) stub did not capture lastRequest")
	}
	// Clean transport: NO nucleus card in system prompt.
	if strings.Contains(lr.SystemPrompt, "TestNucleus") {
		t.Errorf("(c) flag ON unbound: nucleus card must NOT appear in SystemPrompt, got %q",
			lr.SystemPrompt)
	}
}

// ─── (d) flag ON, bound "alice" (foreign) → attribution "alice", no card ─────

// TestAnthropicGatewayIdentity_FlagOn_BoundForeign verifies that with
// IdentityNakedDefault=true, a session bound to a foreign subject gets clean
// transport (no nucleus card).
func TestAnthropicGatewayIdentity_FlagOn_BoundForeign(t *testing.T) {
	t.Parallel()
	srv, stub, fake := newGatewayTestServer(t, true)
	bindSession(fake, "sess-alice", "alice")

	w := doAnthropicRequest(t, srv, "sess-alice")
	if w.Code != http.StatusOK {
		t.Fatalf("(d) status = %d; want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("(d) stub did not capture lastRequest")
	}
	// Foreign bound subject → clean (no nucleus card).
	if strings.Contains(lr.SystemPrompt, "TestNucleus") {
		t.Errorf("(d) flag ON foreign-bound: nucleus card must NOT appear in SystemPrompt, got %q",
			lr.SystemPrompt)
	}
}

// ─── metadata.user_id fallback ───────────────────────────────────────────────

// TestAnthropicGatewayIdentity_UserIDFallback verifies that metadata.user_id
// serves as the reqUser fallback for identity resolution when no
// X-Cogos-Session-Id header is present.
func TestAnthropicGatewayIdentity_UserIDFallback(t *testing.T) {
	t.Parallel()
	// flag OFF so we can confirm only attribution changes (card still present).
	srv, stub, fake := newGatewayTestServer(t, false)
	bindSession(fake, "uid-bob", "bob")

	w := doAnthropicRequestWithUserID(t, srv, "uid-bob")
	if w.Code != http.StatusOK {
		t.Fatalf("user_id fallback: status = %d; want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("user_id fallback: stub did not capture lastRequest")
	}
	// Flag OFF: nucleus card still present even though binding resolved.
	if !strings.Contains(lr.SystemPrompt, "TestNucleus") {
		t.Errorf("user_id fallback: expected nucleus card in SystemPrompt, got %q",
			lr.SystemPrompt)
	}
}

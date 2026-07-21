// serve_identity_grants_test.go — end-to-end tests for board task 60 chunk 1
// (kernel-issued identity grants). Follows the newChannelServer pattern in
// serve_sessions_channel_test.go: a Server wired with just the fields the
// handlers need, fronted by httptest.NewServer.
package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newIdentityGrantServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := &Server{identityGrants: NewIdentityGrantRegistry()}
	mux := http.NewServeMux()
	s.registerIdentityGrantRoutes(mux)
	front := httptest.NewServer(mux)
	t.Cleanup(func() { front.Close() })
	return s, front
}

func identityPostJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func identityDecodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode body %s: %v", raw, err)
	}
}

func TestIdentityGrantMint_ScopeNeverWidened(t *testing.T) {
	_, front := newIdentityGrantServer(t)

	resp := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out identityGrantMintResponse
	identityDecodeBody(t, resp, &out)
	if out.Token == "" {
		t.Fatalf("expected a non-empty token")
	}
	if len(out.Scope) != 1 || out.Scope[0] != "chat:post" {
		t.Fatalf("expected scope to echo exactly what was requested, got %v", out.Scope)
	}
}

func TestIdentityGrantMint_IdempotentPerSurface(t *testing.T) {
	_, front := newIdentityGrantServer(t)

	first := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post", "chat:read"},
	})
	var firstOut identityGrantMintResponse
	identityDecodeBody(t, first, &firstOut)

	// Simulate a restart: mint again for the same surface. Must return the
	// SAME token, not invalidate whatever a client already holds (design
	// §4 chunk-1 verify-teeth item 5).
	second := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post", "chat:read"},
	})
	var secondOut identityGrantMintResponse
	identityDecodeBody(t, second, &secondOut)

	if firstOut.Token != secondOut.Token {
		t.Fatalf("expected idempotent reuse of the live grant, got different tokens: %q vs %q",
			firstOut.Token, secondOut.Token)
	}
	if firstOut.GrantID != secondOut.GrantID {
		t.Fatalf("expected the same grant_id on reuse, got %q vs %q", firstOut.GrantID, secondOut.GrantID)
	}
}

func TestIdentityVerify_ValidAndInvalidTokens(t *testing.T) {
	_, front := newIdentityGrantServer(t)

	mint := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})
	var minted identityGrantMintResponse
	identityDecodeBody(t, mint, &minted)

	// Valid token verifies.
	ok := identityPostJSON(t, front.URL+"/v1/identity/verify", map[string]any{
		"surface": "constellation-chat",
		"token":   minted.Token,
	})
	var okOut identityVerifyResponse
	identityDecodeBody(t, ok, &okOut)
	if !okOut.Valid {
		t.Fatalf("expected valid=true for a freshly minted token")
	}

	// Garbage token is rejected.
	bad := identityPostJSON(t, front.URL+"/v1/identity/verify", map[string]any{
		"surface": "constellation-chat",
		"token":   "garbage",
	})
	var badOut identityVerifyResponse
	identityDecodeBody(t, bad, &badOut)
	if badOut.Valid {
		t.Fatalf("expected valid=false for a garbage token")
	}

	// Right token, wrong surface is rejected — scope isolation across surfaces.
	wrongSurface := identityPostJSON(t, front.URL+"/v1/identity/verify", map[string]any{
		"surface": "signal-dashboard",
		"token":   minted.Token,
	})
	var wrongOut identityVerifyResponse
	identityDecodeBody(t, wrongSurface, &wrongOut)
	if wrongOut.Valid {
		t.Fatalf("expected valid=false when a token is presented for a different surface")
	}
}

func TestIdentityGrantList_NeverIncludesToken(t *testing.T) {
	_, front := newIdentityGrantServer(t)
	identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})

	resp, err := http.Get(front.URL + "/v1/identity/grants")
	if err != nil {
		t.Fatalf("GET grants: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if bytes.Contains(bytes.ToLower(raw), []byte(`"token"`)) {
		t.Fatalf("grant listing must never include a token field; got %s", raw)
	}
}

func TestIdentityGrantCurrent_ZeroPasteBootstrap(t *testing.T) {
	_, front := newIdentityGrantServer(t)

	// No grant minted yet -> 404, so a caller knows to mint or degrade.
	miss, err := http.Get(front.URL + "/v1/identity/grants/current?surface=constellation-chat")
	if err != nil {
		t.Fatalf("GET current: %v", err)
	}
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 before any grant exists, got %d", miss.StatusCode)
	}
	miss.Body.Close()

	minted := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})
	var mintedOut identityGrantMintResponse
	identityDecodeBody(t, minted, &mintedOut)

	// Now the surface's own page can bootstrap with zero paste: GET current
	// returns the SAME live token, no operator action involved.
	hit, err := http.Get(front.URL + "/v1/identity/grants/current?surface=constellation-chat")
	if err != nil {
		t.Fatalf("GET current: %v", err)
	}
	var hitOut identityGrantMintResponse
	identityDecodeBody(t, hit, &hitOut)
	if hitOut.Token != mintedOut.Token {
		t.Fatalf("expected GET current to return the live grant's token unchanged, got %q vs %q",
			hitOut.Token, mintedOut.Token)
	}
}

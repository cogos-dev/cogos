package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newGrantAuthTestServer builds a Server exactly like newTestServer, then
// force-enables the write-route grant-auth gate. makeConfig defaults
// WriteRouteGrantAuthDisabled to true for the rest of this package's test
// suite (most of it predates this gate and exercises routes it now covers
// with no X-Cogos-Grant header) — every test in this file that means to
// exercise the gate itself must flip it back on explicitly rather than rely
// on the package-wide test default.
func newGrantAuthTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(t)
	srv.cfg.WriteRouteGrantAuthDisabled = false
	return srv
}

// mintTestGrant mints a live grant on srv's registry for use as a valid
// X-Cogos-Grant header value in tests. t.Helper so failures point at the
// caller.
func mintTestGrant(t *testing.T, srv *Server, surface string) string {
	t.Helper()
	grant, err := srv.identityGrants.MintOrReuse(surface, []string{"test"}, time.Hour)
	if err != nil {
		t.Fatalf("mintTestGrant: MintOrReuse: %v", err)
	}
	if grant.Token == "" {
		t.Fatalf("mintTestGrant: minted grant has empty token")
	}
	return grant.Token
}

// (a) write route without header -> 401.
func TestGrantAuth_WriteRouteWithoutHeader_401(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/attention", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatalf("POST /v1/attention: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["type"] != "missing_grant" {
		t.Errorf("error.type = %v; want missing_grant", errObj["type"])
	}
}

// (b) with valid grant -> 200 (or at least: not 401 from the grant gate; the
// route's own handler runs and produces its normal response).
func TestGrantAuth_WriteRouteWithValidGrant_PassesGate(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	token := mintTestGrant(t, srv, "test-surface")

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/attention", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(GrantHeaderName, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/attention: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("status = 401 with a valid grant; gate should have passed the request through")
	}
}

// (c) revoked grant -> 401.
func TestGrantAuth_RevokedGrant_401(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	grant, err := srv.identityGrants.MintOrReuse("revoke-me", []string{"test"}, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := grant.Token

	if _, err := srv.identityGrants.Revoke(grant.GrantID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/attention", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(GrantHeaderName, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/attention: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 for a revoked grant", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["type"] != "invalid_grant" {
		t.Errorf("error.type = %v; want invalid_grant", errObj["type"])
	}
}

// (d) /mcp tool call gated — including cog_write_cogdoc, the known
// membrane-bypass companion gap. This test PROVES an unauthenticated
// POST /mcp JSON-RPC tools/call for cog_write_cogdoc is rejected before it
// ever reaches the MCP tool dispatcher: the response is the grant-auth 401
// (not any MCP-protocol-level error), and — the stronger proof — the file
// the call would have written never appears on disk.
func TestGrantAuth_MCP_UnauthenticatedCogWriteCogdoc_Rejected(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const relPath = "semantic/grant-auth-canary.cog.md"
	absPath := filepath.Join(srv.cfg.WorkspaceRoot, ".cog", "mem", relPath)

	rpcBody := `{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "tools/call",
		"params": {
			"name": "cog_write_cogdoc",
			"arguments": {
				"path": "` + relPath + `",
				"title": "grant auth canary",
				"content": "if this file exists, an unauthenticated /mcp call wrote it"
			}
		}
	}`

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(rpcBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 (grant gate must reject before the MCP layer runs)", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["type"] != "missing_grant" {
		t.Errorf("error.type = %v; want missing_grant (proves this is the grant gate, not an MCP-level error)", errObj["type"])
	}

	if _, statErr := os.Stat(absPath); statErr == nil {
		t.Fatalf("cog_write_cogdoc executed despite the missing grant — file exists at %s", absPath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error for %s: %v", absPath, statErr)
	}
}

// (d, continued) /mcp is gated on GET too — proves the exemption carve-out
// for /mcp (never falls into the blanket "GET is exempt" rule) actually
// applies to the GET method specifically, not just POST.
func TestGrantAuth_MCP_GetWithoutHeader_401(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/mcp", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /mcp: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 (GET /mcp must not be covered by the blanket GET exemption)", resp.StatusCode)
	}
}

// (e) GET + /health unaffected.
func TestGrantAuth_GetRoutesAndHealth_Unaffected(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/health", "/v1/manifest", "/v1/context"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("GET %s = 401; want the route's normal (non-gated) behavior", path)
		}
	}
}

// POST /v1/identity/verify stays reachable with no grant — gating the
// verification authority itself would be circular (a caller with no grant
// could never learn whether its token is valid).
func TestGrantAuth_IdentityVerify_ExemptEvenWithoutHeader(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/identity/verify", "application/json",
		bytes.NewBufferString(`{"surface":"x","token":"bogus"}`))
	if err != nil {
		t.Fatalf("POST /v1/identity/verify: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("status = 401; POST /v1/identity/verify must be exempt from the grant gate")
	}
}

// POST /v1/identity/grants (mint) stays reachable with no grant — the
// bootstrap exemption — but is rate-limited.
func TestGrantAuth_IdentityGrantsMint_BootstrapExemptButRateLimited(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	// Tighten the limiter so the test doesn't need 20+ requests to observe
	// the 429.
	srv.grantMintLimiter = newGrantMintLimiter(2, time.Minute)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	mint := func(surface string) int {
		body := `{"surface":"` + surface + `"}`
		resp, err := http.Post(ts.URL+"/v1/identity/grants", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("POST /v1/identity/grants: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := mint("s1"); code == http.StatusUnauthorized {
		t.Fatalf("first mint got 401; bootstrap mint must be exempt from the grant gate")
	}
	if code := mint("s2"); code == http.StatusUnauthorized {
		t.Fatalf("second mint got 401; bootstrap mint must be exempt from the grant gate")
	}
	// Third request in the same window should be rate-limited, not gated by
	// the grant check (still no X-Cogos-Grant header on any of these calls).
	if code := mint("s3"); code != http.StatusTooManyRequests {
		t.Errorf("third mint in window = %d; want 429 rate_limited", code)
	}
}

// (f) enforcement-off knob restores today's behavior.
func TestGrantAuth_DisabledKnob_RestoresUngatedBehavior(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.cfg.WriteRouteGrantAuthDisabled = true
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/attention", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatalf("POST /v1/attention: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("status = 401 with WriteRouteGrantAuthDisabled=true; the knob should fully restore ungated behavior")
	}
}

// grantMintLimiter unit coverage — window reset + limit enforcement in
// isolation from the HTTP layer.
func TestGrantMintLimiter_AllowsUpToLimitThenBlocks(t *testing.T) {
	t.Parallel()
	l := newGrantMintLimiter(3, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if !l.Allow() {
			t.Fatalf("Allow() #%d = false; want true within limit", i)
		}
	}
	if l.Allow() {
		t.Fatalf("Allow() after limit reached = true; want false")
	}

	// Advance past the window: should reset.
	now = now.Add(time.Minute + time.Second)
	if !l.Allow() {
		t.Fatalf("Allow() after window elapsed = false; want true (window should reset)")
	}
}

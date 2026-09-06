package engine

import (
	"bytes"
	"encoding/json"
	"io"
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

// mintTestGrant mints a live, fully-scoped grant on srv's registry for use
// as a valid X-Cogos-Grant header value in tests. t.Helper so failures point
// at the caller.
//
// It carries all three concrete scopes because its callers are testing the
// AUTHENTICATION half of the gate ("does a valid credential get through"),
// not the authorization half — a narrower scope here would make those tests
// fail for a reason they are not about. Tests that are specifically about
// scope use mintScopedTestGrant.
func mintTestGrant(t *testing.T, srv *Server, surface string) string {
	t.Helper()
	return mintScopedTestGrant(t, srv, surface, ScopeInference, ScopeWrite, ScopeAdmin)
}

// mintScopedTestGrant mints a live grant carrying exactly the given scopes.
// Used by the scope-enforcement tests (ledger L02), where the whole point is
// that the grant is live but lacks the scope the route demands.
func mintScopedTestGrant(t *testing.T, srv *Server, surface string, scopes ...string) string {
	t.Helper()
	grant, err := srv.identityGrants.MintOrReuse(surface, scopes, time.Hour)
	if err != nil {
		t.Fatalf("mintScopedTestGrant: MintOrReuse: %v", err)
	}
	if grant.Token == "" {
		t.Fatalf("mintScopedTestGrant: minted grant has empty token")
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

// ── POST /v1/identity/grants (mint): bootstrap exemption removed, round 2 ──
//
// cog-review (PR #551, head 00bc7b2) confirmed the bootstrap exemption's
// confidentiality-only analysis missed integrity/availability: an
// unauthenticated blind cross-origin CSRF POST could mint a superseding
// grant for surface="node-root" and invalidate the live one for every other
// local consumer. The fix removed the exemption entirely (mint is gated like
// every other write route) and added a surface-match rule on top (mint and
// revoke require the presented grant to be node-root or match the target
// surface). The tests below replace the retired
// TestGrantAuth_IdentityGrantsMint_BootstrapExemptButRateLimited, which
// asserted the now-removed exempt behavior.

// (a) unauthenticated POST /v1/identity/grants -> 401, same as any other
// write route now that the bootstrap exemption is gone.
func TestGrantAuth_IdentityGrantsMint_UnauthenticatedIs401(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/identity/grants", "application/json",
		bytes.NewBufferString(`{"surface":"node-root"}`))
	if err != nil {
		t.Fatalf("POST /v1/identity/grants: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401 (mint is no longer bootstrap-exempt)", resp.StatusCode)
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

// (b) the exact CSRF shape the review confirmed: a CORS-simple POST
// (Content-Type: text/plain, no custom header — so no preflight, and no
// X-Cogos-Grant) targeting surface="node-root" with a different scope than
// the live grant. Before the fix this reached MintOrReuse and superseded the
// live node-root grant, invalidating it for every other cached consumer.
// Now it must die at the gate (401) BEFORE ever reaching the handler, and
// the live node-root grant's token must still verify unchanged afterward.
func TestGrantAuth_IdentityGrantsMint_BlindCSRFMintDoesNotSupersedeNodeRoot(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	liveToken := mintTestGrant(t, srv, nodeRootSurface)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/identity/grants",
		strings.NewReader(`{"surface":"node-root","scope":["attacker-scope"]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	// CORS-simple: Content-Type: text/plain, no X-Cogos-Grant. This is the
	// shape a cross-origin page can send with no preflight at all.
	req.Header.Set("Content-Type", "text/plain")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/identity/grants (blind CSRF shape): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401 — the blind CSRF mint must never reach MintOrReuse", resp.StatusCode)
	}

	// The live node-root grant must be entirely unchanged: same token still
	// verifies. If the attack had reached MintOrReuse, this would fail
	// (the scope mismatch would have superseded it).
	verify := identityPostJSON(t, ts.URL+"/v1/identity/verify", map[string]any{
		"surface": nodeRootSurface,
		"token":   liveToken,
	})
	var verifyOut identityVerifyResponse
	identityDecodeBody(t, verify, &verifyOut)
	if !verifyOut.Valid {
		t.Fatalf("expected the live node-root grant to still verify after the blocked blind-CSRF mint attempt")
	}
}

// (c) mint of a NEW surface, presenting the node-root grant -> 200. This is
// the real bootstrap path now: a consumer reads node-root's token from the
// 0600 vault file ~/.cog/vault/node-root-grant (the zero-paste HTTP
// primitive is itself gated as of ledger L03), then uses it to mint its own
// surface-scoped grant.
func TestGrantAuth_IdentityGrantsMint_WithNodeRootGrant_Succeeds(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	nodeRootToken := mintTestGrant(t, srv, nodeRootSurface)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/identity/grants",
		bytes.NewBufferString(`{"surface":"constellation-chat","scope":["chat:post"]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(GrantHeaderName, nodeRootToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/identity/grants: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 when presenting the node-root grant", resp.StatusCode)
	}
	var out identityGrantMintResponse
	identityDecodeBody(t, resp, &out)
	if out.Token == "" {
		t.Fatalf("expected a non-empty token in the mint response")
	}
}

// (d) revoke of node-root's grant, presented with a throwaway-surface
// grant -> 403 (surface_mismatch). Closes the unverified-note attack chain:
// without this, any authenticated-but-unrelated grant holder could revoke
// node-root outright (its grant_id is visible via GET /v1/identity/grants,
// which is itself gated as of ledger L03 but readable by any admin-scoped
// caller).
func TestGrantAuth_IdentityGrantsRevoke_ThrowawaySurfaceCannotRevokeNodeRoot(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	nodeRootGrant, err := srv.identityGrants.MintOrReuse(nodeRootSurface, []string{nodeRootScope}, time.Hour)
	if err != nil {
		t.Fatalf("mint node-root: %v", err)
	}
	throwawayToken := mintTestGrant(t, srv, "throwaway-surface")

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/identity/grants/"+nodeRootGrant.GrantID+"/revoke", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(GrantHeaderName, throwawayToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST revoke: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 surface_mismatch (a throwaway-surface grant must not revoke node-root)", resp.StatusCode)
	}

	// node-root's grant must still be live.
	verify := identityPostJSON(t, ts.URL+"/v1/identity/verify", map[string]any{
		"surface": nodeRootSurface,
		"token":   nodeRootGrant.Token,
	})
	var verifyOut identityVerifyResponse
	identityDecodeBody(t, verify, &verifyOut)
	if !verifyOut.Valid {
		t.Fatalf("expected node-root's grant to still verify after the rejected cross-surface revoke attempt")
	}
}

// (e) revoke of node-root's OWN grant, presented with the node-root grant
// itself -> succeeds. The admin surface can revoke anything, including
// itself (rotation's second half: mint new, then revoke old).
func TestGrantAuth_IdentityGrantsRevoke_WithNodeRootGrant_Succeeds(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	nodeRootGrant, err := srv.identityGrants.MintOrReuse(nodeRootSurface, []string{nodeRootScope}, time.Hour)
	if err != nil {
		t.Fatalf("mint node-root: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/identity/grants/"+nodeRootGrant.GrantID+"/revoke", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(GrantHeaderName, nodeRootGrant.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST revoke: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 — node-root presenting its own grant must be able to revoke it", resp.StatusCode)
	}
}

// (f) the rate limiter still applies to the now-gated mint route, as
// defense-in-depth against an AUTHENTICATED caller minting excessively
// (every request below presents a valid node-root grant, unlike the retired
// bootstrap-exempt version of this test).
func TestGrantAuth_IdentityGrantsMint_StillRateLimitedWhenAuthenticated(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	// Tighten the limiter so the test doesn't need 20+ requests to observe
	// the 429.
	srv.grantMintLimiter = newGrantMintLimiter(2, time.Minute)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	nodeRootToken := mintTestGrant(t, srv, nodeRootSurface)

	mint := func(surface string) int {
		body := `{"surface":"` + surface + `"}`
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/identity/grants", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(GrantHeaderName, nodeRootToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/identity/grants: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := mint("s1"); code != http.StatusOK {
		t.Fatalf("first mint = %d; want 200 (authenticated as node-root)", code)
	}
	if code := mint("s2"); code != http.StatusOK {
		t.Fatalf("second mint = %d; want 200 (authenticated as node-root)", code)
	}
	// Third request in the same window should be rate-limited even though
	// the caller is fully authenticated — the limiter is defense-in-depth,
	// not the primary guard anymore.
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

// TestGrantAuth_DisabledKnob_MintWorksWithoutHeader is the round-3 regression
// test (cog-review, PR #551 round 3): the surface-match hardening added in
// round 2 read the presented grant from request context unconditionally, but
// grantAuthMiddleware only ever populates that context when the gate is
// enabled — with the disable knob flipped, mint 403'd every request instead
// of restoring its pre-grant-auth 200 behavior. Mirrors
// TestGrantAuth_DisabledKnob_RestoresUngatedBehavior but targets the mint
// route specifically, with no X-Cogos-Grant header at all.
func TestGrantAuth_DisabledKnob_MintWorksWithoutHeader(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.cfg.WriteRouteGrantAuthDisabled = true
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/identity/grants", "application/json",
		bytes.NewBufferString(`{"surface":"any-surface"}`))
	if err != nil {
		t.Fatalf("POST /v1/identity/grants: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 with WriteRouteGrantAuthDisabled=true and no header — "+
			"the disable knob must restore mint to its pre-gate behavior, not 403 on the surface-match check", resp.StatusCode)
	}
	var out identityGrantMintResponse
	identityDecodeBody(t, resp, &out)
	if out.Token == "" {
		t.Fatalf("expected a non-empty token when the gate is disabled")
	}
}

// TestGrantAuth_DisabledKnob_RevokeWorksWithoutHeader is revoke's half of the
// same round-3 regression: with the gate disabled, revoking a live grant
// with no X-Cogos-Grant header at all must succeed, not 403 on the
// surface-match check.
func TestGrantAuth_DisabledKnob_RevokeWorksWithoutHeader(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.cfg.WriteRouteGrantAuthDisabled = true
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Mint directly on the registry (bypassing HTTP) so the test doesn't
	// depend on the mint route's own behavior to set up its fixture.
	grant, err := srv.identityGrants.MintOrReuse("revoke-me", []string{"test"}, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	resp, err := http.Post(ts.URL+"/v1/identity/grants/"+grant.GrantID+"/revoke", "application/json", nil)
	if err != nil {
		t.Fatalf("POST revoke: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 with WriteRouteGrantAuthDisabled=true and no header — "+
			"the disable knob must restore revoke to its pre-gate behavior, not 403 on the surface-match check", resp.StatusCode)
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

// ── Authorization: Bearer as a grant carrier ─────────────────────────────────
//
// An OpenAI/Anthropic-compatible client has one auth knob: an "API key"
// field that lands in Authorization: Bearer. It cannot send X-Cogos-Grant.
// The gate must accept the grant there, with the same verification.

func bearerPost(t *testing.T, url, authorization string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/attention", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestGrantAuth_BearerWithValidGrant_PassesGate(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	token := mintTestGrant(t, srv, "bearer-surface")

	if code := bearerPost(t, ts.URL, "Bearer "+token); code == http.StatusUnauthorized {
		t.Fatal("401 with a valid grant in Authorization: Bearer — an ignorant OpenAI-compat client cannot get past the gate")
	}
	// Scheme is case-insensitive per RFC 9110.
	if code := bearerPost(t, ts.URL, "bearer "+token); code == http.StatusUnauthorized {
		t.Fatal("401 with lowercase 'bearer' scheme")
	}
}

func TestGrantAuth_BearerWithBogusGrant_401(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if code := bearerPost(t, ts.URL, "Bearer not-a-real-grant"); code != http.StatusUnauthorized {
		t.Fatalf("status = %d with a bogus bearer token; want 401 — Bearer must go through the SAME verification, not bypass it", code)
	}
}

func TestGrantAuth_NonBearerAuthorization_TreatedAsAbsent(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	token := mintTestGrant(t, srv, "basic-surface")

	// A valid grant under the wrong scheme must NOT be honored.
	for _, hdr := range []string{"Basic " + token, token, "Token " + token, "Bearer"} {
		if code := bearerPost(t, ts.URL, hdr); code != http.StatusUnauthorized {
			t.Errorf("Authorization=%q: status = %d; want 401 — only the Bearer scheme carries a grant", hdr, code)
		}
	}
}

func TestGrantAuth_XCogosGrantWinsOverBearer(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	token := mintTestGrant(t, srv, "both-surface")

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/attention", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(GrantHeaderName, token)
	req.Header.Set("Authorization", "Bearer bogus")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("401 when a valid X-Cogos-Grant is present alongside a bogus Bearer — canonical header must win")
	}
}

func TestGrantAuth_XApiKeyWithValidGrant_PassesGate(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	token := mintTestGrant(t, srv, "apikey-surface")

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/attention", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("401 with a valid grant in x-api-key — an Anthropic-SDK client (dsh's Anthropic provider) cannot get past the gate")
	}

	// Bogus x-api-key must still be 401: same verification, not a bypass.
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/attention", bytes.NewBufferString(`{}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-api-key", "not-a-grant")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d with bogus x-api-key; want 401", resp2.StatusCode)
	}
}

// ─── ledger L03: identity read routes are gated ──────────────────────────────
//
// Before this change, isGrantExemptRequest exempted EVERY GET except /mcp
// from the gate. Two of the identity routes are GETs, and one of them —
// GET /v1/identity/grants/current — returned a live grant's raw token in its
// response body. So any process that could open a loopback socket could read
// the node-root token (the kernel's admin credential, which grantHasScope
// treats as root authority) with no credential of its own, and then mint,
// revoke, and write with full authority. That made every other check in
// serve_grant_auth.go decorative.
//
// NEGATIVE CONTROL: on the pre-change code these routes answered 200 with a
// token in the body; the assertions below fail there.

// TestGrantAuth_IdentityReadRoutes_UnauthenticatedGet_401 is the L03 tooth:
// unauthenticated GET on the identity read routes must be rejected at the
// gate. /v1/identity/grants/current is included in its OLD (GET) shape too,
// so this test also pins that the pre-change verb cannot be resurrected as
// an exempt path by accident.
func TestGrantAuth_IdentityReadRoutes_UnauthenticatedGet_401(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// A live node-root grant exists — the leak this closes is precisely
	// "there IS a token to hand out", so the registry must be populated for
	// the test to be meaningful.
	mintTestGrant(t, srv, nodeRootSurface)

	paths := []string{
		"/v1/identity/grants",
		"/v1/identity/grants/current?surface=node-root",
		"/v1/identity/grants/current",
	}
	for _, path := range paths {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			// Deliberately does NOT print the body — on the old code it
			// contains a live raw token.
			t.Errorf("GET %s = %d; want 401 (unauthenticated identity read must be rejected at the gate)",
				path, resp.StatusCode)
		}
		if bytes.Contains(body, []byte(`"token"`)) {
			t.Errorf("GET %s response body carries a \"token\" field — a raw grant escaped through an unauthenticated read", path)
		}
	}
}

// TestGrantAuth_IdentityGrantCurrent_IsPostBehindGate covers the surviving
// half of the L03 ruling ("keep the zero-paste primitive if it can be made a
// POST behind a grant"): the primitive still works, it just requires a grant
// and a POST now.
func TestGrantAuth_IdentityGrantCurrent_IsPostBehindGate(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	nodeRootToken := mintTestGrant(t, srv, nodeRootSurface)

	// Unauthenticated POST -> 401 at the gate.
	unauth, err := http.Post(ts.URL+"/v1/identity/grants/current?surface=node-root",
		"application/json", nil)
	if err != nil {
		t.Fatalf("unauthenticated POST current: %v", err)
	}
	unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST current = %d; want 401", unauth.StatusCode)
	}

	// Authenticated POST -> 200, and returns the same live token.
	req, err := http.NewRequest(http.MethodPost,
		ts.URL+"/v1/identity/grants/current?surface=node-root", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(GrantHeaderName, nodeRootToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticated POST current: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated POST current = %d; want 200 — the zero-paste primitive must survive the gating", resp.StatusCode)
	}
	var out identityGrantMintResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Token != nodeRootToken {
		t.Fatal("POST current did not return the live node-root token to an authenticated caller")
	}
}

// TestGrantAuth_NonIdentityGetRoutes_StillExempt pins the blast radius of the
// L03 carve-out: it removes GET exemption for /v1/identity/* ONLY. Every
// other read stays exempt, so no dashboard/observability consumer regresses.
func TestGrantAuth_NonIdentityGetRoutes_StillExempt(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/health", "/v1/manifest", "/v1/context", "/v1/ledger", "/v1/vitals"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("GET %s = 401; the identity carve-out must not widen to other reads", path)
		}
	}
}

// ─── ledger L02: scope enforcement ───────────────────────────────────────────
//
// Before this change, IdentityGrant.Scope was recorded at mint time and never
// read: grantAuthMiddleware called VerifyAny, which asks only "is this token
// live". A grant minted for one narrow purpose therefore carried the
// authority of every purpose — a surface allowed to run inference could also
// mutate config, write cogdocs through /mcp, and mint itself grants for any
// other surface.
//
// NEGATIVE CONTROL: on the pre-change code every case below reached its
// handler (non-403), so each assertion fails there.

// TestGrantAuth_InferenceScopedGrant_DeniedOnWriteRoute is the L02 tooth
// named in the ledger: a grant minted with scope ["inference"] is denied on a
// write route.
func TestGrantAuth_InferenceScopedGrant_DeniedOnWriteRoute(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	token := mintScopedTestGrant(t, srv, "inference-only-surface", ScopeInference)

	req, err := http.NewRequest(http.MethodPatch, ts.URL+"/v1/config",
		bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(GrantHeaderName, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH /v1/config: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PATCH /v1/config with an inference-scoped grant = %d; want 403 insufficient_scope", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["type"] != "insufficient_scope" {
		t.Errorf("error.type = %v; want insufficient_scope", errObj["type"])
	}
}

// TestGrantAuth_ScopeMatrix walks the (grant scope × route) grid so a future
// edit that widens one scope's reach fails here rather than in production.
func TestGrantAuth_ScopeMatrix(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	type routeCase struct {
		name   string
		method string
		path   string
		// want is the scope this route demands.
		want string
	}
	routes := []routeCase{
		{"chat-completions", http.MethodPost, "/v1/chat/completions", ScopeInference},
		{"anthropic-messages", http.MethodPost, "/v1/messages", ScopeInference},
		{"config-patch", http.MethodPatch, "/v1/config", ScopeWrite},
		{"config-rollback", http.MethodPost, "/v1/config/rollback", ScopeWrite},
		{"mcp", http.MethodPost, "/mcp", ScopeWrite},
		{"grant-mint", http.MethodPost, "/v1/identity/grants", ScopeAdmin},
		{"grant-list", http.MethodGet, "/v1/identity/grants", ScopeAdmin},
		{"grant-current", http.MethodPost, "/v1/identity/grants/current?surface=node-root", ScopeAdmin},
	}
	scopes := []string{ScopeInference, ScopeWrite, ScopeAdmin}

	// One grant per scope, each on its own surface so MintOrReuse doesn't
	// supersede a sibling.
	tokenForScope := map[string]string{}
	for _, sc := range scopes {
		tokenForScope[sc] = mintScopedTestGrant(t, srv, "matrix-"+sc, sc)
	}
	// Plus the kernel's own root credential, which must reach everything.
	nodeRootToken := mintScopedTestGrant(t, srv, nodeRootSurface, nodeRootScope)

	// deniedForScope reports whether the GATE rejected this request for
	// scope. It keys on the error TYPE, not the status code alone: some of
	// these handlers answer 403 on their own account (PATCH /v1/config hits
	// requireConfigMutation's 403 when EnableConfigMutation is off), and a
	// status-only check could not tell "the gate stopped it" from "the
	// handler ran and declined" — which would make the matching-scope half
	// of this matrix a false failure.
	deniedForScope := func(t *testing.T, rc routeCase, token string) bool {
		t.Helper()
		req, err := http.NewRequest(rc.method, ts.URL+rc.path, bytes.NewBufferString(`{}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(GrantHeaderName, token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", rc.method, rc.path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			return false
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return false
		}
		errObj, _ := body["error"].(map[string]any)
		return errObj["type"] == "insufficient_scope"
	}

	for _, rc := range routes {
		for _, sc := range scopes {
			denied := deniedForScope(t, rc, tokenForScope[sc])
			if sc == rc.want {
				if denied {
					t.Errorf("%s with the matching %q scope was denied insufficient_scope; the gate must not reject a correctly-scoped grant", rc.name, sc)
				}
			} else if !denied {
				t.Errorf("%s with a %q-scoped grant was NOT denied; want 403 insufficient_scope (route requires %q)",
					rc.name, sc, rc.want)
			}
		}
		// node-root retains all scopes.
		if deniedForScope(t, rc, nodeRootToken) {
			t.Errorf("%s with the node-root grant was denied insufficient_scope; node-root must retain every scope", rc.name)
		}
	}
}

// TestGrantAuth_EmptyScopeGrant_DeniedOnScopedRoute pins the fail-closed
// direction: a grant with no scopes at all reaches no scoped route. A
// fail-open reading of grantHasScope ("empty means unrestricted") would make
// every pre-L02 ledger row a skeleton key.
func TestGrantAuth_EmptyScopeGrant_DeniedOnScopedRoute(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	token := mintScopedTestGrant(t, srv, "no-scope-surface")

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(GrantHeaderName, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("scopeless grant on an inference route = %d; want 403 insufficient_scope", resp.StatusCode)
	}
}

// TestGrantAuth_UnscopedRoutes_TakeAnyLiveGrant pins the other half of the
// blast radius: L02 only ADDS requirements on the routes
// requiredScopeForRequest names. Every other gated write route keeps its
// pre-change behavior — any live grant passes.
func TestGrantAuth_UnscopedRoutes_TakeAnyLiveGrant(t *testing.T) {
	t.Parallel()
	srv := newGrantAuthTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	token := mintScopedTestGrant(t, srv, "narrow-surface", ScopeInference)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/attention",
		bytes.NewBufferString(`{}`))
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

	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("POST /v1/attention = 403 with a live grant; L02 must not add a scope requirement to unclassified routes")
	}
}

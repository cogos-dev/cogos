package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORSPreflight_OKStatus verifies that an OPTIONS preflight returns
// 204 No Content with the expected Access-Control-* headers. The handler
// under test is the wrapped server handler (mux + CORS middleware).
func TestCORSPreflight_OKStatus(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodOptions, "/v1/channel-sessions/register", nil)
	req.Header.Set("Origin", "http://localhost:7860")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Content-Type")

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d; want 204", w.Code)
	}
	// All allow-* headers should be present on the preflight response.
	wantHeaders := map[string]string{
		"Access-Control-Allow-Methods": "GET, POST, PATCH, PUT, DELETE, OPTIONS",
		"Access-Control-Allow-Headers": "Content-Type, Mcp-Session-Id, X-Workspace-Root, Authorization",
		"Access-Control-Max-Age":       "86400",
	}
	for h, want := range wantHeaders {
		if got := w.Header().Get(h); got != want {
			t.Errorf("%s = %q; want %q", h, got, want)
		}
	}
}

// TestCORSPreflight_AllowsBrowserOrigin verifies that a preflight from a
// loopback dashboard origin echoes the Origin header (not "*") so
// credentialed requests can round-trip, and sets Allow-Credentials.
func TestCORSPreflight_AllowsBrowserOrigin(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	for _, origin := range []string{
		"http://localhost:7860",
		"http://127.0.0.1:6931",
		"http://localhost",
	} {
		origin := origin
		t.Run(origin, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodOptions, "/v1/manifest", nil)
			req.Header.Set("Origin", origin)
			req.Header.Set("Access-Control-Request-Method", "GET")

			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if got := w.Header().Get("Access-Control-Allow-Origin"); got != origin {
				t.Errorf("Allow-Origin = %q; want %q (echo for loopback)", got, origin)
			}
			if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
				t.Errorf("Allow-Credentials = %q; want %q (echo origin enables credentials)", got, "true")
			}
			if got := w.Header().Get("Vary"); got != "Origin" {
				t.Errorf("Vary = %q; want %q", got, "Origin")
			}
		})
	}
}

// TestCORSPreflight_NonLoopbackOriginGetsStar verifies the widening
// fallback for non-loopback origins (future pod/Tailnet deployments).
// Note: the kernel binds loopback by default, so this path is mostly
// about not breaking future non-dev deployments.
func TestCORSPreflight_NonLoopbackOriginGetsStar(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodOptions, "/v1/manifest", nil)
	req.Header.Set("Origin", "http://evil.example.com")

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q; want %q (widen for non-loopback)", got, "*")
	}
	// Allow-Credentials must not be set for star origin.
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Allow-Credentials = %q; want empty (star + credentials is illegal)", got)
	}
}

// TestCORSHeaders_OnRealGET verifies that a real (non-preflight) GET
// response still carries Access-Control-Allow-Origin when an Origin
// header is present. Uses /v1/manifest which is known to return 200.
func TestCORSHeaders_OnRealGET(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/manifest", nil)
	req.Header.Set("Origin", "http://localhost:7860")

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:7860" {
		t.Errorf("Allow-Origin = %q; want echo of loopback origin", got)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q; want application/json (handler must still run)",
			w.Header().Get("Content-Type"))
	}
}

// TestCORSHeaders_NoOriginNoChange verifies that requests without an
// Origin header (curl, MCP CLI, same-origin fetches) are untouched —
// no CORS headers are added, and the underlying handler still runs.
func TestCORSHeaders_NoOriginNoChange(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/manifest", nil)
	// Deliberately no Origin header.

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q; want empty (no Origin header should skip CORS)", got)
	}
}

// TestIsLoopbackOrigin_Table unit-tests the pure helper so any future
// change to the allow-policy is easy to catch.
func TestIsLoopbackOrigin_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		origin string
		want   bool
	}{
		{"http://localhost", true},
		{"http://localhost:7860", true},
		{"http://127.0.0.1", true},
		{"http://127.0.0.1:6931", true},
		{"https://localhost:8443", true},
		{"https://127.0.0.1:8443", true},
		// IPv6 loopback — browsers serialize the bracketed canonical form.
		{"http://[::1]", true},
		{"http://[::1]:6931", true},
		{"https://[::1]:8443", true},
		{"http://[::2]", false},
		{"http://[::1]evil.example.com", false},
		{"http://[2001:db8::1]:6931", false},
		{"http://[0:0:0:0:0:0:0:1]", false}, // non-canonical; Origin is canonical
		{"http://evil.example.com", false},
		{"http://192.168.1.2:7860", false},
		{"", false},
		{"null", false},
		{"file://", false},
	}
	for _, tc := range cases {
		if got := isLoopbackOrigin(tc.origin); got != tc.want {
			t.Errorf("isLoopbackOrigin(%q) = %v; want %v", tc.origin, got, tc.want)
		}
	}
}

// ── content/credential routes: loopback-only CORS (#507 rounds 4 + 5) ──

// TestIsLoopbackOnlyPath covers the prefix matcher for the corsLoopbackOnly
// tier. As with isDebugPath the negative cases carry the weight: a bare
// strings.HasPrefix("/v1/ledger") would also swallow an unrelated future
// /v1/ledgering route and silently downgrade its CORS. The /v1/context pair
// pins the tier edge inside one route family: the GET fovea list (paths and
// scores) stays permissive while the rendered-text sibling is tiered.
func TestIsLoopbackOnlyPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		// Round 4 originals.
		{"/v1/conversation", true},
		{"/v1/ledger", true},
		{"/v1/events", true},
		{"/v1/events/stream", true},
		{"/v1/tool-calls", true},
		{"/v1/conversation/", true},
		// Round 5 sweep.
		{"/v1/cogdoc/read", true},
		{"/v1/cogdoc", true},
		{"/memory/read", true},
		{"/memory/search", true},
		{"/v1/bus/somebus/events", true},
		{"/v1/bus/events", true},
		{"/v1/sessions", true},
		{"/v1/sessions/abc/context", true},
		{"/v1/handoffs", true},
		{"/v1/blocks/manifest", true},
		{"/v1/blobs/sha256:abc", true},
		{"/v1/claude-code/projects", true},
		{"/v1/identity/grants/current", true},
		{"/v1/config", true},
		{"/v1/dispatch-jobs/j1", true},
		{"/v1/agents/root", true},
		{"/v1/agent/traces", true},
		{"/v1/skills/foo/exec", true},
		{"/mcp", true},
		{"/v1/chat/completions", true},
		{"/v1/messages", true},
		{"/v1/context/foveated", true},
		{"/v1/peer-awareness", true},
		// Round 6: run-log surfaces whose rows carry model-emitted content
		// (ToolArgs in proprioceptive.jsonl, prompt/query previews in the
		// kernel slog; /v1/traces includes the proprioceptive source via
		// source=all).
		{"/v1/proprioceptive", true},
		{"/v1/traces", true},
		{"/v1/kernel-log", true},
		// The tier edge inside /v1/context.
		{"/v1/context", false},
		{"/v1/context/", false},
		// Telemetry and metadata stay permissive.
		{"/v1/manifest", false},
		{"/metrics", false},
		{"/v1/vitals", false},
		{"/v1/hud/state", false},
		{"/v1/resolve", false},
		{"/v1/uri/resolve", false},
		{"/v1/channel-sessions", false},
		{"/v1/channels/ch1/peers", false},
		{"/v1/constellation/fovea", false},
		{"/v1/observer/state", false},
		{"/v1/services", false},
		{"/health", false},
		// Separator discipline: string-prefixed but different routes.
		{"/v1/ledgering", false},
		{"/v1/eventsource", false},
		{"/v1/conversations", false},
		{"/v1/session", false},
		{"/v1/agentx", false},
		{"/v1/configs", false},
		{"/v1/kernel/rates", false}, // sibling of /v1/kernel-log, stays META
		{"/v1/tracesx", false},
		{"/mcpx", false},
		{"/memoryx", false},
		{"/", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isLoopbackOnlyPath(tc.path); got != tc.want {
			t.Errorf("isLoopbackOnlyPath(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}

// TestCORSPolicyForPath pins the tier assignment itself, so a future edit that
// moves a path between isDebugPath and isSessionDataPath is visible.
func TestCORSPolicyForPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want corsPolicy
	}{
		{"/debug/pprof/heap", corsNone},
		{"/v1/debug/last", corsNone},
		{"/v1/conversation", corsLoopbackOnly},
		{"/v1/ledger", corsLoopbackOnly},
		{"/v1/events", corsLoopbackOnly},
		{"/v1/tool-calls", corsLoopbackOnly},
		{"/v1/cogdoc/read", corsLoopbackOnly},
		{"/v1/identity/grants/current", corsLoopbackOnly},
		{"/v1/blocks/manifest", corsLoopbackOnly},
		{"/mcp", corsLoopbackOnly},
		{"/v1/context/foveated", corsLoopbackOnly},
		{"/v1/proprioceptive", corsLoopbackOnly},
		{"/v1/traces", corsLoopbackOnly},
		{"/v1/kernel-log", corsLoopbackOnly},
		{"/v1/context", corsPermissive},
		{"/v1/manifest", corsPermissive},
		{"/v1/channel-sessions/register", corsPermissive},
		{"/metrics", corsPermissive},
		{"/v1/vitals", corsPermissive},
		{"/", corsPermissive},
	}
	for _, tc := range cases {
		if got := corsPolicyForPath(tc.path); got != tc.want {
			t.Errorf("corsPolicyForPath(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}

// sessionDataTestRoutes are the corsLoopbackOnly routes exercised end-to-end
// through the wrapped handler with handler-level assertions (JSON answers,
// no 403). Routes whose handlers need setup (MCP sessions, live buses) or
// hold the connection (SSE streams) are covered by the header-only loop in
// TestLoopbackOnlyTier_AllPrefixes and the unit table above instead.
var sessionDataTestRoutes = []string{
	"/v1/conversation",
	"/v1/ledger",
	"/v1/events",
	"/v1/tool-calls",
	"/v1/cogdoc/read",
	// Round 6: run-log surfaces whose rows carry model-emitted tool args
	// and prompt/query previews. All three answer 200 JSON on a fresh
	// workspace (absent files → empty entries), so the handler-level
	// assertions hold without setup.
	"/v1/proprioceptive",
	"/v1/traces",
	"/v1/kernel-log",
}

// TestSessionDataRoutes_RemoteOriginGetsNoAllowOrigin is the regression test
// for the round-4 finding. Before the fix, corsMiddleware's non-loopback "*"
// fallback meant any page the operator visited could run
// fetch('http://127.0.0.1:6931/v1/ledger').then(r => r.json()) — a simple GET,
// no preflight — and read the full cross-session event ledger, or hit
// /v1/conversation for the live session's untruncated turn text. Without an
// Access-Control-Allow-Origin the browser refuses the read.
func TestSessionDataRoutes_RemoteOriginGetsNoAllowOrigin(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	for _, path := range sessionDataTestRoutes {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:9999"
		req.Header.Set("Origin", "http://evil.example.com")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q; want empty so a cross-origin page cannot read the body", path, got)
		}
		if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Credentials = %q; want empty", path, got)
		}
		if got := w.Header().Get("Vary"); got != "Origin" {
			t.Errorf("%s: Vary = %q; want %q so a cache cannot replay a loopback-blessed response", path, got, "Origin")
		}
	}
}

// TestSessionDataRoutes_PreflightRemoteOriginGetsNoAllowOrigin covers the
// non-simple-request path: a preflight from a remote origin must not be
// answered with an allow-origin, or the follow-up request would be permitted
// to read the body.
func TestSessionDataRoutes_PreflightRemoteOriginGetsNoAllowOrigin(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	for _, path := range sessionDataTestRoutes {
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", "http://evil.example.com")
		req.Header.Set("Access-Control-Request-Method", "GET")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: preflight Access-Control-Allow-Origin = %q; want empty", path, got)
		}
		if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("%s: preflight Access-Control-Allow-Credentials = %q; want empty", path, got)
		}
	}
}

// TestSessionDataRoutes_LoopbackOriginStillAllowed is the other half of the
// contract: the fix must not weaken the loopback allowance that local UIs on
// other ports (mod3 on :7860, the constellation surfaces) rely on. A loopback
// Origin is still echoed, credentials are still advertised, and the handler
// still runs.
func TestSessionDataRoutes_LoopbackOriginStillAllowed(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	for _, path := range sessionDataTestRoutes {
		for _, origin := range []string{"http://localhost:7860", "http://127.0.0.1:6931", "http://[::1]:6931"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "127.0.0.1:9999"
			req.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if got := w.Header().Get("Access-Control-Allow-Origin"); got != origin {
				t.Errorf("%s from %s: Allow-Origin = %q; want the echoed loopback origin", path, origin, got)
			}
			if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
				t.Errorf("%s from %s: Allow-Credentials = %q; want %q", path, origin, got, "true")
			}
			if w.Code == http.StatusForbidden {
				t.Errorf("%s from %s: got 403 — the loopback-only CORS tier must not gate the handler", path, origin)
			}
		}
	}
}

// TestSessionDataRoutes_SameOriginAndCLIUnaffected pins that dropping the star
// fallback costs same-origin browser clients and non-browser clients nothing:
// with no Origin header there was never an Access-Control-Allow-Origin to
// begin with, and the handler must still answer.
func TestSessionDataRoutes_SameOriginAndCLIUnaffected(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	for _, path := range sessionDataTestRoutes {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:9999"
		// Same-origin browser fetch: Referer, no Origin. Also the curl /
		// MCP-CLI shape.
		req.Header.Set("Referer", "http://127.0.0.1:6931/")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: Allow-Origin = %q; want empty (no Origin header)", path, got)
		}
		if w.Code == http.StatusForbidden {
			t.Errorf("%s: got 403 — same-origin and CLI clients must still reach the handler", path)
		}
		if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s: Content-Type = %q; want the handler's JSON response", path, w.Header().Get("Content-Type"))
		}
	}
}

// TestLoopbackOnlyTier_AllPrefixes sweeps EVERY prefix in
// loopbackOnlyPrefixes end-to-end with header-only assertions, so a prefix
// listed in the tier cannot silently fail to take effect (e.g. a typo'd
// entry). Handler status is deliberately not asserted — some handlers 400 or
// 404 without setup, some (config) 403 when their feature is disabled; the
// middleware sets its headers before the handler runs either way.
func TestLoopbackOnlyTier_AllPrefixes(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	for _, prefix := range loopbackOnlyPrefixes {
		// Remote origin: no allow-origin, ever.
		req := httptest.NewRequest(http.MethodGet, prefix, nil)
		req.RemoteAddr = "127.0.0.1:9999"
		req.Header.Set("Origin", "http://evil.example.com")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s from remote origin: Allow-Origin = %q; want empty", prefix, got)
		}
		if got := w.Header().Get("Vary"); got != "Origin" {
			t.Errorf("%s from remote origin: Vary = %q; want %q", prefix, got, "Origin")
		}

		// Loopback origin: still echoed.
		req2 := httptest.NewRequest(http.MethodGet, prefix, nil)
		req2.RemoteAddr = "127.0.0.1:9999"
		req2.Header.Set("Origin", "http://localhost:7860")
		w2 := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w2, req2)
		if got := w2.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:7860" {
			t.Errorf("%s from loopback origin: Allow-Origin = %q; want the echoed origin", prefix, got)
		}
	}
}

// TestCORSPermissiveRoutesKeepStarFallback guards the blast radius: the tier
// split must not change anything for the routes that are not content or
// credential surfaces. /v1/channel-sessions/register is the mod3 dashboard's
// cross-origin POST that motivated this middleware in the first place.
func TestCORSPermissiveRoutesKeepStarFallback(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	for _, path := range []string{"/v1/manifest", "/health"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", "http://evil.example.com")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s: Allow-Origin = %q; want %q (permissive tier unchanged)", path, got, "*")
		}
	}
}

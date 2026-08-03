package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── isLoopbackRemoteAddr ─────────────────────────────────────────────────

func TestIsLoopbackRemoteAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		addr string
		want bool
	}{
		{"ipv4 loopback with port", "127.0.0.1:54321", true},
		{"ipv6 loopback with port", "[::1]:54321", true},
		{"bare ipv4 loopback, no port", "127.0.0.1", true},
		{"httptest default remote addr", "192.0.2.1:1234", false},
		{"private LAN address", "10.0.0.5:9000", false},
		{"public address", "8.8.8.8:443", false},
		{"empty string", "", false},
		{"garbage", "not-an-address", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isLoopbackRemoteAddr(tc.addr); got != tc.want {
				t.Errorf("isLoopbackRemoteAddr(%q) = %v; want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// ── debugLoopbackOnly middleware ─────────────────────────────────────────
//
// newDebugRequest builds a request shaped like a legitimate CLI/curl caller:
// loopback RemoteAddr, the required debugAuthHeader, no Origin, no Referer.
// Individual tests mutate one dimension at a time so each failure mode in
// the four-layer guard (serve_debug.go) has a dedicated assertion.
func newDebugRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set(debugAuthHeader, "1")
	return req
}

func TestDebugLoopbackOnly_AllowsLegitimateRequest(t *testing.T) {
	t.Parallel()
	called := false
	h := debugLoopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	for _, addr := range []string{"127.0.0.1:54321", "[::1]:54321"} {
		called = false
		req := newDebugRequest()
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("RemoteAddr=%q: status = %d; want 200", addr, w.Code)
		}
		if !called {
			t.Errorf("RemoteAddr=%q: inner handler was not invoked for a legitimate caller", addr)
		}
	}
}

func TestDebugLoopbackOnly_RejectsNonLoopback(t *testing.T) {
	t.Parallel()
	called := false
	h := debugLoopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	// Otherwise-legitimate request (header set, no Origin/Referer) but from
	// a non-loopback RemoteAddr — exactly the shape of a real remote caller
	// if the daemon's bind address were ever widened.
	req := newDebugRequest()
	req.RemoteAddr = "192.0.2.1:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", w.Code)
	}
	if called {
		t.Error("inner handler was invoked for a non-loopback caller")
	}
}

func TestDebugLoopbackOnly_RejectsOriginHeader(t *testing.T) {
	t.Parallel()
	called := false
	h := debugLoopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	// RemoteAddr=127.0.0.1 (the operator's own browser satisfies this too),
	// header present, but an Origin header — the shape of a cross-origin
	// browser fetch(), CORS or no-cors. Must be rejected regardless of the
	// loopback RemoteAddr; this is the exact gap #507 flagged.
	req := newDebugRequest()
	req.Header.Set("Origin", "http://evil.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 for a request carrying an Origin header", w.Code)
	}
	if called {
		t.Error("inner handler was invoked for a request carrying an Origin header")
	}
}

func TestDebugLoopbackOnly_RejectsRefererHeader(t *testing.T) {
	t.Parallel()
	called := false
	h := debugLoopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := newDebugRequest()
	req.Header.Set("Referer", "http://evil.example.com/attack.html")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 for a request carrying a Referer header", w.Code)
	}
	if called {
		t.Error("inner handler was invoked for a request carrying a Referer header")
	}
}

func TestDebugLoopbackOnly_RejectsMissingAuthHeader(t *testing.T) {
	t.Parallel()
	called := false
	h := debugLoopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	// Loopback RemoteAddr, no Origin/Referer — but no debugAuthHeader
	// either. This is exactly what a plain <img src="...pprof/profile">
	// or a no-cors fetch() produces: no custom header, browsers won't let
	// page script add one to these request shapes.
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 for a request missing %s", w.Code, debugAuthHeader)
	}
	if called {
		t.Error("inner handler was invoked for a request missing the auth header")
	}
}

// ── Route registration + end-to-end guard behavior via the full server ──

func TestDebugRoutes_RegisteredInManifest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	wantPaths := []string{
		"/debug/pprof/",
		"/debug/pprof/heap",
		"/debug/pprof/goroutine",
		"/debug/pprof/allocs",
		"/debug/pprof/mutex",
		"/debug/pprof/block",
		"/debug/vars",
	}
	seen := make(map[string]bool, len(srv.httpRoutes))
	for _, r := range srv.httpRoutes {
		seen[r.Path] = true
	}
	for _, p := range wantPaths {
		if !seen[p] {
			t.Errorf("route %q not found in s.httpRoutes (manifest would omit it)", p)
		}
	}
}

func TestDebugRoutes_NonLoopbackCallerGets403(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	for _, path := range []string{"/debug/pprof/heap", "/debug/pprof/goroutine", "/debug/vars"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(debugAuthHeader, "1")
		// Non-loopback RemoteAddr — httptest's default is already
		// non-loopback (192.0.2.1), but set it explicitly so the intent is
		// unambiguous regardless of httptest's internal default.
		req.RemoteAddr = "203.0.113.7:4444"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d; want 403 for non-loopback caller", path, w.Code)
		}
	}
}

func TestDebugRoutes_OriginBearingRequestGets403(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// The attack scenario from #507: RemoteAddr is loopback (the operator's
	// own browser), the auth header would never be present from a browser
	// anyway, but even granting it here, an Origin header alone must still
	// 403 the request end-to-end through the real server handler.
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	req.Header.Set(debugAuthHeader, "1")
	req.Header.Set("Origin", "http://evil.example.com")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 for an Origin-bearing loopback request", w.Code)
	}
}

func TestDebugRoutes_MissingAuthHeaderGets403(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Loopback RemoteAddr, no Origin/Referer, but no debugAuthHeader —
	// exactly what a plain <img> tag or no-cors fetch from the operator's
	// own browser produces.
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 for a request with no %s header", w.Code, debugAuthHeader)
	}
}

func TestDebugRoutes_LoopbackCallerReachesHandler(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	req.Header.Set(debugAuthHeader, "1")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q; want application/json (expvar.Handler's own contract)", ct)
	}
}

func TestDebugRoutes_HeapProfileLoopback(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	req.Header.Set(debugAuthHeader, "1")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Error("heap profile body is empty")
	}
}

// TestDebugRoutes_NoAccessControlHeadersEver verifies the corsMiddleware
// exclusion (serve_cors.go): /debug/ responses never carry
// Access-Control-Allow-Origin, even when the request supplies an Origin
// header and otherwise passes the loopback+auth-header gate. This is the
// second half of the #507 fix — the guard alone doesn't help if the
// surrounding middleware still tells the browser it's allowed to read the
// response.
func TestDebugRoutes_NoAccessControlHeadersEver(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	req.Header.Set(debugAuthHeader, "1")
	req.Header.Set("Origin", "http://localhost:7860") // even a loopback-echoed origin
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// The request 403s (Origin is rejected outright by debugLoopbackOnly),
	// but the important assertion is independent of status code: no
	// Access-Control-* header should ever appear on a /debug/ response.
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q; want empty on /debug/ responses", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q; want empty on /debug/ responses", got)
	}

	// Also verify the success path (no Origin, correct header) still emits
	// no CORS headers — the exclusion is unconditional, not just an
	// error-path side effect.
	req2 := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	req2.RemoteAddr = "127.0.0.1:9999"
	req2.Header.Set(debugAuthHeader, "1")
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w2.Code)
	}
	if got := w2.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q; want empty on a successful /debug/ response", got)
	}

	// And an OPTIONS preflight against /debug/ must not get the CORS
	// preflight treatment either — no Access-Control-Allow-Methods/Headers,
	// since /debug/ never registers OPTIONS and the middleware is excluded.
	req3 := httptest.NewRequest(http.MethodOptions, "/debug/pprof/heap", nil)
	req3.Header.Set("Origin", "http://evil.example.com")
	req3.Header.Set("Access-Control-Request-Method", "GET")
	w3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w3, req3)

	if got := w3.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q; want empty on a /debug/ OPTIONS response", got)
	}
	if got := w3.Header().Get("Access-Control-Allow-Methods"); got != "" {
		t.Errorf("Access-Control-Allow-Methods = %q; want empty on a /debug/ OPTIONS response", got)
	}
}

// ── /v1/debug/* CORS exclusion (#507 review round 3) ─────────────────────

// TestIsDebugPath covers the prefix matcher that decides which paths bypass
// corsMiddleware. The negative cases matter as much as the positive ones: a
// bare strings.HasPrefix("/debug") would also swallow unrelated future routes
// like /debugger or /v1/debugging, silently dropping their CORS headers.
func TestIsDebugPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"/debug", true},
		{"/debug/", true},
		{"/debug/pprof/heap", true},
		{"/debug/vars", true},
		{"/v1/debug", true},
		{"/v1/debug/", true},
		{"/v1/debug/last", true},
		{"/v1/debug/context", true},
		{"/v1/chat/completions", false},
		{"/v1/models", false},
		{"/", false},
		{"/debugger", false},
		{"/v1/debugging", false},
		{"/v1/debugger/last", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isDebugPath(tc.path); got != tc.want {
			t.Errorf("isDebugPath(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}

// TestV1DebugRoutes_NoAccessControlHeadersEver is the sibling of
// TestDebugRoutes_NoAccessControlHeadersEver, for the pre-existing
// /v1/debug/ endpoints. /v1/debug/last returns the extracted query text of
// the last chat request plus injected cogdoc paths; before this change
// corsMiddleware's non-loopback "*" fallback let any cross-origin page read
// that body with a plain simple GET (no preflight, since it's a simple
// request). The exclusion removes the header the browser needs to permit
// that read.
func TestV1DebugRoutes_NoAccessControlHeadersEver(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	for _, path := range []string{"/v1/debug/last", "/v1/debug/context"} {
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
		if got := w.Header().Get("Access-Control-Allow-Methods"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Methods = %q; want empty", path, got)
		}
	}
}

// TestV1DebugRoutes_SameOriginConsumersStillReachHandler pins the deliberate
// asymmetry with /debug/pprof: /v1/debug/ gets the CORS exclusion but NOT
// debugLoopbackOnly, because the kernel's own dashboard (GET /) and canvas
// (GET /canvas) fetch these routes same-origin with `API =
// window.location.origin`. A same-origin browser fetch sends a Referer and no
// custom header — exactly the shape debugLoopbackOnly rejects — so applying
// that guard here would 403 the dashboard. The handler must still be reached.
func TestV1DebugRoutes_SameOriginConsumersStillReachHandler(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	for _, path := range []string{"/v1/debug/last", "/v1/debug/context"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:9999"
		// What a same-origin dashboard fetch actually looks like: a Referer,
		// no Origin, and no X-Cogos-Debug header.
		req.Header.Set("Referer", "http://127.0.0.1:6931/")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		// The test server has served no chat request, so the handler answers
		// 404 "no requests yet". The point is that it is the *handler*
		// answering, not debugLoopbackOnly's 403.
		if w.Code == http.StatusForbidden {
			t.Errorf("%s: got 403 — debugLoopbackOnly must not gate /v1/debug/, it would break the same-origin dashboard", path)
		}
		if w.Code != http.StatusOK && w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d; want 200 or 404 from the handler", path, w.Code)
		}
		if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s: Content-Type = %q; want the handler's JSON response", path, w.Header().Get("Content-Type"))
		}
	}
}

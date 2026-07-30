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

func TestDebugLoopbackOnly_RejectsNonLoopback(t *testing.T) {
	t.Parallel()
	called := false
	h := debugLoopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	// httptest.NewRequest defaults RemoteAddr to "192.0.2.1:1234", a
	// non-loopback TEST-NET-1 address — exactly the shape of a real
	// non-loopback caller if the daemon's bind address were ever widened.
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", w.Code)
	}
	if called {
		t.Error("inner handler was invoked for a non-loopback caller")
	}
}

func TestDebugLoopbackOnly_AllowsLoopback(t *testing.T) {
	t.Parallel()
	called := false
	h := debugLoopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	for _, addr := range []string{"127.0.0.1:54321", "[::1]:54321"} {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("RemoteAddr=%q: status = %d; want 200", addr, w.Code)
		}
		if !called {
			t.Errorf("RemoteAddr=%q: inner handler was not invoked for a loopback caller", addr)
		}
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

func TestDebugRoutes_LoopbackCallerReachesHandler(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	req.RemoteAddr = "127.0.0.1:9999"
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
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Error("heap profile body is empty")
	}
}

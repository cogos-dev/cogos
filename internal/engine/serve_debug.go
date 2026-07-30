// serve_debug.go — localhost-only pprof + expvar diagnostic surface.
//
// Filed against #505: a 36GB phys_footprint leak (459 OS threads, 59
// minutes to jetsam-kill) went undiagnosed because the daemon exposed no
// pprof surface — `/debug/pprof/*` 404s. "As it stands the kernel cannot be
// asked about its own memory." This file is that surface:
//
//	GET /debug/pprof/                 — index page
//	GET /debug/pprof/cmdline          — process argv (stdlib)
//	GET /debug/pprof/profile          — 30s CPU profile (stdlib)
//	GET /debug/pprof/symbol           — symbol lookup (stdlib)
//	GET /debug/pprof/trace            — execution trace (stdlib)
//	GET /debug/pprof/heap             — heap profile
//	GET /debug/pprof/goroutine        — goroutine stacks
//	GET /debug/pprof/allocs           — allocation profile
//	GET /debug/pprof/mutex            — mutex contention profile
//	GET /debug/pprof/block            — blocking-event profile
//	GET /debug/pprof/threadcreate     — OS-thread-creation profile
//	GET /debug/vars                   — expvar counters
//
// `go tool pprof http://127.0.0.1:6931/debug/pprof/heap` reads any of the
// profile endpoints directly. `cogos debug heap` / `cogos debug goroutines`
// (cli_debug.go) wrap the fetch-and-save step into one command for an
// operator or seat capturing evidence without a Go toolchain handy.
//
// Note: the mutex and block profiles are zero-rate by default (matches
// net/http/pprof's own upstream behavior — it never calls
// runtime.SetMutexProfileFraction / runtime.SetBlockProfileRate itself).
// Their endpoints exist and respond, but return empty profiles until those
// rates are set; enabling them by default here would add always-on runtime
// overhead to a daemon that #505 already found running hot, so that's left
// to a follow-up (or a future config flag) rather than baked in unasked.
//
// Security — loopback-only, twice over:
//
// The kernel already binds 127.0.0.1 by default (see Config.BindAddr /
// boot_bindaddr_warning.go), but that binding is opt-out — a config value
// an operator can widen for pod/LAN/Tailnet deployments, and #501/#505 both
// note the kernel has no HTTP auth-token surface to fall back on if they do.
// pprof handlers are a serious exposure if reachable non-locally: heap dumps
// can contain data, and /debug/pprof/profile is a cheap remote CPU-burn
// vector. debugLoopbackOnly is a second, per-request layer that holds
// independently of the bind address — even if BindAddr is later widened
// without anyone remembering this surface exists, a non-loopback caller
// still gets 403.
package engine

import (
	"expvar"
	"net"
	"net/http"
	"net/http/pprof"
)

// registerDebugRoutes mounts the pprof + expvar surface under /debug/ on
// mux, gated by debugLoopbackOnly. Called from NewServer alongside the
// other register*Routes calls.
func (s *Server) registerDebugRoutes(mux *http.ServeMux) {
	// Stdlib pprof handlers. The trailing-slash index also serves any named
	// profile not otherwise registered (e.g. threadcreate), same as the
	// classic net/http/pprof registration onto http.DefaultServeMux — the
	// explicit named routes below just make the common ones visible in
	// /v1/manifest and give tests a stable route to assert against.
	s.routeH(mux, "GET /debug/pprof/", debugLoopbackOnly(http.HandlerFunc(pprof.Index)))
	s.routeH(mux, "GET /debug/pprof/cmdline", debugLoopbackOnly(http.HandlerFunc(pprof.Cmdline)))
	s.routeH(mux, "GET /debug/pprof/profile", debugLoopbackOnly(http.HandlerFunc(pprof.Profile)))
	s.routeH(mux, "GET /debug/pprof/symbol", debugLoopbackOnly(http.HandlerFunc(pprof.Symbol)))
	s.routeH(mux, "GET /debug/pprof/trace", debugLoopbackOnly(http.HandlerFunc(pprof.Trace)))

	// Named runtime profiles, requested explicitly by #505: heap, goroutine,
	// allocs, mutex, block. threadcreate is included too — it's part of the
	// same runtime.MemProfile family and costs nothing extra to expose.
	for _, name := range []string{"heap", "goroutine", "allocs", "mutex", "block", "threadcreate"} {
		s.routeH(mux, "GET /debug/pprof/"+name, debugLoopbackOnly(pprof.Handler(name)))
	}

	// expvar counters (Go runtime memstats + cmdline + any process-registered
	// expvar.Publish vars).
	s.routeH(mux, "GET /debug/vars", debugLoopbackOnly(expvar.Handler()))
}

// debugLoopbackOnly wraps a handler so only a caller connecting from a
// loopback address reaches it, independent of what address the server
// itself is bound to. See the file-level doc comment for why this surface
// needs its own gate rather than relying solely on the bind address.
func debugLoopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRemoteAddr(r.RemoteAddr) {
			http.Error(w, "forbidden: debug endpoints are loopback-only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackRemoteAddr reports whether r.RemoteAddr — normally a "host:port"
// pair as net/http sets it from the accepted TCP connection — identifies a
// loopback caller (127.0.0.0/8 or ::1). Fails closed: an address that fails
// to parse, or parses but isn't loopback, is treated as non-loopback. Same
// fail-toward-the-safe-side posture as isLoopbackBindAddr in
// boot_bindaddr_warning.go, applied to the per-request caller address
// instead of the configured bind address.
func isLoopbackRemoteAddr(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

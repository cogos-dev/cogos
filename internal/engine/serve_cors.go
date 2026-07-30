// serve_cors.go — permissive CORS middleware for the kernel HTTP server.
//
// Rationale (Wave 5a, 2026-04-23):
//
// The browser-served dashboard (mod3 at http://localhost:7860) calls the
// kernel at http://127.0.0.1:6931 to register a channel-session via
// /v1/channel-sessions/register. Cross-origin POSTs from the dashboard
// trigger a CORS preflight (OPTIONS) that the kernel previously rejected —
// no Access-Control-* headers were emitted, the browser blocked the
// request, and the dashboard fell back to calling mod3 directly. That
// fallback works but leaves an ugly console error and defeats the point of
// kernel authority over session_id minting.
//
// This middleware wraps the entire mux. It:
//
//   - Short-circuits OPTIONS preflight with 204 No Content + the required
//     Access-Control-* headers.
//   - On every response, sets Access-Control-Allow-Origin based on the
//     request Origin header (echo for loopback origins, otherwise "*").
//   - Declares a permissive method/header set sufficient for the mod3
//     dashboard, MCP /mcp endpoint, and anything else currently calling
//     the kernel. Include Mcp-Session-Id because the MCP streamable-HTTP
//     transport relies on it.
//
// Policy choice — echo-for-loopback with star fallback:
//
// The kernel binds 127.0.0.1:6931 by default (loopback-only), so
// Allow-Origin: * is not a security boundary — any local process can hit
// the socket regardless. But echoing the Origin header when it matches
// a loopback scheme (http://localhost:* or http://127.0.0.1:*) is friendly
// to credentialed requests (if Allow-Origin is "*" the browser refuses to
// send cookies). Future auth layers may want that, so we do the echo now.
// For non-loopback origins we fall back to "*" so the middleware stays
// compatible with pod / Tailnet deployments when cfg.BindAddr widens.
//
// Request whose Origin header is missing (same-origin fetches, curl, the
// MCP CLI, etc.) are untouched — the middleware only adds headers when an
// Origin is present or the method is OPTIONS.
//
// Exclusion — /debug/* never gets CORS headers, full stop:
//
// #505/#507 found that echoing Access-Control-Allow-Origin on the pprof +
// expvar surface under /debug/ (mounted in serve_debug.go) is itself part of
// the exposure: a malicious page's cross-origin fetch() is normally blocked
// by the browser's own same-origin policy on the *response*, but an
// Allow-Origin header (even "*") is the server explicitly waiving that
// protection and letting the page read the response body — e.g. a full heap
// dump. debugLoopbackOnly (serve_debug.go) is /debug/'s own auth layer and
// deliberately does not want ANY CORS header on its responses, ever,
// regardless of Origin. So /debug/ requests are excluded from this
// middleware entirely before any Origin/header logic runs.
package engine

import (
	"net/http"
	"strings"
)

// corsMiddleware wraps an http.Handler with permissive CORS policy. It
// handles OPTIONS preflight directly (returning 204 with the allow-list
// headers) and delegates every other method to `next`, decorating the
// response with Access-Control-Allow-Origin.
//
// The allowed method/header set is intentionally broad: GET/POST are the
// common cases, PATCH/PUT/DELETE are permitted for future REST-ful
// endpoints, and the accepted headers cover Content-Type, the MCP session
// identifier, the Claude Code workspace root override, and Authorization
// so downstream authenticated clients can send bearer tokens without
// another round-trip of configuration.
func corsMiddleware(next http.Handler) http.Handler {
	const allowMethods = "GET, POST, PATCH, PUT, DELETE, OPTIONS"
	const allowHeaders = "Content-Type, Mcp-Session-Id, X-Workspace-Root, Authorization"
	const maxAge = "86400"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// See the file-level "Exclusion" note: /debug/ is authenticated by
		// debugLoopbackOnly and must never carry an Access-Control-* header,
		// so it bypasses this middleware's logic entirely — not even the
		// OPTIONS short-circuit below runs for it.
		if strings.HasPrefix(r.URL.Path, "/debug/") {
			next.ServeHTTP(w, r)
			return
		}

		origin := r.Header.Get("Origin")

		// Echo loopback origins so credentialed requests can round-trip;
		// fall back to "*" for anything else. Empty Origin → same-origin
		// (or non-browser client) → nothing to add.
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", originAllowValue(origin))
			w.Header().Set("Vary", "Origin")
		}

		if r.Method == http.MethodOptions {
			// Preflight short-circuit: reply with the allow-list headers
			// and 204 No Content. Do not call `next` — the mux would
			// return 405 Method Not Allowed for most routes.
			w.Header().Set("Access-Control-Allow-Methods", allowMethods)
			w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
			w.Header().Set("Access-Control-Max-Age", maxAge)
			// If the request asked to send credentials (Cookie, auth
			// header), advertise support when we're echoing a specific
			// origin. Star-origin + credentials is not legal per spec.
			if origin != "" && origin != "*" && originAllowValue(origin) == origin {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Non-preflight: still expose the allow-list headers so browsers
		// treating a non-simple response as cacheable know the shape.
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Methods", allowMethods)
			w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
			if origin != "*" && originAllowValue(origin) == origin {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}

		next.ServeHTTP(w, r)
	})
}

// originAllowValue returns the value to place in Access-Control-Allow-Origin
// for a given request Origin. Loopback schemes echo; anything else gets "*".
//
// Accepted loopback shapes:
//
//	http://localhost          http://localhost:PORT
//	http://127.0.0.1          http://127.0.0.1:PORT
//	https://localhost[:PORT]  https://127.0.0.1[:PORT]
//
// Anything else (null origin, file://, remote) is widened to "*". We do not
// attempt to parse the URL in full — a cheap prefix check is enough because
// the Origin header is a serialized tuple defined by the Fetch spec and has
// no path/query component.
func originAllowValue(origin string) string {
	if isLoopbackOrigin(origin) {
		return origin
	}
	return "*"
}

func isLoopbackOrigin(origin string) bool {
	// Strip scheme so we can do a simple host check.
	var rest string
	switch {
	case strings.HasPrefix(origin, "http://"):
		rest = strings.TrimPrefix(origin, "http://")
	case strings.HasPrefix(origin, "https://"):
		rest = strings.TrimPrefix(origin, "https://")
	default:
		return false
	}
	// rest is "host[:port]" (no path — Fetch spec forbids it on Origin).
	host := rest
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		host = rest[:i]
	}
	return host == "localhost" || host == "127.0.0.1"
}

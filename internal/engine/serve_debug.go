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
// None of these endpoints is reachable by a bare `go tool pprof <url>` or
// `curl <url>` call: debugLoopbackOnly (below) requires the X-Cogos-Debug
// header on every /debug/ request, and neither client sends it by default —
// a request without it 403s before reaching any pprof/expvar handler. Reach
// the surface one of two ways instead:
//
//	cogos debug heap [--out FILE]        — fetches /debug/pprof/heap with
//	cogos debug goroutines [--out FILE]  — the required header set, saves
//	                                        the raw profile to disk, and
//	                                        prints the `go tool pprof <file>`
//	                                        invocation to run against that
//	                                        saved file (cli_debug.go)
//	curl -H "X-Cogos-Debug: 1" http://127.0.0.1:6931/debug/pprof/heap
//	                                      — the same fetch by hand, for any
//	                                        endpoint above, without the CLI
//	                                        wrapper
//
// The header's value is never checked and is not a secret — see "Security"
// below for exactly what it does and does not defend against.
//
// Note: the mutex and block profiles are zero-rate by default (matches
// net/http/pprof's own upstream behavior — it never calls
// runtime.SetMutexProfileFraction / runtime.SetBlockProfileRate itself).
// Their endpoints exist and respond, but return empty profiles until those
// rates are set; enabling them by default here would add always-on runtime
// overhead to a daemon that #505 already found running hot, so that's left
// to a follow-up (or a future config flag) rather than baked in unasked.
//
// Security — what this guard actually defends against, and what it doesn't:
//
// The kernel already binds 127.0.0.1 by default (see Config.BindAddr /
// boot_bindaddr_warning.go), but that binding is opt-out — a config value
// an operator can widen for pod/LAN/Tailnet deployments, and #501/#505 both
// note the kernel has no HTTP auth-token surface to fall back on if they do.
// pprof handlers are a serious exposure if reachable non-locally: heap dumps
// can contain process memory, and /debug/pprof/profile is a cheap remote
// CPU-burn vector (30s of pegged CPU per request, no auth, no rate limit).
//
// An earlier version of this file gated purely on the caller's RemoteAddr
// being loopback (isLoopbackRemoteAddr). #507's review correctly killed that
// as the *only* check: RemoteAddr cannot distinguish "the operator's own
// cogos CLI" from "a webpage the operator's browser happens to have open",
// because both connect from 127.0.0.1. Concretely, with only that check, any
// open tab containing
//
//	<img src="http://127.0.0.1:6931/debug/pprof/profile">
//
// burns 30s of CPU with zero interaction beyond the page loading, and — because
// this mux used to sit behind the kernel's permissive CORS middleware
// (serve_cors.go), which echoed Access-Control-Allow-Origin for arbitrary
// origins — a malicious page's `fetch('http://127.0.0.1:6931/debug/pprof/heap')`
// could read the full heap-dump bytes back cross-origin. That's the
// DNS-rebinding/CSRF class of attack against loopback-bound local daemons.
//
// debugLoopbackOnly now stacks four checks. None of them is a secret
// credential — this is "prove you're not an ordinary browser request against
// this origin", not real authentication of an identity:
//
//  1. Reject any request carrying an Origin header. Cross-origin browser
//     requests (fetch, whether or not "no-cors") always carry one; a
//     legitimate CLI/curl/go-tool-pprof caller sends neither Origin nor
//     Referer. This alone stops the plain <img> and simple cross-origin
//     fetch() cases.
//  2. Reject any request carrying a Referer header, for the same reason —
//     catches same-origin browser navigations/fetches too (belt-and-suspenders
//     with #1, since same-origin requests often omit Origin).
//  3. Require the debugAuthHeader header (any non-empty value). Browsers
//     refuse to let page script set a custom header on <img>/<form>/no-cors
//     fetch — exactly the request shapes an attacker reaches for BECAUSE
//     they skip the CORS preflight a custom header would otherwise trigger.
//     A genuinely cross-origin fetch() that tries to set this header
//     instead triggers a preflight (OPTIONS), and /debug/ routes never
//     answer with Access-Control-* headers (excluded from corsMiddleware
//     entirely — see serve_cors.go), so the browser blocks the real request
//     before it reaches this handler at all.
//  4. isLoopbackRemoteAddr — the original check, kept as defense in depth:
//     it's the only thing standing between a remote caller and this surface
//     if BindAddr is ever widened without anyone remembering this file
//     exists, and it costs nothing to keep alongside 1-3.
//
// What this does NOT defend against, honestly:
//
//   - A DNS-rebinding attack sophisticated enough to make its request
//     *same-origin* with the target port (attacker's page served from a
//     domain that rebinds to 127.0.0.1 after load) can, from the browser's
//     perspective, legally send neither Origin nor Referer AND set an
//     arbitrary header without a preflight — same-origin fetches are exempt
//     from the CORS machinery checks 1-3 lean on. Nothing in this file
//     inspects the Host header to catch that gap; a Host allowlist
//     (127.0.0.1/localhost/::1) would be the standard closer and is a
//     reasonable follow-up if this surface is ever a real target, but it is
//     not implemented here.
//   - Any LOCAL process already running as the operator (malware, a
//     compromised dependency, another loopback-bound service). Every check
//     above is something a local process can trivially construct — they
//     authenticate "this isn't an ordinary browser request", not "this is
//     really the cogos CLI". Real inter-process authentication would need
//     the bearer-token surface #501/#505 already note the kernel lacks;
//     that's a larger piece of work than this diagnostic surface justifies
//     alone.
//   - A browser with a bug or an intentionally-relaxed configuration around
//     same-origin/CORS enforcement. This guard assumes standard browser
//     behavior.
package engine

import (
	"expvar"
	"net"
	"net/http"
	"net/http/pprof"
)

// debugAuthHeader is the header a caller must send, with any non-empty
// value, to reach the /debug/ surface. Its value is not checked and is not a
// secret — the security property comes from browsers refusing to let page
// script set ANY custom header on the request shapes (img/form/no-cors
// fetch) an attacker would otherwise use, not from the header's content
// being hard to guess. See the file-level doc comment for the full
// rationale and its limits.
const debugAuthHeader = "X-Cogos-Debug"

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

// debugLoopbackOnly wraps a handler in the four-layer guard described in the
// file-level doc comment: reject Origin, reject Referer, require
// debugAuthHeader, then fall back to the original loopback-RemoteAddr check
// as defense in depth. Order matters only for which error message a caller
// sees — all four must pass before `next` runs.
func debugLoopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "forbidden: debug endpoints reject requests carrying an Origin header", http.StatusForbidden)
			return
		}
		if r.Header.Get("Referer") != "" {
			http.Error(w, "forbidden: debug endpoints reject requests carrying a Referer header", http.StatusForbidden)
			return
		}
		if r.Header.Get(debugAuthHeader) == "" {
			http.Error(w, "forbidden: debug endpoints require the "+debugAuthHeader+" header", http.StatusForbidden)
			return
		}
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

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
// Exclusion — debug surfaces never get CORS headers, full stop:
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
//
// #507's review then caught that the same read primitive applied unchanged to
// the pre-existing /v1/debug/ endpoints (serve.go, handlers in debug.go):
// /v1/debug/last returns a DebugSnapshot carrying the extracted query text of
// the most recent chat request plus the filesystem paths of every injected
// cogdoc, and /v1/debug/context returns the live context window. With the
// star fallback above, any cross-origin page could read both bodies with a
// plain simple GET — no preflight, no header tricks. Protecting a heap dump
// four layers deep while leaving arguably more sensitive conversation content
// open to the identical attack was an inconsistency, not a deliberate scope
// choice, so isDebugPath below covers that prefix too.
//
// Why the /v1/debug/ endpoints get the CORS exclusion but NOT
// debugLoopbackOnly: unlike /debug/pprof, they have real browser consumers —
// the kernel serves its own dashboard at GET / and canvas at GET /canvas
// (serve.go), and both fetch /v1/debug/last and /v1/debug/context with
// `const API = window.location.origin`. Those are SAME-origin requests: the
// browser's same-origin policy lets the page read the response without any
// Access-Control-Allow-Origin header, so dropping CORS headers here costs
// those UIs nothing while fully closing the cross-origin read. Applying
// debugLoopbackOnly instead would break them outright — every dashboard fetch
// carries a Referer, which that guard rejects by design. Triggering these
// endpoints cross-origin without being able to read the response is harmless
// (they are side-effect-free reads of an in-memory snapshot), which is why the
// no-cors/img vector debugLoopbackOnly exists to stop on /debug/pprof/profile
// — a 30s CPU burn — has no analogue here.
//
// Second exclusion tier — content and credential surfaces are loopback-only:
//
// #507's review rounds 4 and 5 found the identical read primitive on routes
// that are not named "debug" at all — first /v1/conversation (full
// untruncated turn text, session_id defaulting to the live session) and
// /v1/ledger (empty session_id means ALL sessions per LedgerQuery's doc
// comment), then /v1/cogdoc/read (raw workspace file content for any
// resolvable cog: URI). All are unauthenticated simple GETs, so the star
// fallback was enough for any page the operator happened to visit to
// fetch('http://127.0.0.1:6931/v1/ledger') and read the body. Round 5's
// response swept every registered route (the classification table below) and
// found the same class on more surfaces, including one credential leak:
// GET /v1/identity/grants/current returns a live grant's RAW TOKEN by design
// (the "zero-paste primitive" — its stated consumer is "a same-loopback
// page", which is exactly this tier's contract).
//
// Tiered routes do NOT get the /v1/debug treatment of dropping CORS entirely,
// because they have legitimate CROSS-origin browser consumers on other
// loopback ports (the mod3 dashboard on :7860, the constellation surfaces).
// Dropping every Access-Control-* header would break them. What goes is only
// the "*" widening for non-loopback origins: corsLoopbackOnly echoes a
// loopback Origin exactly as before, and emits no Access-Control-Allow-Origin
// at all for anything else, so a remote page's fetch() still reaches the
// handler but the browser refuses to hand it the body. Same-origin clients
// (no Origin header) and non-browser clients (curl, the MCP CLI) are
// unaffected in either direction.
//
// # Route classification table
//
// Every route the engine registers (serve.go's mux block plus every
// register*Routes sibling), classified by response payload. Audit coverage
// against GET /v1/manifest — s.route()-registered paths appear there.
// Classes:
//
//	NONE  — corsNone: middleware fully bypassed, no Access-Control-* ever
//	LOOP  — corsLoopbackOnly: loopback Origin echoed, no header otherwise
//	TELEM — corsPermissive, documented: telemetry/log/attention-score
//	        surfaces; paths, scores, log lines — no document/conversation
//	        bodies, no credentials
//	META  — corsPermissive: public or identifier-level metadata
//
//	NONE  /debug/pprof/*, /debug/vars      pprof heap/profile, expvar
//	NONE  /v1/debug/last, /v1/debug/context pipeline snapshot, context window
//
//	LOOP  /mcp                    full MCP tool surface (cogdoc read, memory
//	                              search, session tools) over streamable HTTP
//	LOOP  /memory/read            raw memory-file bytes; /memory/search index
//	LOOP  /v1/agent/*, /v1/agents/* AgentSnapshot: memory entries, proposals,
//	                              observations, per-cycle Result text
//	LOOP  /v1/blobs/{digest}      raw blob bytes
//	LOOP  /v1/blocks/*            raw block bytes; manifest enumerates every
//	                              hash, so together = full content sync
//	LOOP  /v1/bus/*               bus events incl. dashboard chat text;
//	                              per-bus + cross-bus queries, SSE streams
//	LOOP  /v1/chat/completions    completion grounded in foveated workspace
//	                              context — an oracle over the corpus
//	LOOP  /v1/claude-code/*       session listings incl. FirstPromptSummary
//	LOOP  /v1/cogdoc/read         raw file content for any cog: URI (round 5)
//	LOOP  /v1/config              raw kernel.yaml when mutation is enabled
//	LOOP  /v1/context/foveated    rendered context text + block previews
//	LOOP  /v1/conversation        full turn-by-turn text (round 4)
//	LOOP  /v1/dispatch-jobs/{id}  dispatch result payloads
//	LOOP  /v1/events[/stream]     QueryLedger wrapper (round 4)
//	LOOP  /v1/handoffs/*          OfferPayload carries full state blobs
//	LOOP  /v1/identity/*          grants/current returns a live RAW TOKEN;
//	                              POST mint response carries one too
//	LOOP  /v1/ledger              hash-chained cross-session history (round 4)
//	LOOP  /v1/messages            Anthropic-compat twin of /v1/chat
//	LOOP  /v1/peer-awareness      packet rendered from channel activity
//	LOOP  /v1/sessions/*          {id}[/context] embeds SessionBlock.Content
//	                              (raw context-block text); presence rides
//	LOOP  /v1/skills/*            exec returns skill output; list rides
//	LOOP  /v1/tool-calls          args+outputs across all ledgers (round 4)
//
//	TELEM /metrics, /v1/vitals, /v1/traces, /v1/kernel-log,
//	      /v1/proprioceptive     host/provider gauges, run-log JSONL rows
//	TELEM /v1/context (GET)      fovea paths+scores only — the rendered-text
//	                             sibling /v1/context/foveated is LOOP
//	TELEM /v1/attention, /v1/constellation/*, /v1/observer/state,
//	      /v1/lightcone          attention scores, doc paths/titles, cones
//	TELEM /v1/reconcile/coherence, /v1/kernel/rates, /v1/cluster/status,
//	      /coherence/check, /v1/hud/state  status snapshots, session ids
//
//	META  GET /, /canvas         embedded dashboard HTML (fetches same-origin)
//	META  /health, /v1/manifest, /v1/models, /v1/card, /v1/providers,
//	      /v1/taa, /v1/settings/context, /v1/services/*
//	META  /v1/resolve, /v1/uri/resolve  cog: URI → path + exists; discloses
//	                             layout, never content
//	META  /v1/channel-sessions/*, /v1/channels/{id}/peers  session-identity
//	      records: ids, participant prefs, opaque metadata — no credential
//	      (identity grants are the credential and they are LOOP)
//
// The dividing rule: a response that can carry document/conversation/agent
// CONTENT or a CREDENTIAL is LOOP; identifiers, paths, scores, and gauges
// are TELEM/META. Two maintenance notes: (1) routes registered by extensions
// (RegisterHTTPExtensions) or providers (providers_register.go) default to
// permissive — a content-bearing extension route must add its prefix to
// loopbackOnlyPrefixes; (2) new mux routes default to permissive, so
// classify them against this table when adding them.
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
		policy := corsPolicyForPath(r.URL.Path)

		// See the file-level "Exclusion" note: debug surfaces must never carry
		// an Access-Control-* header, so they bypass this middleware's logic
		// entirely — not even the OPTIONS short-circuit below runs for them.
		if policy == corsNone {
			next.ServeHTTP(w, r)
			return
		}

		origin := r.Header.Get("Origin")

		// allow is "" whenever the response must carry no
		// Access-Control-Allow-Origin: no Origin header at all (same-origin
		// fetch, curl, the MCP CLI — nothing to add), or a remote origin on a
		// corsLoopbackOnly route.
		allow := allowOriginFor(policy, origin)

		// Vary: Origin whenever the emitted headers depend on the request's
		// Origin. On a corsLoopbackOnly route that includes the no-Origin
		// case, so an intermediary cannot replay a loopback-blessed response
		// to a remote origin.
		if origin != "" || policy == corsLoopbackOnly {
			w.Header().Set("Vary", "Origin")
		}
		if allow != "" {
			w.Header().Set("Access-Control-Allow-Origin", allow)
		}

		if r.Method == http.MethodOptions {
			// Preflight short-circuit: reply with the allow-list headers
			// and 204 No Content. Do not call `next` — the mux would
			// return 405 Method Not Allowed for most routes. Without an
			// Access-Control-Allow-Origin the preflight fails at the browser,
			// which is exactly what a remote origin should get on a
			// corsLoopbackOnly route.
			w.Header().Set("Access-Control-Allow-Methods", allowMethods)
			w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
			w.Header().Set("Access-Control-Max-Age", maxAge)
			// If the request asked to send credentials (Cookie, auth
			// header), advertise support when we're echoing a specific
			// origin. Star-origin + credentials is not legal per spec.
			if allow != "" && allow == origin && origin != "*" {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Non-preflight: still expose the allow-list headers so browsers
		// treating a non-simple response as cacheable know the shape.
		if allow != "" {
			w.Header().Set("Access-Control-Allow-Methods", allowMethods)
			w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
			if allow == origin && origin != "*" {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}

		next.ServeHTTP(w, r)
	})
}

// corsPolicy classifies a request path by how much cross-origin read access
// the middleware may grant to its responses. See the two "Exclusion" notes at
// the top of this file for why each tier exists.
type corsPolicy int

const (
	// corsPermissive is the default for every route not named below: echo
	// loopback origins, widen to "*" for anything else so the middleware
	// keeps working if cfg.BindAddr widens to a pod / Tailnet address.
	corsPermissive corsPolicy = iota

	// corsLoopbackOnly echoes loopback origins and emits NO
	// Access-Control-Allow-Origin for anything else. The handler still runs
	// (these are side-effect-free reads), but the browser will not hand the
	// response body to a remote page.
	corsLoopbackOnly

	// corsNone bypasses the middleware entirely — no Access-Control-* header
	// ever appears on the response, regardless of Origin or method.
	corsNone
)

// corsPolicyForPath maps a request path onto its CORS tier.
func corsPolicyForPath(path string) corsPolicy {
	switch {
	case isDebugPath(path):
		return corsNone
	case isLoopbackOnlyPath(path):
		return corsLoopbackOnly
	default:
		return corsPermissive
	}
}

// allowOriginFor returns the Access-Control-Allow-Origin value for a
// (policy, Origin) pair, or "" when the response must carry no such header.
func allowOriginFor(policy corsPolicy, origin string) string {
	if origin == "" {
		return ""
	}
	switch policy {
	case corsNone:
		return ""
	case corsLoopbackOnly:
		if isLoopbackOrigin(origin) {
			return origin
		}
		return ""
	default:
		return originAllowValue(origin)
	}
}

// isDebugPath reports whether a request path belongs to one of the kernel's
// debug surfaces, which are excluded from CORS entirely (see the file-level
// "Exclusion" note for why each one is here):
//
//	/debug/     — pprof + expvar (serve_debug.go), also behind debugLoopbackOnly
//	/v1/debug/  — pipeline snapshot + context window (debug.go), same-origin
//	              dashboard/canvas consumers only
//
// The bare, slashless forms are matched too so a future /v1/debug index route
// cannot quietly land back inside the CORS middleware. Anything that is merely
// prefixed by these strings without a path separator (e.g. /v1/debugger) is
// NOT matched — that would be a different route with different exposure.
func isDebugPath(path string) bool {
	return matchesAnyRoutePrefix(path, "/debug", "/v1/debug")
}

// loopbackOnlyPrefixes are the routes whose response bodies can carry
// document/conversation/agent content or a credential, and which therefore
// never get the "*" widening. The per-prefix rationale lives in the
// file-level route classification table — keep the two in sync. Sorted.
var loopbackOnlyPrefixes = []string{
	"/mcp",
	"/memory",
	"/v1/agent",
	"/v1/agents",
	"/v1/blobs",
	"/v1/blocks",
	"/v1/bus",
	"/v1/chat",
	"/v1/claude-code",
	"/v1/cogdoc",
	"/v1/config",
	"/v1/context/foveated",
	"/v1/conversation",
	"/v1/dispatch-jobs",
	"/v1/events",
	"/v1/handoffs",
	"/v1/identity",
	"/v1/ledger",
	"/v1/messages",
	"/v1/peer-awareness",
	"/v1/sessions",
	"/v1/skills",
	"/v1/tool-calls",
}

// isLoopbackOnlyPath reports whether a request path serves content or
// credential data, i.e. belongs to the corsLoopbackOnly tier.
func isLoopbackOnlyPath(path string) bool {
	return matchesAnyRoutePrefix(path, loopbackOnlyPrefixes...)
}

// matchesAnyRoutePrefix reports whether path equals one of the given route
// prefixes or sits underneath it as a path segment. The bare, slashless form
// is matched so a future index route cannot quietly land in the wrong tier,
// while anything merely string-prefixed without a separator (/v1/debugger,
// /v1/ledgerfoo) is NOT matched — that would be a different route with
// different exposure.
func matchesAnyRoutePrefix(path string, prefixes ...string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// originAllowValue returns the value to place in Access-Control-Allow-Origin
// for a given request Origin. Loopback schemes echo; anything else gets "*".
//
// Accepted loopback shapes:
//
//	http://localhost          http://localhost:PORT
//	http://127.0.0.1          http://127.0.0.1:PORT
//	http://[::1]              http://[::1]:PORT
//	https:// twins of all of the above
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
	//
	// IPv6 hosts are bracketed in the serialized Origin ("http://[::1]:8080"),
	// and the colons inside the brackets would confuse the port split below,
	// so handle the one recognized IPv6 loopback form first. A browser on an
	// IPv6-loopback page serializes exactly "[::1]" — an origin is already in
	// canonical form, so no expansion ("[0:0:0:0:0:0:0:1]") handling is
	// needed, and non-loopback IPv6 hosts fall through to false.
	if strings.HasPrefix(rest, "[") {
		after, ok := strings.CutPrefix(rest, "[::1]")
		if !ok {
			return false
		}
		return after == "" || strings.HasPrefix(after, ":")
	}
	host := rest
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		host = rest[:i]
	}
	return host == "localhost" || host == "127.0.0.1"
}

// serve_grant_auth.go — kernel HTTP write-route grant authentication.
//
// Closes the open CSRF gap documented at length in serve_cors.go's file
// header ("Write-side CSRF is a known, PRE-EXISTING gap"): corsMiddleware
// only ever decided which Access-Control-* headers a RESPONSE carries, never
// whether a request runs — a cross-origin page's CORS-simple POST (or any
// same-origin blind request) reached every mutating handler with no gate at
// all. This file is that gate, per the sealed design in
// cog://mem/procedural/plan-hermes-kernel-loop-wiring (step 5) and the
// operator's move (2026-08-12, verbatim: "can we use the kernel to mint a
// token and store it in the vault and use that to sign the http") — which
// turns out to complete existing machinery rather than add new: board task
// 60 already built kernel-issued ledger-backed identity grants
// (serve_identity_grants.go). This file only wires that machinery onto the
// HTTP request path as middleware, reusing its Verify/VerifyAny logic
// in-process rather than reimplementing any hashing.
//
// Mechanism:
//
//   - Every request whose method is not GET, OR whose path is /mcp
//     (regardless of method — see below), must carry a valid
//     "X-Cogos-Grant: <token>" header. Missing or invalid → 401 with a terse
//     JSON body. Valid means VerifyAny(token) finds a live, unexpired grant
//     anywhere in the registry — this middleware asks "does the caller hold
//     SOME kernel-issued credential", not "which one", so any surface's
//     grant (node-root, a dashboard's own, mod3's own) satisfies it.
//   - /mcp is gated on EVERY method, not just POST/PUT/DELETE, because it is
//     a single JSON-RPC/streamable-HTTP multiplexer — GET /mcp resumes an
//     SSE session, DELETE /mcp tears one down, and POST /mcp carries every
//     tool call including cog_write_cogdoc, the membrane-bypass companion
//     gap this build was explicitly asked to close (see the test file).
//     Carving /mcp out of the blanket "GET routes are exempt" rule below is
//     deliberate, not an oversight.
//   - EXEMPT, unconditionally: GET on any path other than /mcp, GET/HEAD
//     /health, and POST /v1/identity/verify (the verification authority
//     itself — gating it would be circular: a caller with no grant could
//     never learn whether ITS token is valid).
//   - BOOTSTRAP EXEMPTION: POST /v1/identity/grants (mint) stays reachable
//     without a grant — a brand-new local consumer has no token yet and this
//     is how it gets its first one. This is not an open door: the kernel
//     binds loopback-only by default, so any caller here is already a local
//     process; and corsLoopbackOnly (serve_cors.go) withholds
//     Access-Control-Allow-Origin from a remote origin's response to this
//     path, so a cross-origin CSRF POST can blind-mint a grant but can never
//     read the response body back — the minted token cannot be exfiltrated
//     that way. What a blind mint CAN still do is grow the ledger (every
//     mint is a write-ahead ledger append, serve_identity_grants.go's
//     MintOrReuse) — grantMintLimiter below caps that throughput so a script
//     hammering this route cannot flood the identity-grants ledger file.
//     HMAC request-signing for this route is deferred until it needs to
//     leave loopback (design step 5, per the plan doc); the cluster channel
//     is already separately authenticated and out of scope here.
//
// CSRF threat model: a custom header (X-Cogos-Grant) is not one of the
// Fetch spec's CORS-safelisted headers, so any browser request carrying it
// forces a CORS preflight — and corsMiddleware's OPTIONS handling only
// echoes Access-Control-Allow-Origin for a loopback (or otherwise permitted)
// Origin, so a REMOTE page's preflight for a write route fails at the
// browser and the real request never goes out. That is what actually closes
// the CSRF hole this middleware exists for; the 401 body is defense for the
// same-origin/curl/blind case where no preflight ever ran.
//
// Enforcement can be turned off entirely via
// Config.WriteRouteGrantAuthDisabled (see config.go's doc comment on that
// field for the default-ON / fail-safe rationale) — Boot() logs loudly when
// it is.
package engine

import (
	"net/http"
	"sync"
	"time"
)

// GrantHeaderName is the header a caller presents a kernel-issued identity
// grant token in. Exported so hook scripts, the dashboard, and tests share
// one literal rather than re-typing "X-Cogos-Grant".
const GrantHeaderName = "X-Cogos-Grant"

// grantMintRequestPath is the one route the bootstrap exemption applies to.
const grantMintRequestPath = "/v1/identity/grants"

// grantVerifyRequestPath is exempt for a different reason (see file header):
// gating the verification authority itself would be circular.
const grantVerifyRequestPath = "/v1/identity/verify"

// mcpRequestPath is gated on every method — see the file header for why it
// is carved out of the blanket GET exemption.
const mcpRequestPath = "/mcp"

// defaultGrantMintRateLimit and defaultGrantMintRateWindow bound
// POST /v1/identity/grants throughput (see the file header's BOOTSTRAP
// EXEMPTION note). 20/minute comfortably covers every legitimate local
// bootstrap burst (a handful of surfaces each minting once at their own
// startup) while making a blind flood-the-ledger script land well below
// "fills the disk" territory long before an operator would notice the rate
// limit at all.
const (
	defaultGrantMintRateLimit  = 20
	defaultGrantMintRateWindow = time.Minute
)

// grantMintLimiter is a coarse fixed-window rate limiter guarding the
// bootstrap-exempt POST /v1/identity/grants route from being used to flood
// the identity-grants ledger by an unauthenticated (but loopback-bound)
// caller varying surface/scope on every request. Deliberately simple: this
// is a safety net for the loopback threat model, not a general-purpose rate
// limiter — see MintOrReuse's own maxLiveGrantSurfaces cap for the
// complementary "don't grow the live-surface count without bound" defense
// this limiter does not replace.
type grantMintLimiter struct {
	mu          sync.Mutex
	limit       int
	window      time.Duration
	windowStart time.Time
	count       int
	now         func() time.Time
}

func newGrantMintLimiter(limit int, window time.Duration) *grantMintLimiter {
	return &grantMintLimiter{limit: limit, window: window, now: time.Now}
}

// Allow reports whether one more mint is permitted in the current window,
// consuming one slot if so. Resets the window lazily on first call after it
// elapses rather than running a background ticker.
func (l *grantMintLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.windowStart.IsZero() || now.Sub(l.windowStart) >= l.window {
		l.windowStart = now
		l.count = 0
	}
	if l.count >= l.limit {
		return false
	}
	l.count++
	return true
}

// grantAuthMiddleware wraps mux (or any handler further down the chain)
// with the X-Cogos-Grant gate described in the file header. It is installed
// INSIDE corsMiddleware — i.e. handler := corsMiddleware(s.grantAuthMiddleware(mux))
// in NewServer — so CORS still owns the OPTIONS preflight short-circuit and
// this middleware never has to special-case OPTIONS itself (corsMiddleware
// never calls next for that method).
func (s *Server) grantAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg != nil && s.cfg.WriteRouteGrantAuthDisabled {
			next.ServeHTTP(w, r)
			return
		}

		if isGrantExemptRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		if r.Method == http.MethodPost && r.URL.Path == grantMintRequestPath {
			if s.grantMintLimiter != nil && !s.grantMintLimiter.Allow() {
				writeJSONError(w, http.StatusTooManyRequests, "rate_limited",
					"too many grant mint requests; see grantMintLimiter in serve_grant_auth.go")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		token := r.Header.Get(GrantHeaderName)
		if token == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing_grant",
				GrantHeaderName+" header required for this route")
			return
		}
		if s.identityGrants == nil {
			// Nil registry should be structurally impossible (NewServer always
			// constructs one), but fail closed rather than panic if it ever
			// happens — a missing registry is not a green light to skip auth.
			writeJSONError(w, http.StatusUnauthorized, "invalid_grant", "no identity grant registry available")
			return
		}
		if _, ok := s.identityGrants.VerifyAny(token); !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid_grant",
				"grant token is missing, expired, or revoked")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isGrantExemptRequest reports whether r may bypass the grant gate
// unconditionally (i.e. before the bootstrap-exemption / rate-limit branch
// even runs). See the file header for the full rationale per case.
func isGrantExemptRequest(r *http.Request) bool {
	// /mcp is NEVER exempt, on any method — carved out of the GET exemption
	// below before it can apply.
	if r.URL.Path == mcpRequestPath {
		return false
	}
	if r.URL.Path == "/health" {
		return true
	}
	if r.Method == http.MethodPost && r.URL.Path == grantVerifyRequestPath {
		return true
	}
	// Every other GET (and HEAD, which ServeMux treats as GET for handler
	// dispatch) is a read — exempt. Everything else (POST/PUT/PATCH/DELETE
	// on any other path) falls through to the grant check.
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}

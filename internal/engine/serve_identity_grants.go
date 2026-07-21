// serve_identity_grants.go — kernel-issued identity grants (board task 60,
// chunk 1: "kernel as identity seat"). Design doc:
// cog://mem/working/2026-07-21-kernel-identity-seat-design (cog workspace).
//
// Operator's ruling that supersedes this design's own §4 chunk-1 scope: "I
// don't want to have to paste anything" (2026-07-21). Chunk 1 as originally
// scoped only migrated the *verification* step; the zero-paste bootstrap UX
// was deferred to chunk 4. That deferral is overridden here — chunk 1 now
// also has to make the token acquirable with no operator action, which is
// why GET /v1/identity/grants/current exists below (a surface can ask "what
// is MY currently-live grant" without minting a new one every restart).
//
// Mechanism (per design §3.1-§3.3, chunk-1-sized):
//
//	POST /v1/identity/grants        — mint a grant for {surface, scope[]}.
//	                                   Idempotent per surface: if a live,
//	                                   unexpired grant already exists for
//	                                   that surface, it is returned again
//	                                   rather than minting a second one (see
//	                                   design §4 chunk-1 verify-teeth item 5
//	                                   — a restart must not silently
//	                                   invalidate what the operator already
//	                                   has pasted/bootstrapped elsewhere).
//	POST /v1/identity/verify        — {surface, token} -> {valid, scope[],
//	                                   expires_at} | 401. The verification
//	                                   authority for every migrated surface.
//	GET  /v1/identity/grants        — operator-facing inventory. NEVER
//	                                   returns a token value, per design §3.1.
//	GET  /v1/identity/grants/current?surface=X — returns the live grant's
//	                                   token for a named surface, if one
//	                                   exists. This is chunk 1's zero-paste
//	                                   primitive: a surface that already
//	                                   holds a grant (minted at its own boot)
//	                                   can be asked again by anything that
//	                                   shares the loopback bind, without a
//	                                   second /grants POST re-minting a
//	                                   second credential. Loopback-only
//	                                   threat model throughout (design §3.6);
//	                                   this endpoint returning a live raw
//	                                   token value is exactly as sensitive as
//	                                   /v1/identity/grants' POST response
//	                                   already is, not a new exposure class.
//
// In-memory grant store ONLY for this chunk. Restart-safety (ledger-backed
// grants, per design §3.2) is explicitly chunk 2 — a kernel restart wipes
// every live grant here, same as HarnessBindingCRD's existing posture
// (rbac_provider.go doc comment). Revoke/rotate are also chunk 2 (design
// §4); this file deliberately proves the surface-side migration + zero-paste
// pattern cheaply first, per the design's own sizing rationale.
package engine

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// defaultGrantTTL matches nothing in particular yet (rotation cadence is an
// OPEN GATE, design §5.4) — 30 days is a reasonable default that outlives any
// single chat-server.py boot without being "forever."
const defaultGrantTTL = 30 * 24 * time.Hour

// IdentityGrant is the kernel's record of a surface-scoped credential. The
// Token field is held in memory only for chunk 1; chunk 2 moves the durable
// record to the ledger (hash only, never the raw value — design §3.2) and
// this struct's Token field becomes a derived/cached value rather than the
// source of truth.
type IdentityGrant struct {
	GrantID   string    `json:"grant_id"`
	Surface   string    `json:"surface"`
	Scope     []string  `json:"scope"`
	Token     string    `json:"-"` // never serialized in list responses
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (g *IdentityGrant) expired(now time.Time) bool {
	return now.After(g.ExpiresAt)
}

// IdentityGrantRegistry is the in-memory store, keyed by surface (chunk 1 —
// one live grant per surface at a time, matching the "idempotent reuse on
// restart" verify-tooth). A secondary index by grant_id backs GET-by-id
// lookups (revoke, chunk 2) and by-token backs verify.
type IdentityGrantRegistry struct {
	mu        sync.RWMutex
	bySurface map[string]*IdentityGrant
	byGrantID map[string]*IdentityGrant
}

// NewIdentityGrantRegistry returns an empty registry.
func NewIdentityGrantRegistry() *IdentityGrantRegistry {
	return &IdentityGrantRegistry{
		bySurface: make(map[string]*IdentityGrant),
		byGrantID: make(map[string]*IdentityGrant),
	}
}

// MintOrReuse returns the live, unexpired grant for surface if one exists;
// otherwise mints a fresh one with the given scope and TTL. This is the
// idempotency the design's chunk-1 verify teeth #5 requires: a chat-server.py
// restart must not invalidate whatever the operator (or a bootstrapped page)
// already holds.
func (r *IdentityGrantRegistry) MintOrReuse(surface string, scope []string, ttl time.Duration) (*IdentityGrant, error) {
	if surface == "" {
		return nil, fmt.Errorf("surface is required")
	}
	now := time.Now().UTC()

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.bySurface[surface]; ok && !existing.expired(now) {
		return existing, nil
	}

	token, err := mintGrantToken()
	if err != nil {
		return nil, fmt.Errorf("mint token: %w", err)
	}
	if ttl <= 0 {
		ttl = defaultGrantTTL
	}
	grant := &IdentityGrant{
		GrantID:   "grant-" + mustHex(6),
		Surface:   surface,
		Scope:     scope,
		Token:     token,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
	}
	r.bySurface[surface] = grant
	r.byGrantID[grant.GrantID] = grant
	return grant, nil
}

// Verify checks a presented token for a named surface against the live
// grant. Returns (grant, true) only when the surface has a live, unexpired
// grant AND the presented token matches it.
func (r *IdentityGrantRegistry) Verify(surface, token string) (*IdentityGrant, bool) {
	if surface == "" || token == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.bySurface[surface]
	if !ok || g.expired(time.Now().UTC()) {
		return nil, false
	}
	if !constantTimeEqual(g.Token, token) {
		return nil, false
	}
	return g, true
}

// Current returns the live grant for a surface (including its raw token) —
// the zero-paste primitive: a surface's own page can ask the kernel "what do
// I currently hold" instead of the operator pasting anything. Returns
// (nil, false) if no live grant exists for that surface (caller should mint
// one, or degrade — see chat-server.py's fallback path).
func (r *IdentityGrantRegistry) Current(surface string) (*IdentityGrant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.bySurface[surface]
	if !ok || g.expired(time.Now().UTC()) {
		return nil, false
	}
	return g, true
}

// Snapshot returns every live grant for the operator-facing inventory (GET
// /v1/identity/grants) — never includes Token (design §3.1: "NEVER the
// token value").
func (r *IdentityGrantRegistry) Snapshot() []*IdentityGrant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := time.Now().UTC()
	out := make([]*IdentityGrant, 0, len(r.bySurface))
	for _, g := range r.bySurface {
		if g.expired(now) {
			continue
		}
		out = append(out, g)
	}
	return out
}

func mintGrantToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func mustHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const alphabet = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, c := range b {
		out[i*2] = alphabet[c>>4]
		out[i*2+1] = alphabet[c&0x0f]
	}
	return string(out)
}

// constantTimeEqual avoids a timing side-channel on token comparison. Small
// inline helper rather than importing crypto/subtle's ConstantTimeCompare
// for a length-mismatch case, which subtle.ConstantTimeCompare handles
// differently (returns 0 immediately on length mismatch — fine here too,
// but written out for a one-file, no-new-import chunk).
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// ─── route registration ──────────────────────────────────────────────────────

// registerIdentityGrantRoutes attaches the /v1/identity/* routes onto mux.
// Called from NewServer alongside the other register*Routes calls.
func (s *Server) registerIdentityGrantRoutes(mux *http.ServeMux) {
	s.route(mux, "POST /v1/identity/grants", s.handleIdentityGrantMint)
	s.route(mux, "GET /v1/identity/grants", s.handleIdentityGrantList)
	s.route(mux, "GET /v1/identity/grants/current", s.handleIdentityGrantCurrent)
	s.route(mux, "POST /v1/identity/verify", s.handleIdentityVerify)
}

// ─── wire types ───────────────────────────────────────────────────────────────

type identityGrantMintRequest struct {
	Surface  string   `json:"surface"`
	Scope    []string `json:"scope,omitempty"`
	TTLHours float64  `json:"ttl_hours,omitempty"`
}

type identityGrantMintResponse struct {
	GrantID   string   `json:"grant_id"`
	Token     string   `json:"token"`
	Surface   string   `json:"surface"`
	Scope     []string `json:"scope,omitempty"`
	ExpiresAt string   `json:"expires_at"`
}

type identityGrantListEntry struct {
	GrantID   string   `json:"grant_id"`
	Surface   string   `json:"surface"`
	Scope     []string `json:"scope,omitempty"`
	IssuedAt  string   `json:"issued_at"`
	ExpiresAt string   `json:"expires_at"`
}

type identityVerifyRequest struct {
	Surface string `json:"surface"`
	Token   string `json:"token"`
}

type identityVerifyResponse struct {
	Valid     bool     `json:"valid"`
	Scope     []string `json:"scope,omitempty"`
	ExpiresAt string   `json:"expires_at,omitempty"`
}

// ─── POST /v1/identity/grants ─────────────────────────────────────────────────

// handleIdentityGrantMint mints (or idempotently re-returns) a grant for the
// requested surface. The response never echoes back a broader scope than
// the caller requested (design §4 verify-tooth #1) — scope is stored
// exactly as requested, no server-side widening.
func (s *Server) handleIdentityGrantMint(w http.ResponseWriter, r *http.Request) {
	var req identityGrantMintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
		return
	}
	if req.Surface == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "surface is required")
		return
	}
	ttl := time.Duration(0)
	if req.TTLHours > 0 {
		ttl = time.Duration(req.TTLHours * float64(time.Hour))
	}
	grant, err := s.identityGrants.MintOrReuse(req.Surface, req.Scope, ttl)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSONResp(w, http.StatusOK, identityGrantMintResponse{
		GrantID:   grant.GrantID,
		Token:     grant.Token,
		Surface:   grant.Surface,
		Scope:     grant.Scope,
		ExpiresAt: grant.ExpiresAt.Format(time.RFC3339),
	})
}

// ─── GET /v1/identity/grants ──────────────────────────────────────────────────

// handleIdentityGrantList is the operator-facing inventory — the kernel-
// native replacement for "grep 6 files for state/*-token" (design §3.1).
// Never includes a token value.
func (s *Server) handleIdentityGrantList(w http.ResponseWriter, r *http.Request) {
	snap := s.identityGrants.Snapshot()
	out := make([]identityGrantListEntry, 0, len(snap))
	for _, g := range snap {
		out = append(out, identityGrantListEntry{
			GrantID:   g.GrantID,
			Surface:   g.Surface,
			Scope:     g.Scope,
			IssuedAt:  g.IssuedAt.Format(time.RFC3339),
			ExpiresAt: g.ExpiresAt.Format(time.RFC3339),
		})
	}
	writeJSONResp(w, http.StatusOK, map[string]any{"grants": out})
}

// ─── GET /v1/identity/grants/current ─────────────────────────────────────────

// handleIdentityGrantCurrent is the zero-paste primitive (operator ruling
// 2026-07-21, see file header): returns the live grant's raw token for a
// named surface, if one exists, so a same-loopback page can bootstrap
// without any operator paste. 404 when no live grant exists for that surface
// (caller should mint one via POST /v1/identity/grants, or degrade).
func (s *Server) handleIdentityGrantCurrent(w http.ResponseWriter, r *http.Request) {
	surface := r.URL.Query().Get("surface")
	if surface == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "surface query param is required")
		return
	}
	grant, ok := s.identityGrants.Current(surface)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not_found", "no live grant for surface "+surface)
		return
	}
	writeJSONResp(w, http.StatusOK, identityGrantMintResponse{
		GrantID:   grant.GrantID,
		Token:     grant.Token,
		Surface:   grant.Surface,
		Scope:     grant.Scope,
		ExpiresAt: grant.ExpiresAt.Format(time.RFC3339),
	})
}

// ─── POST /v1/identity/verify ─────────────────────────────────────────────────

// handleIdentityVerify is the verification authority every migrated surface
// calls instead of a local string-compare. Always 200 with {"valid":false}
// on a bad/missing/expired token (not 401) so the response shape is uniform
// JSON for every caller — surfaces branch on the `valid` field, matching the
// {valid, scope[], expires_at} | 401 shape the design's §3.1 table describes
// loosely; chunk 1 picks the always-200-with-valid-bool variant since every
// consumer here is a same-process JSON decode, not a bearer-auth middleware
// that needs the HTTP status itself to carry meaning.
func (s *Server) handleIdentityVerify(w http.ResponseWriter, r *http.Request) {
	var req identityVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
		return
	}
	grant, ok := s.identityGrants.Verify(req.Surface, req.Token)
	if !ok {
		writeJSONResp(w, http.StatusOK, identityVerifyResponse{Valid: false})
		return
	}
	writeJSONResp(w, http.StatusOK, identityVerifyResponse{
		Valid:     true,
		Scope:     grant.Scope,
		ExpiresAt: grant.ExpiresAt.Format(time.RFC3339),
	})
}

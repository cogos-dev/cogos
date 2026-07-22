// serve_identity_grants.go — kernel-issued identity grants (board task 60,
// chunk 1: "kernel as identity seat"; chunk 2: ledger-backed grants + revoke).
// Design doc: cog://mem/working/2026-07-21-kernel-identity-seat-design (cog
// workspace), "CHUNK 1 VERIFICATION NOTES" section at its bottom.
//
// Operator's ruling that supersedes this design's own §4 chunk-1 scope: "I
// don't want to have to paste anything" (2026-07-21). Chunk 1 as originally
// scoped only migrated the *verification* step; the zero-paste bootstrap UX
// was deferred to chunk 4. That deferral is overridden here — chunk 1 now
// also has to make the token acquirable with no operator action, which is
// why GET /v1/identity/grants/current exists below (a surface can ask "what
// is MY currently-live grant" without minting a new one every restart).
//
// Mechanism (per design §3.1-§3.3, chunk-1-sized; §3.2 ledger-first for
// chunk 2):
//
//	POST /v1/identity/grants        — mint a grant for {surface, scope[]}.
//	                                   Idempotent per surface: if a live,
//	                                   unexpired grant already exists for
//	                                   that surface AND its raw token is
//	                                   still held in memory, it is returned
//	                                   again rather than minting a second one
//	                                   (see design §4 chunk-1 verify-teeth
//	                                   item 5 — a restart must not silently
//	                                   invalidate what the operator already
//	                                   has pasted/bootstrapped elsewhere).
//	POST /v1/identity/verify        — {surface, token} -> {valid, scope[],
//	                                   expires_at} | 401. The verification
//	                                   authority for every migrated surface.
//	                                   Verifies by hashing the presented
//	                                   token and comparing against the
//	                                   grant's ledger-recorded integrity
//	                                   hash — works identically whether the
//	                                   grant was minted this boot or
//	                                   reconstructed from the ledger after a
//	                                   restart (chunk 2).
//	POST /v1/identity/grants/{id}/revoke — revoke a live grant by id. Chunk
//	                                   2. Appends identity.grant.revoked to
//	                                   the ledger and drops the grant from
//	                                   the live index immediately; subsequent
//	                                   verifies for that token fail.
//	GET  /v1/identity/grants        — operator-facing inventory. NEVER
//	                                   returns a token value, per design §3.1.
//	GET  /v1/identity/grants/current?surface=X — returns the live grant's
//	                                   token for a named surface, if one
//	                                   exists AND its raw value is still
//	                                   cached in this process's memory. This
//	                                   is chunk 1's zero-paste primitive: a
//	                                   surface that already holds a grant
//	                                   (minted at its own boot) can be asked
//	                                   again by anything that shares the
//	                                   loopback bind, without a second
//	                                   /grants POST re-minting a second
//	                                   credential. Loopback-only threat model
//	                                   throughout (design §3.6); this
//	                                   endpoint returning a live raw token
//	                                   value is exactly as sensitive as
//	                                   /v1/identity/grants' POST response
//	                                   already is, not a new exposure class.
//	                                   Post-restart (chunk 2), a grant
//	                                   reconstructed from the ledger has NO
//	                                   raw token cached (the ledger, per
//	                                   ADR-091's hash-not-value discipline,
//	                                   §3.2, never stores the raw value) —
//	                                   this endpoint 404s in that case
//	                                   rather than lying with an empty
//	                                   string, same "shown once" discipline
//	                                   §3.2 already names.
//
// Chunk 2 — ledger-backed grants (design §3.2, "CHUNK 2 VERIFICATION NOTES"):
// every mint and revoke is a write-ahead ledger event
// (identity.grant.issued / identity.grant.revoked) on the kernel's existing
// hash-chained ledger (ledger.go's AppendEvent, same mechanism
// worktree_spawn.go's FilesystemLedgerWriter already uses for its own
// dedicated session bucket) BEFORE the in-memory index is mutated — the
// ledger is the source of truth, the in-memory IdentityGrantRegistry is a
// derived, rebuildable cache. The ledger event carries {grant_id, surface,
// scope, integrity_hash, issued_at/expires_at or revoked_at} — NEVER the raw
// token value (ADR-091 §5; same verifyKeyIntegrity hash-not-value pattern
// identity_provider.go already established). On kernel boot,
// RebuildIdentityGrantRegistryFromLedger replays every identity.grant.*
// event in the dedicated "identity-grants" ledger session bucket to
// reconstruct the live grant set — this is what makes a previously-issued
// grant still verify after a kernel restart (the chunk-1-to-2 verify tooth,
// and the fix for both filed chunk-1 lockout notes: the post-merge
// env-pin/restart lockout, since verify no longer depends on in-memory state
// surviving a restart; and the capacity-fill+restart lockout, since revoke
// now frees exactly one surface's slot without requiring a destructive
// full-registry-wiping kernel restart to "reset" the store).
//
// Honest limitation, not a bug: a grant reconstructed from the ledger has no
// raw token in memory (only its hash) — Verify() doesn't need it (hash
// comparison), but a re-mint of the SAME surface+scope after a kernel
// restart cannot honestly hand back the previously-issued raw bytes (the
// kernel deliberately never persisted them). MintOrReuse detects this case
// (tracked, unexpired, same scope, but Token == "") and mints a fresh grant,
// revoking the old grant_id first so the ledger and in-memory state agree
// (see MintOrReuse's doc comment). This only affects a surface that calls
// POST /v1/identity/grants again after a kernel restart; a surface merely
// presenting its already-held token to POST /v1/identity/verify is
// completely unaffected (that's the tooth chunk 2 must pass).
package engine

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// identityGrantsLedgerSession is the dedicated ledger session bucket for
// identity-grant events, following the same "infra events get a fixed
// session id, not a per-request one" convention worktree_spawn.go's
// FilesystemLedgerWriter already established ("worktree-reconciler"). Events
// land at .cog/ledger/identity-grants/events.jsonl.
const identityGrantsLedgerSession = "identity-grants"

// defaultGrantTTL matches nothing in particular yet (rotation cadence is an
// OPEN GATE, design §5.4) — 30 days is a reasonable default that outlives any
// single chat-server.py boot without being "forever."
const defaultGrantTTL = 30 * 24 * time.Hour

// IdentityGrant is the kernel's record of a surface-scoped credential.
// IntegrityHash (sha256 hex of the raw token) is the durable, ledger-backed
// source of truth for verification (design §3.2, chunk 2) — it survives a
// kernel restart via RebuildIdentityGrantRegistryFromLedger. Token is a
// same-boot-cycle-only cache of the raw value: populated when this process
// itself minted the grant, empty ("") when the grant was reconstructed from
// the ledger after a restart (the ledger never stores the raw value, per
// ADR-091 §5 and identity_provider.go's verifyKeyIntegrity precedent).
// Callers must check Token != "" before treating a grant as
// bootstrap-usable (see Current/handleIdentityGrantCurrent); Verify never
// needs Token, only IntegrityHash.
type IdentityGrant struct {
	GrantID       string    `json:"grant_id"`
	Surface       string    `json:"surface"`
	Scope         []string  `json:"scope"`
	Token         string    `json:"-"` // never serialized; empty after ledger rebuild
	IntegrityHash string    `json:"-"` // never serialized; sha256 hex of Token
	IssuedAt      time.Time `json:"issued_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func (g *IdentityGrant) expired(now time.Time) bool {
	return now.After(g.ExpiresAt)
}

// IdentityGrantRegistry is the in-memory store, keyed by surface (chunk 1 —
// one live grant per surface at a time, matching the "idempotent reuse on
// restart" verify-tooth). A secondary index by grant_id backs GET-by-id
// lookups (revoke, chunk 2) and by-token backs verify.
//
// workspaceRoot (chunk 2) is where AppendEvent writes identity.grant.*
// events; empty ("") means "no ledger" — mint/revoke still work but stay
// in-memory only, matching chunk 1's original behavior exactly (used by
// existing unit tests that construct the registry directly and don't care
// about ledger durability).
type IdentityGrantRegistry struct {
	mu            sync.RWMutex
	bySurface     map[string]*IdentityGrant
	byGrantID     map[string]*IdentityGrant
	workspaceRoot string
}

// NewIdentityGrantRegistry returns an empty, ledger-less registry (in-memory
// only — chunk 1's original behavior, preserved for existing unit tests and
// any caller that doesn't need restart-safety).
func NewIdentityGrantRegistry() *IdentityGrantRegistry {
	return NewIdentityGrantRegistryWithLedger("")
}

// NewIdentityGrantRegistryWithLedger returns an empty registry that appends
// identity.grant.issued/identity.grant.revoked events to the ledger under
// workspaceRoot on every mint/revoke (chunk 2, design §3.2). Pass "" for the
// same ledger-less behavior as NewIdentityGrantRegistry.
func NewIdentityGrantRegistryWithLedger(workspaceRoot string) *IdentityGrantRegistry {
	return &IdentityGrantRegistry{
		bySurface:     make(map[string]*IdentityGrant),
		byGrantID:     make(map[string]*IdentityGrant),
		workspaceRoot: workspaceRoot,
	}
}

// maxLiveGrantSurfaces bounds the number of distinct surfaces the registry
// will track at once. `surface` is an unauthenticated, caller-supplied
// string with no allowlist (cog-review finding, PR #471 second pass,
// serve_identity_grants.go:118 as of commit 7918055): without a cap, any
// loopback caller could grow bySurface/byGrantID without bound simply by
// varying the surface name on every mint call, eventually exhausting kernel
// memory. This is a coarse safety net appropriate to chunk 1's loopback-only
// threat model — a real allowlist/auth story is chunk 4's job, not a
// retrofit here.
const maxLiveGrantSurfaces = 256

// ErrGrantStoreAtCapacity is returned (wrapped) by MintOrReuse when a mint
// for a not-yet-tracked surface would exceed maxLiveGrantSurfaces, so the
// HTTP handler can map it to a distinct status/error code (429) rather than
// a generic 400.
var ErrGrantStoreAtCapacity = errors.New("grant store at capacity")

// MintOrReuse returns the live, unexpired grant for surface if one exists,
// its scope matches the requested scope exactly (as a set), AND its raw
// token is still cached in this process's memory; otherwise mints a fresh
// one with the given scope and TTL, replacing whatever grant (if any)
// previously lived at that surface key.
//
// The scope check matters: reusing a live grant regardless of what scope the
// *current* request asked for would silently echo back a stale, possibly
// broader-or-narrower scope than requested — contradicting this file's own
// "the response never echoes back a broader scope than the caller requested"
// invariant (handleIdentityGrantMint's doc comment, design §4 chunk-1
// verify-tooth #1) the moment a caller re-mints with a *different* scope
// than what's already live. A same-scope re-mint (the restart case, verify
// teeth #5) still reuses the existing token unchanged; a different-scope
// re-mint mints fresh rather than lying about what scope the returned token
// actually carries.
//
// The Token != "" check matters too (chunk 2, new): a grant reconstructed
// from the ledger after a kernel restart has a live, correctly-scoped entry
// in bySurface but NO cached raw token (the ledger never stores it — design
// §3.2). Reusing such an entry would either panic on an empty string or,
// worse, silently hand back "" as if it were a real credential. Instead this
// falls through to the mint-fresh path below, same as a scope change.
//
// Bounding: a mint for a surface NOT already tracked first evicts any
// expired entries (reclaiming space before growing), then rejects with an
// error if the store is still at maxLiveGrantSurfaces — this caps unbounded
// growth from an unauthenticated caller varying the surface string (cog-
// review finding, PR #471 second pass). A re-mint of an ALREADY-tracked
// surface (scope change, or lost-raw-token reissue) never counts against the
// cap, since it doesn't grow the number of distinct surfaces. Chunk 2 closes
// the "capacity-fill + restart" lockout note this way too: revoke (below)
// now frees a surface's slot without needing a destructive full-registry
// restart to reset the store, and a restart no longer silently resets the
// cap at all (ledger replay repopulates the same live set).
//
// byGrantID leak fix: when a re-mint supersedes an existing grant for the
// same surface, the superseded grant's byGrantID entry is deleted before the
// new one is inserted — otherwise byGrantID would grow by one permanently
// unreachable entry per scope-changing re-mint (cog-review finding, PR #471
// second pass, serve_identity_grants.go:147 as of commit 7918055). Chunk 2
// extends this: the supersession is also recorded in the ledger as an
// identity.grant.revoked event for the old grant_id BEFORE the new
// identity.grant.issued event, so a future ledger replay reconstructs the
// same single-live-grant-per-surface invariant instead of resurrecting both
// the old and new grant_id into byGrantID.
//
// Ledger-write-ahead (design §3.2 / ADR-091 §5): every ledger append below
// happens BEFORE the corresponding in-memory mutation. If an append fails,
// MintOrReuse returns the error without touching bySurface/byGrantID, so the
// in-memory index never claims a grant the ledger doesn't also record.
func (r *IdentityGrantRegistry) MintOrReuse(surface string, scope []string, ttl time.Duration) (*IdentityGrant, error) {
	if surface == "" {
		return nil, fmt.Errorf("surface is required")
	}
	now := time.Now().UTC()

	r.mu.Lock()
	defer r.mu.Unlock()

	existing, tracked := r.bySurface[surface]
	if tracked && !existing.expired(now) && existing.Token != "" && scopeSetEqual(existing.Scope, scope) {
		return existing, nil
	}

	if !tracked {
		r.evictExpiredLocked(now)
		if len(r.bySurface) >= maxLiveGrantSurfaces {
			return nil, fmt.Errorf("%w: grant store at capacity (%d live surfaces); cannot mint a grant for a new surface", ErrGrantStoreAtCapacity, maxLiveGrantSurfaces)
		}
	}

	token, err := mintGrantToken()
	if err != nil {
		return nil, fmt.Errorf("mint token: %w", err)
	}
	if ttl <= 0 {
		ttl = defaultGrantTTL
	}
	grant := &IdentityGrant{
		GrantID:       "grant-" + mustHex(6),
		Surface:       surface,
		Scope:         scope,
		Token:         token,
		IntegrityHash: sha256Hex(token),
		IssuedAt:      now,
		ExpiresAt:     now.Add(ttl),
	}

	if tracked {
		// Write-ahead: record the supersession in the ledger before mutating
		// memory, so a replay never resurrects the superseded grant_id.
		if err := r.appendGrantEventLocked("identity.grant.revoked", existing, now); err != nil {
			return nil, fmt.Errorf("ledger revoke (supersede): %w", err)
		}
	}
	if err := r.appendGrantEventLocked("identity.grant.issued", grant, now); err != nil {
		return nil, fmt.Errorf("ledger issue: %w", err)
	}

	if tracked {
		// Supersede: remove the old grant's byGrantID entry so the index
		// doesn't leak an unreachable entry on every scope-changing re-mint.
		delete(r.byGrantID, existing.GrantID)
	}
	r.bySurface[surface] = grant
	r.byGrantID[grant.GrantID] = grant
	return grant, nil
}

// evictExpiredLocked removes every expired grant from both bySurface and
// byGrantID. Caller must hold r.mu for writing. This is the store's only
// reclamation path for chunk 1 (no ledger, no revoke/rotate yet) — it runs
// opportunistically before a new-surface mint would otherwise grow the
// store, so a churn of short-TTL grants across many surface names doesn't
// accumulate permanently between restarts.
func (r *IdentityGrantRegistry) evictExpiredLocked(now time.Time) {
	for surface, g := range r.bySurface {
		if g.expired(now) {
			delete(r.bySurface, surface)
			delete(r.byGrantID, g.GrantID)
		}
	}
}

// Verify checks a presented token for a named surface against the live
// grant. Returns (grant, true) only when the surface has a live, unexpired
// grant AND the presented token's hash matches the grant's IntegrityHash.
//
// Hashing the presented token and comparing hashes (rather than comparing
// raw token to raw Token, as chunk 1 did) is the chunk-2 change that makes
// verification restart-safe: a grant reconstructed from the ledger has
// IntegrityHash populated but Token == "" (design §3.2), and this is the
// only method that needs to keep working identically in that case — the
// tooth "mint -> restart kernel -> grant still verifies" turns entirely on
// this comparison being hash-based, not raw-value-based.
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
	if !constantTimeEqual(g.IntegrityHash, sha256Hex(token)) {
		return nil, false
	}
	return g, true
}

// Current returns the live grant for a surface (including its raw token) —
// the zero-paste primitive: a surface's own page can ask the kernel "what do
// I currently hold" instead of the operator pasting anything. Returns
// (nil, false) if no live grant exists for that surface, it has expired, OR
// (chunk 2) its raw token isn't cached in this process's memory — a grant
// reconstructed from the ledger after a restart has no raw value to return
// (design §3.2's "shown once" discipline), so this deliberately reports
// "not currently available" rather than handing back an empty string as if
// it were a real credential. The caller (or an operator) should re-mint in
// that case, which MintOrReuse handles correctly (see its doc comment).
func (r *IdentityGrantRegistry) Current(surface string) (*IdentityGrant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.bySurface[surface]
	if !ok || g.expired(time.Now().UTC()) || g.Token == "" {
		return nil, false
	}
	return g, true
}

// Revoke removes the live grant identified by grantID, appending an
// identity.grant.revoked ledger event before mutating the in-memory index
// (write-ahead, same discipline as MintOrReuse). Returns (grant, true) on
// success; (nil, false) if grantID names no currently-live grant (already
// revoked, expired-and-evicted, or never existed) — the handler maps that to
// 404, matching the existing not-found shape used elsewhere in this file.
func (r *IdentityGrantRegistry) Revoke(grantID string) (*IdentityGrant, bool) {
	if grantID == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	g, ok := r.byGrantID[grantID]
	if !ok {
		return nil, false
	}
	if err := r.appendGrantEventLocked("identity.grant.revoked", g, time.Now().UTC()); err != nil {
		slog.Warn("identity_grants: ledger revoke append failed; grant remains live", "grant_id", grantID, "err", err)
		return nil, false
	}
	delete(r.byGrantID, grantID)
	// Only clear bySurface if this grant is still the CURRENT live one for
	// its surface — a superseded grant (already replaced by a later mint)
	// no longer occupies bySurface[surface], and revoking its stale
	// grant_id must not accidentally delete whatever surface entry now
	// lives there.
	if current, ok := r.bySurface[g.Surface]; ok && current.GrantID == grantID {
		delete(r.bySurface, g.Surface)
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

// appendGrantEventLocked writes one identity.grant.issued/identity.grant.
// revoked ledger event for grant. Caller must hold r.mu (for writing, since
// this is always called from a mutating path). No-op (returns nil) when
// r.workspaceRoot == "" — the ledger-less mode existing unit tests and any
// caller that doesn't want restart-safety rely on (see
// NewIdentityGrantRegistry's doc comment).
//
// The event payload carries {grant_id, surface, scope, integrity_hash,
// issued_at, expires_at} for BOTH event types (not just issued) — per the
// build directive, so RebuildIdentityGrantRegistryFromLedger can reconstruct
// full grant state from a revoked event alone (e.g. to answer "what was this
// grant's scope before it was revoked") without needing to cross-reference
// an already-possibly-evicted issued event. NEVER includes the raw token.
func (r *IdentityGrantRegistry) appendGrantEventLocked(eventType string, grant *IdentityGrant, now time.Time) error {
	if r.workspaceRoot == "" {
		return nil
	}
	data := map[string]interface{}{
		"grant_id":       grant.GrantID,
		"surface":        grant.Surface,
		"scope":          grant.Scope,
		"integrity_hash": grant.IntegrityHash,
		"issued_at":      grant.IssuedAt.Format(time.RFC3339),
		"expires_at":     grant.ExpiresAt.Format(time.RFC3339),
	}
	if eventType == "identity.grant.revoked" {
		data["revoked_at"] = now.Format(time.RFC3339)
	}
	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      eventType,
			Timestamp: now.Format(time.RFC3339),
			SessionID: identityGrantsLedgerSession,
			Data:      data,
		},
		Metadata: EventMetadata{Source: "identity-grants"},
	}
	return AppendEvent(r.workspaceRoot, identityGrantsLedgerSession, env)
}

// sha256Hex (defined in process.go) is the integrity-hash primitive this
// file uses in place of raw-token comparison (design §3.2, same
// hash-not-value shape identity_provider.go's verifyKeyIntegrity already
// established for resolved key bytes).

// RebuildIdentityGrantRegistryFromLedger reconstructs the live grant set by
// replaying every identity.grant.issued/identity.grant.revoked event from
// the "identity-grants" ledger session bucket, in file order (the ledger is
// append-only, so file order is event order). This is what makes a
// previously-issued grant still verify after a kernel restart — the design's
// chunk-1-to-2 verify tooth — and is called once, from NewServer, in place
// of a bare NewIdentityGrantRegistry() (chunk 1's behavior).
//
// A missing ledger file (no grant has ever been issued in this workspace) is
// not an error — returns an empty, ledger-backed registry so the FIRST mint
// in a fresh workspace still writes forward correctly. A malformed line is
// skipped (matches worktree_spawn.go's scanWorktreeEventsFile precedent:
// "skip malformed rows; do not fail the whole scan") rather than aborting
// the whole rebuild over one corrupt entry.
//
// Reconstructed grants have Token == "" (the ledger never stores the raw
// value) and IntegrityHash populated — sufficient for Verify, insufficient
// for Current/mint-reuse, exactly per this file's documented chunk-2
// semantics.
func RebuildIdentityGrantRegistryFromLedger(workspaceRoot string) (*IdentityGrantRegistry, error) {
	reg := NewIdentityGrantRegistryWithLedger(workspaceRoot)
	if workspaceRoot == "" {
		return reg, nil
	}
	path := filepath.Join(workspaceRoot, ".cog", "ledger", identityGrantsLedgerSession, "events.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return reg, fmt.Errorf("open identity-grants ledger: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Ledger lines can carry sizeable Data payloads; match the default
	// growth ceiling other ledger scanners in this package tolerate rather
	// than the bufio.Scanner default (64KiB), which is plenty here but kept
	// explicit so a future larger scope list doesn't silently truncate.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var env EventEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue // skip malformed rows; do not fail the whole rebuild
		}
		applyIdentityGrantLedgerEvent(reg, &env)
	}
	if err := scanner.Err(); err != nil {
		return reg, fmt.Errorf("scan identity-grants ledger: %w", err)
	}
	return reg, nil
}

// applyIdentityGrantLedgerEvent replays a single ledger event into reg's
// in-memory index. Unlocked mutation directly on the maps is safe here: this
// runs only during RebuildIdentityGrantRegistryFromLedger, before reg is
// published to any concurrent caller.
func applyIdentityGrantLedgerEvent(reg *IdentityGrantRegistry, env *EventEnvelope) {
	data := env.HashedPayload.Data
	grantID, _ := data["grant_id"].(string)
	surface, _ := data["surface"].(string)
	if grantID == "" || surface == "" {
		return
	}

	switch env.HashedPayload.Type {
	case "identity.grant.issued":
		scope := stringSliceFromAny(data["scope"])
		integrityHash, _ := data["integrity_hash"].(string)
		issuedAt := parseRFC3339Lenient(data["issued_at"])
		expiresAt := parseRFC3339Lenient(data["expires_at"])
		grant := &IdentityGrant{
			GrantID:       grantID,
			Surface:       surface,
			Scope:         scope,
			Token:         "", // never persisted — see file header
			IntegrityHash: integrityHash,
			IssuedAt:      issuedAt,
			ExpiresAt:     expiresAt,
		}
		reg.bySurface[surface] = grant
		reg.byGrantID[grantID] = grant
	case "identity.grant.revoked":
		delete(reg.byGrantID, grantID)
		if current, ok := reg.bySurface[surface]; ok && current.GrantID == grantID {
			delete(reg.bySurface, surface)
		}
	}
}

// stringSliceFromAny converts a decoded JSON []interface{} (json.Unmarshal's
// shape for a JSON array into map[string]interface{}) into []string,
// skipping any non-string element rather than failing the whole replay.
func stringSliceFromAny(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// parseRFC3339Lenient parses a decoded JSON string field as RFC3339,
// returning the zero time.Time on any failure (malformed ledger entry) so
// replay degrades to "already expired" rather than panicking.
func parseRFC3339Lenient(v interface{}) time.Time {
	s, ok := v.(string)
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// scopeSetEqual compares two scope lists as sets (order-independent,
// duplicate-insensitive) — a re-mint request listing the same capabilities
// in a different order is still a "same scope" reuse, not a re-scope.
func scopeSetEqual(a, b []string) bool {
	setA := make(map[string]struct{}, len(a))
	for _, s := range a {
		setA[s] = struct{}{}
	}
	setB := make(map[string]struct{}, len(b))
	for _, s := range b {
		setB[s] = struct{}{}
	}
	if len(setA) != len(setB) {
		return false
	}
	for s := range setA {
		if _, ok := setB[s]; !ok {
			return false
		}
	}
	return true
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
	s.route(mux, "POST /v1/identity/grants/{id}/revoke", s.handleIdentityGrantRevoke)
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
		if errors.Is(err, ErrGrantStoreAtCapacity) {
			writeJSONError(w, http.StatusTooManyRequests, "capacity_exceeded", err.Error())
			return
		}
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

// ─── POST /v1/identity/grants/{id}/revoke ────────────────────────────────────

type identityGrantRevokeResponse struct {
	Revoked bool   `json:"revoked"`
	GrantID string `json:"grant_id"`
	Surface string `json:"surface"`
}

// handleIdentityGrantRevoke is chunk 2's mechanism per design §3.4 (rotation
// = issue new + revoke old, using this same endpoint). Appends
// identity.grant.revoked to the ledger and drops the grant from the live
// index immediately (design §3.2: "verify always checks current ledger
// state") — a subsequent POST /v1/identity/verify for this grant's token
// fails right away; the only latency is whatever cache TTL a calling surface
// applies on its own side (design §3.4 note, not this endpoint's concern).
// 404 when grantID names no currently-live grant.
func (s *Server) handleIdentityGrantRevoke(w http.ResponseWriter, r *http.Request) {
	grantID := r.PathValue("id")
	if grantID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "grant id is required")
		return
	}
	grant, ok := s.identityGrants.Revoke(grantID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not_found", "no live grant with id "+grantID)
		return
	}
	writeJSONResp(w, http.StatusOK, identityGrantRevokeResponse{
		Revoked: true,
		GrantID: grant.GrantID,
		Surface: grant.Surface,
	})
}

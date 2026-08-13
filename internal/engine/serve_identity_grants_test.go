// serve_identity_grants_test.go — end-to-end tests for board task 60
// chunk 1 (kernel-issued identity grants, in-memory) and chunk 2
// (ledger-backed grants + revoke). Follows the newChannelServer pattern in
// serve_sessions_channel_test.go: a Server wired with just the fields the
// handlers need, fronted by httptest.NewServer.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newIdentityGrantServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := &Server{identityGrants: NewIdentityGrantRegistry()}
	mux := http.NewServeMux()
	s.registerIdentityGrantRoutes(mux)
	front := httptest.NewServer(mux)
	t.Cleanup(func() { front.Close() })
	return s, front
}

func identityPostJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func identityDecodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode body %s: %v", raw, err)
	}
}

func TestIdentityGrantMint_ScopeNeverWidened(t *testing.T) {
	_, front := newIdentityGrantServer(t)

	resp := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out identityGrantMintResponse
	identityDecodeBody(t, resp, &out)
	if out.Token == "" {
		t.Fatalf("expected a non-empty token")
	}
	if len(out.Scope) != 1 || out.Scope[0] != "chat:post" {
		t.Fatalf("expected scope to echo exactly what was requested, got %v", out.Scope)
	}
}

func TestIdentityGrantMint_IdempotentPerSurface(t *testing.T) {
	_, front := newIdentityGrantServer(t)

	first := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post", "chat:read"},
	})
	var firstOut identityGrantMintResponse
	identityDecodeBody(t, first, &firstOut)

	// Simulate a restart: mint again for the same surface. Must return the
	// SAME token, not invalidate whatever a client already holds (design
	// §4 chunk-1 verify-teeth item 5).
	second := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post", "chat:read"},
	})
	var secondOut identityGrantMintResponse
	identityDecodeBody(t, second, &secondOut)

	if firstOut.Token != secondOut.Token {
		t.Fatalf("expected idempotent reuse of the live grant, got different tokens: %q vs %q",
			firstOut.Token, secondOut.Token)
	}
	if firstOut.GrantID != secondOut.GrantID {
		t.Fatalf("expected the same grant_id on reuse, got %q vs %q", firstOut.GrantID, secondOut.GrantID)
	}
}

// TestIdentityGrantMint_ReuseWithDifferentScope guards the defect confirmed
// by cog-review on PR #471: MintOrReuse must not echo back a stale, live
// grant's scope when the *current* request asks for a different scope than
// what's already live. A same-scope re-mint (simulating a restart) still
// reuses the token unchanged (TestIdentityGrantMint_IdempotentPerSurface);
// a different-scope re-mint must mint fresh and return exactly the newly
// requested scope, never the old one.
func TestIdentityGrantMint_ReuseWithDifferentScope(t *testing.T) {
	_, front := newIdentityGrantServer(t)

	first := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "scope-test",
		"scope":   []string{"chat:post", "chat:admin"},
	})
	var firstOut identityGrantMintResponse
	identityDecodeBody(t, first, &firstOut)
	if len(firstOut.Scope) != 2 {
		t.Fatalf("expected initial mint to carry both requested scopes, got %v", firstOut.Scope)
	}

	// Re-mint the SAME surface with a NARROWER scope. The response must
	// reflect the newly requested scope, not the previously-stored broader
	// one — regardless of idempotency, which only applies to a same-scope
	// restart.
	second := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "scope-test",
		"scope":   []string{"chat:post"},
	})
	var secondOut identityGrantMintResponse
	identityDecodeBody(t, second, &secondOut)
	if len(secondOut.Scope) != 1 || secondOut.Scope[0] != "chat:post" {
		t.Fatalf("expected re-mint with a different scope to echo exactly the newly requested scope %v, got %v",
			[]string{"chat:post"}, secondOut.Scope)
	}
	if secondOut.Token == firstOut.Token {
		t.Fatalf("expected a different-scope re-mint to issue a fresh token, got the same one back")
	}

	// The old, broader token must no longer verify — it was superseded, not
	// left live alongside the new one (one live grant per surface, chunk 1's
	// stated invariant).
	oldVerify := identityPostJSON(t, front.URL+"/v1/identity/verify", map[string]any{
		"surface": "scope-test",
		"token":   firstOut.Token,
	})
	var oldOut identityVerifyResponse
	identityDecodeBody(t, oldVerify, &oldOut)
	if oldOut.Valid {
		t.Fatalf("expected the superseded broader-scope token to no longer verify")
	}
}

func TestIdentityVerify_ValidAndInvalidTokens(t *testing.T) {
	_, front := newIdentityGrantServer(t)

	mint := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})
	var minted identityGrantMintResponse
	identityDecodeBody(t, mint, &minted)

	// Valid token verifies.
	ok := identityPostJSON(t, front.URL+"/v1/identity/verify", map[string]any{
		"surface": "constellation-chat",
		"token":   minted.Token,
	})
	var okOut identityVerifyResponse
	identityDecodeBody(t, ok, &okOut)
	if !okOut.Valid {
		t.Fatalf("expected valid=true for a freshly minted token")
	}

	// Garbage token is rejected.
	bad := identityPostJSON(t, front.URL+"/v1/identity/verify", map[string]any{
		"surface": "constellation-chat",
		"token":   "garbage",
	})
	var badOut identityVerifyResponse
	identityDecodeBody(t, bad, &badOut)
	if badOut.Valid {
		t.Fatalf("expected valid=false for a garbage token")
	}

	// Right token, wrong surface is rejected — scope isolation across surfaces.
	wrongSurface := identityPostJSON(t, front.URL+"/v1/identity/verify", map[string]any{
		"surface": "signal-dashboard",
		"token":   minted.Token,
	})
	var wrongOut identityVerifyResponse
	identityDecodeBody(t, wrongSurface, &wrongOut)
	if wrongOut.Valid {
		t.Fatalf("expected valid=false when a token is presented for a different surface")
	}
}

func TestIdentityGrantList_NeverIncludesToken(t *testing.T) {
	_, front := newIdentityGrantServer(t)
	identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})

	resp, err := http.Get(front.URL + "/v1/identity/grants")
	if err != nil {
		t.Fatalf("GET grants: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if bytes.Contains(bytes.ToLower(raw), []byte(`"token"`)) {
		t.Fatalf("grant listing must never include a token field; got %s", raw)
	}
}

// TestIdentityGrantMint_ByGrantIDDoesNotLeakOnRescope guards the second
// cog-review-confirmed defect on PR #471 (serve_identity_grants.go:147 as of
// commit 7918055): a scope-changing re-mint for the same surface must not
// leave the superseded grant's byGrantID entry behind. Alternates the
// requested scope for one surface across several mints and asserts
// byGrantID never grows past one live entry.
func TestIdentityGrantMint_ByGrantIDDoesNotLeakOnRescope(t *testing.T) {
	reg := NewIdentityGrantRegistry()

	scopes := [][]string{{"a"}, {"b"}, {"a"}, {"b"}, {"a"}}
	var lastGrantID string
	for i, scope := range scopes {
		g, err := reg.MintOrReuse("scope-flapper", scope, 0)
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		lastGrantID = g.GrantID
	}

	if got := len(reg.bySurface); got != 1 {
		t.Fatalf("expected exactly one live surface entry, got %d", got)
	}
	if got := len(reg.byGrantID); got != 1 {
		t.Fatalf("expected byGrantID to hold exactly the one live grant after %d scope-alternating re-mints, got %d entries (leak)", len(scopes), got)
	}
	if _, ok := reg.byGrantID[lastGrantID]; !ok {
		t.Fatalf("expected byGrantID to contain the current live grant_id %q", lastGrantID)
	}
}

// TestIdentityGrantMint_BoundedGrowthAcrossManySurfaces guards the first
// cog-review-confirmed defect on PR #471 (serve_identity_grants.go:118 as of
// commit 7918055): an unauthenticated caller varying the surface string on
// every mint must not grow the store without bound. Mints well past
// maxLiveGrantSurfaces distinct, never-expiring surface names and asserts
// the registry rejects new surfaces once at capacity rather than growing
// forever.
func TestIdentityGrantMint_BoundedGrowthAcrossManySurfaces(t *testing.T) {
	reg := NewIdentityGrantRegistry()

	var lastErr error
	minted := 0
	for i := 0; i < maxLiveGrantSurfaces+50; i++ {
		surface := fmt.Sprintf("surface-%d", i)
		_, err := reg.MintOrReuse(surface, []string{"x"}, 24*time.Hour)
		if err != nil {
			lastErr = err
			continue
		}
		minted++
	}

	if minted > maxLiveGrantSurfaces {
		t.Fatalf("expected at most %d live surfaces to ever be mintable, got %d minted successfully", maxLiveGrantSurfaces, minted)
	}
	if len(reg.bySurface) > maxLiveGrantSurfaces {
		t.Fatalf("expected bySurface to stay bounded at %d, got %d entries", maxLiveGrantSurfaces, len(reg.bySurface))
	}
	if lastErr == nil {
		t.Fatalf("expected minting past capacity to eventually fail, but all %d mints succeeded", maxLiveGrantSurfaces+50)
	}
	if !errors.Is(lastErr, ErrGrantStoreAtCapacity) {
		t.Fatalf("expected the over-capacity error to wrap ErrGrantStoreAtCapacity, got: %v", lastErr)
	}
}

// TestIdentityGrantMint_CapacityErrorMapsTo429 checks the HTTP-layer mapping
// for the capacity error (429, not the generic 400 every other MintOrReuse
// error uses) so a caller/monitoring surface can distinguish "malformed
// request" from "store full."
func TestIdentityGrantMint_CapacityErrorMapsTo429(t *testing.T) {
	s := &Server{identityGrants: NewIdentityGrantRegistry()}
	mux := http.NewServeMux()
	s.registerIdentityGrantRoutes(mux)
	front := httptest.NewServer(mux)
	defer front.Close()

	var lastResp *http.Response
	for i := 0; i < maxLiveGrantSurfaces+5; i++ {
		lastResp = identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
			"surface": fmt.Sprintf("http-surface-%d", i),
			"scope":   []string{"x"},
		})
		if lastResp.StatusCode == http.StatusTooManyRequests {
			break
		}
		lastResp.Body.Close()
	}
	if lastResp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected a 429 once the grant store hit capacity, last status was %d", lastResp.StatusCode)
	}
	lastResp.Body.Close()
}

func TestIdentityGrantCurrent_ZeroPasteBootstrap(t *testing.T) {
	_, front := newIdentityGrantServer(t)

	// No grant minted yet -> 404, so a caller knows to mint or degrade.
	miss, err := http.Get(front.URL + "/v1/identity/grants/current?surface=constellation-chat")
	if err != nil {
		t.Fatalf("GET current: %v", err)
	}
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 before any grant exists, got %d", miss.StatusCode)
	}
	miss.Body.Close()

	minted := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})
	var mintedOut identityGrantMintResponse
	identityDecodeBody(t, minted, &mintedOut)

	// Now the surface's own page can bootstrap with zero paste: GET current
	// returns the SAME live token, no operator action involved.
	hit, err := http.Get(front.URL + "/v1/identity/grants/current?surface=constellation-chat")
	if err != nil {
		t.Fatalf("GET current: %v", err)
	}
	var hitOut identityGrantMintResponse
	identityDecodeBody(t, hit, &hitOut)
	if hitOut.Token != mintedOut.Token {
		t.Fatalf("expected GET current to return the live grant's token unchanged, got %q vs %q",
			hitOut.Token, mintedOut.Token)
	}
}

// ─── chunk 2: ledger-backed grants + revoke ──────────────────────────────────

// TestIdentityGrantMint_LedgerRestartTooth is THE tooth chunk 1 could not
// pass and chunk 2 must (design's "CHUNK 1 VERIFICATION NOTES" / chunk 2
// verify-teeth): mint a grant against a ledger-backed registry, simulate a
// kernel restart by discarding the in-memory registry and reconstructing a
// fresh one from the same workspace's ledger, and confirm the pre-restart
// token still verifies — via the ledger-derived integrity hash, not memory
// that didn't survive.
func TestIdentityGrantMint_LedgerRestartTooth(t *testing.T) {
	workspaceRoot := t.TempDir()
	t.Cleanup(resetLedgerCacheForTest)
	resetLedgerCacheForTest()

	before := NewIdentityGrantRegistryWithLedger(workspaceRoot)
	grant, err := before.MintOrReuse("constellation-chat", []string{"chat:post", "chat:read"}, 0)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := grant.Token
	if token == "" {
		t.Fatalf("expected a non-empty token from a fresh mint")
	}

	// Simulate a kernel restart: throw away `before` entirely and rebuild
	// from the ledger alone, exactly what NewServer does on boot.
	after, err := RebuildIdentityGrantRegistryFromLedger(workspaceRoot)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	g, ok := after.Verify("constellation-chat", token)
	if !ok {
		t.Fatalf("expected the pre-restart token to still verify after ledger rebuild")
	}
	if len(g.Scope) != 2 {
		t.Fatalf("expected rebuilt grant to carry the original scope, got %v", g.Scope)
	}

	// Garbage token must still fail post-rebuild (rebuild doesn't
	// accidentally make verification permissive).
	if _, ok := after.Verify("constellation-chat", "garbage"); ok {
		t.Fatalf("expected a garbage token to fail verification after rebuild")
	}

	// The rebuilt grant must NOT carry the raw token in memory (design §3.2
	// — the ledger never stores it, so a fresh boot can't reconstruct it).
	if g.Token != "" {
		t.Fatalf("expected the rebuilt grant to have no cached raw token, got %q", g.Token)
	}

	// GET-current-style zero-paste bootstrap must honestly 404 post-rebuild
	// rather than hand back an empty string as if it were a real token.
	if _, ok := after.Current("constellation-chat"); ok {
		t.Fatalf("expected Current to report unavailable for a ledger-rebuilt grant with no cached raw token")
	}
}

// TestIdentityGrantRevoke_FailsVerifyImmediately covers design §3.4/§4
// chunk-2 verify-teeth: revoking a grant makes it fail verification right
// away, with no kernel restart or TTL wait involved.
func TestIdentityGrantRevoke_FailsVerifyImmediately(t *testing.T) {
	_, front := newLedgerBackedIdentityGrantServer(t, t.TempDir())

	mint := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})
	var minted identityGrantMintResponse
	identityDecodeBody(t, mint, &minted)

	revokeResp, err := http.Post(front.URL+"/v1/identity/grants/"+minted.GrantID+"/revoke", "application/json", nil)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revokeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on revoke, got %d", revokeResp.StatusCode)
	}
	revokeResp.Body.Close()

	verify := identityPostJSON(t, front.URL+"/v1/identity/verify", map[string]any{
		"surface": "constellation-chat",
		"token":   minted.Token,
	})
	var verifyOut identityVerifyResponse
	identityDecodeBody(t, verify, &verifyOut)
	if verifyOut.Valid {
		t.Fatalf("expected a revoked grant's token to fail verification immediately")
	}

	// Revoking an already-revoked (now-gone) grant_id 404s rather than
	// silently succeeding twice.
	again, err := http.Post(front.URL+"/v1/identity/grants/"+minted.GrantID+"/revoke", "application/json", nil)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if again.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 revoking an already-revoked grant, got %d", again.StatusCode)
	}
	again.Body.Close()
}

// TestIdentityGrantRevoke_ScopeIsolation is design §4 chunk-3's forward-
// looking verify-tooth, applicable already at chunk 2: revoking one
// surface's grant must not disturb a second, independently-scoped surface's
// live grant.
func TestIdentityGrantRevoke_ScopeIsolation(t *testing.T) {
	_, front := newLedgerBackedIdentityGrantServer(t, t.TempDir())

	chatMint := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})
	var chatOut identityGrantMintResponse
	identityDecodeBody(t, chatMint, &chatOut)

	signalMint := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "signal-dashboard",
		"scope":   []string{"signal:post"},
	})
	var signalOut identityGrantMintResponse
	identityDecodeBody(t, signalMint, &signalOut)

	revokeResp, err := http.Post(front.URL+"/v1/identity/grants/"+chatOut.GrantID+"/revoke", "application/json", nil)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	revokeResp.Body.Close()

	signalVerify := identityPostJSON(t, front.URL+"/v1/identity/verify", map[string]any{
		"surface": "signal-dashboard",
		"token":   signalOut.Token,
	})
	var signalVerifyOut identityVerifyResponse
	identityDecodeBody(t, signalVerify, &signalVerifyOut)
	if !signalVerifyOut.Valid {
		t.Fatalf("expected revoking constellation-chat's grant to leave signal-dashboard's grant untouched")
	}
}

// TestIdentityGrantMint_CapacityFreedByRevokeNotJustRestart guards the
// "capacity-fill + restart" lockout note filed against chunk 1: previously
// the only way past a full grant store was a kernel restart that wiped
// EVERY live grant (destructive). Chunk 2's revoke frees exactly one
// surface's slot without disturbing any other live grant, and — since the
// store is now ledger-backed — a restart no longer silently resets the cap
// either (covered by the restart tooth above); this test exercises the
// non-destructive remedy directly.
func TestIdentityGrantMint_CapacityFreedByRevokeNotJustRestart(t *testing.T) {
	reg := NewIdentityGrantRegistryWithLedger(t.TempDir())
	t.Cleanup(resetLedgerCacheForTest)

	var firstGrantID string
	for i := 0; i < maxLiveGrantSurfaces; i++ {
		g, err := reg.MintOrReuse(fmt.Sprintf("cap-surface-%d", i), []string{"x"}, 24*time.Hour)
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		if i == 0 {
			firstGrantID = g.GrantID
		}
	}

	// Store is now exactly at capacity — one more distinct surface must fail.
	if _, err := reg.MintOrReuse("cap-surface-overflow", []string{"x"}, 24*time.Hour); !errors.Is(err, ErrGrantStoreAtCapacity) {
		t.Fatalf("expected capacity error before any revoke, got %v", err)
	}

	// Revoke frees exactly one slot.
	if _, err := reg.Revoke(firstGrantID); err != nil {
		t.Fatalf("expected revoke of a live grant to succeed, got: %v", err)
	}

	if _, err := reg.MintOrReuse("cap-surface-overflow", []string{"x"}, 24*time.Hour); err != nil {
		t.Fatalf("expected minting a new surface to succeed after revoke freed a slot, got: %v", err)
	}

	// Every OTHER previously-minted surface (besides the revoked one) must
	// still verify — revoke must not have disturbed them.
	if _, ok := reg.bySurface["cap-surface-1"]; !ok {
		t.Fatalf("expected an unrelated surface's grant to survive another surface's revoke")
	}
}

// TestIdentityGrantMint_ReMintAfterLedgerRebuildIssuesFresh documents the
// honest limitation named in this file's header: a same-scope re-mint for a
// surface whose grant was reconstructed from the ledger (kernel restart)
// cannot reuse the old raw token (never persisted) and must mint fresh,
// while correctly retiring the old grant_id so byGrantID doesn't leak
// across a restart-then-remint sequence.
func TestIdentityGrantMint_ReMintAfterLedgerRebuildIssuesFresh(t *testing.T) {
	workspaceRoot := t.TempDir()
	t.Cleanup(resetLedgerCacheForTest)
	resetLedgerCacheForTest()

	before := NewIdentityGrantRegistryWithLedger(workspaceRoot)
	original, err := before.MintOrReuse("constellation-chat", []string{"chat:post"}, 0)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	after, err := RebuildIdentityGrantRegistryFromLedger(workspaceRoot)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	reminted, err := after.MintOrReuse("constellation-chat", []string{"chat:post"}, 0)
	if err != nil {
		t.Fatalf("re-mint after rebuild: %v", err)
	}
	if reminted.Token == "" {
		t.Fatalf("expected a fresh, non-empty token from the post-rebuild re-mint")
	}
	if reminted.GrantID == original.GrantID {
		t.Fatalf("expected a fresh grant_id, not reuse of the pre-restart grant_id")
	}

	// The pre-restart token must no longer verify — it was superseded, not
	// left live alongside the new one.
	if _, ok := after.Verify("constellation-chat", original.Token); ok {
		t.Fatalf("expected the pre-restart token to fail verification once superseded by a post-rebuild re-mint")
	}
	// The new token verifies fine.
	if _, ok := after.Verify("constellation-chat", reminted.Token); !ok {
		t.Fatalf("expected the freshly re-minted token to verify")
	}
	// byGrantID must not leak the superseded grant_id.
	if _, ok := after.byGrantID[original.GrantID]; ok {
		t.Fatalf("expected the superseded pre-restart grant_id to be gone from byGrantID after re-mint")
	}
}

// TestIdentityGrantLedger_NeverStoresRawToken guards the ADR-091 /
// design-§3.2 invariant directly against the ledger file on disk: no ledger
// line for any identity.grant.* event may contain the raw token value.
func TestIdentityGrantLedger_NeverStoresRawToken(t *testing.T) {
	workspaceRoot := t.TempDir()
	t.Cleanup(resetLedgerCacheForTest)
	resetLedgerCacheForTest()

	reg := NewIdentityGrantRegistryWithLedger(workspaceRoot)
	grant, err := reg.MintOrReuse("constellation-chat", []string{"chat:post"}, 0)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := reg.Revoke(grant.GrantID); err != nil {
		t.Fatalf("expected revoke to succeed, got: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(workspaceRoot, ".cog", "ledger", "identity-grants", "events.jsonl"))
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}
	if bytes.Contains(raw, []byte(grant.Token)) {
		t.Fatalf("ledger file must never contain the raw token value; found it in: %s", raw)
	}
	if !bytes.Contains(raw, []byte(grant.IntegrityHash)) {
		t.Fatalf("expected the ledger to record the grant's integrity hash; got: %s", raw)
	}
	if !bytes.Contains(raw, []byte("identity.grant.issued")) || !bytes.Contains(raw, []byte("identity.grant.revoked")) {
		t.Fatalf("expected both issued and revoked events in the ledger; got: %s", raw)
	}
}

// TestIdentityGrantMint_SupersedeReplaysAtomically guards the replay side of
// the cog-review fix on PR #472 (serve_identity_grants.go:302 as of commit
// de5cbfe): a scope-changing re-mint of an already-tracked surface now
// writes a single identity.grant.superseded event (not a separate revoked +
// issued pair). This confirms that event alone, replayed through
// RebuildIdentityGrantRegistryFromLedger, both retires the old grant_id AND
// installs the new grant as live for the surface — one ledger line, two
// coordinated effects.
func TestIdentityGrantMint_SupersedeReplaysAtomically(t *testing.T) {
	workspaceRoot := t.TempDir()
	t.Cleanup(resetLedgerCacheForTest)
	resetLedgerCacheForTest()

	reg := NewIdentityGrantRegistryWithLedger(workspaceRoot)
	original, err := reg.MintOrReuse("constellation-chat", []string{"chat:post"}, 0)
	if err != nil {
		t.Fatalf("initial mint: %v", err)
	}
	reminted, err := reg.MintOrReuse("constellation-chat", []string{"chat:admin"}, 0)
	if err != nil {
		t.Fatalf("scope-changing re-mint: %v", err)
	}
	if reminted.GrantID == original.GrantID {
		t.Fatalf("expected a fresh grant_id for the scope-changing re-mint")
	}

	raw, err := os.ReadFile(filepath.Join(workspaceRoot, ".cog", "ledger", "identity-grants", "events.jsonl"))
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}
	if !bytes.Contains(raw, []byte("identity.grant.superseded")) {
		t.Fatalf("expected the supersession to write a single identity.grant.superseded event; ledger: %s", raw)
	}
	if bytes.Contains(raw, []byte("identity.grant.revoked")) {
		t.Fatalf("expected NO identity.grant.revoked event from a supersession (that was the two-append shape this fix replaces); ledger: %s", raw)
	}

	after, err := RebuildIdentityGrantRegistryFromLedger(workspaceRoot)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if _, ok := after.byGrantID[original.GrantID]; ok {
		t.Fatalf("expected the superseded grant_id to be retired by replaying the single superseded event")
	}
	if g, ok := after.byGrantID[reminted.GrantID]; !ok || g.Surface != "constellation-chat" {
		t.Fatalf("expected the new grant_id to be live for constellation-chat after replay")
	}
	if _, ok := after.Verify("constellation-chat", original.Token); ok {
		t.Fatalf("expected the superseded token to fail verification after replay")
	}
	if _, ok := after.Verify("constellation-chat", reminted.Token); !ok {
		t.Fatalf("expected the new token to verify after replay")
	}
}

// TestIdentityGrantMint_SupersedeAppendFailureLeavesOldGrantLive is the
// injected-failure test for the cog-review finding on PR #472
// (serve_identity_grants.go:302 as of commit de5cbfe). The finding: the OLD
// MintOrReuse wrote a supersession as two independent ledger appends
// (revoke-old, then issue-new) and only rolled back the in-memory mutation
// if the SECOND append failed — so a failure isolated to that second append
// left a durable revoke-with-no-reissue on disk, and the surface's still-
// valid old grant would silently vanish on the next restart (the exact
// lockout class this chunk exists to close).
//
// This test forces the (now single, atomic-by-construction) supersession
// append to fail via the registry's injectable appendEvent seam, and proves
// the failure mode is fully closed:
//  1. MintOrReuse returns an error.
//  2. The in-memory index is untouched — the old grant is still exactly
//     what it was, in both bySurface and byGrantID.
//  3. The old grant still verifies immediately (no window of silent
//     invalidation).
//  4. Simulating a restart (rebuild from the ledger, which the failed
//     append never touched) still finds the old grant live — the revoke-
//     with-no-reissue state that used to be reachable here is now
//     unreachable by construction, since there was only ever one append to
//     fail, and it failed cleanly with zero side effects.
func TestIdentityGrantMint_SupersedeAppendFailureLeavesOldGrantLive(t *testing.T) {
	workspaceRoot := t.TempDir()
	t.Cleanup(resetLedgerCacheForTest)
	resetLedgerCacheForTest()

	reg := NewIdentityGrantRegistryWithLedger(workspaceRoot)
	original, err := reg.MintOrReuse("constellation-chat", []string{"chat:post"}, 0)
	if err != nil {
		t.Fatalf("initial mint: %v", err)
	}

	// Inject a failure simulating transient I/O trouble (disk full, fd
	// exhaustion) on exactly the append the supersession path makes.
	injectedErr := errors.New("simulated append failure: disk full")
	reg.appendEvent = func(workspaceRoot, sessionID string, envelope *EventEnvelope) error {
		return injectedErr
	}

	_, err = reg.MintOrReuse("constellation-chat", []string{"chat:admin"}, 0)
	if err == nil {
		t.Fatalf("expected the scope-changing re-mint to fail when the ledger append fails")
	}
	if !errors.Is(err, injectedErr) {
		t.Fatalf("expected MintOrReuse's error to wrap the injected append failure, got: %v", err)
	}

	// In-memory state must be completely untouched by the failed attempt.
	if got := reg.bySurface["constellation-chat"]; got == nil || got.GrantID != original.GrantID {
		t.Fatalf("expected bySurface to still hold the original grant untouched, got %+v", got)
	}
	if got, ok := reg.byGrantID[original.GrantID]; !ok || got.GrantID != original.GrantID {
		t.Fatalf("expected byGrantID to still hold the original grant_id untouched")
	}
	if got := len(reg.byGrantID); got != 1 {
		t.Fatalf("expected exactly one live grant_id (no phantom new grant tracked despite the failed append), got %d", got)
	}
	if got := len(reg.bySurface); got != 1 {
		t.Fatalf("expected exactly one live surface entry, got %d", got)
	}

	// The old token must still verify — no silent invalidation window.
	if _, ok := reg.Verify("constellation-chat", original.Token); !ok {
		t.Fatalf("expected the original grant to still verify immediately after a failed supersession append")
	}

	// Simulate a restart: rebuild from the ledger (untouched by the failed
	// append) using a fresh registry with the real, non-failing writer. The
	// original grant must still be the one and only live grant for this
	// surface — the exact restart-lockout scenario the finding described is
	// now unreachable.
	after, err := RebuildIdentityGrantRegistryFromLedger(workspaceRoot)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if _, ok := after.Verify("constellation-chat", original.Token); !ok {
		t.Fatalf("expected the original grant to still verify after a simulated restart following a failed supersession append")
	}
	if _, ok := after.byGrantID[original.GrantID]; !ok {
		t.Fatalf("expected the original grant_id to still be tracked after restart")
	}
}

// TestIdentityGrantRevoke_AppendFailureReports503NotFakeNotFound guards the
// cog-review finding on PR #472 second pass (serve_identity_grants.go:444 as
// of commit 280aa1a): a ledger-append failure during revoke must NOT be
// reported the same way as "grant not found." Previously Revoke returned the
// identical (nil, false) for both, so the endpoint 404'd — reading as
// "already revoked" — while the grant stayed fully live and its token kept
// verifying: a caller trying to kill a leaked credential was told it was
// dead when it wasn't. This test injects an append failure and proves:
//  1. The HTTP revoke returns 503 (retryable infra fault), not 404.
//  2. The grant is still live and its token still verifies (write-ahead
//     honored — memory untouched on a failed append).
//  3. Once the ledger recovers, the same revoke succeeds (200) and the
//     token stops verifying — the failure was transient, not masked.
func TestIdentityGrantRevoke_AppendFailureReports503NotFakeNotFound(t *testing.T) {
	workspaceRoot := t.TempDir()
	s, front := newLedgerBackedIdentityGrantServer(t, workspaceRoot)

	mint := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})
	var minted identityGrantMintResponse
	identityDecodeBody(t, mint, &minted)

	// Break the ledger writer, simulating transient I/O failure.
	injectedErr := errors.New("simulated append failure: disk full")
	s.identityGrants.appendEvent = func(workspaceRoot, sessionID string, envelope *EventEnvelope) error {
		return injectedErr
	}

	failResp, err := http.Post(front.URL+"/v1/identity/grants/"+minted.GrantID+"/revoke", "application/json", nil)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if failResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the revoke's ledger append fails (grant still live), got %d", failResp.StatusCode)
	}
	failResp.Body.Close()

	// The grant must still be live: write-ahead means a failed append
	// mutates nothing, and the caller was told so (503, not 404).
	verify := identityPostJSON(t, front.URL+"/v1/identity/verify", map[string]any{
		"surface": "constellation-chat",
		"token":   minted.Token,
	})
	var verifyOut identityVerifyResponse
	identityDecodeBody(t, verify, &verifyOut)
	if !verifyOut.Valid {
		t.Fatalf("expected the grant to still verify after a failed (503) revoke — nothing durable was written")
	}

	// Ledger recovers: the same revoke now succeeds and the token dies.
	s.identityGrants.appendEvent = AppendEvent
	okResp, err := http.Post(front.URL+"/v1/identity/grants/"+minted.GrantID+"/revoke", "application/json", nil)
	if err != nil {
		t.Fatalf("retry revoke: %v", err)
	}
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("expected the retried revoke to succeed once the ledger recovered, got %d", okResp.StatusCode)
	}
	okResp.Body.Close()

	verifyAfter := identityPostJSON(t, front.URL+"/v1/identity/verify", map[string]any{
		"surface": "constellation-chat",
		"token":   minted.Token,
	})
	var verifyAfterOut identityVerifyResponse
	identityDecodeBody(t, verifyAfter, &verifyAfterOut)
	if verifyAfterOut.Valid {
		t.Fatalf("expected the token to stop verifying after the successful retried revoke")
	}
}

// TestIdentityGrantMint_LedgerAppendFailureMapsTo503 guards the second
// cog-review finding on PR #472 second pass (serve_identity_grants.go:850 as
// of commit 280aa1a): a ledger-append failure during mint is a server-side
// durability fault and must map to 503 (retryable), not 400 "invalid_request"
// (non-retryable client error).
func TestIdentityGrantMint_LedgerAppendFailureMapsTo503(t *testing.T) {
	s, front := newLedgerBackedIdentityGrantServer(t, t.TempDir())

	injectedErr := errors.New("simulated append failure: disk full")
	s.identityGrants.appendEvent = func(workspaceRoot, sessionID string, envelope *EventEnvelope) error {
		return injectedErr
	}

	resp := identityPostJSON(t, front.URL+"/v1/identity/grants", map[string]any{
		"surface": "constellation-chat",
		"scope":   []string{"chat:post"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the mint's ledger append fails, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(raw, []byte("ledger_unavailable")) {
		t.Fatalf("expected the 503 body to carry the ledger_unavailable error code, got: %s", raw)
	}
}

// newLedgerBackedIdentityGrantServer mirrors newIdentityGrantServer but
// wires a ledger-backed registry rooted at workspaceRoot, for chunk-2 tests
// that need real ledger writes (revoke, capacity+revoke interaction).
func newLedgerBackedIdentityGrantServer(t *testing.T, workspaceRoot string) (*Server, *httptest.Server) {
	t.Helper()
	t.Cleanup(resetLedgerCacheForTest)
	resetLedgerCacheForTest()
	s := &Server{identityGrants: NewIdentityGrantRegistryWithLedger(workspaceRoot)}
	mux := http.NewServeMux()
	s.registerIdentityGrantRoutes(mux)
	front := httptest.NewServer(mux)
	t.Cleanup(func() { front.Close() })
	return s, front
}

// ── ExtendGrant: node-root TTL renewal (boot_node_root_grant.go followup) ──

// TestExtendGrant_ExtendsWithoutChangingTokenHash is the core renewal tooth:
// consumers of a long-lived grant (the dashboard, canvas, THESEUS) cache the
// RAW TOKEN VALUE, so a renewal must extend the existing credential's expiry
// rather than mint a replacement — minting a replacement would change
// GrantID/Token/IntegrityHash and silently invalidate every cached copy.
func TestExtendGrant_ExtendsWithoutChangingTokenHash(t *testing.T) {
	reg := NewIdentityGrantRegistry()
	grant, err := reg.MintOrReuse("node-root", []string{"node-root"}, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	originalGrantID := grant.GrantID
	originalToken := grant.Token
	originalHash := grant.IntegrityHash
	originalExpiresAt := grant.ExpiresAt

	extended, err := reg.ExtendGrant("node-root", 2*time.Hour)
	if err != nil {
		t.Fatalf("extend: %v", err)
	}

	if extended.GrantID != originalGrantID {
		t.Errorf("GrantID changed on extend: got %q, want %q", extended.GrantID, originalGrantID)
	}
	if extended.Token != originalToken {
		t.Errorf("Token changed on extend: got %q, want %q — consumers cache this raw value", extended.Token, originalToken)
	}
	if extended.IntegrityHash != originalHash {
		t.Errorf("IntegrityHash changed on extend: got %q, want %q", extended.IntegrityHash, originalHash)
	}
	if !extended.ExpiresAt.After(originalExpiresAt) {
		t.Errorf("ExpiresAt did not advance: got %v, want after %v", extended.ExpiresAt, originalExpiresAt)
	}

	// The original token must still verify post-extend — extension is not a
	// supersession, so the pre-extend token is not invalidated.
	if _, ok := reg.Verify("node-root", originalToken); !ok {
		t.Fatalf("expected the original token to still verify after extend")
	}
}

// TestExtendGrant_UnknownSurfaceReturnsErrGrantNotFound covers ExtendGrant's
// contract for a surface with no live grant at all (never minted).
func TestExtendGrant_UnknownSurfaceReturnsErrGrantNotFound(t *testing.T) {
	reg := NewIdentityGrantRegistry()
	if _, err := reg.ExtendGrant("node-root", time.Hour); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("expected ErrGrantNotFound for an unknown surface, got %v", err)
	}
}

// TestExtendGrant_ExpiredGrantMintsFreshInstead is the documented edge case:
// extending an already-expired grant is NOT a renewal (see ExtendGrant's doc
// comment) — it returns ErrGrantNotFound so the caller (renewNodeRootGrant in
// boot_node_root_grant.go) falls through to mint-or-recover a genuinely fresh
// grant instead of silently un-expiring a stale one with the same token.
func TestExtendGrant_ExpiredGrantMintsFreshInstead(t *testing.T) {
	reg := NewIdentityGrantRegistry()
	if _, err := reg.MintOrReuse("node-root", []string{"node-root"}, time.Millisecond); err != nil {
		t.Fatalf("mint: %v", err)
	}
	time.Sleep(5 * time.Millisecond) // let the short TTL lapse

	if _, err := reg.ExtendGrant("node-root", time.Hour); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("expected ErrGrantNotFound for an expired grant (not a renewal target), got %v", err)
	}

	// The documented fallback: mint-or-recover establishes a fresh grant, as
	// renewNodeRootGrant does on this exact error.
	fresh, err := reg.MintOrReuse("node-root", []string{"node-root"}, time.Hour)
	if err != nil {
		t.Fatalf("fallback mint: %v", err)
	}
	if fresh.Token == "" {
		t.Fatalf("expected a fresh mint to produce a usable token")
	}
}

// TestExtendGrant_ReplayHonorsExtendedExpiry is the restart tooth for
// renewal: an identity.grant.extended event appended before a simulated
// kernel restart must leave the rebuilt grant's ExpiresAt at the EXTENDED
// value, not the original mint's expires_at — otherwise a renewed grant
// would silently revert to its pre-renewal (possibly already-past) expiry
// the moment the kernel restarted.
func TestExtendGrant_ReplayHonorsExtendedExpiry(t *testing.T) {
	workspaceRoot := t.TempDir()
	t.Cleanup(resetLedgerCacheForTest)
	resetLedgerCacheForTest()

	before := NewIdentityGrantRegistryWithLedger(workspaceRoot)
	grant, err := before.MintOrReuse("node-root", []string{"node-root"}, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := grant.Token

	extended, err := before.ExtendGrant("node-root", 48*time.Hour)
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	// RFC3339 (the ledger's on-disk timestamp format) is second-precision, so
	// round-trip through it here to match what replay will actually produce.
	extendedExpiresAt, err := time.Parse(time.RFC3339, extended.ExpiresAt.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("parse extended.ExpiresAt: %v", err)
	}

	// Simulate a kernel restart: throw away `before`, rebuild from the ledger
	// alone (mint event + extend event, replayed in file order).
	after, err := RebuildIdentityGrantRegistryFromLedger(workspaceRoot)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	g, ok := after.Verify("node-root", token)
	if !ok {
		t.Fatalf("expected the pre-restart token to still verify after ledger rebuild")
	}
	if !g.ExpiresAt.Equal(extendedExpiresAt) {
		t.Fatalf("rebuilt grant's ExpiresAt = %v, want the extended value %v (replay must honor the extend, not just the original issue)",
			g.ExpiresAt, extendedExpiresAt)
	}

	// A stale extend event for a since-superseded grant_id must not resurrect
	// into bySurface — belt-and-suspenders on the "current live grant" guard
	// in applyIdentityGrantLedgerEvent's "extended" case. Re-mint with a
	// different scope to supersede, then confirm the surface now serves the
	// superseding grant, not something an old extend event could revive.
	reMinted, err := before.MintOrReuse("node-root", []string{"node-root", "extra-scope"}, time.Hour)
	if err != nil {
		t.Fatalf("re-mint (supersede): %v", err)
	}
	afterSupersede, err := RebuildIdentityGrantRegistryFromLedger(workspaceRoot)
	if err != nil {
		t.Fatalf("rebuild after supersede: %v", err)
	}
	if _, ok := afterSupersede.Verify("node-root", token); ok {
		t.Fatalf("expected the superseded (pre-supersession) token to no longer verify")
	}
	if _, ok := afterSupersede.Verify("node-root", reMinted.Token); !ok {
		t.Fatalf("expected the superseding grant's token to verify")
	}
}

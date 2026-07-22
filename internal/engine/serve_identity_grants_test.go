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
	if _, ok := reg.Revoke(firstGrantID); !ok {
		t.Fatalf("expected revoke of a live grant to succeed")
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
	if _, ok := reg.Revoke(grant.GrantID); !ok {
		t.Fatalf("expected revoke to succeed")
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

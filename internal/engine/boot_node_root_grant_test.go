// boot_node_root_grant_test.go — tests for the node-root grant TTL renewal
// ticker (boot_node_root_grant.go), specifically the round-4 cold-start fix
// (cog-review, PR #551 round 4): startNodeRootGrantRenewal now runs
// maybeRenewNodeRootGrant once immediately at startup — before ever waiting
// on the ticker — and that check is threshold-based (renews only when the
// live grant's remaining lifetime has dropped to or below the renewal
// interval) rather than firing blind on a fixed 15-day cadence. Before this
// fix, a kernel restarted shortly before the grant's existing expiry would
// recover an almost-dead token (RecoverToken preserves ExpiresAt unchanged)
// and then wait a full interval before the first tick even looked at it —
// well past expiry, reproducing the all-consumers-401 failure this feature
// exists to prevent, via restart timing rather than long uptime.
//
// Every scenario below mints a real, still-live grant first, so only
// ExtendGrant's path is exercised. None of these tests exercise the "no
// live grant" / "already expired" fallback (establishFreshNodeRootGrant ->
// ensureNodeRootGrant), which reads and writes ~/.cog/vault/node-root-grant
// on the real home directory with no dependency-injection seam for that
// path (pre-existing, not something this round changed) — deliberately
// avoided here so these tests never touch the developer's actual
// filesystem.
package engine

import (
	"context"
	"testing"
	"time"
)

// newRenewalTestServer builds a bare *Server with just an in-memory identity
// grant registry — enough for maybeRenewNodeRootGrant and
// startNodeRootGrantRenewal, which only ever touch s.identityGrants.
func newRenewalTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{identityGrants: NewIdentityGrantRegistry()}
}

// TestMaybeRenewNodeRootGrant_DueGrantIsExtended is the direct,
// deterministic half of the round-4 regression test: a grant whose
// remaining lifetime has already dropped below the renewal interval —
// exactly the "recovered a nearly-expired grant after a restart" shape the
// reviewer scripted — must be extended the moment maybeRenewNodeRootGrant
// runs, with no ticker or multi-day wall-clock wait involved.
func TestMaybeRenewNodeRootGrant_DueGrantIsExtended(t *testing.T) {
	t.Parallel()
	s := newRenewalTestServer(t)

	// Mint with a short TTL so remaining lifetime is trivially below
	// nodeRootGrantRenewalInterval() (15 days, half of the 30-day
	// defaultGrantTTL) without needing to fake time.
	grant, err := s.identityGrants.MintOrReuse(nodeRootSurface, []string{nodeRootScope}, 2*time.Second)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	originalToken := grant.Token
	originalGrantID := grant.GrantID
	originalExpiresAt := grant.ExpiresAt

	maybeRenewNodeRootGrant(s)

	extended, ok := s.identityGrants.GrantExpiry(nodeRootSurface)
	if !ok {
		t.Fatalf("expected the node-root grant to still be live after the renewal check")
	}
	if !extended.After(originalExpiresAt) {
		t.Fatalf("expected ExpiresAt to advance past %v, got %v — the due grant was not renewed", originalExpiresAt, extended)
	}

	// Same credential, just longer-lived: token/grant_id must be unchanged
	// (extension, not supersession — see ExtendGrant's doc comment). Every
	// consumer that already cached the raw token depends on this.
	current, ok := s.identityGrants.Current(nodeRootSurface)
	if !ok {
		t.Fatalf("expected Current to report the live node-root grant")
	}
	if current.Token != originalToken {
		t.Errorf("Token changed on renewal: got %q, want %q", current.Token, originalToken)
	}
	if current.GrantID != originalGrantID {
		t.Errorf("GrantID changed on renewal: got %q, want %q", current.GrantID, originalGrantID)
	}
}

// TestMaybeRenewNodeRootGrant_FreshGrantNoOps is the threshold half: a
// freshly-minted grant, far from expiry, must be left completely untouched
// by the renewal check — a genuine no-op, not a defensive re-extend "just in
// case." This is what makes the immediate startup check safe to add: it
// can't thrash a grant that isn't actually due.
func TestMaybeRenewNodeRootGrant_FreshGrantNoOps(t *testing.T) {
	t.Parallel()
	s := newRenewalTestServer(t)

	grant, err := s.identityGrants.MintOrReuse(nodeRootSurface, []string{nodeRootScope}, defaultGrantTTL)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	originalToken := grant.Token
	originalExpiresAt := grant.ExpiresAt

	maybeRenewNodeRootGrant(s)

	current, ok := s.identityGrants.Current(nodeRootSurface)
	if !ok {
		t.Fatalf("expected Current to report the live node-root grant")
	}
	if current.Token != originalToken {
		t.Errorf("Token changed on a no-op renewal check: got %q, want %q", current.Token, originalToken)
	}
	if !current.ExpiresAt.Equal(originalExpiresAt) {
		t.Errorf("ExpiresAt changed on a no-op renewal check: got %v, want unchanged %v", current.ExpiresAt, originalExpiresAt)
	}
}

// TestStartNodeRootGrantRenewal_ImmediateCheckOnStartup is the end-to-end
// half of the round-4 regression test: the actual production entry point
// (startNodeRootGrantRenewal, as called from Boot()) must perform its first
// renewal check before ever waiting on the ticker — a due grant must be
// extended within a short poll window, not after a full
// nodeRootGrantRenewalInterval (15 days, which this test obviously never
// waits for — that's the entire point of the round-4 fix).
func TestStartNodeRootGrantRenewal_ImmediateCheckOnStartup(t *testing.T) {
	t.Parallel()
	s := newRenewalTestServer(t)

	// A generous TTL margin (30s, still trivially "due" against the 15-day
	// threshold) so that even under heavy parallel test-suite load there is
	// no realistic chance the grant naturally expires before the immediate
	// check — which normally runs within microseconds of the goroutine
	// starting — has a chance to extend it. Naturally expiring here would
	// divert into the mint/recover fallback this test deliberately avoids
	// (see the file header).
	grant, err := s.identityGrants.MintOrReuse(nodeRootSurface, []string{nodeRootScope}, 30*time.Second)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	originalExpiresAt := grant.ExpiresAt

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startNodeRootGrantRenewal(ctx, s)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if expiresAt, ok := s.identityGrants.GrantExpiry(nodeRootSurface); ok && expiresAt.After(originalExpiresAt) {
			return // extended — the immediate check ran without waiting for a tick
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected startNodeRootGrantRenewal's immediate check to extend the due grant within 3s of starting; it never did")
}

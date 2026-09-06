// boot_node_root_grant.go — mint/recover the kernel's node-root identity
// grant at boot, per the sealed design (step 5b) in
// cog://mem/procedural/plan-hermes-kernel-loop-wiring: "the kernel mints a
// node-root grant at boot and writes it to ~/.cog/vault/ (0600; identity-CRD
// credential-ref file:// shape)". This is the bootstrap credential local
// consumers (the dashboard, canvas, Claude Code hooks, THESEUS, ...) use to
// satisfy serve_grant_auth.go's X-Cogos-Grant gate with zero operator paste.
//
// Storage: the design's PREFERRED path is the CogOS credential plane —
// Vaultwarden via the envspec/bw_bridge pattern (see ADR-102 /
// ADR-node-secret-provider.cog.md, "Node-Level Credential Plane —
// NodeSecretProvider Contract and the secret: Projection"). That ADR's own
// text is explicit that today's implementation is READ-only: its
// NodeSecretProvider "pipes the value into a caller-supplied sink [...]
// without returning it" for RESOLUTION, and deposit is a separate
// provisioning-time entrypoint (bootstrap.py/deposit.py), not something the
// in-process Go kernel can call. envspec (this repo's envspec/envspec.go)
// mirrors that shape exactly: its Resolver interface has Resolve only, no
// Deposit — grep envspec/resolvers.go and note every resolver (Bitwarden,
// BitwardenCLI, VaultFile, Keychain) reads a secret in, none writes one out
// through the Vaultwarden/Bitwarden Secrets Manager API. KeychainResolver.Store
// exists but is a local OS-keychain cache, not the shared credential plane.
//
// Building a Deposit path into the bw_bridge/envspec machinery is real
// bridge plumbing (new API surface on the Vaultwarden side, new envspec
// interface, a decision about which resolver becomes the deposit target) —
// out of scope for this PR per its own task boundary. FALLBACK per that same
// boundary: write the raw token to ~/.cog/vault/node-root-grant, 0600. This
// is a deliberate, documented residual, not a silent gap:
//
//   - The vault file is readable by any local process running as the
//     operator's own user — no different from every other loopback-bound
//     kernel state on this host (kernel.yaml, the ledger itself). The
//     kernel's threat model throughout board task 60
//     (serve_identity_grants.go) is already "loopback-only, any local
//     process is trusted"; this file does not weaken that, it instantiates
//     it once more.
//   - OPEN QUESTION for ADR-102 follow-up: once envspec/bw_bridge grows a
//     Deposit primitive, ensureNodeRootGrant below should call it instead of
//     persistNodeRootGrant, and the vault file should become a
//     migrate-then-delete fallback rather than the primary store. Tracked
//     here rather than silently left for someone to rediscover.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// nodeRootSurface is the identity-grant surface name the kernel mints its
// own bootstrap credential under. Distinct from any per-consumer surface
// (a dashboard, mod3, etc. mint their own via POST /v1/identity/grants) —
// node-root exists so a local process has SOMETHING to present on its very
// first write-route request, before it has bootstrapped its own grant.
const nodeRootSurface = "node-root"

// nodeRootScope is the root scope recorded on the node-root grant. It is no
// longer documentary (ledger L02): grantHasScope in serve_grant_auth.go
// treats it as the root authority — a grant carrying this scope satisfies
// every scope requirement, which is what "node-root retains all scopes"
// means mechanically. Every OTHER surface's grant is checked against the
// concrete ScopeInference/ScopeWrite/ScopeAdmin vocabulary.
//
// The carve-out is keyed on this scope string rather than on
// nodeRootSurface so a node-root grant reconstructed from a pre-L02 ledger
// keeps its authority across the upgrade: MintOrReuse REUSES a still-live
// grant rather than re-minting, so the Scope list recorded at the original
// mint is what a restarted kernel sees, and no ledger migration is needed.
const nodeRootScope = "node-root"

// nodeRootScopes is the scope list a freshly minted node-root grant records.
// nodeRootScope alone would suffice (grantHasScope treats it as root), but
// enumerating the concrete scopes alongside it makes `GET /v1/identity/grants`
// self-describing for an operator reading the inventory, and means a future
// change that narrows the root carve-out does not silently strip the
// kernel's own credential of the authority it actually exercises.
func nodeRootScopes() []string {
	return []string{nodeRootScope, ScopeInference, ScopeWrite, ScopeAdmin}
}

// nodeRootVaultFileName is the fallback on-disk store for the node-root
// grant's raw token — see the file header for why this is a fallback rather
// than the preferred credential-plane path.
const nodeRootVaultFileName = "node-root-grant"

// nodeRootVaultPath returns ~/.cog/vault/node-root-grant.
func nodeRootVaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".cog", "vault", nodeRootVaultFileName), nil
}

// ensureNodeRootGrant mints or recovers the kernel's node-root identity
// grant at boot. Called from Boot() after NewServer (which has already run
// RebuildIdentityGrantRegistryFromLedger, so a prior boot's grant — if any —
// is already reconstructed in s.identityGrants with its IntegrityHash but no
// cached raw Token).
//
// Recover-before-mint: if the vault file still holds a raw token whose hash
// matches that reconstructed grant, RecoverToken just re-populates the
// in-memory Token cache — no ledger write, and critically, no supersession
// of the still-valid grant. Minting fresh on every ordinary restart would
// invalidate the token for every OTHER local consumer that already cached
// the previous value (the dashboard, a running Claude Code hook session,
// THESEUS) for no reason; recovery avoids that churn. Only when recovery
// fails (first boot ever, vault file missing/corrupted, or the ledger's live
// grant has genuinely changed underneath — e.g. an operator revoked it) does
// this fall through to MintOrReuse (board task 60's existing
// idempotent-per-surface path — see its doc comment) and persist the new
// raw token.
func ensureNodeRootGrant(s *Server) (*IdentityGrant, error) {
	if s == nil || s.identityGrants == nil {
		return nil, fmt.Errorf("ensureNodeRootGrant: server or identity grant registry is nil")
	}

	vaultPath, pathErr := nodeRootVaultPath()
	if pathErr == nil {
		if raw, err := os.ReadFile(vaultPath); err == nil {
			if token := strings.TrimSpace(string(raw)); token != "" {
				if grant, ok := s.identityGrants.RecoverToken(nodeRootSurface, token); ok {
					return grant, nil
				}
			}
		}
	}

	grant, err := s.identityGrants.MintOrReuse(nodeRootSurface, nodeRootScopes(), 0)
	if err != nil {
		return nil, fmt.Errorf("mint node-root grant: %w", err)
	}

	if pathErr != nil {
		slog.Warn("boot: could not resolve home directory to persist the node-root grant vault file; "+
			"the token lives only in this process's memory. With the unauthenticated GET removed (ledger L03), "+
			"NO local consumer can bootstrap from this kernel: set HOME or pass a vault path, then restart",
			"err", pathErr)
		return grant, nil
	}
	if err := persistNodeRootGrant(vaultPath, grant.Token); err != nil {
		slog.Warn("boot: failed to persist the node-root grant to its vault file; "+
			"the token lives only in this process's memory until the next mint",
			"path", vaultPath, "err", err)
	}
	return grant, nil
}

// nodeRootGrantRenewalInterval is the cadence startNodeRootGrantRenewal ticks
// at: half of defaultGrantTTL (serve_identity_grants.go), not "just before
// expiry", so a single missed or failed tick (kernel under load, a transient
// ledger append failure) still leaves a full TTL/2 window before the grant
// would actually lapse. This is the fix for the finding that
// ensureNodeRootGrant mints via MintOrReuse(..., 0) — the package-default
// 30-day TTL — with nothing renewing it, so a kernel with >30d uptime started
// 401ing every local consumer of the write-route grant-auth gate until
// restart.
func nodeRootGrantRenewalInterval() time.Duration {
	return defaultGrantTTL / 2
}

// startNodeRootGrantRenewal starts a background ticker, owned by the kernel
// lifecycle exactly like LocalHarnessController.runTicker
// (local_agent_harness.go) and ReconcileDaemon's own loop: it stops the
// instant ctx is cancelled, no separate Stop() method needed. Called from
// Boot() after kernelCtx exists, so Kernel.Stop's context cancellation is
// sufficient to tear this down along with every other kernel goroutine.
//
// Runs maybeRenewNodeRootGrant ONCE IMMEDIATELY, before ever waiting on the
// ticker — this closes the cold-start gap a review round confirmed (PR #551
// round 4): ensureNodeRootGrant's RecoverToken path preserves a recovered
// grant's ExpiresAt unchanged on an ordinary restart, so a kernel restarted
// shortly before the grant's original expiry would otherwise recover an
// almost-dead token and then wait a FULL nodeRootGrantRenewalInterval (15
// days) before the first tick even checked it — well past expiry, a
// multi-day all-consumers-401 window triggered purely by restart timing.
// Running the same check immediately at startup means restart timing can no
// longer create a renewal gap the ticker cadence doesn't cover, and every
// subsequent tick runs the identical check — see maybeRenewNodeRootGrant's
// doc comment for why that's safe to call at an arbitrary time rather than
// only right after a tick.
func startNodeRootGrantRenewal(ctx context.Context, s *Server) {
	interval := nodeRootGrantRenewalInterval()
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		maybeRenewNodeRootGrant(s)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				maybeRenewNodeRootGrant(s)
			}
		}
	}()
}

// maybeRenewNodeRootGrant is the renewal check: THRESHOLD-based, not
// fire-blind. It renews (via ExtendGrant) only when the live node-root
// grant's remaining lifetime has dropped below nodeRootGrantRenewalInterval;
// otherwise it no-ops. That threshold is what makes the check idempotent and
// safe to call from anywhere at any time — the immediate call at startup
// (startNodeRootGrantRenewal) and every half-TTL tick both call this exact
// same function, and neither can ever renew a grant that isn't actually due,
// so there's no risk of the immediate call thrashing a freshly-minted grant
// or drifting the tick phase away from the expiry phase over repeated
// restarts.
//
// Three outcomes:
//
//   - No live grant at all (GrantExpiry reports not-found: never minted, or
//     already expired) — establish one via the ordinary mint-or-recover
//     path (ensureNodeRootGrant), same as boot's own bootstrap. Logged at
//     Info: this is a real credential change every consumer must re-fetch.
//   - A live grant exists but its remaining lifetime is still comfortably
//     above the renewal threshold — no-op. This is the common case on every
//     tick once the ticker's own cadence has caught up, and now also the
//     common case on an ordinary restart that happens nowhere near expiry.
//   - A live grant exists and is due (remaining lifetime <= the renewal
//     interval, which covers both the "kernel has been up a while" case the
//     ticker was originally built for AND the "recovered a nearly-expired
//     grant after a restart" case round 4 found) — extend it via
//     ExtendGrant, which leaves GrantID/Token/IntegrityHash untouched so
//     every consumer's cached raw token value keeps working. If ExtendGrant
//     itself reports ErrGrantNotFound (a narrow race: the grant expired
//     between the GrantExpiry check above and this call), fall through to
//     the same mint-or-recover path as the not-found case.
func maybeRenewNodeRootGrant(s *Server) {
	if s == nil || s.identityGrants == nil {
		return
	}

	expiresAt, ok := s.identityGrants.GrantExpiry(nodeRootSurface)
	if !ok {
		establishFreshNodeRootGrant(s, "no live node-root grant found")
		return
	}

	remaining := time.Until(expiresAt)
	if remaining > nodeRootGrantRenewalInterval() {
		slog.Debug("node-root grant renewal: not due yet", "remaining", remaining.String())
		return
	}

	grant, err := s.identityGrants.ExtendGrant(nodeRootSurface, defaultGrantTTL)
	if err != nil {
		if errors.Is(err, ErrGrantNotFound) {
			establishFreshNodeRootGrant(s, "grant expired between the due-check and the extend attempt")
			return
		}
		slog.Warn("node-root grant renewal: extend failed; will retry on the next check", "err", err)
		return
	}
	slog.Debug("node-root grant renewal: extended", "grant_id", grant.GrantID, "expires_at", grant.ExpiresAt.Format(time.RFC3339))
}

// establishFreshNodeRootGrant runs ensureNodeRootGrant's ordinary
// mint-or-recover path from within a renewal check (either because no live
// grant exists, or because ExtendGrant lost a narrow race against expiry —
// see maybeRenewNodeRootGrant's doc comment for both call sites). This IS a
// real credential change every local consumer must pick up (re-read the vault
// file, or POST /v1/identity/grants/current with a still-valid grant), so
// success logs at Info
// rather than Debug; reason is a short, call-site-specific description for
// the log line.
func establishFreshNodeRootGrant(s *Server, reason string) {
	fresh, err := ensureNodeRootGrant(s)
	if err != nil {
		slog.Warn("node-root grant renewal: "+reason+", and mint/recover failed; "+
			"local consumers must self-mint via POST /v1/identity/grants until the next check",
			"err", err)
		return
	}
	slog.Info("node-root grant renewal: "+reason+"; minted/recovered a fresh grant "+
		"(this is a real credential change — consumers must re-read ~/.cog/vault/node-root-grant or POST /v1/identity/grants/current with a still-valid grant)",
		"grant_id", fresh.GrantID, "expires_at", fresh.ExpiresAt.Format(time.RFC3339))
}

// persistNodeRootGrant writes token to path with 0600 permissions, creating
// parent directories (0700) as needed. Writes to a sibling .tmp file and
// renames into place so a crash mid-write can never leave a truncated or
// corrupt token on disk for the next boot's recovery read to trip over.
func persistNodeRootGrant(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0600); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

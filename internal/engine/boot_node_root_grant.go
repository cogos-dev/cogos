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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// nodeRootSurface is the identity-grant surface name the kernel mints its
// own bootstrap credential under. Distinct from any per-consumer surface
// (a dashboard, mod3, etc. mint their own via POST /v1/identity/grants) —
// node-root exists so a local process has SOMETHING to present on its very
// first write-route request, before it has bootstrapped its own grant.
const nodeRootSurface = "node-root"

// nodeRootScope is the (single, coarse) scope recorded on the node-root
// grant. The write-route grant-auth middleware (serve_grant_auth.go) does
// not itself check scope — VerifyAny only asks "is this token live" — so
// this value is documentary today (what the credential is FOR), not
// enforced. A future scope-aware consumer of IdentityGrant.Scope can rely on
// this name without a corpus-wide rename later.
const nodeRootScope = "node-root"

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

	grant, err := s.identityGrants.MintOrReuse(nodeRootSurface, []string{nodeRootScope}, 0)
	if err != nil {
		return nil, fmt.Errorf("mint node-root grant: %w", err)
	}

	if pathErr != nil {
		slog.Warn("boot: could not resolve home directory to persist the node-root grant vault file; "+
			"the token lives only in this process's memory and local consumers must re-fetch it via "+
			"GET /v1/identity/grants/current?surface=node-root",
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

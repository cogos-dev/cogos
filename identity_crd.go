// identity_crd.go — Thin re-export shim. Canonical Identity CRD schema
// lives in pkg/substrate/identity per ADR-100 Step 3.
//
// Type aliases + function wrappers below let existing kernel call sites
// (identity_provider.go, identity_crd_test.go, identity_provider_test.go,
// identity_provider_e2e_test.go) compile unchanged. New code should prefer
// the pkg/substrate/identity import path.
//
// The IdentityProvider reconciler that implements the Reconcilable contract
// against these schema types stays kernel-resident at identity_provider.go
// because it depends on internal/engine runtime types.

package main

import (
	"github.com/myrgic/cogos/pkg/substrate/identity"
)

// ─── CRD Type aliases ───────────────────────────────────────────────────────────

// IdentityCRD is the top-level Kubernetes-style identity definition.
// Canonical home: pkg/substrate/identity.CRD.
type IdentityCRD = identity.CRD

// IdentityCRDMeta matches the standard CRD metadata shape.
// Canonical home: pkg/substrate/identity.CRDMeta.
type IdentityCRDMeta = identity.CRDMeta

// IdentityCRDSpec holds the OIDC-shaped identity body.
// Canonical home: pkg/substrate/identity.CRDSpec.
type IdentityCRDSpec = identity.CRDSpec

// KeyRef points at key material stored outside the manifest.
// Canonical home: pkg/substrate/identity.KeyRef.
type KeyRef = identity.KeyRef

// AuthFactor declares a multi-factor requirement.
// Canonical home: pkg/substrate/identity.AuthFactor.
type AuthFactor = identity.AuthFactor

// AuthFactorEntry is one factor within an AuthFactor group.
// Canonical home: pkg/substrate/identity.AuthFactorEntry.
type AuthFactorEntry = identity.AuthFactorEntry

// VoiceProfile separates the generative TTS head from the discriminative
// speaker-recognition head.
// Canonical home: pkg/substrate/identity.VoiceProfile.
type VoiceProfile = identity.VoiceProfile

// VoiceGenerativeHead holds TTS conditioning parameters for one engine.
// Canonical home: pkg/substrate/identity.VoiceGenerativeHead.
type VoiceGenerativeHead = identity.VoiceGenerativeHead

// VoiceDiscriminativeHead holds speaker-recognition parameters.
// Canonical home: pkg/substrate/identity.VoiceDiscriminativeHead.
type VoiceDiscriminativeHead = identity.VoiceDiscriminativeHead

// IdentityExpression is one projection of an identity into a specific audience.
// Canonical home: pkg/substrate/identity.Expression.
type IdentityExpression = identity.Expression

// ─── Constant aliases ───────────────────────────────────────────────────────────

// IdentityAPIVersion is the apiVersion validated on load.
// Canonical home: pkg/substrate/identity.APIVersion.
const IdentityAPIVersion = identity.APIVersion

// IdentityKind is the kind string validated on load.
// Canonical home: pkg/substrate/identity.Kind.
const IdentityKind = identity.Kind

// ─── Function wrappers ──────────────────────────────────────────────────────────

// LoadIdentityCRD loads a single identity CRD by subject slug.
// Delegates to pkg/substrate/identity.LoadCRD.
func LoadIdentityCRD(root, sub string) (*IdentityCRD, error) {
	return identity.LoadCRD(root, sub)
}

// LoadIdentityCRDs loads every identity CRD under
// {root}/.cog/config/identities/*.yaml.
// Delegates to pkg/substrate/identity.LoadCRDs.
func LoadIdentityCRDs(root string) ([]*IdentityCRD, error) {
	return identity.LoadCRDs(root)
}

// ─── Private helper aliases (for existing kernel + test call sites) ─────────────
// These keep lowercase callers (identity_provider.go, identity_crd_test.go,
// identity_provider_e2e_test.go) compiling unchanged. New code should use
// the exported substrate identifiers directly.

// identityCRDDir is the unexported alias for identity.CRDDir.
func identityCRDDir(root string) string {
	return identity.CRDDir(root)
}

// parseKeyRefScheme is the unexported alias for identity.ParseKeyRefScheme.
func parseKeyRefScheme(ref string) (string, bool) {
	return identity.ParseKeyRefScheme(ref)
}

// allowedKeyRefSchemes is the unexported alias for identity.AllowedKeyRefSchemes.
var allowedKeyRefSchemes = identity.AllowedKeyRefSchemes

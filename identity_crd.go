// identity_crd.go
// Loads Identity CRD definitions from .cog/config/identities/*.yaml.
//
// Identity is a CRD managed by the CogOS reconciliation loop (pkg/reconcile).
// The spec on disk is one leg of the (Spec, Projections, Reconciler) triple
// that defines an identity at runtime — see
// project_cogos_identity_as_crd.md in user memory for the architecture.
//
// Shape is OIDC-compatible: each identity has a stable (iss, sub) anchor and
// a list of expressions keyed by audience (workspace, channel, resource).
// The reconciler picks the expression matching the current audience when
// projecting — that's the "collapse operation" that maps global identity
// down to its contextual local expression.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ─── CRD Types ──────────────────────────────────────────────────────────────────

// IdentityCRD is the top-level Kubernetes-style identity definition.
type IdentityCRD struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   IdentityCRDMeta `yaml:"metadata"`
	Spec       IdentityCRDSpec `yaml:"spec"`
}

// IdentityCRDMeta matches the standard CRD metadata shape.
type IdentityCRDMeta struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// IdentityCRDSpec holds the OIDC-shaped identity body.
//
//	Issuer     — OIDC `iss`; who minted this identity (e.g., "myrgic",
//	             "https://accounts.google.com", "node:<hostname>", or a
//	             federation URL). Required.
//	Subject    — OIDC `sub`; globally stable identifier for the principal.
//	             Required. Conventionally matches metadata.name for
//	             self-minted identities, but may differ for federated ones.
//	Type       — agent | human | service. Governs downstream semantics
//	             (e.g., humans don't need presence expectation heartbeats).
//	PublicKey  — optional PEM-encoded public key. Inlined because the public
//	             half is not secret; committing it to the manifest is the
//	             point. When present, the reconciler records it for
//	             signature-verified attestations. Layer-1 node keys live
//	             separately in the constellation repo — this is Layer-2
//	             principal identity.
//	PrivateKey — optional reference to the private-key material. The ref
//	             URI is location-independent (file://, vault://, s3://,
//	             keychain://, env://, etc.) and is paired with an integrity
//	             hash. Moving the key between storage media keeps the
//	             identity stable as long as the hash matches — the
//	             cryptographic material itself is the identity, its
//	             location is ephemeral.
//	AuthFactors — optional multi-factor requirements for operations that
//	              modify this identity (apply, deregister). The reconciler
//	              is the enforcement point because it is the source of
//	              truth; everything downstream trusts its output. Evaluated
//	              at ApplyPlan, not at read-time. (Wiring this to real
//	              MFA providers is a later wave — the field is declared
//	              now so the schema is stable.)
//	Expressions — how this identity is projected into audiences. The
//	              reconciler's "collapse operation" picks the entry whose
//	              aud matches the current context.
type IdentityCRDSpec struct {
	Issuer      string               `yaml:"iss"`
	Subject     string               `yaml:"sub"`
	Type        string               `yaml:"type"`
	PublicKey   string               `yaml:"public_key,omitempty"`
	PrivateKey  *KeyRef              `yaml:"private_key,omitempty"`
	AuthFactors []AuthFactor         `yaml:"auth_factors,omitempty"`
	Expressions []IdentityExpression `yaml:"expressions"`
}

// KeyRef points at key material stored outside the manifest. The ref is
// resolved at ApplyPlan time through a pluggable KeyResolver (see
// identity_provider.go); IntegrityHash is verified after resolution. If
// the bytes at `ref` ever hash to a different value, the reconciler
// refuses to apply and surfaces the mismatch in Health.
//
// Supported ref URI schemes (extensible — each registered with the
// provider's KeyResolver):
//
//	file://<absolute-path>           — plain-PEM file on disk
//	vault://<path>                   — HashiCorp Vault secret path
//	s3://<bucket>/<key>              — S3 object
//	keychain://<service>/<account>   — macOS keychain entry
//	env://<VAR_NAME>                 — environment variable (dev only)
//	kms://<provider>/<arn>           — cloud KMS reference
//	inline://pem                     — base64 bytes inline (testing; avoid)
//
// IntegrityHash is `<algo>:<hex>` (e.g., "sha256:a1b2..."). The reconciler
// enforces algo ∈ {sha256, sha512}.
type KeyRef struct {
	Ref           string `yaml:"ref"`
	IntegrityHash string `yaml:"integrity_hash"`
}

// AuthFactor declares a multi-factor requirement. At least one factor per
// entry must be satisfied (OR semantics within an entry); each entry in
// the AuthFactors slice adds an AND requirement (entry1 AND entry2 AND …).
// Example: require either TOTP or a hardware key, AND also require a
// signed challenge from the Layer-1 node:
//
//	auth_factors:
//	  - kind: any-of
//	    factors:
//	      - type: totp
//	        ref: vault://secret/cogos/identities/cog/totp-seed
//	      - type: webauthn
//	        ref: keychain://cogos/cog-yubikey
//	  - kind: required
//	    factors:
//	      - type: node-signed-challenge
//	        ref: ""  # uses the current node's Layer-1 key
//
// The schema is declared now so manifests are stable; enforcement
// (prompting/verifying the factor) is a follow-up wave.
type AuthFactor struct {
	Kind    string            `yaml:"kind"` // "required" | "any-of"
	Factors []AuthFactorEntry `yaml:"factors"`
}

// AuthFactorEntry is one factor within an AuthFactor group.
type AuthFactorEntry struct {
	Type string `yaml:"type"` // totp | webauthn | node-signed-challenge | oidc-step-up | ...
	Ref  string `yaml:"ref,omitempty"`
	// Claims carries factor-specific config (e.g., TOTP period, WebAuthn RP ID).
	Claims map[string]any `yaml:"claims,omitempty"`
}

// VoiceProfile separates the generative TTS head from the discriminative
// speaker-recognition head. Both are URI-addressed substrate records so
// their file locations are projections, not the binding. This makes voice
// assets substrate-addressable via cog://voices/* — the same pattern used
// for skills (cog://skills/*) and memory (cog://mem/*).
type VoiceProfile struct {
	// Generative is the TTS conditioning head (Chatterbox-Turbo or compatible).
	// When non-nil, the identity can speak via mod3 using these conditionals.
	Generative *VoiceGenerativeHead `yaml:"generative,omitempty"`
	// Discriminative is the speaker-recognition head (ECAPA-TDNN or compatible).
	// When non-nil, the pipeline can identify this speaker from raw audio.
	// Schema only in this wave — no live ECAPA integration yet.
	Discriminative *VoiceDiscriminativeHead `yaml:"discriminative,omitempty"`
}

// VoiceGenerativeHead holds TTS conditioning parameters for one engine.
// ConditionalsRef is a cog://voices/<name> URI that resolves to the
// safetensors file containing the Chatterbox speaker conditionals.
type VoiceGenerativeHead struct {
	Engine          string `yaml:"engine"`                    // e.g. "chatterbox-turbo"
	ConditionalsRef string `yaml:"conditionals_ref"`           // cog://voices/<name>
	EnrolledAt      string `yaml:"enrolled_at,omitempty"`      // RFC3339; set by enrollment
}

// VoiceDiscriminativeHead holds speaker-recognition parameters.
// EmbeddingRef is a cog://voices/<name>/ecapa-embedding URI that
// resolves to the ECAPA-TDNN speaker embedding vector.
// Schema only in this wave — ECAPA integration is a follow-up.
type VoiceDiscriminativeHead struct {
	Model        string `yaml:"model"`                  // e.g. "speechbrain/spkrec-ecapa-voxceleb"
	EmbeddingRef string `yaml:"embedding_ref"`           // cog://voices/<name>/ecapa-embedding
	EnrolledAt   string `yaml:"enrolled_at,omitempty"`   // RFC3339
}

// IdentityExpression is one projection of an identity into a specific audience.
// At least one expression per Identity. The reconciler matches by `aud`:
//
//	workspace:<name>     — project into a workspace's view
//	channel:<channel_id> — project into a specific channel (mod3 room, etc.)
//	resource:<uri>       — project against an arbitrary cog:// URI
//	*                    — catch-all (used as fallback when no audience matches)
type IdentityExpression struct {
	Audience    string   `yaml:"aud"`
	DisplayName string   `yaml:"display_name,omitempty"`
	Role        string   `yaml:"role,omitempty"`
	Skills      []string `yaml:"skills,omitempty"`
	// VoiceProfile holds structured voice configuration for this expression.
	// Replaces the former Voice string field (removed in Wave 6c, 2026-05-19).
	// Generative head: TTS conditioning via cog://voices/* URIs.
	// Discriminative head: speaker-recognition embedding (schema only; no live
	// ECAPA integration in this wave).
	VoiceProfile *VoiceProfile `yaml:"voice_profile,omitempty"`
	Sudo         bool          `yaml:"sudo,omitempty"`
	// MemoryNamespace optionally scopes this expression's memory reads/writes
	// (e.g., "cog://" for root, "cog://agents/sandy/" for a scoped agent).
	MemoryNamespace string `yaml:"memory_namespace,omitempty"`
	// WorkspaceRoot is the canonical cog:// URI home for this identity
	// in this audience context. Location-independent; path projection
	// is resolved by the WorkspaceBinding reconciler (Wave 6c).
	// Example: "cog://workspaces/cog", "cog://workspaces/chaz"
	WorkspaceRoot string `yaml:"workspace_root,omitempty"`
	// Claims is an OIDC-style free-form claim bag. The reconciler preserves
	// unknown claims verbatim — future tooling can read them without the
	// spec needing a schema bump.
	Claims map[string]any `yaml:"claims,omitempty"`
}

// Valid identity types accepted by the loader.
var validIdentityTypes = map[string]struct{}{
	"agent":   {},
	"human":   {},
	"service": {},
}

// Valid apiVersion/kind — validated on load to catch typos early.
const (
	IdentityAPIVersion = "cog.os/v1alpha1"
	IdentityKind       = "Identity"
)

// ─── Loader ─────────────────────────────────────────────────────────────────────

// identityCRDDir returns the path to the identity definitions directory.
// Matches the `.cog/config/<provider>/` convention used by services.
func identityCRDDir(root string) string {
	return filepath.Join(root, ".cog", "config", "identities")
}

// LoadIdentityCRD loads a single identity CRD by subject slug.
// Looks for {root}/.cog/config/identities/{sub}.yaml.
func LoadIdentityCRD(root, sub string) (*IdentityCRD, error) {
	if sub == "" {
		return nil, errors.New("load identity CRD: empty subject")
	}
	path := filepath.Join(identityCRDDir(root), sub+".yaml")
	return loadIdentityCRDFile(path)
}

// LoadIdentityCRDs loads every identity CRD under
// {root}/.cog/config/identities/*.yaml. Returns an empty slice (not an
// error) when the directory does not exist — identity is optional.
//
// The returned slice is deterministically ordered by filename so plan
// diffs stay stable.
func LoadIdentityCRDs(root string) ([]*IdentityCRD, error) {
	dir := identityCRDDir(root)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read identities dir %q: %w", dir, err)
	}

	var out []*IdentityCRD
	var loadErrs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		crd, err := loadIdentityCRDFile(filepath.Join(dir, name))
		if err != nil {
			loadErrs = append(loadErrs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		out = append(out, crd)
	}

	if len(loadErrs) > 0 {
		return out, fmt.Errorf("load identities: %d error(s): %s",
			len(loadErrs), strings.Join(loadErrs, "; "))
	}
	return out, nil
}

// loadIdentityCRDFile reads + parses + validates one CRD file.
func loadIdentityCRDFile(path string) (*IdentityCRD, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}

	var crd IdentityCRD
	if err := yaml.Unmarshal(data, &crd); err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}

	if err := validateIdentityCRD(&crd); err != nil {
		return nil, fmt.Errorf("validate %q: %w", path, err)
	}
	return &crd, nil
}

// validateIdentityCRD enforces the invariants the reconciler depends on.
// Fail fast at load time so the plan/apply loop never sees malformed specs.
func validateIdentityCRD(crd *IdentityCRD) error {
	if crd == nil {
		return errors.New("nil CRD")
	}
	if crd.APIVersion != IdentityAPIVersion {
		return fmt.Errorf("apiVersion = %q, want %q", crd.APIVersion, IdentityAPIVersion)
	}
	if crd.Kind != IdentityKind {
		return fmt.Errorf("kind = %q, want %q", crd.Kind, IdentityKind)
	}
	if strings.TrimSpace(crd.Metadata.Name) == "" {
		return errors.New("metadata.name is required")
	}
	if strings.TrimSpace(crd.Spec.Issuer) == "" {
		return errors.New("spec.iss is required")
	}
	if strings.TrimSpace(crd.Spec.Subject) == "" {
		return errors.New("spec.sub is required")
	}
	if _, ok := validIdentityTypes[crd.Spec.Type]; !ok {
		return fmt.Errorf("spec.type = %q, want one of {agent, human, service}", crd.Spec.Type)
	}
	if len(crd.Spec.Expressions) == 0 {
		return errors.New("spec.expressions must contain at least one entry")
	}
	seenAud := make(map[string]struct{}, len(crd.Spec.Expressions))
	for i, exp := range crd.Spec.Expressions {
		if strings.TrimSpace(exp.Audience) == "" {
			return fmt.Errorf("spec.expressions[%d].aud is required", i)
		}
		if _, dup := seenAud[exp.Audience]; dup {
			return fmt.Errorf("spec.expressions[%d].aud = %q is duplicated; each audience must appear at most once", i, exp.Audience)
		}
		seenAud[exp.Audience] = struct{}{}
	}
	if err := validateKeyRef(crd.Spec.PrivateKey, "spec.private_key"); err != nil {
		return err
	}
	for i, factor := range crd.Spec.AuthFactors {
		if err := validateAuthFactor(&factor, fmt.Sprintf("spec.auth_factors[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// validateKeyRef enforces the structural invariants on a key reference.
// Nil is valid (optional field). When present, both ref and integrity_hash
// are required, the ref must have a recognized scheme, and the hash must
// be a known algorithm.
func validateKeyRef(k *KeyRef, path string) error {
	if k == nil {
		return nil
	}
	if strings.TrimSpace(k.Ref) == "" {
		return fmt.Errorf("%s.ref is required when private_key is set", path)
	}
	scheme, ok := parseKeyRefScheme(k.Ref)
	if !ok {
		return fmt.Errorf("%s.ref = %q: expected scheme://... (one of: %s)",
			path, k.Ref, strings.Join(knownKeyRefSchemes(), ", "))
	}
	if _, allowed := allowedKeyRefSchemes[scheme]; !allowed {
		return fmt.Errorf("%s.ref scheme %q is not recognized; allowed: %s",
			path, scheme, strings.Join(knownKeyRefSchemes(), ", "))
	}
	if strings.TrimSpace(k.IntegrityHash) == "" {
		return fmt.Errorf("%s.integrity_hash is required when private_key is set", path)
	}
	algo, _, ok := strings.Cut(k.IntegrityHash, ":")
	if !ok {
		return fmt.Errorf("%s.integrity_hash = %q: expected \"<algo>:<hex>\"", path, k.IntegrityHash)
	}
	if _, allowed := allowedHashAlgos[algo]; !allowed {
		return fmt.Errorf("%s.integrity_hash algo %q not allowed; allowed: sha256, sha512", path, algo)
	}
	return nil
}

// validateAuthFactor enforces the structural invariants on an auth-factor
// group. Each group must have a kind and at least one factor entry.
func validateAuthFactor(f *AuthFactor, path string) error {
	if f == nil {
		return nil
	}
	if _, ok := validAuthFactorKinds[f.Kind]; !ok {
		return fmt.Errorf("%s.kind = %q, want one of {required, any-of}", path, f.Kind)
	}
	if len(f.Factors) == 0 {
		return fmt.Errorf("%s.factors must contain at least one entry", path)
	}
	for i, entry := range f.Factors {
		if strings.TrimSpace(entry.Type) == "" {
			return fmt.Errorf("%s.factors[%d].type is required", path, i)
		}
	}
	return nil
}

// parseKeyRefScheme extracts the scheme from a ref URI. Returns (scheme, true)
// on success. Does not validate that the scheme is allowed — that's a
// separate check so the error message can distinguish "malformed URI" from
// "scheme not recognized."
func parseKeyRefScheme(ref string) (string, bool) {
	idx := strings.Index(ref, "://")
	if idx <= 0 {
		return "", false
	}
	return ref[:idx], true
}

// knownKeyRefSchemes returns a sorted slice of allowed schemes for error
// messages. The allow-list is declared so downstream provider code can
// implement resolvers with predictable coverage.
func knownKeyRefSchemes() []string {
	out := make([]string, 0, len(allowedKeyRefSchemes))
	for s := range allowedKeyRefSchemes {
		out = append(out, s)
	}
	// Sort for deterministic error messages.
	sortStrings(out)
	return out
}

// Known schemes for KeyRef.Ref. New schemes are added here as resolvers
// are implemented in identity_provider.go.
var allowedKeyRefSchemes = map[string]struct{}{
	"file":     {},
	"vault":    {},
	"s3":       {},
	"keychain": {},
	"env":      {},
	"kms":      {},
	"inline":   {},
}

// Allowed integrity-hash algorithms.
var allowedHashAlgos = map[string]struct{}{
	"sha256": {},
	"sha512": {},
}

// Allowed AuthFactor.Kind values.
var validAuthFactorKinds = map[string]struct{}{
	"required": {},
	"any-of":   {},
}

// sortStrings is a tiny stable-sort helper so identity_crd.go does not
// have to import "sort" just for the error-message path. Insertion sort
// is fine at len ≤ ~10.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// ─── Collapse Operation ─────────────────────────────────────────────────────────

// ExpressionFor returns the expression matching `aud`, or the catch-all `*`
// expression when no exact match exists. Returns nil when neither is present.
// This is the "collapse operation" — global identity collapsing down to a
// local expression based on the caller's context. Pure function; the
// reconciler calls it both at ComputePlan (to decide what to project) and
// at runtime (to resolve an identity's contextual view for a tool call).
func (s *IdentityCRDSpec) ExpressionFor(aud string) *IdentityExpression {
	if s == nil {
		return nil
	}
	var wildcard *IdentityExpression
	for i := range s.Expressions {
		exp := &s.Expressions[i]
		if exp.Audience == aud {
			return exp
		}
		if exp.Audience == "*" && wildcard == nil {
			wildcard = exp
		}
	}
	return wildcard
}

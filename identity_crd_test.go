// identity_crd_test.go
// Covers: CRD YAML parsing, validation invariants, LoadIdentityCRDs
// directory enumeration, and the ExpressionFor collapse operation.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── YAML parsing + validation ──────────────────────────────────────────────────

const minimalValidIdentityYAML = `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: cog
spec:
  iss: cogos-dev
  sub: cog-pattern
  type: agent
  expressions:
    - aud: workspace:cog-workspace
      display_name: "Cog — Workspace Guardian"
`

func writeTempCRD(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestLoadIdentityCRD_MinimalValid(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	if err := os.MkdirAll(idDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTempCRD(t, idDir, "cog.yaml", minimalValidIdentityYAML)

	crd, err := LoadIdentityCRD(dir, "cog")
	if err != nil {
		t.Fatalf("LoadIdentityCRD: %v", err)
	}
	if crd.APIVersion != IdentityAPIVersion {
		t.Errorf("APIVersion = %q, want %q", crd.APIVersion, IdentityAPIVersion)
	}
	if crd.Spec.Issuer != "cogos-dev" {
		t.Errorf("iss = %q", crd.Spec.Issuer)
	}
	if crd.Spec.Subject != "cog-pattern" {
		t.Errorf("sub = %q", crd.Spec.Subject)
	}
	if len(crd.Spec.Expressions) != 1 {
		t.Fatalf("expressions len = %d, want 1", len(crd.Spec.Expressions))
	}
	if crd.Spec.Expressions[0].Audience != "workspace:cog-workspace" {
		t.Errorf("aud = %q", crd.Spec.Expressions[0].Audience)
	}
}

func TestLoadIdentityCRD_RejectsBadKind(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "bad.yaml", `
apiVersion: cog.os/v1alpha1
kind: Widget
metadata:
  name: bad
spec:
  iss: cogos-dev
  sub: bad
  type: agent
  expressions:
    - aud: "*"
`)
	if _, err := LoadIdentityCRD(dir, "bad"); err == nil {
		t.Fatal("expected error for wrong kind, got nil")
	}
}

func TestLoadIdentityCRD_RejectsMissingIssOrSub(t *testing.T) {
	cases := map[string]string{
		"missing iss": `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  sub: x
  type: agent
  expressions:
    - aud: "*"
`,
		"missing sub": `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  type: agent
  expressions:
    - aud: "*"
`,
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			idDir := filepath.Join(dir, ".cog", "config", "identities")
			_ = os.MkdirAll(idDir, 0o755)
			writeTempCRD(t, idDir, "x.yaml", body)
			if _, err := LoadIdentityCRD(dir, "x"); err == nil {
				t.Fatalf("expected error for %s, got nil", label)
			}
		})
	}
}

func TestLoadIdentityCRD_RejectsBadType(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: robot
  expressions:
    - aud: "*"
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for bad type, got nil")
	}
}

func TestLoadIdentityCRD_RejectsEmptyExpressions(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  expressions: []
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for empty expressions, got nil")
	}
}

func TestLoadIdentityCRD_RejectsDuplicateAudience(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  expressions:
    - aud: workspace:foo
      display_name: first
    - aud: workspace:foo
      display_name: second
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for duplicate aud, got nil")
	}
}

// ─── Directory enumeration ──────────────────────────────────────────────────────

func TestLoadIdentityCRDs_EmptyWhenDirMissing(t *testing.T) {
	out, err := LoadIdentityCRDs(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentityCRDs: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(out))
	}
}

func TestLoadIdentityCRDs_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)

	writeTempCRD(t, idDir, "cog.yaml", minimalValidIdentityYAML)
	writeTempCRD(t, idDir, "sandy.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: sandy
spec:
  iss: cogos-dev
  sub: sandy
  type: agent
  expressions:
    - aud: workspace:cog-workspace
      display_name: "Sandy — Lab Engineer"
`)
	// A .yml alias should also be picked up.
	writeTempCRD(t, idDir, "chaz.yml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: chaz
spec:
  iss: cogos-dev
  sub: chaz
  type: human
  expressions:
    - aud: "*"
      display_name: Chaz
`)
	// A non-yaml file should be ignored.
	writeTempCRD(t, idDir, "README.md", "# ignore me\n")

	out, err := LoadIdentityCRDs(dir)
	if err != nil {
		t.Fatalf("LoadIdentityCRDs: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d CRDs, want 3", len(out))
	}
	// Deterministic order (sorted by filename).
	wantOrder := []string{"chaz", "cog", "sandy"}
	for i, crd := range out {
		if crd.Metadata.Name != wantOrder[i] {
			t.Errorf("out[%d].Name = %q, want %q", i, crd.Metadata.Name, wantOrder[i])
		}
	}
}

func TestLoadIdentityCRDs_CollectsErrorsContinuesLoading(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)

	writeTempCRD(t, idDir, "good.yaml", minimalValidIdentityYAML)
	writeTempCRD(t, idDir, "bad.yaml", `apiVersion: cog.os/v1alpha1
kind: Identity
spec:
  iss: cogos-dev
`) // missing metadata.name, missing sub, missing type, empty expressions

	out, err := LoadIdentityCRDs(dir)
	if err == nil {
		t.Fatal("expected error for bad.yaml, got nil")
	}
	if len(out) != 1 {
		t.Errorf("expected 1 good CRD loaded, got %d", len(out))
	}
}

// ─── Collapse operation (ExpressionFor) ─────────────────────────────────────────

func TestExpressionFor_ExactMatch(t *testing.T) {
	spec := &IdentityCRDSpec{
		Expressions: []IdentityExpression{
			{Audience: "workspace:a", DisplayName: "A"},
			{Audience: "workspace:b", DisplayName: "B"},
		},
	}
	got := spec.ExpressionFor("workspace:b")
	if got == nil || got.DisplayName != "B" {
		t.Fatalf("got %+v, want DisplayName=B", got)
	}
}

func TestExpressionFor_WildcardFallback(t *testing.T) {
	spec := &IdentityCRDSpec{
		Expressions: []IdentityExpression{
			{Audience: "workspace:a", DisplayName: "A"},
			{Audience: "*", DisplayName: "Fallback"},
		},
	}
	got := spec.ExpressionFor("channel:xyz")
	if got == nil || got.DisplayName != "Fallback" {
		t.Fatalf("got %+v, want DisplayName=Fallback", got)
	}
}

func TestExpressionFor_ExactBeatsWildcard(t *testing.T) {
	spec := &IdentityCRDSpec{
		Expressions: []IdentityExpression{
			{Audience: "*", DisplayName: "Fallback"},
			{Audience: "workspace:a", DisplayName: "A"},
		},
	}
	got := spec.ExpressionFor("workspace:a")
	if got == nil || got.DisplayName != "A" {
		t.Fatalf("got %+v, want DisplayName=A (exact should beat *)", got)
	}
}

func TestExpressionFor_NoMatchNoWildcard(t *testing.T) {
	spec := &IdentityCRDSpec{
		Expressions: []IdentityExpression{
			{Audience: "workspace:a", DisplayName: "A"},
		},
	}
	got := spec.ExpressionFor("workspace:b")
	if got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestExpressionFor_NilSpec(t *testing.T) {
	var spec *IdentityCRDSpec
	if got := spec.ExpressionFor("anything"); got != nil {
		t.Errorf("nil spec should return nil, got %+v", got)
	}
}

// ─── KeyRef validation (spec.private_key) ───────────────────────────────────────

func TestLoadIdentityCRD_PrivateKeyRef_ValidFileScheme(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	keysDir := t.TempDir()
	keyPath := filepath.Join(keysDir, "cog-priv.pem")
	keyRef := "file://" + keyPath
	writeTempCRD(t, idDir, "cog.yaml", fmt.Sprintf(`
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: cog
spec:
  iss: cogos-dev
  sub: cog
  type: agent
  public_key: |-
    -----BEGIN PUBLIC KEY-----
    MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEstub...
    -----END PUBLIC KEY-----
  private_key:
    ref: %q
    integrity_hash: "sha256:a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2"
  expressions:
    - aud: "*"
      display_name: Cog
`, keyRef))
	crd, err := LoadIdentityCRD(dir, "cog")
	if err != nil {
		t.Fatalf("LoadIdentityCRD: %v", err)
	}
	if crd.Spec.PrivateKey == nil {
		t.Fatal("PrivateKey is nil; expected populated")
	}
	if crd.Spec.PrivateKey.Ref != keyRef {
		t.Errorf("Ref = %q, want %q", crd.Spec.PrivateKey.Ref, keyRef)
	}
	if !strings.HasPrefix(crd.Spec.PrivateKey.IntegrityHash, "sha256:") {
		t.Errorf("IntegrityHash = %q", crd.Spec.PrivateKey.IntegrityHash)
	}
}

func TestLoadIdentityCRD_PrivateKeyRef_AllSchemes(t *testing.T) {
	// Every scheme in allowedKeyRefSchemes should parse successfully.
	for scheme := range allowedKeyRefSchemes {
		t.Run(scheme, func(t *testing.T) {
			dir := t.TempDir()
			idDir := filepath.Join(dir, ".cog", "config", "identities")
			_ = os.MkdirAll(idDir, 0o755)
			ref := scheme + "://some/path"
			writeTempCRD(t, idDir, "x.yaml", fmt.Sprintf(`
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  private_key:
    ref: %q
    integrity_hash: "sha256:deadbeef"
  expressions:
    - aud: "*"
`, ref))
			if _, err := LoadIdentityCRD(dir, "x"); err != nil {
				t.Fatalf("scheme %q: %v", scheme, err)
			}
		})
	}
}

func TestLoadIdentityCRD_PrivateKeyRef_RejectsUnknownScheme(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  private_key:
    ref: "ftp://legacy.example.com/key.pem"
    integrity_hash: "sha256:deadbeef"
  expressions:
    - aud: "*"
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for unknown scheme, got nil")
	}
}

func TestLoadIdentityCRD_PrivateKeyRef_RejectsMalformedURI(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  private_key:
    ref: "not-a-uri"
    integrity_hash: "sha256:deadbeef"
  expressions:
    - aud: "*"
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for malformed ref, got nil")
	}
}

func TestLoadIdentityCRD_PrivateKeyRef_RejectsMissingHash(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  private_key:
    ref: "file:///tmp/k.pem"
  expressions:
    - aud: "*"
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for missing integrity_hash, got nil")
	}
}

func TestLoadIdentityCRD_PrivateKeyRef_RejectsUnknownHashAlgo(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  private_key:
    ref: "file:///tmp/k.pem"
    integrity_hash: "md5:deadbeef"
  expressions:
    - aud: "*"
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for md5 hash algo, got nil")
	}
}

func TestLoadIdentityCRD_PrivateKeyOptional(t *testing.T) {
	// No private_key field at all — should still load successfully.
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", minimalValidIdentityYAML)
	crd, err := LoadIdentityCRD(dir, "x")
	if err != nil {
		t.Fatalf("LoadIdentityCRD: %v", err)
	}
	if crd.Spec.PrivateKey != nil {
		t.Errorf("PrivateKey = %+v, want nil when unset", crd.Spec.PrivateKey)
	}
}

// ─── AuthFactors validation ─────────────────────────────────────────────────────

func TestLoadIdentityCRD_AuthFactors_ValidAnyOfGroup(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  auth_factors:
    - kind: any-of
      factors:
        - type: totp
          ref: "vault://secret/cogos/x/totp"
        - type: webauthn
          ref: "keychain://cogos/x-yubikey"
    - kind: required
      factors:
        - type: node-signed-challenge
  expressions:
    - aud: "*"
`)
	crd, err := LoadIdentityCRD(dir, "x")
	if err != nil {
		t.Fatalf("LoadIdentityCRD: %v", err)
	}
	if len(crd.Spec.AuthFactors) != 2 {
		t.Fatalf("AuthFactors len = %d, want 2", len(crd.Spec.AuthFactors))
	}
	if crd.Spec.AuthFactors[0].Kind != "any-of" {
		t.Errorf("first kind = %q", crd.Spec.AuthFactors[0].Kind)
	}
	if len(crd.Spec.AuthFactors[0].Factors) != 2 {
		t.Errorf("first factors len = %d, want 2", len(crd.Spec.AuthFactors[0].Factors))
	}
}

func TestLoadIdentityCRD_AuthFactors_RejectsBadKind(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  auth_factors:
    - kind: one-of-many
      factors:
        - type: totp
  expressions:
    - aud: "*"
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for unknown factor kind, got nil")
	}
}

func TestLoadIdentityCRD_AuthFactors_RejectsEmptyFactors(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  auth_factors:
    - kind: required
      factors: []
  expressions:
    - aud: "*"
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for empty factors list, got nil")
	}
}

// ─── WorkspaceRoot field (Primitive 1) ─────────────────────────────────────────

// TestLoadIdentityCRD_WorkspaceRoot_Set verifies that workspace_root survives
// a YAML parse + validate round-trip when explicitly set.
func TestLoadIdentityCRD_WorkspaceRoot_Set(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "cog.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: cog
spec:
  iss: cogos-dev
  sub: cog
  type: agent
  expressions:
    - aud: "*"
      display_name: Cog
      memory_namespace: "cog://"
      workspace_root: "cog://workspaces/cog"
`)
	crd, err := LoadIdentityCRD(dir, "cog")
	if err != nil {
		t.Fatalf("LoadIdentityCRD: %v", err)
	}
	if len(crd.Spec.Expressions) != 1 {
		t.Fatalf("expressions len = %d, want 1", len(crd.Spec.Expressions))
	}
	got := crd.Spec.Expressions[0].WorkspaceRoot
	if got != "cog://workspaces/cog" {
		t.Errorf("WorkspaceRoot = %q, want %q", got, "cog://workspaces/cog")
	}
}

// TestLoadIdentityCRD_WorkspaceRoot_Optional verifies that omitting
// workspace_root does not cause a validation error (the field is optional).
func TestLoadIdentityCRD_WorkspaceRoot_Optional(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	// minimalValidIdentityYAML has no workspace_root — reuse it.
	writeTempCRD(t, idDir, "cog.yaml", minimalValidIdentityYAML)
	crd, err := LoadIdentityCRD(dir, "cog")
	if err != nil {
		t.Fatalf("LoadIdentityCRD: %v", err)
	}
	for _, exp := range crd.Spec.Expressions {
		if exp.WorkspaceRoot != "" {
			t.Errorf("WorkspaceRoot = %q, want empty string when unset", exp.WorkspaceRoot)
		}
	}
}

// TestLoadIdentityCRD_WorkspaceRoot_MultiExpression verifies that different
// expressions can carry different workspace_root values (audience-scoped).
func TestLoadIdentityCRD_WorkspaceRoot_MultiExpression(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "chaz.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: chaz
spec:
  iss: cogos-dev
  sub: chaz
  type: human
  expressions:
    - aud: "workspace:cog-workspace"
      display_name: Chaz
      workspace_root: "cog://workspaces/chaz"
    - aud: "*"
      display_name: Chaz
`)
	crd, err := LoadIdentityCRD(dir, "chaz")
	if err != nil {
		t.Fatalf("LoadIdentityCRD: %v", err)
	}
	exp := crd.Spec.ExpressionFor("workspace:cog-workspace")
	if exp == nil {
		t.Fatal("ExpressionFor returned nil")
	}
	if exp.WorkspaceRoot != "cog://workspaces/chaz" {
		t.Errorf("WorkspaceRoot = %q, want %q", exp.WorkspaceRoot, "cog://workspaces/chaz")
	}
	// Catch-all expression has no workspace_root set.
	wildcard := crd.Spec.ExpressionFor("channel:anything")
	if wildcard == nil {
		t.Fatal("wildcard ExpressionFor returned nil")
	}
	if wildcard.WorkspaceRoot != "" {
		t.Errorf("wildcard WorkspaceRoot = %q, want empty", wildcard.WorkspaceRoot)
	}
}

// TestLoadIdentityCRD_WorkspaceRoot_strings checks that workspace_root
// preserves its value exactly including the cog:// scheme prefix.
func TestLoadIdentityCRD_WorkspaceRoot_SchemePreserved(t *testing.T) {
	cases := []string{
		"cog://workspaces/cog",
		"cog://workspaces/chaz",
		"cog://workspaces/eclipse",
	}
	for _, want := range cases {
		t.Run(want, func(t *testing.T) {
			dir := t.TempDir()
			idDir := filepath.Join(dir, ".cog", "config", "identities")
			_ = os.MkdirAll(idDir, 0o755)
			writeTempCRD(t, idDir, "x.yaml", fmt.Sprintf(`
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  expressions:
    - aud: "*"
      workspace_root: %q
`, want))
			crd, err := LoadIdentityCRD(dir, "x")
			if err != nil {
				t.Fatalf("LoadIdentityCRD: %v", err)
			}
			got := crd.Spec.Expressions[0].WorkspaceRoot
			if got != want {
				t.Errorf("WorkspaceRoot = %q, want %q", got, want)
			}
		})
	}
}

// ─── VoiceProfile (Primitive 3) ─────────────────────────────────────────────────

// TestLoadIdentityCRD_VoiceProfile_GenerativeOnly asserts that a YAML with
// only a generative head parses correctly and discriminative is nil.
func TestLoadIdentityCRD_VoiceProfile_GenerativeOnly(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "cog.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: cog
spec:
  iss: cogos-dev
  sub: cog
  type: agent
  expressions:
    - aud: "*"
      display_name: Cog
      voice_profile:
        generative:
          engine: chatterbox-turbo
          conditionals_ref: "cog://voices/cog"
          enrolled_at: "2026-05-19T00:00:00Z"
`)
	crd, err := LoadIdentityCRD(dir, "cog")
	if err != nil {
		t.Fatalf("LoadIdentityCRD: %v", err)
	}
	exp := crd.Spec.Expressions[0]
	if exp.VoiceProfile == nil {
		t.Fatal("VoiceProfile is nil; expected populated")
	}
	if exp.VoiceProfile.Generative == nil {
		t.Fatal("VoiceProfile.Generative is nil; expected populated")
	}
	if exp.VoiceProfile.Generative.Engine != "chatterbox-turbo" {
		t.Errorf("Engine = %q, want %q", exp.VoiceProfile.Generative.Engine, "chatterbox-turbo")
	}
	if exp.VoiceProfile.Generative.ConditionalsRef != "cog://voices/cog" {
		t.Errorf("ConditionalsRef = %q, want %q", exp.VoiceProfile.Generative.ConditionalsRef, "cog://voices/cog")
	}
	if exp.VoiceProfile.Discriminative != nil {
		t.Errorf("Discriminative should be nil, got %+v", exp.VoiceProfile.Discriminative)
	}
}

// TestLoadIdentityCRD_VoiceProfile_BothHeads asserts that a YAML with both
// generative and discriminative heads round-trips all fields correctly.
func TestLoadIdentityCRD_VoiceProfile_BothHeads(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "cog.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: cog
spec:
  iss: cogos-dev
  sub: cog
  type: agent
  expressions:
    - aud: "*"
      display_name: Cog
      voice_profile:
        generative:
          engine: chatterbox-turbo
          conditionals_ref: "cog://voices/cog"
        discriminative:
          model: "speechbrain/spkrec-ecapa-voxceleb"
          embedding_ref: "cog://voices/cog/ecapa-embedding"
          enrolled_at: "2026-05-19T00:00:00Z"
`)
	crd, err := LoadIdentityCRD(dir, "cog")
	if err != nil {
		t.Fatalf("LoadIdentityCRD: %v", err)
	}
	exp := crd.Spec.Expressions[0]
	if exp.VoiceProfile == nil {
		t.Fatal("VoiceProfile is nil")
	}
	if exp.VoiceProfile.Generative == nil {
		t.Fatal("Generative is nil")
	}
	if exp.VoiceProfile.Discriminative == nil {
		t.Fatal("Discriminative is nil")
	}
	if exp.VoiceProfile.Discriminative.Model != "speechbrain/spkrec-ecapa-voxceleb" {
		t.Errorf("Model = %q", exp.VoiceProfile.Discriminative.Model)
	}
	if exp.VoiceProfile.Discriminative.EmbeddingRef != "cog://voices/cog/ecapa-embedding" {
		t.Errorf("EmbeddingRef = %q", exp.VoiceProfile.Discriminative.EmbeddingRef)
	}
}

// TestLoadIdentityCRD_VoiceProfile_Absent asserts that an identity without
// voice_profile has VoiceProfile == nil and loads without error.
func TestLoadIdentityCRD_VoiceProfile_Absent(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "cog.yaml", minimalValidIdentityYAML)
	crd, err := LoadIdentityCRD(dir, "cog")
	if err != nil {
		t.Fatalf("LoadIdentityCRD: %v", err)
	}
	exp := crd.Spec.Expressions[0]
	if exp.VoiceProfile != nil {
		t.Errorf("expected VoiceProfile nil when absent, got %+v", exp.VoiceProfile)
	}
}

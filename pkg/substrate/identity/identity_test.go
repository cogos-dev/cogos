// identity_test.go — Package-local tests for pkg/substrate/identity.
//
// The root package tests (identity_crd_test.go, identity_provider_test.go,
// identity_provider_e2e_test.go) exercise this code transitively via the
// type aliases. The tests here are the minimal local coverage so a future
// refactor inside this package gets a fast-fail signal without depending
// on the root test suite.

package identity_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/identity"
)

func TestValidateCRD_MinimalValid(t *testing.T) {
	crd := &identity.CRD{
		APIVersion: identity.APIVersion,
		Kind:       identity.Kind,
		Metadata:   identity.CRDMeta{Name: "cog"},
		Spec: identity.CRDSpec{
			Issuer:  "myrgic",
			Subject: "cog",
			Type:    "agent",
			Expressions: []identity.Expression{
				{Audience: "workspace:cog", DisplayName: "Cog"},
			},
		},
	}
	if err := identity.ValidateCRD(crd); err != nil {
		t.Fatalf("expected valid CRD, got error: %v", err)
	}
}

func TestValidateCRD_RejectsBadKind(t *testing.T) {
	crd := &identity.CRD{
		APIVersion: identity.APIVersion,
		Kind:       "NotIdentity",
		Metadata:   identity.CRDMeta{Name: "x"},
		Spec: identity.CRDSpec{
			Issuer:      "i",
			Subject:     "s",
			Type:        "agent",
			Expressions: []identity.Expression{{Audience: "a"}},
		},
	}
	if err := identity.ValidateCRD(crd); err == nil {
		t.Fatal("expected error for bad kind")
	}
}

func TestValidateCRD_RejectsMissingIssOrSub(t *testing.T) {
	base := identity.CRD{
		APIVersion: identity.APIVersion,
		Kind:       identity.Kind,
		Metadata:   identity.CRDMeta{Name: "x"},
		Spec: identity.CRDSpec{
			Issuer: "i", Subject: "s", Type: "agent",
			Expressions: []identity.Expression{{Audience: "a"}},
		},
	}

	t.Run("missing iss", func(t *testing.T) {
		c := base
		c.Spec.Issuer = ""
		if err := identity.ValidateCRD(&c); err == nil {
			t.Fatal("expected error for missing iss")
		}
	})
	t.Run("missing sub", func(t *testing.T) {
		c := base
		c.Spec.Subject = ""
		if err := identity.ValidateCRD(&c); err == nil {
			t.Fatal("expected error for missing sub")
		}
	})
}

func TestValidateCRD_RejectsBadType(t *testing.T) {
	crd := &identity.CRD{
		APIVersion: identity.APIVersion,
		Kind:       identity.Kind,
		Metadata:   identity.CRDMeta{Name: "x"},
		Spec: identity.CRDSpec{
			Issuer:      "i",
			Subject:     "s",
			Type:        "bot", // not in {agent, human, service}
			Expressions: []identity.Expression{{Audience: "a"}},
		},
	}
	if err := identity.ValidateCRD(crd); err == nil {
		t.Fatal("expected error for bad type")
	}
}

func TestValidateCRD_RejectsDuplicateAudience(t *testing.T) {
	crd := &identity.CRD{
		APIVersion: identity.APIVersion,
		Kind:       identity.Kind,
		Metadata:   identity.CRDMeta{Name: "x"},
		Spec: identity.CRDSpec{
			Issuer:  "i",
			Subject: "s",
			Type:    "agent",
			Expressions: []identity.Expression{
				{Audience: "workspace:a"},
				{Audience: "workspace:a"},
			},
		},
	}
	if err := identity.ValidateCRD(crd); err == nil {
		t.Fatal("expected error for duplicate audience")
	}
}

func TestLoadCRDs_EmptyWhenDirMissing(t *testing.T) {
	root := t.TempDir() // no .cog/config/identities subdir
	got, err := identity.LoadCRDs(root)
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(got))
	}
}

func TestLoadCRD_RoundtripValid(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".cog", "config", "identities")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlBody := `apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: cog
spec:
  iss: myrgic
  sub: cog
  type: agent
  expressions:
    - aud: workspace:cog
      display_name: Cog
      role: substrate-guardian
`
	if err := os.WriteFile(filepath.Join(dir, "cog.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	crd, err := identity.LoadCRD(root, "cog")
	if err != nil {
		t.Fatalf("LoadCRD: %v", err)
	}
	if crd.Metadata.Name != "cog" || crd.Spec.Issuer != "myrgic" {
		t.Errorf("loaded CRD has wrong fields: %+v", crd)
	}
	if len(crd.Spec.Expressions) != 1 || crd.Spec.Expressions[0].Audience != "workspace:cog" {
		t.Errorf("expression wrong: %+v", crd.Spec.Expressions)
	}
}

func TestExpressionFor_ExactMatch(t *testing.T) {
	spec := &identity.CRDSpec{
		Expressions: []identity.Expression{
			{Audience: "workspace:a", DisplayName: "A"},
			{Audience: "workspace:b", DisplayName: "B"},
		},
	}
	exp := spec.ExpressionFor("workspace:b")
	if exp == nil || exp.DisplayName != "B" {
		t.Errorf("expected B, got %+v", exp)
	}
}

func TestExpressionFor_WildcardFallback(t *testing.T) {
	spec := &identity.CRDSpec{
		Expressions: []identity.Expression{
			{Audience: "workspace:a", DisplayName: "A"},
			{Audience: "*", DisplayName: "Default"},
		},
	}
	exp := spec.ExpressionFor("channel:nonexistent")
	if exp == nil || exp.DisplayName != "Default" {
		t.Errorf("expected wildcard fallback, got %+v", exp)
	}
}

func TestExpressionFor_NoMatchReturnsNil(t *testing.T) {
	spec := &identity.CRDSpec{
		Expressions: []identity.Expression{
			{Audience: "workspace:a"},
		},
	}
	exp := spec.ExpressionFor("channel:nope")
	if exp != nil {
		t.Errorf("expected nil for no-match without wildcard, got %+v", exp)
	}
}

func TestParseKeyRefScheme(t *testing.T) {
	cases := []struct {
		input    string
		want     string
		wantOK   bool
	}{
		{"file:///abs/path", "file", true},
		{"vault://secret/cogos", "vault", true},
		{"keychain://service/account", "keychain", true},
		{"no-scheme", "", false},
		{"://no-prefix", "", false},
	}
	for _, c := range cases {
		got, ok := identity.ParseKeyRefScheme(c.input)
		if got != c.want || ok != c.wantOK {
			t.Errorf("ParseKeyRefScheme(%q) = (%q, %v), want (%q, %v)",
				c.input, got, ok, c.want, c.wantOK)
		}
	}
}

func TestCRDDir_PathShape(t *testing.T) {
	got := identity.CRDDir("/x")
	want := "/x/.cog/config/identities"
	if got != want {
		t.Errorf("CRDDir = %q, want %q", got, want)
	}
}

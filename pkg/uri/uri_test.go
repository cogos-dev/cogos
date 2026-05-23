package uri_test

import (
	"reflect"
	"testing"

	shim "github.com/myrgic/cogos/pkg/uri"
	canonical "github.com/myrgic/cogos/pkg/substrate/uri"
)

// TestTypeIdentity verifies that type aliases in the legacy re-export shim
// are identical to their canonical types via reflect.TypeOf. Because Go type
// aliases share the same runtime type, these must always be equal.
func TestTypeIdentity(t *testing.T) {
	cases := []struct {
		name      string
		canonical reflect.Type
		shim      reflect.Type
	}{
		{"URI", reflect.TypeOf(canonical.URI{}), reflect.TypeOf(shim.URI{})},
		{"Error", reflect.TypeOf(canonical.Error{}), reflect.TypeOf(shim.Error{})},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.canonical != tc.shim {
				t.Errorf("%s: canonical type %v != shim type %v (alias is broken)", tc.name, tc.canonical, tc.shim)
			}
		})
	}
}

// TestConstantIdentity verifies that re-exported constants have the same value
// as their canonical counterparts.
func TestConstantIdentity(t *testing.T) {
	if canonical.Scheme != shim.Scheme {
		t.Errorf("Scheme mismatch: %q != %q", canonical.Scheme, shim.Scheme)
	}
	if canonical.SchemeLegacy != shim.SchemeLegacy {
		t.Errorf("SchemeLegacy mismatch: %q != %q", canonical.SchemeLegacy, shim.SchemeLegacy)
	}
}

// TestParseRoundTrip verifies the re-exported Parse function works correctly.
func TestParseRoundTrip(t *testing.T) {
	raw := "cog:mem/semantic/foo"
	u, err := shim.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", raw, err)
	}
	if u.Namespace != "mem" {
		t.Errorf("Namespace = %q, want %q", u.Namespace, "mem")
	}
	if u.Path != "semantic/foo" {
		t.Errorf("Path = %q, want %q", u.Path, "semantic/foo")
	}
}

// TestIsCogURI verifies the re-exported predicate.
func TestIsCogURI(t *testing.T) {
	if !shim.IsCogURI("cog:mem/foo") {
		t.Errorf("IsCogURI(\"cog:mem/foo\") = false, want true")
	}
	if shim.IsCogURI("https://example.com") {
		t.Errorf("IsCogURI(\"https://example.com\") = true, want false")
	}
}

// TestIsValidNamespace verifies the re-exported namespace check.
func TestIsValidNamespace(t *testing.T) {
	if !shim.IsValidNamespace("mem") {
		t.Errorf("IsValidNamespace(\"mem\") = false, want true")
	}
	if shim.IsValidNamespace("notanamespace") {
		t.Errorf("IsValidNamespace(\"notanamespace\") = true, want false")
	}
}

// TestExtractInlineRefs verifies the re-exported inline ref extractor.
func TestExtractInlineRefs(t *testing.T) {
	content := "See cog://mem/semantic/foo and cog://adr/100"
	refs := shim.ExtractInlineRefs(content)
	if len(refs) != 2 {
		t.Errorf("ExtractInlineRefs len = %d, want 2; refs=%v", len(refs), refs)
	}
}

// TestNamespacesMap verifies the re-exported namespace registry is non-empty.
func TestNamespacesMap(t *testing.T) {
	if len(shim.Namespaces) == 0 {
		t.Errorf("Namespaces map is empty")
	}
	if !shim.Namespaces["mem"] {
		t.Errorf("Namespaces[\"mem\"] = false, want true")
	}
}

// TestErrorSentinels verifies re-exported sentinel errors are the same values.
func TestErrorSentinels(t *testing.T) {
	if canonical.ErrInvalidURI != shim.ErrInvalidURI {
		t.Errorf("ErrInvalidURI mismatch")
	}
	if canonical.ErrUnknownNamespace != shim.ErrUnknownNamespace {
		t.Errorf("ErrUnknownNamespace mismatch")
	}
}

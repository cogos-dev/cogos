package uri_test

import (
	"reflect"
	"testing"

	origin    "github.com/myrgic/cogos/pkg/uri"
	substrate "github.com/myrgic/cogos/pkg/substrate/uri"
)

// TestTypeIdentity verifies that type aliases in the substrate re-export layer
// are identical to their origin types via reflect.TypeOf. Because Go type
// aliases share the same runtime type, these must always be equal.
func TestTypeIdentity(t *testing.T) {
	cases := []struct {
		name     string
		origin   reflect.Type
		reexport reflect.Type
	}{
		{"URI", reflect.TypeOf(origin.URI{}), reflect.TypeOf(substrate.URI{})},
		{"Error", reflect.TypeOf(origin.Error{}), reflect.TypeOf(substrate.Error{})},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.origin != tc.reexport {
				t.Errorf("%s: origin type %v != substrate type %v (alias is broken)", tc.name, tc.origin, tc.reexport)
			}
		})
	}
}

// TestConstantIdentity verifies that re-exported constants have the same value
// as their origin counterparts.
func TestConstantIdentity(t *testing.T) {
	if origin.Scheme != substrate.Scheme {
		t.Errorf("Scheme mismatch: %q != %q", origin.Scheme, substrate.Scheme)
	}
	if origin.SchemeLegacy != substrate.SchemeLegacy {
		t.Errorf("SchemeLegacy mismatch: %q != %q", origin.SchemeLegacy, substrate.SchemeLegacy)
	}
}

// TestParseRoundTrip verifies the re-exported Parse function works correctly.
func TestParseRoundTrip(t *testing.T) {
	raw := "cog:mem/semantic/foo"
	u, err := substrate.Parse(raw)
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
	if !substrate.IsCogURI("cog:mem/foo") {
		t.Errorf("IsCogURI(\"cog:mem/foo\") = false, want true")
	}
	if substrate.IsCogURI("https://example.com") {
		t.Errorf("IsCogURI(\"https://example.com\") = true, want false")
	}
}

// TestIsValidNamespace verifies the re-exported namespace check.
func TestIsValidNamespace(t *testing.T) {
	if !substrate.IsValidNamespace("mem") {
		t.Errorf("IsValidNamespace(\"mem\") = false, want true")
	}
	if substrate.IsValidNamespace("notanamespace") {
		t.Errorf("IsValidNamespace(\"notanamespace\") = true, want false")
	}
}

// TestExtractInlineRefs verifies the re-exported inline ref extractor.
func TestExtractInlineRefs(t *testing.T) {
	content := "See cog://mem/semantic/foo and cog://adr/100"
	refs := substrate.ExtractInlineRefs(content)
	if len(refs) != 2 {
		t.Errorf("ExtractInlineRefs len = %d, want 2; refs=%v", len(refs), refs)
	}
}

// TestNamespacesMap verifies the re-exported namespace registry is non-empty.
func TestNamespacesMap(t *testing.T) {
	if len(substrate.Namespaces) == 0 {
		t.Errorf("Namespaces map is empty")
	}
	if !substrate.Namespaces["mem"] {
		t.Errorf("Namespaces[\"mem\"] = false, want true")
	}
}

// TestErrorSentinels verifies re-exported sentinel errors are the same values.
func TestErrorSentinels(t *testing.T) {
	if origin.ErrInvalidURI != substrate.ErrInvalidURI {
		t.Errorf("ErrInvalidURI mismatch")
	}
	if origin.ErrUnknownNamespace != substrate.ErrUnknownNamespace {
		t.Errorf("ErrUnknownNamespace mismatch")
	}
}

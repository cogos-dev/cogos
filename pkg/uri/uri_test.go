package uri

import (
	"errors"
	"strings"
	"testing"
)

// ── Parse ───────────────────────────────────────────────────────────────────────

func TestParseBasic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw       string
		wantNS    string
		wantPath  string
		wantFrag  string
		wantQuery map[string]string
	}{
		// Bare form (canonical per ADR-067)
		{
			raw:    "cog:mem/semantic/insights/eigenform",
			wantNS: "mem", wantPath: "semantic/insights/eigenform",
		},
		{
			raw:    "cog:mem/semantic/insights/eigenform.cog.md#Seed",
			wantNS: "mem", wantPath: "semantic/insights/eigenform.cog.md", wantFrag: "Seed",
		},
		{
			raw:    "cog:conf/kernel.yaml",
			wantNS: "conf", wantPath: "kernel.yaml",
		},
		{
			raw:    "cog:crystal",
			wantNS: "crystal",
		},
		{
			raw:       "cog:signals/inference?above=0.3",
			wantNS:    "signals",
			wantPath:  "inference",
			wantQuery: map[string]string{"above": "0.3"},
		},
		{
			raw:       "cog:context?budget=50000&model=sonnet",
			wantNS:    "context",
			wantQuery: map[string]string{"budget": "50000", "model": "sonnet"},
		},
		{
			raw:      "cog:thread/current#last-10",
			wantNS:   "thread",
			wantPath: "current",
			wantFrag: "last-10",
		},
		// Authority form (legacy — must still be accepted)
		{
			raw:    "cog://mem/semantic/insights/eigenform",
			wantNS: "mem", wantPath: "semantic/insights/eigenform",
		},
		{
			raw:    "cog://crystal",
			wantNS: "crystal",
		},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			u, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.raw, err)
			}
			if u.Namespace != tc.wantNS {
				t.Errorf("Namespace = %q; want %q", u.Namespace, tc.wantNS)
			}
			if u.Path != tc.wantPath {
				t.Errorf("Path = %q; want %q", u.Path, tc.wantPath)
			}
			if u.Fragment != tc.wantFrag {
				t.Errorf("Fragment = %q; want %q", u.Fragment, tc.wantFrag)
			}
			for k, v := range tc.wantQuery {
				if got := u.GetQuery(k); got != v {
					t.Errorf("Query[%q] = %q; want %q", k, got, v)
				}
			}
			if u.Raw != tc.raw {
				t.Errorf("Raw = %q; want %q", u.Raw, tc.raw)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	t.Parallel()
	_, err := Parse("")
	if err == nil {
		t.Fatal("expected error for empty URI")
	}
	if !errors.Is(err, ErrInvalidURI) {
		t.Errorf("expected ErrInvalidURI; got %v", err)
	}
}

func TestParseWrongScheme(t *testing.T) {
	t.Parallel()
	_, err := Parse("https://example.com/foo")
	if err == nil {
		t.Fatal("expected error for non-cog scheme")
	}
	if !errors.Is(err, ErrInvalidURI) {
		t.Errorf("expected ErrInvalidURI; got %v", err)
	}
}

func TestParseUnknownNamespace(t *testing.T) {
	t.Parallel()
	_, err := Parse("cog://nonexistent/foo")
	if err == nil {
		t.Fatal("expected error for unknown namespace")
	}
	if !errors.Is(err, ErrUnknownNamespace) {
		t.Errorf("expected ErrUnknownNamespace; got %v", err)
	}
}

func TestParseMissingNamespace(t *testing.T) {
	t.Parallel()
	_, err := Parse("cog:///foo")
	if err == nil {
		t.Fatal("expected error for missing namespace")
	}
	if !errors.Is(err, ErrInvalidURI) {
		t.Errorf("expected ErrInvalidURI; got %v", err)
	}
}

// ── String (round-trip) ─────────────────────────────────────────────────────────

func TestStringRoundTrip(t *testing.T) {
	t.Parallel()
	// String() always emits the canonical bare form regardless of input form.
	cases := []struct {
		input string
		want  string
	}{
		{"cog:mem/semantic/insights/eigenform", "cog:mem/semantic/insights/eigenform"},
		{"cog://mem/semantic/insights/eigenform", "cog:mem/semantic/insights/eigenform"},
		{"cog:crystal", "cog:crystal"},
		{"cog://crystal", "cog:crystal"},
		{"cog:thread/current#last-10", "cog:thread/current#last-10"},
		{"cog://thread/current#last-10", "cog:thread/current#last-10"},
		{"cog:conf/kernel.yaml", "cog:conf/kernel.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			u, err := Parse(tc.input)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			got := u.String()
			if got != tc.want {
				t.Errorf("String() = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestStringWithQuery(t *testing.T) {
	t.Parallel()
	u, err := Parse("cog:signals/inference?above=0.3")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := u.String()
	if !strings.HasPrefix(s, "cog:signals/inference?") {
		t.Errorf("unexpected String() = %q", s)
	}
	if !strings.Contains(s, "above=0.3") {
		t.Errorf("String() missing query param; got %q", s)
	}
}

// ── Query helpers ───────────────────────────────────────────────────────────────

func TestGetQueryInt(t *testing.T) {
	t.Parallel()
	u, _ := Parse("cog:context?budget=50000")
	if got := u.GetQueryInt("budget", 0); got != 50000 {
		t.Errorf("GetQueryInt = %d; want 50000", got)
	}
	if got := u.GetQueryInt("missing", 42); got != 42 {
		t.Errorf("GetQueryInt missing = %d; want 42", got)
	}
}

func TestGetQueryFloat(t *testing.T) {
	t.Parallel()
	u, _ := Parse("cog:signals/inference?above=0.3")
	if got := u.GetQueryFloat("above", 0); got != 0.3 {
		t.Errorf("GetQueryFloat = %f; want 0.3", got)
	}
}

func TestGetQueryBool(t *testing.T) {
	t.Parallel()
	u, _ := Parse("cog:context?verbose=true")
	if !u.GetQueryBool("verbose") {
		t.Error("GetQueryBool(verbose) = false; want true")
	}
	if u.GetQueryBool("missing") {
		t.Error("GetQueryBool(missing) = true; want false")
	}
}

func TestWithQuery(t *testing.T) {
	t.Parallel()
	u, _ := Parse("cog:context?budget=50000")
	u2 := u.WithQuery("model", "sonnet")
	// Original unchanged.
	if u.GetQuery("model") != "" {
		t.Error("original URI modified by WithQuery")
	}
	if u2.GetQuery("model") != "sonnet" {
		t.Errorf("new URI model = %q; want sonnet", u2.GetQuery("model"))
	}
	// Budget carried over.
	if u2.GetQuery("budget") != "50000" {
		t.Errorf("budget not carried over: %q", u2.GetQuery("budget"))
	}
}

// ── Path helpers ────────────────────────────────────────────────────────────────

func TestHasPath(t *testing.T) {
	t.Parallel()
	u1, _ := Parse("cog://mem/semantic/foo")
	if !u1.HasPath() {
		t.Error("expected HasPath=true")
	}
	u2, _ := Parse("cog://crystal")
	if u2.HasPath() {
		t.Error("expected HasPath=false")
	}
}

func TestPathSegments(t *testing.T) {
	t.Parallel()
	u, _ := Parse("cog://mem/semantic/insights/eigenform")
	segs := u.PathSegments()
	if len(segs) != 3 {
		t.Fatalf("PathSegments len = %d; want 3", len(segs))
	}
	if segs[0] != "semantic" || segs[1] != "insights" || segs[2] != "eigenform" {
		t.Errorf("PathSegments = %v", segs)
	}
}

func TestPathSegmentsEmpty(t *testing.T) {
	t.Parallel()
	u, _ := Parse("cog://crystal")
	if segs := u.PathSegments(); segs != nil {
		t.Errorf("expected nil; got %v", segs)
	}
}

func TestIsNamespace(t *testing.T) {
	t.Parallel()
	u1, _ := Parse("cog://crystal")
	if !u1.IsNamespace() {
		t.Error("expected IsNamespace=true for cog://crystal")
	}
	u2, _ := Parse("cog://mem/foo")
	if u2.IsNamespace() {
		t.Error("expected IsNamespace=false for cog://mem/foo")
	}
}

// ── IsCogURI ────────────────────────────────────────────────────────────────────

func TestIsCogURI(t *testing.T) {
	t.Parallel()
	if !IsCogURI("cog://mem/foo") {
		t.Error("expected true for cog://mem/foo")
	}
	if IsCogURI("https://example.com") {
		t.Error("expected false for https://")
	}
	if IsCogURI("") {
		t.Error("expected false for empty string")
	}
}

// ── ADR-067 compliance ─────────────────────────────────────────────────────────

// TestParseDigestFailClosed verifies that a URI carrying ?digest=sha256:...
// is rejected by Parse per ADR-067 §170 (fail-closed integrity contract).
func TestParseDigestFailClosed(t *testing.T) {
	t.Parallel()
	digests := []string{
		"cog:mem/semantic/foo.cog.md?digest=sha256:abc123",
		"cog://mem/semantic/foo.cog.md?digest=sha256:abc123",
		"cog:conf/kernel.yaml?digest=sha256:deadbeef",
		// digest alongside other params
		"cog:mem/foo.cog.md?ref=main&digest=sha256:111",
	}
	for _, raw := range digests {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(raw)
			if err == nil {
				t.Fatalf("Parse(%q): expected error (digest fail-closed), got nil", raw)
			}
			if !errors.Is(err, ErrInvalidURI) {
				t.Errorf("Parse(%q): got %v; want errors.Is ErrInvalidURI (digest fail-closed)", raw, err)
			}
		})
	}
}

// TestParseNonDigestQueryAllowed verifies non-digest query params do not trigger
// the digest fail-closed check.
func TestParseNonDigestQueryAllowed(t *testing.T) {
	t.Parallel()
	_, err := Parse("cog:mem/semantic/foo.cog.md?ref=main")
	if err != nil {
		t.Errorf("non-digest query: unexpected error %v", err)
	}
}

// TestParseBothFormsRoundTrip verifies that both URI forms parse successfully
// and that String() always emits the canonical bare form, so a second Parse
// of the emitted form reproduces an identical URI.
func TestParseBothFormsRoundTrip(t *testing.T) {
	t.Parallel()
	pairs := []struct {
		bare      string
		authority string
	}{
		{"cog:mem/semantic/insights/eigenform.cog.md", "cog://mem/semantic/insights/eigenform.cog.md"},
		{"cog:conf/kernel.yaml", "cog://conf/kernel.yaml"},
		{"cog:crystal", "cog://crystal"},
		{"cog:thread/current#last-10", "cog://thread/current#last-10"},
	}
	for _, p := range pairs {
		p := p
		t.Run(p.bare, func(t *testing.T) {
			t.Parallel()

			uBare, err := Parse(p.bare)
			if err != nil {
				t.Fatalf("Parse(bare %q): %v", p.bare, err)
			}
			uAuth, err := Parse(p.authority)
			if err != nil {
				t.Fatalf("Parse(authority %q): %v", p.authority, err)
			}

			// Both must emit the same canonical string.
			if uBare.String() != uAuth.String() {
				t.Errorf("bare.String()=%q != auth.String()=%q", uBare.String(), uAuth.String())
			}

			// Round-trip: re-parse the emitted string → must succeed and reproduce.
			reparse, err := Parse(uBare.String())
			if err != nil {
				t.Fatalf("Parse(round-trip %q): %v", uBare.String(), err)
			}
			if reparse.String() != uBare.String() {
				t.Errorf("round-trip mismatch: %q → %q", uBare.String(), reparse.String())
			}

			// Namespace and Path must be identical for both forms.
			if uBare.Namespace != uAuth.Namespace {
				t.Errorf("Namespace: bare=%q auth=%q", uBare.Namespace, uAuth.Namespace)
			}
			if uBare.Path != uAuth.Path {
				t.Errorf("Path: bare=%q auth=%q", uBare.Path, uAuth.Path)
			}
		})
	}
}

// ── cog://voices/* namespace (Primitive 3 — VoiceProfile) ──────────────────────

// TestParseVoicesNamespace verifies that cog://voices/* URIs are accepted by
// the parser now that "voices" is registered in the Namespaces map (Wave 6c).
func TestParseVoicesNamespace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw      string
		wantNS   string
		wantPath string
	}{
		{"cog:voices/cog", "voices", "cog"},
		{"cog://voices/cog", "voices", "cog"},
		{"cog:voices/cog/ecapa-embedding", "voices", "cog/ecapa-embedding"},
		{"cog:voices/bm_lewis", "voices", "bm_lewis"},
		{"cog:voices/eng_uk_m_davids", "voices", "eng_uk_m_davids"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			u, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.raw, err)
			}
			if u.Namespace != tc.wantNS {
				t.Errorf("Namespace = %q; want %q", u.Namespace, tc.wantNS)
			}
			if u.Path != tc.wantPath {
				t.Errorf("Path = %q; want %q", u.Path, tc.wantPath)
			}
		})
	}
}

// TestIsValidNamespace_Voices asserts that "voices" is recognized as a valid
// cog: namespace after the Wave 6c registration.
func TestIsValidNamespace_Voices(t *testing.T) {
	t.Parallel()
	if !IsValidNamespace("voices") {
		t.Error("IsValidNamespace(\"voices\") = false; want true")
	}
}

// ── Fragment-before-query rejection in canonical parser (issue #171 follow-up) ──

// TestParseFragmentBeforeQuery_Rejected verifies that pkg/uri.Parse rejects a URI
// where '#' appears before '?' — the canonical parser must enforce RFC 3986 ordering
// so every caller (resolver, registry, future callers) gets consistent protection.
//
// Without the fix: url.Parse folds "?digest=..." into the Fragment field, so the
// digest fail-closed check never fires and digest verification can be bypassed.
func TestParseFragmentBeforeQuery_Rejected(t *testing.T) {
	t.Parallel()
	malformed := []string{
		"cog:mem/foo.cog.md#Section?digest=sha256:abc",
		"cog://mem/semantic/x#Anchor?digest=sha256:deadbeef",
		"cog:conf/kernel.yaml#top?ref=main",
	}
	for _, raw := range malformed {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(raw)
			if err == nil {
				t.Fatalf("Parse(%q): expected error for fragment-before-query URI, got nil", raw)
			}
			if !errors.Is(err, ErrInvalidURI) {
				t.Errorf("Parse(%q): got %v; want errors.Is ErrInvalidURI", raw, err)
			}
		})
	}
}

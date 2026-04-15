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
		{
			raw:    "cog://mem/semantic/insights/eigenform",
			wantNS: "mem", wantPath: "semantic/insights/eigenform",
		},
		{
			raw:    "cog://mem/semantic/insights/eigenform.cog.md#Seed",
			wantNS: "mem", wantPath: "semantic/insights/eigenform.cog.md", wantFrag: "Seed",
		},
		{
			raw:    "cog://conf/kernel.yaml",
			wantNS: "conf", wantPath: "kernel.yaml",
		},
		{
			raw:    "cog://crystal",
			wantNS: "crystal",
		},
		{
			raw:       "cog://signals/inference?above=0.3",
			wantNS:    "signals",
			wantPath:  "inference",
			wantQuery: map[string]string{"above": "0.3"},
		},
		{
			raw:       "cog://context?budget=50000&model=sonnet",
			wantNS:    "context",
			wantQuery: map[string]string{"budget": "50000", "model": "sonnet"},
		},
		{
			raw:      "cog://thread/current#last-10",
			wantNS:   "thread",
			wantPath: "current",
			wantFrag: "last-10",
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
	cases := []string{
		"cog://mem/semantic/insights/eigenform",
		"cog://crystal",
		"cog://thread/current#last-10",
		"cog://conf/kernel.yaml",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			u, err := Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			got := u.String()
			if got != raw {
				t.Errorf("String() = %q; want %q", got, raw)
			}
		})
	}
}

func TestStringWithQuery(t *testing.T) {
	t.Parallel()
	u, err := Parse("cog://signals/inference?above=0.3")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := u.String()
	if !strings.HasPrefix(s, "cog://signals/inference?") {
		t.Errorf("unexpected String() = %q", s)
	}
	if !strings.Contains(s, "above=0.3") {
		t.Errorf("String() missing query param; got %q", s)
	}
}

// ── Query helpers ───────────────────────────────────────────────────────────────

func TestGetQueryInt(t *testing.T) {
	t.Parallel()
	u, _ := Parse("cog://context?budget=50000")
	if got := u.GetQueryInt("budget", 0); got != 50000 {
		t.Errorf("GetQueryInt = %d; want 50000", got)
	}
	if got := u.GetQueryInt("missing", 42); got != 42 {
		t.Errorf("GetQueryInt missing = %d; want 42", got)
	}
}

func TestGetQueryFloat(t *testing.T) {
	t.Parallel()
	u, _ := Parse("cog://signals/inference?above=0.3")
	if got := u.GetQueryFloat("above", 0); got != 0.3 {
		t.Errorf("GetQueryFloat = %f; want 0.3", got)
	}
}

func TestGetQueryBool(t *testing.T) {
	t.Parallel()
	u, _ := Parse("cog://context?verbose=true")
	if !u.GetQueryBool("verbose") {
		t.Error("GetQueryBool(verbose) = false; want true")
	}
	if u.GetQueryBool("missing") {
		t.Error("GetQueryBool(missing) = true; want false")
	}
}

func TestWithQuery(t *testing.T) {
	t.Parallel()
	u, _ := Parse("cog://context?budget=50000")
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

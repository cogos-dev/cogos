package constellation

import "testing"

// TestBuildKeywordFTSQuery covers the AND-default fix for the OR-join
// pattern flagged in review as the sibling of the mcp_stubs.go bug fixed
// for #568: QueryRelevant, QueryRelevantWithSubstance, and
// QueryRelevantWithEmbedding all built their FTS5 MATCH query by
// unconditionally OR-joining every extracted keyword, which made every
// relevance query match any document containing any single keyword.
func TestBuildKeywordFTSQuery(t *testing.T) {
	tests := []struct {
		name     string
		keywords []string
		want     string
	}{
		{"empty", nil, ""},
		{"single keyword", []string{"claude"}, `"claude"`},
		{"two keywords AND, not OR", []string{"spirit", "letter"}, `"spirit" "letter"`},
		{"three keywords", []string{"spirit", "over", "letter"}, `"spirit" "over" "letter"`},
		{"embedded quote stripped", []string{`cla"ude`}, `"claude"`},
		{"empty keyword after stripping is dropped", []string{`"`, "claude"}, `"claude"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildKeywordFTSQuery(tt.keywords)
			if got != tt.want {
				t.Errorf("buildKeywordFTSQuery(%#v) = %q, want %q", tt.keywords, got, tt.want)
			}
		})
	}
}

// TestBuildKeywordFTSQuery_NotOROfKeywords is a regression guard: the fixed
// query must never contain a bare " OR " between multiple keywords, since
// FTS5 treats space-separated terms as an implicit AND and " OR " was the
// exact unselective pattern being removed.
func TestBuildKeywordFTSQuery_NotOROfKeywords(t *testing.T) {
	got := buildKeywordFTSQuery([]string{"spirit", "over", "letter"})
	want := `"spirit" "over" "letter"`
	if got != want {
		t.Fatalf("buildKeywordFTSQuery = %q, want %q (AND-joined, not OR-joined)", got, want)
	}
}

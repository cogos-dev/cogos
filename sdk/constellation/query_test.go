package constellation

import "testing"

// TestBuildKeywordFTSQuery covers buildKeywordFTSQuery: it OR-joins already
// -extracted keywords (unchanged selectivity from the pre-existing
// strings.Join(keywords, " OR ") behavior) but individually double-quotes
// each keyword first, so a keyword containing FTS5 special characters
// (notably an embedded '"') can never produce invalid FTS5 syntax.
//
// This deliberately does NOT adopt the AND-default fix applied to
// buildFTSQuery in internal/engine/mcp_stubs.go. That fix targets a typed
// search-box query (a handful of words, matched precisely); extractKeywords
// (anchor, goal) has no cap and can plausibly yield 8-15+ keywords for
// realistic multi-sentence input, feeding a recall-then-rank pipeline
// (maxCandidates candidates, substance/embedding re-ranking, maxResults
// truncation) that depends on OR's broader recall to have anything to rank.
// AND-joining that many keywords would require literal co-occurrence of
// every one in a single document, collapsing the candidate pool toward
// zero for anything short of a near-verbatim quote of the target document.
func TestBuildKeywordFTSQuery(t *testing.T) {
	tests := []struct {
		name     string
		keywords []string
		want     string
	}{
		{"empty", nil, ""},
		{"single keyword", []string{"claude"}, `"claude"`},
		{"two keywords, OR not AND", []string{"spirit", "letter"}, `"spirit" OR "letter"`},
		{"three keywords", []string{"spirit", "over", "letter"}, `"spirit" OR "over" OR "letter"`},
		{"embedded quote stripped", []string{`cla"ude`}, `"claude"`},
		{"empty keyword after stripping is dropped", []string{`"`, "claude"}, `"claude"`},
		// A realistic extractKeywords output for multi-sentence anchor/goal
		// text: well beyond "a handful" of keywords. Must still produce a
		// valid, non-degenerate OR query -- not something that requires
		// every term to co-occur.
		{
			"realistic multi-sentence keyword count stays OR-joined",
			[]string{"spirit", "letter", "phrase", "search", "query", "relevance",
				"substance", "ranking", "candidate", "embedding", "document", "index"},
			`"spirit" OR "letter" OR "phrase" OR "search" OR "query" OR "relevance" OR ` +
				`"substance" OR "ranking" OR "candidate" OR "embedding" OR "document" OR "index"`,
		},
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

// TestBuildKeywordFTSQuery_NotANDOfKeywords is a regression guard: the
// query must never implicitly-AND multiple keywords together (bare
// space-separated terms, which FTS5 treats as AND) -- only an explicit
// " OR " join, matching the pre-existing recall-then-rank contract that
// QueryRelevant / QueryRelevantWithSubstance / QueryRelevantWithEmbedding
// depend on.
func TestBuildKeywordFTSQuery_NotANDOfKeywords(t *testing.T) {
	got := buildKeywordFTSQuery([]string{"spirit", "over", "letter"})
	want := `"spirit" OR "over" OR "letter"`
	if got != want {
		t.Fatalf("buildKeywordFTSQuery = %q, want %q (OR-joined, not AND-joined)", got, want)
	}
}

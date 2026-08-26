// serve_compat_memory_search_test.go — negative controls for GET /memory/search.
//
// Regression guards for myrgic/cogos#578: handleMemorySearch used to score with
//
//	combined := queryRelevance(doc, keywords)*2.0 + salience
//
// where queryRelevance is capped at 1.0 (so it contributes at most +2.0) and
// salience is unbounded (observed 4.2–4.3 on the live corpus). Salience is
// query-independent, so it dominated the sort and the query string was inert:
// three unrelated queries returned byte-identical results with identical
// scores, and the `combined <= 0` guard admitted relevance-0 documents.
//
// The tests below are the acceptance criteria from that issue. They are
// deliberately behavioural (query in → results out) rather than pinned to the
// implementation, so a future rewrite that keeps the contract still passes.
package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// newMemorySearchServer builds a Server whose WorkspaceRoot contains the FTS
// fixture at the canonical .cog/.state/constellation.db location, so
// SearchMemory finds it.
func newMemorySearchServer(t *testing.T) *Server {
	t.Helper()

	root := t.TempDir()
	stateDir := filepath.Join(root, ".cog", ".state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	// setupFTSFixture (mcp_stubs_test.go) seeds documents + documents_fts and
	// skips the test if the binary lacks FTS5.
	fixturePath, cleanup := setupFTSFixture(t)
	t.Cleanup(cleanup)

	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture db: %v", err)
	}
	dbPath := filepath.Join(stateDir, "constellation.db")
	if err := os.WriteFile(dbPath, data, 0o644); err != nil {
		t.Fatalf("stage fixture db: %v", err)
	}

	return &Server{cfg: &Config{WorkspaceRoot: root}}
}

// doSearch issues GET /memory/search?query=… and decodes the response.
func doSearch(t *testing.T, s *Server, query string) (int, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet,
		"/memory/search?query="+url.QueryEscape(query), nil)
	rec := httptest.NewRecorder()
	s.handleMemorySearch(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return res.StatusCode, nil
	}

	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode response for %q: %v", query, err)
	}
	return res.StatusCode, out
}

// resultPaths extracts the ordered path list from a search response.
func resultPaths(t *testing.T, out map[string]any) []string {
	t.Helper()

	raw, ok := out["results"]
	if !ok || raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("results is %T, want []any", raw)
	}
	paths := make([]string, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("result item is %T, want map", it)
		}
		p, _ := m["path"].(string)
		paths = append(paths, p)
	}
	return paths
}

// TestMemorySearch_QueryIsNotInert is the primary regression guard for #578.
//
// Unrelated queries must produce different result sets. Under the old scorer
// every query returned the same salience-ranked list, so this is the single
// test that would have caught the bug.
func TestMemorySearch_QueryIsNotInert(t *testing.T) {
	s := newMemorySearchServer(t)

	_, claudeOut := doSearch(t, s, "claude")
	_, kernelOut := doSearch(t, s, "kernel")

	claude := resultPaths(t, claudeOut)
	kernel := resultPaths(t, kernelOut)

	if len(claude) == 0 {
		t.Fatal(`query "claude" returned 0 results; expected matches`)
	}
	if len(kernel) == 0 {
		t.Fatal(`query "kernel" returned 0 results; expected matches`)
	}

	if fmt.Sprint(claude) == fmt.Sprint(kernel) {
		t.Fatalf("unrelated queries returned identical result sets (%v); "+
			"the query string is inert — see myrgic/cogos#578", claude)
	}

	// "kernel" matches only doc-3; it must not surface the claude documents.
	for _, p := range kernel {
		if p == "/mem/a.md" || p == "/mem/b.md" {
			t.Errorf(`query "kernel" returned claude-only document %s`, p)
		}
	}
}

// TestMemorySearch_ZeroHitsReturnsZero verifies the caller can distinguish
// "no matches" from "I am broken". The old handler returned 20 salience-ranked
// documents for a term absent from the corpus.
func TestMemorySearch_ZeroHitsReturnsZero(t *testing.T) {
	s := newMemorySearchServer(t)

	_, out := doSearch(t, s, "zzzznotinthecorpuszzzz")

	paths := resultPaths(t, out)
	if len(paths) != 0 {
		t.Errorf("absent term returned %d results (%v); want 0", len(paths), paths)
	}

	switch c := out["count"].(type) {
	case float64:
		if c != 0 {
			t.Errorf("count = %v, want 0", c)
		}
	case nil:
		// absent count with empty results is acceptable
	default:
		t.Errorf("unexpected count type %T", c)
	}
}

// TestMemorySearch_MatchesDocumentBody verifies content is actually searched.
//
// The old queryRelevance scored title+id+tags+basename only, so a phrase
// present solely in a document body was unreachable. "anthropic" appears only
// in doc-1's content.
func TestMemorySearch_MatchesDocumentBody(t *testing.T) {
	s := newMemorySearchServer(t)

	_, out := doSearch(t, s, "anthropic")

	paths := resultPaths(t, out)
	if len(paths) == 0 {
		t.Fatal(`body-only term "anthropic" returned 0 results; ` +
			`document bodies are not being searched`)
	}

	found := false
	for _, p := range paths {
		if p == "/mem/a.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("body-only term did not return its document; got %v", paths)
	}
}

// TestMemorySearch_MissingQueryIsBadRequest keeps the argument contract.
func TestMemorySearch_MissingQueryIsBadRequest(t *testing.T) {
	s := newMemorySearchServer(t)

	req := httptest.NewRequest(http.MethodGet, "/memory/search", nil)
	rec := httptest.NewRecorder()
	s.handleMemorySearch(rec, req)

	if rec.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("missing query: status = %d, want %d",
			rec.Result().StatusCode, http.StatusBadRequest)
	}
}

// TestQueryRelevanceCannotOutrankSalience pins the *arithmetic* defect from
// myrgic/cogos#578 as a standing invariant, independent of any handler.
//
// The old ranking was `queryRelevance(doc, kw)*2.0 + salience`. This test
// documents why that shape is unsound: queryRelevance is bounded by 1.0, so
// its maximum contribution is 2.0, while salience is unbounded and was
// observed at 4.2–4.3 on the live corpus. A perfectly-matching document
// therefore loses to a non-matching one whenever the latter's salience exceeds
// the former's by more than 2.0 — which is why unrelated queries returned
// byte-identical, salience-ordered results.
//
// If a future change reintroduces an additive salience term, this test states
// the bound that must be respected: relevance has to be able to outrank any
// attentional weight, or the query is not what determines the ordering.
func TestQueryRelevanceCannotOutrankSalience(t *testing.T) {
	perfectMatch := &IndexedCogdoc{
		Title: "autonomic memory decay",
		ID:    "autonomic-memory-decay",
		Path:  "/mem/semantic/autonomic-memory-decay.cog.md",
	}
	noMatch := &IndexedCogdoc{
		Title: "raycast pack test capture",
		ID:    "raycast-pack-test-capture",
		Path:  "/mem/semantic/inbox/captures/raycast-pack-test-capture.cog.md",
	}

	keywords := []string{"autonomic", "memory", "decay"}

	best := queryRelevance(perfectMatch, keywords)
	worst := queryRelevance(noMatch, keywords)

	if best != 1.0 {
		t.Fatalf("expected a perfect metadata match to score 1.0, got %v", best)
	}
	if worst != 0.0 {
		t.Fatalf("expected a non-matching doc to score 0.0, got %v", worst)
	}

	// The old formula, with salience values actually observed in production.
	const (
		relevanceWeight = 2.0
		salienceOfMatch = 0.5 // a considered doc, rarely touched
		salienceOfJunk  = 4.3 // a bulk-ingest capture, touched by every migration
		oldScoreOfMatch = 1.0*relevanceWeight + salienceOfMatch
		oldScoreOfJunk  = 0.0*relevanceWeight + salienceOfJunk
	)

	if oldScoreOfJunk <= oldScoreOfMatch {
		t.Fatal("fixture no longer reproduces the inversion; " +
			"update the salience constants to observed values")
	}

	t.Logf("old ranking inverts: non-matching doc scores %.1f, "+
		"perfect match scores %.1f", oldScoreOfJunk, oldScoreOfMatch)

	// The invariant: relevance must be capable of outranking salience.
	maxRelevanceContribution := 1.0 * relevanceWeight
	if maxRelevanceContribution < salienceOfJunk {
		t.Logf("confirmed unsound: max relevance contribution %.1f < "+
			"observed salience %.1f, so the query cannot determine order",
			maxRelevanceContribution, salienceOfJunk)
	}
}

// TestRankScore_RelevanceStrictlyDominatesSalience guards the sibling ranking
// path in serve_foveated.go, which had the same defect (myrgic/cogos#578
// review finding 3).
//
// rankScore must make relevance the primary key: no amount of salience may
// promote a less-relevant document above a more-relevant one. Salience may only
// reorder documents that are equally relevant.
//
// The multi-keyword cases matter most. queryRelevance returns
// matches/numKeywords, so the gap between adjacent relevance levels shrinks as
// the query lengthens; a tiebreaker that ignores keyword count would bridge
// that gap for any query longer than one word.
func TestRankScore_RelevanceStrictlyDominatesSalience(t *testing.T) {
	pathological := []float64{4.3, 50, 1e6}

	// Across query lengths, one more keyword matched must always win,
	// regardless of how salient the weaker match is.
	for _, n := range []int{1, 2, 3, 5, 10} {
		for _, junkSalience := range pathological {
			for matched := 0; matched < n; matched++ {
				weaker := rankScore(float64(matched)/float64(n), junkSalience, n)
				stronger := rankScore(float64(matched+1)/float64(n), 0, n)
				if weaker >= stronger {
					t.Errorf("n=%d salience=%g: doc matching %d/%d keywords "+
						"(%.6f) beat one matching %d/%d (%.6f); relevance "+
						"must dominate", n, junkSalience, matched, n, weaker,
						matched+1, n, stronger)
				}
			}
		}
	}

	// Within equal relevance, higher salience must win — the tiebreaker still
	// has to do its job.
	if rankScore(1.0, 5.0, 1) <= rankScore(1.0, 0.5, 1) {
		t.Error("salience failed to break a tie between equally relevant docs")
	}

	// The tiebreak span must stay strictly below one match-step (1.0), which
	// is what keeps it from bridging a relevance gap at any keyword count.
	if got := rankScore(0, 1e9, 3) - rankScore(0, 0, 3); got >= 1.0 {
		t.Errorf("salience tiebreak spans %.6f; must stay below 1.0", got)
	}

	// Defensive: a negative salience must not subtract from relevance.
	if rankScore(1.0, -5, 1) != rankScore(1.0, 0, 1) {
		t.Error("negative salience altered the score")
	}
}

// TestNoRankingPathUsesUnboundedSalience is a whole-class guard.
//
// myrgic/cogos#578 was fixed three times: handleMemorySearch, then
// serve_foveated.go, then context_assembly.go — each found only after the
// previous fix shipped. This test asserts the invariant across the package
// rather than per-site, so a fourth instance cannot be introduced silently.
//
// It scans non-test sources for the unsound `relevance*2.0 + salience` shape
// used as an ORDERING value. The one permitted survivor is the admission gate
// in context_assembly.go, which deliberately keeps the legacy scale because
// salienceFloor is user-configurable and rescaling it would change which
// documents every existing workspace admits — it decides "above the noise
// floor at all", never relative order.
func TestNoRankingPathUsesUnboundedSalience(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	// file -> the single variable name permitted to hold the legacy formula.
	allowed := map[string]string{
		"context_assembly.go": "gate", // admission threshold, not a sort key
	}

	pattern := regexp.MustCompile(`(\w+)\s*:?=\s*relevance\*2\.0 \+ salience`)

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range pattern.FindAllStringSubmatch(string(src), -1) {
			varName := m[1]
			if allowed[name] == varName {
				continue
			}
			t.Errorf("%s: `%s := relevance*2.0 + salience` reintroduces the "+
				"unbounded-salience ranking defect (#578). Use "+
				"rankScore(relevance, salience, len(keywords)) so relevance "+
				"stays the primary key.", name, varName)
		}
	}
}

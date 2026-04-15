package uri

import (
	"strings"
	"testing"
)

func TestExtractInlineRefsBasic(t *testing.T) {
	t.Parallel()
	content := `See cog://mem/semantic/foo.cog.md and cog://conf/kernel.yaml for details.`
	refs := ExtractInlineRefs(content)
	if len(refs) != 2 {
		t.Fatalf("got %d refs; want 2: %v", len(refs), refs)
	}
	// Sorted output.
	if refs[0] != "cog://conf/kernel.yaml" {
		t.Errorf("refs[0] = %q; want cog://conf/kernel.yaml", refs[0])
	}
	if refs[1] != "cog://mem/semantic/foo.cog.md" {
		t.Errorf("refs[1] = %q; want cog://mem/semantic/foo.cog.md", refs[1])
	}
}

func TestExtractInlineRefsDeduplicated(t *testing.T) {
	t.Parallel()
	content := `cog://mem/foo.md appears here and cog://mem/foo.md again here.`
	refs := ExtractInlineRefs(content)
	if len(refs) != 1 {
		t.Errorf("got %d refs; want 1 (deduplicated): %v", len(refs), refs)
	}
}

func TestExtractInlineRefsWithFragment(t *testing.T) {
	t.Parallel()
	content := `Read cog://mem/doc.cog.md#the-seed for context.`
	refs := ExtractInlineRefs(content)
	if len(refs) != 1 {
		t.Fatalf("got %d refs; want 1: %v", len(refs), refs)
	}
	if !strings.Contains(refs[0], "#the-seed") {
		t.Errorf("fragment not captured in ref %q", refs[0])
	}
}

func TestExtractInlineRefsEmpty(t *testing.T) {
	t.Parallel()
	if refs := ExtractInlineRefs("no URIs here"); refs != nil {
		t.Errorf("expected nil; got %v", refs)
	}
	if refs := ExtractInlineRefs(""); refs != nil {
		t.Errorf("expected nil for empty string; got %v", refs)
	}
}

func TestExtractInlineRefsInMarkdownLink(t *testing.T) {
	t.Parallel()
	content := `[link](cog://mem/semantic/foo.cog.md) and plain cog://mem/bar.cog.md`
	refs := ExtractInlineRefs(content)
	if len(refs) != 2 {
		t.Errorf("got %d refs; want 2: %v", len(refs), refs)
	}
}

func TestExtractInlineRefsDoesNotMatchHTTPS(t *testing.T) {
	t.Parallel()
	content := `See https://example.com/foo and cog://mem/bar.md`
	refs := ExtractInlineRefs(content)
	for _, r := range refs {
		if strings.HasPrefix(r, "https://") {
			t.Errorf("https:// URI incorrectly matched: %q", r)
		}
	}
}

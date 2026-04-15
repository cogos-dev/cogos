package uri

import (
	"regexp"
	"sort"
)

// cogURIPattern matches cog:// URI references embedded in document content.
// It captures the scheme, namespace, optional path, and optional #fragment.
var cogURIPattern = regexp.MustCompile(
	`cog://\w+(?:/[\w./_-]*)?(?:#[\w-]*)?`,
)

// ExtractInlineRefs scans content for embedded cog:// URIs and returns a
// deduplicated, sorted slice of every unique URI found.
func ExtractInlineRefs(content string) []string {
	raw := cogURIPattern.FindAllString(content, -1)
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

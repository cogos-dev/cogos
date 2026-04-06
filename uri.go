// uri.go — cog:// URI projection system for CogOS v3
//
// A cog:// URI has the form:
//
//	cog://type/path[#fragment]
//
// "type" selects a Projection that maps to a filesystem location under the
// workspace root.  "path" is the resource name within that projection.
// "fragment" (optional, after '#') identifies a section within the file.
//
// Examples:
//
//	cog://mem/semantic/insights/eigenform.cog.md        → .cog/mem/semantic/insights/eigenform.cog.md
//	cog://mem/semantic/insights/eigenform.cog.md#Seed   → same path, anchor "Seed"
//	cog://conf/kernel.yaml                              → .cog/config/kernel.yaml
//	cog://crystal                                       → .cog/ledger/crystal.json
package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ── Projection ───────────────────────────────────────────────────────────────

// Projection defines how a cog:// URI type maps to the filesystem.
type Projection struct {
	// Base is the workspace-relative prefix under the workspace root
	// (e.g. ".cog/mem/"). Mutually exclusive with ExtBase.
	Base string
	// ExtBase is a workspace-root-relative prefix for paths that live
	// outside .cog/ (e.g. ".claude/skills/").
	ExtBase string
	// Pattern controls resolution: "direct" | "directory" | "glob" | "singleton".
	Pattern string
	// Suffix is appended to the resolved path for "direct" patterns
	// (e.g. ".cog.md" for specs).
	Suffix string
	// GlobPat is a fmt.Sprintf template (one %s) for "glob" patterns.
	// E.g. "%s-*.md" matches numbered ADR files.
	GlobPat string
}

// projections maps every known cog:// type to its filesystem projection.
// Aliases (e.g. "role"/"roles") point to equivalent Projections.
var projections = map[string]*Projection{
	// ── Memory corpus ────────────────────────────────────────────────────
	"mem": {Base: ".cog/mem/", Pattern: "direct"},

	// ── Architecture decision records ────────────────────────────────────
	"adr": {Base: ".cog/adr/", Pattern: "glob", GlobPat: "%s-*.md"},

	// ── Role definitions ─────────────────────────────────────────────────
	"role":  {Base: ".cog/roles/", Pattern: "directory"},
	"roles": {Base: ".cog/roles/", Pattern: "directory"},

	// ── Skills (portable — live outside .cog/) ───────────────────────────
	"skill":  {ExtBase: ".claude/skills/", Pattern: "directory"},
	"skills": {ExtBase: ".claude/skills/", Pattern: "directory"},

	// ── Agent definitions ────────────────────────────────────────────────
	"agent":  {Base: ".cog/bin/agents/", Pattern: "directory"},
	"agents": {Base: ".cog/bin/agents/", Pattern: "directory"},

	// ── Specs / RFCs ─────────────────────────────────────────────────────
	"spec":  {Base: ".cog/specs/", Pattern: "direct", Suffix: ".cog.md"},
	"specs": {Base: ".cog/specs/", Pattern: "direct", Suffix: ".cog.md"},

	// ── Status snapshots ─────────────────────────────────────────────────
	"status": {Base: ".cog/status/", Pattern: "direct", Suffix: ".json"},

	// ── Event ledger ─────────────────────────────────────────────────────
	"ledger": {Base: ".cog/ledger/", Pattern: "directory"},

	// ── Crystal state (singleton) ────────────────────────────────────────
	// cog://crystal resolves to .cog/ledger/crystal.json (no path component).
	"crystal": {Base: ".cog/ledger/crystal.json", Pattern: "singleton"},

	// ── Raw .cog/ paths ──────────────────────────────────────────────────
	"kernel":    {Base: ".cog/", Pattern: "direct"},
	"canonical": {Base: ".cog/", Pattern: "direct"},

	// Config aliases.
	"conf":   {Base: ".cog/config/", Pattern: "direct"},
	"config": {Base: ".cog/config/", Pattern: "direct"},

	// ── Ontology definitions ─────────────────────────────────────────────
	"ontology": {Base: ".cog/ontology/", Pattern: "direct", Suffix: ".cog.md"},

	// ── Work items ───────────────────────────────────────────────────────
	"work": {Base: ".cog/work/", Pattern: "direct"},

	// ── Handoff documents ────────────────────────────────────────────────
	"handoff":  {Base: ".cog/handoffs/", Pattern: "direct", Suffix: ".md"},
	"handoffs": {Base: ".cog/handoffs/", Pattern: "directory"},

	// ── Artifacts ────────────────────────────────────────────────────────
	"artifact":  {Base: ".cog/ledger/", Pattern: "glob", GlobPat: "*/artifacts/%s.*"},
	"artifacts": {Base: ".cog/ledger/", Pattern: "glob", GlobPat: "*/artifacts/%s.*"},

	// ── Docs ─────────────────────────────────────────────────────────────
	"docs": {Base: ".cog/docs/", Pattern: "direct"},

	// ── Hooks ────────────────────────────────────────────────────────────
	"hooks": {Base: ".cog/hooks/", Pattern: "direct"},
}

// ── Resolution ───────────────────────────────────────────────────────────────

// URIResolution is the result of resolving a cog:// URI to the filesystem.
type URIResolution struct {
	// Path is the absolute filesystem path.
	Path string
	// Fragment is the section anchor stripped from the URI (empty if none).
	Fragment string
}

// cogURIPattern matches cog:// URI references embedded in document content.
// It captures the scheme, type, optional path, and optional #fragment.
var cogURIPattern = regexp.MustCompile(
	`cog://` + // scheme
		`\w+` + // type (required)
		`(?:/[\w./_-]*)?` + // /path (optional; allows dots, slashes, underscores, hyphens)
		`(?:#[\w-]*)? `, // #fragment (optional)
)

// init replaces the trailing space in the pattern (used to anchor the
// character class close) with a version that stops at natural boundaries.
func init() {
	cogURIPattern = regexp.MustCompile(
		`cog://\w+(?:/[\w./_-]*)?(?:#[\w-]*)?`,
	)
}

// ResolveURI resolves a cog:// URI to an absolute filesystem path.
// The #fragment part (section anchor) is separated and returned in
// URIResolution.Fragment without modifying the path resolution.
func ResolveURI(workspaceRoot, uri string) (*URIResolution, error) {
	if !strings.HasPrefix(uri, "cog://") {
		return nil, fmt.Errorf("not a cog:// URI: %q", uri)
	}

	// Strip scheme.
	rest := strings.TrimPrefix(uri, "cog://")

	// Split off fragment.
	fragment := ""
	if idx := strings.IndexByte(rest, '#'); idx >= 0 {
		fragment = rest[idx+1:]
		rest = rest[:idx]
	}

	// Split type from path (path may be empty for singletons like cog://crystal).
	uriType, uriPath, _ := strings.Cut(rest, "/")

	proj, ok := projections[uriType]
	if !ok {
		return nil, fmt.Errorf("unknown cog:// type %q in URI %q", uriType, uri)
	}

	path, err := resolveProjection(workspaceRoot, proj, uriPath)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", uri, err)
	}
	return &URIResolution{Path: path, Fragment: fragment}, nil
}

// resolveProjection applies a Projection to produce an absolute filesystem path.
func resolveProjection(workspaceRoot string, proj *Projection, uriPath string) (string, error) {
	// Compute base directory.
	var base string
	if proj.ExtBase != "" {
		base = filepath.Join(workspaceRoot, proj.ExtBase)
	} else {
		base = filepath.Join(workspaceRoot, proj.Base)
	}

	switch proj.Pattern {
	case "singleton":
		// Base already encodes the complete workspace-relative path.
		return filepath.Join(workspaceRoot, proj.Base), nil

	case "direct":
		return filepath.Join(base, uriPath) + proj.Suffix, nil

	case "directory":
		if uriPath == "" {
			return base + string(filepath.Separator), nil
		}
		return filepath.Join(base, uriPath) + string(filepath.Separator), nil

	case "glob":
		pattern := filepath.Join(base, fmt.Sprintf(proj.GlobPat, uriPath))
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", fmt.Errorf("glob %q: %w", pattern, err)
		}
		if len(matches) == 0 {
			return "", fmt.Errorf("no files match glob %q", pattern)
		}
		// Return the first (alphabetically first) match for determinism.
		return matches[0], nil

	default:
		return "", fmt.Errorf("unknown projection pattern %q", proj.Pattern)
	}
}

// ── Reverse mapping (path → URI) ─────────────────────────────────────────────

// uriMapping maps a workspace-relative filesystem path prefix to a cog:// URI prefix.
type uriMapping struct {
	// pathPrefix is workspace-relative (e.g. ".cog/mem/").
	pathPrefix string
	// uriPrefix is the cog:// prefix including trailing slash (e.g. "cog://mem/").
	uriPrefix string
	// stripExts lists file extensions to remove from the name component.
	// Applied in order; first match wins.
	stripExts []string
}

// uriMappings ordered longest-prefix-first so specific paths take precedence
// over generic catch-alls (e.g. ".cog/bin/agents/" before ".cog/").
var uriMappings = []uriMapping{
	{".cog/bin/agents/", "cog://agents/", []string{".md"}},
	{".cog/handoffs/", "cog://handoffs/", []string{".md"}},
	{".cog/ontology/", "cog://ontology/", []string{".cog.md", ".md"}},
	{".cog/config/", "cog://conf/", nil},
	{".cog/hooks/", "cog://hooks/", nil},
	{".cog/specs/", "cog://specs/", []string{".cog.md", ".md"}},
	{".cog/roles/", "cog://roles/", []string{".md"}},
	{".cog/status/", "cog://status/", []string{".json"}},
	{".cog/ledger/", "cog://ledger/", nil},
	{".cog/work/", "cog://work/", nil},
	{".cog/docs/", "cog://docs/", nil},
	{".cog/adr/", "cog://adr/", []string{".md"}},
	{".cog/mem/", "cog://mem/", nil}, // keep full extension for mem
	{".cog/", "cog://kernel/", nil},
	{".claude/skills/", "cog://skills/", nil},
	{".claude/agents/", "cog://agents/", nil},
}

// PathToURI converts an absolute (or workspace-relative) filesystem path to a
// cog:// URI using the longest-matching prefix rule.
// Returns an error if no mapping covers the path.
func PathToURI(workspaceRoot, path string) (string, error) {
	// Normalise to absolute.
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workspaceRoot, path)
	}

	for _, m := range uriMappings {
		// Build the absolute prefix string, then append separator so we don't
		// accidentally match ".cog/mem" against ".cog/memory/".
		prefix := filepath.Join(workspaceRoot, m.pathPrefix)
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		if !strings.HasPrefix(abs, prefix) {
			continue
		}

		rel := strings.TrimPrefix(abs, prefix)
		// Strip recognised extensions.
		for _, ext := range m.stripExts {
			if strings.HasSuffix(rel, ext) {
				rel = rel[:len(rel)-len(ext)]
				break
			}
		}
		return m.uriPrefix + rel, nil
	}
	return "", fmt.Errorf("no cog:// mapping for path %q", path)
}

// ── Inline reference extraction ──────────────────────────────────────────────

// ExtractInlineRefs scans document content for embedded cog:// URIs and
// returns a deduplicated, sorted slice of every unique URI found.
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

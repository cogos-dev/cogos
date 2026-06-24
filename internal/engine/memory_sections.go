// memory_sections.go — Port of legacy MemoryTOC + MemoryIndex from
// cog-workspace/.cog/memory.go:670-765 (+ supporting helpers from
// cog-workspace/.cog/lib/go/frontmatter/sections.go).
//
// RFC-017 Cogdoc Substrate Unity, Phase B: eliminates the v3→shell→legacy
// dependency inversion for section-aware memory operations. Before this file
// existed the kernel had no way to surface `cog memory toc` / `cog memory
// index` except by shelling out to scripts/cog, which invoked the 2.5.0
// monolith binary. Agents reaching the v3 kernel via MCP therefore could
// not generate or inspect the `sections:` frontmatter block that makes
// cheap section-addressed reads possible — and without that block, every
// cog_read_cogdoc call degrades to reading the full document.
//
// The port is behavior-exact with the legacy implementation. Specifically:
//
//   - ParseMemorySections matches .cog/lib/go/frontmatter/sections.go:ParseSections.
//     Identical fenced-code-block skipping, anchor extraction, 1-indexed
//     line numbering, EndLine-exclusive ranges, and per-section byte size
//     accounting (one extra byte per line for the newline).
//
//   - GenerateMemorySectionsYAML matches sections.go:GenerateSectionsYAML:
//     only level ≥ 2 headings flow into the frontmatter block, the YAML
//     shape is `- title/anchor/line/size` (anchor omitted when empty), and
//     serialization is gopkg.in/yaml.v3 default output.
//
//   - MemoryTOC matches memory.go:670. Same stripped-frontmatter body,
//     same --yaml path emitting `sections:\n` + 2-space-indented YAML, same
//     textual table (header, lines/bytes totals, per-section indentation by
//     (level-2)*2 spaces, fixed-width columns).
//
//   - MemoryIndex matches memory.go:718. Dry-run returns the `sections:`
//     block only; live-run requires existing frontmatter, removes any prior
//     `sections:` field with its continuation lines, appends the new block,
//     and writes via atomicWriteFile (temp file + fsync + rename). Refuses
//     to clobber a file that has no frontmatter to inject into.
//
// The v3 kernel already has a type `Section` (cogblock.go:70) with a
// different shape — Title/Anchor/Hash/Size — used for CogBlock content
// addressing. To avoid colliding with it, the ported type is named
// `MemorySection` here. The two types serve different purposes: Section
// is content-hash-addressed for ADR-059 delta sync; MemorySection is
// line-addressed for the legacy-compatible frontmatter TOC block.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// MemorySection describes a markdown heading and its content range within
// a CogDoc body. Matches the legacy Section type in lib/go/frontmatter/
// sections.go field-for-field (the fields consumed by the legacy
// frontmatter/TOC code paths).
type MemorySection struct {
	Title   string `yaml:"title" json:"title"`
	Anchor  string `yaml:"anchor,omitempty" json:"anchor,omitempty"`
	Level   int    `yaml:"level" json:"level"`
	Line    int    `yaml:"line" json:"line"`         // 1-indexed start line
	EndLine int    `yaml:"end_line" json:"end_line"` // 1-indexed end line (exclusive)
	Size    int    `yaml:"size" json:"size"`         // bytes, incl. newlines
}

// memSectionAnchorRe matches {#anchor-id} at the end of a heading title.
var memSectionAnchorRe = regexp.MustCompile(`\{#([a-zA-Z0-9_-]+)\}\s*$`)

// memSectionHeadingRe matches markdown ATX headings (# through ######).
var memSectionHeadingRe = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)

// ParseMemorySections scans markdown body for ATX headings and returns a
// flat list of sections with computed line ranges and byte sizes. Headings
// inside fenced code blocks (``` or ~~~) are ignored. The result includes
// level-1 headings so MemoryTOC can display document titles; the
// frontmatter serializer (GenerateMemorySectionsYAML) filters those out.
func ParseMemorySections(body string) []MemorySection {
	lines := strings.Split(body, "\n")
	var sections []MemorySection
	inCodeBlock := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}

		m := memSectionHeadingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		level := len(m[1])
		title := strings.TrimSpace(m[2])
		anchor := ""

		// Extract {#anchor} if present.
		if am := memSectionAnchorRe.FindStringSubmatch(title); am != nil {
			anchor = am[1]
			title = strings.TrimSpace(memSectionAnchorRe.ReplaceAllString(title, ""))
		}

		sections = append(sections, MemorySection{
			Title:  title,
			Anchor: anchor,
			Level:  level,
			Line:   i + 1, // 1-indexed
		})
	}

	// Compute EndLine and Size for each section.
	for i := range sections {
		if i+1 < len(sections) {
			sections[i].EndLine = sections[i+1].Line
		} else {
			sections[i].EndLine = len(lines) + 1
		}

		// Byte size of the section, counting a newline per line (matching
		// the legacy implementation; a document that ends without a final
		// newline therefore over-counts by one byte on its last section —
		// intentional for parity).
		start := sections[i].Line - 1 // 0-indexed
		end := sections[i].EndLine - 1
		if end > len(lines) {
			end = len(lines)
		}
		size := 0
		for j := start; j < end; j++ {
			size += len(lines[j]) + 1
		}
		sections[i].Size = size
	}

	return sections
}

// GenerateMemorySectionsYAML produces the YAML payload for a `sections:`
// frontmatter field. The return value is the YAML list body only — the
// caller prepends `sections:\n` and indents each line by two spaces, in
// both the live-write path (MemoryIndex) and the read-only YAML path
// (MemoryTOC with asYAML=true).
//
// Only level ≥ 2 headings are emitted. The level-1 heading of a document
// is the document title; the section index is for navigation within the
// body, so including it would be redundant.
func GenerateMemorySectionsYAML(body string) string {
	sections := ParseMemorySections(body)
	if len(sections) == 0 {
		return ""
	}

	// Entry shape matches the legacy projection exactly. yaml.v3 omits
	// empty Anchor (omitempty) and stable-orders the struct fields in
	// declaration order.
	type sectionEntry struct {
		Title  string `yaml:"title"`
		Anchor string `yaml:"anchor,omitempty"`
		Line   int    `yaml:"line"`
		Size   int    `yaml:"size"`
	}

	var entries []sectionEntry
	for _, s := range sections {
		if s.Level < 2 {
			continue
		}
		entries = append(entries, sectionEntry{
			Title:  s.Title,
			Anchor: s.Anchor,
			Line:   s.Line,
			Size:   s.Size,
		})
	}

	if len(entries) == 0 {
		return ""
	}

	data, err := yaml.Marshal(entries)
	if err != nil {
		return ""
	}
	return string(data)
}

// splitFrontmatter extracts frontmatter and body from a CogDoc exactly
// matching the legacy frontmatter.Extract semantics (lib/go/frontmatter/
// frontmatter.go:Extract). hasFM is true iff the content opens with
// "---\n" AND a closing "\n---" is found before EOF. When hasFM is false
// the returned fmText is empty and body equals the full input content —
// mirroring how MemoryTOC and MemoryIndex treat pre-frontmatter or
// frontmatter-less documents.
func splitFrontmatter(content string) (fmText, body string, hasFM bool) {
	if !strings.HasPrefix(content, "---\n") {
		return "", content, false
	}
	end := strings.Index(content[4:], "\n---")
	if end == -1 {
		return "", content, false
	}
	fmText = content[4 : 4+end]
	fmText = strings.TrimSuffix(fmText, "\n")
	body = strings.TrimPrefix(content[4+end+4:], "\n")
	return fmText, body, true
}

// resolveMemoryDocPath resolves an input path to an absolute filesystem
// path. Handles memory-relative paths (`.cog/mem/...`), workspace-relative
// paths, absolute paths, and memory-bare paths (e.g. `semantic/foo.md`).
// Mirrors legacy memory.go:resolveDocPath:769.
func resolveMemoryDocPath(path, cogRoot string) (string, error) {
	// This resolver feeds both reads (cog_memory_toc) and writes
	// (cog_memory_index) from caller-supplied paths, so an unconstrained path is
	// an arbitrary-filesystem primitive on the MCP surface. Reject absolute input
	// and contain every resolution branch within the workspace root (the function
	// legitimately reaches workspace-relative files like .cog/ontology, so the
	// boundary is cogRoot, not .cog/mem).
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("memory doc path %q: absolute paths not permitted", path)
	}

	memoryDir := filepath.Join(cogRoot, ".cog", "mem")
	var candidate string
	switch {
	case strings.Contains(path, "/.cog/mem/"):
		parts := strings.SplitN(path, "/.cog/mem/", 2)
		candidate = filepath.Join(memoryDir, parts[1])
	case strings.HasPrefix(path, ".cog/mem/"):
		candidate = filepath.Join(memoryDir, strings.TrimPrefix(path, ".cog/mem/"))
	default:
		// Try workspace-relative first (some paths like
		// `.cog/ontology/crystal.cog.md` live outside `.cog/mem/`), else
		// fall back to memory-relative.
		wsPath := filepath.Join(cogRoot, path)
		if _, err := os.Stat(wsPath); err == nil {
			candidate = wsPath
		} else {
			candidate = filepath.Join(memoryDir, path)
		}
	}

	if !pathWithin(cogRoot, candidate) {
		return "", fmt.Errorf("memory doc path %q escapes workspace", path)
	}
	return filepath.Clean(candidate), nil
}

// readCogDocContentV3 is the side-effect-free read used by MemoryTOC and
// MemoryIndex. It does NOT update last_accessed (the legacy counterpart
// at memory.go:620 is named readCogDocContent and has the same contract).
// The v3-specific suffix avoids clashing with any future kernel-wide
// read helper.
func readCogDocContentV3(cogRoot, path string) (string, error) {
	fullPath, perr := resolveMemoryDocPath(path, cogRoot)
	if perr != nil {
		return "", perr
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", path)
	}
	return string(data), nil
}

// indentLines prefixes each non-empty line of s with prefix and appends a
// trailing newline. Matches legacy memory.go:indent.
func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// formatMemorySectionSize formats a byte count as a human-readable size
// string for section display in the textual TOC. Matches legacy
// memory.go:formatSectionSize.
func formatMemorySectionSize(b int) string {
	if b < 1024 {
		return fmt.Sprintf("%dB", b)
	}
	return fmt.Sprintf("%.1fKB", float64(b)/1024.0)
}

// removeMemoryFrontmatterField removes a YAML field (including any array
// or mapping continuation lines) from a frontmatter body. Used by
// MemoryIndex to strip any stale `sections:` block before appending a
// freshly generated one. Mirrors legacy memory.go:removeFrontmatterField.
//
// The legacy heuristic: lines that start with the field prefix are
// elided; subsequent lines that are indented (space or tab) or that start
// with `- ` (array item) are considered continuation and also elided; the
// first top-level line terminates the removal.
func removeMemoryFrontmatterField(fm, field string) string {
	lines := strings.Split(fm, "\n")
	var result []string
	inField := false
	fieldPrefix := field + ":"

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, fieldPrefix) {
			inField = true
			continue
		}

		if inField {
			if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(trimmed, "- ") {
				continue
			}
			inField = false
		}

		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

// atomicWriteMemoryFile writes data to path via temp-file + fsync +
// rename, refusing to clobber a non-empty file with zero-byte content.
// Mirrors legacy memory.go:atomicWriteFile. Distinct name from
// engine/config_write.go:atomicWriteConfigFile because the config helper
// pre-creates parent directories — for memory writes the caller should
// already have a path to an existing file (MemoryIndex only modifies
// cogdocs that were read moments earlier).
//
// See cog-workspace/.cog/mem/reflective/incidents/
// 2026-04-16-cogdoc-truncation.md for the truncation incident that made
// the empty-content guard load-bearing.
func atomicWriteMemoryFile(path string, data []byte, perm os.FileMode) error {
	// Guard: never write into archived/attic regions (R-1, 2026-06-09).
	if IsProtectedArchivePath(path) {
		return fmt.Errorf("refusing write into protected archive path: %s", path)
	}
	// Guard: refuse to truncate a non-empty file to zero bytes.
	if len(data) == 0 {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return fmt.Errorf("refusing to truncate non-empty file %s to zero bytes", path)
		}
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cogdoc-*.tmp")
	if err != nil {
		return fmt.Errorf("atomicWriteMemoryFile: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// Clean up temp on any failure before rename.
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("atomicWriteMemoryFile: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("atomicWriteMemoryFile: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("atomicWriteMemoryFile: close: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("atomicWriteMemoryFile: chmod: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("atomicWriteMemoryFile: rename: %w", err)
	}
	return nil
}

// MemoryTOC returns a formatted table of contents for a cogdoc. When
// asYAML is true the output is the raw `sections:` YAML block (the same
// shape MemoryIndex would inject into the frontmatter). When asYAML is
// false the output is a pretty-printed text table with total line count,
// total byte size, per-section line ranges, and sizes.
//
// Port of legacy cog-workspace/.cog/memory.go:670.
func MemoryTOC(cogRoot, path string, asYAML bool) (string, error) {
	content, err := readCogDocContentV3(cogRoot, path)
	if err != nil {
		return "", err
	}

	// Strip frontmatter to get body.
	body := content
	if _, b, ok := splitFrontmatter(content); ok {
		body = b
	}

	if asYAML {
		yamlStr := GenerateMemorySectionsYAML(body)
		if yamlStr == "" {
			return "", fmt.Errorf("no sections found in: %s", path)
		}
		return "sections:\n" + indentLines(yamlStr, "  "), nil
	}

	sections := ParseMemorySections(body)
	if len(sections) == 0 {
		return "", fmt.Errorf("no sections found in: %s", path)
	}

	totalLines := len(strings.Split(content, "\n"))

	var b strings.Builder
	fmt.Fprintf(&b, "Sections in %s (%d lines, %d bytes):\n\n", path, totalLines, len(content))

	for _, s := range sections {
		prefix := strings.Repeat("#", s.Level)
		sizeStr := formatMemorySectionSize(s.Size)
		indent := ""
		if s.Level > 2 {
			indent = strings.Repeat("  ", s.Level-2)
		}
		fmt.Fprintf(&b, "  %s%-4s %-40s lines %4d-%-4d %s\n",
			indent, prefix, s.Title, s.Line, s.EndLine-1, sizeStr)
	}
	return b.String(), nil
}

// MemoryIndex generates the `sections:` YAML block for a cogdoc and
// injects it into the document's frontmatter, replacing any prior
// sections field. When dryRun is true the injection is skipped and the
// generated block is returned for preview; the on-disk file is unchanged.
//
// Port of legacy cog-workspace/.cog/memory.go:718.
//
// Failure modes preserved exactly:
//   - `no sections found in: <path>` — the body has no headings level 2+.
//   - `no frontmatter to inject into: <path>` — live write requires an
//     existing frontmatter block; indexing a plain markdown file is a
//     no-op by design (the operator is expected to cog memory write
//     first, then index).
//
// Writes are atomic: a sibling `.cogdoc-*.tmp` file is written + fsynced
// + renamed over the destination, so a mid-write crash leaves either the
// old content or the new content, never a zero-byte file.
func MemoryIndex(cogRoot, path string, dryRun bool) (string, error) {
	content, err := readCogDocContentV3(cogRoot, path)
	if err != nil {
		return "", err
	}

	// Strip frontmatter to get body.
	body := content
	fmContent := ""
	if fm, b, ok := splitFrontmatter(content); ok {
		body = b
		fmContent = fm
	}

	yamlStr := GenerateMemorySectionsYAML(body)
	if yamlStr == "" {
		return "", fmt.Errorf("no sections found in: %s", path)
	}

	sectionsBlock := "sections:\n" + indentLines(yamlStr, "  ")

	if dryRun {
		return sectionsBlock, nil
	}

	// Inject into frontmatter.
	if fmContent == "" {
		return "", fmt.Errorf("no frontmatter to inject into: %s", path)
	}

	// Remove existing sections: block if present.
	newFM := removeMemoryFrontmatterField(fmContent, "sections")
	newFM = strings.TrimRight(newFM, "\n") + "\n" + sectionsBlock

	newContent := "---\n" + newFM + "---\n" + body

	fullPath, perr := resolveMemoryDocPath(path, cogRoot)
	if perr != nil {
		return "", perr
	}
	if err := atomicWriteMemoryFile(fullPath, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("failed to write: %w", err)
	}

	return fmt.Sprintf("Indexed %s (%d sections)", path, len(ParseMemorySections(body))), nil
}

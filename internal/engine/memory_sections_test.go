package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── ParseMemorySections ───────────────────────────────────────────────────────

func TestParseMemorySections_EmptyBody(t *testing.T) {
	t.Parallel()
	got := ParseMemorySections("")
	// strings.Split("", "\n") returns [""] — a single empty line with
	// no headings produces no sections.
	if len(got) != 0 {
		t.Errorf("len = %d; want 0", len(got))
	}
}

func TestParseMemorySections_HeadingsOnly(t *testing.T) {
	t.Parallel()
	body := "# Title\n\n## Section A\n\nbody a\n\n## Section B\n\nbody b\n"
	got := ParseMemorySections(body)
	if len(got) != 3 {
		t.Fatalf("len = %d; want 3; got %+v", len(got), got)
	}
	if got[0].Title != "Title" || got[0].Level != 1 || got[0].Line != 1 {
		t.Errorf("section[0] = %+v", got[0])
	}
	if got[1].Title != "Section A" || got[1].Level != 2 || got[1].Line != 3 {
		t.Errorf("section[1] = %+v", got[1])
	}
	if got[2].Title != "Section B" || got[2].Level != 2 || got[2].Line != 7 {
		t.Errorf("section[2] = %+v", got[2])
	}
	// The body ends with a trailing newline, so strings.Split yields
	// 10 elements (the last "" after the final \n). EndLine of the last
	// section spans to len(lines)+1 = 11.
	if got[2].EndLine != 11 {
		t.Errorf("section[2].EndLine = %d; want 11", got[2].EndLine)
	}
	// EndLine of section A should equal section B's start (line 7).
	if got[1].EndLine != 7 {
		t.Errorf("section[1].EndLine = %d; want 7", got[1].EndLine)
	}
}

func TestParseMemorySections_WithAnchors(t *testing.T) {
	t.Parallel()
	body := "## The Constants {#constants}\n\nfoo\n"
	got := ParseMemorySections(body)
	if len(got) != 1 {
		t.Fatalf("len = %d; want 1", len(got))
	}
	if got[0].Title != "The Constants" {
		t.Errorf("Title = %q; want %q", got[0].Title, "The Constants")
	}
	if got[0].Anchor != "constants" {
		t.Errorf("Anchor = %q; want %q", got[0].Anchor, "constants")
	}
}

func TestParseMemorySections_SkipsHeadingsInCodeBlocks(t *testing.T) {
	t.Parallel()
	body := "## Real\n\n```\n# fake heading\n## also fake\n```\n\n## After\n"
	got := ParseMemorySections(body)
	if len(got) != 2 {
		t.Fatalf("len = %d; want 2; got %+v", len(got), got)
	}
	if got[0].Title != "Real" || got[1].Title != "After" {
		t.Errorf("titles = [%q %q]; want [Real After]", got[0].Title, got[1].Title)
	}
}

func TestParseMemorySections_TildeFenceAlsoSkipped(t *testing.T) {
	t.Parallel()
	body := "## Real\n\n~~~\n## buried\n~~~\n\n## After\n"
	got := ParseMemorySections(body)
	if len(got) != 2 {
		t.Fatalf("len = %d; want 2; got %+v", len(got), got)
	}
	if got[0].Title != "Real" || got[1].Title != "After" {
		t.Errorf("titles = [%q %q]; want [Real After]", got[0].Title, got[1].Title)
	}
}

func TestParseMemorySections_SizeAccountingIncludesNewlines(t *testing.T) {
	t.Parallel()
	// "## H\n" (5) + "a\n" (2) + "b\n" (2) = 9 bytes for section 1;
	// "## H2\n" (6) for section 2 on its own line.
	body := "## H\na\nb\n## H2\n"
	got := ParseMemorySections(body)
	if len(got) != 2 {
		t.Fatalf("len = %d; want 2", len(got))
	}
	if got[0].Size != 9 {
		t.Errorf("section[0].Size = %d; want 9", got[0].Size)
	}
}

// ── GenerateMemorySectionsYAML ────────────────────────────────────────────────

func TestGenerateMemorySectionsYAML_FiltersLevel1(t *testing.T) {
	t.Parallel()
	body := "# Title\n\n## A\n\n## B\n"
	yaml := GenerateMemorySectionsYAML(body)
	if strings.Contains(yaml, "Title") {
		t.Errorf("YAML should NOT include level-1 Title; got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "title: A") {
		t.Errorf("YAML should include A; got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "title: B") {
		t.Errorf("YAML should include B; got:\n%s", yaml)
	}
}

func TestGenerateMemorySectionsYAML_EmptyWhenNoLevel2(t *testing.T) {
	t.Parallel()
	if yaml := GenerateMemorySectionsYAML("# Only Title\n"); yaml != "" {
		t.Errorf("YAML should be empty when only level-1 heading; got %q", yaml)
	}
	if yaml := GenerateMemorySectionsYAML("no headings at all\n"); yaml != "" {
		t.Errorf("YAML should be empty when no headings; got %q", yaml)
	}
}

func TestGenerateMemorySectionsYAML_OmitsEmptyAnchor(t *testing.T) {
	t.Parallel()
	body := "## Plain\n\ntext\n## With Anchor {#a}\n\nmore\n"
	yaml := GenerateMemorySectionsYAML(body)
	// Plain section should not have an anchor key (YAML `anchor:` should
	// be absent). The second should carry anchor: a.
	if strings.Contains(yaml, "anchor: \"\"") {
		t.Errorf("YAML should omit empty anchor; got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "anchor: a") {
		t.Errorf("YAML should include anchor: a; got:\n%s", yaml)
	}
}

// ── splitFrontmatter ──────────────────────────────────────────────────────────

func TestSplitFrontmatter_WithFrontmatter(t *testing.T) {
	t.Parallel()
	content := "---\ntitle: Foo\n---\n# Body\n"
	fm, body, ok := splitFrontmatter(content)
	if !ok {
		t.Fatal("expected hasFM = true")
	}
	if fm != "title: Foo" {
		t.Errorf("fm = %q; want %q", fm, "title: Foo")
	}
	if body != "# Body\n" {
		t.Errorf("body = %q; want %q", body, "# Body\n")
	}
}

func TestSplitFrontmatter_NoFrontmatter(t *testing.T) {
	t.Parallel()
	content := "# Just body\n\ntext\n"
	fm, body, ok := splitFrontmatter(content)
	if ok {
		t.Errorf("expected hasFM = false")
	}
	if fm != "" {
		t.Errorf("fm = %q; want empty", fm)
	}
	if body != content {
		t.Errorf("body should equal content when no frontmatter; got %q", body)
	}
}

func TestSplitFrontmatter_UnclosedFrontmatter(t *testing.T) {
	t.Parallel()
	// Legacy behaviour: if the closing --- is missing, return hasFM=false
	// and treat the whole thing as body — avoid losing any content.
	content := "---\ntitle: Foo\n# Body"
	_, body, ok := splitFrontmatter(content)
	if ok {
		t.Error("expected hasFM = false for unclosed frontmatter")
	}
	if body != content {
		t.Errorf("body should equal content; got %q", body)
	}
}

// ── removeMemoryFrontmatterField ──────────────────────────────────────────────

func TestRemoveMemoryFrontmatterField_RemovesSectionsBlockWithArrayItems(t *testing.T) {
	t.Parallel()
	fm := `title: Foo
sections:
  - title: A
    line: 3
    size: 100
  - title: B
    line: 10
    size: 200
tags:
  - test`
	got := removeMemoryFrontmatterField(fm, "sections")
	if strings.Contains(got, "sections:") {
		t.Errorf("sections: should be removed; got:\n%s", got)
	}
	if strings.Contains(got, "title: A") {
		t.Errorf("sections array items should be removed; got:\n%s", got)
	}
	// tags: should survive.
	if !strings.Contains(got, "tags:") {
		t.Errorf("tags: field should survive; got:\n%s", got)
	}
	if !strings.Contains(got, "- test") {
		t.Errorf("tags item should survive; got:\n%s", got)
	}
}

func TestRemoveMemoryFrontmatterField_NoOpWhenFieldAbsent(t *testing.T) {
	t.Parallel()
	fm := "title: Foo\ntags:\n  - a\n"
	got := removeMemoryFrontmatterField(fm, "sections")
	if got != fm {
		t.Errorf("should be unchanged; got %q want %q", got, fm)
	}
}

// ── resolveMemoryDocPath ──────────────────────────────────────────────────────

func TestResolveMemoryDocPath_Variants(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	memDir := filepath.Join(root, ".cog", "mem")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		// Absolute input is now rejected (was an arbitrary-FS pass-through).
		{"absolute-rejected", "/some/absolute/path.md", "", true},
		{"dotcog-mem-prefix", ".cog/mem/semantic/foo.md", filepath.Join(memDir, "semantic/foo.md"), false},
		{"bare-memory-relative", "semantic/insights/foo.cog.md", filepath.Join(memDir, "semantic/insights/foo.cog.md"), false},
	}
	for _, tc := range cases {
		got, err := resolveMemoryDocPath(tc.in, root)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: got %q, want error", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q; want %q", tc.name, got, tc.want)
		}
	}
}

func TestResolveMemoryDocPath_WorkspaceRelativePreferred(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Create an ontology file OUTSIDE .cog/mem/ — resolution should
	// find it as workspace-relative rather than forcing it into .cog/mem/.
	if err := os.MkdirAll(filepath.Join(root, ".cog", "ontology"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cog", "ontology", "crystal.cog.md"), []byte("# Crystal\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveMemoryDocPath(".cog/ontology/crystal.cog.md", root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, ".cog", "ontology", "crystal.cog.md")
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

// ── MemoryTOC ─────────────────────────────────────────────────────────────────

func writeMemDoc(t *testing.T, root, relPath, content string) string {
	t.Helper()
	full := filepath.Join(root, ".cog", "mem", relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestMemoryTOC_TextOutput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	doc := `---
title: Testing TOC
---
# Testing TOC

## Introduction

Some intro text.

## Details

More content here.

### Sub-detail

A child heading.
`
	writeMemDoc(t, root, "semantic/insights/toc-test.cog.md", doc)

	out, err := MemoryTOC(root, "semantic/insights/toc-test.cog.md", false)
	if err != nil {
		t.Fatalf("MemoryTOC: %v", err)
	}
	// Header line includes total byte count.
	if !strings.Contains(out, "Sections in semantic/insights/toc-test.cog.md") {
		t.Errorf("output missing header; got:\n%s", out)
	}
	if !strings.Contains(out, "Introduction") {
		t.Errorf("output missing Introduction section; got:\n%s", out)
	}
	if !strings.Contains(out, "Details") {
		t.Errorf("output missing Details section; got:\n%s", out)
	}
	// Sub-detail (level 3) should be indented more than level-2 headings.
	if !strings.Contains(out, "Sub-detail") {
		t.Errorf("output missing Sub-detail; got:\n%s", out)
	}
}

func TestMemoryTOC_YAMLOutput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	doc := `---
title: Y
---
# Y

## A

body a

## B

body b
`
	writeMemDoc(t, root, "semantic/y.cog.md", doc)

	out, err := MemoryTOC(root, "semantic/y.cog.md", true)
	if err != nil {
		t.Fatalf("MemoryTOC yaml: %v", err)
	}
	if !strings.HasPrefix(out, "sections:\n") {
		t.Errorf("YAML output should start with sections:; got:\n%s", out)
	}
	if !strings.Contains(out, "title: A") {
		t.Errorf("YAML should include title: A; got:\n%s", out)
	}
	if !strings.Contains(out, "title: B") {
		t.Errorf("YAML should include title: B; got:\n%s", out)
	}
	// The level-1 title should NOT appear.
	if strings.Contains(out, "title: Y\n") {
		t.Errorf("YAML should NOT include level-1 title Y; got:\n%s", out)
	}
}

func TestMemoryTOC_ErrorWhenNoSections(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	doc := `---
title: empty
---
Just prose, no headings at all.
`
	writeMemDoc(t, root, "semantic/empty.cog.md", doc)

	if _, err := MemoryTOC(root, "semantic/empty.cog.md", false); err == nil {
		t.Error("expected error for doc with no sections")
	}
	if _, err := MemoryTOC(root, "semantic/empty.cog.md", true); err == nil {
		t.Error("expected error for doc with no sections (yaml mode)")
	}
}

func TestMemoryTOC_ErrorWhenFileMissing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cog", "mem"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := MemoryTOC(root, "nonexistent.md", false); err == nil {
		t.Error("expected error for missing file")
	}
}

// ── MemoryIndex ───────────────────────────────────────────────────────────────

func TestMemoryIndex_DryRunDoesNotWrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	doc := `---
title: dry
---
## One

body

## Two

body
`
	path := "semantic/dry.cog.md"
	full := writeMemDoc(t, root, path, doc)

	out, err := MemoryIndex(root, path, true)
	if err != nil {
		t.Fatalf("MemoryIndex dry-run: %v", err)
	}
	if !strings.HasPrefix(out, "sections:\n") {
		t.Errorf("dry-run output should start with sections:; got:\n%s", out)
	}
	// File on disk should be unchanged.
	after, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != doc {
		t.Errorf("dry-run mutated file; before:\n%s\nafter:\n%s", doc, string(after))
	}
}

func TestMemoryIndex_LiveInjectsSections(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	doc := `---
title: live
tags:
  - test
---
## Alpha

first

## Beta

second

### Beta child

nested
`
	path := "semantic/live.cog.md"
	full := writeMemDoc(t, root, path, doc)

	out, err := MemoryIndex(root, path, false)
	if err != nil {
		t.Fatalf("MemoryIndex live: %v", err)
	}
	// Message should report the count (level ≥ 2 is 3: Alpha, Beta, Beta child).
	if !strings.Contains(out, "Indexed") {
		t.Errorf("live output should mention Indexed; got %q", out)
	}
	if !strings.Contains(out, "3 sections") {
		t.Errorf("live output should count 3 sections; got %q", out)
	}

	// Verify the file now has a sections: block.
	after, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	ac := string(after)
	if !strings.Contains(ac, "sections:") {
		t.Errorf("file should have sections: block; got:\n%s", ac)
	}
	if !strings.Contains(ac, "title: Alpha") {
		t.Errorf("sections should list Alpha; got:\n%s", ac)
	}
	// tags: should survive (not cannibalized by removeFrontmatterField).
	if !strings.Contains(ac, "tags:") {
		t.Errorf("tags: should survive; got:\n%s", ac)
	}
	// Body should survive intact.
	if !strings.Contains(ac, "## Alpha") {
		t.Errorf("body should survive; got:\n%s", ac)
	}
	if !strings.Contains(ac, "### Beta child") {
		t.Errorf("nested heading body should survive; got:\n%s", ac)
	}
}

func TestMemoryIndex_LiveReplacesExistingSectionsBlock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Doc already has a stale sections: block — index should overwrite it
	// with a fresh one that reflects the current body.
	doc := `---
title: stale
sections:
  - title: old-stale-name
    line: 999
    size: 1
---
## New A

body

## New B

body
`
	path := "semantic/stale.cog.md"
	full := writeMemDoc(t, root, path, doc)

	if _, err := MemoryIndex(root, path, false); err != nil {
		t.Fatalf("MemoryIndex: %v", err)
	}

	after, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	ac := string(after)
	if strings.Contains(ac, "old-stale-name") {
		t.Errorf("stale sections entry should be gone; got:\n%s", ac)
	}
	if !strings.Contains(ac, "title: New A") {
		t.Errorf("new section should be indexed; got:\n%s", ac)
	}
	if !strings.Contains(ac, "title: New B") {
		t.Errorf("new section should be indexed; got:\n%s", ac)
	}
}

func TestMemoryIndex_ErrorWhenNoFrontmatter(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	doc := "## A\nbody\n"
	path := "semantic/no-fm.md"
	writeMemDoc(t, root, path, doc)

	_, err := MemoryIndex(root, path, false)
	if err == nil {
		t.Fatal("expected error: no frontmatter to inject into")
	}
	if !strings.Contains(err.Error(), "no frontmatter") {
		t.Errorf("error should mention missing frontmatter; got %v", err)
	}
}

func TestMemoryIndex_DryRunWorksWithoutFrontmatter(t *testing.T) {
	t.Parallel()
	// Dry-run does NOT require frontmatter — it only asks for sections
	// to generate. This lets agents preview the block for a plain .md
	// file before deciding whether to add a frontmatter.
	root := t.TempDir()
	doc := "## A\n\nbody\n"
	path := "semantic/plain.md"
	writeMemDoc(t, root, path, doc)

	out, err := MemoryIndex(root, path, true)
	if err != nil {
		t.Fatalf("MemoryIndex dry-run without fm: %v", err)
	}
	if !strings.HasPrefix(out, "sections:\n") {
		t.Errorf("dry-run output should still produce sections: block; got:\n%s", out)
	}
}

func TestMemoryIndex_ErrorWhenNoSectionsFound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	doc := `---
title: empty
---
Just prose.
`
	path := "semantic/no-secs.cog.md"
	writeMemDoc(t, root, path, doc)

	if _, err := MemoryIndex(root, path, false); err == nil {
		t.Error("expected error for doc with no sections")
	}
}

// ── atomicWriteMemoryFile ─────────────────────────────────────────────────────

func TestAtomicWriteMemoryFile_RefusesZeroByteOverwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(path, []byte("# Keep\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := atomicWriteMemoryFile(path, []byte{}, 0644)
	if err == nil {
		t.Fatal("expected error: refuse to truncate non-empty file")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error should mention refusing; got %v", err)
	}
	// File must still contain the original content.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# Keep\n" {
		t.Errorf("file mutated despite guard; got %q", string(got))
	}
}

func TestAtomicWriteMemoryFile_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	payload := []byte("# Hello\n\nbody.\n")
	if err := atomicWriteMemoryFile(path, payload, 0644); err != nil {
		t.Fatalf("atomicWriteMemoryFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("round-trip mismatch; got %q want %q", string(got), string(payload))
	}
}

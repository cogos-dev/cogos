//go:build mcpserver

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── Pipeline routing ─────────────────────────────────────────────────────────

func TestIngestPipelineURL(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	inboxDir := filepath.Join(tmp, ".cog", "mem", "semantic", "inbox", "links")
	if err := os.MkdirAll(inboxDir, 0755); err != nil {
		t.Fatalf("mkdir inbox: %v", err)
	}

	pipeline := NewIngestPipeline(tmp)
	urlDecomposer := NewURLDecomposer(tmp)
	pipeline.Register(urlDecomposer)

	req := &IngestRequest{
		Source: SourceURL,
		Format: FormatURL,
		Data:   "https://github.com/anthropics/claude-code",
	}

	// Verify the URLDecomposer reports it can handle this request.
	if !urlDecomposer.CanDecompose(req) {
		t.Fatal("URLDecomposer.CanDecompose returned false for a FormatURL request")
	}

	// Also verify CanDecompose works via URL prefix heuristic (no explicit format).
	reqHeuristic := &IngestRequest{
		Source: SourceURL,
		Format: "unknown",
		Data:   "https://github.com/anthropics/claude-code",
	}
	if !urlDecomposer.CanDecompose(reqHeuristic) {
		t.Fatal("URLDecomposer.CanDecompose should match https:// prefix regardless of format")
	}

	// Non-URL data should not match.
	reqPlain := &IngestRequest{
		Source: SourceFile,
		Format: FormatDocument,
		Data:   "just some plain text",
	}
	if urlDecomposer.CanDecompose(reqPlain) {
		t.Fatal("URLDecomposer.CanDecompose should return false for plain text")
	}
}

// ── No-match fallback ────────────────────────────────────────────────────────

func TestIngestPipelineNoMatch(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	pipeline := NewIngestPipeline(tmp) // no decomposers registered

	req := &IngestRequest{
		Source: SourceFile,
		Format: FormatDocument,
		Data:   "some opaque blob",
	}

	result, err := pipeline.Ingest(context.Background(), req)
	if err != nil {
		t.Fatalf("Ingest returned error: %v", err)
	}

	if result.ContentType != ContentUnknown {
		t.Errorf("ContentType = %q; want %q", result.ContentType, ContentUnknown)
	}
	if result.Title != "some opaque blob" {
		t.Errorf("Title = %q; want raw data echoed back", result.Title)
	}
	if result.Source != SourceFile {
		t.Errorf("Source = %q; want %q", result.Source, SourceFile)
	}
	if result.SourceID != "some opaque blob" {
		t.Errorf("SourceID = %q; want raw data as fallback dedup key", result.SourceID)
	}
}

// ── Deduplication ────────────────────────────────────────────────────────────

func TestDedupChecker(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	inboxDir := filepath.Join(tmp, ".cog", "mem", "semantic", "inbox", "links")
	if err := os.MkdirAll(inboxDir, 0755); err != nil {
		t.Fatalf("mkdir inbox: %v", err)
	}

	// Write a fake CogDoc with source_id in frontmatter.
	fakeCogDoc := `---
title: "Test Link"
source_id: "https://example.com/test"
status: raw
---

# Test Link
`
	fakeFile := filepath.Join(inboxDir, "test-link.cog.md")
	if err := os.WriteFile(fakeFile, []byte(fakeCogDoc), 0644); err != nil {
		t.Fatalf("write fake cogdoc: %v", err)
	}

	dedup := NewDedupChecker(tmp)

	if !dedup.IsDuplicate(`"https://example.com/test"`) {
		t.Error("IsDuplicate should return true for existing source_id")
	}

	if dedup.IsDuplicate("https://example.com/other") {
		t.Error("IsDuplicate should return false for unknown source_id")
	}

	// Empty string should never match.
	if dedup.IsDuplicate("") {
		t.Error("IsDuplicate should return false for empty string")
	}
}

// ── CogDoc writing ───────────────────────────────────────────────────────────

func TestWriteCogDocIntegration(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()

	opts := CogDocWriteOpts{
		Title:    "Test Insight",
		Content:  "This is a test CogDoc with meaningful content.",
		Tags:     []string{"test", "integration"},
		Status:   "raw",
		DocType:  "link",
		Source:   "url",
		URL:      "https://example.com/article",
		SourceID: "https://example.com/article",
	}

	memPath := "semantic/inbox/links/test-insight.cog.md"
	uri, err := WriteCogDoc(tmp, memPath, opts)
	if err != nil {
		t.Fatalf("WriteCogDoc: %v", err)
	}

	expectedURI := "cog:mem/" + memPath
	if uri != expectedURI {
		t.Errorf("URI = %q; want %q", uri, expectedURI)
	}

	// Read back the written file.
	fullPath := filepath.Join(tmp, ".cog", "mem", memPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)

	// Verify frontmatter fields.
	checks := []struct {
		label, needle string
	}{
		{"title", `title: "Test Insight"`},
		{"status", "status: raw"},
		{"type", "type: link"},
		{"source", "source: url"},
		{"url", `url: "https://example.com/article"`},
		{"source_id", `source_id: "https://example.com/article"`},
		{"memory_sector", "memory_sector: semantic"},
		{"created", "created:"},
		{"tag test", "  - test"},
		{"tag integration", "  - integration"},
	}
	for _, c := range checks {
		if !strings.Contains(content, c.needle) {
			t.Errorf("missing %s: expected %q in output", c.label, c.needle)
		}
	}

	// Verify body content is present.
	if !strings.Contains(content, "This is a test CogDoc with meaningful content.") {
		t.Error("body content missing from written CogDoc")
	}

	// Verify the file has proper frontmatter fences.
	if !strings.HasPrefix(content, "---\n") {
		t.Error("CogDoc should start with YAML frontmatter fence")
	}
	// Second fence should appear (closing frontmatter).
	if strings.Count(content, "---\n") < 2 {
		t.Error("CogDoc should have opening and closing frontmatter fences")
	}
}

// ── Slugify ──────────────────────────────────────────────────────────────────

func TestSlugify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"Hello World", "hello-world"},
		{"", ""},
		{"already-a-slug", "already-a-slug"},
		{"Special!@#Characters$%^&*()", "special-characters"},
		{"  leading and trailing spaces  ", "leading-and-trailing-spaces"},
	}

	for _, tt := range tests {
		got := slugify(tt.input)
		if got != tt.want {
			t.Errorf("slugify(%q) = %q; want %q", tt.input, got, tt.want)
		}
	}

	// Test truncation: long input should be <= 50 chars.
	long := slugify("GitHub - anthropics/claude-code: An agentic coding tool that lives in your terminal")
	if len(long) > 50 {
		t.Errorf("slugify long string: len=%d (>50), got %q", len(long), long)
	}
	// Should still start with github.
	if !strings.HasPrefix(long, "github") {
		t.Errorf("slugify long string should start with 'github', got %q", long)
	}
}

// ── Domain classification ────────────────────────────────────────────────────

func TestClassifyDomain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		domain string
		want   ContentType
	}{
		{"github.com", ContentRepo},
		{"www.github.com", ContentRepo},
		{"arxiv.org", ContentPaper},
		{"www.arxiv.org", ContentPaper},
		{"scholar.google.com", ContentPaper},
		{"youtube.com", ContentVideo},
		{"www.youtube.com", ContentVideo},
		{"youtu.be", ContentVideo},
		{"reddit.com", ContentDiscussion},
		{"old.reddit.com", ContentDiscussion},
		{"news.ycombinator.com", ContentDiscussion},
		{"example.com", ContentArticle},
		{"blog.example.org", ContentArticle},
	}

	for _, tt := range tests {
		got := classifyDomain(tt.domain)
		if got != tt.want {
			t.Errorf("classifyDomain(%q) = %q; want %q", tt.domain, got, tt.want)
		}
	}
}

// ── Ledger event emission ────────────────────────────────────────────────────

func TestEmitIngestEvent(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	cfg := &Config{
		WorkspaceRoot: tmp,
		CogDir:        filepath.Join(tmp, ".cog"),
	}

	result := &IngestResult{
		Title:       "Test Article",
		URL:         "https://example.com/test",
		ContentType: ContentArticle,
		Source:      SourceURL,
		SourceID:    "https://example.com/test",
	}

	cogdocPath := "semantic/inbox/links/test-article.cog.md"
	err := EmitIngestEvent(cfg, result, cogdocPath)
	if err != nil {
		t.Fatalf("EmitIngestEvent: %v", err)
	}

	// Read the ledger file and verify the event.
	ledgerPath := filepath.Join(tmp, ".cog", "ledger", "events.jsonl")
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("ReadFile ledger: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 ledger line, got %d", len(lines))
	}

	var event map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("unmarshal ledger event: %v", err)
	}

	if event["type"] != IngestEventType {
		t.Errorf("event type = %v; want %q", event["type"], IngestEventType)
	}
	if event["title"] != "Test Article" {
		t.Errorf("event title = %v; want %q", event["title"], "Test Article")
	}
	if event["cogdoc_path"] != cogdocPath {
		t.Errorf("event cogdoc_path = %v; want %q", event["cogdoc_path"], cogdocPath)
	}
	if event["cogdoc_uri"] != "cog:mem/"+cogdocPath {
		t.Errorf("event cogdoc_uri = %v; want %q", event["cogdoc_uri"], "cog:mem/"+cogdocPath)
	}
	if _, ok := event["timestamp"]; !ok {
		t.Error("event missing timestamp")
	}
}

// ── Pipeline dedup integration ───────────────────────────────────────────────

func TestIngestPipelineDedupIntegration(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	inboxDir := filepath.Join(tmp, ".cog", "mem", "semantic", "inbox", "links")
	if err := os.MkdirAll(inboxDir, 0755); err != nil {
		t.Fatalf("mkdir inbox: %v", err)
	}

	// Pre-populate a CogDoc that should be detected as duplicate.
	existing := `---
title: "Existing Link"
source_id: "https://example.com/already-ingested"
status: raw
---

# Existing Link
`
	if err := os.WriteFile(
		filepath.Join(inboxDir, "existing.cog.md"),
		[]byte(existing), 0644,
	); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	pipeline := NewIngestPipeline(tmp)

	// The dedup check uses quoted source_id values from WriteCogDoc.
	if !pipeline.CheckDuplicate(`"https://example.com/already-ingested"`) {
		t.Error("CheckDuplicate should return true for existing source_id")
	}
	if pipeline.CheckDuplicate("https://example.com/new-thing") {
		t.Error("CheckDuplicate should return false for unknown source_id")
	}
}

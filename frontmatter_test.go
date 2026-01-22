// .cog/frontmatter_test.go
// Tests for frontmatter management

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// === SECTOR DETECTION TESTS ===

func TestDetectSector(t *testing.T) {
	tests := []struct {
		path     string
		expected MemorySector
	}{
		{".cog/mem/semantic/arch.md", SectorSemantic},
		{".cog/mem/episodic/session.md", SectorEpisodic},
		{".cog/mem/procedural/guide.md", SectorProcedural},
		{".cog/mem/reflective/insight.md", SectorReflective},
		{"/some/semantic/path/doc.md", SectorSemantic},
		{"/random/path.md", SectorSemantic}, // default
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := DetectSector(tt.path)
			if result != tt.expected {
				t.Errorf("DetectSector(%q) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

func TestGetSectorSchema(t *testing.T) {
	tests := []struct {
		sector       MemorySector
		wantDecay    float64
		wantSalience string
	}{
		{SectorSemantic, 0.08, "medium"},
		{SectorEpisodic, 0.20, "medium"},
		{SectorProcedural, 0.12, "medium"},
		{SectorReflective, 0.05, "high"},
	}

	for _, tt := range tests {
		t.Run(string(tt.sector), func(t *testing.T) {
			schema := GetSectorSchema(tt.sector)
			if schema.DecayRate != tt.wantDecay {
				t.Errorf("DecayRate = %v, want %v", schema.DecayRate, tt.wantDecay)
			}
			if schema.Salience != tt.wantSalience {
				t.Errorf("Salience = %v, want %v", schema.Salience, tt.wantSalience)
			}
		})
	}
}

// === FRONTMATTER EXTRACTION TESTS ===

func TestHasFrontmatter(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected bool
	}{
		{"valid frontmatter", "---\ntitle: Test\n---\n", true},
		{"no frontmatter", "# Heading\n", false},
		{"wrong delimiter", "***\ntitle: Test\n***\n", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := HasFrontmatter(tt.content)
			if result != tt.expected {
				t.Errorf("HasFrontmatter() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestExtractFrontmatter(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantFM      string
		wantBody    string
		wantErr     bool
		errContains string
	}{
		{
			name:     "valid frontmatter",
			content:  "---\ntitle: Test\n---\nBody content",
			wantFM:   "title: Test",
			wantBody: "Body content",
			wantErr:  false,
		},
		{
			name:        "no frontmatter",
			content:     "# Heading\n",
			wantErr:     true,
			errContains: "no frontmatter",
		},
		{
			name:        "unclosed frontmatter",
			content:     "---\ntitle: Test\n",
			wantErr:     true,
			errContains: "unclosed frontmatter",
		},
		{
			name:     "empty body",
			content:  "---\ntitle: Test\n---\n",
			wantFM:   "title: Test",
			wantBody: "",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := ExtractFrontmatter(tt.content)

			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error containing %q, got nil", tt.errContains)
					return
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Error = %q, want error containing %q", err.Error(), tt.errContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if doc.Frontmatter != tt.wantFM {
				t.Errorf("Frontmatter = %q, want %q", doc.Frontmatter, tt.wantFM)
			}
			if doc.Body != tt.wantBody {
				t.Errorf("Body = %q, want %q", doc.Body, tt.wantBody)
			}
		})
	}
}

func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name        string
		frontmatter string
		wantErr     bool
		checkField  string
		checkValue  interface{}
	}{
		{
			name:        "valid YAML",
			frontmatter: "title: Test\ncreated: 2026-01-16\n",
			wantErr:     false,
			checkField:  "title",
			checkValue:  "Test",
		},
		{
			name:        "invalid YAML",
			frontmatter: "title: [unclosed\n",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := ParseFrontmatter(tt.frontmatter)

			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if tt.checkField != "" {
				if val, ok := data[tt.checkField]; !ok {
					t.Errorf("Field %q not found", tt.checkField)
				} else if val != tt.checkValue {
					t.Errorf("%s = %v, want %v", tt.checkField, val, tt.checkValue)
				}
			}
		})
	}
}

// === TITLE EXTRACTION TESTS ===

func TestExtractTitleFromFrontmatter(t *testing.T) {
	tests := []struct {
		name        string
		frontmatter string
		expected    string
	}{
		{"simple title", "title: Test Document\n", "Test Document"},
		{"quoted title", "title: \"Test Document\"\n", "Test Document"},
		{"single quoted", "title: 'Test Document'\n", "Test Document"},
		{"no title field", "created: 2026-01-16\n", ""},
		{"invalid YAML", "title: [unclosed\n", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractTitleFromFrontmatter(tt.frontmatter)
			if result != tt.expected {
				t.Errorf("ExtractTitleFromFrontmatter() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestExtractTitleFromMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{"h1 heading", "# Test Document\n## Subheading\n", "Test Document"},
		{"no heading", "Just text\n", ""},
		{"h2 only", "## Subheading\n", ""},
		{"multiple h1", "# First\n# Second\n", "First"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractTitleFromMarkdown(tt.content)
			if result != tt.expected {
				t.Errorf("ExtractTitleFromMarkdown() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestExtractTitleFromFilename(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"/path/to/my-document.md", "my document"},
		{"/path/to/my_document.cog.md", "my document"},
		{"/path/to/test.md", "test"},
		{"/path/to/multi-word-title.md", "multi word title"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := ExtractTitleFromFilename(tt.path)
			if result != tt.expected {
				t.Errorf("ExtractTitleFromFilename(%q) = %q, want %q", tt.path, result, tt.expected)
			}
		})
	}
}

// === FRONTMATTER GENERATION TESTS ===

func TestGenerateFrontmatter(t *testing.T) {
	path := ".cog/mem/semantic/test.md"
	title := "Test Document"

	result := GenerateFrontmatter(path, title)

	if !strings.HasPrefix(result, "---\n") {
		t.Errorf("Result doesn't start with ---")
	}
	if !strings.HasSuffix(result, "---\n") {
		t.Errorf("Result doesn't end with ---")
	}
	if !strings.Contains(result, "title: "+title) {
		t.Errorf("Result doesn't contain title")
	}
	if !strings.Contains(result, "memory_sector: semantic") {
		t.Errorf("Result doesn't contain correct sector")
	}
	if !strings.Contains(result, "decay_rate: 0.08") {
		t.Errorf("Result doesn't contain correct decay rate for semantic")
	}
}

func TestGenerateMinimalFrontmatter(t *testing.T) {
	path := ".cog/mem/episodic/test.md"
	title := "Test Document"

	result := GenerateMinimalFrontmatter(path, title)

	if !strings.Contains(result, "title: "+title) {
		t.Errorf("Result doesn't contain title")
	}
	if !strings.Contains(result, "memory_sector: episodic") {
		t.Errorf("Result doesn't contain correct sector")
	}
	// Should NOT contain optional fields
	if strings.Contains(result, "decay_rate") {
		t.Errorf("Minimal frontmatter should not contain decay_rate")
	}
}

// === VALIDATION TESTS ===

func TestIsCogdoc(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name     string
		content  string
		filename string
		expected bool
	}{
		{
			name:     "valid cogdoc with memory_sector",
			content:  "---\ntitle: Test\nmemory_sector: semantic\n---\nBody",
			filename: "valid.md",
			expected: true,
		},
		{
			name:     "valid cogdoc with cogn8",
			content:  "---\ntitle: Test\ncogn8:\n  version: 1.0\n---\nBody",
			filename: "cogn8.md",
			expected: true,
		},
		{
			name:     "no frontmatter",
			content:  "# Heading\nBody",
			filename: "no-fm.md",
			expected: false,
		},
		{
			name:     "frontmatter without required fields",
			content:  "---\ntitle: Test\n---\nBody",
			filename: "incomplete.md",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := filepath.Join(tmpDir, tt.filename)
			if err := os.WriteFile(filePath, []byte(tt.content), 0644); err != nil {
				t.Fatalf("Failed to create test file: %v", err)
			}

			result, err := IsCogdoc(filePath)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if result != tt.expected {
				t.Errorf("IsCogdoc() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// === FILE OPERATIONS TESTS ===

func TestApplyFrontmatter(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name          string
		initialContent string
		force         bool
		shouldModify  bool
	}{
		{
			name:          "add frontmatter to file without",
			initialContent: "# Test Document\nBody content",
			force:         false,
			shouldModify:  true,
		},
		{
			name:          "skip complete frontmatter",
			initialContent: "---\ntitle: Test\nmemory_sector: semantic\n---\nBody",
			force:         false,
			shouldModify:  false,
		},
		{
			name:          "force fix complete frontmatter",
			initialContent: "---\ntitle: Test\nmemory_sector: semantic\n---\nBody",
			force:         true,
			shouldModify:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := filepath.Join(tmpDir, "test.md")
			if err := os.WriteFile(filePath, []byte(tt.initialContent), 0644); err != nil {
				t.Fatalf("Failed to create test file: %v", err)
			}

			err := ApplyFrontmatter(filePath, tt.force)
			if err != nil {
				t.Fatalf("ApplyFrontmatter failed: %v", err)
			}

			newContent, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatalf("Failed to read file: %v", err)
			}

			modified := string(newContent) != tt.initialContent
			if modified != tt.shouldModify {
				t.Errorf("File modification = %v, want %v", modified, tt.shouldModify)
			}

			// Should always have frontmatter after
			if !HasFrontmatter(string(newContent)) {
				t.Errorf("File should have frontmatter after ApplyFrontmatter")
			}
		})
	}
}

func TestCogifyFile(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name         string
		filename     string
		content      string
		shouldRename bool
	}{
		{
			name:         "rename valid cogdoc",
			filename:     "test.md",
			content:      "---\ntitle: Test\nmemory_sector: semantic\n---\nBody",
			shouldRename: true,
		},
		{
			name:         "skip already renamed",
			filename:     "test.cog.md",
			content:      "---\ntitle: Test\nmemory_sector: semantic\n---\nBody",
			shouldRename: false,
		},
		{
			name:         "skip non-cogdoc",
			filename:     "regular.md",
			content:      "# Just a regular markdown file",
			shouldRename: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := filepath.Join(tmpDir, tt.filename)
			if err := os.WriteFile(filePath, []byte(tt.content), 0644); err != nil {
				t.Fatalf("Failed to create test file: %v", err)
			}

			newPath, err := CogifyFile(filePath)
			if err != nil {
				t.Fatalf("CogifyFile failed: %v", err)
			}

			renamed := newPath != filePath
			if renamed != tt.shouldRename {
				t.Errorf("File renamed = %v, want %v (got %s)", renamed, tt.shouldRename, newPath)
			}

			// Check file exists at the returned path
			if _, err := os.Stat(newPath); os.IsNotExist(err) {
				t.Errorf("File doesn't exist at returned path: %s", newPath)
			}
		})
	}
}

// === BENCHMARK TESTS ===

func BenchmarkExtractFrontmatter(b *testing.B) {
	content := "---\ntitle: Test Document\ncreated: 2026-01-16\nmemory_sector: semantic\n---\nBody content here"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ExtractFrontmatter(content)
	}
}

func BenchmarkParseFrontmatter(b *testing.B) {
	frontmatter := "title: Test Document\ncreated: 2026-01-16\nmemory_sector: semantic\ntags:\n  - test\n  - benchmark\n"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ParseFrontmatter(frontmatter)
	}
}

func BenchmarkGenerateFrontmatter(b *testing.B) {
	path := ".cog/mem/semantic/test.md"
	title := "Test Document"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = GenerateFrontmatter(path, title)
	}
}

func BenchmarkDetectSector(b *testing.B) {
	path := ".cog/mem/episodic/sessions/test.md"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = DetectSector(path)
	}
}

func BenchmarkExtractTitle(b *testing.B) {
	tmpDir := b.TempDir()
	filePath := filepath.Join(tmpDir, "test.md")
	content := "---\ntitle: Test Document\n---\n# Heading\nBody"
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		b.Fatalf("Failed to create test file: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ExtractTitle(filePath)
	}
}

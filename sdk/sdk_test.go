package sdk

import (
	"testing"
)

func TestConstants(t *testing.T) {
	src := Constants()

	// Test Ln2
	if src.Ln2 < 0.693 || src.Ln2 > 0.694 {
		t.Errorf("Ln2 = %f, want ~0.693", src.Ln2)
	}

	// Test Tau1 == Ln2
	if src.Tau1 != src.Ln2 {
		t.Errorf("Tau1 = %f, want %f (= Ln2)", src.Tau1, src.Ln2)
	}

	// Test Tau2 == 2*Ln2
	if src.Tau2 < 1.386 || src.Tau2 > 1.387 {
		t.Errorf("Tau2 = %f, want ~1.386", src.Tau2)
	}

	// Test GEff = 1/3
	if src.GEff < 0.333 || src.GEff > 0.334 {
		t.Errorf("GEff = %f, want ~0.333", src.GEff)
	}

	// Test VarianceRatio = 6 (exact)
	if src.VarianceRatio != 6 {
		t.Errorf("VarianceRatio = %d, want 6", src.VarianceRatio)
	}

	// Test CorrelationThreshold = sqrt(2/3)
	if src.CorrelationThreshold < 0.816 || src.CorrelationThreshold > 0.817 {
		t.Errorf("CorrelationThreshold = %f, want ~0.816", src.CorrelationThreshold)
	}
}

func TestMessageWeight(t *testing.T) {
	// At depth 0, weight should be 1.0
	w0 := MessageWeight(0)
	if w0 < 0.999 || w0 > 1.001 {
		t.Errorf("MessageWeight(0) = %f, want 1.0", w0)
	}

	// At depth 1, weight should be 0.5
	w1 := MessageWeight(1)
	if w1 < 0.499 || w1 > 0.501 {
		t.Errorf("MessageWeight(1) = %f, want 0.5", w1)
	}

	// At depth 2, weight should be 0.25
	w2 := MessageWeight(2)
	if w2 < 0.249 || w2 > 0.251 {
		t.Errorf("MessageWeight(2) = %f, want 0.25", w2)
	}
}

func TestParseURI(t *testing.T) {
	tests := []struct {
		uri       string
		namespace string
		path      string
		wantErr   bool
	}{
		{"cog://mem", "mem", "", false},
		{"cog://mem/semantic", "mem", "semantic", false},
		{"cog://mem/semantic/insights/test", "mem", "semantic/insights/test", false},
		{"cog://coherence", "coherence", "", false},
		{"cog://src", "src", "", false},
		{"cog://identity", "identity", "", false},
		{"cog://signals/inference", "signals", "inference", false},
		{"", "", "", true},                  // Empty
		{"http://example.com", "", "", true}, // Wrong scheme
		{"cog://unknown", "", "", true},      // Unknown namespace
	}

	for _, tt := range tests {
		parsed, err := ParseURI(tt.uri)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseURI(%q) = nil, want error", tt.uri)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseURI(%q) error: %v", tt.uri, err)
			continue
		}
		if parsed.Namespace != tt.namespace {
			t.Errorf("ParseURI(%q).Namespace = %q, want %q", tt.uri, parsed.Namespace, tt.namespace)
		}
		if parsed.Path != tt.path {
			t.Errorf("ParseURI(%q).Path = %q, want %q", tt.uri, parsed.Path, tt.path)
		}
	}
}

func TestParseURIWithQuery(t *testing.T) {
	parsed, err := ParseURI("cog://mem/semantic?q=topic&limit=10")
	if err != nil {
		t.Fatalf("ParseURI error: %v", err)
	}

	if parsed.GetQuery("q") != "topic" {
		t.Errorf("GetQuery(q) = %q, want %q", parsed.GetQuery("q"), "topic")
	}

	if parsed.GetQueryInt("limit", 0) != 10 {
		t.Errorf("GetQueryInt(limit) = %d, want 10", parsed.GetQueryInt("limit", 0))
	}

	if parsed.GetQueryInt("missing", 5) != 5 {
		t.Errorf("GetQueryInt(missing, 5) = %d, want 5", parsed.GetQueryInt("missing", 5))
	}
}

func TestNewResource(t *testing.T) {
	r := NewResource("cog://test", []byte("hello"))

	if r.URI != "cog://test" {
		t.Errorf("URI = %q, want %q", r.URI, "cog://test")
	}

	if string(r.Content) != "hello" {
		t.Errorf("Content = %q, want %q", string(r.Content), "hello")
	}

	if r.ContentType != ContentTypeRaw {
		t.Errorf("ContentType = %q, want %q", r.ContentType, ContentTypeRaw)
	}
}

func TestResourceMetadata(t *testing.T) {
	r := NewResource("cog://test", nil)
	r.SetMetadata("key", "value")

	v, ok := r.GetMetadata("key")
	if !ok {
		t.Error("GetMetadata(key) not found")
	}
	if v != "value" {
		t.Errorf("GetMetadata(key) = %v, want %q", v, "value")
	}

	if r.GetMetadataString("key") != "value" {
		t.Errorf("GetMetadataString(key) = %q, want %q", r.GetMetadataString("key"), "value")
	}

	if r.GetMetadataString("missing") != "" {
		t.Errorf("GetMetadataString(missing) = %q, want empty", r.GetMetadataString("missing"))
	}
}

func TestMutation(t *testing.T) {
	set := NewSetMutation([]byte("content"))
	if !set.IsSet() {
		t.Error("NewSetMutation should be Set")
	}

	patch := NewPatchMutation([]byte("{}"))
	if !patch.IsPatch() {
		t.Error("NewPatchMutation should be Patch")
	}

	del := NewDeleteMutation()
	if !del.IsDelete() {
		t.Error("NewDeleteMutation should be Delete")
	}

	app := NewAppendMutation([]byte("item"))
	if !app.IsAppend() {
		t.Error("NewAppendMutation should be Append")
	}
}

func TestVersion(t *testing.T) {
	if Version == "" {
		t.Error("Version should not be empty")
	}
}

// --- Tests for new SDK features ---

func TestExtendedNamespaces(t *testing.T) {
	// Test new namespaces are recognized
	extendedNamespaces := []string{
		"spec", "specs", "status", "canonical", "handoff", "handoffs",
		"crystal", "role", "roles", "skill", "skills", "agent", "agents",
	}

	for _, ns := range extendedNamespaces {
		uri := "cog://" + ns
		parsed, err := ParseURI(uri)
		if err != nil {
			t.Errorf("ParseURI(%q) error: %v", uri, err)
			continue
		}
		if parsed.Namespace != ns {
			t.Errorf("ParseURI(%q).Namespace = %q, want %q", uri, parsed.Namespace, ns)
		}
	}
}

func TestCogdocTypeValidation(t *testing.T) {
	tests := []struct {
		typeName string
		valid    bool
	}{
		// Core types
		{"identity", true},
		{"ontology", true},
		{"mem", true},
		{"schema", true},
		{"decision", true},
		{"session", true},
		{"handoff", true},
		{"guide", true},
		{"adr", true},
		{"knowledge", true},
		// Extended types
		{"note", true},
		{"term", true},
		{"spec", true},
		{"claim", true},
		{"insight", true},
		{"architecture", true},
		{"research_synthesis", true},
		{"observation", true},
		{"assessment", true},
		{"procedural", true},
		{"summary", true},
		{"specification", true},
		// Invalid types
		{"invalid", false},
		{"unknown", false},
		{"", false},
	}

	for _, tt := range tests {
		got := ValidateType(tt.typeName)
		if got != tt.valid {
			t.Errorf("ValidateType(%q) = %v, want %v", tt.typeName, got, tt.valid)
		}
	}
}

func TestIsKebabCase(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"hello-world", true},
		{"hello", true},
		{"hello-world-test", true},
		{"test123", true},
		{"test-123", true},
		{"123-test", true},
		{"HelloWorld", false},
		{"hello_world", false},
		{"hello world", false},
		{"Hello-World", false},
		{"", false},
	}

	for _, tt := range tests {
		got := IsKebabCase(tt.input)
		if got != tt.valid {
			t.Errorf("IsKebabCase(%q) = %v, want %v", tt.input, got, tt.valid)
		}
	}
}

func TestIsValidDate(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"2025-01-10", true},
		{"2024-12-31", true},
		{"2000-06-15", true},
		{"25-01-10", false},
		{"2025/01/10", false},
		{"2025-1-10", false},
		{"2025-01-1", false},
		{"not-a-date", false},
		{"", false},
	}

	for _, tt := range tests {
		got := IsValidDate(tt.input)
		if got != tt.valid {
			t.Errorf("IsValidDate(%q) = %v, want %v", tt.input, got, tt.valid)
		}
	}
}

func TestTypedRefRelations(t *testing.T) {
	validRels := []string{"refs", "implements", "extends", "supersedes", "describes", "requires", "suggests"}

	for _, rel := range validRels {
		if !ValidRefRelations[rel] {
			t.Errorf("ValidRefRelations[%q] = false, want true", rel)
		}
	}

	invalidRels := []string{"invalid", "unknown", "references"}
	for _, rel := range invalidRels {
		if ValidRefRelations[rel] {
			t.Errorf("ValidRefRelations[%q] = true, want false", rel)
		}
	}
}

func TestParseFrontmatter(t *testing.T) {
	content := `---
type: decision
id: test-decision
title: Test Decision
created: 2025-01-10
status: active
tags:
  - test
  - example
---

# Test Decision

This is the body.
`

	fm, body, err := ParseFrontmatter(content)
	if err != nil {
		t.Fatalf("ParseFrontmatter error: %v", err)
	}

	if fm.Type != "decision" {
		t.Errorf("Type = %q, want %q", fm.Type, "decision")
	}
	if fm.ID != "test-decision" {
		t.Errorf("ID = %q, want %q", fm.ID, "test-decision")
	}
	if fm.Title != "Test Decision" {
		t.Errorf("Title = %q, want %q", fm.Title, "Test Decision")
	}
	if fm.Created != "2025-01-10" {
		t.Errorf("Created = %q, want %q", fm.Created, "2025-01-10")
	}
	if fm.Status != "active" {
		t.Errorf("Status = %q, want %q", fm.Status, "active")
	}
	if len(fm.Tags) != 2 {
		t.Errorf("len(Tags) = %d, want 2", len(fm.Tags))
	}
	if !containsString(body, "# Test Decision") {
		t.Errorf("Body should contain '# Test Decision'")
	}
}

func TestParseFrontmatterErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"no frontmatter", "# Just markdown\n"},
		{"unclosed frontmatter", "---\ntype: test\n# No closing ---\n"},
	}

	for _, tt := range tests {
		_, _, err := ParseFrontmatter(tt.content)
		if err == nil {
			t.Errorf("ParseFrontmatter(%s) should return error", tt.name)
		}
	}
}

func containsString(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(haystack == needle ||
		 len(haystack) > len(needle) &&
		 (haystack[:len(needle)] == needle ||
		  containsSubstring(haystack, needle)))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

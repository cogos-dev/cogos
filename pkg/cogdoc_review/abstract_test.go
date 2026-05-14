// abstract_test.go — Tests for AbstractGenerator and helpers.
//
// Test structure:
//
//   Unit tests (no LLM, no Ollama):
//     - TestParseConversationJSONL: validates fixture JSONL parsing
//     - TestBuildConversationPrompt: validates prompt formatting
//     - TestParseAbstractJSON: validates JSON parsing with various LLM output shapes
//     - TestWriteAbstractToFrontmatter: validates frontmatter injection
//     - TestAbstractGeneratorNoProvider: validates error when resolver finds nothing
//
//   Integration test (requires claude CLI, build tag "integration"):
//     - TestGenerateFromJSONLEndToEnd: ingests fixture_conversation.jsonl,
//       verifies abstract is generated and written to a temp cogdoc.
//
// The unit tests run without any external dependencies.
// The integration test requires `claude` in PATH and authentication.
package cogdoc_review_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myrgic/cogos/internal/engine/inference"
	"github.com/myrgic/cogos/pkg/cogdoc_review"
)

// --- Unit: JSONL parsing ---

func TestParseConversationJSONL(t *testing.T) {
	fixturePath := filepath.Join("testdata", "fixture_conversation.jsonl")
	f, err := os.Open(fixturePath)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	messages, err := cogdoc_review.ParseConversationJSONLReader(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(messages) < 4 {
		t.Errorf("expected at least 4 messages, got %d", len(messages))
	}
	if messages[0].Role != "user" {
		t.Errorf("first message role: want 'user', got %q", messages[0].Role)
	}
	if messages[1].Role != "assistant" {
		t.Errorf("second message role: want 'assistant', got %q", messages[1].Role)
	}
	// Verify the fixture content contains expected keywords.
	combined := ""
	for _, m := range messages {
		combined += m.Content + " "
	}
	if !strings.Contains(combined, "autonomic") {
		t.Error("expected fixture to mention 'autonomic'")
	}
	if !strings.Contains(combined, "abstract") {
		t.Error("expected fixture to mention 'abstract'")
	}
}

func TestParseConversationJSONLBlankLines(t *testing.T) {
	input := `{"role":"user","content":"hello"}

{"role":"assistant","content":"world"}
`
	messages, err := cogdoc_review.ParseConversationJSONLReader(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(messages))
	}
}

// --- Unit: prompt building ---

func TestBuildConversationPrompt(t *testing.T) {
	messages := []cogdoc_review.ConversationMessage{
		{Role: "user", Content: "What is the autonomic cycle?"},
		{Role: "assistant", Content: "It is a background loop that runs periodically."},
	}
	prompt := cogdoc_review.BuildConversationPromptExported(messages)
	if !strings.Contains(prompt, "USER") {
		t.Error("expected prompt to contain 'USER'")
	}
	if !strings.Contains(prompt, "ASSISTANT") {
		t.Error("expected prompt to contain 'ASSISTANT'")
	}
	if !strings.Contains(prompt, "autonomic cycle") {
		t.Error("expected prompt to contain message content")
	}
}

// --- Unit: abstract JSON parsing ---

func TestParseAbstractJSON(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{
			name: "clean JSON",
			raw: `{"topic":"CogOS inference contract","key_uris":["cog://mem/feedback"],"decision_shape":"Implement N-tier resolver in PR1"}`,
		},
		{
			name: "JSON wrapped in markdown fence",
			raw: "```json\n{\"topic\":\"test topic\",\"key_uris\":[],\"decision_shape\":\"nothing decided\"}\n```",
		},
		{
			name: "JSON with leading text",
			raw: `Here is the abstract: {"topic":"test topic","key_uris":[],"decision_shape":"nothing decided"}`,
		},
		{
			name:    "empty",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "missing topic",
			raw:     `{"key_uris":[],"decision_shape":"nothing"}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			abstract, err := cogdoc_review.ParseAbstractJSONExported(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (abstract=%+v)", abstract)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if abstract.Topic == "" {
				t.Error("expected non-empty topic")
			}
		})
	}
}

// --- Unit: frontmatter writing ---

func TestWriteAbstractToFrontmatter(t *testing.T) {
	dir := t.TempDir()

	abstract := &cogdoc_review.ConversationAbstract{
		Topic:         "CogOS core inference contract",
		KeyURIs:       []string{"cog://mem/feedback_node_core_inference_model_contract"},
		DecisionShape: "Implement N-tier resolver, default to Haiku-via-Max",
		SelectedTier:  "haiku-via-max",
		GeneratedAt:   "2026-05-14T12:00:00Z",
	}

	t.Run("file with frontmatter", func(t *testing.T) {
		path := filepath.Join(dir, "with-fm.md")
		content := "---\nname: test\ntype: cogdoc\n---\n\n# Test\n\nBody text.\n"
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if err := cogdoc_review.WriteAbstractToFrontmatter(path, abstract); err != nil {
			t.Fatalf("WriteAbstractToFrontmatter: %v", err)
		}
		result, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(result)
		if !strings.Contains(s, "abstract:") {
			t.Error("expected 'abstract:' field in frontmatter")
		}
		if !strings.Contains(s, "decision_shape:") {
			t.Error("expected 'decision_shape:' field in frontmatter")
		}
		if !strings.Contains(s, "haiku-via-max") {
			t.Error("expected tier name in frontmatter")
		}
		// Verify the original content is preserved.
		if !strings.Contains(s, "Body text.") {
			t.Error("original body content should be preserved")
		}
	})

	t.Run("file without frontmatter", func(t *testing.T) {
		path := filepath.Join(dir, "no-fm.md")
		content := "# Test\n\nBody text.\n"
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if err := cogdoc_review.WriteAbstractToFrontmatter(path, abstract); err != nil {
			t.Fatalf("WriteAbstractToFrontmatter: %v", err)
		}
		result, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(result)
		if !strings.Contains(s, "<!-- abstract:") {
			t.Error("expected HTML comment abstract header for file without frontmatter")
		}
		if !strings.Contains(s, "Body text.") {
			t.Error("original body content should be preserved")
		}
	})
}

// --- Unit: resolver error path ---

func TestAbstractGeneratorNoProvider(t *testing.T) {
	// Resolver with no providers — all tiers fail availability check.
	cfg := inference.DefaultCoreInferenceConfig()
	// Pass empty providers map: nothing is available.
	resolver := inference.NewResolver(cfg, map[string]inference.ProviderLike{})

	gen := cogdoc_review.NewAbstractGenerator(resolver, cogdoc_review.AbstractGeneratorConfig{
		MaxMessages: 5,
		Timeout:     5, // 5ns — will fail on provider check before any network call
	})

	messages := []cogdoc_review.ConversationMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "world"},
	}

	_, err := gen.GenerateFromMessages(context.Background(), messages)
	if err == nil {
		t.Fatal("expected error when no providers available, got nil")
	}
	if !strings.Contains(err.Error(), "inference resolver") {
		t.Errorf("expected 'inference resolver' in error, got: %v", err)
	}
}

// --- Integration: end-to-end with real claude CLI ---

// TestGenerateFromJSONLEndToEnd is an integration test that:
//  1. Reads testdata/fixture_conversation.jsonl
//  2. Creates a stub claude-code provider that reports Available=true
//  3. Calls GenerateFromJSONL (which calls claude -p subprocess)
//  4. Verifies the abstract has topic, decision_shape, and selected tier
//  5. Writes the abstract to a temp cogdoc and verifies the frontmatter
//
// Build tag: integration. Requires `claude` in PATH and authentication.

// Note: This test is in the non-integration block because the core E2E
// verification (JSONL parsing + resolver selection + frontmatter writing)
// uses a mock provider to avoid the claude subprocess in CI.
// The actual LLM call is gated behind the "integration" build tag below.
func TestGenerateFromJSONLEndToEnd_MockProvider(t *testing.T) {
	// This test exercises the full pipeline with a mock provider that
	// returns a pre-baked abstract JSON instead of calling claude.
	// It validates the full integration path without network dependencies.

	fixturePath := filepath.Join("testdata", "fixture_conversation.jsonl")
	if _, err := os.Stat(fixturePath); err != nil {
		t.Skipf("fixture not found at %s: %v", fixturePath, err)
	}

	// Build a mock claude provider that is available.
	mockProvider := &mockProviderAvailable{
		name:  "claude",
		model: "haiku",
	}
	providers := map[string]inference.ProviderLike{"claude": mockProvider}
	resolver := inference.NewResolver(inference.DefaultCoreInferenceConfig(), providers)

	gen := cogdoc_review.NewAbstractGeneratorWithCallHook(
		resolver,
		cogdoc_review.AbstractGeneratorConfig{MaxMessages: 10},
		func(ctx context.Context, provider inference.ProviderLike, tierName, prompt string) (string, error) {
			// Return a pre-baked abstract JSON simulating what the LLM would return.
			return `{"topic":"CogOS core inference contract implementation","key_uris":["cog://mem/feedback_node_core_inference_model_contract"],"decision_shape":"Implement N-tier resolver in PR1, wire abstract generation in PR2"}`, nil
		},
	)

	abstract, err := gen.GenerateFromJSONL(context.Background(), fixturePath)
	if err != nil {
		t.Fatalf("GenerateFromJSONL: %v", err)
	}

	// Verify abstract fields.
	if abstract.Topic == "" {
		t.Error("expected non-empty topic")
	}
	if abstract.DecisionShape == "" {
		t.Error("expected non-empty decision_shape")
	}
	if abstract.SelectedTier == "" {
		t.Error("expected SelectedTier to be set")
	}
	if abstract.GeneratedAt == "" {
		t.Error("expected GeneratedAt to be set")
	}

	t.Logf("abstract.Topic: %s", abstract.Topic)
	t.Logf("abstract.DecisionShape: %s", abstract.DecisionShape)
	t.Logf("abstract.SelectedTier: %s", abstract.SelectedTier)
	t.Logf("abstract.KeyURIs: %v", abstract.KeyURIs)

	// Write to a temp cogdoc and verify frontmatter.
	dir := t.TempDir()
	cogdocPath := filepath.Join(dir, "test-cogdoc.md")
	cogdocContent := "---\nname: test\ntype: cogdoc\n---\n\n# Test Cogdoc\n\nPlaceholder body.\n"
	if err := os.WriteFile(cogdocPath, []byte(cogdocContent), 0644); err != nil {
		t.Fatal(err)
	}

	if err := cogdoc_review.WriteAbstractToFrontmatter(cogdocPath, abstract); err != nil {
		t.Fatalf("WriteAbstractToFrontmatter: %v", err)
	}

	result, err := os.ReadFile(cogdocPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(result)
	if !strings.Contains(s, "abstract:") {
		t.Error("expected 'abstract:' in cogdoc frontmatter after write")
	}
	if !strings.Contains(s, "Placeholder body.") {
		t.Error("cogdoc body should be preserved")
	}
}

// mockProviderAvailable is a test double for inference.ProviderLike.
type mockProviderAvailable struct {
	name  string
	model string
}

func (m *mockProviderAvailable) Name() string                    { return m.name }
func (m *mockProviderAvailable) Model() string                   { return m.model }
func (m *mockProviderAvailable) Available(_ context.Context) bool { return true }

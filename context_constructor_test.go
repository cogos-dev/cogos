package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helper to make a ChatMessage with string content
func makeMsg(role, content string) ChatMessage {
	raw, _ := json.Marshal(content)
	return ChatMessage{Role: role, Content: raw}
}

// makeTempWorkspace creates a minimal workspace for testing.
// Returns the workspace root path; caller should defer os.RemoveAll.
func makeTempWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// Create identity config
	identDir := filepath.Join(root, ".cog", "config")
	os.MkdirAll(identDir, 0o755)
	os.WriteFile(filepath.Join(identDir, "identity.yaml"), []byte(`
default_identity: test
identity_directory: .cog/identities
load_on_session_start: true
inject_context_plugin: false
`), 0o644)

	// Create identity card
	cardDir := filepath.Join(root, ".cog", "identities")
	os.MkdirAll(cardDir, 0o755)
	os.WriteFile(filepath.Join(cardDir, "identity_test.md"), []byte(`---
name: Test
role: tester
context_plugin: ""
memory_path: ""
memory_namespace: test
---
# Test Identity

You are a test identity.
`), 0o644)

	// Create working memory
	memDir := filepath.Join(root, ".cog", "mem")
	os.MkdirAll(memDir, 0o755)
	os.WriteFile(filepath.Join(memDir, "working.cog.md"), []byte(`---
title: Working Memory
---
# Current Focus
Testing the TAA pipeline

# Next Actions
- Verify all tiers
`), 0o644)

	return root
}

func TestConstructContextState(t *testing.T) {
	root := makeTempWorkspace(t)

	messages := []ChatMessage{
		makeMsg("user", "I want to implement the agent provider"),
		makeMsg("assistant", "Sure, let me help with the agent provider implementation."),
		makeMsg("user", "Start with the reconciliation loop for the agent provider"),
	}

	state, err := ConstructContextState(messages, "test-session", root)
	// err may be non-nil (partial construction) due to missing constellation etc.
	// but state should still be populated
	if state == nil {
		t.Fatalf("ConstructContextState returned nil state, err=%v", err)
	}

	// Tier 1 should have loaded the test identity
	if state.Tier1Identity == nil {
		t.Error("Tier1Identity is nil — identity should have loaded")
	} else if !strings.Contains(state.Tier1Identity.Content, "Test Identity") {
		t.Errorf("Tier1Identity content doesn't contain identity card: %s", state.Tier1Identity.Content[:min(100, len(state.Tier1Identity.Content))])
	}

	// Tier 2 should have temporal context
	if state.Tier2Temporal == nil {
		t.Error("Tier2Temporal is nil")
	}

	// Tier 3 should have present context
	if state.Tier3Present == nil {
		t.Error("Tier3Present is nil")
	}

	// Anchor should be set (agent, provider are repeated words)
	if state.Anchor == "" {
		t.Error("Anchor is empty — expected topic extraction from messages")
	}

	// TotalTokens should be positive
	if state.TotalTokens <= 0 {
		t.Errorf("TotalTokens=%d, expected positive", state.TotalTokens)
	}

	// CoherenceScore should be between 0 and 1
	if state.CoherenceScore < 0 || state.CoherenceScore > 1 {
		t.Errorf("CoherenceScore=%.2f, expected 0..1", state.CoherenceScore)
	}
}

func TestConstructContextStateMinimal(t *testing.T) {
	messages := []ChatMessage{
		makeMsg("user", "hello world"),
	}

	state := ConstructContextStateMinimal(messages)
	if state == nil {
		t.Fatal("ConstructContextStateMinimal returned nil")
	}

	// Only Tier 3 should be populated
	if state.Tier1Identity != nil {
		t.Error("Tier1Identity should be nil for minimal state")
	}
	if state.Tier2Temporal != nil {
		t.Error("Tier2Temporal should be nil for minimal state")
	}
	if state.Tier3Present == nil {
		t.Error("Tier3Present should not be nil for minimal state")
	}

	// Should be marked for refresh
	if !state.ShouldRefresh {
		t.Error("ShouldRefresh should be true for minimal state")
	}

	// Coherence should be partial
	if state.CoherenceScore >= 1.0 {
		t.Errorf("CoherenceScore=%.2f, expected partial for minimal state", state.CoherenceScore)
	}
}

func TestConstructContextStatePartialFailure(t *testing.T) {
	// Use a non-existent workspace to trigger tier failures
	messages := []ChatMessage{
		makeMsg("user", "test message"),
	}

	state, err := ConstructContextState(messages, "test-session", "/nonexistent/workspace")

	// Should get partial errors
	if err == nil {
		t.Error("Expected error for non-existent workspace")
	}

	// State should still be non-nil (partial success)
	if state == nil {
		t.Fatal("State should not be nil even with errors")
	}

	// Tier 3 should still work (doesn't depend on workspace)
	if state.Tier3Present == nil {
		t.Error("Tier3Present should still be populated despite workspace errors")
	}
}

func TestCoherenceScoring(t *testing.T) {
	tests := []struct {
		name     string
		tier1    bool
		tier2    bool
		tier3    bool
		tier4    bool
		expected float64
	}{
		{"all tiers", true, true, true, true, 1.0},
		{"three tiers", true, true, true, false, 0.75},
		{"two tiers", true, false, true, false, 0.5},
		{"one tier", false, false, true, false, 0.25},
		{"no tiers", false, false, false, false, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &ContextState{}
			if tt.tier1 {
				state.Tier1Identity = &ContextTier{Content: "t1", Tokens: 10}
			}
			if tt.tier2 {
				state.Tier2Temporal = &ContextTier{Content: "t2", Tokens: 10}
			}
			if tt.tier3 {
				state.Tier3Present = &ContextTier{Content: "t3", Tokens: 10}
			}
			if tt.tier4 {
				state.Tier4Semantic = &ContextTier{Content: "t4", Tokens: 10}
			}

			// Calculate coherence the same way ConstructContextState does
			successfulTiers := 0
			if state.Tier1Identity != nil {
				successfulTiers++
			}
			if state.Tier2Temporal != nil {
				successfulTiers++
			}
			if state.Tier3Present != nil {
				successfulTiers++
			}
			if state.Tier4Semantic != nil {
				successfulTiers++
			}
			score := float64(successfulTiers) / 4.0

			if score != tt.expected {
				t.Errorf("coherence score=%.2f, expected %.2f", score, tt.expected)
			}
		})
	}
}

func TestBuildContextString(t *testing.T) {
	state := &ContextState{
		Tier1Identity: &ContextTier{Content: "identity content", Tokens: 4},
		Tier2Temporal: &ContextTier{Content: "temporal content", Tokens: 4},
		Tier3Present:  &ContextTier{Content: "present content", Tokens: 4},
	}

	result := state.BuildContextString()

	if !strings.Contains(result, "identity content") {
		t.Error("BuildContextString missing tier 1 content")
	}
	if !strings.Contains(result, "temporal content") {
		t.Error("BuildContextString missing tier 2 content")
	}
	if !strings.Contains(result, "present content") {
		t.Error("BuildContextString missing tier 3 content")
	}

	// Should have separators between tiers
	if !strings.Contains(result, "---") {
		t.Error("BuildContextString missing tier separators")
	}
}

func TestBuildContextStringNil(t *testing.T) {
	var state *ContextState
	result := state.BuildContextString()
	if result != "" {
		t.Errorf("BuildContextString on nil should return empty, got %q", result)
	}
}

func TestBuildContextStringEmpty(t *testing.T) {
	state := &ContextState{}
	result := state.BuildContextString()
	if result != "" {
		t.Errorf("BuildContextString on empty state should return empty, got %q", result)
	}
}

func TestChainSystemPrompt(t *testing.T) {
	// Both TAA and client system prompt — should chain with separator
	req := &InferenceRequest{
		SystemPrompt: "You are a helpful assistant.",
		ContextState: &ContextState{
			Tier1Identity: &ContextTier{Content: "identity block"},
			Tier2Temporal: &ContextTier{Content: "temporal block"},
		},
	}
	result := chainSystemPrompt(req)
	if !strings.Contains(result, "identity block") {
		t.Error("chain should include TAA content")
	}
	if !strings.Contains(result, "You are a helpful assistant.") {
		t.Error("chain should include client system prompt")
	}
	// TAA should come before client system prompt
	taaIdx := strings.Index(result, "identity block")
	clientIdx := strings.Index(result, "You are a helpful assistant.")
	if taaIdx > clientIdx {
		t.Error("TAA context should come before client system prompt")
	}
	// Should be separated by ---
	separatorIdx := strings.Index(result, "\n\n---\n\n")
	if separatorIdx < 0 || separatorIdx < taaIdx || separatorIdx > clientIdx {
		t.Error("TAA and client prompt should be joined by --- separator")
	}
}

func TestChainSystemPromptTAAOnly(t *testing.T) {
	req := &InferenceRequest{
		ContextState: &ContextState{
			Tier1Identity: &ContextTier{Content: "identity only"},
		},
	}
	result := chainSystemPrompt(req)
	if result != "identity only" {
		t.Errorf("TAA-only chain = %q, expected 'identity only'", result)
	}
}

func TestChainSystemPromptClientOnly(t *testing.T) {
	req := &InferenceRequest{
		SystemPrompt: "client instructions",
	}
	result := chainSystemPrompt(req)
	if result != "client instructions" {
		t.Errorf("client-only chain = %q, expected 'client instructions'", result)
	}
}

func TestChainSystemPromptNeither(t *testing.T) {
	req := &InferenceRequest{}
	result := chainSystemPrompt(req)
	if result != "" {
		t.Errorf("empty chain should return empty, got %q", result)
	}
}

func TestParseContextURI(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		budget  int
		profile string
		model   string
		wantErr bool
	}{
		{"full URI", "cog://context?budget=50000&profile=default&model=sonnet", 50000, "default", "sonnet", false},
		{"profile only", "cog://context?profile=minimal", 0, "minimal", "", false},
		{"bare", "cog://context", 0, "", "", false},
		{"shorthand", "context?budget=30000", 30000, "", "", false},
		{"wrong namespace", "cog://mem/semantic", 0, "", "", true},
		{"invalid URI", "not-a-uri", 0, "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, err := parseContextURI(tt.uri)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if params.budget != tt.budget {
				t.Errorf("budget=%d, want %d", params.budget, tt.budget)
			}
			if params.profile != tt.profile {
				t.Errorf("profile=%q, want %q", params.profile, tt.profile)
			}
			if params.model != tt.model {
				t.Errorf("model=%q, want %q", params.model, tt.model)
			}
		})
	}
}

func TestBuildCLIContext(t *testing.T) {
	root := makeTempWorkspace(t)
	t.Setenv("COG_ROOT", root)

	// Test with profile
	state := buildCLIContext("test prompt", "default", "", nil, "cli")
	// May get partial errors due to missing profile file, but function should not panic
	// The test workspace doesn't have .cog/config/taa/profiles/default.yaml,
	// so it falls back to ConstructContextState
	if state != nil {
		if state.TotalTokens < 0 {
			t.Errorf("TotalTokens=%d, expected non-negative", state.TotalTokens)
		}
	}
}

func TestContextSummary(t *testing.T) {
	state := &ContextState{
		Tier1Identity:  &ContextTier{Content: "id", Tokens: 100, Source: "identity"},
		Tier3Present:   &ContextTier{Content: "present", Tokens: 500, Source: "present"},
		TotalTokens:    600,
		CoherenceScore: 0.5,
		ShouldRefresh:  true,
	}

	summary := state.ContextSummary()

	if !strings.Contains(summary, "Tier 1 (Identity): 100 tokens") {
		t.Error("Summary missing tier 1 info")
	}
	if !strings.Contains(summary, "Tier 2 (Temporal): not loaded") {
		t.Error("Summary should show tier 2 as not loaded")
	}
	if !strings.Contains(summary, "needs refresh") {
		t.Error("Summary should indicate needs refresh")
	}
}

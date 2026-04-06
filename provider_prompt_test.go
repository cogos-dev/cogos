package main

import (
	"strings"
	"testing"
)

func TestClaudeCodeBuildPromptIncludesMultipleUserTurns(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{}
	req := &CompletionRequest{
		SystemPrompt: "workspace manifest",
		Messages: []ProviderMessage{
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: "first answer"},
			{Role: "user", Content: "second question"},
		},
	}

	prompt := p.buildPrompt(req)
	if !strings.Contains(prompt, "first question") {
		t.Fatal("prompt should contain the first user turn")
	}
	if !strings.Contains(prompt, "second question") {
		t.Fatal("prompt should contain the second user turn")
	}
	if strings.Count(prompt, "User Turn") < 2 {
		t.Fatalf("prompt should render multiple user turns, got %q", prompt)
	}
}

func TestCodexBuildPromptIncludesContextAndTranscript(t *testing.T) {
	t.Parallel()

	p := &CodexProvider{}
	req := &CompletionRequest{
		SystemPrompt: "workspace manifest",
		Messages: []ProviderMessage{
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: "first answer"},
			{Role: "user", Content: "second question"},
		},
	}

	prompt := p.buildPrompt(req)
	if !strings.Contains(prompt, "## Context") {
		t.Fatal("prompt should include the context heading")
	}
	if !strings.Contains(prompt, "workspace manifest") {
		t.Fatal("prompt should include the system context")
	}
	if !strings.Contains(prompt, "## Assistant") {
		t.Fatal("prompt should include assistant transcript sections")
	}
	if !strings.Contains(prompt, "second question") {
		t.Fatal("prompt should include later user turns")
	}
}

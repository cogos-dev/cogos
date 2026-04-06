package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestHandleFoveatedContext(t *testing.T) {
	t.Parallel()

	// Set up a temp workspace with a git repo and a CogDoc.
	tmp := t.TempDir()
	memDir := filepath.Join(tmp, ".cog", "mem", "semantic", "inbox", "links")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cogdoc := filepath.Join(memDir, "test-arxiv-paper.cog.md")
	if err := os.WriteFile(cogdoc, []byte(`---
id: test-paper
title: "Attention Is All You Need"
description: Transformer paper introducing self-attention architectures
type: link
status: raw
tags: [arxiv, paper, transformers]
---

This paper introduces the Transformer architecture based on self-attention mechanisms.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Init a git repo and commit the CogDoc so salience scoring can open it.
	repo, err := git.PlainInit(tmp, false)
	if err != nil {
		t.Fatal("git init:", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal("worktree:", err)
	}
	if _, err := wt.Add("."); err != nil {
		t.Fatal("git add:", err)
	}
	sig := &object.Signature{Name: "test", Email: "test@test", When: time.Now()}
	if _, err := wt.Commit("init", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal("git commit:", err)
	}

	cfg := &Config{WorkspaceRoot: tmp, CogDir: filepath.Join(tmp, ".cog"), Port: 0, SalienceDaysWindow: 90}
	nucleus := &Nucleus{Name: "test", Card: "test identity"}
	process := NewProcess(cfg, nucleus)
	if err := process.Field().Update(); err != nil {
		t.Fatal("field update:", err)
	}
	// Build the CogDoc index so the keyword fallback path can find docs.
	if idx, err := BuildIndex(tmp); err != nil {
		t.Fatal("build index:", err)
	} else {
		process.indexMu.Lock()
		process.index = idx
		process.indexMu.Unlock()
	}

	srv := NewServer(cfg, nucleus, process)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(foveatedRequest{
		Prompt:    "tell me about transformer architectures and attention",
		Iris:      irisSignal{Size: 200000, Used: 10000},
		Profile:   "claude-code",
		SessionID: "test-session",
	})

	resp, err := http.Post(ts.URL+"/v1/context/foveated", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	var result foveatedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal("decode:", err)
	}

	// Should have non-empty context with block markers.
	if !strings.Contains(result.Context, "<!-- block:tier4:knowledge") {
		t.Error("missing tier4:knowledge block in context")
	}
	if !strings.Contains(result.Context, "<!-- block:tier2:current-focus") {
		t.Error("missing tier2:current-focus block in context")
	}
	if !strings.Contains(result.Context, "<!-- block:tier2:user-intent") {
		t.Error("missing tier2:user-intent block in context")
	}
	if !strings.Contains(result.Context, "Use cog_read_cogdoc to access full content when needed") {
		t.Error("missing manifest retrieval hint in context")
	}
	if !strings.Contains(result.Context, "Transformer paper introducing self-attention architectures") {
		t.Error("missing manifest summary in context")
	}

	// Anchor should contain meaningful keywords.
	if result.Anchor == "" || result.Anchor == "(none)" {
		t.Errorf("anchor = %q; want non-empty keywords", result.Anchor)
	}

	// Goal should be classified.
	if result.Goal == "" {
		t.Error("goal is empty")
	}

	// Iris pressure should be computed correctly.
	expectedPressure := 10000.0 / 200000.0
	if result.IrisPressure != expectedPressure {
		t.Errorf("iris_pressure = %f; want %f", result.IrisPressure, expectedPressure)
	}

	// Tokens should be positive.
	if result.Tokens <= 0 {
		t.Errorf("tokens = %d; want > 0", result.Tokens)
	}

	if len(result.Blocks) != 4 {
		t.Fatalf("blocks = %d; want 4", len(result.Blocks))
	}
	if result.Blocks[0].Tier != "tier4" || result.Blocks[0].Name != "knowledge" {
		t.Fatalf("first block = %s/%s; want tier4/knowledge", result.Blocks[0].Tier, result.Blocks[0].Name)
	}
	if len(result.Blocks[0].Sources) == 0 {
		t.Fatal("knowledge block missing sources")
	}
	if result.Blocks[0].Sources[0].Title == "" {
		t.Error("knowledge source title is empty")
	}
	if result.Blocks[0].Hash == "" {
		t.Error("knowledge block hash is empty")
	}

	t.Logf("anchor=%q goal=%q tokens=%d pressure=%.1f%%",
		result.Anchor, result.Goal, result.Tokens, result.IrisPressure*100)
	t.Logf("context preview: %s", result.Context[:min(200, len(result.Context))])
}

func TestExtractAnchor(t *testing.T) {
	tests := []struct {
		prompt string
		want   string // substring that should appear
	}{
		{"tell me about transformer architectures", "transformer"},
		{"how does the ingestion pipeline work?", "ingestion"},
		{"", "(none)"},
	}
	for _, tt := range tests {
		got := extractAnchor(tt.prompt)
		if !strings.Contains(got, tt.want) {
			t.Errorf("extractAnchor(%q) = %q; want to contain %q", tt.prompt, got, tt.want)
		}
	}
}

func TestExtractGoal(t *testing.T) {
	tests := []struct {
		prompt string
		prefix string
	}{
		{"how does this work?", "understand:"},
		{"build the auto-doc pipeline", "build the auto-doc pipeline"},
		{"let's knock it out", "let's knock it out"},
		{"interesting stuff", "(exploring/discussing)"},
	}
	for _, tt := range tests {
		got := extractGoal(tt.prompt)
		if !strings.HasPrefix(got, tt.prefix) {
			t.Errorf("extractGoal(%q) = %q; want prefix %q", tt.prompt, got, tt.prefix)
		}
	}
}

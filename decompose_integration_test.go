//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDecompE2EIntegration runs a full decomposition pipeline against a real
// Ollama instance with a sample markdown file, then verifies all tiers, embeddings,
// CogDoc storage, and bus event emission.
//
// Requires: Ollama running on localhost:11434 with gemma4:e4b loaded.
// Run with: go test -tags integration -run TestDecompE2EIntegration -timeout 120s
func TestDecompE2EIntegration(t *testing.T) {
	// 1. Check Ollama availability — skip gracefully if not running
	ollamaURL := "http://localhost:11434"
	if env := os.Getenv("OLLAMA_HOST"); env != "" {
		ollamaURL = env
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ollamaURL+"/api/tags", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("Ollama not available at %s: %v", ollamaURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Skipf("Ollama returned status %d", resp.StatusCode)
	}

	// 2. Create temp workspace with a sample .md file
	tmpDir := t.TempDir()
	cogDir := filepath.Join(tmpDir, ".cog")
	if err := os.MkdirAll(cogDir, 0755); err != nil {
		t.Fatalf("create .cog dir: %v", err)
	}

	sampleContent := `---
title: "Test ADR: Sample Architecture Decision"
type: architecture
status: accepted
---

# Test ADR: Sample Architecture Decision

## Context

The system needs a decomposition pipeline that breaks documents into tiered summaries.
Each tier provides a different level of detail, from a single sentence to the full document.

## Decision

We will implement a four-tier decomposition pipeline:
- **Tier 0**: One-sentence summary (~15 tokens)
- **Tier 1**: Paragraph summary with key terms (~100 tokens)
- **Tier 2**: Full CogDoc structure with sections
- **Tier 3**: Raw passthrough

The pipeline uses a local LLM (Gemma E4B) via Ollama for tiers 0-2.

## Consequences

- Efficient memory indexing through tiered compression
- Local-first: no cloud API calls needed for decomposition
- Deterministic hashing enables deduplication
`

	samplePath := filepath.Join(tmpDir, "test-adr.md")
	if err := os.WriteFile(samplePath, []byte(sampleContent), 0644); err != nil {
		t.Fatalf("write sample file: %v", err)
	}

	// 3. Create harness pointing at real Ollama
	harness := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: ollamaURL,
		Model:     "gemma4:e4b",
	})

	// Create file-based event callback for the temp workspace
	callback := newFileEventCallback(tmpDir)

	runner := NewDecompositionRunner(harness, callback)

	// 4. Run decomposition with all tiers
	runCtx, runCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer runCancel()

	input := &DecompInput{
		Text:      sampleContent,
		Format:    detectFormat(sampleContent),
		SourceURI: "file://" + samplePath,
		ByteSize:  len(sampleContent),
	}

	result, err := runner.Run(runCtx, input)
	if err != nil {
		t.Fatalf("decomposition run failed: %v", err)
	}

	// 5. Verify all tiers are populated
	t.Run("tier_0_populated", func(t *testing.T) {
		if result.Tier0 == nil {
			t.Fatal("Tier0 is nil")
		}
		if result.Tier0.Summary == "" {
			t.Error("Tier0 summary is empty")
		}
		t.Logf("Tier 0 summary: %s", result.Tier0.Summary)
	})

	t.Run("tier_1_populated", func(t *testing.T) {
		if result.Tier1 == nil {
			t.Fatal("Tier1 is nil")
		}
		if result.Tier1.Summary == "" {
			t.Error("Tier1 summary is empty")
		}
		if len(result.Tier1.KeyTerms) == 0 {
			t.Error("Tier1 key_terms is empty")
		}
		t.Logf("Tier 1 key terms: %v", result.Tier1.KeyTerms)
	})

	t.Run("tier_2_populated", func(t *testing.T) {
		if result.Tier2 == nil {
			t.Fatal("Tier2 is nil")
		}
		if result.Tier2.Title == "" {
			t.Error("Tier2 title is empty")
		}
		if result.Tier2.Type == "" {
			t.Error("Tier2 type is empty")
		}
		if len(result.Tier2.Tags) == 0 {
			t.Error("Tier2 tags is empty")
		}
		if len(result.Tier2.Sections) == 0 {
			t.Error("Tier2 sections is empty")
		}
		t.Logf("Tier 2 title: %s, type: %s", result.Tier2.Title, result.Tier2.Type)
	})

	t.Run("tier_3_raw", func(t *testing.T) {
		if result.Tier3Raw == "" {
			t.Error("Tier3Raw is empty")
		}
		if result.Tier3Raw != sampleContent {
			t.Error("Tier3Raw does not match original input")
		}
	})

	// 6. Verify metrics
	t.Run("metrics", func(t *testing.T) {
		m := result.Metrics
		if m.TotalLatencyMs <= 0 {
			t.Error("total_latency_ms should be positive")
		}
		if m.Tier0LatencyMs <= 0 {
			t.Error("tier0_latency_ms should be positive")
		}
		if m.CompressionRatio <= 0 {
			t.Error("compression_ratio should be positive")
		}
		t.Logf("Latency: total=%dms, t0=%dms, t1=%dms, t2=%dms, compression=%.1f:1",
			m.TotalLatencyMs, m.Tier0LatencyMs, m.Tier1LatencyMs, m.Tier2LatencyMs, m.CompressionRatio)
	})

	// 7. Verify embeddings were attempted (best-effort — check if non-nil)
	t.Run("embeddings_attempted", func(t *testing.T) {
		// "" -> defaultEmbedModel (bge-m3:latest); set EMBED_MODEL env to override.
		embedModel := os.Getenv("EMBED_MODEL")
		embedResults(runCtx, ollamaURL, embedModel, result)
		if result.Embeddings == nil {
			t.Log("Embeddings are nil — embed model may not be available (acceptable)")
		} else {
			if len(result.Embeddings.Tier0_128) == 0 {
				t.Log("Tier0_128 embeddings empty — truncation may have produced empty vector")
			} else {
				t.Logf("Tier0_128 has %d dimensions", len(result.Embeddings.Tier0_128))
			}
			if len(result.Embeddings.Tier2_768) == 0 {
				t.Log("Tier2_768 embeddings empty")
			} else {
				t.Logf("Tier2_768 has %d dimensions", len(result.Embeddings.Tier2_768))
			}
		}
	})

	// 8. Verify CogDoc was written
	t.Run("cogdoc_stored", func(t *testing.T) {
		storeResult(tmpDir, result)
		decompDir := filepath.Join(tmpDir, ".cog", "mem", "semantic", "decompositions")
		cogdocPath := filepath.Join(decompDir, result.InputHash+".cog.md")

		if _, err := os.Stat(cogdocPath); os.IsNotExist(err) {
			t.Fatalf("CogDoc not found at %s", cogdocPath)
		}

		content, err := os.ReadFile(cogdocPath)
		if err != nil {
			t.Fatalf("read CogDoc: %v", err)
		}

		cogdoc := string(content)
		// Verify it has YAML frontmatter
		if !strings.HasPrefix(cogdoc, "---\n") {
			t.Error("CogDoc missing YAML frontmatter")
		}
		// Verify it contains tier sections
		if !strings.Contains(cogdoc, "# Tier 0") {
			t.Error("CogDoc missing Tier 0 section")
		}
		if !strings.Contains(cogdoc, "# Tier 1") {
			t.Error("CogDoc missing Tier 1 section")
		}
		if !strings.Contains(cogdoc, "# Tier 2") {
			t.Error("CogDoc missing Tier 2 section")
		}
		t.Logf("CogDoc written: %d bytes at %s", len(content), cogdocPath)
	})

	// 9. Verify bus events were emitted
	t.Run("bus_events_emitted", func(t *testing.T) {
		eventsPath := filepath.Join(tmpDir, ".cog", ".state", "buses", "decompose", "events.jsonl")
		if _, err := os.Stat(eventsPath); os.IsNotExist(err) {
			t.Fatalf("bus events file not found at %s", eventsPath)
		}

		data, err := os.ReadFile(eventsPath)
		if err != nil {
			t.Fatalf("read events: %v", err)
		}

		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) < 2 {
			t.Fatalf("expected at least 2 events (start + complete), got %d", len(lines))
		}

		// Parse and verify event types
		var eventTypes []string
		for _, line := range lines {
			var evt struct {
				Type string `json:"type"`
				From string `json:"from"`
			}
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				t.Errorf("parse event line: %v", err)
				continue
			}
			if evt.From != "decompose" {
				t.Errorf("event from=%q, expected 'decompose'", evt.From)
			}
			eventTypes = append(eventTypes, evt.Type)
		}

		// Must have start and complete
		hasStart := false
		hasComplete := false
		for _, et := range eventTypes {
			if et == DecompEventStart {
				hasStart = true
			}
			if et == DecompEventComplete {
				hasComplete = true
			}
		}
		if !hasStart {
			t.Error("missing decompose.start event")
		}
		if !hasComplete {
			t.Error("missing decompose.complete event")
		}
		t.Logf("Bus events: %d total, types: %v", len(lines), eventTypes)
	})

	// 10. Verify JSON round-trip
	t.Run("json_roundtrip", func(t *testing.T) {
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("marshal result: %v", err)
		}
		var roundTripped DecompositionResult
		if err := json.Unmarshal(data, &roundTripped); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if roundTripped.InputHash != result.InputHash {
			t.Error("input_hash mismatch after round-trip")
		}
		if roundTripped.Tier0 == nil || roundTripped.Tier0.Summary != result.Tier0.Summary {
			t.Error("tier_0 mismatch after round-trip")
		}
	})

	fmt.Printf("\n=== Integration Test Summary ===\n")
	fmt.Printf("Input: %d bytes (%s)\n", result.InputSize, result.InputFormat)
	fmt.Printf("Tier 0: %s\n", result.Tier0.Summary)
	fmt.Printf("Tier 1 terms: %v\n", result.Tier1.KeyTerms)
	fmt.Printf("Tier 2: %s [%s]\n", result.Tier2.Title, result.Tier2.Type)
	fmt.Printf("Latency: %dms total\n", result.Metrics.TotalLatencyMs)
	fmt.Printf("Compression: %.0f:1\n", result.Metrics.CompressionRatio)
}

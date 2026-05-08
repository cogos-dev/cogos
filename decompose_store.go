// decompose_store.go — Wave 3: Embedding generation and CogDoc storage
//
// embedResults() calls Ollama's native /api/embed endpoint to generate
// vector embeddings for each decomposition tier. Best-effort: if the
// embed endpoint is unreachable, logs a warning and continues.
//
// buildCogDoc() renders a DecompositionResult as a CogDoc markdown file
// with YAML frontmatter suitable for storage in .cog/mem/semantic/.
//
// storeResult() writes the CogDoc to disk. Also best-effort.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/myrgic/cogos/sdk/constellation"
)

// === C1+C2: Embedding Generation ===

// ollamaEmbedRequest is the request body for Ollama's /api/embed endpoint.
type ollamaEmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// ollamaEmbedResponse is the response from Ollama's /api/embed endpoint.
type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// embedFromOllama calls Ollama's native embed endpoint for a single text.
// Returns the full embedding vector (768-dim for nomic-embed-text) or error.
func embedFromOllama(ctx context.Context, ollamaURL, text string) ([]float32, error) {
	reqBody := ollamaEmbedRequest{
		Model: "nomic-embed-text",
		Input: text,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	url := ollamaURL + "/api/embed"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embed server returned %d: %s", resp.StatusCode, string(body))
	}

	var embedResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}

	if len(embedResp.Embeddings) == 0 {
		return nil, fmt.Errorf("embed server returned no embeddings")
	}

	return embedResp.Embeddings[0], nil
}

// truncateVec returns the first n elements of a vector (Matryoshka truncation).
func truncateVec(v []float32, n int) []float32 {
	if len(v) <= n {
		return v
	}
	out := make([]float32, n)
	copy(out, v[:n])
	return out
}

// embedResults generates embeddings for each tier in the decomposition result.
// Best-effort: logs warnings on failure, never returns an error.
func embedResults(ctx context.Context, ollamaURL string, result *DecompositionResult) {
	if result == nil {
		return
	}

	emb := &DecompEmbeddings{}
	var generated bool

	// Tier 0: embed the one-sentence summary
	if result.Tier0 != nil && result.Tier0.Summary != "" {
		vec, err := embedFromOllama(ctx, ollamaURL, "search_document: "+result.Tier0.Summary)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: tier 0 embedding failed: %v\n", err)
		} else {
			emb.Tier0_128 = truncateVec(vec, 128)
			generated = true
		}
	}

	// Tier 1: embed the paragraph summary
	if result.Tier1 != nil && result.Tier1.Summary != "" {
		vec, err := embedFromOllama(ctx, ollamaURL, "search_document: "+result.Tier1.Summary)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: tier 1 embedding failed: %v\n", err)
		} else {
			emb.Tier1_128 = truncateVec(vec, 128)
			generated = true
		}
	}

	// Tier 2: embed full content (title + summary + sections concatenated)
	if result.Tier2 != nil {
		var parts []string
		if result.Tier2.Title != "" {
			parts = append(parts, result.Tier2.Title)
		}
		if result.Tier2.Summary != "" {
			parts = append(parts, result.Tier2.Summary)
		}
		for _, s := range result.Tier2.Sections {
			parts = append(parts, s.Heading+": "+s.Content)
		}
		if len(parts) > 0 {
			fullText := strings.Join(parts, "\n")
			vec, err := embedFromOllama(ctx, ollamaURL, "search_document: "+fullText)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: tier 2 embedding failed: %v\n", err)
			} else {
				emb.Tier2_768 = vec
				emb.Tier2_128 = truncateVec(vec, 128)
				generated = true
			}
		}
	}

	if generated {
		result.Embeddings = emb
	}
}

// === C3: CogDoc Storage ===

// buildCogDoc renders a DecompositionResult as a CogDoc markdown string
// with YAML frontmatter.
func buildCogDoc(result *DecompositionResult) string {
	var b strings.Builder

	// --- YAML frontmatter ---
	title := "Decomposition of " + result.InputHash
	if result.Tier2 != nil && result.Tier2.Title != "" {
		title = result.Tier2.Title
	}

	var tags []string
	if result.Tier2 != nil {
		tags = result.Tier2.Tags
	}

	sourceURI := result.SourceURI
	if sourceURI == "" {
		sourceURI = "hash://" + result.InputHash
	}

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("title: %q\n", title))
	b.WriteString("type: decomposition\n")
	b.WriteString(fmt.Sprintf("created: %s\n", result.CreatedAt.Format(time.RFC3339)))
	b.WriteString("status: active\n")
	b.WriteString("salience: medium\n")
	b.WriteString("memory_sector: semantic\n")

	// tags
	if len(tags) > 0 {
		quotedTags := make([]string, len(tags))
		for i, t := range tags {
			quotedTags[i] = fmt.Sprintf("%q", t)
		}
		b.WriteString(fmt.Sprintf("tags: [%s]\n", strings.Join(quotedTags, ", ")))
	} else {
		b.WriteString("tags: []\n")
	}

	// refs
	b.WriteString("refs:\n")
	b.WriteString(fmt.Sprintf("  - uri: %q\n", sourceURI))
	b.WriteString("    rel: decomposes\n")

	b.WriteString("sections: [tier-0, tier-1, tier-2, metadata]\n")
	b.WriteString("---\n\n")

	// --- Body sections ---

	// Tier 0
	b.WriteString("# Tier 0 — Sentence\n")
	if result.Tier0 != nil {
		b.WriteString(result.Tier0.Summary)
	} else {
		b.WriteString("_(not generated)_")
	}
	b.WriteString("\n\n")

	// Tier 1
	b.WriteString("# Tier 1 — Paragraph\n")
	if result.Tier1 != nil {
		b.WriteString(result.Tier1.Summary)
		if len(result.Tier1.KeyTerms) > 0 {
			b.WriteString("\n\n**Key terms:** ")
			b.WriteString(strings.Join(result.Tier1.KeyTerms, ", "))
		}
	} else {
		b.WriteString("_(not generated)_")
	}
	b.WriteString("\n\n")

	// Tier 2
	b.WriteString("# Tier 2 — Structured\n")
	if result.Tier2 != nil {
		for _, s := range result.Tier2.Sections {
			b.WriteString(fmt.Sprintf("## %s\n%s\n\n", s.Heading, s.Content))
		}
	} else {
		b.WriteString("_(not generated)_\n\n")
	}

	// Metadata
	b.WriteString("# Metadata\n")
	b.WriteString(fmt.Sprintf("- Input: %d bytes (%s)\n", result.InputSize, result.InputFormat))
	b.WriteString(fmt.Sprintf("- Hash: %s\n", result.InputHash))
	if result.Metrics.CompressionRatio > 0 {
		b.WriteString(fmt.Sprintf("- Compression: %.1f:1\n", result.Metrics.CompressionRatio))
	}
	b.WriteString(fmt.Sprintf("- Generated: %s\n", result.CreatedAt.Format(time.RFC3339)))

	return b.String()
}

// storeResult writes the CogDoc to .cog/mem/semantic/decompositions/{hash}.cog.md.
// Best-effort: logs warnings on failure, never returns an error.
func storeResult(workspaceRoot string, result *DecompositionResult) {
	if result == nil || workspaceRoot == "" {
		return
	}

	dir := filepath.Join(workspaceRoot, ".cog", "mem", "semantic", "decompositions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not create decomposition dir: %v\n", err)
		return
	}

	path := filepath.Join(dir, result.InputHash+".cog.md")
	content := buildCogDoc(result)

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not write CogDoc: %v\n", err)
		return
	}

	fmt.Fprintf(os.Stderr, "Stored: %s\n", path)
}

// indexInConstellation indexes a CogDoc file in the constellation database
// for vector+FTS5 retrieval. Best-effort: returns error on failure but
// callers should treat it as non-fatal.
func indexInConstellation(workspaceRoot string, cogdocPath string) error {
	c, err := constellation.Open(workspaceRoot)
	if err != nil {
		return fmt.Errorf("open constellation: %w", err)
	}
	defer c.Close()

	if err := c.IndexFile(cogdocPath); err != nil {
		return fmt.Errorf("index file: %w", err)
	}
	return nil
}

// findWorkspaceRoot walks up from dir looking for a .cog/ directory.
// Returns the workspace root path, or empty string if not found.
func findWorkspaceRoot(dir string) string {
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".cog")); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

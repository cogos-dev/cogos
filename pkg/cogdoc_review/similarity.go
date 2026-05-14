// Package cogdoc_review implements the deterministic review pipeline for
// cogdoc authoring.
//
// The similarity search module (T02) is the core primitive: given a proposed
// cogdoc query string, it embeds the query via Ollama bge-m3, walks the corpus
// directories for existing cogdocs, embeds each cogdoc's title+abstract, and
// returns the top-N candidates above a cosine-similarity threshold.
//
// This module is used by:
//   - CogdocReviewReconciler (Layer A, always-on substrate guarantee)
//   - git-pre-commit.py hook (Layer B, authoring-time feedback, Python calls out via subprocess)
//   - /cogdoc:propose skill (Layer C, conversational surface)
//
// The similarity.go file in this package intentionally mirrors the OllamaEmbed
// and l2Normalize patterns from internal/engine/trm_context.go.
// It does NOT import the internal/engine package (no circular dependency).
// Instead it re-implements the slim embed+normalize primitives it needs.
// Both implementations must stay aligned with the workspace-canonical embedding
// model (bge-m3:latest) and the Matryoshka truncation to 384 dimensions.
package cogdoc_review

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/myrgic/cogos/pkg/reconcile"
)

const (
	defaultEmbedModel    = "bge-m3:latest"
	matryoshkaDim        = 384
	pipelineVersion      = "v0.1.0"
)

// --- Ollama embed (slim copy; no import of internal/engine) ---

type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
	Embedding []float64 `json:"embedding"`
}

// embedQuery embeds a query string via Ollama and returns a unit-normalized
// float32 vector truncated to matryoshkaDim (384).
func embedQuery(ctx context.Context, ollamaEndpoint, model, query string) ([]float32, error) {
	if ollamaEndpoint == "" {
		ollamaEndpoint = "http://localhost:11434"
	}
	if model == "" {
		model = defaultEmbedModel
	}

	reqBody, err := json.Marshal(ollamaEmbedRequest{
		Model:  model,
		Prompt: query, // bge-m3 does not use task prefixes
	})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	httpCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(httpCtx, http.MethodPost,
		ollamaEndpoint+"/api/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("ollama embed: status %d: %s", resp.StatusCode, string(body))
	}

	var embedResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}

	dim := len(embedResp.Embedding)
	if dim > matryoshkaDim {
		dim = matryoshkaDim
	}
	vec := make([]float32, dim)
	for i := 0; i < dim; i++ {
		vec[i] = float32(embedResp.Embedding[i])
	}
	return l2Normalize(vec), nil
}

// l2Normalize normalizes v to unit length in-place-style (returns new slice).
func l2Normalize(v []float32) []float32 {
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	norm := math.Sqrt(sumSq)
	if norm == 0 {
		return v
	}
	out := make([]float32, len(v))
	invNorm := float32(1.0 / norm)
	for i, x := range v {
		out[i] = x * invNorm
	}
	return out
}

// cosineSimilarity computes dot product of two pre-normalized unit vectors.
func cosineSimilarity(a, b []float32) float32 {
	min := len(a)
	if len(b) < min {
		min = len(b)
	}
	var dot float32
	for i := 0; i < min; i++ {
		dot += a[i] * b[i]
	}
	return dot
}

// --- Cogdoc frontmatter parser ---

// cogdocFrontmatter is the minimal frontmatter we need for similarity search.
type cogdocFrontmatter struct {
	Type        string   `yaml:"type"`
	ID          string   `yaml:"id"`
	Title       string   `yaml:"title"`
	Abstract    string   `yaml:"abstract"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
}

// cogdocFile holds frontmatter plus the first content paragraph for richer embedding.
type cogdocFile struct {
	FM             cogdocFrontmatter
	FirstParagraph string // first non-empty paragraph after the closing ---
}

// parseCogdocFile extracts YAML frontmatter and the first content paragraph.
// Returns zero-value struct if no frontmatter found or parse fails.
func parseCogdocFile(path string) (cogdocFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return cogdocFile{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Increase scanner buffer for large files.
	scanner.Buffer(make([]byte, 64*1024), 512*1024)

	// Must start with ---
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return cogdocFile{}, nil // no frontmatter
	}

	var fmBuf strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		fmBuf.WriteString(line + "\n")
	}

	var fm cogdocFrontmatter
	if err := yaml.Unmarshal([]byte(fmBuf.String()), &fm); err != nil {
		return cogdocFile{}, fmt.Errorf("parse frontmatter %s: %w", path, err)
	}

	// Read the first non-empty, non-heading, non-YAML content paragraph.
	var paraLines []string
	var inPara bool
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			if inPara && len(paraLines) > 0 {
				break // end of first paragraph
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "---") ||
			strings.HasPrefix(trimmed, "```") ||
			strings.HasPrefix(trimmed, "|") {
			if inPara {
				break
			}
			continue
		}
		inPara = true
		paraLines = append(paraLines, trimmed)
		if len(paraLines) >= 4 { // cap at 4 lines
			break
		}
	}

	firstPara := strings.Join(paraLines, " ")

	return cogdocFile{FM: fm, FirstParagraph: firstPara}, nil
}

// parseFrontmatter is a compatibility wrapper that returns just the frontmatter.
func parseFrontmatter(path string) (cogdocFrontmatter, error) {
	cf, err := parseCogdocFile(path)
	return cf.FM, err
}

// queryText returns the text used for embedding from a cogdoc.
// Uses: title + (abstract or description) + first content paragraph.
// More context = better semantic signal for similarity matching.
func (cf cogdocFile) queryText() string {
	fm := cf.FM
	parts := []string{fm.Title}

	if fm.Abstract != "" {
		parts = append(parts, fm.Abstract)
	} else if fm.Description != "" {
		parts = append(parts, fm.Description)
	}

	if cf.FirstParagraph != "" {
		parts = append(parts, cf.FirstParagraph)
	}

	return strings.Join(parts, "\n\n")
}

// --- Corpus walker ---

// corpusEntry is a discovered cogdoc in the corpus.
type corpusEntry struct {
	FilePath string
	FM       cogdocFrontmatter
	Doc      cogdocFile // full parsed file including first paragraph
}

// walkCorpus walks each path in corpusPaths (relative to workspaceRoot),
// finds all *.cog.md files, and parses their content.
// Skips files without valid frontmatter or without an id field.
func walkCorpus(workspaceRoot string, corpusPaths []string) ([]corpusEntry, error) {
	if len(corpusPaths) == 0 {
		corpusPaths = []string{".cog/mem/"}
	}

	var entries []corpusEntry
	seen := map[string]bool{}

	for _, cp := range corpusPaths {
		root := filepath.Join(workspaceRoot, cp)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".cog.md") {
				return nil
			}
			if seen[path] {
				return nil
			}
			seen[path] = true

			doc, err := parseCogdocFile(path)
			if err != nil || doc.FM.ID == "" {
				return nil // skip unparseable or id-less files
			}

			rel, _ := filepath.Rel(workspaceRoot, path)
			entries = append(entries, corpusEntry{FilePath: rel, FM: doc.FM, Doc: doc})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk corpus %s: %w", cp, err)
		}
	}

	return entries, nil
}

// --- Top-level API ---

// SimilaritySearchConfig controls the similarity search.
type SimilaritySearchConfig struct {
	// WorkspaceRoot is the absolute path to the workspace.
	WorkspaceRoot string

	// OllamaEndpoint is the Ollama server URL. Defaults to http://localhost:11434.
	OllamaEndpoint string

	// EmbedModel is the embedding model. Defaults to bge-m3:latest.
	EmbedModel string

	// Threshold is the minimum cosine similarity score [0.0, 1.0].
	// Candidates below this score are excluded.
	Threshold float64

	// TopN is the maximum number of candidates to return.
	TopN int

	// CorpusPaths are workspace-relative directories to search.
	CorpusPaths []string

	// ExcludeFile is an optional workspace-relative path to exclude
	// (e.g., the file being authored so it doesn't match itself).
	ExcludeFile string
}

// SimilarityResult is a single result from SearchSimilar.
type SimilarityResult struct {
	CogdocID string
	FilePath string
	Title    string
	Type     string
	Score    float64
}

// SearchSimilar runs the grep-embed similarity search against the corpus.
// It embeds the query text, walks the corpus, embeds each cogdoc's title+abstract,
// computes cosine similarity, and returns the top-N candidates above threshold.
//
// This is the core primitive used by all three pipeline layers.
func SearchSimilar(ctx context.Context, cfg SimilaritySearchConfig, queryText string) ([]SimilarityResult, error) {
	if cfg.TopN <= 0 {
		cfg.TopN = 5
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 0.70
	}
	if cfg.WorkspaceRoot == "" {
		return nil, fmt.Errorf("cogdoc_review.SearchSimilar: WorkspaceRoot required")
	}

	// 1. Embed the query.
	queryEmb, err := embedQuery(ctx, cfg.OllamaEndpoint, cfg.EmbedModel, queryText)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	// 2. Walk the corpus.
	entries, err := walkCorpus(cfg.WorkspaceRoot, cfg.CorpusPaths)
	if err != nil {
		return nil, fmt.Errorf("walk corpus: %w", err)
	}

	// 3. Score each entry.
	type scoredEntry struct {
		entry corpusEntry
		score float32
	}
	var scored []scoredEntry

	for _, entry := range entries {
		// Skip the file being authored (if set).
		if cfg.ExcludeFile != "" && entry.FilePath == cfg.ExcludeFile {
			continue
		}

		// Use richer query text: title + abstract/description + first paragraph.
		docText := entry.Doc.queryText()
		if docText == "" {
			continue
		}

		docEmb, err := embedQuery(ctx, cfg.OllamaEndpoint, cfg.EmbedModel, docText)
		if err != nil {
			// Log and skip — don't fail the whole search on one bad embed.
			continue
		}

		score := cosineSimilarity(queryEmb, docEmb)
		if float64(score) >= cfg.Threshold {
			scored = append(scored, scoredEntry{entry: entry, score: score})
		}
	}

	// 4. Sort descending by score.
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// 5. Take top-N.
	if len(scored) > cfg.TopN {
		scored = scored[:cfg.TopN]
	}

	// 6. Build results.
	results := make([]SimilarityResult, len(scored))
	for i, s := range scored {
		results[i] = SimilarityResult{
			CogdocID: s.entry.FM.ID,
			FilePath: s.entry.FilePath,
			Title:    s.entry.FM.Title,
			Type:     s.entry.FM.Type,
			Score:    float64(s.score),
		}
	}

	return results, nil
}

// SearchSimilarFromProposal is a convenience wrapper that builds the query
// from a CogdocProposal and maps SimilarityResult to SimilarityCandidate.
func SearchSimilarFromProposal(
	ctx context.Context,
	cfg SimilaritySearchConfig,
	proposal reconcile.CogdocProposal,
) ([]reconcile.SimilarityCandidate, error) {
	results, err := SearchSimilar(ctx, cfg, proposal.QueryText())
	if err != nil {
		return nil, err
	}

	candidates := make([]reconcile.SimilarityCandidate, len(results))
	for i, r := range results {
		candidates[i] = reconcile.SimilarityCandidate{
			CogdocID: r.CogdocID,
			FilePath: r.FilePath,
			Title:    r.Title,
			Type:     r.Type,
			Score:    r.Score,
		}
	}
	return candidates, nil
}

// ProvenanceForProposal builds the ProvenanceRecord for a completed review.
func ProvenanceForProposal(
	proposal reconcile.CogdocProposal,
	candidates []reconcile.SimilarityCandidate,
	acknowledgments []reconcile.CandidateAcknowledgment,
	action reconcile.ReviewActionTaken,
) reconcile.ProvenanceRecord {
	return reconcile.ProvenanceRecord{
		PipelineVersion:    pipelineVersion,
		ProposedAt:         proposal.ProposedAt,
		ReviewedAt:         time.Now().UTC(),
		CandidatesSurfaced: len(candidates),
		Acknowledgments:    acknowledgments,
		ActionTaken:        action,
		SessionID:          proposal.SessionID,
	}
}

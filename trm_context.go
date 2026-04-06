// trm_context.go — TRM integration into the context assembly pipeline.
//
// Provides:
//   - OllamaEmbed: embed a query via the local Ollama /api/embeddings endpoint
//   - trmScoreDocs: score CogDoc candidates using MambaTRM + embedding index
//   - loadTRMAtStartup: one-shot loader called from main.go
//
// When the TRM is available, it replaces keyword+salience scoring as the
// primary CogDoc ranking signal. When unavailable (no weights, Ollama down,
// etc.), the pipeline falls back to the existing scoring transparently.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"os"
	"time"
)

// ollamaEmbedRequest is the request body for POST /api/embeddings.
type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// ollamaEmbedResponse is the response from POST /api/embeddings.
type ollamaEmbedResponse struct {
	Embedding []float64 `json:"embedding"`
}

// OllamaEmbed calls Ollama to embed a query string. Returns a 384-dim float32 vector.
// The endpoint and model are taken from config; defaults are localhost:11434 and nomic-embed-text.
func OllamaEmbed(ctx context.Context, cfg *Config, query string) ([]float32, error) {
	endpoint := cfg.OllamaEmbedEndpoint
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	model := cfg.OllamaEmbedModel
	if model == "" {
		model = "nomic-embed-text"
	}

	reqBody, err := json.Marshal(ollamaEmbedRequest{
		Model:  model,
		Prompt: "search_query: " + query,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	httpCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(httpCtx, http.MethodPost, endpoint+"/api/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create embed request: %w", err)
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

	// Convert float64 to float32, truncating to EMBED_DIM (Matryoshka).
	dim := len(embedResp.Embedding)
	if dim > 384 {
		dim = 384 // Matryoshka truncation to match index
	}
	vec := make([]float32, dim)
	for i := 0; i < dim; i++ {
		vec[i] = float32(embedResp.Embedding[i])
	}

	// L2-normalize for cosine similarity.
	vec = l2Normalize(vec)

	return vec, nil
}

// l2Normalize normalizes a vector to unit length.
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

// trmScoredDoc is a CogDoc candidate scored by TRM.
type trmScoredDoc struct {
	IndexResult IndexResult // from embedding index
	TRMScore    float32     // from MambaTRM.ScoreCandidates
}

// trmScoreDocs runs the full TRM scoring pipeline:
//  1. Embed the query via Ollama
//  2. CosineTopK pre-filtering from the embedding index
//  3. MambaTRM.Step with the light cone for this conversation
//  4. MambaTRM.ScoreCandidates over the pre-filtered candidates
//
// Returns sorted trmScoredDoc slice (highest TRM score first) and the updated light cone.
// On any error, returns nil — the caller should fall back to keyword scoring.
func trmScoreDocs(ctx context.Context, p *Process, query string, convID string, topK int) []trmScoredDoc {
	trm := p.TRM()
	idx := p.EmbeddingIndex()
	if trm == nil || idx == nil {
		return nil
	}

	// 1. Embed the query.
	queryEmb, err := OllamaEmbed(ctx, p.cfg, query)
	if err != nil {
		slog.Warn("trm: query embedding failed, falling back to keyword scoring", "err", err)
		return nil
	}

	// 2. Pre-filter with cosine similarity.
	if topK <= 0 {
		topK = 100
	}
	indices, candidateEmbs := idx.CosineTopKIndices(queryEmb, topK)
	if len(indices) == 0 {
		return nil
	}

	// 3. Step through the TRM with the light cone.
	lc := p.LightCones().Get(convID)
	contextVec, newLC := trm.Step(queryEmb, 0, lc) // event_type=0 (query)
	p.LightCones().Set(convID, newLC)

	// 4. Score candidates.
	scores := trm.ScoreCandidates(contextVec, candidateEmbs)

	// Build result set.
	results := make([]trmScoredDoc, len(indices))
	for i, embIdx := range indices {
		results[i] = trmScoredDoc{
			IndexResult: IndexResult{
				Index:     embIdx,
				Score:     cosineSim(queryEmb, idx.Embeddings[embIdx]),
				ChunkMeta: idx.Chunks[embIdx],
			},
			TRMScore: scores[i],
		}
	}

	// Residual scoring: blend TRM score with cosine baseline.
	// This ensures new content (high cosine) can surface even when
	// the TRM hasn't been trained on access patterns for it yet.
	// As the TRM trains on more data, its signal dominates.
	const trmWeight = 0.6
	const cosineWeight = 0.4

	for i := range results {
		results[i].TRMScore = trmWeight*results[i].TRMScore + cosineWeight*results[i].IndexResult.Score
	}

	// Sort by blended score descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].TRMScore > results[j].TRMScore
	})

	return results
}

// loadTRMAtStartup attempts to load TRM weights and embedding index.
// Returns (trm, index) or (nil, nil) if files don't exist or loading fails.
// Errors are logged as warnings — TRM is optional.
func loadTRMAtStartup(cfg *Config) (*MambaTRM, *EmbeddingIndex) {
	weightsPath := cfg.TRMWeightsPath
	embPath := cfg.TRMEmbeddingsPath
	chunksPath := cfg.TRMChunksPath

	// All three must be specified.
	if weightsPath == "" || embPath == "" || chunksPath == "" {
		slog.Info("trm: paths not configured, TRM scoring disabled")
		return nil, nil
	}

	// Check file existence before attempting to load.
	for _, path := range []string{weightsPath, embPath, chunksPath} {
		if _, err := os.Stat(path); err != nil {
			slog.Warn("trm: file not found, TRM scoring disabled", "path", path, "err", err)
			return nil, nil
		}
	}

	slog.Info("trm: loading model weights", "path", weightsPath)
	trm, err := LoadTRM(weightsPath)
	if err != nil {
		slog.Warn("trm: weight loading failed, TRM scoring disabled", "err", err)
		return nil, nil
	}
	slog.Info("trm: model loaded",
		"layers", trm.Config.NLayers,
		"d_model", trm.Config.DModel,
		"d_state", trm.Config.DState,
	)

	slog.Info("trm: loading embedding index", "emb", embPath, "chunks", chunksPath)
	idx, err := LoadEmbeddingIndex(embPath, chunksPath)
	if err != nil {
		slog.Warn("trm: embedding index loading failed, TRM scoring disabled", "err", err)
		return nil, nil
	}
	slog.Info("trm: embedding index loaded", "chunks", idx.Size(), "dim", idx.Dim)

	return trm, idx
}

// probe_scorer.go — Trained linear probe for context ranking (Phase E.1c)
//
// Loads weights from probe_weights.json (exported by train-probe.py) and
// scores (query, candidate) embedding pairs using a logistic regression.
//
// The probe takes concatenated [query_128 || candidate_128] = 256-dim input
// and outputs a relevance probability via sigmoid(weights · x + bias).

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
)

// ProbeScorer holds the trained linear probe weights.
type ProbeScorer struct {
	weights  []float32 // 256 weights (128 query + 128 candidate)
	bias     float32
	dims     int
	accuracy float64
}

// probeWeightsJSON is the JSON format exported by train-probe.py.
type probeWeightsJSON struct {
	Weights      []float64 `json:"weights"`
	Bias         float64   `json:"bias"`
	Dims         int       `json:"dims"`
	Accuracy     float64   `json:"accuracy"`
	Precision    float64   `json:"precision"`
	Recall       float64   `json:"recall"`
	TrainSamples int       `json:"train_samples"`
	TestSamples  int       `json:"test_samples"`
}

var (
	globalProbe     *ProbeScorer
	globalProbeOnce sync.Once
	globalProbeErr  error
)

// getProbeScorer returns the singleton probe scorer, loading weights on first call.
// Returns nil if weights file doesn't exist (probe not yet trained).
func getProbeScorer(workspaceRoot string) *ProbeScorer {
	globalProbeOnce.Do(func() {
		weightsPath := filepath.Join(workspaceRoot, ".cog", ".state", "probe_weights.json")
		globalProbe, globalProbeErr = loadProbeWeights(weightsPath)
		if globalProbeErr != nil {
			fmt.Fprintf(os.Stderr, "[probe] weights not available: %v\n", globalProbeErr)
			globalProbe = nil
		}
	})
	return globalProbe
}

// loadProbeWeights loads probe weights from a JSON file.
func loadProbeWeights(path string) (*ProbeScorer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var pw probeWeightsJSON
	if err := json.Unmarshal(data, &pw); err != nil {
		return nil, fmt.Errorf("parse probe weights: %w", err)
	}

	if pw.Dims != 256 {
		return nil, fmt.Errorf("expected 256 dims, got %d", pw.Dims)
	}

	if len(pw.Weights) != pw.Dims {
		return nil, fmt.Errorf("weight count %d != dims %d", len(pw.Weights), pw.Dims)
	}

	// Convert float64 weights to float32 for efficient scoring
	weights := make([]float32, len(pw.Weights))
	for i, w := range pw.Weights {
		weights[i] = float32(w)
	}

	return &ProbeScorer{
		weights:  weights,
		bias:     float32(pw.Bias),
		dims:     pw.Dims,
		accuracy: pw.Accuracy,
	}, nil
}

// ScoreProbe computes the relevance probability for a (query, candidate) pair.
// queryEmb and candidateEmb must both be 128-dim float32 vectors.
// Returns a probability in [0, 1] where 1 = highly relevant.
func (ps *ProbeScorer) ScoreProbe(queryEmb, candidateEmb []float32) float64 {
	if ps == nil || len(queryEmb) != 128 || len(candidateEmb) != 128 {
		return 0.5 // neutral fallback
	}

	// Compute dot product: weights · [query || candidate] + bias
	var dot float64
	for i := 0; i < 128; i++ {
		dot += float64(ps.weights[i]) * float64(queryEmb[i])
	}
	for i := 0; i < 128; i++ {
		dot += float64(ps.weights[128+i]) * float64(candidateEmb[i])
	}
	dot += float64(ps.bias)

	// Sigmoid
	return 1.0 / (1.0 + math.Exp(-dot))
}

// Accuracy returns the test set accuracy of the trained probe.
func (ps *ProbeScorer) Accuracy() float64 {
	if ps == nil {
		return 0
	}
	return ps.accuracy
}

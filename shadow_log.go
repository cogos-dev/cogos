// shadow_log.go — Dual-score shadow logging for the context engine.
//
// Records both heuristic (BM25+substance) and embedding similarity scores
// for every Tier 4 query, enabling offline analysis and eventual training
// of a learned ranker (Phase E).

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cogos-dev/cogos/sdk/constellation"
)

// ShadowLogEntry represents one Tier 4 query with dual scores for all candidates.
type ShadowLogEntry struct {
	Timestamp  string                `json:"timestamp"`
	QueryText  string                `json:"query_text"`
	QueryEmb128 []float32            `json:"query_embedding_128,omitempty"`
	Candidates []ShadowLogCandidate  `json:"candidates"`
}

// ShadowLogCandidate records all scoring methods for a single candidate document.
type ShadowLogCandidate struct {
	DocID               string  `json:"doc_id"`
	DocPath             string  `json:"doc_path"`
	HeuristicScore      float64 `json:"heuristic_score"`
	BM25Score           float64 `json:"bm25_score"`
	SubstanceScore      float64 `json:"substance_score"`
	EmbeddingSimilarity float64 `json:"embedding_similarity"`
	ProbeScore          float64 `json:"probe_score,omitempty"` // Phase E: trained probe score
	WasIncluded         bool    `json:"was_included"`
	TokensUsed          int     `json:"tokens_used,omitempty"`
	DetailLevel         string  `json:"detail_level,omitempty"` // full, section, metadata
}

// shadowLogger writes JSONL entries to the shadow log file.
type shadowLogger struct {
	mu   sync.Mutex
	path string
}

var (
	globalShadowLogger *shadowLogger
	shadowLoggerOnce   sync.Once
)

// getShadowLogger returns the singleton shadow logger.
func getShadowLogger(workspaceRoot string) *shadowLogger {
	shadowLoggerOnce.Do(func() {
		cfg := LoadTAAConfig(workspaceRoot)
		if !cfg.ShadowLog.Enabled {
			return
		}

		logPath := cfg.ShadowLog.Path
		if !filepath.IsAbs(logPath) {
			logPath = filepath.Join(workspaceRoot, logPath)
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
			fmt.Fprintf(os.Stderr, "[shadow-log] failed to create directory: %v\n", err)
			return
		}

		globalShadowLogger = &shadowLogger{path: logPath}
	})
	return globalShadowLogger
}

// Log appends a shadow log entry as a JSONL line.
func (sl *shadowLogger) Log(entry ShadowLogEntry) error {
	if sl == nil {
		return nil
	}

	sl.mu.Lock()
	defer sl.mu.Unlock()

	f, err := os.OpenFile(sl.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open shadow log: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal shadow entry: %w", err)
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write shadow entry: %w", err)
	}

	return nil
}

// WriteShadowLog creates and writes a shadow log entry from scored candidates.
func WriteShadowLog(workspaceRoot, queryText string, queryEmb128 []float32,
	candidates []constellation.NodeWithScore, includedCount int) {

	sl := getShadowLogger(workspaceRoot)
	if sl == nil {
		return
	}

	entry := ShadowLogEntry{
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		QueryText:   queryText,
		QueryEmb128: queryEmb128,
	}

	for i, c := range candidates {
		sc := ShadowLogCandidate{
			DocID:               c.Node.ID,
			DocPath:             c.Node.Path,
			HeuristicScore:      c.CombinedScore,
			BM25Score:           c.BM25Score,
			SubstanceScore:      c.SubstanceScore,
			EmbeddingSimilarity: c.EmbeddingSimilarity,
			ProbeScore:          c.ProbeScore,
			WasIncluded:         i < includedCount,
		}

		// Determine detail level based on rank position
		switch {
		case i < 5:
			sc.DetailLevel = "full"
		case i < 10:
			sc.DetailLevel = "section"
		default:
			sc.DetailLevel = "metadata"
		}

		entry.Candidates = append(entry.Candidates, sc)
	}

	if err := sl.Log(entry); err != nil {
		fmt.Fprintf(os.Stderr, "[shadow-log] write error: %v\n", err)
	}
}

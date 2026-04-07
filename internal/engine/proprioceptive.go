// proprioceptive.go — Proprioceptive logging for TRM prediction-vs-reality tracking.
//
// After each chat request, the TRM predicts which chunks will be referenced.
// This logger records predictions alongside actual references extracted from
// the response, enabling continuous calibration of the light cone.
//
// Log format: JSONL at .cog/run/proprioceptive.jsonl
package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ProprioceptiveEntry is a single prediction-vs-reality log entry.
type ProprioceptiveEntry struct {
	Timestamp   string           `json:"timestamp"`
	Event       string           `json:"event,omitempty"`
	Provider    string           `json:"provider,omitempty"`
	ToolName    string           `json:"tool_name,omitempty"`
	ToolCallID  string           `json:"tool_call_id,omitempty"`
	ToolArgs    string           `json:"tool_args,omitempty"`
	Reason      string           `json:"reason,omitempty"`
	Query       string           `json:"query"`
	Predicted   []PredictedChunk `json:"predicted"`
	Actual      []string         `json:"actual"`
	Hits        int              `json:"hits"`
	Delta       float64          `json:"delta"`
	ResponseLen int              `json:"response_len"`
}

// PredictedChunk is a single TRM prediction.
type PredictedChunk struct {
	Path         string  `json:"path"`
	SectionTitle string  `json:"section_title,omitempty"`
	Score        float32 `json:"score"`
}

// ProprioceptiveLogger writes proprioceptive entries to a JSONL file.
type ProprioceptiveLogger struct {
	mu      sync.Mutex
	logPath string
}

// NewProprioceptiveLogger creates a logger writing to the given path.
// The parent directory is created if it does not exist.
func NewProprioceptiveLogger(logPath string) *ProprioceptiveLogger {
	dir := filepath.Dir(logPath)
	_ = os.MkdirAll(dir, 0o755)
	return &ProprioceptiveLogger{logPath: logPath}
}

// Log appends a proprioceptive entry to the JSONL log.
func (p *ProprioceptiveLogger) Log(entry ProprioceptiveEntry) {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	f, err := os.OpenFile(p.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	_, _ = f.Write(data)
	_, _ = f.WriteString("\n")
}

// ComputeEntry builds a ProprioceptiveEntry from TRM predictions and a response.
func ComputeEntry(query string, predicted []PredictedChunk, response string) ProprioceptiveEntry {
	actual := ExtractReferencedPaths(response)

	// Compute hits: how many predicted paths appear in actual references
	actualSet := make(map[string]bool, len(actual))
	for _, a := range actual {
		actualSet[a] = true
		// Also match basename
		actualSet[filepath.Base(a)] = true
	}

	hits := 0
	for _, pred := range predicted {
		if actualSet[pred.Path] || actualSet[filepath.Base(pred.Path)] {
			hits++
		}
	}

	// Delta: prediction accuracy as fraction
	var delta float64
	if len(predicted) > 0 {
		delta = float64(hits) / float64(len(predicted))
	}

	return ProprioceptiveEntry{
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Query:       truncateString(query, 200),
		Predicted:   predicted,
		Actual:      actual,
		Hits:        hits,
		Delta:       delta,
		ResponseLen: len(response),
	}
}

// ── Path extraction from response text ───────────────────────────────────────

// Patterns that match file references in markdown-formatted LLM responses.
var pathPatterns = []*regexp.Regexp{
	// cog:// URIs
	regexp.MustCompile(`cog://[a-zA-Z0-9_./-]+`),
	// .cog/ relative paths
	regexp.MustCompile(`\.cog/[a-zA-Z0-9_./-]+\.(?:md|cog\.md|yaml|json)`),
	// Backtick-quoted paths ending in common extensions
	regexp.MustCompile("`([a-zA-Z0-9_./-]+\\.(?:md|cog\\.md|yaml|json|go|py))`"),
	// Bare relative paths with at least one directory separator
	regexp.MustCompile(`(?:^|\s)([a-zA-Z0-9_.-]+/[a-zA-Z0-9_./-]+\.(?:md|cog\.md))`),
}

// ExtractReferencedPaths extracts file paths from an LLM response.
func ExtractReferencedPaths(response string) []string {
	seen := make(map[string]bool)
	var paths []string

	for _, pat := range pathPatterns {
		matches := pat.FindAllStringSubmatch(response, -1)
		for _, m := range matches {
			// Use the last capture group if present, otherwise full match
			p := m[0]
			if len(m) > 1 && m[1] != "" {
				p = m[1]
			}

			// Normalize: strip cog:// prefix for comparison
			normalized := strings.TrimPrefix(p, "cog://")
			// Remove trailing punctuation that may have been captured
			normalized = strings.TrimRight(normalized, ".,;:!?)")

			if !seen[normalized] && len(normalized) > 3 {
				seen[normalized] = true
				paths = append(paths, normalized)
			}
		}
	}
	return paths
}

// truncateString truncates s to maxLen runes, appending "..." if truncated.
func truncateString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

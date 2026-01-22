package sdk

import (
	"strings"
	"unicode/utf8"
)

// Token estimation utilities for budget management.
//
// These provide rough estimates suitable for context budget allocation.
// Actual token counts vary by model and tokenizer.

// TokensPerChar is the approximate characters per token.
// Claude uses ~4 characters per token on average for English text.
const TokensPerChar = 4

// EstimateTokens estimates the token count for text.
// Uses the 4 chars per token heuristic.
//
// This is a rough estimate - actual tokenization varies by:
//   - Model (different tokenizers)
//   - Language (CJK uses more tokens)
//   - Content type (code vs prose)
//
// For budget allocation, this is sufficiently accurate.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// Use rune count for Unicode correctness
	charCount := utf8.RuneCountInString(text)
	return (charCount + TokensPerChar - 1) / TokensPerChar // Round up
}

// TruncateToTokens truncates content to fit a token budget.
// Returns the truncated content and whether truncation occurred.
//
// The truncation is approximate - it may be slightly under budget.
// Attempts to truncate at word boundaries for readability.
func TruncateToTokens(content string, maxTokens int) (string, bool) {
	if maxTokens <= 0 {
		return content, false
	}

	estimated := EstimateTokens(content)
	if estimated <= maxTokens {
		return content, false
	}

	// Calculate approximate character limit
	maxChars := maxTokens * TokensPerChar

	// Find a good truncation point (word boundary)
	if maxChars >= len(content) {
		return content, false
	}

	// Start from maxChars and look for word boundary
	truncPoint := maxChars
	for truncPoint > 0 && truncPoint < len(content) && !isWordBoundary(content, truncPoint) {
		truncPoint--
	}

	// If we went too far back, just use maxChars
	if truncPoint < maxChars/2 {
		truncPoint = maxChars
	}

	// Ensure we don't split a UTF-8 sequence
	for truncPoint > 0 && !utf8.RuneStart(content[truncPoint]) {
		truncPoint--
	}

	return content[:truncPoint] + "...", true
}

// isWordBoundary checks if position is at a word boundary.
func isWordBoundary(s string, pos int) bool {
	if pos <= 0 || pos >= len(s) {
		return true
	}
	c := s[pos]
	return c == ' ' || c == '\n' || c == '\t' || c == '.' || c == ',' || c == ';'
}

// AllocateBudget distributes a token budget across items.
// Items with higher priority get larger allocations.
//
// The allocation uses a proportional scheme:
//   - Higher priority items get (1/priority) weight
//   - Weights are normalized to sum to totalBudget
func AllocateBudget(totalBudget int, priorities map[string]int) map[string]int {
	if len(priorities) == 0 || totalBudget <= 0 {
		return make(map[string]int)
	}

	// Calculate weights (inverse of priority)
	weights := make(map[string]float64)
	var totalWeight float64
	for name, priority := range priorities {
		if priority <= 0 {
			priority = 1
		}
		weight := 1.0 / float64(priority)
		weights[name] = weight
		totalWeight += weight
	}

	// Allocate proportionally
	allocations := make(map[string]int)
	remaining := totalBudget
	for name, weight := range weights {
		allocation := int(float64(totalBudget) * weight / totalWeight)
		allocations[name] = allocation
		remaining -= allocation
	}

	// Distribute remaining tokens to highest priority
	if remaining > 0 {
		var highestPriority string
		highestPriorityValue := 999999
		for name, priority := range priorities {
			if priority < highestPriorityValue {
				highestPriorityValue = priority
				highestPriority = name
			}
		}
		if highestPriority != "" {
			allocations[highestPriority] += remaining
		}
	}

	return allocations
}

// AllocateBudgetByPercentage distributes budget using percentages.
// Percentages should sum to 100 for expected behavior.
func AllocateBudgetByPercentage(totalBudget int, percentages map[string]int) map[string]int {
	if len(percentages) == 0 || totalBudget <= 0 {
		return make(map[string]int)
	}

	allocations := make(map[string]int)
	remaining := totalBudget

	for name, pct := range percentages {
		allocation := totalBudget * pct / 100
		allocations[name] = allocation
		remaining -= allocation
	}

	// Distribute rounding remainder to first item
	if remaining > 0 {
		for name := range allocations {
			allocations[name] += remaining
			break
		}
	}

	return allocations
}

// CompactContent attempts to reduce content size while preserving meaning.
// Uses simple heuristics - not a true summarizer.
//
// Strategies:
//   - Remove excessive whitespace
//   - Collapse repeated blank lines
//   - Trim leading/trailing whitespace from lines
func CompactContent(content string) string {
	if content == "" {
		return ""
	}

	// Split into lines
	lines := strings.Split(content, "\n")
	result := make([]string, 0, len(lines))

	prevBlank := false
	for _, line := range lines {
		// Trim whitespace
		trimmed := strings.TrimSpace(line)

		// Collapse multiple blank lines into one
		if trimmed == "" {
			if !prevBlank {
				result = append(result, "")
				prevBlank = true
			}
			continue
		}

		prevBlank = false
		result = append(result, trimmed)
	}

	return strings.Join(result, "\n")
}

// ContentStats returns statistics about content.
type ContentStats struct {
	// Chars is the character count (runes).
	Chars int `json:"chars"`

	// Words is the approximate word count.
	Words int `json:"words"`

	// Lines is the line count.
	Lines int `json:"lines"`

	// Tokens is the estimated token count.
	Tokens int `json:"tokens"`
}

// AnalyzeContent returns statistics about the content.
func AnalyzeContent(content string) *ContentStats {
	if content == "" {
		return &ContentStats{}
	}

	chars := utf8.RuneCountInString(content)
	lines := strings.Count(content, "\n") + 1
	words := len(strings.Fields(content))
	tokens := EstimateTokens(content)

	return &ContentStats{
		Chars:  chars,
		Words:  words,
		Lines:  lines,
		Tokens: tokens,
	}
}

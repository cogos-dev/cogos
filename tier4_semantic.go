// Tier 4: Semantic Memory (Constellation)
//
// This tier queries the constellation knowledge graph for documents relevant
// to the current conversation anchor and goal. It provides semantic context
// from across all cogdocs in the workspace.
//
// Configuration is loaded from .cog/config/taa.yaml

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/cogos-dev/cogos/sdk/constellation"
)

// QueryConstellation queries the constellation graph for relevant knowledge.
//
// It uses the anchor and goal from Tier 2 to extract keywords and search
// the FTS5-indexed cogdoc collection. Results are formatted for context injection.
//
// Configuration is loaded from .cog/config/taa.yaml and controls:
//   - Max candidates to fetch before filtering
//   - Max results to include in context
//   - Substance filtering thresholds
//   - Ranking weights (BM25 vs substance)
//
// Parameters:
//   - workspaceRoot: Absolute path to workspace root
//   - anchor: Conversation anchor from Tier 2 (current topic)
//   - goal: Conversation goal from Tier 2 (what user wants to achieve)
//   - budget: Token budget for this tier
//
// Returns:
//   - Formatted context string with relevant knowledge
//   - error if constellation fails (nil if empty results)
func QueryConstellation(workspaceRoot, anchor, goal string, budget int) (string, error) {
	// Load TAA configuration
	cfg := LoadTAAConfig(workspaceRoot)

	if cfg.Debug.TraceQueries {
		fmt.Fprintf(os.Stderr, "[TAA] Tier 4: querying with anchor=%q goal=%q budget=%d\n", anchor, goal, budget)
	}

	// Open constellation database
	c, err := constellation.Open(workspaceRoot)
	if err != nil {
		// If constellation doesn't exist yet, return empty (not an error)
		fmt.Fprintf(os.Stderr, "[TAA] Tier 4: constellation not available: %v\n", err)
		return "", nil
	}
	// Fix 4: Tier 4 Nil Check
	// Safety check to prevent panic on nil constellation
	if c == nil {
		fmt.Fprintf(os.Stderr, "[TAA] Tier 4: constellation returned nil\n")
		return "", nil
	}
	defer c.Close()

	// If no anchor/goal, skip (no query basis)
	if anchor == "" && goal == "" {
		return "", nil
	}

	var nodes []constellation.Node

	// Use substance-aware query if enabled
	if cfg.Substance.Enabled {
		filter := constellation.SubstanceFilterConfig{
			MinSubstanceRatio: cfg.Substance.MinRatio,
			PreferLeafNodes:   cfg.Substance.PreferLeafNodes,
			LeafThreshold:     cfg.Substance.LeafSubstanceThreshold,
			LeafMaxRefs:       cfg.Substance.LeafMaxRefs,
			BM25Weight:        cfg.Ranking.BM25Weight,
			SubstanceWeight:   cfg.Ranking.SubstanceWeight,
		}

		if cfg.Debug.TraceSubstance {
			fmt.Fprintf(os.Stderr, "[TAA] Tier 4: substance filter enabled (min_ratio=%.2f, leaf_boost=%v)\n",
				filter.MinSubstanceRatio, filter.PreferLeafNodes)
		}

		nodes, err = c.QueryRelevantWithSubstance(anchor, goal,
			cfg.Semantic.MaxCandidates, cfg.Semantic.MaxResults, filter)
	} else {
		// Fallback to basic query
		nodes, err = c.QueryRelevant(anchor, goal, cfg.Semantic.MaxResults)
	}

	if err != nil {
		return "", fmt.Errorf("constellation query failed: %w", err)
	}

	// If no results, return empty
	if len(nodes) == 0 {
		if cfg.Debug.TraceQueries {
			fmt.Fprintf(os.Stderr, "[TAA] Tier 4: no results found\n")
		}
		return "", nil
	}

	if cfg.Debug.TraceQueries {
		fmt.Fprintf(os.Stderr, "[TAA] Tier 4: found %d results\n", len(nodes))
	}

	// Format nodes for context
	var sb strings.Builder
	sb.WriteString("# Relevant Knowledge (Constellation)\n\n")
	sb.WriteString("The following documents from the workspace knowledge graph are relevant to your query:\n\n")

	charsPerToken := cfg.Semantic.CharsPerToken
	if charsPerToken <= 0 {
		charsPerToken = 4 // Fallback
	}
	currentTokens := len(sb.String()) / charsPerToken

	for _, node := range nodes {
		// Format node with configured truncation
		nodeStr := formatNodeWithConfig(node, cfg.Semantic.NodeTruncateChars)
		nodeTokens := len(nodeStr) / charsPerToken

		// Check budget
		if currentTokens+nodeTokens > budget {
			if cfg.Debug.TraceQueries {
				fmt.Fprintf(os.Stderr, "[TAA] Tier 4: budget exceeded, stopping at %d nodes\n", len(nodes))
			}
			break
		}

		sb.WriteString(nodeStr)
		sb.WriteString("\n\n")
		currentTokens += nodeTokens
	}

	return sb.String(), nil
}

// formatNodeWithConfig formats a constellation node with configurable truncation.
func formatNodeWithConfig(node constellation.Node, maxContentChars int) string {
	var sb strings.Builder

	// Header with metadata
	sb.WriteString(fmt.Sprintf("## %s\n\n", node.Title))

	// Metadata line
	meta := []string{
		fmt.Sprintf("Type: %s", node.Type),
	}
	if node.Sector != "" {
		meta = append(meta, fmt.Sprintf("Sector: %s", node.Sector))
	}
	if node.Status != "" {
		meta = append(meta, fmt.Sprintf("Status: %s", node.Status))
	}
	sb.WriteString(fmt.Sprintf("*%s*\n\n", strings.Join(meta, " | ")))

	// Content (truncate if too long)
	content := node.Content
	if maxContentChars <= 0 {
		maxContentChars = 2000 // Fallback
	}
	if len(content) > maxContentChars {
		content = content[:maxContentChars] + "\n\n...(truncated)"
	}

	sb.WriteString(content)

	return sb.String()
}

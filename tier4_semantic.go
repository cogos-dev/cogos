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
	"sync"

	"github.com/cogos-dev/cogos/sdk/constellation"
)

// embedClientSingleton holds the shared embed client (nil if embedding disabled).
var (
	embedClientSingleton *constellation.EmbedClient
	embedClientOnce      sync.Once
)

// getEmbedClient returns the shared embed client, creating it on first call.
// Returns nil if embedding is not enabled in config.
func getEmbedClient(workspaceRoot string) *constellation.EmbedClient {
	embedClientOnce.Do(func() {
		cfg := LoadTAAConfig(workspaceRoot)
		if !cfg.Embedding.Enabled {
			return
		}

		embedClientSingleton = constellation.NewEmbedClient(constellation.EmbedConfig{
			Enabled:        cfg.Embedding.Enabled,
			ServerSocket:   cfg.Embedding.ServerSocket,
			ServerHTTP:     cfg.Embedding.ServerHTTP,
			DimsFull:       cfg.Embedding.DimsFull,
			DimsCompressed: cfg.Embedding.DimsCompressed,
			TimeoutMs:      cfg.Embedding.TimeoutMs,
		})
	})

	return embedClientSingleton
}

// QueryConstellation queries the constellation graph for relevant knowledge.
//
// It uses the anchor and goal from Tier 2 to extract keywords and search
// the FTS5-indexed cogdoc collection. Results are formatted for context injection.
//
// When embedding is enabled, it also:
//   - Embeds the query text for cosine similarity scoring
//   - Records both heuristic and embedding scores in shadow log
//   - Uses variable-resolution loading to fit more docs in budget
func QueryConstellation(workspaceRoot, anchor, goal string, budget int) (string, error) {
	cfg := LoadTAAConfig(workspaceRoot)

	if cfg.Debug.TraceQueries {
		fmt.Fprintf(os.Stderr, "[TAA] Tier 4: querying with anchor=%q goal=%q budget=%d\n", anchor, goal, budget)
	}

	c, err := getConstellation()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[TAA] Tier 4: constellation not available: %v\n", err)
		return "", nil
	}
	if c == nil {
		fmt.Fprintf(os.Stderr, "[TAA] Tier 4: constellation returned nil\n")
		return "", nil
	}

	if anchor == "" && goal == "" {
		return "", nil
	}

	// --- Query-time embedding (C.1a) ---
	var queryEmb128 []float32
	embedClient := getEmbedClient(workspaceRoot)
	if embedClient != nil {
		queryText := anchor + " " + goal
		result, err := embedClient.EmbedOne(queryText, "search_query")
		if err != nil {
			fmt.Fprintf(os.Stderr, "[TAA] Tier 4: query embedding failed (continuing with heuristic): %v\n", err)
		} else {
			queryEmb128 = result.Embedding128
		}
	}

	// --- Scoring ---
	var scoredCandidates []constellation.NodeWithScore
	var nodes []constellation.Node

	if cfg.Embedding.Enabled && queryEmb128 != nil {
		// Dual scoring path: heuristic + embedding similarity
		filter := constellation.SubstanceFilterConfig{
			MinSubstanceRatio: cfg.Substance.MinRatio,
			PreferLeafNodes:   cfg.Substance.PreferLeafNodes,
			LeafThreshold:     cfg.Substance.LeafSubstanceThreshold,
			LeafMaxRefs:       cfg.Substance.LeafMaxRefs,
			BM25Weight:        cfg.Ranking.BM25Weight,
			SubstanceWeight:   cfg.Ranking.SubstanceWeight,
		}

		// Fetch more candidates for variable-resolution loading
		maxResults := cfg.Semantic.MaxResults
		if maxResults < 20 {
			maxResults = 20 // C.2: variable resolution needs more candidates
		}

		scoredCandidates, err = c.QueryRelevantWithEmbedding(
			anchor, goal,
			cfg.Semantic.MaxCandidates, maxResults, filter, queryEmb128,
		)
		if err != nil {
			return "", fmt.Errorf("constellation query failed: %w", err)
		}

		// Phase E: Apply probe scores and blend if trained probe is available
		blendWeight := cfg.Ranking.ProbeBlendWeight
		if blendWeight > 0 {
			probe := getProbeScorer(workspaceRoot)
			if probe != nil {
				for i := range scoredCandidates {
					docEmb128 := scoredCandidates[i].Embedding128
					if len(docEmb128) == 128 {
						probeScore := probe.ScoreProbe(queryEmb128, docEmb128)
						scoredCandidates[i].ProbeScore = probeScore

						// Blend: combined = (1-w)*heuristic + w*probe
						scoredCandidates[i].CombinedScore =
							(1-blendWeight)*scoredCandidates[i].CombinedScore +
								blendWeight*probeScore
					}
				}

				// Re-sort by blended score
				constellation.SortNodesByScore(scoredCandidates)

				if cfg.Debug.TraceQueries {
					fmt.Fprintf(os.Stderr, "[TAA] Tier 4: probe blend weight=%.2f, accuracy=%.3f\n",
						blendWeight, probe.Accuracy())
				}
			}
		}

		// Extract nodes for formatting
		nodes = make([]constellation.Node, len(scoredCandidates))
		for i, sc := range scoredCandidates {
			nodes[i] = sc.Node
		}
	} else if cfg.Substance.Enabled {
		// Substance-aware heuristic path (original)
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
		// Basic FTS path
		nodes, err = c.QueryRelevant(anchor, goal, cfg.Semantic.MaxResults)
	}

	if err != nil {
		return "", fmt.Errorf("constellation query failed: %w", err)
	}

	if len(nodes) == 0 {
		if cfg.Debug.TraceQueries {
			fmt.Fprintf(os.Stderr, "[TAA] Tier 4: no results found\n")
		}
		return "", nil
	}

	if cfg.Debug.TraceQueries {
		fmt.Fprintf(os.Stderr, "[TAA] Tier 4: found %d results\n", len(nodes))
	}

	// --- Format nodes with variable resolution (C.2) ---
	var sb strings.Builder
	sb.WriteString("# Relevant Knowledge (Constellation)\n\n")
	sb.WriteString("The following documents from the workspace knowledge graph are relevant to your query:\n\n")

	charsPerToken := cfg.Semantic.CharsPerToken
	if charsPerToken <= 0 {
		charsPerToken = 4
	}
	currentTokens := len(sb.String()) / charsPerToken
	includedCount := 0

	for i, node := range nodes {
		// Variable-resolution loading (C.2):
		// Rank 0-4:  full content
		// Rank 5-9:  truncated to best section
		// Rank 10+:  metadata + section titles only
		var nodeStr string

		if cfg.Embedding.Enabled {
			switch {
			case i < 5:
				nodeStr = formatNodeWithConfig(node, cfg.Semantic.NodeTruncateChars)
			case i < 10:
				nodeStr = formatNodeSection(node, cfg.Semantic.NodeTruncateChars/2)
			default:
				nodeStr = formatNodeMetadataOnly(node)
			}
		} else {
			nodeStr = formatNodeWithConfig(node, cfg.Semantic.NodeTruncateChars)
		}

		nodeTokens := len(nodeStr) / charsPerToken

		if currentTokens+nodeTokens > budget {
			if cfg.Debug.TraceQueries {
				fmt.Fprintf(os.Stderr, "[TAA] Tier 4: budget exceeded at rank %d, included %d nodes\n", i, includedCount)
			}
			break
		}

		sb.WriteString(nodeStr)
		sb.WriteString("\n\n")
		currentTokens += nodeTokens
		includedCount++
	}

	// --- Shadow log (C.1c) ---
	if cfg.ShadowLog.Enabled && scoredCandidates != nil {
		WriteShadowLog(workspaceRoot, anchor+" "+goal, queryEmb128, scoredCandidates, includedCount)
	}

	return sb.String(), nil
}

// QueryConstellationWithIris queries the constellation with iris-aware resolution.
//
// Unlike QueryConstellation which uses rank-based cutoffs (i < 5: full, i < 10: section),
// this function uses score-based thresholds that slide with iris pressure:
// - Wide iris (low pressure) → lower thresholds → more content at full resolution
// - Narrow iris (high pressure) → higher thresholds → only peak signals at full resolution
//
// The scored candidates are preserved through the formatting loop (not stripped).
func QueryConstellationWithIris(workspaceRoot, anchor, goal string, budget int, irisPressure float64) (string, error) {
	cfg := LoadTAAConfig(workspaceRoot)

	if cfg.Debug.TraceQueries {
		fmt.Fprintf(os.Stderr, "[TAA-iris] Tier 4: querying with anchor=%q goal=%q budget=%d pressure=%.1f%%\n",
			anchor, goal, budget, irisPressure*100)
	}

	c, err := getConstellation()
	if err != nil || c == nil {
		return "", nil
	}
	if anchor == "" && goal == "" {
		return "", nil
	}

	// Query-time embedding
	var queryEmb128 []float32
	embedClient := getEmbedClient(workspaceRoot)
	if embedClient != nil {
		queryText := anchor + " " + goal
		result, err := embedClient.EmbedOne(queryText, "search_query")
		if err != nil {
			fmt.Fprintf(os.Stderr, "[TAA-iris] Tier 4: query embedding failed: %v\n", err)
		} else {
			queryEmb128 = result.Embedding128
		}
	}

	// Score candidates
	var scoredCandidates []constellation.NodeWithScore

	if cfg.Embedding.Enabled && queryEmb128 != nil {
		filter := constellation.SubstanceFilterConfig{
			MinSubstanceRatio: cfg.Substance.MinRatio,
			PreferLeafNodes:   cfg.Substance.PreferLeafNodes,
			LeafThreshold:     cfg.Substance.LeafSubstanceThreshold,
			LeafMaxRefs:       cfg.Substance.LeafMaxRefs,
			BM25Weight:        cfg.Ranking.BM25Weight,
			SubstanceWeight:   cfg.Ranking.SubstanceWeight,
		}

		maxResults := cfg.Semantic.MaxResults
		if maxResults < 20 {
			maxResults = 20
		}

		scoredCandidates, err = c.QueryRelevantWithEmbedding(
			anchor, goal,
			cfg.Semantic.MaxCandidates, maxResults, filter, queryEmb128,
		)
		if err != nil {
			return "", fmt.Errorf("constellation query failed: %w", err)
		}

		// Apply probe blend if available
		blendWeight := cfg.Ranking.ProbeBlendWeight
		if blendWeight > 0 {
			probe := getProbeScorer(workspaceRoot)
			if probe != nil {
				for i := range scoredCandidates {
					docEmb128 := scoredCandidates[i].Embedding128
					if len(docEmb128) == 128 {
						probeScore := probe.ScoreProbe(queryEmb128, docEmb128)
						scoredCandidates[i].ProbeScore = probeScore
						scoredCandidates[i].CombinedScore =
							(1-blendWeight)*scoredCandidates[i].CombinedScore +
								blendWeight*probeScore
					}
				}
				constellation.SortNodesByScore(scoredCandidates)
			}
		}
	} else {
		// Fall back to heuristic-only path
		return QueryConstellation(workspaceRoot, anchor, goal, budget)
	}

	if len(scoredCandidates) == 0 {
		return "", nil
	}

	// --- Score-based resolution thresholds ---
	// These slide with iris pressure: high pressure raises thresholds.
	//
	// At low pressure (0.0):  fullThreshold=0.3, sectionThreshold=0.15
	// At high pressure (0.9): fullThreshold=0.75, sectionThreshold=0.45
	//
	// The top score anchors the thresholds — they're relative to the best hit.
	topScore := scoredCandidates[0].CombinedScore
	if topScore <= 0 {
		topScore = 1.0
	}

	// Base thresholds (fraction of top score)
	fullBase := 0.6    // 60% of top score gets full content
	sectionBase := 0.3 // 30% of top score gets section content

	// Pressure scaling: as pressure increases, thresholds rise toward 1.0
	pressureScale := irisPressure * irisPressure // Quadratic — gentle at low pressure, aggressive at high
	fullThreshold := topScore * (fullBase + (1.0-fullBase)*pressureScale)
	sectionThreshold := topScore * (sectionBase + (1.0-sectionBase)*pressureScale)

	if cfg.Debug.TraceQueries {
		fmt.Fprintf(os.Stderr, "[TAA-iris] Tier 4: score thresholds — full>=%.3f section>=%.3f (top=%.3f pressure=%.1f%%)\n",
			fullThreshold, sectionThreshold, topScore, irisPressure*100)
	}

	// Format with score-based variable resolution
	var sb strings.Builder
	sb.WriteString("# Relevant Knowledge (Constellation)\n\n")

	charsPerToken := cfg.Semantic.CharsPerToken
	if charsPerToken <= 0 {
		charsPerToken = 4
	}
	currentTokens := len(sb.String()) / charsPerToken
	includedCount := 0

	for _, sc := range scoredCandidates {
		var nodeStr string

		switch {
		case sc.CombinedScore >= fullThreshold:
			nodeStr = formatNodeWithConfig(sc.Node, cfg.Semantic.NodeTruncateChars)
		case sc.CombinedScore >= sectionThreshold:
			nodeStr = formatNodeSection(sc.Node, cfg.Semantic.NodeTruncateChars/2)
		default:
			nodeStr = formatNodeMetadataOnly(sc.Node)
		}

		nodeTokens := len(nodeStr) / charsPerToken
		if currentTokens+nodeTokens > budget {
			if cfg.Debug.TraceQueries {
				fmt.Fprintf(os.Stderr, "[TAA-iris] Tier 4: budget exceeded, included %d nodes\n", includedCount)
			}
			break
		}

		sb.WriteString(nodeStr)
		sb.WriteString("\n\n")
		currentTokens += nodeTokens
		includedCount++
	}

	// Shadow log
	if cfg.ShadowLog.Enabled {
		WriteShadowLog(workspaceRoot, anchor+" "+goal, queryEmb128, scoredCandidates, includedCount)
	}

	return sb.String(), nil
}

// formatNodeWithConfig formats a constellation node with configurable truncation.
func formatNodeWithConfig(node constellation.Node, maxContentChars int) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## %s\n\n", node.Title))

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

	content := node.Content
	if maxContentChars <= 0 {
		maxContentChars = 2000
	}
	if len(content) > maxContentChars {
		content = content[:maxContentChars] + "\n\n...(truncated)"
	}

	sb.WriteString(content)
	return sb.String()
}

// formatNodeSection formats a node showing only the most relevant section.
// For variable-resolution loading (C.2), used for rank 5-9 documents.
func formatNodeSection(node constellation.Node, maxChars int) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## %s\n\n", node.Title))

	meta := []string{
		fmt.Sprintf("Type: %s", node.Type),
	}
	if node.Sector != "" {
		meta = append(meta, fmt.Sprintf("Sector: %s", node.Sector))
	}
	sb.WriteString(fmt.Sprintf("*%s*\n\n", strings.Join(meta, " | ")))

	// Extract the longest section (heuristic for "most substantive")
	sections := splitIntoSections(node.Content)
	if len(sections) == 0 {
		// No sections — just truncate
		content := node.Content
		if len(content) > maxChars {
			content = content[:maxChars] + "\n...(section excerpt)"
		}
		sb.WriteString(content)
		return sb.String()
	}

	// Pick the longest section (most content = most substantive)
	best := sections[0]
	for _, s := range sections[1:] {
		if len(s) > len(best) {
			best = s
		}
	}

	if len(best) > maxChars {
		best = best[:maxChars] + "\n...(section excerpt)"
	}

	sb.WriteString(best)
	return sb.String()
}

// formatNodeMetadataOnly formats a node showing only title, type, and section headers.
// For variable-resolution loading (C.2), used for rank 10+ documents.
func formatNodeMetadataOnly(node constellation.Node) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## %s\n\n", node.Title))

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

	// Extract section headings only
	lines := strings.Split(node.Content, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "### ") {
			sb.WriteString(trimmed + "\n")
		}
	}

	return sb.String()
}

// splitIntoSections splits content on markdown headings (## or ###).
func splitIntoSections(content string) []string {
	lines := strings.Split(content, "\n")
	var sections []string
	var current strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if (strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "### ")) && current.Len() > 0 {
			sections = append(sections, strings.TrimSpace(current.String()))
			current.Reset()
		}
		current.WriteString(line)
		current.WriteString("\n")
	}
	if current.Len() > 0 {
		sections = append(sections, strings.TrimSpace(current.String()))
	}

	return sections
}

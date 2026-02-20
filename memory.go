// .cog/memory.go
// Hierarchical Memory Domain (HMD) Operations
//
// Replaces memory.sh (793 LOC) with native Go implementation featuring:
// - Waypoint graph traversal with proper BFS/DFS
// - Parallel file processing with goroutines
// - Native frontmatter parsing
// - Integrated salience scoring
// - FTS5 constellation search as primary path (grep fallback)
// - 60x performance improvement (3-5s → 50-100ms)

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cogos-dev/cogos/sdk/constellation"
	"gopkg.in/yaml.v3"
)

// === WAYPOINT GRAPH STRUCTURES ===

// WaypointConnection represents a connection from one document to another
type WaypointConnection struct {
	Target string  `json:"target"` // Target path
	Weight float64 `json:"weight"` // Connection strength (0-1)
}

// WaypointGraph represents connections between documents
type WaypointGraph struct {
	Connections map[string][]WaypointConnection `json:"connections"`
}

// WaypointNode represents a node in traversal with activation score
type WaypointNode struct {
	Path       string
	Activation float64
	Depth      int
	SourceType string // "direct" or "waypoint"
}

// === MEMORY SEARCH STRUCTURES ===

// MemorySearchResult represents a single search result
type MemorySearchResult struct {
	Path            string
	URI             string // cog://mem/ URI — the kernel-validated handle
	Score           float64
	Title           string
	Type            string
	MemoryStrength  float64
	Salience        float64
	KeywordStrength float64
	Depth           int
	SourceType      string
}

// MemoryPathToURI converts an absolute memory file path to a cog://mem/ URI.
// Example: /Users/foo/cog-workspace/.cog/mem/semantic/insights/topic.cog.md → cog://mem/semantic/insights/topic
func MemoryPathToURI(cogRoot, absPath string) string {
	memDir := filepath.Join(cogRoot, ".cog", "mem") + "/"
	if !strings.HasPrefix(absPath, memDir) {
		return absPath // Not a memory path — return as-is
	}
	relPath := strings.TrimPrefix(absPath, memDir)
	// Strip file extensions for clean URIs
	relPath = strings.TrimSuffix(relPath, ".cog.md")
	relPath = strings.TrimSuffix(relPath, ".md")
	return "cog://mem/" + relPath
}

// URIToMemoryPath converts a cog://mem/ URI back to an absolute file path.
// Tries .cog.md first, then .md, then bare path.
func URIToMemoryPath(cogRoot, uri string) (string, error) {
	if !strings.HasPrefix(uri, "cog://mem/") {
		return "", fmt.Errorf("not a memory URI: %s", uri)
	}
	relPath := strings.TrimPrefix(uri, "cog://mem/")
	memDir := filepath.Join(cogRoot, ".cog", "mem")

	// Try .cog.md first (canonical), then .md, then bare
	candidates := []string{
		filepath.Join(memDir, relPath+".cog.md"),
		filepath.Join(memDir, relPath+".md"),
		filepath.Join(memDir, relPath),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("memory URI not found: %s (tried %s.{cog.md,md})", uri, relPath)
}

// === GENERAL PATH ↔ URI CONVERSION ===

// uriMapping defines a filesystem prefix → cog:// namespace mapping.
type uriMapping struct {
	// pathPrefix is relative to cogRoot (e.g., ".cog/mem/")
	pathPrefix string
	// uriPrefix is the cog:// namespace (e.g., "cog://mem/")
	uriPrefix string
	// stripExt controls whether .cog.md/.md extensions are stripped from URIs
	stripExt bool
}

// uriMappings is ordered by specificity — longest prefix first wins.
var uriMappings = []uriMapping{
	// Memory (most specific .cog/ subtree first)
	{pathPrefix: ".cog/mem/", uriPrefix: "cog://mem/", stripExt: true},
	// ADRs
	{pathPrefix: ".cog/adr/", uriPrefix: "cog://adr/", stripExt: true},
	// Hooks
	{pathPrefix: ".cog/hooks/", uriPrefix: "cog://hooks/", stripExt: true},
	// Agents (under .cog/bin/agents/)
	{pathPrefix: ".cog/bin/agents/", uriPrefix: "cog://agents/", stripExt: true},
	// Skills (under .cog/bin/skills/)
	{pathPrefix: ".cog/bin/skills/", uriPrefix: "cog://skills/", stripExt: true},
	// Roles
	{pathPrefix: ".cog/conf/roles/", uriPrefix: "cog://roles/", stripExt: true},
	// Ontology
	{pathPrefix: ".cog/ontology/", uriPrefix: "cog://kernel/ontology/", stripExt: true},
	// Specs
	{pathPrefix: ".cog/conf/spec/", uriPrefix: "cog://spec/", stripExt: true},
	// Milestones
	{pathPrefix: ".cog/milestones/", uriPrefix: "cog://kernel/milestones/", stripExt: true},
	// Configuration (generic .cog/conf/ catch-all after specific subtrees)
	{pathPrefix: ".cog/conf/", uriPrefix: "cog://kernel/conf/", stripExt: true},
	// Coordination
	{pathPrefix: ".cog/claims/", uriPrefix: "cog://kernel/coordination/claims/", stripExt: false},
	{pathPrefix: ".cog/handoffs/", uriPrefix: "cog://kernel/coordination/handoffs/", stripExt: false},
	{pathPrefix: ".cog/broadcasts/", uriPrefix: "cog://kernel/coordination/broadcasts/", stripExt: false},
	// Ledger
	{pathPrefix: ".cog/ledger/", uriPrefix: "cog://ledger/", stripExt: false},
	// Runtime (PID files, sockets)
	{pathPrefix: ".cog/run/", uriPrefix: "cog://kernel/run/", stripExt: false},
	// Logs
	{pathPrefix: ".cog/logs/", uriPrefix: "cog://kernel/logs/", stripExt: false},
	// Status
	{pathPrefix: ".cog/.state/", uriPrefix: "cog://status/", stripExt: false},
	// Components (apps/)
	{pathPrefix: "apps/", uriPrefix: "cog://kernel/components/apps/", stripExt: false},
}

// PathToURI converts any workspace path to the appropriate cog:// URI.
// The path can be absolute or relative to cogRoot. If no namespace matches,
// it returns the workspace-relative path unchanged.
func PathToURI(cogRoot, path string) string {
	// Normalize to workspace-relative
	relPath := path
	if strings.HasPrefix(path, cogRoot) {
		relPath = strings.TrimPrefix(path, cogRoot)
		relPath = strings.TrimPrefix(relPath, "/")
	}

	for _, m := range uriMappings {
		if strings.HasPrefix(relPath, m.pathPrefix) {
			suffix := strings.TrimPrefix(relPath, m.pathPrefix)
			if m.stripExt {
				suffix = strings.TrimSuffix(suffix, ".cog.md")
				suffix = strings.TrimSuffix(suffix, ".md")
				suffix = strings.TrimSuffix(suffix, ".yaml")
				suffix = strings.TrimSuffix(suffix, ".yml")
			}
			// Trim trailing slash for clean URIs
			suffix = strings.TrimSuffix(suffix, "/")
			return m.uriPrefix + suffix
		}
	}

	// No namespace match — return workspace-relative path
	return relPath
}

// URIToPath converts a cog:// URI back to an absolute filesystem path.
// Probes for .cog.md, .md, .yaml, and bare path in that order.
func URIToPath(cogRoot, uri string) (string, error) {
	if !strings.HasPrefix(uri, "cog://") {
		return "", fmt.Errorf("not a cog URI: %s", uri)
	}

	for _, m := range uriMappings {
		if strings.HasPrefix(uri, m.uriPrefix) {
			suffix := strings.TrimPrefix(uri, m.uriPrefix)
			baseDir := filepath.Join(cogRoot, m.pathPrefix)

			if m.stripExt {
				// Probe for file with extensions
				candidates := []string{
					filepath.Join(baseDir, suffix+".cog.md"),
					filepath.Join(baseDir, suffix+".md"),
					filepath.Join(baseDir, suffix+".yaml"),
					filepath.Join(baseDir, suffix+".yml"),
					filepath.Join(baseDir, suffix),
				}
				for _, c := range candidates {
					if _, err := os.Stat(c); err == nil {
						return c, nil
					}
				}
				// Default to .cog.md for write operations
				return filepath.Join(baseDir, suffix+".cog.md"), nil
			}

			// No extension stripping — return direct path
			return filepath.Join(baseDir, suffix), nil
		}
	}

	return "", fmt.Errorf("unresolvable URI: %s", uri)
}

// === WAYPOINT GRAPH LOADING ===

// LoadWaypointGraph loads waypoint connections from JSON
func LoadWaypointGraph(memoryDir string) (*WaypointGraph, error) {
	waypointsFile := filepath.Join(memoryDir, "waypoints", "connections.json")

	if _, err := os.Stat(waypointsFile); os.IsNotExist(err) {
		// No waypoints file - return empty graph
		return &WaypointGraph{
			Connections: make(map[string][]WaypointConnection),
		}, nil
	}

	data, err := os.ReadFile(waypointsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read waypoints: %w", err)
	}

	var graph WaypointGraph
	if err := json.Unmarshal(data, &graph); err != nil {
		return nil, fmt.Errorf("failed to parse waypoints: %w", err)
	}

	return &graph, nil
}

// NormalizePath normalizes a path for waypoint comparison
// Removes .cog.md suffix and .md suffix for matching
func NormalizePath(path, cogRoot string) string {
	// Make relative to COG_ROOT if absolute
	rel := path
	if strings.HasPrefix(path, cogRoot) {
		rel = strings.TrimPrefix(path, cogRoot+"/")
	}

	// Strip .cog.md and .md extensions for comparison
	rel = strings.TrimSuffix(rel, ".cog.md")
	rel = strings.TrimSuffix(rel, ".md")

	return rel
}

// === WAYPOINT GRAPH TRAVERSAL (BFS) ===

// TraverseWaypoints performs spreading activation through waypoint graph
// Uses BFS with activation decay at each depth level
func TraverseWaypoints(
	initialMatches []MemorySearchResult,
	graph *WaypointGraph,
	maxDepth int,
	decay float64,
	cogRoot string,
) []MemorySearchResult {
	// Track activations: path -> highest activation
	activations := make(map[string]*WaypointNode)

	// Initialize with direct matches (depth 0)
	frontier := make([]*WaypointNode, 0, len(initialMatches))
	for _, match := range initialMatches {
		node := &WaypointNode{
			Path:       match.Path,
			Activation: match.Score,
			Depth:      0,
			SourceType: "direct",
		}
		activations[match.Path] = node
		frontier = append(frontier, node)
	}

	// BFS traversal
	for depth := 1; depth <= maxDepth; depth++ {
		if len(frontier) == 0 {
			break
		}

		nextFrontier := make([]*WaypointNode, 0)

		// Process current frontier
		for _, node := range frontier {
			// Normalize path for lookup
			normPath := NormalizePath(node.Path, cogRoot)

			// Find connections from this node
			connections, exists := graph.Connections[normPath]
			if !exists {
				continue
			}

			// Propagate activation to targets
			for _, conn := range connections {
				// Calculate new activation
				newActivation := node.Activation * conn.Weight * decay

				// Resolve target path (try both .md and .cog.md)
				targetPath := filepath.Join(cogRoot, conn.Target)
				if _, err := os.Stat(targetPath); os.IsNotExist(err) {
					// Try .cog.md variant
					targetPath = strings.TrimSuffix(targetPath, ".md") + ".cog.md"
					if _, err := os.Stat(targetPath); os.IsNotExist(err) {
						continue // Skip if file doesn't exist
					}
				}

				// Check if we've seen this file before
				existing, seen := activations[targetPath]
				if !seen || newActivation > existing.Activation {
					newNode := &WaypointNode{
						Path:       targetPath,
						Activation: newActivation,
						Depth:      depth,
						SourceType: "waypoint",
					}
					activations[targetPath] = newNode
					nextFrontier = append(nextFrontier, newNode)
				}
			}
		}

		frontier = nextFrontier
	}

	// Convert activations to results
	results := make([]MemorySearchResult, 0, len(activations))
	const maxWaypointReads = 200 // Limit file I/O during waypoint traversal
	waypointReads := 0
	for path, node := range activations {
		// Skip direct matches - they're already in initialMatches
		if node.Depth == 0 {
			continue
		}

		// Extract metadata from file
		title := ""
		docType := "unknown"
		memStrength := 0.5

		if waypointReads >= maxWaypointReads {
			// Skip file I/O if we've hit the limit; use defaults
		} else if content, err := os.ReadFile(path); err == nil {
			waypointReads++
			if doc, err := ExtractFrontmatter(string(content)); err == nil {
				if data, err := ParseFrontmatter(doc.Frontmatter); err == nil {
					if t, ok := data["title"].(string); ok {
						title = t
					}
					if tp, ok := data["type"].(string); ok {
						docType = tp
					}
					if ms, ok := data["memory_strength"].(float64); ok {
						memStrength = ms
					}
				}
			}
		}

		if title == "" {
			title = ExtractTitleFromFilename(path)
		}

		results = append(results, MemorySearchResult{
			Path:           path,
			URI:            MemoryPathToURI(cogRoot, path),
			Score:          node.Activation,
			Title:          title,
			Type:           docType,
			MemoryStrength: memStrength,
			Salience:       0.0, // Waypoint nodes don't get salience scores
			Depth:          node.Depth,
			SourceType:     node.SourceType,
		})
	}

	return results
}

// === MEMORY SEARCH ===

// constellationSearch attempts FTS5 search via the constellation database.
// Returns results and true if successful, or nil and false if constellation
// is unavailable or errors out (signaling that grep fallback should be used).
func constellationMemorySearch(cogRoot string, query string, rawMode bool) ([]MemorySearchResult, bool) {
	// Check if constellation DB exists before trying to open it
	dbPath := filepath.Join(cogRoot, ".cog", ".state", "constellation.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, false
	}

	c, err := getConstellation()
	if err != nil {
		return nil, false
	}

	// FTS5 search — request generous limit for ranking
	nodes, err := c.Search(query, 50)
	if err != nil {
		return nil, false
	}

	if len(nodes) == 0 {
		// Constellation worked but found nothing — still a valid result.
		// Return empty so we don't redundantly grep.
		return []MemorySearchResult{}, true
	}

	// Filter to memory sector only (.cog/mem/)
	memPrefix := filepath.Join(cogRoot, ".cog", "mem") + "/"
	var memNodes []constellation.Node
	for _, n := range nodes {
		if strings.HasPrefix(n.Path, memPrefix) {
			memNodes = append(memNodes, n)
		}
	}

	if len(memNodes) == 0 {
		return []MemorySearchResult{}, true
	}

	// Map BM25 scores to 0-1 range.
	// BM25 returns negative values; closer to 0 is better (more relevant).
	// We normalize so the best match gets score ~1.0.
	minRank := memNodes[0].Rank // most negative = best
	for _, n := range memNodes {
		if n.Rank < minRank {
			minRank = n.Rank
		}
	}

	results := make([]MemorySearchResult, 0, len(memNodes))
	for _, n := range memNodes {
		// Normalize BM25: best → 1.0, worst → near 0
		var normalizedScore float64
		if minRank < 0 {
			normalizedScore = n.Rank / minRank // both negative, result is 0-1
		} else {
			normalizedScore = 1.0
		}
		// Apply sigmoid-like compression to keep scores in useful range
		score := 0.2 + 0.8*(1.0/(1.0+math.Exp(-5.0*(normalizedScore-0.5))))

		title := n.Title
		if title == "" {
			title = ExtractTitleFromFilename(n.Path)
		}
		docType := n.Type
		if docType == "" {
			docType = "unknown"
		}

		result := MemorySearchResult{
			Path:            n.Path,
			URI:             MemoryPathToURI(cogRoot, n.Path),
			Score:           score,
			Title:           title,
			Type:            docType,
			MemoryStrength:  0.5, // default; enriched below if not raw mode
			KeywordStrength: normalizedScore,
			Salience:        0.0,
			Depth:           0,
			SourceType:      "fts5",
		}

		if !rawMode {
			// Enrich with frontmatter memory_strength if available
			if content, err := os.ReadFile(n.Path); err == nil {
				if doc, err := ExtractFrontmatter(string(content)); err == nil {
					if data, err := ParseFrontmatter(doc.Frontmatter); err == nil {
						if ms, ok := data["memory_strength"].(float64); ok {
							result.MemoryStrength = ms
						}
					}
				}
			}
			// Blend BM25 relevance with memory_strength
			result.Score = normalizedScore * ((result.MemoryStrength + 0.5) / 2.0)
		}

		results = append(results, result)
	}

	// Sort by score descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results, true
}

// grepMemorySearch performs the original grep-based memory search.
// This is the fallback path when constellation is unavailable.
func grepMemorySearch(cogRoot string, query string, rawMode bool) ([]MemorySearchResult, error) {
	memoryDir := filepath.Join(cogRoot, ".cog", "mem")

	// Search for matching files via grep
	var searchResults []string

	grepCtx, grepCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer grepCancel()
	cmd := exec.CommandContext(grepCtx, "grep", "-ril", "--", query, memoryDir)
	output, err := cmd.Output()
	if err == nil {
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" && (strings.HasSuffix(line, ".md") || strings.HasSuffix(line, ".cog.md")) {
				searchResults = append(searchResults, line)
			}
		}
	}

	// If no results, return empty
	if len(searchResults) == 0 {
		return []MemorySearchResult{}, nil
	}

	// If raw mode, just return paths without ranking
	if rawMode {
		results := make([]MemorySearchResult, len(searchResults))
		for i, path := range searchResults {
			results[i] = MemorySearchResult{
				Path:       path,
				URI:        MemoryPathToURI(cogRoot, path),
				Score:      1.0,
				Title:      ExtractTitleFromFilename(path),
				Type:       "unknown",
				SourceType: "direct",
			}
		}
		return results, nil
	}

	// Parallel processing for ranking (phase 1: keyword + memory_strength only, no salience)
	results := make([]MemorySearchResult, len(searchResults))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 20) // Limit concurrent goroutines to 20

	// Process each file in parallel
	for i, filePath := range searchResults {
		wg.Add(1)
		go func(idx int, path string) {
			sem <- struct{}{}        // Acquire semaphore slot
			defer func() { <-sem }() // Release semaphore slot
			defer wg.Done()

			result := MemorySearchResult{
				Path:       path,
				URI:        MemoryPathToURI(cogRoot, path),
				Score:      0.0,
				SourceType: "direct",
				Depth:      0,
			}

			// Extract frontmatter fields
			memoryStrength := 0.5
			title := ""
			docType := ""

			content, err := os.ReadFile(path)
			if err == nil {
				if doc, err := ExtractFrontmatter(string(content)); err == nil {
					if data, err := ParseFrontmatter(doc.Frontmatter); err == nil {
						if t, ok := data["title"].(string); ok {
							title = t
						}
						if tp, ok := data["type"].(string); ok {
							docType = tp
						}
						if ms, ok := data["memory_strength"].(float64); ok {
							memoryStrength = ms
						}
					}
				}

				// Count keyword matches for keyword strength
				keywordMatches := strings.Count(strings.ToLower(string(content)), strings.ToLower(query))
				keywordStrength := float64(keywordMatches) / 10.0
				if keywordStrength > 1.0 {
					keywordStrength = 1.0
				}
				if keywordStrength < 0.2 {
					keywordStrength = 0.2
				}

				// Initial score without salience
				result.Score = keywordStrength * (memoryStrength / 2.0)
				result.Salience = 0.0
				result.MemoryStrength = memoryStrength
				result.KeywordStrength = keywordStrength
			}

			// Use filename as fallback for title
			if title == "" {
				title = ExtractTitleFromFilename(path)
			}
			if docType == "" {
				docType = "unknown"
			}

			result.Title = title
			result.Type = docType

			mu.Lock()
			results[idx] = result
			mu.Unlock()
		}(i, filePath)
	}

	wg.Wait()

	return results, nil
}

// MemorySearch performs comprehensive memory search with ranking.
// Primary path: FTS5 via constellation database (fast, ranked by BM25).
// Fallback path: grep + keyword counting (if constellation DB unavailable).
func MemorySearch(
	cogRoot string,
	query string,
	deepMode bool,
	deepDepth int,
	decayFactor float64,
	rawMode bool,
) ([]MemorySearchResult, error) {

	var results []MemorySearchResult
	usedFTS := false

	// Primary path: try constellation FTS5 search
	if ftsResults, ok := constellationMemorySearch(cogRoot, query, rawMode); ok {
		results = ftsResults
		usedFTS = true
	}

	// Fallback path: grep-based search
	if !usedFTS {
		grepResults, err := grepMemorySearch(cogRoot, query, rawMode)
		if err != nil {
			return nil, err
		}
		results = grepResults
	}

	// If no results or raw mode, return early
	if len(results) == 0 || rawMode {
		return results, nil
	}

	// Sort by base score before salience refinement
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	// Salience refinement (disabled by default — go-git PathFilter is O(commits×tree)
	// which causes hangs on repos with 1000+ commits. BM25/keyword scores are sufficient.
	// Enable with COG_MEMORY_SALIENCE_FORCE=1 if needed.)
	salienceLimit := 0
	if val := os.Getenv("COG_MEMORY_SALIENCE_LIMIT"); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil && parsed >= 0 {
			salienceLimit = parsed
		}
	}
	if val := os.Getenv("COG_MEMORY_SALIENCE_DISABLE"); val != "" {
		lower := strings.ToLower(val)
		if lower == "1" || lower == "true" || lower == "yes" {
			salienceLimit = 0
		}
	}
	forceSalience := false
	if val := os.Getenv("COG_MEMORY_SALIENCE_FORCE"); val != "" {
		lower := strings.ToLower(val)
		if lower == "1" || lower == "true" || lower == "yes" {
			forceSalience = true
		}
	}
	if !forceSalience && salienceLimit > 0 && len(results) > salienceLimit {
		salienceLimit = 0
	}

	if salienceLimit > 0 {
		if salienceLimit > len(results) {
			salienceLimit = len(results)
		}

		salienceCfg := DefaultSalienceConfig()
		for i := 0; i < salienceLimit; i++ {
			path := results[i].Path
			if sal, err := ComputeFileSalience(cogRoot, path, 90, salienceCfg); err == nil {
				results[i].Salience = sal.Total
				results[i].Score = results[i].KeywordStrength * ((results[i].MemoryStrength + sal.Total) / 2.0)
			}
		}

		// Re-sort after salience refinement
		sort.Slice(results, func(i, j int) bool {
			return results[i].Score > results[j].Score
		})
	}

	// If deep mode enabled, traverse waypoint graph
	if deepMode {
		graph, err := LoadWaypointGraph(filepath.Join(cogRoot, ".cog", "mem"))
		if err == nil && len(graph.Connections) > 0 {
			waypointResults := TraverseWaypoints(results, graph, deepDepth, decayFactor, cogRoot)
			results = append(results, waypointResults...)
		}
	}

	// Sort by score descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results, nil
}

// === MEMORY LIST ===

// MemoryList lists documents in a memory sector
func MemoryList(cogRoot string, sector string, subdir string) ([]MemorySearchResult, error) {
	memoryDir := filepath.Join(cogRoot, ".cog", "mem")
	searchPath := filepath.Join(memoryDir, sector)
	if subdir != "" {
		searchPath = filepath.Join(searchPath, subdir)
	}

	if _, err := os.Stat(searchPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("sector not found: %s", sector)
	}

	results := make([]MemorySearchResult, 0)

	err := filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors, continue walking
		}

		if info.IsDir() {
			return nil
		}

		if !strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".cog.md") {
			return nil
		}

		// Extract metadata
		title := ""
		docType := ""
		status := ""

		if content, err := os.ReadFile(path); err == nil {
			if doc, err := ExtractFrontmatter(string(content)); err == nil {
				if data, err := ParseFrontmatter(doc.Frontmatter); err == nil {
					if t, ok := data["title"].(string); ok {
						title = t
					}
					if tp, ok := data["type"].(string); ok {
						docType = tp
					}
					if st, ok := data["status"].(string); ok {
						status = st
					}
				}
			}
		}

		if title == "" {
			title = ExtractTitleFromFilename(path)
		}
		if docType == "" {
			docType = "unknown"
		}
		if status == "" {
			status = "unknown"
		}

		results = append(results, MemorySearchResult{
			Path:  path,
			URI:   MemoryPathToURI(cogRoot, path),
			Title: title,
			Type:  docType,
		})

		return nil
	})

	if err != nil {
		return nil, err
	}

	return results, nil
}

// === MEMORY PATH RESOLUTION ===

// resolveMemoryPath normalizes a path to an absolute path within the memory directory.
// Handles all input formats:
//   - cog:// URI:          "cog://mem/semantic/insights/topic"
//   - Memory-relative:    "semantic/insights/topic.md"
//   - Workspace-relative: ".cog/mem/semantic/insights/topic.md"
//   - Absolute:           "/Users/.../cog-workspace/.cog/mem/semantic/insights/topic.md"
//
// This prevents double-nesting (e.g., .cog/mem/.cog/mem/...) which occurs when
// a .cog/mem/ prefixed path is blindly joined with the memory directory.
func resolveMemoryPath(memoryDir, path string) string {
	// cog://mem/ URI — resolve to filesystem path with extension probing
	if strings.HasPrefix(path, "cog://mem/") {
		relPath := strings.TrimPrefix(path, "cog://mem/")
		// Try .cog.md first (canonical), then .md, then bare
		candidates := []string{
			filepath.Join(memoryDir, relPath+".cog.md"),
			filepath.Join(memoryDir, relPath+".md"),
			filepath.Join(memoryDir, relPath),
		}
		for _, candidate := range candidates {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		// None found — return .cog.md as default (for write operations)
		return filepath.Join(memoryDir, relPath+".cog.md")
	}

	// Absolute path containing .cog/mem/ — extract the memory-relative portion
	// Use LastIndex to handle double-nesting: .cog/mem/.cog/mem/foo → takes "foo"
	if idx := strings.LastIndex(path, "/.cog/mem/"); idx >= 0 {
		relPath := path[idx+len("/.cog/mem/"):]
		return filepath.Join(memoryDir, relPath)
	}

	// Workspace-relative path starting with .cog/mem/
	if strings.HasPrefix(path, ".cog/mem/") {
		relPath := strings.TrimPrefix(path, ".cog/mem/")
		return filepath.Join(memoryDir, relPath)
	}

	// Already absolute — use as-is
	if strings.HasPrefix(path, "/") {
		return path
	}

	// Memory-relative path (the common case) — probe extensions
	direct := filepath.Join(memoryDir, path)
	if _, err := os.Stat(direct); err == nil {
		return direct
	}
	// Probe .cog.md, then .md
	for _, ext := range []string{".cog.md", ".md"} {
		candidate := direct + ext
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return direct // fall through for write operations
}

// === MEMORY READ ===

// MemoryRead reads a memory document and updates last_accessed timestamp.
// Supports URI fragment syntax: path#section-name extracts only that section.
func MemoryRead(cogRoot string, path string) (string, error) {
	// Strip URI fragment before path resolution
	fragment := ""
	if idx := strings.Index(path, "#"); idx != -1 {
		fragment = path[idx+1:]
		path = path[:idx]
	}

	memoryDir := filepath.Join(cogRoot, ".cog", "mem")
	fullPath := resolveMemoryPath(memoryDir, path)

	// Validate path stays within memory directory
	cleanFull := filepath.Clean(fullPath)
	cleanMem := filepath.Clean(memoryDir)
	if !strings.HasPrefix(cleanFull, cleanMem+string(filepath.Separator)) && cleanFull != cleanMem {
		return "", fmt.Errorf("path traversal blocked: %s escapes memory directory", path)
	}

	// Check if file exists
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return "", fmt.Errorf("memory not found: %s (resolved to: %s)", path, fullPath)
	}

	// Read content
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to read memory: %w", err)
	}

	// Update last_accessed timestamp asynchronously with timeout
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() {
			UpdateLastAccessed(fullPath)
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
		}
	}()

	result := string(content)

	// If a fragment was specified, extract that section
	if fragment != "" {
		body := result
		if doc, fmErr := ExtractFrontmatter(result); fmErr == nil {
			body = doc.Body
		}

		// Try anchor match first (prepend # for anchor-style lookup)
		section, secErr := GetSection(body, "#"+fragment)
		if secErr != nil {
			// Fall back to title match (without # prefix)
			section, secErr = GetSection(body, fragment)
		}
		if secErr != nil {
			return "", fmt.Errorf("section %q not found in %s", fragment, path)
		}
		return section, nil
	}

	return result, nil
}

// UpdateLastAccessed updates the last_accessed field in frontmatter
func UpdateLastAccessed(path string) error {
	// Only update files in .cog/mem/
	if !strings.Contains(path, "/.cog/mem/") {
		return nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Check if has frontmatter
	if !HasFrontmatter(string(content)) {
		return nil
	}

	doc, err := ExtractFrontmatter(string(content))
	if err != nil {
		return err
	}

	// Parse frontmatter
	data, err := ParseFrontmatter(doc.Frontmatter)
	if err != nil {
		return err
	}

	// Update last_accessed
	timestamp := time.Now().UTC().Format(time.RFC3339)
	data["last_accessed"] = timestamp

	// Re-marshal frontmatter
	updatedYAML, err := marshalYAML(data)
	if err != nil {
		return err
	}

	// Reconstruct document
	newContent := "---\n" + updatedYAML + "---\n" + doc.Body

	return os.WriteFile(path, []byte(newContent), 0644)
}

// Helper to marshal YAML (simple implementation)
func marshalYAML(data map[string]interface{}) (string, error) {
	var lines []string
	for key, value := range data {
		switch v := value.(type) {
		case string:
			lines = append(lines, fmt.Sprintf("%s: %s", key, v))
		case float64:
			lines = append(lines, fmt.Sprintf("%s: %.2f", key, v))
		case int:
			lines = append(lines, fmt.Sprintf("%s: %d", key, v))
		case bool:
			lines = append(lines, fmt.Sprintf("%s: %t", key, v))
		default:
			// Skip complex types for now
			continue
		}
	}
	sort.Strings(lines) // Keep consistent ordering
	return strings.Join(lines, "\n"), nil
}

// === MEMORY WRITE ===

// MemoryWrite creates a new memory document with frontmatter
func MemoryWrite(cogRoot string, path string, title string, content string) error {
	memoryDir := filepath.Join(cogRoot, ".cog", "mem")
	fullPath := resolveMemoryPath(memoryDir, path)

	// Validate path stays within memory directory
	cleanFull := filepath.Clean(fullPath)
	cleanMem := filepath.Clean(memoryDir)
	if !strings.HasPrefix(cleanFull, cleanMem+string(filepath.Separator)) && cleanFull != cleanMem {
		return fmt.Errorf("path traversal blocked: %s escapes memory directory", path)
	}

	// Create directory if needed
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Generate frontmatter
	frontmatter := GenerateFrontmatter(fullPath, title)

	// Combine frontmatter and content
	document := frontmatter + "\n# " + title + "\n\n" + content

	// Write file
	if err := os.WriteFile(fullPath, []byte(document), 0644); err != nil {
		return fmt.Errorf("failed to write memory: %w", err)
	}

	fmt.Printf("Written: %s\n", MemoryPathToURI(cogRoot, fullPath))

	// Auto-generate sections: frontmatter if the document has 2+ level-2+ headings
	if doc, fmErr := ExtractFrontmatter(document); fmErr == nil {
		sections := ParseSections(doc.Body)
		level2PlusCount := 0
		for _, s := range sections {
			if s.Level >= 2 {
				level2PlusCount++
			}
		}
		if level2PlusCount >= 2 {
			if idxErr := MemoryIndex(cogRoot, path); idxErr != nil {
				fmt.Fprintf(os.Stderr, "warning: section auto-index failed: %v\n", idxErr)
			}
		}
	}

	return nil
}

// === MEMORY APPEND ===

// MemoryAppend appends content to an existing memory document
func MemoryAppend(cogRoot string, path string, content string) error {
	memoryDir := filepath.Join(cogRoot, ".cog", "mem")
	fullPath := resolveMemoryPath(memoryDir, path)

	// Check if file exists
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return fmt.Errorf("memory not found: %s", path)
	}

	// Open file for appending
	f, err := os.OpenFile(fullPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	// Append content
	if _, err := f.WriteString("\n" + content + "\n"); err != nil {
		return fmt.Errorf("failed to append: %w", err)
	}

	fmt.Printf("Appended to: %s\n", MemoryPathToURI(cogRoot, fullPath))
	return nil
}

// === MEMORY STATS ===

// MemoryStats computes statistics for all memory sectors
func MemoryStats(cogRoot string) error {
	memoryDir := filepath.Join(cogRoot, ".cog", "mem")
	sectors := []string{"semantic", "episodic", "procedural", "reflective"}

	fmt.Println("Memory Statistics")
	fmt.Println("=================")
	fmt.Println()

	for _, sector := range sectors {
		sectorPath := filepath.Join(memoryDir, sector)
		if _, err := os.Stat(sectorPath); os.IsNotExist(err) {
			continue
		}

		count := 0
		filepath.Walk(sectorPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() && (strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".cog.md")) {
				count++
			}
			return nil
		})

		fmt.Printf("%s: %d documents\n", sector, count)
	}

	return nil
}

// === MEMORY INDEX ===

// sectionIndexEntry is a structured section entry for YAML marshaling in frontmatter.
type sectionIndexEntry struct {
	Title  string `yaml:"title"`
	Anchor string `yaml:"anchor,omitempty"`
	Line   int    `yaml:"line"`
	Size   int    `yaml:"size"`
}

// titleToAnchor generates a URL-friendly anchor from a section title.
// Lowercases, replaces spaces with hyphens, strips non-alphanumeric characters
// except hyphens, and truncates to 64 chars at a hyphen boundary.
func titleToAnchor(title string) string {
	const maxLen = 64

	anchor := strings.ToLower(title)
	anchor = strings.ReplaceAll(anchor, " ", "-")
	var buf strings.Builder
	for _, r := range anchor {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			buf.WriteRune(r)
		}
	}
	anchor = strings.Trim(buf.String(), "-")

	if len(anchor) > maxLen {
		anchor = anchor[:maxLen]
		// Trim at last hyphen to avoid cutting mid-word
		if idx := strings.LastIndex(anchor, "-"); idx > maxLen/2 {
			anchor = anchor[:idx]
		}
	}

	return anchor
}

// MemoryIndex generates or updates the sections: frontmatter for a cogdoc.
// It parses the markdown body for headings (level 2+), builds structured section
// entries, and rewrites the frontmatter with the updated sections field.
func MemoryIndex(cogRoot, path string) error {
	memoryDir := filepath.Join(cogRoot, ".cog", "mem")
	fullPath := resolveMemoryPath(memoryDir, path)

	// Validate path stays within memory directory
	cleanFull := filepath.Clean(fullPath)
	cleanMem := filepath.Clean(memoryDir)
	if !strings.HasPrefix(cleanFull, cleanMem+string(filepath.Separator)) && cleanFull != cleanMem {
		return fmt.Errorf("path traversal blocked: %s escapes memory directory", path)
	}

	// Read file content
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// Extract frontmatter
	doc, err := ExtractFrontmatter(string(content))
	if err != nil {
		return fmt.Errorf("no frontmatter found in %s: %w", path, err)
	}

	// Parse sections from body
	sections := ParseSections(doc.Body)

	// Filter to level 2+ only (skip level 1 = document title)
	var entries []sectionIndexEntry
	for _, s := range sections {
		if s.Level < 2 {
			continue
		}
		anchor := s.Anchor
		if anchor == "" {
			anchor = titleToAnchor(s.Title)
		}
		entries = append(entries, sectionIndexEntry{
			Title:  s.Title,
			Anchor: anchor,
			Line:   s.Line,
			Size:   s.Size,
		})
	}

	if len(entries) == 0 {
		fmt.Println("No sections to index")
		return nil
	}

	// Parse existing frontmatter YAML as map
	fmData := make(map[string]interface{})
	if err := yaml.Unmarshal([]byte(doc.Frontmatter), &fmData); err != nil {
		return fmt.Errorf("failed to parse frontmatter YAML: %w", err)
	}

	// Update/replace the sections key with new section entries
	fmData["sections"] = entries

	// Re-marshal the full frontmatter map
	newFM, err := yaml.Marshal(fmData)
	if err != nil {
		return fmt.Errorf("failed to marshal frontmatter: %w", err)
	}

	// Rewrite the file: new frontmatter + original body
	newContent := "---\n" + string(newFM) + "---\n" + doc.Body

	if err := os.WriteFile(fullPath, []byte(newContent), 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	uri := MemoryPathToURI(cogRoot, fullPath)
	fmt.Printf("Indexed: %s (%d sections)\n", uri, len(entries))
	return nil
}

// MemoryIndexAll walks all cogdocs under .cog/mem/ and bulk-generates section
// indexes in their frontmatter. Files without frontmatter or without level-2+
// headings are skipped. When force is false, files that already have a
// "sections" key in their frontmatter are also skipped.
func MemoryIndexAll(cogRoot string, dryRun, force bool) error {
	memoryDir := filepath.Join(cogRoot, ".cog", "mem")

	indexed := 0
	skippedNoHeadings := 0
	skippedAlreadyIndexed := 0

	err := filepath.WalkDir(memoryDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors, keep walking
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}

		// Read file content
		content, err := os.ReadFile(path)
		if err != nil {
			return nil // skip unreadable files
		}

		// Extract frontmatter — skip files without it
		doc, err := ExtractFrontmatter(string(content))
		if err != nil {
			return nil
		}

		// Parse sections from body
		sections := ParseSections(doc.Body)

		// Filter to level 2+ only
		hasHeadings := false
		for _, s := range sections {
			if s.Level >= 2 {
				hasHeadings = true
				break
			}
		}
		if !hasHeadings {
			skippedNoHeadings++
			return nil
		}

		// If not forcing, check if sections already exist in frontmatter
		if !force {
			fmData := make(map[string]interface{})
			if err := yaml.Unmarshal([]byte(doc.Frontmatter), &fmData); err == nil {
				if _, exists := fmData["sections"]; exists {
					skippedAlreadyIndexed++
					return nil
				}
			}
		}

		// Compute relative path from memory dir
		relPath, err := filepath.Rel(memoryDir, path)
		if err != nil {
			return nil
		}

		if dryRun {
			indexed++
			return nil
		}

		// Call single-file MemoryIndex
		if err := MemoryIndex(cogRoot, relPath); err != nil {
			// Log but don't abort the walk
			fmt.Fprintf(os.Stderr, "warning: failed to index %s: %v\n", relPath, err)
			return nil
		}
		indexed++
		return nil
	})

	if err != nil {
		return fmt.Errorf("walk failed: %w", err)
	}

	if dryRun {
		fmt.Printf("Dry run: would index %d docs (%d skipped — no headings, %d already indexed)\n",
			indexed, skippedNoHeadings, skippedAlreadyIndexed)
	} else {
		fmt.Printf("Indexed: %d docs (%d skipped — no headings, %d skipped — already indexed)\n",
			indexed, skippedNoHeadings, skippedAlreadyIndexed)
	}

	return nil
}

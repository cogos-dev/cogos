// cogfield.go - CogField graph endpoint
//
// Serves the full workspace knowledge graph from constellation.db
// for the CogField visualization frontend.
//
// GET /api/cogfield/graph - Returns all nodes + edges + stats
//
// Type normalization: 64 document types → 11 CogField entity types
// Sector inference: path-based when DB sector is NULL (98% of docs)

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cogos-dev/cogos/sdk/constellation"
)

// CogFieldNode matches the frontend CogFieldNode interface
type CogFieldNode struct {
	ID           string                 `json:"id"`
	Label        string                 `json:"label"`
	EntityType   string                 `json:"entity_type"`
	Sector       string                 `json:"sector"`
	Tags         []string               `json:"tags"`
	Created      string                 `json:"created"`
	Modified     string                 `json:"modified"`
	BackrefCount int                    `json:"backref_count"`
	Strength     float64                `json:"strength"`
	Meta         map[string]interface{} `json:"meta,omitempty"`
}

// CogFieldEdge matches the frontend CogFieldEdge interface
type CogFieldEdge struct {
	Source   string  `json:"source"`
	Target   string  `json:"target"`
	Relation string  `json:"relation"`
	Weight   float64 `json:"weight,omitempty"`
	Thread   string  `json:"thread"`
}

// CogFieldStats matches the frontend CogFieldStats interface
type CogFieldStats struct {
	TotalNodes      int            `json:"total_nodes"`
	TotalEdges      int            `json:"total_edges"`
	NodesByType     map[string]int `json:"nodes_by_type"`
	NodesBySector   map[string]int `json:"nodes_by_sector"`
	EdgesByRelation map[string]int `json:"edges_by_relation"`
	EdgesByThread   map[string]int `json:"edges_by_thread"`
	MostConnected   []string       `json:"most_connected"`
}

// CogFieldGraph matches the frontend CogFieldGraph interface
type CogFieldGraph struct {
	Nodes []CogFieldNode `json:"nodes"`
	Edges []CogFieldEdge `json:"edges"`
	Stats CogFieldStats  `json:"stats"`
}

// normalizeEntityType maps constellation.db document types to CogField entity types
func normalizeEntityType(docType string) string {
	switch docType {
	case "session":
		return "session"
	case "adr":
		return "adr"
	case "skill":
		return "skill"
	case "hook":
		return "hook"
	case "ontology", "term", "claim", "pattern", "theorem", "principle":
		return "ontology"
	case "component":
		return "component"
	case "node":
		return "node"
	case "identity":
		return "agent"
	default:
		return "document"
	}
}

// inferSector determines the sector from the document path when DB sector is NULL
func inferSector(path, dbSector string) string {
	// Use DB sector if it's meaningful
	if dbSector != "" {
		// Normalize the DB sector values
		switch dbSector {
		case "semantic", "episodic", "procedural", "reflective",
			"identities", "reference", "temporal", "emotional", "waypoints":
			return dbSector
		case "architecture", "semantic/architecture":
			return "architecture"
		case "infrastructure":
			return "infrastructure"
		}
	}

	// Infer from path
	p := strings.ToLower(path)

	// Memory sectors
	if strings.Contains(p, "/semantic/") || strings.HasPrefix(p, "semantic/") {
		// Sub-sector detection
		if strings.Contains(p, "/architecture/") {
			return "architecture"
		}
		return "semantic"
	}
	if strings.Contains(p, "/episodic/") || strings.HasPrefix(p, "episodic/") {
		return "episodic"
	}
	if strings.Contains(p, "/procedural/") || strings.HasPrefix(p, "procedural/") {
		return "procedural"
	}
	if strings.Contains(p, "/reflective/") || strings.HasPrefix(p, "reflective/") {
		return "reflective"
	}
	if strings.Contains(p, "/identities/") || strings.HasPrefix(p, "identities/") {
		return "identities"
	}
	if strings.Contains(p, "/reference/") || strings.HasPrefix(p, "reference/") {
		return "reference"
	}

	// Non-memory paths
	if strings.Contains(p, "/adr/") || strings.HasPrefix(p, "adr/") {
		return "architecture"
	}
	if strings.Contains(p, "/ontology/") || strings.HasPrefix(p, "ontology/") {
		return "ontology"
	}

	// Default
	return "semantic"
}

// strengthFromMetrics calculates a 0-10 strength value from substance metrics
func strengthFromMetrics(substanceRatio float64, refCount, wordCount int) float64 {
	// Base from substance ratio (0-4)
	strength := substanceRatio * 4.0

	// Bonus from reference density (0-3)
	if refCount > 10 {
		strength += 3.0
	} else if refCount > 5 {
		strength += 2.0
	} else if refCount > 0 {
		strength += 1.0
	}

	// Bonus from content length (0-3)
	if wordCount > 1000 {
		strength += 3.0
	} else if wordCount > 300 {
		strength += 2.0
	} else if wordCount > 50 {
		strength += 1.0
	}

	if strength > 10 {
		strength = 10
	}
	return strength
}

// handleCogFieldGraph handles GET /api/cogfield/graph
func (s *serveServer) handleCogFieldGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()

	// Open constellation DB (per-request workspace or singleton fallback)
	var c *constellation.Constellation
	var err error
	var wsRoot string
	if ws := workspaceFromRequest(r); ws != nil {
		c, err = getConstellationForWorkspace(ws.root)
		wsRoot = ws.root
	} else {
		c, err = getConstellation()
	}
	if err != nil {
		log.Printf("cogfield: failed to open constellation: %v", err)
		http.Error(w, "Failed to open constellation database", http.StatusInternalServerError)
		return
	}

	graph, err := buildCogFieldGraph(c, wsRoot)
	if err != nil {
		log.Printf("cogfield: failed to build graph: %v", err)
		http.Error(w, "Failed to build graph", http.StatusInternalServerError)
		return
	}

	log.Printf("cogfield: built graph with %d nodes, %d edges in %v",
		graph.Stats.TotalNodes, graph.Stats.TotalEdges, time.Since(start))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=30") // Cache for 30s
	json.NewEncoder(w).Encode(graph)
}

// buildCogFieldGraph queries constellation.db and assembles the full graph.
// wsRoot, if non-empty, overrides ResolveWorkspace() for adapter summary nodes.
func buildCogFieldGraph(c *constellation.Constellation, wsRoot string) (*CogFieldGraph, error) {
	db := c.DB()

	// --- Query all documents ---
	rows, err := db.Query(`
		SELECT
			d.id,
			d.path,
			COALESCE(d.title, ''),
			COALESCE(d.type, ''),
			COALESCE(d.sector, ''),
			COALESCE(d.created, ''),
			COALESCE(d.updated, ''),
			COALESCE(d.word_count, 0),
			COALESCE(d.substance_ratio, 0.0),
			COALESCE(d.ref_count, 0),
			(SELECT COUNT(*) FROM backlinks b WHERE b.target_id = d.id)
		FROM documents d
		WHERE COALESCE(d.status, '') != 'deprecated'
		ORDER BY d.updated DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query documents: %w", err)
	}
	defer rows.Close()

	nodeMap := make(map[string]*CogFieldNode)
	pathMap := make(map[string]string)  // docID → path (for sibling computation)
	dateMap := make(map[string]string)  // docID → date YYYY-MM-DD (for temporal)
	var nodes []CogFieldNode

	for rows.Next() {
		var (
			id, path, title, docType, sector string
			created, updated                 string
			wordCount, refCount, backrefCount int
			substanceRatio                    float64
		)

		if err := rows.Scan(&id, &path, &title, &docType, &sector,
			&created, &updated, &wordCount, &substanceRatio, &refCount, &backrefCount); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}

		// Use title, fallback to filename
		label := title
		if label == "" {
			parts := strings.Split(path, "/")
			label = parts[len(parts)-1]
			label = strings.TrimSuffix(label, ".cog.md")
			label = strings.TrimSuffix(label, ".md")
			label = strings.ReplaceAll(label, "-", " ")
		}

		node := CogFieldNode{
			ID:           id,
			Label:        label,
			EntityType:   normalizeEntityType(docType),
			Sector:       inferSector(path, sector),
			Tags:         []string{},
			Created:      created,
			Modified:     updated,
			BackrefCount: backrefCount,
			Strength:     strengthFromMetrics(substanceRatio, refCount, wordCount),
		}

		// Preserve original type in meta if it was normalized
		if docType != "" && docType != node.EntityType {
			node.Meta = map[string]interface{}{
				"doc_type": docType,
			}
		}

		nodes = append(nodes, node)
		nodeMap[id] = &nodes[len(nodes)-1]
		pathMap[id] = path
		if len(created) >= 10 {
			dateMap[id] = created[:10]
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate documents: %w", err)
	}

	// --- Query tags ---
	tagRows, err := db.Query(`SELECT document_id, tag FROM tags`)
	if err != nil {
		return nil, fmt.Errorf("query tags: %w", err)
	}
	defer tagRows.Close()

	for tagRows.Next() {
		var docID, tag string
		if err := tagRows.Scan(&docID, &tag); err != nil {
			continue
		}
		if node, ok := nodeMap[docID]; ok {
			node.Tags = append(node.Tags, tag)
		}
	}

	// --- Thread 1: explicit edges from doc_references ---
	edgeRows, err := db.Query(`
		SELECT source_id, target_id, COALESCE(relation, 'refs')
		FROM doc_references
		WHERE target_id IS NOT NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	defer edgeRows.Close()

	var edges []CogFieldEdge
	edgesByRelation := make(map[string]int)

	for edgeRows.Next() {
		var source, target, relation string
		if err := edgeRows.Scan(&source, &target, &relation); err != nil {
			continue
		}

		// Only include edges where both endpoints exist
		if _, srcOK := nodeMap[source]; !srcOK {
			continue
		}
		if _, tgtOK := nodeMap[target]; !tgtOK {
			continue
		}

		// Normalize hyphenated relation types
		relation = strings.ReplaceAll(relation, "-", "_")

		edges = append(edges, CogFieldEdge{
			Source:   source,
			Target:   target,
			Relation: relation,
			Weight:   1.0,
			Thread:   "explicit",
		})
		edgesByRelation[relation]++
	}

	// --- Thread 2: shared_tags (docs sharing 2+ non-trivial tags) ---
	// Exclude date tags, generic type tags (session, claude-code, etc.), and
	// skip pairs where both nodes are sessions (they share generic tags and
	// create massive cliques that dominate the thread).
	tagEdgeRows, err := db.Query(`
		SELECT t1.document_id, t2.document_id, COUNT(*) as n
		FROM tags t1
		JOIN tags t2 ON t1.tag = t2.tag AND t1.document_id < t2.document_id
		WHERE t1.tag NOT GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'
		  AND t1.tag NOT IN ('session', 'claude-code', 'claude', 'code')
		GROUP BY t1.document_id, t2.document_id
		HAVING n >= 2
		ORDER BY n DESC
		LIMIT 5000
	`)
	if err != nil {
		log.Printf("cogfield: shared_tags query failed: %v", err)
	} else {
		defer tagEdgeRows.Close()
		for tagEdgeRows.Next() {
			var doc1, doc2 string
			var shared int
			if err := tagEdgeRows.Scan(&doc1, &doc2, &shared); err != nil {
				continue
			}
			n1, ok1 := nodeMap[doc1]
			n2, ok2 := nodeMap[doc2]
			if !ok1 || !ok2 {
				continue
			}
			// Skip session↔session pairs — they pollute with generic tags
			if n1.EntityType == "session" && n2.EntityType == "session" {
				continue
			}
			weight := math.Min(float64(shared)*0.15, 1.0)
			edges = append(edges, CogFieldEdge{
				Source:   doc1,
				Target:   doc2,
				Relation: "shared_tags",
				Weight:   weight,
				Thread:   "shared_tags",
			})
		}
	}

	// --- Thread 3: siblings (same parent directory, groups of 2-9) ---
	// Cap at 10 (was 20). Groups of 10+ create 45+ edges which form
	// visual cliques. For large directories, use a chain instead of a
	// full clique to keep edge count O(n) not O(n²).
	dirGroups := make(map[string][]string) // parent dir → doc IDs
	for id, p := range pathMap {
		if _, ok := nodeMap[id]; !ok {
			continue
		}
		dir := filepath.Dir(p)
		dirGroups[dir] = append(dirGroups[dir], id)
	}
	for _, group := range dirGroups {
		if len(group) < 2 {
			continue
		}
		sort.Strings(group) // deterministic ordering

		if len(group) <= 10 {
			// Small groups: full clique (max 45 edges)
			for i := 0; i < len(group); i++ {
				for j := i + 1; j < len(group); j++ {
					edges = append(edges, CogFieldEdge{
						Source:   group[i],
						Target:   group[j],
						Relation: "sibling",
						Weight:   0.4,
						Thread:   "siblings",
					})
				}
			}
		} else {
			// Large groups: chain (n-1 edges, keeps them linked without O(n²) explosion)
			for i := 1; i < len(group); i++ {
				edges = append(edges, CogFieldEdge{
					Source:   group[i-1],
					Target:   group[i],
					Relation: "sibling",
					Weight:   0.3,
					Thread:   "siblings",
				})
			}
		}
	}

	// --- Thread 4: temporal (created same day, non-session documents only) ---
	// Sessions dominate temporal groups (28 sessions on a single day = 378 edges).
	// Exclude sessions so temporal links only connect content documents.
	dateGroups := make(map[string][]string) // date → doc IDs
	for id, date := range dateMap {
		node, ok := nodeMap[id]
		if !ok || date == "" {
			continue
		}
		// Skip sessions — they cluster by date and create massive cliques
		if node.EntityType == "session" {
			continue
		}
		dateGroups[date] = append(dateGroups[date], id)
	}
	for _, group := range dateGroups {
		if len(group) < 2 {
			continue
		}
		sort.Strings(group)

		if len(group) <= 12 {
			// Small groups: full clique
			for i := 0; i < len(group); i++ {
				for j := i + 1; j < len(group); j++ {
					edges = append(edges, CogFieldEdge{
						Source:   group[i],
						Target:   group[j],
						Relation: "temporal",
						Weight:   0.15,
						Thread:   "temporal",
					})
				}
			}
		} else {
			// Large groups: chain to avoid O(n²)
			for i := 1; i < len(group); i++ {
				edges = append(edges, CogFieldEdge{
					Source:   group[i-1],
					Target:   group[i],
					Relation: "temporal",
					Weight:   0.1,
					Thread:   "temporal",
				})
			}
		}
	}

	// --- Add session + bus nodes via adapters ---
	root := wsRoot
	if root == "" {
		root, _, _ = ResolveWorkspace()
	}
	if root != "" {
		for _, adapter := range adapters {
			summaryNodes, summaryEdges := adapter.SummaryNodes(root)
			nodes = append(nodes, summaryNodes...)
			edges = append(edges, summaryEdges...)
		}
	}

	// --- Mark isolates ---
	// Nodes with zero visible (explicit/chain) edges get meta.isolate=true
	// so the frontend can dim or deprioritize them.
	connectedNodes := make(map[string]bool)
	for _, e := range edges {
		if e.Thread == "explicit" || e.Thread == "chain" {
			connectedNodes[e.Source] = true
			connectedNodes[e.Target] = true
		}
	}

	// Rebuild nodeMap since adapters may have added new nodes.
	// Deduplicate: adapters can produce duplicates (e.g. multiple event files for same session).
	seen := make(map[string]bool)
	var deduped []CogFieldNode
	for _, n := range nodes {
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		deduped = append(deduped, n)
	}
	nodes = deduped
	nodeMap = make(map[string]*CogFieldNode)
	for i := range nodes {
		nodeMap[nodes[i].ID] = &nodes[i]
	}

	isolateCount := 0
	for i := range nodes {
		if !connectedNodes[nodes[i].ID] {
			isolateCount++
			if nodes[i].Meta == nil {
				nodes[i].Meta = make(map[string]interface{})
			}
			nodes[i].Meta["isolate"] = true
			// Reduce strength of isolates so they render smaller
			nodes[i].Strength = nodes[i].Strength * 0.3
		}
	}
	log.Printf("cogfield: %d/%d nodes are isolates (no explicit/chain edges)", isolateCount, len(nodes))

	// --- Build stats ---
	nodesByType := make(map[string]int)
	nodesBySector := make(map[string]int)
	edgesByThread := make(map[string]int)
	type scored struct {
		id    string
		score int
	}
	var topNodes []scored

	for i := range nodes {
		nodesByType[nodes[i].EntityType]++
		nodesBySector[nodes[i].Sector]++
		topNodes = append(topNodes, scored{nodes[i].ID, nodes[i].BackrefCount})
	}

	for i := range edges {
		edgesByThread[edges[i].Thread]++
	}

	// Find top 10 most connected
	sort.Slice(topNodes, func(i, j int) bool {
		return topNodes[i].score > topNodes[j].score
	})
	mostConnected := make([]string, 0, 10)
	for i := 0; i < len(topNodes) && i < 10; i++ {
		if topNodes[i].score > 0 {
			mostConnected = append(mostConnected, topNodes[i].id)
		}
	}

	stats := CogFieldStats{
		TotalNodes:      len(nodes),
		TotalEdges:      len(edges),
		NodesByType:     nodesByType,
		NodesBySector:   nodesBySector,
		EdgesByRelation: edgesByRelation,
		EdgesByThread:   edgesByThread,
		MostConnected:   mostConnected,
	}

	return &CogFieldGraph{
		Nodes: nodes,
		Edges: edges,
		Stats: stats,
	}, nil
}

// sessionJSONLEvent is the union type for the two JSONL formats found in .cog/.state/events/
type sessionJSONLEvent struct {
	// Flat format fields
	ID        string `json:"id,omitempty"`
	Seq       int    `json:"seq,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Ts        string `json:"ts,omitempty"`
	Type      string `json:"type,omitempty"`

	// Hashed payload envelope format
	HashedPayload *struct {
		Type      string                 `json:"type"`
		Timestamp string                 `json:"timestamp"`
		SessionID string                 `json:"session_id"`
		Data      map[string]interface{} `json:"data"`
	} `json:"hashed_payload,omitempty"`
	Metadata *struct {
		Seq int `json:"seq"`
	} `json:"metadata,omitempty"`

	// Reconciler/bridge format
	Data map[string]interface{} `json:"data,omitempty"`
}

// buildSessionNodes scans .cog/.state/events/*.jsonl and creates a CogFieldNode per session file
func buildSessionNodes(root string) []CogFieldNode {
	eventsDir := filepath.Join(root, ".cog", ".state", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		return nil
	}

	var nodes []CogFieldNode
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}

		// Parse filename: {date}-{sessionID}.jsonl
		base := strings.TrimSuffix(name, ".jsonl")
		parts := strings.SplitN(base, "-", 4) // YYYY-MM-DD-sessionID
		var sessionID string
		if len(parts) >= 4 {
			sessionID = parts[3]
		} else {
			sessionID = base
		}

		fpath := filepath.Join(eventsDir, name)
		f, err := os.Open(fpath)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)

		var firstLine, lastLine string
		lineCount := 0
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			lineCount++
			if lineCount == 1 {
				firstLine = line
			}
			lastLine = line
		}
		f.Close()

		if lineCount == 0 {
			continue
		}

		// Extract timestamps from first/last lines
		var firstEvent, lastEvent sessionJSONLEvent
		json.Unmarshal([]byte(firstLine), &firstEvent)
		json.Unmarshal([]byte(lastLine), &lastEvent)

		created := extractTimestamp(firstEvent)
		modified := extractTimestamp(lastEvent)

		node := CogFieldNode{
			ID:         "session:" + sessionID,
			Label:      sessionID,
			EntityType: "session",
			Sector:     "sessions",
			Tags:       []string{},
			Created:    created,
			Modified:   modified,
			Strength:   math.Min(float64(lineCount)/20.0, 10.0),
			Meta: map[string]interface{}{
				"event_count": lineCount,
				"file":        name,
			},
		}
		nodes = append(nodes, node)
	}

	return nodes
}

// extractTimestamp pulls the timestamp from either JSONL format
func extractTimestamp(evt sessionJSONLEvent) string {
	if evt.HashedPayload != nil && evt.HashedPayload.Timestamp != "" {
		return evt.HashedPayload.Timestamp
	}
	if evt.Ts != "" {
		return evt.Ts
	}
	return ""
}

// busRegistryEntry matches the JSON format in .cog/.state/buses/registry.json
type busRegistryEntry struct {
	BusID        string   `json:"bus_id"`
	State        string   `json:"state"`
	Participants []string `json:"participants"`
	Transport    string   `json:"transport"`
	Endpoint     string   `json:"endpoint"`
	CreatedAt    string   `json:"created_at"`
	LastEventSeq int      `json:"last_event_seq"`
	LastEventAt  string   `json:"last_event_at"`
	EventCount   int      `json:"event_count"`
}

// handleCogFieldQuery handles GET /api/cogfield/query
// Builds the full graph then filters by type, sector, tag, min_strength,
// and optionally performs a BFS subgraph extraction from a given node.
func (s *serveServer) handleCogFieldQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()

	var c *constellation.Constellation
	var err error
	var wsRoot string
	if ws := workspaceFromRequest(r); ws != nil {
		c, err = getConstellationForWorkspace(ws.root)
		wsRoot = ws.root
	} else {
		c, err = getConstellation()
	}
	if err != nil {
		log.Printf("cogfield: query: failed to open constellation: %v", err)
		http.Error(w, "Failed to open constellation database", http.StatusInternalServerError)
		return
	}

	graph, err := buildCogFieldGraph(c, wsRoot)
	if err != nil {
		log.Printf("cogfield: query: failed to build graph: %v", err)
		http.Error(w, "Failed to build graph", http.StatusInternalServerError)
		return
	}

	// Parse filter params
	typeFilter := r.URL.Query().Get("type")
	sectorFilter := r.URL.Query().Get("sector")
	tagFilter := r.URL.Query().Get("tag")
	minStrengthStr := r.URL.Query().Get("min_strength")
	connectedTo := r.URL.Query().Get("connected")
	depthStr := r.URL.Query().Get("depth")

	// Build filter sets from comma-separated values
	typeSet := parseCSVSet(typeFilter)
	sectorSet := parseCSVSet(sectorFilter)
	tagSet := parseCSVSet(tagFilter)

	var minStrength float64
	if minStrengthStr != "" {
		if v, err := strconv.ParseFloat(minStrengthStr, 64); err == nil {
			minStrength = v
		}
	}

	depth := 1
	if depthStr != "" {
		if v, err := strconv.Atoi(depthStr); err == nil && v > 0 {
			depth = v
		}
	}

	// Filter nodes by type, sector, tag, and strength
	filtered := filterCogFieldNodes(graph.Nodes, typeSet, sectorSet, tagSet, minStrength)

	// If connected parameter is set, apply BFS subgraph extraction
	if connectedTo != "" {
		filtered = bfsCogFieldSubgraph(filtered, graph.Edges, connectedTo, depth)
	}

	// Build node ID set from filtered nodes
	nodeIDs := make(map[string]bool, len(filtered))
	for _, n := range filtered {
		nodeIDs[n.ID] = true
	}

	// Filter edges: keep only edges where both source and target are in filtered set
	var filteredEdges []CogFieldEdge
	for _, e := range graph.Edges {
		if nodeIDs[e.Source] && nodeIDs[e.Target] {
			filteredEdges = append(filteredEdges, e)
		}
	}

	// Compute stats for the filtered result
	stats := computeCogFieldStats(filtered, filteredEdges)

	result := CogFieldGraph{
		Nodes: filtered,
		Edges: filteredEdges,
		Stats: stats,
	}

	log.Printf("cogfield: query returned %d nodes, %d edges (from %d/%d) in %v",
		len(filtered), len(filteredEdges), graph.Stats.TotalNodes, graph.Stats.TotalEdges, time.Since(start))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// parseCSVSet splits a comma-separated string into a set for O(1) lookup.
// Returns nil if the input is empty (meaning "no filter").
func parseCSVSet(csv string) map[string]bool {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	set := make(map[string]bool, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			set[p] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// filterCogFieldNodes returns nodes matching all provided filters (AND logic).
// A nil/empty filter set means "accept all" for that dimension.
func filterCogFieldNodes(nodes []CogFieldNode, types, sectors, tags map[string]bool, minStrength float64) []CogFieldNode {
	var result []CogFieldNode
	for _, n := range nodes {
		// Type filter
		if len(types) > 0 && !types[n.EntityType] {
			continue
		}
		// Sector filter
		if len(sectors) > 0 && !sectors[n.Sector] {
			continue
		}
		// Tag filter: node must have at least one matching tag
		if len(tags) > 0 {
			match := false
			for _, t := range n.Tags {
				if tags[t] {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		// Strength filter
		if minStrength > 0 && n.Strength < minStrength {
			continue
		}
		result = append(result, n)
	}
	return result
}

// bfsCogFieldSubgraph performs a BFS from startID up to maxDepth hops,
// returning only nodes from the input set that are reachable.
// Edges are treated as undirected for traversal.
func bfsCogFieldSubgraph(nodes []CogFieldNode, edges []CogFieldEdge, startID string, maxDepth int) []CogFieldNode {
	// Build adjacency list from edges (both directions for undirected BFS)
	adj := make(map[string][]string)
	for _, e := range edges {
		adj[e.Source] = append(adj[e.Source], e.Target)
		adj[e.Target] = append(adj[e.Target], e.Source)
	}

	// Also build set of input node IDs so BFS only visits filtered nodes
	nodeSet := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		nodeSet[n.ID] = true
	}

	// BFS
	type bfsEntry struct {
		id    string
		depth int
	}
	visited := map[string]bool{startID: true}
	queue := []bfsEntry{{startID, 0}}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		if curr.depth >= maxDepth {
			continue
		}
		for _, neighbor := range adj[curr.id] {
			if !visited[neighbor] && nodeSet[neighbor] {
				visited[neighbor] = true
				queue = append(queue, bfsEntry{neighbor, curr.depth + 1})
			}
		}
	}

	// Return only visited nodes
	var result []CogFieldNode
	for _, n := range nodes {
		if visited[n.ID] {
			result = append(result, n)
		}
	}
	return result
}

// computeCogFieldStats builds a CogFieldStats from the given nodes and edges.
func computeCogFieldStats(nodes []CogFieldNode, edges []CogFieldEdge) CogFieldStats {
	nodesByType := make(map[string]int)
	nodesBySector := make(map[string]int)
	edgesByRelation := make(map[string]int)
	edgesByThread := make(map[string]int)

	type scored struct {
		id    string
		score int
	}
	var topNodes []scored

	for _, n := range nodes {
		nodesByType[n.EntityType]++
		nodesBySector[n.Sector]++
		topNodes = append(topNodes, scored{n.ID, n.BackrefCount})
	}

	for _, e := range edges {
		edgesByRelation[e.Relation]++
		edgesByThread[e.Thread]++
	}

	// Top 10 most connected
	sort.Slice(topNodes, func(i, j int) bool {
		return topNodes[i].score > topNodes[j].score
	})
	mostConnected := make([]string, 0, 10)
	for i := 0; i < len(topNodes) && i < 10; i++ {
		if topNodes[i].score > 0 {
			mostConnected = append(mostConnected, topNodes[i].id)
		}
	}

	return CogFieldStats{
		TotalNodes:      len(nodes),
		TotalEdges:      len(edges),
		NodesByType:     nodesByType,
		NodesBySector:   nodesBySector,
		EdgesByRelation: edgesByRelation,
		EdgesByThread:   edgesByThread,
		MostConnected:   mostConnected,
	}
}

// buildBusNodes reads .cog/.state/buses/registry.json and creates a CogFieldNode per bus
func buildBusNodes(root string) []CogFieldNode {
	registryPath := filepath.Join(root, ".cog", ".state", "buses", "registry.json")
	data, err := os.ReadFile(registryPath)
	if err != nil {
		return nil
	}

	var entries []busRegistryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}

	var nodes []CogFieldNode
	for _, bus := range entries {
		node := CogFieldNode{
			ID:         "bus:" + bus.BusID,
			Label:      bus.BusID,
			EntityType: "bus",
			Sector:     "buses",
			Tags:       bus.Participants,
			Created:    bus.CreatedAt,
			Modified:   bus.LastEventAt,
			Strength:   math.Min(float64(bus.EventCount)*2.0, 10.0),
			Meta: map[string]interface{}{
				"state":       bus.State,
				"event_count": bus.EventCount,
				"transport":   bus.Transport,
			},
		}
		nodes = append(nodes, node)
	}

	return nodes
}

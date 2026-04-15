// Package cogfield defines the CogField knowledge graph schema types.
//
// CogField is the workspace knowledge graph, built from constellation.db
// and runtime sources (buses, sessions, signals, components). These types
// define the graph structure shared between the kernel (which builds the
// graph) and consumers (frontends, adapters, conditions).
//
// The graph consists of:
//   - Nodes: documents, sessions, buses, signals, components, resources
//   - Edges: explicit references, shared tags, siblings, temporal links
//   - Stats: aggregate counts and most-connected nodes
//
// Type normalization maps 64+ document types to 11 CogField entity types.
// Sector inference determines memory sector from document paths.
package cogfield

import (
	"fmt"
	"sort"
	"strings"
)

// Node represents a single entity in the CogField knowledge graph.
type Node struct {
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

// Edge represents a directed relationship between two nodes.
type Edge struct {
	Source   string  `json:"source"`
	Target   string  `json:"target"`
	Relation string  `json:"relation"`
	Weight   float64 `json:"weight,omitempty"`
	Thread   string  `json:"thread"`
}

// Stats holds aggregate statistics for a CogField graph.
type Stats struct {
	TotalNodes      int            `json:"total_nodes"`
	TotalEdges      int            `json:"total_edges"`
	NodesByType     map[string]int `json:"nodes_by_type"`
	NodesBySector   map[string]int `json:"nodes_by_sector"`
	EdgesByRelation map[string]int `json:"edges_by_relation"`
	EdgesByThread   map[string]int `json:"edges_by_thread"`
	MostConnected   []string       `json:"most_connected"`
}

// Graph is the top-level CogField knowledge graph container.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
	Stats Stats  `json:"stats"`
}

// NormalizeEntityType maps constellation.db document types to CogField entity types.
// The 64+ document types in the DB are normalized to 11 canonical entity types.
func NormalizeEntityType(docType string) string {
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

// InferSector determines the memory sector from a document path and optional DB sector.
// Falls back to path-based inference when the DB sector is NULL (98% of docs).
func InferSector(path, dbSector string) string {
	if dbSector != "" {
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

	p := strings.ToLower(path)

	if strings.Contains(p, "/semantic/") || strings.HasPrefix(p, "semantic/") {
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

	if strings.Contains(p, "/adr/") || strings.HasPrefix(p, "adr/") {
		return "architecture"
	}
	if strings.Contains(p, "/ontology/") || strings.HasPrefix(p, "ontology/") {
		return "ontology"
	}

	return "semantic"
}

// StrengthFromMetrics calculates a 0-10 strength value from document substance metrics.
func StrengthFromMetrics(substanceRatio float64, refCount, wordCount int) float64 {
	strength := substanceRatio * 4.0

	if refCount > 10 {
		strength += 3.0
	} else if refCount > 5 {
		strength += 2.0
	} else if refCount > 0 {
		strength += 1.0
	}

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

// ParseCSVSet splits a comma-separated string into a set for O(1) lookup.
// Returns nil if the input is empty (meaning "no filter").
func ParseCSVSet(csv string) map[string]bool {
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

// FilterNodes returns nodes matching all provided filters (AND logic).
// A nil/empty filter set means "accept all" for that dimension.
func FilterNodes(nodes []Node, types, sectors, tags map[string]bool, minStrength float64) []Node {
	var result []Node
	for _, n := range nodes {
		if len(types) > 0 && !types[n.EntityType] {
			continue
		}
		if len(sectors) > 0 && !sectors[n.Sector] {
			continue
		}
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
		if minStrength > 0 && n.Strength < minStrength {
			continue
		}
		result = append(result, n)
	}
	return result
}

// BFSSubgraph performs a BFS from startID up to maxDepth hops,
// returning only nodes from the input set that are reachable.
// Edges are treated as undirected for traversal.
func BFSSubgraph(nodes []Node, edges []Edge, startID string, maxDepth int) []Node {
	adj := make(map[string][]string)
	for _, e := range edges {
		adj[e.Source] = append(adj[e.Source], e.Target)
		adj[e.Target] = append(adj[e.Target], e.Source)
	}

	nodeSet := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		nodeSet[n.ID] = true
	}

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

	var result []Node
	for _, n := range nodes {
		if visited[n.ID] {
			result = append(result, n)
		}
	}
	return result
}

// ComputeStats builds Stats from the given nodes and edges.
func ComputeStats(nodes []Node, edges []Edge) Stats {
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

	sort.Slice(topNodes, func(i, j int) bool {
		return topNodes[i].score > topNodes[j].score
	})
	mostConnected := make([]string, 0, 10)
	for i := 0; i < len(topNodes) && i < 10; i++ {
		if topNodes[i].score > 0 {
			mostConnected = append(mostConnected, topNodes[i].id)
		}
	}

	return Stats{
		TotalNodes:      len(nodes),
		TotalEdges:      len(edges),
		NodesByType:     nodesByType,
		NodesBySector:   nodesBySector,
		EdgesByRelation: edgesByRelation,
		EdgesByThread:   edgesByThread,
		MostConnected:   mostConnected,
	}
}

// FilterByMeta filters nodes whose Meta fields match the given criteria.
func FilterByMeta(nodes []Node, match map[string]string) []Node {
	var result []Node
	for _, n := range nodes {
		if n.Meta == nil {
			continue
		}
		allMatch := true
		for key, want := range match {
			got, ok := n.Meta[key]
			if !ok {
				allMatch = false
				break
			}
			if fmt.Sprintf("%v", got) != want {
				allMatch = false
				break
			}
		}
		if allMatch {
			result = append(result, n)
		}
	}
	return result
}

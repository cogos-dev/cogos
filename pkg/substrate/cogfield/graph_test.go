package cogfield

import (
	"encoding/json"
	"testing"
)

func TestNormalizeEntityType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"session", "session"},
		{"adr", "adr"},
		{"skill", "skill"},
		{"hook", "hook"},
		{"ontology", "ontology"},
		{"term", "ontology"},
		{"claim", "ontology"},
		{"pattern", "ontology"},
		{"theorem", "ontology"},
		{"principle", "ontology"},
		{"component", "component"},
		{"node", "node"},
		{"identity", "agent"},
		{"unknown", "document"},
		{"", "document"},
		{"random-type", "document"},
	}

	for _, tt := range tests {
		got := NormalizeEntityType(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeEntityType(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestInferSector(t *testing.T) {
	tests := []struct {
		path     string
		dbSector string
		want     string
	}{
		// DB sector takes precedence
		{"anything", "semantic", "semantic"},
		{"anything", "episodic", "episodic"},
		{"anything", "architecture", "architecture"},
		{"anything", "semantic/architecture", "architecture"},
		{"anything", "infrastructure", "infrastructure"},

		// Path-based inference
		{".cog/mem/semantic/insights/foo.md", "", "semantic"},
		{".cog/mem/semantic/architecture/foo.md", "", "architecture"},
		{".cog/mem/episodic/sessions/foo.md", "", "episodic"},
		{".cog/mem/procedural/guides/foo.md", "", "procedural"},
		{".cog/mem/reflective/retros/foo.md", "", "reflective"},
		{"identities/cog.md", "", "identities"},
		{"reference/tools.md", "", "reference"},
		{".cog/docs/adr/001.md", "", "architecture"},
		{".cog/ontology/crystal.md", "", "ontology"},

		// Default
		{"random/path/foo.md", "", "semantic"},

		// Unrecognized DB sector falls through to path
		{".cog/mem/episodic/foo.md", "garbage", "episodic"},
	}

	for _, tt := range tests {
		got := InferSector(tt.path, tt.dbSector)
		if got != tt.want {
			t.Errorf("InferSector(%q, %q) = %q, want %q", tt.path, tt.dbSector, got, tt.want)
		}
	}
}

func TestStrengthFromMetrics(t *testing.T) {
	// Zero everything
	if got := StrengthFromMetrics(0, 0, 0); got != 0 {
		t.Errorf("zero metrics = %f, want 0", got)
	}

	// Max substance, max refs, max words = capped at 10
	got := StrengthFromMetrics(1.0, 20, 2000)
	if got != 10 {
		t.Errorf("max metrics = %f, want 10", got)
	}

	// Mid-range: 0.5*4 + 1(refs>0) + 1(words>50) = 4
	got = StrengthFromMetrics(0.5, 3, 200)
	if got != 4 {
		t.Errorf("mid metrics = %f, want 4", got)
	}
}

func TestParseCSVSet(t *testing.T) {
	if got := ParseCSVSet(""); got != nil {
		t.Errorf("empty string should return nil, got %v", got)
	}

	got := ParseCSVSet("a,b,c")
	if len(got) != 3 || !got["a"] || !got["b"] || !got["c"] {
		t.Errorf("ParseCSVSet(\"a,b,c\") = %v", got)
	}

	// Spaces are trimmed
	got = ParseCSVSet(" x , y ")
	if !got["x"] || !got["y"] {
		t.Errorf("spaces not trimmed: %v", got)
	}
}

func TestFilterNodes(t *testing.T) {
	nodes := []Node{
		{ID: "1", EntityType: "session", Sector: "episodic", Tags: []string{"tag1"}, Strength: 5},
		{ID: "2", EntityType: "document", Sector: "semantic", Tags: []string{"tag2"}, Strength: 3},
		{ID: "3", EntityType: "session", Sector: "episodic", Tags: []string{"tag1", "tag2"}, Strength: 8},
	}

	// No filters returns all
	got := FilterNodes(nodes, nil, nil, nil, 0)
	if len(got) != 3 {
		t.Errorf("no filter: got %d, want 3", len(got))
	}

	// Type filter
	got = FilterNodes(nodes, map[string]bool{"session": true}, nil, nil, 0)
	if len(got) != 2 {
		t.Errorf("type=session: got %d, want 2", len(got))
	}

	// Sector filter
	got = FilterNodes(nodes, nil, map[string]bool{"semantic": true}, nil, 0)
	if len(got) != 1 || got[0].ID != "2" {
		t.Errorf("sector=semantic: got %v", got)
	}

	// Tag filter
	got = FilterNodes(nodes, nil, nil, map[string]bool{"tag2": true}, 0)
	if len(got) != 2 {
		t.Errorf("tag=tag2: got %d, want 2", len(got))
	}

	// Strength filter
	got = FilterNodes(nodes, nil, nil, nil, 6)
	if len(got) != 1 || got[0].ID != "3" {
		t.Errorf("min_strength=6: got %v", got)
	}
}

func TestBFSSubgraph(t *testing.T) {
	nodes := []Node{
		{ID: "A"}, {ID: "B"}, {ID: "C"}, {ID: "D"},
	}
	edges := []Edge{
		{Source: "A", Target: "B"},
		{Source: "B", Target: "C"},
		{Source: "C", Target: "D"},
	}

	// Depth 1 from A -> A, B
	got := BFSSubgraph(nodes, edges, "A", 1)
	if len(got) != 2 {
		t.Errorf("BFS depth=1 from A: got %d nodes, want 2", len(got))
	}

	// Depth 2 from A -> A, B, C
	got = BFSSubgraph(nodes, edges, "A", 2)
	if len(got) != 3 {
		t.Errorf("BFS depth=2 from A: got %d nodes, want 3", len(got))
	}

	// Depth 3 -> all
	got = BFSSubgraph(nodes, edges, "A", 3)
	if len(got) != 4 {
		t.Errorf("BFS depth=3 from A: got %d nodes, want 4", len(got))
	}
}

func TestComputeStats(t *testing.T) {
	nodes := []Node{
		{ID: "1", EntityType: "session", Sector: "episodic", BackrefCount: 5},
		{ID: "2", EntityType: "document", Sector: "semantic", BackrefCount: 3},
	}
	edges := []Edge{
		{Source: "1", Target: "2", Relation: "refs", Thread: "explicit"},
	}

	stats := ComputeStats(nodes, edges)
	if stats.TotalNodes != 2 {
		t.Errorf("TotalNodes = %d, want 2", stats.TotalNodes)
	}
	if stats.TotalEdges != 1 {
		t.Errorf("TotalEdges = %d, want 1", stats.TotalEdges)
	}
	if stats.NodesByType["session"] != 1 {
		t.Errorf("NodesByType[session] = %d, want 1", stats.NodesByType["session"])
	}
	if stats.EdgesByRelation["refs"] != 1 {
		t.Errorf("EdgesByRelation[refs] = %d, want 1", stats.EdgesByRelation["refs"])
	}
	if len(stats.MostConnected) != 2 || stats.MostConnected[0] != "1" {
		t.Errorf("MostConnected = %v, want [1, 2]", stats.MostConnected)
	}
}

func TestFilterByMeta(t *testing.T) {
	nodes := []Node{
		{ID: "1", Meta: map[string]interface{}{"status": "active", "count": float64(5)}},
		{ID: "2", Meta: map[string]interface{}{"status": "inactive"}},
		{ID: "3", Meta: nil},
	}

	got := FilterByMeta(nodes, map[string]string{"status": "active"})
	if len(got) != 1 || got[0].ID != "1" {
		t.Errorf("FilterByMeta status=active: got %v", got)
	}

	got = FilterByMeta(nodes, map[string]string{"count": "5"})
	if len(got) != 1 || got[0].ID != "1" {
		t.Errorf("FilterByMeta count=5: got %v", got)
	}
}

func TestNodeJSONRoundTrip(t *testing.T) {
	node := Node{
		ID:           "test-id",
		Label:        "Test Node",
		EntityType:   "document",
		Sector:       "semantic",
		Tags:         []string{"tag1", "tag2"},
		Created:      "2026-04-14T12:00:00Z",
		Modified:     "2026-04-14T13:00:00Z",
		BackrefCount: 3,
		Strength:     7.5,
		Meta:         map[string]interface{}{"key": "value"},
	}

	data, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Node
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != node.ID {
		t.Errorf("ID mismatch: %q != %q", decoded.ID, node.ID)
	}
	if decoded.Strength != node.Strength {
		t.Errorf("Strength mismatch: %f != %f", decoded.Strength, node.Strength)
	}
	if len(decoded.Tags) != 2 {
		t.Errorf("Tags count: %d != 2", len(decoded.Tags))
	}
}

func TestEdgeJSONRoundTrip(t *testing.T) {
	edge := Edge{
		Source:   "src",
		Target:   "tgt",
		Relation: "refs",
		Weight:   0.5,
		Thread:   "explicit",
	}

	data, err := json.Marshal(edge)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Edge
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Source != edge.Source || decoded.Target != edge.Target {
		t.Errorf("Edge mismatch")
	}
	if decoded.Weight != edge.Weight {
		t.Errorf("Weight mismatch: %f != %f", decoded.Weight, edge.Weight)
	}
}

func TestGraphJSONRoundTrip(t *testing.T) {
	graph := Graph{
		Nodes: []Node{{ID: "1", Label: "A", EntityType: "doc", Sector: "s", Tags: []string{}}},
		Edges: []Edge{{Source: "1", Target: "1", Relation: "self", Thread: "x"}},
		Stats: Stats{
			TotalNodes:      1,
			TotalEdges:      1,
			NodesByType:     map[string]int{"doc": 1},
			NodesBySector:   map[string]int{"s": 1},
			EdgesByRelation: map[string]int{"self": 1},
			EdgesByThread:   map[string]int{"x": 1},
			MostConnected:   []string{},
		},
	}

	data, err := json.Marshal(graph)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Graph
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Stats.TotalNodes != 1 {
		t.Errorf("Stats.TotalNodes = %d, want 1", decoded.Stats.TotalNodes)
	}
}

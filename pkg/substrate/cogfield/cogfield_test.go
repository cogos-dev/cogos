package cogfield_test

import (
	"reflect"
	"testing"
	"time"

	origin    "github.com/myrgic/cogos/pkg/cogfield"
	substrate "github.com/myrgic/cogos/pkg/substrate/cogfield"
)

// TestTypeIdentity verifies that type aliases in the substrate re-export layer
// are identical to their origin types via reflect.TypeOf.
func TestTypeIdentity(t *testing.T) {
	cases := []struct {
		name     string
		origin   reflect.Type
		reexport reflect.Type
	}{
		{"Node", reflect.TypeOf(origin.Node{}), reflect.TypeOf(substrate.Node{})},
		{"Edge", reflect.TypeOf(origin.Edge{}), reflect.TypeOf(substrate.Edge{})},
		{"Stats", reflect.TypeOf(origin.Stats{}), reflect.TypeOf(substrate.Stats{})},
		{"Graph", reflect.TypeOf(origin.Graph{}), reflect.TypeOf(substrate.Graph{})},
		{"Block", reflect.TypeOf(origin.Block{}), reflect.TypeOf(substrate.Block{})},
		{"GraphBlock", reflect.TypeOf(origin.GraphBlock{}), reflect.TypeOf(substrate.GraphBlock{})},
		{"AdapterNodeConfig", reflect.TypeOf(origin.AdapterNodeConfig{}), reflect.TypeOf(substrate.AdapterNodeConfig{})},
		{"BlockTypeConfig", reflect.TypeOf(origin.BlockTypeConfig{}), reflect.TypeOf(substrate.BlockTypeConfig{})},
		{"ExpandNodeResponse", reflect.TypeOf(origin.ExpandNodeResponse{}), reflect.TypeOf(substrate.ExpandNodeResponse{})},
		{"BusDetail", reflect.TypeOf(origin.BusDetail{}), reflect.TypeOf(substrate.BusDetail{})},
		{"BusRegistryEntry", reflect.TypeOf(origin.BusRegistryEntry{}), reflect.TypeOf(substrate.BusRegistryEntry{})},
		{"FieldCondition", reflect.TypeOf(origin.FieldCondition{}), reflect.TypeOf(substrate.FieldCondition{})},
		{"TriggeredCondition", reflect.TypeOf(origin.TriggeredCondition{}), reflect.TypeOf(substrate.TriggeredCondition{})},
		{"FieldConditionState", reflect.TypeOf(origin.FieldConditionState{}), reflect.TypeOf(substrate.FieldConditionState{})},
		{"DocRef", reflect.TypeOf(origin.DocRef{}), reflect.TypeOf(substrate.DocRef{})},
		{"DocumentDetail", reflect.TypeOf(origin.DocumentDetail{}), reflect.TypeOf(substrate.DocumentDetail{})},
		{"SessionJSONLEvent", reflect.TypeOf(origin.SessionJSONLEvent{}), reflect.TypeOf(substrate.SessionJSONLEvent{})},
		{"SessionMessage", reflect.TypeOf(origin.SessionMessage{}), reflect.TypeOf(substrate.SessionMessage{})},
		{"SessionDetail", reflect.TypeOf(origin.SessionDetail{}), reflect.TypeOf(substrate.SessionDetail{})},
		{"SignalFieldState", reflect.TypeOf(origin.SignalFieldState{}), reflect.TypeOf(substrate.SignalFieldState{})},
		{"PersistedSignal", reflect.TypeOf(origin.PersistedSignal{}), reflect.TypeOf(substrate.PersistedSignal{})},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.origin != tc.reexport {
				t.Errorf("%s: origin type %v != substrate type %v (alias is broken)", tc.name, tc.origin, tc.reexport)
			}
		})
	}
}

// TestNormalizeEntityType verifies the re-exported function maps types correctly.
func TestNormalizeEntityType(t *testing.T) {
	if got := substrate.NormalizeEntityType("session"); got != "session" {
		t.Errorf("NormalizeEntityType(\"session\") = %q, want %q", got, "session")
	}
	if got := substrate.NormalizeEntityType("unknown_type"); got != "document" {
		t.Errorf("NormalizeEntityType(\"unknown_type\") = %q, want %q", got, "document")
	}
}

// TestInferSector verifies the re-exported sector inference function.
func TestInferSector(t *testing.T) {
	got := substrate.InferSector("/mem/semantic/foo.md", "")
	if got != "semantic" {
		t.Errorf("InferSector = %q, want %q", got, "semantic")
	}
}

// TestStrengthFromMetrics verifies the re-exported strength calculation.
func TestStrengthFromMetrics(t *testing.T) {
	// substanceRatio=1.0, refCount=15, wordCount=1500 → 4+3+3=10
	got := substrate.StrengthFromMetrics(1.0, 15, 1500)
	if got != 10.0 {
		t.Errorf("StrengthFromMetrics = %v, want 10.0", got)
	}
}

// TestParseCSVSet verifies the re-exported CSV set parser.
func TestParseCSVSet(t *testing.T) {
	m := substrate.ParseCSVSet("a,b,c")
	if len(m) != 3 {
		t.Errorf("ParseCSVSet len = %d, want 3", len(m))
	}
	if !m["a"] || !m["b"] || !m["c"] {
		t.Errorf("ParseCSVSet missing expected keys: %v", m)
	}
	if substrate.ParseCSVSet("") != nil {
		t.Errorf("ParseCSVSet(\"\") should return nil")
	}
}

// TestFilterNodes verifies the re-exported filter function.
func TestFilterNodes(t *testing.T) {
	nodes := []substrate.Node{
		{ID: "n1", EntityType: "document", Sector: "semantic", Strength: 5.0},
		{ID: "n2", EntityType: "session", Sector: "episodic", Strength: 3.0},
	}
	filtered := substrate.FilterNodes(nodes, map[string]bool{"document": true}, nil, nil, 0)
	if len(filtered) != 1 || filtered[0].ID != "n1" {
		t.Errorf("FilterNodes = %v, want [{ID:n1}]", filtered)
	}
}

// TestComputeStats verifies the re-exported stats computation.
func TestComputeStats(t *testing.T) {
	nodes := []substrate.Node{
		{ID: "n1", EntityType: "document", Sector: "semantic"},
		{ID: "n2", EntityType: "session", Sector: "episodic"},
	}
	edges := []substrate.Edge{
		{Source: "n1", Target: "n2", Relation: "ref"},
	}
	stats := substrate.ComputeStats(nodes, edges)
	if stats.TotalNodes != 2 {
		t.Errorf("TotalNodes = %d, want 2", stats.TotalNodes)
	}
	if stats.TotalEdges != 1 {
		t.Errorf("TotalEdges = %d, want 1", stats.TotalEdges)
	}
}

// TestGraphBlockToNode verifies the re-exported block-to-node converter.
func TestGraphBlockToNode(t *testing.T) {
	gb := substrate.GraphBlock{
		URI:  "cog://bus/test/1",
		Type: "bus.message",
		Ts:   "2026-01-01T00:00:00Z",
	}
	n := substrate.GraphBlockToNode(gb)
	if n.ID != "cog://bus/test/1" {
		t.Errorf("Node.ID = %q, want %q", n.ID, "cog://bus/test/1")
	}
	if n.Sector != "buses" {
		t.Errorf("Node.Sector = %q, want %q", n.Sector, "buses")
	}
}

// TestSignalIsActive verifies the re-exported signal activity check.
func TestSignalIsActive(t *testing.T) {
	ps := &substrate.PersistedSignal{
		Strength:    10.0,
		DepositedAt: float64(time.Now().Unix()),
		HalfLife:    24.0, // 24 hour half-life
	}
	if !substrate.SignalIsActive(ps, time.Now()) {
		t.Errorf("SignalIsActive for fresh high-strength signal = false, want true")
	}
}

// TestExtractTimestamp verifies the re-exported timestamp extractor.
func TestExtractTimestamp(t *testing.T) {
	evt := substrate.SessionJSONLEvent{Ts: "2026-01-01T00:00:00Z"}
	got := substrate.ExtractTimestamp(evt)
	if got != "2026-01-01T00:00:00Z" {
		t.Errorf("ExtractTimestamp = %q, want %q", got, "2026-01-01T00:00:00Z")
	}
}

package cogfield_test

import (
	"reflect"
	"testing"
	"time"

	shim "github.com/myrgic/cogos/pkg/cogfield"
	canonical "github.com/myrgic/cogos/pkg/substrate/cogfield"
)

// TestTypeIdentity verifies that type aliases in the legacy re-export shim
// are identical to their canonical types via reflect.TypeOf.
func TestTypeIdentity(t *testing.T) {
	cases := []struct {
		name      string
		canonical reflect.Type
		shim      reflect.Type
	}{
		{"Node", reflect.TypeOf(canonical.Node{}), reflect.TypeOf(shim.Node{})},
		{"Edge", reflect.TypeOf(canonical.Edge{}), reflect.TypeOf(shim.Edge{})},
		{"Stats", reflect.TypeOf(canonical.Stats{}), reflect.TypeOf(shim.Stats{})},
		{"Graph", reflect.TypeOf(canonical.Graph{}), reflect.TypeOf(shim.Graph{})},
		{"Block", reflect.TypeOf(canonical.Block{}), reflect.TypeOf(shim.Block{})},
		{"GraphBlock", reflect.TypeOf(canonical.GraphBlock{}), reflect.TypeOf(shim.GraphBlock{})},
		{"AdapterNodeConfig", reflect.TypeOf(canonical.AdapterNodeConfig{}), reflect.TypeOf(shim.AdapterNodeConfig{})},
		{"BlockTypeConfig", reflect.TypeOf(canonical.BlockTypeConfig{}), reflect.TypeOf(shim.BlockTypeConfig{})},
		{"ExpandNodeResponse", reflect.TypeOf(canonical.ExpandNodeResponse{}), reflect.TypeOf(shim.ExpandNodeResponse{})},
		{"BusDetail", reflect.TypeOf(canonical.BusDetail{}), reflect.TypeOf(shim.BusDetail{})},
		{"BusRegistryEntry", reflect.TypeOf(canonical.BusRegistryEntry{}), reflect.TypeOf(shim.BusRegistryEntry{})},
		{"FieldCondition", reflect.TypeOf(canonical.FieldCondition{}), reflect.TypeOf(shim.FieldCondition{})},
		{"TriggeredCondition", reflect.TypeOf(canonical.TriggeredCondition{}), reflect.TypeOf(shim.TriggeredCondition{})},
		{"FieldConditionState", reflect.TypeOf(canonical.FieldConditionState{}), reflect.TypeOf(shim.FieldConditionState{})},
		{"DocRef", reflect.TypeOf(canonical.DocRef{}), reflect.TypeOf(shim.DocRef{})},
		{"DocumentDetail", reflect.TypeOf(canonical.DocumentDetail{}), reflect.TypeOf(shim.DocumentDetail{})},
		{"SessionJSONLEvent", reflect.TypeOf(canonical.SessionJSONLEvent{}), reflect.TypeOf(shim.SessionJSONLEvent{})},
		{"SessionMessage", reflect.TypeOf(canonical.SessionMessage{}), reflect.TypeOf(shim.SessionMessage{})},
		{"SessionDetail", reflect.TypeOf(canonical.SessionDetail{}), reflect.TypeOf(shim.SessionDetail{})},
		{"SignalFieldState", reflect.TypeOf(canonical.SignalFieldState{}), reflect.TypeOf(shim.SignalFieldState{})},
		{"PersistedSignal", reflect.TypeOf(canonical.PersistedSignal{}), reflect.TypeOf(shim.PersistedSignal{})},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.canonical != tc.shim {
				t.Errorf("%s: canonical type %v != shim type %v (alias is broken)", tc.name, tc.canonical, tc.shim)
			}
		})
	}
}

// TestNormalizeEntityType verifies the re-exported function maps types correctly.
func TestNormalizeEntityType(t *testing.T) {
	if got := shim.NormalizeEntityType("session"); got != "session" {
		t.Errorf("NormalizeEntityType(\"session\") = %q, want %q", got, "session")
	}
	if got := shim.NormalizeEntityType("unknown_type"); got != "document" {
		t.Errorf("NormalizeEntityType(\"unknown_type\") = %q, want %q", got, "document")
	}
}

// TestInferSector verifies the re-exported sector inference function.
func TestInferSector(t *testing.T) {
	got := shim.InferSector("/mem/semantic/foo.md", "")
	if got != "semantic" {
		t.Errorf("InferSector = %q, want %q", got, "semantic")
	}
}

// TestStrengthFromMetrics verifies the re-exported strength calculation.
func TestStrengthFromMetrics(t *testing.T) {
	// substanceRatio=1.0, refCount=15, wordCount=1500 -> 4+3+3=10
	got := shim.StrengthFromMetrics(1.0, 15, 1500)
	if got != 10.0 {
		t.Errorf("StrengthFromMetrics = %v, want 10.0", got)
	}
}

// TestParseCSVSet verifies the re-exported CSV set parser.
func TestParseCSVSet(t *testing.T) {
	m := shim.ParseCSVSet("a,b,c")
	if len(m) != 3 {
		t.Errorf("ParseCSVSet len = %d, want 3", len(m))
	}
	if !m["a"] || !m["b"] || !m["c"] {
		t.Errorf("ParseCSVSet missing expected keys: %v", m)
	}
	if shim.ParseCSVSet("") != nil {
		t.Errorf("ParseCSVSet(\"\") should return nil")
	}
}

// TestFilterNodes verifies the re-exported filter function.
func TestFilterNodes(t *testing.T) {
	nodes := []shim.Node{
		{ID: "n1", EntityType: "document", Sector: "semantic", Strength: 5.0},
		{ID: "n2", EntityType: "session", Sector: "episodic", Strength: 3.0},
	}
	filtered := shim.FilterNodes(nodes, map[string]bool{"document": true}, nil, nil, 0)
	if len(filtered) != 1 || filtered[0].ID != "n1" {
		t.Errorf("FilterNodes = %v, want [{ID:n1}]", filtered)
	}
}

// TestComputeStats verifies the re-exported stats computation.
func TestComputeStats(t *testing.T) {
	nodes := []shim.Node{
		{ID: "n1", EntityType: "document", Sector: "semantic"},
		{ID: "n2", EntityType: "session", Sector: "episodic"},
	}
	edges := []shim.Edge{
		{Source: "n1", Target: "n2", Relation: "ref"},
	}
	stats := shim.ComputeStats(nodes, edges)
	if stats.TotalNodes != 2 {
		t.Errorf("TotalNodes = %d, want 2", stats.TotalNodes)
	}
	if stats.TotalEdges != 1 {
		t.Errorf("TotalEdges = %d, want 1", stats.TotalEdges)
	}
}

// TestGraphBlockToNode verifies the re-exported block-to-node converter.
func TestGraphBlockToNode(t *testing.T) {
	gb := shim.GraphBlock{
		URI:  "cog://bus/test/1",
		Type: "bus.message",
		Ts:   "2026-01-01T00:00:00Z",
	}
	n := shim.GraphBlockToNode(gb)
	if n.ID != "cog://bus/test/1" {
		t.Errorf("Node.ID = %q, want %q", n.ID, "cog://bus/test/1")
	}
	if n.Sector != "buses" {
		t.Errorf("Node.Sector = %q, want %q", n.Sector, "buses")
	}
}

// TestSignalIsActive verifies the re-exported signal activity check.
func TestSignalIsActive(t *testing.T) {
	ps := &shim.PersistedSignal{
		Strength:    10.0,
		DepositedAt: float64(time.Now().Unix()),
		HalfLife:    24.0, // 24 hour half-life
	}
	if !shim.SignalIsActive(ps, time.Now()) {
		t.Errorf("SignalIsActive for fresh high-strength signal = false, want true")
	}
}

// TestExtractTimestamp verifies the re-exported timestamp extractor.
func TestExtractTimestamp(t *testing.T) {
	evt := shim.SessionJSONLEvent{Ts: "2026-01-01T00:00:00Z"}
	got := shim.ExtractTimestamp(evt)
	if got != "2026-01-01T00:00:00Z" {
		t.Errorf("ExtractTimestamp = %q, want %q", got, "2026-01-01T00:00:00Z")
	}
}

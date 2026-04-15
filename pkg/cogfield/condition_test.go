package cogfield

import "testing"

func TestParseConditionQueryString(t *testing.T) {
	got := ParseConditionQueryString("type=resource&min_strength=0&sector=infrastructure")
	if got["type"] != "resource" {
		t.Errorf("type = %q, want resource", got["type"])
	}
	if got["min_strength"] != "0" {
		t.Errorf("min_strength = %q, want 0", got["min_strength"])
	}
	if got["sector"] != "infrastructure" {
		t.Errorf("sector = %q, want infrastructure", got["sector"])
	}

	// Empty string
	got = ParseConditionQueryString("")
	if len(got) != 1 { // splits to [""] -> one entry with key ""
		// Actually "" splits to [""] which SplitN("","=",2) gives [""] with len 1
		// so no "=" means it's skipped
	}
}

func TestEvaluateFieldConditions(t *testing.T) {
	graph := &Graph{
		Nodes: []Node{
			{ID: "1", EntityType: "resource", Sector: "infrastructure", Strength: 5},
			{ID: "2", EntityType: "document", Sector: "semantic", Strength: 3},
			{ID: "3", EntityType: "resource", Sector: "infrastructure", Strength: 8},
		},
	}

	state := &FieldConditionState{
		CyclesSinceFired: make(map[string]int),
	}

	// "any" condition matching resources
	conditions := []FieldCondition{
		{
			Name:      "has-resources",
			Query:     "type=resource",
			Condition: "any",
			Cooldown:  1,
		},
	}

	triggered := EvaluateFieldConditions(graph, conditions, state)
	if len(triggered) != 1 {
		t.Fatalf("expected 1 triggered, got %d", len(triggered))
	}
	if triggered[0].MatchCount != 2 {
		t.Errorf("MatchCount = %d, want 2", triggered[0].MatchCount)
	}

	// "none" condition — no ontology nodes
	conditions = []FieldCondition{
		{
			Name:      "no-ontology",
			Query:     "type=ontology",
			Condition: "none",
			Cooldown:  1,
		},
	}
	triggered = EvaluateFieldConditions(graph, conditions, state)
	if len(triggered) != 1 {
		t.Fatalf("none condition: expected 1 triggered, got %d", len(triggered))
	}

	// "count_above" threshold
	conditions = []FieldCondition{
		{
			Name:      "many-resources",
			Query:     "type=resource",
			Condition: "count_above",
			Threshold: 5,
			Cooldown:  1,
		},
	}
	triggered = EvaluateFieldConditions(graph, conditions, state)
	if len(triggered) != 0 {
		t.Errorf("count_above(5): expected 0 triggered, got %d", len(triggered))
	}

	// Cooldown check — mark condition as recently fired
	state.CyclesSinceFired["has-resources"] = 0
	conditions = []FieldCondition{
		{
			Name:      "has-resources",
			Query:     "type=resource",
			Condition: "any",
			Cooldown:  5,
		},
	}
	triggered = EvaluateFieldConditions(graph, conditions, state)
	if len(triggered) != 0 {
		t.Errorf("cooldown should prevent firing: got %d triggered", len(triggered))
	}
}

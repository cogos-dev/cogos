package cogfield

import (
	"fmt"
	"strings"
)

// FieldCondition defines a reactive condition on the field graph.
type FieldCondition struct {
	Name      string            `yaml:"name" json:"name"`
	Query     string            `yaml:"query" json:"query"`
	Condition string            `yaml:"condition" json:"condition"`
	Match     map[string]string `yaml:"match,omitempty" json:"match,omitempty"`
	Threshold int               `yaml:"threshold,omitempty" json:"threshold,omitempty"`
	Cooldown  int               `yaml:"cooldown" json:"cooldown"`
	Handler   string            `yaml:"handler" json:"handler"`
	Context   string            `yaml:"context,omitempty" json:"context,omitempty"`
}

// TriggeredCondition represents a condition that matched during evaluation.
type TriggeredCondition struct {
	Condition    FieldCondition
	MatchedNodes []Node
	MatchCount   int
}

// FieldConditionState tracks cooldown counters per condition.
type FieldConditionState struct {
	CyclesSinceFired map[string]int // condition name -> cycles since last fire
}

// ParseConditionQueryString parses a simple query string (key=value&key=value).
func ParseConditionQueryString(query string) map[string]string {
	result := make(map[string]string)
	for _, pair := range strings.Split(query, "&") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

// EvaluateFieldConditions checks all conditions against the current field graph.
func EvaluateFieldConditions(graph *Graph, conditions []FieldCondition, state *FieldConditionState) []TriggeredCondition {
	var triggered []TriggeredCondition

	for _, cond := range conditions {
		cycles, exists := state.CyclesSinceFired[cond.Name]
		if exists && cycles < cond.Cooldown {
			continue
		}

		params := ParseConditionQueryString(cond.Query)

		typeSet := ParseCSVSet(params["type"])
		sectorSet := ParseCSVSet(params["sector"])
		tagSet := ParseCSVSet(params["tag"])
		var minStrength float64
		if v, ok := params["min_strength"]; ok {
			fmt.Sscanf(v, "%f", &minStrength)
		}

		matched := FilterNodes(graph.Nodes, typeSet, sectorSet, tagSet, minStrength)

		if cond.Condition == "any_where" && len(cond.Match) > 0 {
			matched = FilterByMeta(matched, cond.Match)
		}

		fire := false
		switch cond.Condition {
		case "any", "any_where":
			fire = len(matched) > 0
		case "none":
			fire = len(matched) == 0
		case "count_above":
			fire = len(matched) > cond.Threshold
		case "count_below":
			fire = len(matched) < cond.Threshold
		default:
			fire = len(matched) > 0
		}

		if fire {
			triggered = append(triggered, TriggeredCondition{
				Condition:    cond,
				MatchedNodes: matched,
				MatchCount:   len(matched),
			})
		}
	}

	return triggered
}

// cogfield_conditions.go - Field-reactive hooks for CogOS
//
// Defines field conditions that are evaluated during the reconcile watch loop.
// When a condition matches, it triggers a hook via the existing dispatch system.
//
// Conditions are defined in hook-config.yaml under `field_conditions:` and use
// the same query syntax as GET /api/cogfield/query (type, sector, tag, min_strength).
//
// Condition types:
//   - any:          fires when any nodes match the query
//   - none:         fires when no nodes match the query
//   - count_above:  fires when matched node count exceeds threshold
//   - count_below:  fires when matched node count is below threshold
//   - any_where:    fires when any nodes match query AND meta field criteria

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FieldCondition defines a reactive condition on the field graph.
type FieldCondition struct {
	Name      string            `yaml:"name" json:"name"`
	Query     string            `yaml:"query" json:"query"`                                // Query params (e.g. "type=resource&min_strength=0")
	Condition string            `yaml:"condition" json:"condition"`                         // "any", "none", "count_above", "count_below", "any_where"
	Match     map[string]string `yaml:"match,omitempty" json:"match,omitempty"`             // For any_where: meta field matches
	Threshold int               `yaml:"threshold,omitempty" json:"threshold,omitempty"`     // For count_above/below
	Cooldown  int               `yaml:"cooldown" json:"cooldown"`                           // Cycles between firings
	Handler   string            `yaml:"handler" json:"handler"`                             // Hook handler path
	Context   string            `yaml:"context,omitempty" json:"context,omitempty"`         // Hook context (main/subtask/exploration)
}

// TriggeredCondition represents a condition that matched.
type TriggeredCondition struct {
	Condition    FieldCondition
	MatchedNodes []CogFieldNode
	MatchCount   int
}

// FieldConditionState tracks cooldown counters per condition.
type FieldConditionState struct {
	CyclesSinceFired map[string]int // condition name -> cycles since last fire
}

// LoadFieldConditions reads field conditions from hook-config.yaml.
func LoadFieldConditions(root string) ([]FieldCondition, error) {
	configPath := filepath.Join(root, ".cog", "hooks", "hook-config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil // No config = no conditions, not an error
	}

	// Parse the top-level YAML to find field_conditions section
	var config map[string]interface{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse hook config: %w", err)
	}

	fcRaw, ok := config["field_conditions"]
	if !ok {
		return nil, nil // No field conditions defined
	}

	// Re-marshal and unmarshal the field_conditions section into our type
	fcBytes, err := yaml.Marshal(fcRaw)
	if err != nil {
		return nil, fmt.Errorf("marshal field_conditions: %w", err)
	}

	// field_conditions is a map of name -> condition
	var condMap map[string]FieldCondition
	if err := yaml.Unmarshal(fcBytes, &condMap); err != nil {
		return nil, fmt.Errorf("parse field_conditions: %w", err)
	}

	var conditions []FieldCondition
	for name, cond := range condMap {
		cond.Name = name
		if cond.Cooldown == 0 {
			cond.Cooldown = 1 // Minimum 1 cycle between firings
		}
		conditions = append(conditions, cond)
	}
	return conditions, nil
}

// EvaluateFieldConditions checks all conditions against the current field graph.
func EvaluateFieldConditions(graph *CogFieldGraph, conditions []FieldCondition, state *FieldConditionState) []TriggeredCondition {
	var triggered []TriggeredCondition

	for _, cond := range conditions {
		// Check cooldown
		cycles, exists := state.CyclesSinceFired[cond.Name]
		if exists && cycles < cond.Cooldown {
			continue
		}

		// Parse query into filter sets.
		// The query string uses the same format as the /api/cogfield/query endpoint.
		params := parseConditionQueryString(cond.Query)

		typeSet := parseCSVSet(params["type"])
		sectorSet := parseCSVSet(params["sector"])
		tagSet := parseCSVSet(params["tag"])
		var minStrength float64
		if v, ok := params["min_strength"]; ok {
			fmt.Sscanf(v, "%f", &minStrength)
		}

		// Filter nodes using the shared filter from cogfield.go
		matched := filterCogFieldNodes(graph.Nodes, typeSet, sectorSet, tagSet, minStrength)

		// Apply any_where meta matching
		if cond.Condition == "any_where" && len(cond.Match) > 0 {
			matched = filterByMeta(matched, cond.Match)
		}

		// Evaluate condition
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
			fire = len(matched) > 0 // Default to "any"
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

// EvaluateAndDispatchFieldConditions runs field condition checks.
// Called at the end of each reconcile cycle. Zero cost if no conditions are loaded.
func EvaluateAndDispatchFieldConditions(root string, conditions []FieldCondition, state *FieldConditionState) {
	if len(conditions) == 0 {
		return
	}

	// Increment all cooldown counters
	for name := range state.CyclesSinceFired {
		state.CyclesSinceFired[name]++
	}

	// Build field graph via constellation singleton
	c, err := getConstellation()
	if err != nil {
		log.Printf("[Field] Failed to open constellation for condition eval: %v", err)
		return
	}

	graph, err := buildCogFieldGraph(c)
	if err != nil {
		log.Printf("[Field] Failed to build graph for condition eval: %v", err)
		return
	}

	// Evaluate conditions
	triggered := EvaluateFieldConditions(graph, conditions, state)

	// Dispatch triggered conditions as hook events
	for _, t := range triggered {
		log.Printf("[Field] Condition '%s' triggered: %d nodes matched", t.Condition.Name, t.MatchCount)

		// Reset cooldown
		state.CyclesSinceFired[t.Condition.Name] = 0

		// Dispatch via hook system
		dispatchFieldConditionHook(root, t)
	}
}

// parseConditionQueryString parses a simple query string (key=value&key=value).
func parseConditionQueryString(query string) map[string]string {
	result := make(map[string]string)
	for _, pair := range strings.Split(query, "&") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

// filterByMeta filters nodes whose Meta fields match the given criteria.
func filterByMeta(nodes []CogFieldNode, match map[string]string) []CogFieldNode {
	var result []CogFieldNode
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

// dispatchFieldConditionHook dispatches a triggered condition through the hook system.
func dispatchFieldConditionHook(root string, t TriggeredCondition) {
	// Build event data for the hook
	eventData := map[string]interface{}{
		"condition":   t.Condition.Name,
		"match_count": t.MatchCount,
		"query":       t.Condition.Query,
		"handler":     t.Condition.Handler,
	}

	// Add matched node IDs (not full nodes -- keep it lean)
	nodeIDs := make([]string, 0, len(t.MatchedNodes))
	for _, n := range t.MatchedNodes {
		nodeIDs = append(nodeIDs, n.ID)
	}
	eventData["matched_node_ids"] = nodeIDs

	data, _ := json.Marshal(eventData)

	// Write event data to temp file for hook to read
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("cogfield-condition-%s.json", t.Condition.Name))
	os.WriteFile(tmpFile, data, 0644)
	defer os.Remove(tmpFile)

	// Resolve handler path
	handlerPath := t.Condition.Handler
	if !filepath.IsAbs(handlerPath) {
		handlerPath = filepath.Join(root, ".cog", "hooks", handlerPath)
	}

	// Check if handler exists as a file; if not, try as a .d directory
	if info, err := os.Stat(handlerPath); err != nil || !info.IsDir() {
		if !strings.HasSuffix(handlerPath, ".d") {
			dirPath := handlerPath + ".d"
			if info, err := os.Stat(dirPath); err == nil && info.IsDir() {
				handlerPath = dirPath
			}
		}
	}

	// Emit a reconcile event for observability
	EmitReconcileEvent("cog.field.condition.triggered", t.Condition.Name, map[string]any{
		"condition":   t.Condition.Name,
		"match_count": t.MatchCount,
		"query":       t.Condition.Query,
	})

	// Log the trigger with full context.
	// Full dispatch.py integration is a follow-up; the critical piece is
	// condition evaluation and trigger mechanism.
	log.Printf("[Field] Dispatching hook: condition=%s handler=%s matched=%d data=%s",
		t.Condition.Name, handlerPath, t.MatchCount, string(data))
}

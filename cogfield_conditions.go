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

	"github.com/cogos-dev/cogos/pkg/cogfield"
	"github.com/cogos-dev/cogos/sdk/constellation"
	"gopkg.in/yaml.v3"
)

// Type aliases — canonical types live in pkg/cogfield.
type FieldCondition = cogfield.FieldCondition
type TriggeredCondition = cogfield.TriggeredCondition
type FieldConditionState = cogfield.FieldConditionState

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

// EvaluateFieldConditions delegates to pkg/cogfield.
var EvaluateFieldConditions = cogfield.EvaluateFieldConditions

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

	// Build field graph via workspace-specific constellation
	var c *constellation.Constellation
	var err error
	if root != "" {
		c, err = getConstellationForWorkspace(root)
	} else {
		c, err = getConstellation()
	}
	if err != nil {
		log.Printf("[Field] Failed to open constellation for condition eval: %v", err)
		return
	}

	graph, err := buildCogFieldGraph(c, root)
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

// parseConditionQueryString delegates to pkg/cogfield.
var parseConditionQueryString = cogfield.ParseConditionQueryString

// filterByMeta delegates to pkg/cogfield.
var filterByMeta = cogfield.FilterByMeta

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

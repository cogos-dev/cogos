// cogfield_reconcile.go — ReconcileAdapter for CogField graph visualization.
//
// Projects reconcile resources (providers, state, dependencies) into the
// cognitive field as infrastructure nodes. Each registered provider becomes
// a "resource" node; expanding it reveals individual "resource.item" nodes.
//
// Implements BlockAdapter from cogfield_adapters.go.

package main

import (
	"fmt"
	"strings"
)

// ReconcileAdapter produces reconcile resource entities for the cognitive field.
type ReconcileAdapter struct{}

func (a *ReconcileAdapter) ID() string { return "reconcile" }

func (a *ReconcileAdapter) NodeConfig() AdapterNodeConfig {
	return AdapterNodeConfig{
		BlockTypes: map[string]BlockTypeConfig{
			"resource":      {EntityType: "resource", Shape: "diamond", Color: "#10b981", Label: "Resource"},
			"resource.item": {EntityType: "resource.item", Shape: "square", Color: "#34d399", Label: "Resource Item"},
		},
		DefaultSector: "infrastructure",
		ChainThread:   "explicit",
	}
}

// SummaryNodes reads registered providers and their state to produce resource nodes.
func (a *ReconcileAdapter) SummaryNodes(root string) ([]CogFieldNode, []CogFieldEdge) {
	providerNames := ListProviders()
	if len(providerNames) == 0 {
		return nil, nil
	}

	// Load meta config for dependency info (best-effort)
	metaCfg, _ := loadMetaConfig(root)
	depMap := make(map[string][]string)       // name -> depends_on
	if metaCfg != nil {
		for _, r := range metaCfg.Resources {
			depMap[r.Name] = r.DependsOn
		}
	}

	var nodes []CogFieldNode
	var edges []CogFieldEdge

	for _, name := range providerNames {
		// Try loading state
		state, _ := LoadReconcileState(root, name)

		// Get health from provider
		var syncStr, healthStr, opStr, message string
		var strength float64

		provider, err := GetProvider(name)
		if err == nil {
			status := provider.Health()
			syncStr = string(status.Sync)
			healthStr = string(status.Health)
			opStr = string(status.Operation)
			message = status.Message
		} else {
			syncStr = string(SyncStatusUnknown)
			healthStr = string(HealthMissing)
			opStr = string(OperationIdle)
		}

		// Strength based on sync status
		switch SyncStatus(syncStr) {
		case SyncStatusSynced:
			strength = 8.0
		case SyncStatusOutOfSync:
			strength = 5.0
		default:
			strength = 3.0
		}
		if healthStr == string(HealthMissing) {
			strength = 2.0
		}

		meta := map[string]interface{}{
			"sync_status":   syncStr,
			"health_status": healthStr,
			"operation_phase": opStr,
			"message":       message,
		}

		if state != nil {
			meta["resource_count"] = len(state.Resources)
			meta["lineage"] = state.Lineage
			meta["serial"] = state.Serial
			meta["last_refreshed"] = state.GeneratedAt
		} else {
			meta["resource_count"] = 0
		}

		node := CogFieldNode{
			ID:         "resource:" + name,
			Label:      name,
			EntityType: "resource",
			Sector:     "infrastructure",
			Tags:       []string{name, syncStr},
			Strength:   strength,
			Meta:       meta,
		}
		if state != nil {
			node.Modified = state.GeneratedAt
		}

		nodes = append(nodes, node)
	}

	// Create depends_on edges from meta config
	for _, name := range providerNames {
		for _, dep := range depMap[name] {
			edges = append(edges, CogFieldEdge{
				Source:   "resource:" + name,
				Target:   "resource:" + dep,
				Relation: "depends_on",
				Weight:   1.0,
				Thread:   "explicit",
			})
		}

		// Best-effort manages edge: resource -> component
		edges = append(edges, CogFieldEdge{
			Source:   "resource:" + name,
			Target:   "component:" + name,
			Relation: "manages",
			Weight:   0.5,
			Thread:   "explicit",
		})
	}

	return nodes, edges
}

// ExpandNode expands a resource node into its individual resource items.
func (a *ReconcileAdapter) ExpandNode(root, nodeID string) ([]CogFieldNode, []CogFieldEdge, error) {
	if !strings.HasPrefix(nodeID, "resource:") {
		return nil, nil, fmt.Errorf("not a resource node: %s", nodeID)
	}
	name := strings.TrimPrefix(nodeID, "resource:")

	// Don't expand sub-items
	if strings.Contains(name, ":") {
		return nil, nil, fmt.Errorf("cannot expand resource item: %s", nodeID)
	}

	state, err := LoadReconcileState(root, name)
	if err != nil {
		return nil, nil, fmt.Errorf("load state for %s: %w", name, err)
	}
	if state == nil {
		return nil, nil, fmt.Errorf("no state found for resource %s", name)
	}
	if len(state.Resources) == 0 {
		return nil, nil, fmt.Errorf("no resources in state for %s", name)
	}

	var nodes []CogFieldNode
	var edges []CogFieldEdge

	for _, res := range state.Resources {
		// Strength based on mode
		strength := 3.0
		switch res.Mode {
		case ModeManaged:
			strength = 6.0
		case ModeUnmanaged:
			strength = 3.0
		case ModeData:
			strength = 2.0
		}

		meta := map[string]interface{}{
			"type":           res.Type,
			"mode":           string(res.Mode),
			"external_id":    res.ExternalID,
			"attributes":     res.Attributes,
			"parent":         res.ParentAddress,
			"last_refreshed": res.LastRefreshed,
		}

		itemID := fmt.Sprintf("resource:%s:%s", name, res.Address)

		node := CogFieldNode{
			ID:         itemID,
			Label:      res.Name,
			EntityType: "resource.item",
			Sector:     "infrastructure",
			Tags:       []string{res.Type, string(res.Mode)},
			Modified:   res.LastRefreshed,
			Strength:   strength,
			Meta:       meta,
		}
		nodes = append(nodes, node)

		// Edge: item -> parent resource node
		edges = append(edges, CogFieldEdge{
			Source:   itemID,
			Target:   nodeID,
			Relation: "part_of",
			Weight:   1.0,
			Thread:   "explicit",
		})

		// Edge: item -> parent item if ParentAddress is set
		if res.ParentAddress != "" {
			edges = append(edges, CogFieldEdge{
				Source:   itemID,
				Target:   fmt.Sprintf("resource:%s:%s", name, res.ParentAddress),
				Relation: "child_of",
				Weight:   0.8,
				Thread:   "explicit",
			})
		}
	}

	return nodes, edges, nil
}

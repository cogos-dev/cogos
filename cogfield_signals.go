// cogfield_signals.go — SignalAdapter for CogField graph visualization.
//
// Reads the signal field state (.cog/run/signals/field_state.json) and produces
// signal nodes grouped by location for the cognitive field.
//
// Implements BlockAdapter from cogfield_adapters.go.

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cogos-dev/cogos/pkg/cogfield"
)

// Type aliases — canonical types live in pkg/cogfield.
type signalFieldState = cogfield.SignalFieldState
type persistedSignal = cogfield.PersistedSignal

// SignalAdapter produces signal entities for the cognitive field.
type SignalAdapter struct{}

func (a *SignalAdapter) ID() string { return "signal" }

func (a *SignalAdapter) NodeConfig() AdapterNodeConfig {
	return AdapterNodeConfig{
		BlockTypes: map[string]BlockTypeConfig{
			"signal.location": {EntityType: "signal.location", Shape: "ring", Color: "#f59e0b", Label: "Location"},
			"signal":          {EntityType: "signal", Shape: "ring", Color: "#fbbf24", Label: "Signal"},
		},
		DefaultSector: "signals",
		ChainThread:   "explicit",
	}
}

// SummaryNodes reads the signal field state and produces location + signal nodes.
func (a *SignalAdapter) SummaryNodes(root string) ([]CogFieldNode, []CogFieldEdge) {
	state, err := loadSignalFieldState(root)
	if err != nil {
		return nil, nil // No signal state yet — graceful degradation
	}

	now := time.Now()
	var nodes []CogFieldNode
	var edges []CogFieldEdge

	for location, sigMap := range state.Signals {
		// Count active signals at this location
		activeCount := 0
		for _, ps := range sigMap {
			if signalIsActive(ps, now) {
				activeCount++
			}
		}

		// Location node
		locNodeID := "signal:location:" + location
		nodes = append(nodes, CogFieldNode{
			ID:         locNodeID,
			Label:      location,
			EntityType: "signal.location",
			Sector:     "signals",
			Tags:       strings.Split(location, "."),
			Strength:   float64(activeCount),
			Meta: map[string]any{
				"signal_count": len(sigMap),
				"active_count": activeCount,
			},
		})

		// Individual signal nodes
		for sigType, ps := range sigMap {
			sigNodeID := fmt.Sprintf("signal:%s:%s", location, sigType)
			depositedAt := time.Unix(int64(ps.DepositedAt), 0)
			relevance := computeRelevance(ps, now)

			tags := []string{sigType}
			for _, part := range strings.Split(location, ".") {
				if part != sigType {
					tags = append(tags, part)
				}
			}

			meta := map[string]any{
				"deposited_by": ps.DepositedBy,
				"decay_type":   ps.DecayType,
				"half_life":    ps.HalfLife,
				"strength":     ps.Strength,
				"relevance":    math.Round(relevance*1000) / 1000,
			}
			if ps.Metadata != nil {
				meta["metadata"] = ps.Metadata
			}

			nodes = append(nodes, CogFieldNode{
				ID:         sigNodeID,
				Label:      sigType,
				EntityType: "signal",
				Sector:     "signals",
				Tags:       tags,
				Created:    depositedAt.Format(time.RFC3339),
				Modified:   depositedAt.Format(time.RFC3339),
				Strength:   math.Min(relevance*10, 10),
				Meta:       meta,
			})

			// Edge: signal → location
			edges = append(edges, CogFieldEdge{
				Source:   sigNodeID,
				Target:   locNodeID,
				Relation: "at",
				Weight:   1.0,
				Thread:   "explicit",
			})
		}
	}

	return nodes, edges
}

// ExpandNode returns detailed signal nodes for a location.
func (a *SignalAdapter) ExpandNode(root, nodeID string) ([]CogFieldNode, []CogFieldEdge, error) {
	if !strings.HasPrefix(nodeID, "signal:location:") {
		return nil, nil, fmt.Errorf("not an expandable signal node: %s", nodeID)
	}

	location := strings.TrimPrefix(nodeID, "signal:location:")

	state, err := loadSignalFieldState(root)
	if err != nil {
		return nil, nil, fmt.Errorf("load signal field: %w", err)
	}

	sigMap, ok := state.Signals[location]
	if !ok {
		return nil, nil, fmt.Errorf("no signals at location %s", location)
	}

	now := time.Now()
	var nodes []CogFieldNode
	var edges []CogFieldEdge

	for sigType, ps := range sigMap {
		sigNodeID := fmt.Sprintf("signal:%s:%s", location, sigType)
		depositedAt := time.Unix(int64(ps.DepositedAt), 0)
		relevance := computeRelevance(ps, now)

		meta := map[string]any{
			"deposited_by": ps.DepositedBy,
			"deposited_at": depositedAt.Format(time.RFC3339),
			"decay_type":   ps.DecayType,
			"half_life":    ps.HalfLife,
			"strength":     ps.Strength,
			"relevance":    math.Round(relevance*1000) / 1000,
			"active":       signalIsActive(ps, now),
		}
		if ps.Metadata != nil {
			meta["metadata"] = ps.Metadata
		}

		nodes = append(nodes, CogFieldNode{
			ID:         sigNodeID,
			Label:      sigType,
			EntityType: "signal",
			Sector:     "signals",
			Tags:       append([]string{sigType}, strings.Split(location, ".")...),
			Created:    depositedAt.Format(time.RFC3339),
			Modified:   depositedAt.Format(time.RFC3339),
			Strength:   math.Min(relevance*10, 10),
			Meta:       meta,
		})

		edges = append(edges, CogFieldEdge{
			Source:   sigNodeID,
			Target:   nodeID,
			Relation: "at",
			Weight:   1.0,
			Thread:   "explicit",
		})
	}

	return nodes, edges, nil
}

// --- Helpers ---

// loadSignalFieldState reads and parses the signal field state JSON.
func loadSignalFieldState(root string) (*signalFieldState, error) {
	path := filepath.Join(root, ".cog", "run", "signals", "field_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state signalFieldState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse signal field state: %w", err)
	}
	return &state, nil
}

// computeRelevance delegates to pkg/cogfield.
var computeRelevance = cogfield.ComputeRelevance

// signalIsActive delegates to pkg/cogfield.
var signalIsActive = cogfield.SignalIsActive

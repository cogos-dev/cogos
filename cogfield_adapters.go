// cogfield_adapters.go - GraphBlock adapter layer for CogField
//
// Defines the BlockAdapter interface and implementations for bus and session
// data sources. Each adapter can produce summary nodes for the main graph
// and expand individual nodes into their constituent message chains.
//
// GET /api/cogfield/expand/{nodeId} - Expands a session or bus node into blocks

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/cogos-dev/cogos/pkg/cogfield"
)

// Type aliases — canonical types live in pkg/cogfield.
type GraphBlock = cogfield.GraphBlock
type BlockAdapter = cogfield.BlockAdapter
type AdapterNodeConfig = cogfield.AdapterNodeConfig
type BlockTypeConfig = cogfield.BlockTypeConfig

// adapters is the registry of all block adapters.
var adapters = []BlockAdapter{&BusAdapter{}, &SessionAdapter{}, &ComponentAdapter{}, &SignalAdapter{}, &ReconcileAdapter{}}

// graphBlockToNode delegates to pkg/cogfield.
var graphBlockToNode = cogfield.GraphBlockToNode

// --- BusAdapter ---

// BusAdapter expands bus summary nodes into their event chains.
type BusAdapter struct{}

func (a *BusAdapter) ID() string { return "bus" }

func (a *BusAdapter) NodeConfig() AdapterNodeConfig {
	return AdapterNodeConfig{
		BlockTypes: map[string]BlockTypeConfig{
			"bus.message":      {EntityType: "bus.message", Shape: "block", Color: "#22d3ee", Label: "Message"},
			"bus.open":         {EntityType: "bus.open", Shape: "triangle", Color: "#22d3ee", Label: "Open"},
			"bus.close":        {EntityType: "bus.close", Shape: "triangle", Color: "#22d3ee", Label: "Close"},
			"bus.chat_request": {EntityType: "bus.chat_request", Shape: "block", Color: "#818cf8", Label: "Chat In"},
			"bus.chat_response":{EntityType: "bus.chat_response", Shape: "block", Color: "#34d399", Label: "Chat Out"},
			"bus.chat_error":   {EntityType: "bus.chat_error", Shape: "ring", Color: "#f87171", Label: "Chat Err"},
		},
		DefaultSector: "buses",
		ChainThread:   "chain",
	}
}

func (a *BusAdapter) SummaryNodes(root string) ([]CogFieldNode, []CogFieldEdge) {
	nodes := buildBusNodes(root)
	return nodes, nil
}

func (a *BusAdapter) ExpandNode(root, nodeID string) ([]CogFieldNode, []CogFieldEdge, error) {
	if !strings.HasPrefix(nodeID, "bus:") {
		return nil, nil, fmt.Errorf("not a bus node: %s", nodeID)
	}
	busID := strings.TrimPrefix(nodeID, "bus:")

	eventsPath := filepath.Join(root, ".cog", ".state", "buses", busID, "events.jsonl")
	f, err := os.Open(eventsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open bus events: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	var blocks []GraphBlock
	seen := make(map[int]bool)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var evt CogBlock
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if seen[evt.Seq] {
			continue
		}
		seen[evt.Seq] = true

		blocks = append(blocks, busEventToGraphBlock(busID, evt))
	}

	if len(blocks) == 0 {
		return nil, nil, fmt.Errorf("no events found for bus %s", busID)
	}

	var nodes []CogFieldNode
	var edges []CogFieldEdge

	for i, block := range blocks {
		node := graphBlockToNode(block)
		nodes = append(nodes, node)

		if i == 0 {
			// bus → first event
			edges = append(edges, CogFieldEdge{
				Source:   nodeID,
				Target:   block.URI,
				Relation: "bus_genesis",
				Weight:   1.0,
				Thread:   "chain",
			})
		} else {
			// event → event
			edges = append(edges, CogFieldEdge{
				Source:   blocks[i-1].URI,
				Target:   block.URI,
				Relation: "chain_next",
				Weight:   1.0,
				Thread:   "chain",
			})
		}
	}

	return nodes, edges, nil
}

// busEventToGraphBlock converts a CogBlock into a GraphBlock for CogField visualization.
func busEventToGraphBlock(busID string, evt CogBlock) GraphBlock {
	blockType := "bus.message"
	switch evt.Type {
	case "open", "bus.open":
		blockType = "bus.open"
	case "close", "bus.close":
		blockType = "bus.close"
	case BlockChatRequest:
		blockType = "bus.chat_request"
	case BlockChatResponse:
		blockType = "bus.chat_response"
	case BlockChatError:
		blockType = "bus.chat_error"
	}

	return GraphBlock{
		URI:      fmt.Sprintf("bus:%s:%d", busID, evt.Seq),
		Type:     blockType,
		From:     evt.From,
		Ts:       evt.Ts,
		Hash:     evt.Hash,
		PrevHash: evt.PrevHash,
		Payload:  evt.Payload,
		Meta: map[string]interface{}{
			"seq":    evt.Seq,
			"bus_id": busID,
		},
	}
}

// --- SessionAdapter ---

// SessionAdapter expands session summary nodes into their turn chains.
type SessionAdapter struct{}

func (a *SessionAdapter) ID() string { return "session" }

func (a *SessionAdapter) NodeConfig() AdapterNodeConfig {
	return AdapterNodeConfig{
		BlockTypes: map[string]BlockTypeConfig{
			"session.turn":  {EntityType: "session.turn", Shape: "block", Color: "#64748b", Label: "Turn"},
			"session.event": {EntityType: "session.event", Shape: "ring", Color: "#64748b", Label: "Event"},
		},
		DefaultSector: "sessions",
		ChainThread:   "chain",
	}
}

func (a *SessionAdapter) SummaryNodes(root string) ([]CogFieldNode, []CogFieldEdge) {
	nodes := buildSessionNodes(root)
	return nodes, nil
}

func (a *SessionAdapter) ExpandNode(root, nodeID string) ([]CogFieldNode, []CogFieldEdge, error) {
	if !strings.HasPrefix(nodeID, "session:") {
		return nil, nil, fmt.Errorf("not a session node: %s", nodeID)
	}
	sessionID := strings.TrimPrefix(nodeID, "session:")

	detail, err := loadSessionDetail(root, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("load session detail: %w", err)
	}

	if detail.MessageCount == 0 && len(detail.Messages) == 0 {
		return nil, nil, fmt.Errorf("no messages found for session %s", sessionID)
	}

	var nodes []CogFieldNode
	var edges []CogFieldEdge

	prevURI := ""
	for i, msg := range detail.Messages {
		// Determine entity type based on role
		entityType := "session.event"
		switch msg.Role {
		case "user":
			entityType = "block.user"
		case "assistant":
			entityType = "block.assistant"
		case "system":
			entityType = "block.system"
		}

		// Build label from content
		label := msg.Content
		if len(label) > 60 {
			label = label[:60] + "..."
		}
		if label == "" {
			label = msg.Type
			if label == "" {
				label = entityType
			}
		}

		uri := fmt.Sprintf("session:%s:%d", sessionID, msg.Seq)

		meta := map[string]interface{}{
			"block_type": entityType,
			"seq":        msg.Seq,
			"session_id": sessionID,
		}
		if msg.Role != "" {
			meta["role"] = msg.Role
		}
		if msg.Content != "" {
			meta["full_content"] = msg.Content
		}
		if msg.Type != "" {
			meta["event_type"] = msg.Type
		}
		if detail.Source != "" {
			meta["source"] = detail.Source
		}

		node := CogFieldNode{
			ID:         uri,
			Label:      label,
			EntityType: entityType,
			Sector:     "sessions",
			Tags:       []string{},
			Created:    msg.Timestamp,
			Modified:   msg.Timestamp,
			Strength:   math.Min(float64(len(msg.Content))/500.0+1, 5.0),
			Meta:       meta,
		}
		nodes = append(nodes, node)

		if i == 0 {
			// session → first turn
			edges = append(edges, CogFieldEdge{
				Source:   nodeID,
				Target:   uri,
				Relation: "session_start",
				Weight:   1.0,
				Thread:   "chain",
			})
		} else {
			// turn → turn
			edges = append(edges, CogFieldEdge{
				Source:   prevURI,
				Target:   uri,
				Relation: "turn_next",
				Weight:   1.0,
				Thread:   "chain",
			})
		}
		prevURI = uri
	}

	return nodes, edges, nil
}

// --- Expand endpoint ---

// ExpandNodeResponse is a type alias — canonical type lives in pkg/cogfield.
type ExpandNodeResponse = cogfield.ExpandNodeResponse

// handleExpandNode handles GET /api/cogfield/expand/{nodeId}
func (s *serveServer) handleExpandNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	nodeID := strings.TrimPrefix(r.URL.Path, "/api/cogfield/expand/")
	if nodeID == "" {
		http.Error(w, "Node ID required", http.StatusBadRequest)
		return
	}

	var root string
	if ws := workspaceFromRequest(r); ws != nil {
		root = ws.root
	} else {
		var err error
		root, _, err = ResolveWorkspace()
		if err != nil {
			http.Error(w, "Failed to resolve workspace", http.StatusInternalServerError)
			return
		}
	}

	// Try each adapter
	for _, adapter := range adapters {
		nodes, edges, err := adapter.ExpandNode(root, nodeID)
		if err != nil {
			continue // This adapter doesn't handle this node type
		}
		resp := ExpandNodeResponse{
			ParentID: nodeID,
			Nodes:    nodes,
			Edges:    edges,
			Config:   adapter.NodeConfig(),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	log.Printf("cogfield: no adapter found for node %s", nodeID)
	http.Error(w, "No adapter found for node", http.StatusNotFound)
}

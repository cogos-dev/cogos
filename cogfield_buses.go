// cogfield_buses.go - Bus detail endpoint for CogField
//
// GET /api/cogfield/buses/{id} - Returns bus metadata and event timeline

package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// CogBlock is the canonical content atom for the CogOS bus protocol (ADR-059).
// V1 blocks use PrevHash (string); V2 blocks use Prev ([]string) for DAG-style linking.
// Both fields are written during the transition period for backward compatibility.
type CogBlock struct {
	V        int                    `json:"v"`
	ID       string                 `json:"id,omitempty"`
	BusID    string                 `json:"bus_id,omitempty"`
	Seq      int                    `json:"seq,omitempty"`
	Ts       string                 `json:"ts"`
	From     string                 `json:"from"`
	To       string                 `json:"to,omitempty"`
	Type     string                 `json:"type"`
	Payload  map[string]interface{} `json:"payload"`
	Prev     []string               `json:"prev,omitempty"`
	PrevHash string                 `json:"prev_hash,omitempty"` // V1 compat — written alongside Prev during transition
	Hash     string                 `json:"hash"`
	Merkle   string                 `json:"merkle,omitempty"`
	Sig      string                 `json:"sig,omitempty"`
	Size     int                    `json:"size,omitempty"`
}

// BusEventData is a backward-compatible alias for CogBlock.
// Existing code can continue using BusEventData until fully migrated.
type BusEventData = CogBlock

// BusDetail is the response for GET /api/cogfield/buses/{id}
type BusDetail struct {
	BusID        string         `json:"bus_id"`
	State        string         `json:"state"`
	Participants []string       `json:"participants"`
	Created      string         `json:"created"`
	Modified     string         `json:"modified"`
	EventCount   int            `json:"event_count"`
	Events       []CogBlock     `json:"events"`
}

// handleBusDetail handles GET /api/cogfield/buses/{id}
func (s *serveServer) handleBusDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	busID := strings.TrimPrefix(r.URL.Path, "/api/cogfield/buses/")
	if busID == "" {
		http.Error(w, "Bus ID required", http.StatusBadRequest)
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

	detail, err := loadBusDetail(root, busID)
	if err != nil {
		log.Printf("cogfield: bus detail error: %v", err)
		http.Error(w, "Bus not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(detail)
}

// loadBusDetail reads registry metadata and events for a bus
func loadBusDetail(root, busID string) (*BusDetail, error) {
	busesDir := filepath.Join(root, ".cog", ".state", "buses")

	// Read registry for metadata
	registryPath := filepath.Join(busesDir, "registry.json")
	registryData, err := os.ReadFile(registryPath)
	if err != nil {
		return nil, err
	}

	var entries []busRegistryEntry
	if err := json.Unmarshal(registryData, &entries); err != nil {
		return nil, err
	}

	// Find matching bus
	var bus *busRegistryEntry
	for i := range entries {
		if entries[i].BusID == busID {
			bus = &entries[i]
			break
		}
	}
	if bus == nil {
		return nil, os.ErrNotExist
	}

	// Read events
	eventsPath := filepath.Join(busesDir, busID, "events.jsonl")
	events := make([]CogBlock, 0)

	f, err := os.Open(eventsPath)
	if err == nil {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		seen := make(map[int]bool) // deduplicate by seq (file may have dups)

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var block CogBlock
			if err := json.Unmarshal([]byte(line), &block); err != nil {
				continue
			}
			if seen[block.Seq] {
				continue
			}
			seen[block.Seq] = true
			events = append(events, block)
		}
		f.Close()
	}

	participants := bus.Participants
	if participants == nil {
		participants = []string{}
	}

	return &BusDetail{
		BusID:        bus.BusID,
		State:        bus.State,
		Participants: participants,
		Created:      bus.CreatedAt,
		Modified:     bus.LastEventAt,
		EventCount:   len(events),
		Events:       events,
	}, nil
}

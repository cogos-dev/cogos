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

// BusEventData represents a single event in the bus hash chain
type BusEventData struct {
	V        int                    `json:"v"`
	BusID    string                 `json:"bus_id"`
	Seq      int                    `json:"seq"`
	Ts       string                 `json:"ts"`
	From     string                 `json:"from"`
	To       string                 `json:"to,omitempty"`
	Type     string                 `json:"type"`
	Payload  map[string]interface{} `json:"payload"`
	PrevHash string                 `json:"prev_hash"`
	Hash     string                 `json:"hash"`
}

// BusDetail is the response for GET /api/cogfield/buses/{id}
type BusDetail struct {
	BusID        string         `json:"bus_id"`
	State        string         `json:"state"`
	Participants []string       `json:"participants"`
	Created      string         `json:"created"`
	Modified     string         `json:"modified"`
	EventCount   int            `json:"event_count"`
	Events       []BusEventData `json:"events"`
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
	events := make([]BusEventData, 0)

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
			var evt BusEventData
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				continue
			}
			if seen[evt.Seq] {
				continue
			}
			seen[evt.Seq] = true
			events = append(events, evt)
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

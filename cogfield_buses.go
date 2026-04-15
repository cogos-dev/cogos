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

	"github.com/cogos-dev/cogos/pkg/cogfield"
)

// Type aliases — canonical types live in pkg/cogfield.
type CogBlock = cogfield.Block
type BusEventData = cogfield.Block
type BusDetail = cogfield.BusDetail

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

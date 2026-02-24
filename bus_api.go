package main

// bus_api.go — HTTP API for inter-workspace bus messaging.
//
// POST /v1/bus/send — Send a message on a bus (general-purpose, not chat-specific)
// POST /v1/bus/open — Create/register a bus for inter-workspace communication
// GET  /v1/bus/list — List all known buses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// busSendRequest is the JSON body for POST /v1/bus/send.
type busSendRequest struct {
	BusID   string `json:"bus_id"`
	From    string `json:"from"`
	To      string `json:"to,omitempty"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"` // event type, defaults to "message"
}

// busSendResponse is returned on successful send.
type busSendResponse struct {
	OK   bool   `json:"ok"`
	Seq  int    `json:"seq"`
	Hash string `json:"hash"`
}

// handleBusSend handles POST /v1/bus/send.
// Appends a message event to the specified bus and broadcasts via SSE.
func (s *serveServer) handleBusSend(w http.ResponseWriter, r *http.Request) {
	var req busSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	if req.BusID == "" || req.Message == "" {
		http.Error(w, `{"error":"bus_id and message are required"}`, http.StatusBadRequest)
		return
	}

	if req.From == "" {
		req.From = "anonymous"
	}

	eventType := req.Type
	if eventType == "" {
		eventType = "message"
	}

	// Resolve the busChat manager — prefer workspace-specific, fall back to server default.
	busChat := s.busChat
	if ws := workspaceFromRequest(r); ws != nil && ws.busChat != nil {
		busChat = ws.busChat
	}

	if busChat == nil {
		http.Error(w, `{"error":"no bus manager available"}`, http.StatusServiceUnavailable)
		return
	}

	// Ensure bus directory and events file exist.
	busDir := filepath.Join(busChat.manager.busesDir(), req.BusID)
	if err := os.MkdirAll(busDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"create bus dir: %s"}`, err), http.StatusInternalServerError)
		return
	}
	eventsFile := filepath.Join(busDir, "events.jsonl")
	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		f, err := os.Create(eventsFile)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"create events file: %s"}`, err), http.StatusInternalServerError)
			return
		}
		f.Close()
	}

	// Build the payload.
	payload := map[string]interface{}{
		"content": req.Message,
	}
	if req.To != "" {
		payload["to"] = req.To
	}

	// Append the event. The onEvent callback will broadcast to SSE subscribers.
	evt, err := busChat.manager.appendBusEvent(req.BusID, eventType, req.From, payload)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"append failed: %s"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(busSendResponse{
		OK:   true,
		Seq:  evt.Seq,
		Hash: evt.Hash,
	})
}

// handleBusOpen handles POST /v1/bus/open.
// Creates or re-opens a bus for inter-workspace communication.
func (s *serveServer) handleBusOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BusID        string   `json:"bus_id"`
		Participants []string `json:"participants"`
		Transport    string   `json:"transport"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	if req.BusID == "" {
		http.Error(w, `{"error":"bus_id is required"}`, http.StatusBadRequest)
		return
	}

	busChat := s.busChat
	if ws := workspaceFromRequest(r); ws != nil && ws.busChat != nil {
		busChat = ws.busChat
	}

	if busChat == nil {
		http.Error(w, `{"error":"no bus manager available"}`, http.StatusServiceUnavailable)
		return
	}

	// Create the bus directory and events file directly (no bus_chat_ prefix).
	busDir := filepath.Join(busChat.manager.busesDir(), req.BusID)
	if err := os.MkdirAll(busDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"create bus dir: %s"}`, err), http.StatusInternalServerError)
		return
	}
	eventsFile := filepath.Join(busDir, "events.jsonl")
	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		f, err := os.Create(eventsFile)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"create events file: %s"}`, err), http.StatusInternalServerError)
			return
		}
		f.Close()
	}

	// Register in the bus registry.
	origin := "api"
	if len(req.Participants) > 0 {
		origin = req.Participants[0]
	}
	if err := busChat.manager.registerBus(req.BusID, origin, origin); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"register failed: %s"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"bus_id": req.BusID,
		"state":  "active",
	})
}

// handleBusList handles GET /v1/bus/list.
// Returns all known buses from the registry.
func (s *serveServer) handleBusList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	busChat := s.busChat
	if ws := workspaceFromRequest(r); ws != nil && ws.busChat != nil {
		busChat = ws.busChat
	}

	if busChat == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	// Load ALL buses from registry, not just chat buses.
	entries := busChat.manager.loadRegistry()
	if entries == nil {
		entries = []busRegistryEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

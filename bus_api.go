package main

// bus_api.go — HTTP API for inter-workspace bus messaging and event visibility.
//
// POST /v1/bus/send                     — Send a message on a bus
// POST /v1/bus/open                     — Create/register a bus
// GET  /v1/bus/list                     — List all known buses
// GET  /v1/bus/events                   — Cross-bus event search
// GET  /v1/bus/{bus_id}/events          — Query events (type, from, after, before, since, until, limit)
// GET  /v1/bus/{bus_id}/events/{seq}    — Single event lookup
// GET  /v1/bus/{bus_id}/stats           — Bus statistics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

// =============================================================================
// EVENT QUERY API
// =============================================================================

// eventQueryParams holds parsed query parameters for event filtering.
type eventQueryParams struct {
	Type   string // filter by event type
	From   string // filter by sender
	After  int    // events with seq > this
	Before int    // events with seq < this
	Limit  int    // max events to return
	Since  string // ISO8601 — events after this time
	Until  string // ISO8601 — events before this time
}

// parseEventQuery extracts query params from the request URL.
func parseEventQuery(r *http.Request) eventQueryParams {
	q := r.URL.Query()
	params := eventQueryParams{
		Type:  q.Get("type"),
		From:  q.Get("from"),
		Since: q.Get("since"),
		Until: q.Get("until"),
		Limit: 100,
	}
	if v := q.Get("after"); v != "" {
		params.After, _ = strconv.Atoi(v)
	}
	if v := q.Get("before"); v != "" {
		params.Before, _ = strconv.Atoi(v)
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			params.Limit = n
		}
	}
	if params.Limit > 1000 {
		params.Limit = 1000
	}
	return params
}

// filterEvents applies query params to an event list.
func filterEvents(events []CogBlock, p eventQueryParams) []CogBlock {
	var result []CogBlock
	for _, e := range events {
		if p.Type != "" && e.Type != p.Type {
			continue
		}
		if p.From != "" && e.From != p.From {
			continue
		}
		if p.After > 0 && e.Seq <= p.After {
			continue
		}
		if p.Before > 0 && e.Seq >= p.Before {
			continue
		}
		if p.Since != "" && e.Ts < p.Since {
			continue
		}
		if p.Until != "" && e.Ts > p.Until {
			continue
		}
		result = append(result, e)
		if len(result) >= p.Limit {
			break
		}
	}
	if result == nil {
		result = []CogBlock{}
	}
	return result
}

// handleBusRoute is the catch-all handler for GET /v1/bus/{bus_id}/...
// It dispatches to events (with query/pagination), single event lookup, or stats.
func (s *serveServer) handleBusRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse path: /v1/bus/{bus_id}/events, /v1/bus/{bus_id}/events/{seq}, /v1/bus/{bus_id}/stats
	path := strings.TrimPrefix(r.URL.Path, "/v1/bus/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, `{"error":"expected /v1/bus/{bus_id}/{action}"}`, http.StatusBadRequest)
		return
	}
	busID := parts[0]
	action := ""
	extra := ""
	if len(parts) >= 2 {
		action = parts[1]
	}
	if len(parts) >= 3 {
		extra = parts[2]
	}

	switch action {
	case "events":
		if extra != "" {
			s.handleBusEventBySeq(w, r, busID, extra)
		} else {
			s.handleBusEvents(w, r, busID)
		}
	case "stats":
		s.handleBusStats(w, r, busID)
	default:
		http.Error(w, `{"error":"expected /v1/bus/{bus_id}/events or /v1/bus/{bus_id}/stats"}`, http.StatusBadRequest)
	}
}

// resolveBusManager resolves the bus session manager from the request.
func (s *serveServer) resolveBusManager(r *http.Request) *busSessionManager {
	busChat := s.busChat
	if ws := workspaceFromRequest(r); ws != nil && ws.busChat != nil {
		busChat = ws.busChat
	}
	if busChat == nil {
		return nil
	}
	return busChat.manager
}

// handleBusEvents serves GET /v1/bus/{bus_id}/events with query params.
func (s *serveServer) handleBusEvents(w http.ResponseWriter, r *http.Request, busID string) {
	mgr := s.resolveBusManager(r)
	if mgr == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	events, err := mgr.readBusEvents(busID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	params := parseEventQuery(r)
	filtered := filterEvents(events, params)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(filtered)
}

// handleBusEventBySeq serves GET /v1/bus/{bus_id}/events/{seq}.
func (s *serveServer) handleBusEventBySeq(w http.ResponseWriter, r *http.Request, busID, seqStr string) {
	seq, err := strconv.Atoi(seqStr)
	if err != nil {
		http.Error(w, `{"error":"seq must be an integer"}`, http.StatusBadRequest)
		return
	}

	mgr := s.resolveBusManager(r)
	if mgr == nil {
		http.Error(w, `{"error":"event not found"}`, http.StatusNotFound)
		return
	}

	events, err := mgr.readBusEvents(busID)
	if err != nil {
		http.Error(w, `{"error":"event not found"}`, http.StatusNotFound)
		return
	}

	for _, e := range events {
		if e.Seq == seq {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(e)
			return
		}
	}

	http.Error(w, `{"error":"event not found"}`, http.StatusNotFound)
}

// busStatsResponse is the response for GET /v1/bus/{bus_id}/stats.
type busStatsResponse struct {
	BusID        string         `json:"bus_id"`
	EventCount   int            `json:"event_count"`
	FirstEventAt string         `json:"first_event_at,omitempty"`
	LastEventAt  string         `json:"last_event_at,omitempty"`
	Types        map[string]int `json:"types"`
	Senders      map[string]int `json:"senders"`
}

// handleBusStats serves GET /v1/bus/{bus_id}/stats.
func (s *serveServer) handleBusStats(w http.ResponseWriter, r *http.Request, busID string) {
	mgr := s.resolveBusManager(r)
	if mgr == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(busStatsResponse{BusID: busID, Types: map[string]int{}, Senders: map[string]int{}})
		return
	}

	events, _ := mgr.readBusEvents(busID)

	stats := busStatsResponse{
		BusID:      busID,
		EventCount: len(events),
		Types:      make(map[string]int),
		Senders:    make(map[string]int),
	}

	for i, e := range events {
		stats.Types[e.Type]++
		stats.Senders[e.From]++
		if i == 0 {
			stats.FirstEventAt = e.Ts
		}
		stats.LastEventAt = e.Ts
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// =============================================================================
// CROSS-BUS EVENT SEARCH
// =============================================================================

// crossBusEvent wraps a CogBlock with its bus_id for cross-bus results.
type crossBusEvent struct {
	CogBlock
	BusID string `json:"bus_id"`
}

// handleBusEventsGlobal serves GET /v1/bus/events — cross-bus event search.
func (s *serveServer) handleBusEventsGlobal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mgr := s.resolveBusManager(r)
	if mgr == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	params := parseEventQuery(r)

	// Load all buses from registry
	entries := mgr.loadRegistry()
	var allEvents []crossBusEvent

	for _, entry := range entries {
		events, err := mgr.readBusEvents(entry.BusID)
		if err != nil {
			continue
		}
		filtered := filterEvents(events, eventQueryParams{
			Type:  params.Type,
			From:  params.From,
			Since: params.Since,
			Until: params.Until,
			Limit: params.Limit, // per-bus limit to avoid reading too many
		})
		for _, e := range filtered {
			evt := crossBusEvent{CogBlock: e}
			// BusID might already be set on the event; ensure it's present
			if evt.CogBlock.BusID == "" {
				evt.BusID = entry.BusID
			} else {
				evt.BusID = evt.CogBlock.BusID
			}
			allEvents = append(allEvents, evt)
		}
	}

	// Sort by timestamp descending (most recent first)
	sort.Slice(allEvents, func(i, j int) bool {
		return allEvents[i].Ts > allEvents[j].Ts
	})

	// Apply global limit
	if len(allEvents) > params.Limit {
		allEvents = allEvents[:params.Limit]
	}

	if allEvents == nil {
		allEvents = []crossBusEvent{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(allEvents)
}

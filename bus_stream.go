// bus_stream.go - SSE event stream and REST events endpoint for CogBus
//
// GET /v1/events/stream?bus_id={id} - SSE stream of bus events (long-lived)
// GET /v1/bus/{bus_id}/events        - REST: returns all events as JSON array

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// busEventEnvelope is the SSE envelope format expected by OpenClaw's CogBus monitor.
// Format: {"id":"...","type":"bus.event","timestamp":"...","data":{...BusEventData...}}
type busEventEnvelope struct {
	ID        string        `json:"id"`
	Type      string        `json:"type"`
	Timestamp string        `json:"timestamp"`
	Data      *BusEventData `json:"data"`
}

// busEventBroker manages SSE subscribers for real-time bus event delivery.
type busEventBroker struct {
	mu          sync.RWMutex
	subscribers map[string]map[chan *BusEventData]struct{} // busID -> set of channels
}

func newBusEventBroker() *busEventBroker {
	return &busEventBroker{
		subscribers: make(map[string]map[chan *BusEventData]struct{}),
	}
}

// subscribe registers a channel to receive events for a given bus.
func (b *busEventBroker) subscribe(busID string, ch chan *BusEventData) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subscribers[busID] == nil {
		b.subscribers[busID] = make(map[chan *BusEventData]struct{})
	}
	b.subscribers[busID][ch] = struct{}{}
}

// unsubscribe removes a channel from a bus's subscriber set.
func (b *busEventBroker) unsubscribe(busID string, ch chan *BusEventData) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if subs, ok := b.subscribers[busID]; ok {
		delete(subs, ch)
		if len(subs) == 0 {
			delete(b.subscribers, busID)
		}
	}
}

// publish sends an event to all subscribers of a bus. Non-blocking: drops if channel full.
func (b *busEventBroker) publish(busID string, evt *BusEventData) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	subs, ok := b.subscribers[busID]
	if !ok {
		return
	}
	for ch := range subs {
		select {
		case ch <- evt:
		default:
			// subscriber too slow, drop event
		}
	}
}

// handleEventsStream serves GET /v1/events/stream?bus_id={id}
// This is the SSE endpoint that OpenClaw's CogBus monitor connects to.
func (s *serveServer) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	busID := r.URL.Query().Get("bus_id")
	if busID == "" {
		http.Error(w, "bus_id query parameter required", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx passthrough

	// Send initial connected event
	connected := map[string]interface{}{
		"type":      "connected",
		"bus_id":    busID,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}
	connData, _ := json.Marshal(connected)
	fmt.Fprintf(w, "data: %s\n\n", connData)
	flusher.Flush()

	// Replay existing events for the bus
	if s.busChat != nil {
		events, err := s.busChat.manager.readBusEvents(busID)
		if err == nil {
			for i := range events {
				envelope := busEventEnvelope{
					ID:        fmt.Sprintf("replay_%s_%d", busID, events[i].Seq),
					Type:      "bus.event",
					Timestamp: events[i].Ts,
					Data:      &events[i],
				}
				data, _ := json.Marshal(envelope)
				fmt.Fprintf(w, "data: %s\n\n", data)
			}
			flusher.Flush()
		}
	}

	// Subscribe for live events
	ch := make(chan *BusEventData, 64)
	s.busBroker.subscribe(busID, ch)
	defer s.busBroker.unsubscribe(busID, ch)

	// Keep-alive ticker (30s heartbeat)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()

	log.Printf("[bus-stream] SSE client connected for bus=%s", busID)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[bus-stream] SSE client disconnected for bus=%s", busID)
			return

		case evt := <-ch:
			envelope := busEventEnvelope{
				ID:        fmt.Sprintf("live_%s_%d", busID, evt.Seq),
				Type:      "bus.event",
				Timestamp: evt.Ts,
				Data:      evt,
			}
			data, err := json.Marshal(envelope)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

		case <-ticker.C:
			// SSE comment keep-alive — prevents proxy/client timeout without
			// generating a data event that subscribers need to handle.
			fmt.Fprintf(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}

// handleBusEventsREST serves GET /v1/bus/{bus_id}/events
// Returns all events for a bus as a JSON array.
func (s *serveServer) handleBusEventsREST(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract bus_id from path: /v1/bus/{bus_id}/events
	path := strings.TrimPrefix(r.URL.Path, "/v1/bus/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || parts[1] != "events" || parts[0] == "" {
		http.Error(w, "Expected /v1/bus/{bus_id}/events", http.StatusBadRequest)
		return
	}
	busID := parts[0]

	if s.busChat == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	events, err := s.busChat.manager.readBusEvents(busID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	if events == nil {
		events = []BusEventData{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

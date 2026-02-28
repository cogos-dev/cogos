// bus_stream.go - SSE event stream and REST events endpoint for CogBus
//
// GET /v1/events/stream?bus_id={id} - SSE stream of bus events (long-lived)
// GET /v1/bus/{bus_id}/events        - REST: returns all events as JSON array

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// busEventEnvelope is the SSE envelope format expected by OpenClaw's CogBus monitor.
// Format: {"id":"...","type":"bus.event","timestamp":"...","data":{...CogBlock...}}
type busEventEnvelope struct {
	ID        string        `json:"id"`
	Type      string        `json:"type"`
	Timestamp string        `json:"timestamp"`
	Data      *CogBlock `json:"data"`
}

// maxSSEPerBus limits the number of concurrent SSE subscribers per bus.
// When exceeded, new connections are still allowed but excess old ones are evicted.
const maxSSEPerBus = 5

// sseIdleTimeout is the maximum duration a subscriber can go without a
// successful write before it is considered stale and eligible for eviction.
const sseIdleTimeout = 5 * time.Minute

// sseSubscriber tracks per-connection metadata for liveness detection.
type sseSubscriber struct {
	ch        chan *CogBlock
	ctx       context.Context // request context — Done() when client disconnects
	lastWrite time.Time       // last successful event/heartbeat write
}

// busEventBroker manages SSE subscribers for real-time bus event delivery.
type busEventBroker struct {
	mu          sync.RWMutex
	subscribers map[string]map[chan *CogBlock]*sseSubscriber // busID -> channel -> subscriber
}

func newBusEventBroker() *busEventBroker {
	return &busEventBroker{
		subscribers: make(map[string]map[chan *CogBlock]*sseSubscriber),
	}
}

// subscribe registers a channel to receive events for a given bus.
// If the bus is at the connection limit, it sweeps stale/dead subscribers
// before rejecting. Returns false only if the bus is still full after cleanup.
func (b *busEventBroker) subscribe(busID string, ch chan *CogBlock, ctx context.Context) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subscribers[busID] == nil {
		b.subscribers[busID] = make(map[chan *CogBlock]*sseSubscriber)
	}

	if len(b.subscribers[busID]) >= maxSSEPerBus {
		b.sweepLocked(busID)
	}

	if len(b.subscribers[busID]) >= maxSSEPerBus {
		return false
	}

	b.subscribers[busID][ch] = &sseSubscriber{
		ch:        ch,
		ctx:       ctx,
		lastWrite: time.Now(),
	}
	return true
}

// sweepLocked removes dead or idle subscribers for a bus.
// Caller must hold b.mu (write lock).
func (b *busEventBroker) sweepLocked(busID string) {
	subs, ok := b.subscribers[busID]
	if !ok {
		return
	}
	now := time.Now()
	for ch, sub := range subs {
		dead := false
		// Check if the client's request context has been cancelled.
		select {
		case <-sub.ctx.Done():
			dead = true
		default:
		}
		// Check idle timeout — no write in sseIdleTimeout.
		if !dead && now.Sub(sub.lastWrite) > sseIdleTimeout {
			dead = true
		}
		if dead {
			log.Printf("[bus-stream] evicting stale SSE subscriber for bus=%s (lastWrite=%s ago)",
				busID, now.Sub(sub.lastWrite).Round(time.Second))
			delete(subs, ch)
			close(ch)
		}
	}
	if len(subs) == 0 {
		delete(b.subscribers, busID)
	}
}

// subscriberCount returns the number of active subscribers for a bus.
func (b *busEventBroker) subscriberCount(busID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers[busID])
}

// unsubscribe removes a channel from a bus's subscriber set.
func (b *busEventBroker) unsubscribe(busID string, ch chan *CogBlock) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if subs, ok := b.subscribers[busID]; ok {
		delete(subs, ch)
		if len(subs) == 0 {
			delete(b.subscribers, busID)
		}
	}
}

// touchWrite updates the lastWrite timestamp for a subscriber.
func (b *busEventBroker) touchWrite(busID string, ch chan *CogBlock) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if subs, ok := b.subscribers[busID]; ok {
		if sub, ok := subs[ch]; ok {
			sub.lastWrite = time.Now()
		}
	}
}

// publish sends an event to all subscribers of a bus. Non-blocking: drops if channel full.
func (b *busEventBroker) publish(busID string, evt *CogBlock) {
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

// sseWriteWindow is the rolling write-deadline extension applied before each
// SSE write.  The server's global WriteTimeout (5 min) is an absolute cap that
// kills long-lived SSE connections.  ResponseController.SetWriteDeadline lets
// us push the deadline forward on every write, converting the hard cap into a
// per-write idle timeout.  We use 5 minutes so that a quiet-but-alive stream
// (30 s heartbeats) never hits the deadline.
const sseWriteWindow = 5 * time.Minute

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

	// ResponseController lets us extend the per-response write deadline so the
	// server's global WriteTimeout doesn't kill this long-lived SSE stream.
	rc := http.NewResponseController(w)

	// extendDeadline pushes the write deadline forward by sseWriteWindow.
	// Must be called before every write+flush to prevent the global
	// WriteTimeout from terminating the connection.
	extendDeadline := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteWindow))
	}

	// Subscribe for live events — check limit BEFORE writing any headers.
	// Pass request context so the broker can detect dead connections.
	ch := make(chan *CogBlock, 64)
	if !s.busBroker.subscribe(busID, ch, r.Context()) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Too many SSE connections for this bus", http.StatusTooManyRequests)
		log.Printf("[bus-stream] SSE connection rejected for bus=%s (limit=%d, after sweep)", busID, maxSSEPerBus)
		return
	}
	defer s.busBroker.unsubscribe(busID, ch)

	// Resolve per-workspace busChat; fall back to server default.
	busChat := s.busChat
	if ws := workspaceFromRequest(r); ws != nil && ws.busChat != nil {
		busChat = ws.busChat
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
	extendDeadline()
	fmt.Fprintf(w, "data: %s\n\n", connData)
	flusher.Flush()

	// Replay existing events for the bus
	if busChat != nil {
		events, err := busChat.manager.readBusEvents(busID)
		if err == nil {
			extendDeadline()
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

	// Keep-alive ticker (30s heartbeat)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()

	log.Printf("[bus-stream] SSE client connected for bus=%s (active=%d)", busID, s.busBroker.subscriberCount(busID))

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
			extendDeadline()
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			s.busBroker.touchWrite(busID, ch)

		case <-ticker.C:
			// SSE comment keep-alive — prevents proxy/client timeout without
			// generating a data event that subscribers need to handle.
			extendDeadline()
			fmt.Fprintf(w, ": keep-alive\n\n")
			flusher.Flush()
			s.busBroker.touchWrite(busID, ch)
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

	// Resolve per-workspace busChat; fall back to server default.
	busChat := s.busChat
	if ws := workspaceFromRequest(r); ws != nil && ws.busChat != nil {
		busChat = ws.busChat
	}

	if busChat == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	events, err := busChat.manager.readBusEvents(busID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	if events == nil {
		events = []CogBlock{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

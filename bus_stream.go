// bus_stream.go - SSE event stream and REST events endpoint for CogBus
//
// GET /v1/events/stream?bus_id={id} - SSE stream of bus events (long-lived)
//   bus_id is optional — when omitted, the client subscribes to ALL buses
//   via the wildcard key "*".
// GET /v1/bus/{bus_id}/events        - REST: returns all events as JSON array

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
// The openclaw-gateway opens many concurrent EventSource connections
// (one per UI component, reconnection storm after kernel re-exec), so
// this must be generous. Localhost-only, no external risk.
const maxSSEPerBus = 50

// sseIdleTimeout is the maximum duration a subscriber can go without a
// successful write before it is considered stale and eligible for eviction.
// Short timeout helps recover from reconnection storms where old
// EventSource instances are abandoned without closing.
const sseIdleTimeout = 2 * time.Minute

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
// first, then evicts the oldest subscriber to make room. New connections
// always succeed — this prevents any single client from monopolizing all slots.
func (b *busEventBroker) subscribe(busID string, ch chan *CogBlock, ctx context.Context) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subscribers[busID] == nil {
		b.subscribers[busID] = make(map[chan *CogBlock]*sseSubscriber)
	}

	if len(b.subscribers[busID]) >= maxSSEPerBus {
		b.sweepLocked(busID)
	}

	// If still at limit after sweep, evict the oldest subscriber
	if len(b.subscribers[busID]) >= maxSSEPerBus {
		b.evictOldestLocked(busID)
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

// evictOldestLocked removes the subscriber with the oldest lastWrite time.
// Caller must hold b.mu (write lock).
func (b *busEventBroker) evictOldestLocked(busID string) {
	subs, ok := b.subscribers[busID]
	if !ok || len(subs) == 0 {
		return
	}

	var oldestCh chan *CogBlock
	var oldestTime time.Time
	first := true
	for ch, sub := range subs {
		if first || sub.lastWrite.Before(oldestTime) {
			oldestCh = ch
			oldestTime = sub.lastWrite
			first = false
		}
	}
	if oldestCh != nil {
		delete(subs, oldestCh)
		close(oldestCh)
		log.Printf("[bus-stream] evicted oldest SSE subscriber for bus=%s (age=%s)",
			busID, time.Since(oldestTime).Round(time.Second))
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

// publish sends an event to all subscribers of a bus AND to wildcard ("*")
// subscribers. Non-blocking: drops if channel full.
func (b *busEventBroker) publish(busID string, evt *CogBlock) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Deliver to bus-specific subscribers.
	if subs, ok := b.subscribers[busID]; ok {
		for ch := range subs {
			select {
			case ch <- evt:
			default:
			}
		}
	}

	// Deliver to wildcard subscribers (connected without bus_id).
	if busID != "*" {
		if subs, ok := b.subscribers["*"]; ok {
			for ch := range subs {
				select {
				case ch <- evt:
				default:
				}
			}
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
		busID = "*" // wildcard — receive events from all buses
	}

	consumerID := r.URL.Query().Get("consumer")

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

	// Subscribe for live events. If the bus is at capacity, the broker
	// evicts the oldest subscriber to make room — new connections always succeed.
	ch := make(chan *CogBlock, 64)
	s.busBroker.subscribe(busID, ch, r.Context())
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

	// Replay existing events — cursor-aware if consumer is registered.
	if busChat != nil && busID != "*" {
		events, err := busChat.manager.readBusEvents(busID)
		if err == nil {
			var startSeq int64
			if consumerID != "" {
				// Cursor-based: get or create cursor, replay from cursor position
				cursor := s.consumerReg.getOrCreate(busID, consumerID)
				startSeq = cursor.LastAckedSeq
				log.Printf("[bus-stream] Consumer %s connected to bus=%s, resuming from seq=%d",
					consumerID, busID, startSeq)
			}

			extendDeadline()
			for i := range events {
				// Skip events the consumer has already acknowledged
				if int64(events[i].Seq) <= startSeq {
					continue
				}
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

	if consumerID != "" {
		log.Printf("[bus-stream] SSE consumer=%s connected for bus=%s (active=%d)",
			consumerID, busID, s.busBroker.subscriberCount(busID))
	} else {
		log.Printf("[bus-stream] SSE client connected for bus=%s (active=%d)",
			busID, s.busBroker.subscriberCount(busID))
	}

	for {
		select {
		case <-ctx.Done():
			log.Printf("[bus-stream] SSE client disconnected for bus=%s", busID)
			return

		case evt, ok := <-ch:
			if !ok {
				// Channel closed by broker (evicted as stale) — exit gracefully.
				log.Printf("[bus-stream] SSE channel closed by broker for bus=%s, disconnecting", busID)
				return
			}
			if evt == nil {
				continue
			}
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

// ---------------------------------------------------------------------------
// Consumer Cursors — ADR-061 server-side consumer position tracking
// ---------------------------------------------------------------------------

// ConsumerCursor tracks a consumer's position in a bus event stream.
// Persisted to {bus_id}.cursors.jsonl alongside events.
type ConsumerCursor struct {
	ConsumerID   string    `json:"consumer_id"`
	BusID        string    `json:"bus_id"`
	LastAckedSeq int64     `json:"last_acked_seq"`
	ConnectedAt  time.Time `json:"connected_at"`
	LastAckAt    time.Time `json:"last_ack_at"`
	Stale        bool      `json:"stale"`
}

// consumerRegistry manages consumer cursors for all buses.
// Thread-safe — all public methods acquire the mutex.
type consumerRegistry struct {
	mu      sync.RWMutex
	cursors map[string]map[string]*ConsumerCursor // busID -> consumerID -> cursor
	dataDir string                                // persistence directory (.cog/run/bus/)
}

func newConsumerRegistry(dataDir string) *consumerRegistry {
	return &consumerRegistry{
		cursors: make(map[string]map[string]*ConsumerCursor),
		dataDir: dataDir,
	}
}

// getOrCreate returns the cursor for a consumer, creating one at position 0 if it doesn't exist.
func (cr *consumerRegistry) getOrCreate(busID, consumerID string) *ConsumerCursor {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.cursors[busID] == nil {
		cr.cursors[busID] = make(map[string]*ConsumerCursor)
	}
	cursor, ok := cr.cursors[busID][consumerID]
	if !ok {
		cursor = &ConsumerCursor{
			ConsumerID:   consumerID,
			BusID:        busID,
			LastAckedSeq: 0,
			ConnectedAt:  time.Now(),
			Stale:        false,
		}
		cr.cursors[busID][consumerID] = cursor
		log.Printf("[bus-cursor] Created cursor for consumer=%s bus=%s", consumerID, busID)
	} else {
		cursor.ConnectedAt = time.Now()
		cursor.Stale = false
	}
	return cursor
}

// ack advances a consumer's cursor. Returns the updated cursor.
// Returns nil if the consumer doesn't exist.
// ACKs are monotonic — ignores seq <= current LastAckedSeq.
func (cr *consumerRegistry) ack(busID, consumerID string, seq int64) (*ConsumerCursor, error) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.cursors[busID] == nil {
		return nil, fmt.Errorf("unknown bus: %s", busID)
	}
	cursor, ok := cr.cursors[busID][consumerID]
	if !ok {
		return nil, fmt.Errorf("unknown consumer: %s on bus %s", consumerID, busID)
	}
	if seq <= cursor.LastAckedSeq {
		// Monotonic — silently ignore duplicate/stale ACKs
		return cursor, nil
	}
	cursor.LastAckedSeq = seq
	cursor.LastAckAt = time.Now()
	cursor.Stale = false

	// Persist cursor update
	cr.persistLocked(busID, cursor)

	return cursor, nil
}

// list returns all cursors, optionally filtered by busID (empty = all).
func (cr *consumerRegistry) list(busID string) []*ConsumerCursor {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	var result []*ConsumerCursor
	for bid, consumers := range cr.cursors {
		if busID != "" && bid != busID {
			continue
		}
		for _, cursor := range consumers {
			c := *cursor // copy
			result = append(result, &c)
		}
	}
	return result
}

// remove deletes a consumer's cursor.
func (cr *consumerRegistry) remove(consumerID string) bool {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	found := false
	for busID, consumers := range cr.cursors {
		if _, ok := consumers[consumerID]; ok {
			delete(consumers, consumerID)
			found = true
			log.Printf("[bus-cursor] Removed cursor for consumer=%s bus=%s", consumerID, busID)
			if len(consumers) == 0 {
				delete(cr.cursors, busID)
			}
		}
	}
	return found
}

// persistLocked writes a cursor snapshot to the cursors.jsonl file.
// Caller must hold cr.mu.
func (cr *consumerRegistry) persistLocked(busID string, cursor *ConsumerCursor) {
	if cr.dataDir == "" {
		return
	}
	path := filepath.Join(cr.dataDir, busID+".cursors.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[bus-cursor] Failed to open cursor file %s: %v", path, err)
		return
	}
	defer f.Close()
	data, _ := json.Marshal(cursor)
	f.Write(append(data, '\n'))
}

// loadFromDisk reads all cursor files and reconstructs the latest state per consumer.
func (cr *consumerRegistry) loadFromDisk() error {
	if cr.dataDir == "" {
		return nil
	}
	entries, err := os.ReadDir(cr.dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no persistence dir yet
		}
		return fmt.Errorf("read cursor dir: %w", err)
	}

	cr.mu.Lock()
	defer cr.mu.Unlock()

	loaded := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".cursors.jsonl") {
			continue
		}
		busID := strings.TrimSuffix(entry.Name(), ".cursors.jsonl")
		path := filepath.Join(cr.dataDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[bus-cursor] Failed to read %s: %v", path, err)
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var cursor ConsumerCursor
			if err := json.Unmarshal([]byte(line), &cursor); err != nil {
				continue
			}
			if cr.cursors[busID] == nil {
				cr.cursors[busID] = make(map[string]*ConsumerCursor)
			}
			// Last entry wins (append-only log)
			cr.cursors[busID][cursor.ConsumerID] = &cursor
			loaded++
		}
	}
	if loaded > 0 {
		log.Printf("[bus-cursor] Loaded %d cursor entries from disk", loaded)
	}
	return nil
}

// sweepStale marks consumers as stale if they haven't ACKed within the staleness window.
// connectedWindow: staleness timeout for connected consumers (default 5 min)
// disconnectedWindow: staleness timeout for disconnected consumers (default 24h)
func (cr *consumerRegistry) sweepStale(connectedWindow, disconnectedWindow time.Duration) int {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	now := time.Now()
	marked := 0
	for _, consumers := range cr.cursors {
		for _, cursor := range consumers {
			if cursor.Stale {
				continue // already stale
			}
			window := disconnectedWindow
			if !cursor.ConnectedAt.IsZero() && now.Sub(cursor.ConnectedAt) < connectedWindow {
				window = connectedWindow
			}
			lastActivity := cursor.LastAckAt
			if lastActivity.IsZero() {
				lastActivity = cursor.ConnectedAt
			}
			if !lastActivity.IsZero() && now.Sub(lastActivity) > window {
				cursor.Stale = true
				marked++
				log.Printf("[bus-cursor] Marked consumer=%s bus=%s as stale (last activity %s ago)",
					cursor.ConsumerID, cursor.BusID, now.Sub(lastActivity).Round(time.Second))
			}
		}
	}
	return marked
}

// minAckedSeq returns the minimum LastAckedSeq across all active (non-stale) cursors for a bus.
// Events with seq <= this value are safe to garbage-collect.
// Returns 0 if no active cursors exist (nothing is safe to GC).
func (cr *consumerRegistry) minAckedSeq(busID string) int64 {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	consumers, ok := cr.cursors[busID]
	if !ok || len(consumers) == 0 {
		return 0
	}
	var minSeq int64
	first := true
	for _, cursor := range consumers {
		if cursor.Stale {
			continue // don't let stale cursors prevent GC
		}
		if first || cursor.LastAckedSeq < minSeq {
			minSeq = cursor.LastAckedSeq
			first = false
		}
	}
	return minSeq
}

// runLifecycle runs the cursor lifecycle loop: periodic staleness sweep.
// Runs until ctx is cancelled.
func (cr *consumerRegistry) runLifecycle(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			marked := cr.sweepStale(5*time.Minute, 24*time.Hour)
			if marked > 0 {
				log.Printf("[bus-cursor] Staleness sweep: marked %d consumers as stale", marked)
			}
		}
	}
}

// handleBusEventsREST is replaced by handleBusRoute in bus_api.go.
// The new handler supports query filtering, single event lookup, stats, and cross-bus search.

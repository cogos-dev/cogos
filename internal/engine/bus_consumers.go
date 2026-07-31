// bus_consumers.go — consumer cursor registry for the bus surface.
//
// Track 5 Phase 3: port of the consumer-cursor portion of root's bus_stream.go
// (ADR-061 server-side consumer position tracking). Kept in a separate file
// from the SSE broker for readability — the two surfaces cooperate but are
// decoupled.
//
// Persistence layout:
//
//	{workspace}/.cog/run/bus/{bus_id}.cursors.jsonl   — append-only log, last
//	                                                    entry per consumer wins
package engine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/pathsafe"
)

// ConsumerCursor tracks a consumer's position in a bus event stream.
// Persisted to {bus_id}.cursors.jsonl alongside events.
//
// JSON field names MUST stay identical to root for byte-compat (the cursor
// objects are what GET /v1/bus/consumers returns under "consumers").
type ConsumerCursor struct {
	ConsumerID   string    `json:"consumer_id"`
	BusID        string    `json:"bus_id"`
	LastAckedSeq int64     `json:"last_acked_seq"`
	ConnectedAt  time.Time `json:"connected_at"`
	LastAckAt    time.Time `json:"last_ack_at"`
	Stale        bool      `json:"stale"`
}

// ConsumerRegistry manages consumer cursors for all buses.
// Thread-safe — all public methods acquire the mutex.
type ConsumerRegistry struct {
	mu      sync.RWMutex
	cursors map[string]map[string]*ConsumerCursor // busID -> consumerID -> cursor
	dataDir string                                // persistence directory (.cog/run/bus/)
}

// NewConsumerRegistry constructs an empty registry that will persist cursor
// updates to dataDir. If dataDir is empty, persistence is disabled (tests).
func NewConsumerRegistry(dataDir string) *ConsumerRegistry {
	return &ConsumerRegistry{
		cursors: make(map[string]map[string]*ConsumerCursor),
		dataDir: dataDir,
	}
}

// GetOrCreate returns the cursor for a consumer, creating one at position 0
// if it doesn't exist.
func (cr *ConsumerRegistry) GetOrCreate(busID, consumerID string) *ConsumerCursor {
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
		slog.Debug("bus-cursor: created cursor", "consumer", consumerID, "bus", busID)
	} else {
		cursor.ConnectedAt = time.Now()
		cursor.Stale = false
	}
	return cursor
}

// Ack advances a consumer's cursor. Returns the updated cursor. Monotonic —
// ignores seq <= current LastAckedSeq. Returns error if the bus/consumer is
// unknown (matches root's semantics for /v1/bus/{id}/ack).
func (cr *ConsumerRegistry) Ack(busID, consumerID string, seq int64) (*ConsumerCursor, error) {
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
		return cursor, nil
	}
	cursor.LastAckedSeq = seq
	cursor.LastAckAt = time.Now()
	cursor.Stale = false
	cr.persistLocked(busID, cursor)
	return cursor, nil
}

// List returns all cursors, optionally filtered by busID (empty = all).
// Returned cursors are copies; modifying them has no effect on the registry.
func (cr *ConsumerRegistry) List(busID string) []*ConsumerCursor {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	var result []*ConsumerCursor
	for bid, consumers := range cr.cursors {
		if busID != "" && bid != busID {
			continue
		}
		for _, cursor := range consumers {
			c := *cursor
			result = append(result, &c)
		}
	}
	return result
}

// Remove deletes a consumer's cursor by consumer ID across all buses.
// Returns true if at least one cursor was removed.
func (cr *ConsumerRegistry) Remove(consumerID string) bool {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	found := false
	for busID, consumers := range cr.cursors {
		if _, ok := consumers[consumerID]; ok {
			delete(consumers, consumerID)
			found = true
			slog.Debug("bus-cursor: removed", "consumer", consumerID, "bus", busID)
			if len(consumers) == 0 {
				delete(cr.cursors, busID)
			}
		}
	}
	return found
}

// persistLocked writes a cursor snapshot to the cursors.jsonl file.
// Caller must hold cr.mu.
//
// busID is sanitized via pathsafe.SanitizeComponent before it becomes part
// of the filename: it is the same caller-supplied, structurally-unsafe
// identifier bus_session.go's EventsPath now sanitizes for the identical
// reason (myrgic/cogos#489 round 2 — cog-review flagged this as the same
// bug-class pattern, unverified-reachable at review time). GetOrCreate/Ack,
// the only callers that reach here, are not currently wired to any
// HTTP/MCP route (only List/Remove are, from serve_bus.go, and neither
// takes the persist-to-disk path), so this is a preventative fix rather
// than a live-exploit closure — but it means a future caller can't
// reintroduce the defect this file's sibling was written to fix.
func (cr *ConsumerRegistry) persistLocked(busID string, cursor *ConsumerCursor) {
	if cr.dataDir == "" {
		return
	}
	if err := os.MkdirAll(cr.dataDir, 0755); err != nil {
		slog.Warn("bus-cursor: mkdir failed", "dir", cr.dataDir, "err", err)
		return
	}
	path := filepath.Join(cr.dataDir, pathsafe.SanitizeComponent(busID)+".cursors.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		slog.Warn("bus-cursor: open cursor file failed", "path", path, "err", err)
		return
	}
	defer f.Close()
	data, _ := json.Marshal(cursor)
	f.Write(append(data, '\n'))
}

// LoadFromDisk reads all cursor files and reconstructs the latest state per consumer.
func (cr *ConsumerRegistry) LoadFromDisk() error {
	if cr.dataDir == "" {
		return nil
	}
	entries, err := os.ReadDir(cr.dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
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
			slog.Warn("bus-cursor: read cursor file failed", "path", path, "err", err)
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
			cr.cursors[busID][cursor.ConsumerID] = &cursor
			loaded++
		}
	}
	if loaded > 0 {
		slog.Debug("bus-cursor: loaded cursor entries from disk", "count", loaded)
	}
	return nil
}

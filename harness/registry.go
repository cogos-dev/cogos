// registry.go tracks in-flight inference requests for visibility and cancellation.
//
// Each Harness owns a RequestRegistry. When a request starts, it's registered with
// status "running". On completion it moves to "completed", "failed", or "cancelled".
// The kernel exposes registry contents via GET /v1/requests and supports cancellation
// via DELETE /v1/requests/:id.
//
// StartRegistryCleanup should be called once at startup to periodically remove
// stale entries (completed/failed/cancelled older than 1 hour).
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// RequestEntry represents a tracked request in the registry.
type RequestEntry struct {
	ID      string             `json:"id"`
	Origin  string             `json:"origin"`
	Model   string             `json:"model"`
	Started time.Time          `json:"started"`
	Status  string             `json:"status"` // "running", "completed", "cancelled", "failed"
	Cancel  context.CancelFunc `json:"-"`
	Prompt  string             `json:"prompt,omitempty"` // First 100 chars for display
}

// RequestRegistry tracks in-flight inference requests
type RequestRegistry struct {
	mu       sync.RWMutex
	requests map[string]*RequestEntry
}

// NewRequestRegistry creates a new request registry
func NewRequestRegistry() *RequestRegistry {
	return &RequestRegistry{
		requests: make(map[string]*RequestEntry),
	}
}

// Register adds a new request to the registry
func (r *RequestRegistry) Register(req *InferenceRequest, cancel context.CancelFunc) *RequestEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Generate ID if not provided
	if req.ID == "" {
		req.ID = GenerateRequestID(req.Origin)
	}

	// Truncate prompt for display
	promptPreview := req.Prompt
	if len(promptPreview) > 100 {
		promptPreview = promptPreview[:100] + "..."
	}

	entry := &RequestEntry{
		ID:      req.ID,
		Origin:  req.Origin,
		Model:   req.Model,
		Started: time.Now(),
		Status:  "running",
		Cancel:  cancel,
		Prompt:  promptPreview,
	}

	r.requests[req.ID] = entry
	return entry
}

// Complete marks a request as completed with given status
func (r *RequestRegistry) Complete(id string, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry, ok := r.requests[id]; ok {
		entry.Status = status
	}
}

// Cancel cancels a request by ID, returns true if found and cancelled
func (r *RequestRegistry) Cancel(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry, ok := r.requests[id]; ok {
		if entry.Cancel != nil {
			entry.Cancel()
		}
		entry.Status = "cancelled"
		return true
	}
	return false
}

// Get retrieves a request entry by ID (returns a copy to prevent data races)
func (r *RequestRegistry) Get(id string) *RequestEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if entry, ok := r.requests[id]; ok {
		entryCopy := *entry
		return &entryCopy
	}
	return nil
}

// List returns all request entries (copies to prevent data races)
func (r *RequestRegistry) List() []RequestEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]RequestEntry, 0, len(r.requests))
	for _, entry := range r.requests {
		entries = append(entries, *entry)
	}
	return entries
}

// ListRunning returns only running request entries (copies to prevent data races)
func (r *RequestRegistry) ListRunning() []RequestEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]RequestEntry, 0)
	for _, entry := range r.requests {
		if entry.Status == "running" {
			entries = append(entries, *entry)
		}
	}
	return entries
}

// Remove removes a request from the registry
func (r *RequestRegistry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.requests, id)
}

// Cleanup removes completed/failed/cancelled requests older than duration
func (r *RequestRegistry) Cleanup(maxAge time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	count := 0

	for id, entry := range r.requests {
		if entry.Status != "running" && entry.Started.Before(cutoff) {
			delete(r.requests, id)
			count++
		}
	}
	return count
}

// GenerateRequestID creates a unique request ID with format: req-{origin}-{timestamp}-{random}
func GenerateRequestID(origin string) string {
	if origin == "" {
		origin = "unknown"
	}

	// Timestamp component (compact)
	ts := time.Now().Unix()

	// Random component
	randomBytes := make([]byte, 4)
	rand.Read(randomBytes)
	randomHex := hex.EncodeToString(randomBytes)

	return fmt.Sprintf("req-%s-%d-%s", origin, ts, randomHex)
}

// StartRegistryCleanup starts a background goroutine that periodically
// removes completed/failed/cancelled entries older than 1 hour.
func StartRegistryCleanup(registry *RequestRegistry) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			registry.Cleanup(1 * time.Hour)
		}
	}()
}

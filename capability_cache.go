// capability_cache.go
// In-memory, TTL-based cache for agent capability advertisements received
// from the bus. Enables cross-agent URI resolution and tool invocation by
// maintaining a local view of which agents expose which tools, MCP servers,
// memory sectors, and bus subscriptions.

package main

import (
	"sync"
	"time"
)

const defaultCapabilityTTL = 1 * time.Hour

// CapabilityCache stores agent capability advertisements with TTL-based expiry.
// Thread-safe for concurrent read/write access.
type CapabilityCache struct {
	mu      sync.RWMutex
	entries map[string]*capEntry
}

type capEntry struct {
	payload   AgentCapabilitiesPayload
	expiresAt time.Time
}

// NewCapabilityCache creates a new empty cache.
func NewCapabilityCache() *CapabilityCache {
	return &CapabilityCache{
		entries: make(map[string]*capEntry),
	}
}

// Get returns the cached capabilities for an agent, or nil if not found/expired.
// Performs lazy expiry: if the entry exists but is past its TTL, it is deleted
// and nil is returned.
func (c *CapabilityCache) Get(agentID string) *AgentCapabilitiesPayload {
	c.mu.RLock()
	entry, ok := c.entries[agentID]
	c.mu.RUnlock()

	if !ok {
		return nil
	}

	if time.Now().After(entry.expiresAt) {
		// Expired — promote to write lock and delete.
		c.mu.Lock()
		// Re-check under write lock in case another goroutine already cleaned it.
		if e, still := c.entries[agentID]; still && time.Now().After(e.expiresAt) {
			delete(c.entries, agentID)
		}
		c.mu.Unlock()
		return nil
	}

	// Return a copy so callers cannot mutate cache internals.
	copied := entry.payload
	return &copied
}

// Set stores capabilities for an agent with the given TTL.
// If ttl is 0, uses a default of 1 hour.
func (c *CapabilityCache) Set(agentID string, payload AgentCapabilitiesPayload, ttl time.Duration) {
	if ttl <= 0 {
		ttl = defaultCapabilityTTL
	}

	c.mu.Lock()
	c.entries[agentID] = &capEntry{
		payload:   payload,
		expiresAt: time.Now().Add(ttl),
	}
	c.mu.Unlock()
}

// List returns all non-expired cached capabilities keyed by agent ID.
func (c *CapabilityCache) List() map[string]AgentCapabilitiesPayload {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	result := make(map[string]AgentCapabilitiesPayload, len(c.entries))
	for id, entry := range c.entries {
		if now.Before(entry.expiresAt) || now.Equal(entry.expiresAt) {
			result[id] = entry.payload
		}
	}
	return result
}

// Delete removes an agent from the cache.
func (c *CapabilityCache) Delete(agentID string) {
	c.mu.Lock()
	delete(c.entries, agentID)
	c.mu.Unlock()
}

// ExpireSweep removes all expired entries. Call periodically or let
// StartExpirySweeper handle it automatically. Returns the number of
// entries removed.
func (c *CapabilityCache) ExpireSweep() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	removed := 0
	for id, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, id)
			removed++
		}
	}
	return removed
}

// StartExpirySweeper runs ExpireSweep every interval in a background goroutine.
// Returns a stop function that halts the sweeper when called.
func (c *CapabilityCache) StartExpirySweeper(interval time.Duration) func() {
	ticker := time.NewTicker(interval)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:
				c.ExpireSweep()
			case <-done:
				ticker.Stop()
				return
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
	}
}

// HasTool checks if a cached agent has a specific tool available.
// A tool is considered available if:
//   - The agent's entry exists and is not expired, AND
//   - The tool appears in the allow list (if the allow list is non-empty), AND
//   - The tool does NOT appear in the deny list.
//
// If the allow list is empty, all tools are considered allowed unless
// explicitly denied.
func (c *CapabilityCache) HasTool(agentID, tool string) bool {
	cap := c.Get(agentID)
	if cap == nil {
		return false
	}

	// Check deny list first — explicit deny always wins.
	for _, denied := range cap.Tools.Deny {
		if denied == tool {
			return false
		}
	}

	// If allow list is non-empty, tool must be explicitly listed.
	if len(cap.Tools.Allow) > 0 {
		for _, allowed := range cap.Tools.Allow {
			if allowed == tool {
				return true
			}
		}
		return false
	}

	// Allow list is empty and tool is not denied — allowed by default.
	return true
}

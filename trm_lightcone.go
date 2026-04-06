// trm_lightcone.go — Thread-safe per-conversation light cone state manager.
//
// Each conversation maintains its own light cone — the SSM hidden state
// that compresses the observer's trajectory through the workspace. The
// LightConeManager provides concurrent-safe access keyed by conversation ID.
package main

import (
	"sync"
	"time"
)

// LightConeInfo is a summary of a stored light cone for the /v1/lightcone endpoint.
type LightConeInfo struct {
	ConversationID string    `json:"conversation_id"`
	NLayers        int       `json:"n_layers"`
	LayerNorms     []float64 `json:"layer_norms"`
	CompressedNorm float64   `json:"compressed_norm"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// lightConeEntry is the internal storage for a light cone plus metadata.
type lightConeEntry struct {
	cone      *LightCone
	updatedAt time.Time
}

// LightConeManager provides thread-safe per-conversation light cone storage.
type LightConeManager struct {
	mu    sync.RWMutex
	cones map[string]*lightConeEntry
	trm   *MambaTRM // reference for computing norms
}

// NewLightConeManager creates a new manager. The trm parameter is used
// for computing light cone norms (can be nil if norms are not needed).
func NewLightConeManager(trm *MambaTRM) *LightConeManager {
	return &LightConeManager{
		cones: make(map[string]*lightConeEntry),
		trm:   trm,
	}
}

// Get returns the light cone for a conversation, or nil if none exists.
func (m *LightConeManager) Get(convID string) *LightCone {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.cones[convID]
	if !ok {
		return nil
	}
	return entry.cone
}

// Set stores or updates the light cone for a conversation.
func (m *LightConeManager) Set(convID string, lc *LightCone) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cones[convID] = &lightConeEntry{
		cone:      lc,
		updatedAt: time.Now(),
	}
}

// Delete removes the light cone for a conversation.
func (m *LightConeManager) Delete(convID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cones, convID)
}

// List returns summary information for all stored light cones.
func (m *LightConeManager) List() []LightConeInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]LightConeInfo, 0, len(m.cones))
	for convID, entry := range m.cones {
		info := LightConeInfo{
			ConversationID: convID,
			NLayers:        len(entry.cone.States),
			UpdatedAt:      entry.updatedAt,
		}
		if m.trm != nil {
			info.LayerNorms, info.CompressedNorm = m.trm.GetLightConeNorms(entry.cone)
		}
		infos = append(infos, info)
	}
	return infos
}

// Count returns the number of active light cones.
func (m *LightConeManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.cones)
}

// Prune removes light cones that haven't been updated since the given time.
// Returns the number of pruned entries.
func (m *LightConeManager) Prune(before time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	pruned := 0
	for convID, entry := range m.cones {
		if entry.updatedAt.Before(before) {
			delete(m.cones, convID)
			pruned++
		}
	}
	return pruned
}

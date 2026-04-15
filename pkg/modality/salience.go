// salience.go — Attentional field scoring for modality events.
//
// Extends the salience concept to real-time modality events: voice inputs,
// TTS outputs, gate decisions, transforms, errors. Each event boosts a
// keyed score that decays exponentially, giving the agent a live salience
// map of its sensorimotor activity.

package modality

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Score boosts per modality event type.
var defaultBoosts = map[string]float64{
	EventInput:       1.0, // perception just happened
	EventOutput:      0.8, // action was taken
	EventGate:        0.3, // gate decision, lower salience
	EventTransform:   0.2, // pipeline step
	EventStateChange: 0.5, // module lifecycle change
	EventError:       1.5, // errors are highly salient
}

// DefaultDecayHalfLife is the default half-life for exponential decay.
const DefaultDecayHalfLife = 5 * time.Minute

// DecayThreshold is the minimum score; entries below this are pruned.
const DecayThreshold = 0.01

// SalienceEntry tracks a single salience score with exponential decay.
type SalienceEntry struct {
	Key       string    `json:"key"`
	Score     float64   `json:"score"`
	LastEvent time.Time `json:"last_event"`
	EventType string    `json:"event_type"`
	Count     int       `json:"count"`
}

// Salience extends the attentional field with modality event scoring.
type Salience struct {
	mu     sync.RWMutex
	scores map[string]*SalienceEntry
}

// NewSalience creates a new modality salience tracker.
func NewSalience() *Salience {
	return &Salience{
		scores: make(map[string]*SalienceEntry),
	}
}

// OnEvent implements EventListener for the EventRouter. It extracts
// modality and channel from data, builds a key, and boosts the score.
func (ms *Salience) OnEvent(eventType string, data map[string]any) {
	channel, _ := data["channel"].(string)
	if channel == "" {
		channel = "unknown"
	}

	key := "modality:" + eventType + ":" + channel

	boost, ok := defaultBoosts[eventType]
	if !ok {
		boost = 0.1 // unknown event types get minimal salience
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()

	entry, exists := ms.scores[key]
	if !exists {
		entry = &SalienceEntry{Key: key}
		ms.scores[key] = entry
	}
	entry.Score += boost
	entry.LastEvent = time.Now()
	entry.EventType = eventType
	entry.Count++
}

// Decay applies exponential decay to all entries and prunes those
// below DecayThreshold. score *= 0.5 ^ (elapsed / halfLife)
func (ms *Salience) Decay(now time.Time, halfLife time.Duration) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	for key, entry := range ms.scores {
		elapsed := now.Sub(entry.LastEvent)
		if elapsed <= 0 {
			continue
		}
		factor := math.Pow(0.5, float64(elapsed)/float64(halfLife))
		entry.Score *= factor
		if entry.Score < DecayThreshold {
			delete(ms.scores, key)
		}
	}
}

// TopN returns the N highest-scoring entries, sorted descending.
func (ms *Salience) TopN(n int) []*SalienceEntry {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	entries := make([]*SalienceEntry, 0, len(ms.scores))
	for _, e := range ms.scores {
		cp := *e
		entries = append(entries, &cp)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Score > entries[j].Score
	})
	if n > 0 && len(entries) > n {
		entries = entries[:n]
	}
	return entries
}

// Snapshot returns all scores as a flat map (for HUD integration).
func (ms *Salience) Snapshot() map[string]float64 {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	out := make(map[string]float64, len(ms.scores))
	for key, entry := range ms.scores {
		out[key] = entry.Score
	}
	return out
}

// Score returns the current score for a single key, or 0 if absent.
func (ms *Salience) Score(key string) float64 {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	if entry, ok := ms.scores[key]; ok {
		return entry.Score
	}
	return 0
}

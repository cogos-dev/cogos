package types

import "time"

// CoherenceState represents the current workspace coherence status.
type CoherenceState struct {
	// Coherent is true if current state matches canonical.
	Coherent bool `json:"coherent"`

	// CanonicalHash is the stored baseline tree hash.
	CanonicalHash string `json:"canonical_hash"`

	// CurrentHash is the computed current tree hash.
	CurrentHash string `json:"current_hash"`

	// Drift lists files that have drifted from canonical.
	// Empty if Coherent is true.
	Drift []string `json:"drift,omitempty"`

	// Timestamp is when this state was computed.
	Timestamp time.Time `json:"timestamp"`

	// CheckDuration is how long the coherence check took.
	CheckDuration time.Duration `json:"check_duration,omitempty"`
}

// IsDrifted returns true if there are drifted files.
func (c *CoherenceState) IsDrifted() bool {
	return len(c.Drift) > 0
}

// DriftCount returns the number of drifted files.
func (c *CoherenceState) DriftCount() int {
	return len(c.Drift)
}

// CoherenceHistory tracks coherence state over time.
type CoherenceHistory struct {
	// States is a list of historical coherence states.
	States []CoherenceHistoryEntry `json:"states"`
}

// CoherenceHistoryEntry is a single historical coherence record.
type CoherenceHistoryEntry struct {
	// Timestamp is when this state was recorded.
	Timestamp time.Time `json:"timestamp"`

	// Hash is the tree hash at that time.
	Hash string `json:"hash"`

	// Coherent was the coherence status.
	Coherent bool `json:"coherent"`

	// DriftCount is how many files were drifted.
	DriftCount int `json:"drift_count,omitempty"`

	// Action describes what caused this entry (check, baseline, etc.).
	Action string `json:"action,omitempty"`
}

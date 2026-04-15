// state.go
// Generic state management for the reconciliation framework.
// Provides load/write/lineage operations for any resource provider.
//
// Each provider stores state at .cog/config/{resource_type}/.state.json
// using the State format from types.go.

package reconcile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StatePath returns the path to a provider's state file.
func StatePath(root, resourceType string) string {
	return filepath.Join(root, ".cog", "config", resourceType, ".state.json")
}

// LoadState loads the state file for a given resource type.
// Returns nil, nil if no state file exists yet.
func LoadState(root, resourceType string) (*State, error) {
	data, err := os.ReadFile(StatePath(root, resourceType))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s state: %w", resourceType, err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing %s state: %w", resourceType, err)
	}
	return &state, nil
}

// WriteState atomically writes the state file for a resource type.
// Increments serial and sets generated_at timestamp automatically.
func WriteState(root, resourceType string, state *State) error {
	state.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	state.Serial++
	state.ResourceType = resourceType

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s state: %w", resourceType, err)
	}

	sp := StatePath(root, resourceType)

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(sp), 0755); err != nil {
		return fmt.Errorf("creating state dir for %s: %w", resourceType, err)
	}

	// Atomic write: tmp file + rename
	tmp := sp + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("writing tmp %s state: %w", resourceType, err)
	}
	if err := os.Rename(tmp, sp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming %s state: %w", resourceType, err)
	}
	return nil
}

// NewState creates a fresh state with a new lineage.
func NewState(resourceType string) *State {
	return &State{
		Version:      1,
		Lineage:      GenerateLineage(),
		Serial:       0,
		ResourceType: resourceType,
		Resources:    []Resource{},
	}
}

// GenerateLineage creates a random hex string for state lineage tracking.
func GenerateLineage() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cogos-dev/cogos/sdk/internal/fs"
	"github.com/cogos-dev/cogos/sdk/types"
)

// signalFieldState represents the persisted signal field structure.
// This matches the format stored in .cog/run/signals/field_state.json
type signalFieldState struct {
	Signals map[string]map[string]*persistedSignal `json:"signals"` // location -> type -> signal
	SavedAt float64                                `json:"saved_at"`
}

// persistedSignal matches the kernel's signal format.
type persistedSignal struct {
	SignalType  string         `json:"signal_type"`
	Strength    float64        `json:"strength"`
	DepositedBy string         `json:"deposited_by"`
	DepositedAt float64        `json:"deposited_at"`
	HalfLife    float64        `json:"half_life"`
	DecayType   string         `json:"decay_type"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// toTypesSignal converts a persisted signal to a types.Signal.
func (ps *persistedSignal) toTypesSignal(location string) *types.Signal {
	depositedAt := time.Unix(int64(ps.DepositedAt), 0)
	return &types.Signal{
		Location:    location,
		Type:        ps.SignalType,
		DepositedAt: depositedAt,
		HalfLife:    ps.HalfLife,
		Strength:    ps.Strength,
		Metadata:    ps.Metadata,
	}
}

// signalProjector handles cog://signals/* namespace.
// Provides stigmergic coordination through signal field access.
type signalProjector struct {
	BaseProjector
	kernel *Kernel
}

// CanMutate returns true - signals can be deposited and removed.
func (p *signalProjector) CanMutate() bool {
	return true
}

// Resolve reads signals from the signal field.
//
// URI patterns:
//   - cog://signals - List all signals
//   - cog://signals/inference - Signals at 'inference' location
//   - cog://signals/inference?above=0.3 - Only signals with relevance > 0.3
//   - cog://signals?location=inference - Same as above
func (p *signalProjector) Resolve(ctx context.Context, uri *ParsedURI) (*Resource, error) {
	state, err := p.loadSignalField()
	if err != nil {
		if os.IsNotExist(err) {
			// No signals yet - return empty set
			return p.emptySignalResource(uri)
		}
		return nil, NewPathError("Resolve", p.signalFieldPath(), err)
	}

	// Filter parameters
	location := uri.Path
	if location == "" {
		location = uri.GetQuery("location")
	}
	aboveThreshold := uri.GetQueryFloat("above", 0.0)

	now := time.Now()
	result := &types.SignalSet{
		Location:  location,
		Signals:   make([]*types.Signal, 0),
		Timestamp: now,
	}

	if location != "" {
		// Get signals at specific location
		if locSignals, ok := state.Signals[location]; ok {
			for _, ps := range locSignals {
				sig := ps.toTypesSignal(location)
				rel := sig.Relevance(now)
				if rel > aboveThreshold {
					result.Signals = append(result.Signals, sig)
					if sig.IsActive(now) {
						result.ActiveCount++
					}
				}
			}
		}
	} else {
		// Get all signals
		for loc, locSignals := range state.Signals {
			for _, ps := range locSignals {
				sig := ps.toTypesSignal(loc)
				rel := sig.Relevance(now)
				if rel > aboveThreshold {
					result.Signals = append(result.Signals, sig)
					if sig.IsActive(now) {
						result.ActiveCount++
					}
				}
			}
		}
	}

	return NewJSONResource(uri.Raw, result)
}

// Mutate deposits or removes signals.
//
// Operations:
//   - Set: Deposit or update a signal at the URI location
//   - Delete: Remove a signal from the URI location
//
// The mutation content should be a JSON object with signal properties:
//
//	{
//	  "type": "ACTIVE",
//	  "strength": 1.0,
//	  "half_life": 4.0,
//	  "source": "cog-chat"
//	}
func (p *signalProjector) Mutate(ctx context.Context, uri *ParsedURI, m *Mutation) error {
	if uri.Path == "" {
		return NewURIError("Mutate", uri.Raw, fmt.Errorf("signal location required (e.g., cog://signals/inference)"))
	}

	location := uri.Path

	switch m.Op {
	case MutationSet:
		return p.depositSignal(location, m.Content)
	case MutationDelete:
		return p.removeSignal(location, m.Content)
	default:
		return NewURIError("Mutate", uri.Raw, fmt.Errorf("unsupported op: %s (use set or delete)", m.Op))
	}
}

// depositSignal adds or updates a signal at the given location.
func (p *signalProjector) depositSignal(location string, content []byte) error {
	// Parse the signal data from content
	var input struct {
		Type     string         `json:"type"`
		Strength float64        `json:"strength"`
		HalfLife float64        `json:"half_life"`
		Source   string         `json:"source"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(content, &input); err != nil {
		return NewError("Mutate", fmt.Errorf("invalid signal data: %w", err))
	}

	// Set defaults
	if input.Type == "" {
		input.Type = "ACTIVE"
	}
	if input.Strength == 0 {
		input.Strength = 1.0
	}
	if input.HalfLife == 0 {
		input.HalfLife = 4.0 // 4 hours default
	}

	// Load existing state
	state, err := p.loadSignalField()
	if err != nil && !os.IsNotExist(err) {
		return NewPathError("Mutate", p.signalFieldPath(), err)
	}
	if state == nil {
		state = &signalFieldState{
			Signals: make(map[string]map[string]*persistedSignal),
		}
	}

	// Ensure location map exists
	if state.Signals[location] == nil {
		state.Signals[location] = make(map[string]*persistedSignal)
	}

	// Create the signal
	now := time.Now()
	signal := &persistedSignal{
		SignalType:  input.Type,
		Strength:    input.Strength,
		DepositedBy: input.Source,
		DepositedAt: float64(now.Unix()),
		HalfLife:    input.HalfLife,
		DecayType:   "exponential",
		Metadata:    input.Metadata,
	}

	// Store the signal (keyed by type for now)
	state.Signals[location][input.Type] = signal
	state.SavedAt = float64(now.Unix())

	// Write atomically
	return p.saveSignalField(state)
}

// removeSignal removes a signal from the given location.
func (p *signalProjector) removeSignal(location string, content []byte) error {
	// Parse the signal type to remove
	var input struct {
		Type string `json:"type"`
	}
	if len(content) > 0 {
		if err := json.Unmarshal(content, &input); err != nil {
			return NewError("Mutate", fmt.Errorf("invalid signal data: %w", err))
		}
	}

	// Load existing state
	state, err := p.loadSignalField()
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Nothing to remove
		}
		return NewPathError("Mutate", p.signalFieldPath(), err)
	}

	// Remove the signal
	if locSignals, ok := state.Signals[location]; ok {
		if input.Type != "" {
			// Remove specific type
			delete(locSignals, input.Type)
		} else {
			// Remove all signals at location
			delete(state.Signals, location)
		}
	}

	state.SavedAt = float64(time.Now().Unix())

	// Write atomically
	return p.saveSignalField(state)
}

// signalFieldPath returns the path to the signal field state file.
func (p *signalProjector) signalFieldPath() string {
	return filepath.Join(p.kernel.StateDir(), "signals", "field_state.json")
}

// loadSignalField reads the signal field from disk.
func (p *signalProjector) loadSignalField() (*signalFieldState, error) {
	return fs.ReadJSON[*signalFieldState](p.signalFieldPath())
}

// saveSignalField writes the signal field to disk atomically.
func (p *signalProjector) saveSignalField(state *signalFieldState) error {
	return fs.WriteJSON(p.signalFieldPath(), state, 0644)
}

// emptySignalResource returns an empty signal set resource.
func (p *signalProjector) emptySignalResource(uri *ParsedURI) (*Resource, error) {
	result := &types.SignalSet{
		Location:    uri.Path,
		Signals:     make([]*types.Signal, 0),
		ActiveCount: 0,
		Timestamp:   time.Now(),
	}
	return NewJSONResource(uri.Raw, result)
}

package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cogos-dev/cogos/sdk"
	"github.com/cogos-dev/cogos/sdk/types"
)

// SignalClient provides ergonomic access to cog://signals/*
//
// Signals are the stigmergic coordination mechanism - ephemeral markers
// that decay over time and indicate workspace activity.
//
// Common locations:
//   - "inference" - Inference activity signals
//   - "mem/semantic" - Semantic memory activity
//   - "thread" - Thread activity
//   - "coherence" - Coherence state signals
//
// All methods are goroutine-safe.
type SignalClient struct {
	kernel *sdk.Kernel
}

// NewSignalClient creates a new SignalClient.
func NewSignalClient(k *sdk.Kernel) *SignalClient {
	return &SignalClient{kernel: k}
}

// SignalField represents the complete signal field state.
type SignalField struct {
	// Signals contains all signals grouped by location.
	Signals map[string][]*types.Signal

	// TotalActive is the count of signals above relevance threshold.
	TotalActive int

	// Timestamp is when the field was queried.
	Timestamp time.Time
}

// Field returns the complete signal field state.
//
// Example:
//
//	field, err := c.Signal.Field()
//	for loc, signals := range field.Signals {
//	    fmt.Printf("%s: %d signals\n", loc, len(signals))
//	}
func (c *SignalClient) Field() (*SignalField, error) {
	return c.FieldContext(context.Background())
}

// FieldContext is like Field but accepts a context.
func (c *SignalClient) FieldContext(ctx context.Context) (*SignalField, error) {
	resource, err := c.kernel.ResolveContext(ctx, "cog://signals")
	if err != nil {
		return nil, err
	}

	var signalSet types.SignalSet
	if err := json.Unmarshal(resource.Content, &signalSet); err != nil {
		return nil, fmt.Errorf("parse signal field: %w", err)
	}

	// Group signals by location
	field := &SignalField{
		Signals:     make(map[string][]*types.Signal),
		TotalActive: signalSet.ActiveCount,
		Timestamp:   signalSet.Timestamp,
	}

	for _, sig := range signalSet.Signals {
		field.Signals[sig.Location] = append(field.Signals[sig.Location], sig)
		if sig.IsActive(time.Now()) {
			field.TotalActive++
		}
	}

	return field, nil
}

// Get returns all signals at a specific location.
//
// Example:
//
//	signals, err := c.Signal.Get("inference")
//	for _, sig := range signals {
//	    fmt.Printf("%s: relevance=%.2f\n", sig.Type, sig.Relevance(time.Now()))
//	}
func (c *SignalClient) Get(location string) ([]*types.Signal, error) {
	return c.GetContext(context.Background(), location)
}

// GetContext is like Get but accepts a context.
func (c *SignalClient) GetContext(ctx context.Context, location string) ([]*types.Signal, error) {
	uri := fmt.Sprintf("cog://signals/%s", location)
	resource, err := c.kernel.ResolveContext(ctx, uri)
	if err != nil {
		return nil, err
	}

	var signalSet types.SignalSet
	if err := json.Unmarshal(resource.Content, &signalSet); err != nil {
		return nil, fmt.Errorf("parse signals: %w", err)
	}

	return signalSet.Signals, nil
}

// Above returns all signals with relevance above the threshold.
// The threshold should be in the range [0, 1].
//
// Example:
//
//	// Get signals that are still "hot" (relevance > 0.5)
//	signals, err := c.Signal.Above(0.5)
func (c *SignalClient) Above(threshold float64) ([]*types.Signal, error) {
	return c.AboveContext(context.Background(), threshold)
}

// AboveContext is like Above but accepts a context.
func (c *SignalClient) AboveContext(ctx context.Context, threshold float64) ([]*types.Signal, error) {
	uri := fmt.Sprintf("cog://signals?above=%f", threshold)
	resource, err := c.kernel.ResolveContext(ctx, uri)
	if err != nil {
		return nil, err
	}

	var signalSet types.SignalSet
	if err := json.Unmarshal(resource.Content, &signalSet); err != nil {
		return nil, fmt.Errorf("parse signals: %w", err)
	}

	return signalSet.Signals, nil
}

// Active returns all signals that are currently active.
// A signal is active if its relevance is above the SRC threshold (e^(-1) * sqrt(2/3) ~ 0.30).
//
// Example:
//
//	signals, err := c.Signal.Active()
func (c *SignalClient) Active() ([]*types.Signal, error) {
	return c.ActiveContext(context.Background())
}

// ActiveContext is like Active but accepts a context.
func (c *SignalClient) ActiveContext(ctx context.Context) ([]*types.Signal, error) {
	// The SRC threshold is e^(-1) * sqrt(2/3) ~ 0.30
	threshold := 0.30
	return c.AboveContext(ctx, threshold)
}

// Deposit deposits a signal at a location.
//
// Example:
//
//	sig := types.Signal{
//	    Location: "inference",
//	    Type:     "ACTIVE",
//	    Strength: 1.0,
//	    HalfLife: 4.0, // hours
//	}
//	err := c.Signal.Deposit(sig)
func (c *SignalClient) Deposit(signal types.Signal) error {
	return c.DepositContext(context.Background(), signal)
}

// DepositContext is like Deposit but accepts a context.
func (c *SignalClient) DepositContext(ctx context.Context, signal types.Signal) error {
	if signal.Location == "" {
		return fmt.Errorf("signal location required")
	}

	// Set defaults
	if signal.Type == "" {
		signal.Type = "ACTIVE"
	}
	if signal.Strength == 0 {
		signal.Strength = 1.0
	}
	if signal.HalfLife == 0 {
		signal.HalfLife = 4.0 // 4 hours default
	}

	// Build mutation content
	content, err := json.Marshal(map[string]interface{}{
		"type":      signal.Type,
		"strength":  signal.Strength,
		"half_life": signal.HalfLife,
		"source":    signal.Metadata["source"],
		"metadata":  signal.Metadata,
	})
	if err != nil {
		return fmt.Errorf("marshal signal: %w", err)
	}

	uri := fmt.Sprintf("cog://signals/%s", signal.Location)
	mutation := sdk.NewSetMutation(content)
	return c.kernel.MutateContext(ctx, uri, mutation)
}

// DepositQuick deposits a simple signal with default parameters.
//
// Example:
//
//	err := c.Signal.DepositQuick("inference", "ACTIVE")
func (c *SignalClient) DepositQuick(location, signalType string) error {
	return c.Deposit(types.Signal{
		Location: location,
		Type:     signalType,
		Strength: 1.0,
		HalfLife: 4.0,
	})
}

// Remove removes a signal by type from a location.
// If signalType is empty, removes all signals at the location.
//
// Example:
//
//	err := c.Signal.Remove("inference", "ACTIVE")
func (c *SignalClient) Remove(location, signalType string) error {
	return c.RemoveContext(context.Background(), location, signalType)
}

// RemoveContext is like Remove but accepts a context.
func (c *SignalClient) RemoveContext(ctx context.Context, location, signalType string) error {
	if location == "" {
		return fmt.Errorf("signal location required")
	}

	var content []byte
	if signalType != "" {
		var err error
		content, err = json.Marshal(map[string]string{"type": signalType})
		if err != nil {
			return fmt.Errorf("marshal remove request: %w", err)
		}
	}

	uri := fmt.Sprintf("cog://signals/%s", location)
	mutation := sdk.NewDeleteMutation()
	mutation.Content = content
	return c.kernel.MutateContext(ctx, uri, mutation)
}

// Clear removes all signals at a location.
//
// Example:
//
//	err := c.Signal.Clear("inference")
func (c *SignalClient) Clear(location string) error {
	return c.Remove(location, "")
}

// HasActiveAt returns true if there are any active signals at the location.
//
// Example:
//
//	if c.Signal.HasActiveAt("inference") {
//	    fmt.Println("Inference activity detected")
//	}
func (c *SignalClient) HasActiveAt(location string) bool {
	signals, err := c.Get(location)
	if err != nil {
		return false
	}

	now := time.Now()
	for _, sig := range signals {
		if sig.IsActive(now) {
			return true
		}
	}
	return false
}

// CountActive returns the count of active signals at a location.
func (c *SignalClient) CountActive(location string) int {
	signals, err := c.Get(location)
	if err != nil {
		return 0
	}

	count := 0
	now := time.Now()
	for _, sig := range signals {
		if sig.IsActive(now) {
			count++
		}
	}
	return count
}

// MaxRelevance returns the highest relevance signal at a location.
// Returns nil if no signals exist at the location.
func (c *SignalClient) MaxRelevance(location string) *types.Signal {
	signals, err := c.Get(location)
	if err != nil || len(signals) == 0 {
		return nil
	}

	now := time.Now()
	var maxSig *types.Signal
	maxRel := -1.0

	for _, sig := range signals {
		rel := sig.Relevance(now)
		if rel > maxRel {
			maxRel = rel
			maxSig = sig
		}
	}

	return maxSig
}

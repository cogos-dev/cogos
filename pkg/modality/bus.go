// bus.go — Bus orchestrates modality modules.
//
// The bus is the sensorimotor boundary between the agent and its
// environment. It owns the module registry, routes perceive/act
// through the Gate/Decoder/Encoder pipeline, and exposes a HUD
// for the agent's context assembly.

package modality

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ChannelConnection tracks an active channel (placeholder for A5).
type ChannelConnection struct {
	ID         string         `json:"id"`
	Modalities []ModalityType `json:"modalities"`
}

// BusEvent is an internal log entry for the HUD.
type BusEvent struct {
	Type      string         `json:"type"`
	Modality  string         `json:"modality"`
	Channel   string         `json:"channel"`
	Timestamp time.Time      `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"`
}

const defaultMaxEvents = 500

// Bus orchestrates modality modules. It is the sensorimotor
// boundary between the agent and its environment.
type Bus struct {
	mu        sync.RWMutex
	modules   map[ModalityType]Module
	order     []ModalityType // insertion order for deterministic start/stop
	channels  map[string]*ChannelConnection
	events    []BusEvent
	maxEvents int
}

// NewBus creates a bus with default configuration.
func NewBus() *Bus {
	return &Bus{
		modules:   make(map[ModalityType]Module),
		channels:  make(map[string]*ChannelConnection),
		events:    make([]BusEvent, 0, 64),
		maxEvents: defaultMaxEvents,
	}
}

// Register adds a modality module. Errors if the modality is already registered.
func (b *Bus) Register(module Module) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	mt := module.Type()
	if _, exists := b.modules[mt]; exists {
		return fmt.Errorf("modality %s: already registered", mt)
	}
	b.modules[mt] = module
	b.order = append(b.order, mt)
	return nil
}

// Perceive runs raw input through gate (if present) -> decode -> cognitive event.
// Returns (nil, nil) when the gate rejects input (rejection is not an error).
func (b *Bus) Perceive(raw []byte, modality ModalityType, channel string) (*CognitiveEvent, error) {
	b.mu.RLock()
	module, ok := b.modules[modality]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("perceive: no module for modality %s", modality)
	}

	// Gate check (optional).
	if g := module.Gate(); g != nil {
		result, err := g.Check(raw, modality)
		if err != nil {
			return nil, fmt.Errorf("perceive: gate error for %s: %w", modality, err)
		}
		b.LogEvent(BusEvent{
			Type: "modality.gate", Modality: string(modality),
			Channel: channel, Timestamp: time.Now(),
			Data: map[string]any{
				"allowed": result.Allowed, "confidence": result.Confidence, "reason": result.Reason,
			},
		})
		if !result.Allowed {
			return nil, nil
		}
	}

	// Decode.
	decoder := module.Decoder()
	if decoder == nil {
		return nil, fmt.Errorf("perceive: module %s has no decoder", modality)
	}
	event, err := decoder.Decode(raw, modality, channel)
	if err != nil {
		return nil, fmt.Errorf("perceive: decode error for %s: %w", modality, err)
	}
	b.LogEvent(BusEvent{
		Type: "modality.input", Modality: string(modality),
		Channel: channel, Timestamp: time.Now(),
		Data: map[string]any{"content_len": len(event.Content), "confidence": event.Confidence},
	})
	return event, nil
}

// Act encodes a cognitive intent into raw output via the target modality's encoder.
func (b *Bus) Act(intent *CognitiveIntent) (*EncodedOutput, error) {
	b.mu.RLock()
	module, ok := b.modules[intent.Modality]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("act: no module for modality %s", intent.Modality)
	}
	encoder := module.Encoder()
	if encoder == nil {
		return nil, fmt.Errorf("act: module %s has no encoder", intent.Modality)
	}
	output, err := encoder.Encode(intent)
	if err != nil {
		return nil, fmt.Errorf("act: encode error for %s: %w", intent.Modality, err)
	}
	b.LogEvent(BusEvent{
		Type: "modality.output", Modality: string(intent.Modality),
		Channel: intent.Channel, Timestamp: time.Now(),
		Data: map[string]any{"mime_type": output.MimeType, "bytes": len(output.Data)},
	})
	return output, nil
}

// HUD returns current bus state for the agent's context assembly.
func (b *Bus) HUD() map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()

	modules := make(map[string]any, len(b.modules))
	for mt, mod := range b.modules {
		state := mod.State()
		entry := map[string]any{"status": string(state.Status)}
		if state.PID != 0 {
			entry["pid"] = state.PID
		}
		if state.LastError != "" {
			entry["error"] = state.LastError
		}
		modules[string(mt)] = entry
	}

	channels := make(map[string]any, len(b.channels))
	for id, ch := range b.channels {
		mods := make([]string, len(ch.Modalities))
		for i, m := range ch.Modalities {
			mods[i] = string(m)
		}
		channels[id] = map[string]any{"modalities": mods}
	}

	n := len(b.events)
	start := n - 10
	if start < 0 {
		start = 0
	}
	recent := make([]map[string]any, 0, n-start)
	for _, ev := range b.events[start:] {
		recent = append(recent, map[string]any{
			"type": ev.Type, "modality": ev.Modality, "channel": ev.Channel,
			"timestamp": ev.Timestamp, "data": ev.Data,
		})
	}
	return map[string]any{"modules": modules, "channels": channels, "recent_events": recent}
}

// Start starts all registered modules in registration order.
func (b *Bus) Start(ctx context.Context) error {
	b.mu.RLock()
	order := make([]ModalityType, len(b.order))
	copy(order, b.order)
	b.mu.RUnlock()
	for _, mt := range order {
		b.mu.RLock()
		mod := b.modules[mt]
		b.mu.RUnlock()
		if err := mod.Start(ctx); err != nil {
			return fmt.Errorf("start module %s: %w", mt, err)
		}
	}
	return nil
}

// Stop stops all registered modules in reverse registration order.
func (b *Bus) Stop(ctx context.Context) error {
	b.mu.RLock()
	order := make([]ModalityType, len(b.order))
	copy(order, b.order)
	b.mu.RUnlock()
	var firstErr error
	for i := len(order) - 1; i >= 0; i-- {
		b.mu.RLock()
		mod := b.modules[order[i]]
		b.mu.RUnlock()
		if err := mod.Stop(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("stop module %s: %w", order[i], err)
		}
	}
	return firstErr
}

// Health returns the current status of every registered module.
func (b *Bus) Health() map[ModalityType]ModuleStatus {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[ModalityType]ModuleStatus, len(b.modules))
	for mt, mod := range b.modules {
		out[mt] = mod.Health()
	}
	return out
}

// Order returns the module registration order (for HUD formatting).
func (b *Bus) Order() []ModalityType {
	b.mu.RLock()
	defer b.mu.RUnlock()
	order := make([]ModalityType, len(b.order))
	copy(order, b.order)
	return order
}

// LogEvent appends a BusEvent, trimming to maxEvents.
func (b *Bus) LogEvent(ev BusEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, ev)
	if len(b.events) > b.maxEvents {
		b.events = b.events[len(b.events)-b.maxEvents:]
	}
}

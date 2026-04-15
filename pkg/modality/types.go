// types.go — Core types and interfaces for the modality bus.
//
// Defines the Gate/Decoder/Encoder pattern for modality modules.
// Each modality module (text, voice, vision, spatial) implements
// ModalityModule to participate in the sensorimotor boundary.

package modality

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------------
// Modality type enum
// ---------------------------------------------------------------------------

// ModalityType identifies the sensory modality a module handles.
type ModalityType string

const (
	Text    ModalityType = "text"
	Voice   ModalityType = "voice"
	Vision  ModalityType = "vision"
	Spatial ModalityType = "spatial"
)

// ---------------------------------------------------------------------------
// Module status enum
// ---------------------------------------------------------------------------

// ModuleStatus represents the operational status of a modality module.
type ModuleStatus string

const (
	StatusStarting ModuleStatus = "starting"
	StatusHealthy  ModuleStatus = "healthy"
	StatusDegraded ModuleStatus = "degraded"
	StatusStopped  ModuleStatus = "stopped"
	StatusCrashed  ModuleStatus = "crashed"
)

// ---------------------------------------------------------------------------
// Core data types
// ---------------------------------------------------------------------------

// CognitiveEvent represents a decoded perception -- raw signal transformed
// into meaning. This is what the agent "sees" after a decoder processes
// raw input (e.g., transcribed text from audio, a caption from an image).
type CognitiveEvent struct {
	Modality   ModalityType   `json:"modality"`
	Channel    string         `json:"channel"`
	Content    string         `json:"content"`
	Confidence float64        `json:"confidence,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`
}

// CognitiveIntent represents a desire to act -- meaning to be encoded
// into raw signal. This is what the agent "says" before an encoder
// transforms it into output (e.g., text into synthesized speech).
type CognitiveIntent struct {
	Modality ModalityType   `json:"modality"`
	Channel  string         `json:"channel"`
	Content  string         `json:"content"`
	Params   map[string]any `json:"params,omitempty"`
}

// EncodedOutput represents the result of encoding an intent into raw
// signal, ready for channel delivery (e.g., WAV audio bytes, PNG image).
type EncodedOutput struct {
	Modality ModalityType   `json:"modality"`
	Data     []byte         `json:"data,omitempty"`
	MimeType string         `json:"mime_type,omitempty"`
	Duration time.Duration  `json:"duration,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// GateResult represents the decision of an input gate -- whether raw
// input should pass through to the decoder for processing.
type GateResult struct {
	Allowed    bool    `json:"allowed"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason,omitempty"`
}

// ModuleState represents the current operational state of a modality
// module, used for health monitoring and the agent's HUD.
type ModuleState struct {
	Status    ModuleStatus   `json:"status"`
	Modality  ModalityType   `json:"modality"`
	PID       int            `json:"pid,omitempty"`
	Uptime    time.Duration  `json:"uptime,omitempty"`
	LastError string         `json:"last_error,omitempty"`
	Metrics   map[string]any `json:"metrics,omitempty"`
}

// ---------------------------------------------------------------------------
// Interfaces -- the module contract
// ---------------------------------------------------------------------------

// Gate filters incoming signals -- the perception threshold.
// A gate decides whether raw input contains signal worth decoding
// (e.g., VAD for voice, change detection for vision).
type Gate interface {
	Check(raw []byte, modality ModalityType) (*GateResult, error)
}

// Decoder transforms raw signal into cognitive events.
// This is the inbound half of the sensorimotor boundary
// (e.g., STT for voice, OCR for vision).
type Decoder interface {
	Decode(raw []byte, modality ModalityType, channel string) (*CognitiveEvent, error)
}

// Encoder transforms cognitive intents into raw signal.
// This is the outbound half of the sensorimotor boundary
// (e.g., TTS for voice, image generation for vision).
type Encoder interface {
	Encode(intent *CognitiveIntent) (*EncodedOutput, error)
}

// Module is the full module contract. Each sensory modality
// (text, voice, vision, spatial) implements this interface to
// participate in the modality bus.
type Module interface {
	// Type returns the modality this module handles.
	Type() ModalityType

	// Gate returns the input gate, or nil for passthrough modalities
	// that accept all input (e.g., text).
	Gate() Gate

	// Decoder returns the signal-to-event decoder.
	Decoder() Decoder

	// Encoder returns the intent-to-signal encoder.
	Encoder() Encoder

	// State returns current operational state for health monitoring
	// and the agent's HUD.
	State() *ModuleState

	// Start initializes the module. For subprocess-backed modules,
	// this spawns the Python child process and waits for readiness.
	Start(ctx context.Context) error

	// Stop gracefully shuts down the module, sending shutdown commands
	// to any child processes and waiting for clean exit.
	Stop(ctx context.Context) error

	// Health returns the current operational status of the module.
	Health() ModuleStatus
}

// EventListener receives modality events for real-time processing.
type EventListener func(eventType string, data map[string]any)

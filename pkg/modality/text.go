// text.go — Text modality module: pure passthrough.
//
// No subprocess, no transformation. Text is the identity modality.
// This is the reference implementation for Module.

package modality

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------------
// TextModule — implements Module
// ---------------------------------------------------------------------------

// TextModule implements Module for text — pure passthrough.
// No subprocess, no transformation. Text is the identity modality.
type TextModule struct {
	status  ModuleStatus
	decoder *textDecoder
	encoder *textEncoder
}

// NewTextModule creates a text modality module.
func NewTextModule() *TextModule {
	return &TextModule{
		status:  StatusStopped,
		decoder: &textDecoder{},
		encoder: &textEncoder{},
	}
}

// Type returns Text.
func (m *TextModule) Type() ModalityType { return Text }

// Gate returns nil — text has no input gate, all input passes through.
func (m *TextModule) Gate() Gate { return nil }

// Decoder returns the text decoder.
func (m *TextModule) Decoder() Decoder { return m.decoder }

// Encoder returns the text encoder.
func (m *TextModule) Encoder() Encoder { return m.encoder }

// State returns the current module state.
func (m *TextModule) State() *ModuleState {
	return &ModuleState{
		Status:   m.status,
		Modality: Text,
	}
}

// Start sets status to healthy (no subprocess needed).
func (m *TextModule) Start(_ context.Context) error {
	m.status = StatusHealthy
	return nil
}

// Stop sets status to stopped.
func (m *TextModule) Stop(_ context.Context) error {
	m.status = StatusStopped
	return nil
}

// Health returns the current status.
func (m *TextModule) Health() ModuleStatus { return m.status }

// ---------------------------------------------------------------------------
// textDecoder / textEncoder — identity transforms
// ---------------------------------------------------------------------------

// textDecoder decodes raw text bytes into a CognitiveEvent.
type textDecoder struct{}

func (d *textDecoder) Decode(raw []byte, _ ModalityType, channel string) (*CognitiveEvent, error) {
	return &CognitiveEvent{
		Modality:   Text,
		Channel:    channel,
		Content:    string(raw),
		Confidence: 1.0,
		Timestamp:  time.Now(),
	}, nil
}

// textEncoder encodes a CognitiveIntent into raw text bytes.
type textEncoder struct{}

func (e *textEncoder) Encode(intent *CognitiveIntent) (*EncodedOutput, error) {
	return &EncodedOutput{
		Modality: Text,
		Data:     []byte(intent.Content),
		MimeType: "text/plain",
	}, nil
}

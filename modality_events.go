// modality_events.go — Event construction for the modality bus.
//
// Event data structs and constants are defined in pkg/modality.
// This file provides the event construction functions that depend
// on kernel ledger types (EventPayload).

package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cogos-dev/cogos/pkg/modality"
)

// Re-export event type constants.
const (
	EventModalityInput       = modality.EventInput
	EventModalityOutput      = modality.EventOutput
	EventModalityTransform   = modality.EventTransform
	EventModalityGate        = modality.EventGate
	EventModalityStateChange = modality.EventStateChange
	EventModalityError       = modality.EventError
)

// Type aliases for event data structs.
type ModalityInputData = modality.InputData
type ModalityOutputData = modality.OutputData
type ModalityTransformData = modality.TransformData
type ModalityGateData = modality.GateData
type ModalityStateChangeData = modality.StateChangeData
type ModalityErrorData = modality.ErrorData

// newModalityPayload builds an EventPayload from a typed data struct.
func newModalityPayload(eventType, sessionID string, data any) (*EventPayload, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal data: %w", eventType, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: unmarshal data: %w", eventType, err)
	}
	return &EventPayload{
		Type:      eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		SessionID: sessionID,
		Data:      m,
	}, nil
}

// requireFields returns an error naming the first empty field, or nil.
func requireFields(eventType string, pairs ...string) error {
	return modality.RequireFields(eventType, pairs...)
}

// NewModalityInputEvent creates an EventPayload for a modality.input event.
func NewModalityInputEvent(sessionID string, d *ModalityInputData) (*EventPayload, error) {
	if err := requireFields(EventModalityInput,
		"modality", d.Modality, "channel", d.Channel, "transcript", d.Transcript,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityInput, sessionID, d)
}

// NewModalityOutputEvent creates an EventPayload for a modality.output event.
func NewModalityOutputEvent(sessionID string, d *ModalityOutputData) (*EventPayload, error) {
	if err := requireFields(EventModalityOutput,
		"modality", d.Modality, "channel", d.Channel, "text", d.Text,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityOutput, sessionID, d)
}

// NewModalityTransformEvent creates an EventPayload for a modality.transform event.
func NewModalityTransformEvent(sessionID string, d *ModalityTransformData) (*EventPayload, error) {
	if err := requireFields(EventModalityTransform,
		"from_modality", d.FromModality, "to_modality", d.ToModality, "step", d.Step,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityTransform, sessionID, d)
}

// NewModalityGateEvent creates an EventPayload for a modality.gate event.
func NewModalityGateEvent(sessionID string, d *ModalityGateData) (*EventPayload, error) {
	if err := requireFields(EventModalityGate,
		"modality", d.Modality, "channel", d.Channel, "decision", d.Decision,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityGate, sessionID, d)
}

// NewModalityStateChangeEvent creates an EventPayload for a modality.state_change event.
func NewModalityStateChangeEvent(sessionID string, d *ModalityStateChangeData) (*EventPayload, error) {
	if err := requireFields(EventModalityStateChange,
		"modality", d.Modality, "module", d.Module,
		"from_state", d.FromState, "to_state", d.ToState,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityStateChange, sessionID, d)
}

// NewModalityErrorEvent creates an EventPayload for a modality.error event.
func NewModalityErrorEvent(sessionID string, d *ModalityErrorData) (*EventPayload, error) {
	if err := requireFields(EventModalityError,
		"modality", d.Modality, "module", d.Module, "error", d.Error,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityError, sessionID, d)
}

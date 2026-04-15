package main

import (
	"github.com/cogos-dev/cogos/pkg/cogblock"
)

// Re-export ledger types and functions from pkg/cogblock so existing kernel
// code continues to compile. The canonical implementations now live in
// the importable library package.

// Type aliases for backward compatibility.
type EventEnvelope = cogblock.EventEnvelope
type EventPayload = cogblock.EventPayload
type EventMetadata = cogblock.EventMetadata

// Function wrappers delegate to pkg/cogblock.

func CanonicalizeEvent(payload *EventPayload) ([]byte, error) {
	return cogblock.CanonicalizeEvent(payload)
}

func HashEvent(canonicalBytes []byte, algorithm string) (string, error) {
	return cogblock.HashEvent(canonicalBytes, algorithm)
}

func AppendEvent(workspaceRoot, sessionID string, envelope *EventEnvelope) error {
	return cogblock.AppendEvent(workspaceRoot, sessionID, envelope)
}

func GetLastEvent(workspaceRoot, sessionID string) (*EventEnvelope, error) {
	return cogblock.GetLastEvent(workspaceRoot, sessionID)
}

func GetHashAlgorithm(workspaceRoot string) (string, error) {
	return cogblock.GetHashAlgorithm(workspaceRoot)
}

func VerifyLedger(workspaceRoot, sessionID string) error {
	return cogblock.VerifyLedger(workspaceRoot, sessionID)
}

func NewEventEnvelope(eventType, sessionID string) *EventEnvelope {
	return cogblock.NewEventEnvelope(eventType, sessionID)
}

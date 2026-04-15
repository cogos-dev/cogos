// modality_types.go — Re-exports from pkg/modality for kernel use.
//
// All core types and interfaces are defined in pkg/modality.
// This file provides package-level aliases so existing kernel code
// continues to compile without import changes.

package main

import "github.com/cogos-dev/cogos/pkg/modality"

// Type aliases — modality types.
type ModalityType = modality.ModalityType

const (
	ModalityText    = modality.Text
	ModalityVoice   = modality.Voice
	ModalityVision  = modality.Vision
	ModalitySpatial = modality.Spatial
)

// Type aliases — module status.
type ModuleStatus = modality.ModuleStatus

const (
	ModuleStatusStarting = modality.StatusStarting
	ModuleStatusHealthy  = modality.StatusHealthy
	ModuleStatusDegraded = modality.StatusDegraded
	ModuleStatusStopped  = modality.StatusStopped
	ModuleStatusCrashed  = modality.StatusCrashed
)

// Type aliases — core data types.
type CognitiveEvent = modality.CognitiveEvent
type CognitiveIntent = modality.CognitiveIntent
type EncodedOutput = modality.EncodedOutput
type GateResult = modality.GateResult
type ModuleState = modality.ModuleState

// Type aliases — interfaces.
type Gate = modality.Gate
type Decoder = modality.Decoder
type Encoder = modality.Encoder
type ModalityModule = modality.Module

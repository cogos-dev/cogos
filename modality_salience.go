// modality_salience.go — Re-exports from pkg/modality for kernel use.
//
// The salience implementation lives in pkg/modality. This file provides
// package-level aliases so existing kernel code compiles unchanged.

package main

import "github.com/cogos-dev/cogos/pkg/modality"

// Type aliases — salience types.
type SalienceEntry = modality.SalienceEntry
type ModalitySalience = modality.Salience

// Re-export constants.
const DefaultDecayHalfLife = modality.DefaultDecayHalfLife

// NewModalitySalience creates a new modality salience tracker.
func NewModalitySalience() *ModalitySalience {
	return modality.NewSalience()
}

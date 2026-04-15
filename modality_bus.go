// modality_bus.go — Re-exports from pkg/modality for kernel use.
//
// The bus implementation lives in pkg/modality. This file provides
// package-level aliases so existing kernel code compiles unchanged.

package main

import "github.com/cogos-dev/cogos/pkg/modality"

// Type aliases — bus types.
type ChannelConnection = modality.ChannelConnection
type BusEvent = modality.BusEvent
type ModalityBus = modality.Bus

// NewModalityBus creates a new modality bus.
func NewModalityBus() *ModalityBus {
	return modality.NewBus()
}

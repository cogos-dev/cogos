// modality_supervisor.go — Re-exports from pkg/modality for kernel use.
//
// The supervisor implementation lives in pkg/modality. This file provides
// package-level aliases so existing kernel code compiles unchanged.

package main

import "github.com/cogos-dev/cogos/pkg/modality"

// Type aliases — supervisor types.
type SupervisorConfig = modality.SupervisorConfig
type ManagedModule = modality.ManagedModule
type ProcessSupervisor = modality.ProcessSupervisor

// NewProcessSupervisor creates a new process supervisor.
func NewProcessSupervisor(rootDir string) *ProcessSupervisor {
	return modality.NewProcessSupervisor(rootDir)
}

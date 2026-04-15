// modality_wire.go — Re-exports from pkg/modality for kernel use.
//
// The wire protocol implementation lives in pkg/modality. This file
// provides package-level aliases so existing kernel code compiles unchanged.

package main

import "github.com/cogos-dev/cogos/pkg/modality"

// Type aliases — wire protocol types.
type WireMessage = modality.WireMessage
type SubprocessConn = modality.SubprocessConn

// Re-export constants.
const maxWireLineSize = modality.MaxWireLineSize

// NewSubprocessConn spawns a child process and wires stdin/stdout pipes.
func NewSubprocessConn(module, command, stderrLogPath string, args ...string) (*SubprocessConn, error) {
	return modality.NewSubprocessConn(module, command, stderrLogPath, args...)
}

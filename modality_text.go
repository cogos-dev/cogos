// modality_text.go — Re-exports from pkg/modality for kernel use.
//
// The text module implementation lives in pkg/modality. This file
// provides package-level aliases so existing kernel code compiles unchanged.

package main

import "github.com/cogos-dev/cogos/pkg/modality"

// Type alias — text module.
type TextModule = modality.TextModule

// NewTextModule creates a text modality module.
func NewTextModule() *TextModule {
	return modality.NewTextModule()
}

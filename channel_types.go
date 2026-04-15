// channel_types.go — Re-exports from pkg/modality for kernel use.
//
// Channel types live in pkg/modality. This file provides package-level
// aliases so existing kernel code compiles unchanged.

package main

import "github.com/cogos-dev/cogos/pkg/modality"

// Type aliases — channel types.
type ChannelDescriptor = modality.ChannelDescriptor
type ChannelAdapter = modality.ChannelAdapter
type ChannelRegistry = modality.ChannelRegistry

// NewChannelRegistry creates an empty channel registry.
func NewChannelRegistry() *ChannelRegistry {
	return modality.NewChannelRegistry()
}

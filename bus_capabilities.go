// bus_capabilities.go
// Defines the bus block schema for agent capability advertisement.
// Agents post an agent.capabilities block on startup (or when capabilities
// change after reconciler apply) to announce their tools, MCP servers,
// memory sectors, and bus subscriptions to the field.

package main

import "time"

// Block type constant for capability advertisement.
const BlockAgentCapabilities = "agent.capabilities"

// AgentCapabilitiesPayload is posted on the bus when an agent comes online
// or when its capabilities change (e.g., after reconciler apply).
type AgentCapabilitiesPayload struct {
	AgentID          string    `json:"agentId"`
	AgentType        string    `json:"agentType"`                  // "interactive", "declarative", "headless"
	Endpoint         string    `json:"endpoint,omitempty"`         // bus endpoint
	Tools            CapTools  `json:"tools"`
	MCPServers       []string  `json:"mcpServers,omitempty"`
	MemorySectors    []string  `json:"memorySectors,omitempty"`
	BusSubscriptions []string  `json:"busSubscriptions,omitempty"`
	TTL              string    `json:"ttl,omitempty"`              // e.g., "1h"
	AdvertisedAt     time.Time `json:"advertisedAt"`
}

// CapTools mirrors the allow/deny tool policy for bus advertisement.
type CapTools struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

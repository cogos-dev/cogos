// Package capability is the substrate-canonical home for the capability
// envelope vocabulary — the schema types and bus block constant agents use
// to advertise their tools, MCP servers, memory sectors, and bus
// subscriptions to the field.
//
// Per ADR-100 Step 3, the schema types extracted from the root package
// (bus_capabilities.go) live here. The resolver, cache, and advertiser
// implementations remain kernel-resident in the root package because they
// depend on kernel runtime types (busSessionManager, agent CRD loaders).
//
// Per RFC-034 §3.3 (diagnostic rule): "if the behavior can be tested
// without a running process, it belongs in the substrate library." The
// schema types here pass that test trivially — they are pure data
// declarations.
//
// Composes with:
//   - cog://architecture/rfcs/capability-envelope-and-policy-vocabulary
//     (RFC-015) — the policy vocabulary built atop this schema.
//   - cog://architecture/adrs/eigen-as-universal-self-harness — capability
//     advertisement is one of the Eigen module-registration interfaces.
package capability

import "time"

// BlockAgentCapabilities is the bus event type for capability advertisement.
// Agents post this block on startup and after reconciler apply.
const BlockAgentCapabilities = "agent.capabilities"

// Payload is posted on the bus when an agent comes online or when its
// capabilities change (e.g., after reconciler apply).
//
// JSON-serialized for transport on the system capabilities bus.
type Payload struct {
	AgentID          string    `json:"agentId"`
	AgentType        string    `json:"agentType"`                  // "interactive", "declarative", "headless"
	Endpoint         string    `json:"endpoint,omitempty"`         // bus endpoint
	Tools            Tools     `json:"tools"`
	MCPServers       []string  `json:"mcpServers,omitempty"`
	MemorySectors    []string  `json:"memorySectors,omitempty"`
	BusSubscriptions []string  `json:"busSubscriptions,omitempty"`
	TTL              string    `json:"ttl,omitempty"` // e.g., "1h"
	AdvertisedAt     time.Time `json:"advertisedAt"`
}

// Tools mirrors the allow/deny tool policy for bus advertisement.
//
// Semantics (matched by the kernel-side capability cache HasTool logic):
//   - If Deny contains the tool, it is denied (deny always wins).
//   - If Allow is non-empty, the tool must appear in Allow to be allowed.
//   - If Allow is empty, the tool is allowed by default unless denied.
type Tools struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

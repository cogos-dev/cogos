// bus_capabilities.go — Thin re-export shim. Canonical capability envelope
// vocabulary lives in pkg/substrate/capability per ADR-100 Step 3.
//
// Type aliases below let existing kernel call sites (capability_cache.go,
// capability_resolver.go, capability_advertiser.go, headless_agent_test.go,
// bus_tool_router.go) compile unchanged. New code should prefer the
// pkg/substrate/capability import path.

package main

import (
	"github.com/myrgic/cogos/pkg/substrate/capability"
)

// BlockAgentCapabilities is the bus block type for capability advertisement.
// Canonical home: pkg/substrate/capability.BlockAgentCapabilities.
const BlockAgentCapabilities = capability.BlockAgentCapabilities

// AgentCapabilitiesPayload is posted on the bus when an agent comes online
// or when its capabilities change (e.g., after reconciler apply).
// Canonical home: pkg/substrate/capability.Payload.
type AgentCapabilitiesPayload = capability.Payload

// CapTools mirrors the allow/deny tool policy for bus advertisement.
// Canonical home: pkg/substrate/capability.Tools.
type CapTools = capability.Tools

// capability_advertiser.go
// Advertises agent capabilities on the bus at startup and after reconciler apply.
// Reads all agent CRDs, filters for spec.capabilities.advertise == true, and posts
// an agent.capabilities event for each on the system bus (bus_system_capabilities).

package main

import (
	"fmt"
	"log"
	"time"
)

// systemCapBusID is the well-known bus ID for capability advertisements.
// Must match what createChatBus("system_capabilities", ...) generates.
const systemCapBusID = "bus_chat_system_capabilities"

// AdvertiseAgentCapabilities loads all agent CRDs and posts agent.capabilities
// events on the bus for agents with spec.capabilities.advertise == true.
// It is safe to call multiple times (idempotent bus creation, append-only events).
func AdvertiseAgentCapabilities(root string, mgr *busSessionManager) error {
	crds, err := ListAgentCRDs(root)
	if err != nil {
		return fmt.Errorf("advertise capabilities: list CRDs: %w", err)
	}

	if len(crds) == 0 {
		log.Printf("[cap-advert] no agent CRDs found in %s", root)
		return nil
	}

	// Ensure the system capabilities bus exists.
	if _, err := mgr.createChatBus("system_capabilities", "kernel:cogos"); err != nil {
		return fmt.Errorf("advertise capabilities: create system bus: %w", err)
	}

	advertised := 0
	for _, crd := range crds {
		if !crd.Spec.Capabilities.Advertise {
			continue
		}

		payload := BuildCapabilitiesPayload(crd)
		payloadMap := capabilitiesPayloadToMap(payload)

		evt, err := mgr.appendBusEvent(
			systemCapBusID,
			BlockAgentCapabilities,
			"kernel:cogos",
			payloadMap,
		)
		if err != nil {
			log.Printf("[cap-advert] failed to advertise agent %q: %v", crd.Metadata.Name, err)
			continue
		}

		log.Printf("[cap-advert] advertised agent=%s type=%s seq=%d tools_allow=%d tools_deny=%d mcp=%d",
			payload.AgentID, payload.AgentType, evt.Seq,
			len(payload.Tools.Allow), len(payload.Tools.Deny),
			len(payload.MCPServers))
		advertised++
	}

	log.Printf("[cap-advert] %d/%d agents advertised on %s", advertised, len(crds), systemCapBusID)
	return nil
}

// BuildCapabilitiesPayload converts an AgentCRD to an AgentCapabilitiesPayload.
func BuildCapabilitiesPayload(crd AgentCRD) AgentCapabilitiesPayload {
	// Collect MCP server names.
	var mcpNames []string
	for _, mcp := range crd.Spec.Capabilities.MCPServers {
		mcpNames = append(mcpNames, mcp.Name)
	}

	// Collect memory sectors (scope list, or single sector).
	var sectors []string
	if len(crd.Spec.Context.Memory.Scope) > 0 {
		sectors = crd.Spec.Context.Memory.Scope
	} else if crd.Spec.Context.Memory.Sector != "" {
		sectors = []string{crd.Spec.Context.Memory.Sector}
	}

	return AgentCapabilitiesPayload{
		AgentID:   crdToOpenClawID(crd.Metadata.Name),
		AgentType: crd.Spec.Type,
		Endpoint:  crd.Spec.Bus.Endpoint,
		Tools: CapTools{
			Allow: crd.Spec.Capabilities.Tools.Allow,
			Deny:  crd.Spec.Capabilities.Tools.Deny,
		},
		MCPServers:       mcpNames,
		MemorySectors:    sectors,
		BusSubscriptions: crd.Spec.Bus.Subscribe,
		AdvertisedAt:     time.Now().UTC(),
	}
}

// capabilitiesPayloadToMap converts an AgentCapabilitiesPayload to a
// map[string]interface{} suitable for appendBusEvent.
func capabilitiesPayloadToMap(p AgentCapabilitiesPayload) map[string]interface{} {
	m := map[string]interface{}{
		"agentId":      p.AgentID,
		"agentType":    p.AgentType,
		"advertisedAt": p.AdvertisedAt.Format(time.RFC3339Nano),
	}
	if p.Endpoint != "" {
		m["endpoint"] = p.Endpoint
	}

	// Tools — always include so consumers see an explicit empty list.
	tools := map[string]interface{}{}
	if len(p.Tools.Allow) > 0 {
		tools["allow"] = p.Tools.Allow
	}
	if len(p.Tools.Deny) > 0 {
		tools["deny"] = p.Tools.Deny
	}
	m["tools"] = tools

	if len(p.MCPServers) > 0 {
		m["mcpServers"] = p.MCPServers
	}
	if len(p.MemorySectors) > 0 {
		m["memorySectors"] = p.MemorySectors
	}
	if len(p.BusSubscriptions) > 0 {
		m["busSubscriptions"] = p.BusSubscriptions
	}
	if p.TTL != "" {
		m["ttl"] = p.TTL
	}
	return m
}

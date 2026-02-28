// capability_test.go
// Validation tests for the capability advertisement + peer discovery pipeline.
// Covers: BuildCapabilitiesPayload, CapabilityCache (Set/Get/TTL/HasTool/List),
// and CapabilityResolver (ResolveAgent/CanInvokeTool/ListAvailableAgents).

package main

import (
	"testing"
	"time"
)

// ─── Helpers ────────────────────────────────────────────────────────────────────

// testAgentCRD returns a fully-populated AgentCRD suitable for testing
// BuildCapabilitiesPayload. Fields map to the most common CRD shape.
func testAgentCRD() AgentCRD {
	return AgentCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "Agent",
		Metadata: AgentCRDMeta{
			Name:      "sandy",
			Namespace: "lab",
		},
		Spec: AgentCRDSpec{
			Type: "interactive",
			Capabilities: AgentCRDCapabilities{
				Tools: AgentCRDToolPolicy{
					Allow: []string{"Read", "Write", "Grep", "Glob"},
					Deny:  []string{"Bash"},
				},
				MCPServers: []AgentCRDMCP{
					{Name: "memory-server", URL: "http://localhost:3100"},
					{Name: "search-server", URL: "http://localhost:3200"},
				},
				Advertise: true,
			},
			Context: AgentCRDContext{
				Memory: AgentCRDMemory{
					Scope: []string{"semantic/insights", "episodic/sessions"},
				},
			},
			Bus: AgentCRDBus{
				Endpoint:  "bus_agent_sandy",
				Subscribe: []string{"system.health", "agent.capabilities"},
			},
		},
	}
}

// testPayload returns a pre-built AgentCapabilitiesPayload for cache tests.
func testPayload(agentID string) AgentCapabilitiesPayload {
	return AgentCapabilitiesPayload{
		AgentID:   agentID,
		AgentType: "interactive",
		Endpoint:  "bus_agent_" + agentID,
		Tools: CapTools{
			Allow: []string{"Read", "Write"},
			Deny:  []string{"Bash"},
		},
		MCPServers:       []string{"memory-server"},
		MemorySectors:    []string{"semantic/insights"},
		BusSubscriptions: []string{"system.health"},
		AdvertisedAt:     time.Now().UTC(),
	}
}

// ─── BuildCapabilitiesPayload ───────────────────────────────────────────────────

func TestBuildCapabilitiesPayload(t *testing.T) {
	crd := testAgentCRD()
	payload := BuildCapabilitiesPayload(crd)

	if payload.AgentID != "sandy" {
		t.Errorf("AgentID = %q, want %q", payload.AgentID, "sandy")
	}
	if payload.AgentType != "interactive" {
		t.Errorf("AgentType = %q, want %q", payload.AgentType, "interactive")
	}
	if payload.Endpoint != "bus_agent_sandy" {
		t.Errorf("Endpoint = %q, want %q", payload.Endpoint, "bus_agent_sandy")
	}

	// Tools
	if len(payload.Tools.Allow) != 4 {
		t.Errorf("Tools.Allow length = %d, want 4", len(payload.Tools.Allow))
	}
	if len(payload.Tools.Deny) != 1 || payload.Tools.Deny[0] != "Bash" {
		t.Errorf("Tools.Deny = %v, want [Bash]", payload.Tools.Deny)
	}

	// MCP servers — only names, not URLs
	if len(payload.MCPServers) != 2 {
		t.Errorf("MCPServers length = %d, want 2", len(payload.MCPServers))
	} else {
		if payload.MCPServers[0] != "memory-server" {
			t.Errorf("MCPServers[0] = %q, want %q", payload.MCPServers[0], "memory-server")
		}
		if payload.MCPServers[1] != "search-server" {
			t.Errorf("MCPServers[1] = %q, want %q", payload.MCPServers[1], "search-server")
		}
	}

	// Memory sectors from scope
	if len(payload.MemorySectors) != 2 {
		t.Errorf("MemorySectors length = %d, want 2", len(payload.MemorySectors))
	}

	// Bus subscriptions
	if len(payload.BusSubscriptions) != 2 {
		t.Errorf("BusSubscriptions length = %d, want 2", len(payload.BusSubscriptions))
	}

	// AdvertisedAt should be recent (within last second)
	if time.Since(payload.AdvertisedAt) > time.Second {
		t.Errorf("AdvertisedAt too old: %v", payload.AdvertisedAt)
	}
}

func TestBuildCapabilitiesPayload_FallbackSingleSector(t *testing.T) {
	crd := testAgentCRD()
	// Clear scope, set single sector
	crd.Spec.Context.Memory.Scope = nil
	crd.Spec.Context.Memory.Sector = "semantic"

	payload := BuildCapabilitiesPayload(crd)

	if len(payload.MemorySectors) != 1 || payload.MemorySectors[0] != "semantic" {
		t.Errorf("MemorySectors = %v, want [semantic]", payload.MemorySectors)
	}
}

func TestBuildCapabilitiesPayload_NoMemory(t *testing.T) {
	crd := testAgentCRD()
	crd.Spec.Context.Memory.Scope = nil
	crd.Spec.Context.Memory.Sector = ""

	payload := BuildCapabilitiesPayload(crd)

	if len(payload.MemorySectors) != 0 {
		t.Errorf("MemorySectors = %v, want empty", payload.MemorySectors)
	}
}

func TestBuildCapabilitiesPayload_NoMCPServers(t *testing.T) {
	crd := testAgentCRD()
	crd.Spec.Capabilities.MCPServers = nil

	payload := BuildCapabilitiesPayload(crd)

	if len(payload.MCPServers) != 0 {
		t.Errorf("MCPServers = %v, want empty", payload.MCPServers)
	}
}

// ─── CapabilityCache: Set / Get ─────────────────────────────────────────────────

func TestCapabilityCacheSetGet(t *testing.T) {
	cache := NewCapabilityCache()
	payload := testPayload("sandy")

	cache.Set("sandy", payload, 0) // default TTL

	got := cache.Get("sandy")
	if got == nil {
		t.Fatal("Get returned nil, want cached payload")
	}
	if got.AgentID != "sandy" {
		t.Errorf("AgentID = %q, want %q", got.AgentID, "sandy")
	}
	if got.AgentType != "interactive" {
		t.Errorf("AgentType = %q, want %q", got.AgentType, "interactive")
	}
	if got.Endpoint != "bus_agent_sandy" {
		t.Errorf("Endpoint = %q, want %q", got.Endpoint, "bus_agent_sandy")
	}
	if len(got.Tools.Allow) != 2 {
		t.Errorf("Tools.Allow length = %d, want 2", len(got.Tools.Allow))
	}
}

func TestCapabilityCacheGetUnknown(t *testing.T) {
	cache := NewCapabilityCache()

	got := cache.Get("nonexistent")
	if got != nil {
		t.Errorf("Get returned %+v for unknown agent, want nil", got)
	}
}

func TestCapabilityCacheGetReturnsCopy(t *testing.T) {
	cache := NewCapabilityCache()
	payload := testPayload("sandy")
	cache.Set("sandy", payload, 0)

	// Mutate the returned copy
	got := cache.Get("sandy")
	got.AgentID = "mutated"

	// Original should be unchanged
	again := cache.Get("sandy")
	if again.AgentID != "sandy" {
		t.Errorf("cache was mutated via returned pointer: AgentID = %q", again.AgentID)
	}
}

// ─── CapabilityCache: TTL Expiry ────────────────────────────────────────────────

func TestCapabilityCacheTTLExpiry(t *testing.T) {
	cache := NewCapabilityCache()
	payload := testPayload("ephemeral")

	cache.Set("ephemeral", payload, 1*time.Millisecond)

	// Immediately after set, it should be there.
	got := cache.Get("ephemeral")
	if got == nil {
		t.Fatal("Get returned nil immediately after Set")
	}

	// Wait for expiry.
	time.Sleep(10 * time.Millisecond)

	got = cache.Get("ephemeral")
	if got != nil {
		t.Errorf("expected nil after TTL expiry, got %+v", got)
	}
}

func TestCapabilityCacheExpireSweep(t *testing.T) {
	cache := NewCapabilityCache()
	cache.Set("agent-a", testPayload("agent-a"), 1*time.Millisecond)
	cache.Set("agent-b", testPayload("agent-b"), 1*time.Hour)

	time.Sleep(10 * time.Millisecond)

	removed := cache.ExpireSweep()
	if removed != 1 {
		t.Errorf("ExpireSweep removed %d, want 1", removed)
	}

	// agent-b should still be present
	if cache.Get("agent-b") == nil {
		t.Error("agent-b should not have been swept")
	}
}

// ─── CapabilityCache: HasTool ───────────────────────────────────────────────────

func TestCapabilityCacheHasTool(t *testing.T) {
	cache := NewCapabilityCache()
	payload := testPayload("sandy") // allow: [Read, Write], deny: [Bash]
	cache.Set("sandy", payload, 0)

	tests := []struct {
		tool string
		want bool
		desc string
	}{
		{"Read", true, "explicitly allowed"},
		{"Write", true, "explicitly allowed"},
		{"Bash", false, "explicitly denied"},
		{"Grep", false, "not in allow list (allow is non-empty)"},
	}

	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			got := cache.HasTool("sandy", tt.tool)
			if got != tt.want {
				t.Errorf("HasTool(%q) = %v, want %v (%s)", tt.tool, got, tt.want, tt.desc)
			}
		})
	}
}

func TestCapabilityCacheHasTool_EmptyAllowList(t *testing.T) {
	cache := NewCapabilityCache()

	// Empty allow + deny only Bash — everything except Bash is allowed.
	payload := AgentCapabilitiesPayload{
		AgentID:   "permissive",
		AgentType: "headless",
		Tools: CapTools{
			Allow: nil,
			Deny:  []string{"Bash"},
		},
		AdvertisedAt: time.Now().UTC(),
	}
	cache.Set("permissive", payload, 0)

	if !cache.HasTool("permissive", "Read") {
		t.Error("HasTool(Read) = false, want true (empty allow = all allowed)")
	}
	if !cache.HasTool("permissive", "Grep") {
		t.Error("HasTool(Grep) = false, want true (empty allow = all allowed)")
	}
	if cache.HasTool("permissive", "Bash") {
		t.Error("HasTool(Bash) = true, want false (explicitly denied)")
	}
}

func TestCapabilityCacheHasTool_UnknownAgent(t *testing.T) {
	cache := NewCapabilityCache()

	if cache.HasTool("ghost", "Read") {
		t.Error("HasTool for unknown agent should return false")
	}
}

func TestCapabilityCacheHasTool_ExpiredAgent(t *testing.T) {
	cache := NewCapabilityCache()
	cache.Set("expired", testPayload("expired"), 1*time.Millisecond)
	time.Sleep(10 * time.Millisecond)

	if cache.HasTool("expired", "Read") {
		t.Error("HasTool for expired agent should return false")
	}
}

// ─── CapabilityCache: List ──────────────────────────────────────────────────────

func TestCapabilityCacheList(t *testing.T) {
	cache := NewCapabilityCache()
	cache.Set("agent-a", testPayload("agent-a"), 1*time.Hour)
	cache.Set("agent-b", testPayload("agent-b"), 1*time.Hour)
	cache.Set("agent-c", testPayload("agent-c"), 1*time.Millisecond)

	time.Sleep(10 * time.Millisecond)

	list := cache.List()
	if len(list) != 2 {
		t.Errorf("List returned %d entries, want 2 (agent-c should be expired)", len(list))
	}
	if _, ok := list["agent-a"]; !ok {
		t.Error("agent-a missing from List")
	}
	if _, ok := list["agent-b"]; !ok {
		t.Error("agent-b missing from List")
	}
	if _, ok := list["agent-c"]; ok {
		t.Error("agent-c should be expired and not in List")
	}
}

func TestCapabilityCacheListEmpty(t *testing.T) {
	cache := NewCapabilityCache()
	list := cache.List()
	if len(list) != 0 {
		t.Errorf("List on empty cache returned %d entries, want 0", len(list))
	}
}

// ─── CapabilityCache: Delete ────────────────────────────────────────────────────

func TestCapabilityCacheDelete(t *testing.T) {
	cache := NewCapabilityCache()
	cache.Set("sandy", testPayload("sandy"), 0)

	cache.Delete("sandy")

	if cache.Get("sandy") != nil {
		t.Error("agent should be nil after Delete")
	}
}

func TestCapabilityCacheDeleteNonexistent(t *testing.T) {
	cache := NewCapabilityCache()
	// Should not panic.
	cache.Delete("ghost")
}

// ─── CapabilityResolver: ResolveAgent ───────────────────────────────────────────

func TestCapabilityResolverResolveAgent(t *testing.T) {
	cache := NewCapabilityCache()
	resolver := NewCapabilityResolver(cache)

	cache.Set("sandy", testPayload("sandy"), 0)

	caps, ok := resolver.ResolveAgent("sandy")
	if !ok {
		t.Fatal("ResolveAgent returned false for cached agent")
	}
	if caps.AgentID != "sandy" {
		t.Errorf("AgentID = %q, want %q", caps.AgentID, "sandy")
	}
	if caps.Endpoint != "bus_agent_sandy" {
		t.Errorf("Endpoint = %q, want %q", caps.Endpoint, "bus_agent_sandy")
	}
}

func TestCapabilityResolverResolveAgent_Unknown(t *testing.T) {
	cache := NewCapabilityCache()
	resolver := NewCapabilityResolver(cache)

	caps, ok := resolver.ResolveAgent("nonexistent")
	if ok {
		t.Error("ResolveAgent returned true for unknown agent")
	}
	if caps != nil {
		t.Errorf("caps should be nil for unknown agent, got %+v", caps)
	}
}

func TestCapabilityResolverResolveAgent_Expired(t *testing.T) {
	cache := NewCapabilityCache()
	resolver := NewCapabilityResolver(cache)

	cache.Set("ephemeral", testPayload("ephemeral"), 1*time.Millisecond)
	time.Sleep(10 * time.Millisecond)

	caps, ok := resolver.ResolveAgent("ephemeral")
	if ok {
		t.Error("ResolveAgent returned true for expired agent")
	}
	if caps != nil {
		t.Errorf("caps should be nil for expired agent, got %+v", caps)
	}
}

// ─── CapabilityResolver: CanInvokeTool ──────────────────────────────────────────

func TestCapabilityResolverCanInvokeTool(t *testing.T) {
	cache := NewCapabilityCache()
	resolver := NewCapabilityResolver(cache)

	// allow: [Read, Write], deny: [Bash]
	cache.Set("sandy", testPayload("sandy"), 0)

	tests := []struct {
		agentID string
		tool    string
		want    bool
		desc    string
	}{
		{"sandy", "Read", true, "allowed tool"},
		{"sandy", "Write", true, "allowed tool"},
		{"sandy", "Bash", false, "denied tool"},
		{"sandy", "Grep", false, "not in allow list"},
		{"ghost", "Read", false, "unknown agent"},
	}

	for _, tt := range tests {
		t.Run(tt.agentID+"/"+tt.tool, func(t *testing.T) {
			got := resolver.CanInvokeTool(tt.agentID, tt.tool)
			if got != tt.want {
				t.Errorf("CanInvokeTool(%q, %q) = %v, want %v (%s)",
					tt.agentID, tt.tool, got, tt.want, tt.desc)
			}
		})
	}
}

// ─── CapabilityResolver: ListAvailableAgents ────────────────────────────────────

func TestCapabilityResolverListAvailableAgents(t *testing.T) {
	cache := NewCapabilityCache()
	resolver := NewCapabilityResolver(cache)

	cache.Set("sandy", testPayload("sandy"), 1*time.Hour)
	cache.Set("cog", testPayload("cog"), 1*time.Hour)

	agents := resolver.ListAvailableAgents()
	if len(agents) != 2 {
		t.Errorf("ListAvailableAgents returned %d, want 2", len(agents))
	}
	if _, ok := agents["sandy"]; !ok {
		t.Error("sandy missing from available agents")
	}
	if _, ok := agents["cog"]; !ok {
		t.Error("cog missing from available agents")
	}
}

// ─── Full Pipeline: CRD → Payload → Cache → Resolve ────────────────────────────

func TestFullPipeline_CRDToResolve(t *testing.T) {
	// Step 1: Build payload from CRD (advertiser)
	crd := testAgentCRD()
	payload := BuildCapabilitiesPayload(crd)

	// Step 2: Store in cache (what the bus listener would do)
	cache := NewCapabilityCache()
	cache.Set(payload.AgentID, payload, 0)

	// Step 3: Resolve via resolver (what URI dispatch would do)
	resolver := NewCapabilityResolver(cache)

	caps, ok := resolver.ResolveAgent("sandy")
	if !ok {
		t.Fatal("full pipeline: ResolveAgent failed")
	}

	// Verify the full chain preserved all fields
	if caps.AgentID != "sandy" {
		t.Errorf("AgentID = %q, want %q", caps.AgentID, "sandy")
	}
	if caps.AgentType != "interactive" {
		t.Errorf("AgentType = %q, want %q", caps.AgentType, "interactive")
	}
	if caps.Endpoint != "bus_agent_sandy" {
		t.Errorf("Endpoint = %q, want %q", caps.Endpoint, "bus_agent_sandy")
	}

	// Tool resolution through the full chain
	if !resolver.CanInvokeTool("sandy", "Read") {
		t.Error("CanInvokeTool(Read) = false, want true")
	}
	if !resolver.CanInvokeTool("sandy", "Write") {
		t.Error("CanInvokeTool(Write) = false, want true")
	}
	if resolver.CanInvokeTool("sandy", "Bash") {
		t.Error("CanInvokeTool(Bash) = true, want false (denied)")
	}

	// MCP servers survive the pipeline
	if len(caps.MCPServers) != 2 {
		t.Errorf("MCPServers length = %d, want 2", len(caps.MCPServers))
	}

	// Memory sectors survive the pipeline
	if len(caps.MemorySectors) != 2 {
		t.Errorf("MemorySectors length = %d, want 2", len(caps.MemorySectors))
	}

	// Bus subscriptions survive the pipeline
	if len(caps.BusSubscriptions) != 2 {
		t.Errorf("BusSubscriptions length = %d, want 2", len(caps.BusSubscriptions))
	}
}

// ─── capabilitiesPayloadToMap ───────────────────────────────────────────────────

func TestCapabilitiesPayloadToMap(t *testing.T) {
	payload := testPayload("sandy")
	m := capabilitiesPayloadToMap(payload)

	if m["agentId"] != "sandy" {
		t.Errorf("agentId = %v, want sandy", m["agentId"])
	}
	if m["agentType"] != "interactive" {
		t.Errorf("agentType = %v, want interactive", m["agentType"])
	}
	if m["endpoint"] != "bus_agent_sandy" {
		t.Errorf("endpoint = %v, want bus_agent_sandy", m["endpoint"])
	}

	// Tools map should be present
	tools, ok := m["tools"].(map[string]interface{})
	if !ok {
		t.Fatal("tools is not a map")
	}
	if tools["allow"] == nil {
		t.Error("tools.allow should be present")
	}
	if tools["deny"] == nil {
		t.Error("tools.deny should be present")
	}

	// mcpServers should be present
	if m["mcpServers"] == nil {
		t.Error("mcpServers should be present")
	}

	// advertisedAt should parse as RFC3339Nano
	advStr, ok := m["advertisedAt"].(string)
	if !ok {
		t.Fatal("advertisedAt is not a string")
	}
	if _, err := time.Parse(time.RFC3339Nano, advStr); err != nil {
		t.Errorf("advertisedAt is not valid RFC3339Nano: %v", err)
	}
}

func TestCapabilitiesPayloadToMap_EmptyOptionals(t *testing.T) {
	payload := AgentCapabilitiesPayload{
		AgentID:      "minimal",
		AgentType:    "headless",
		AdvertisedAt: time.Now().UTC(),
	}
	m := capabilitiesPayloadToMap(payload)

	// Endpoint should be absent
	if _, ok := m["endpoint"]; ok {
		t.Error("endpoint should be omitted when empty")
	}
	// MCP servers should be absent
	if _, ok := m["mcpServers"]; ok {
		t.Error("mcpServers should be omitted when empty")
	}
	// Bus subscriptions should be absent
	if _, ok := m["busSubscriptions"]; ok {
		t.Error("busSubscriptions should be omitted when empty")
	}
	// Tools map should still exist (always included per implementation)
	if _, ok := m["tools"]; !ok {
		t.Error("tools should always be present")
	}
}

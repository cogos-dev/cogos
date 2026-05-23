package capability_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/capability"
)

// TestPayloadJSONRoundtrip verifies the wire format is stable: serialize +
// deserialize must produce the same Payload (modulo time precision).
func TestPayloadJSONRoundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	p := capability.Payload{
		AgentID:   "sandy",
		AgentType: "interactive",
		Endpoint:  "bus://kernel:sandy",
		Tools: capability.Tools{
			Allow: []string{"read_file", "write_file"},
			Deny:  []string{"shell_exec"},
		},
		MCPServers:       []string{"filesystem", "git"},
		MemorySectors:    []string{"semantic/insights"},
		BusSubscriptions: []string{"bus_chat_main"},
		TTL:              "1h",
		AdvertisedAt:     now,
	}

	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}

	var got capability.Payload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}

	if got.AgentID != p.AgentID {
		t.Errorf("AgentID drift: got %q, want %q", got.AgentID, p.AgentID)
	}
	if got.AgentType != p.AgentType {
		t.Errorf("AgentType drift: got %q, want %q", got.AgentType, p.AgentType)
	}
	if got.Tools.Allow[0] != "read_file" || got.Tools.Deny[0] != "shell_exec" {
		t.Errorf("Tools drift: got %+v, want %+v", got.Tools, p.Tools)
	}
	if !got.AdvertisedAt.Equal(p.AdvertisedAt) {
		t.Errorf("AdvertisedAt drift: got %v, want %v", got.AdvertisedAt, p.AdvertisedAt)
	}
}

// TestBlockTypeStability verifies the bus block type string hasn't drifted.
// External agents on the bus rely on this exact value to dispatch capability
// advertisements; changing it is a wire-protocol break.
func TestBlockTypeStability(t *testing.T) {
	if capability.BlockAgentCapabilities != "agent.capabilities" {
		t.Errorf("BlockAgentCapabilities = %q, want %q (wire-protocol stability)",
			capability.BlockAgentCapabilities, "agent.capabilities")
	}
}

// TestPayloadOmitEmpty verifies optional fields are omitted from JSON when
// empty (Endpoint, MCPServers, MemorySectors, BusSubscriptions, TTL).
// Operators reading bus events should not see noise from empty fields.
func TestPayloadOmitEmpty(t *testing.T) {
	p := capability.Payload{
		AgentID:      "minimal",
		AgentType:    "headless",
		AdvertisedAt: time.Unix(0, 0).UTC(),
		Tools:        capability.Tools{}, // intentionally empty
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, field := range []string{`"endpoint"`, `"mcpServers"`, `"memorySectors"`, `"busSubscriptions"`, `"ttl"`} {
		if contains(s, field) {
			t.Errorf("expected %s to be omitted, got: %s", field, s)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

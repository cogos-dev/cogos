package cogfield

import "testing"

func TestGraphBlockToNode(t *testing.T) {
	block := GraphBlock{
		URI:  "bus:abc:1",
		Type: "bus.message",
		From: "agent-a",
		Ts:   "2026-04-14T12:00:00Z",
		Hash: "hash-1",
		Payload: map[string]interface{}{
			"content": "Hello world",
		},
		Meta: map[string]interface{}{
			"custom": "value",
		},
	}

	node := GraphBlockToNode(block)

	if node.ID != "bus:abc:1" {
		t.Errorf("ID = %q, want bus:abc:1", node.ID)
	}
	if node.Sector != "buses" {
		t.Errorf("Sector = %q, want buses (bus. prefix)", node.Sector)
	}
	if node.Label != "Hello world" {
		t.Errorf("Label = %q, want Hello world", node.Label)
	}
	if node.Meta["hash"] != "hash-1" {
		t.Errorf("Meta[hash] = %v, want hash-1", node.Meta["hash"])
	}
	if node.Meta["custom"] != "value" {
		t.Errorf("Meta[custom] = %v, want value", node.Meta["custom"])
	}
}

func TestGraphBlockToNodeSectorInference(t *testing.T) {
	// Session type -> sessions sector
	node := GraphBlockToNode(GraphBlock{
		URI:     "session:abc:1",
		Type:    "session.turn",
		Payload: map[string]interface{}{},
	})
	if node.Sector != "sessions" {
		t.Errorf("session type: Sector = %q, want sessions", node.Sector)
	}

	// Bus type -> buses sector
	node = GraphBlockToNode(GraphBlock{
		URI:     "bus:abc:1",
		Type:    "bus.open",
		Payload: map[string]interface{}{},
	})
	if node.Sector != "buses" {
		t.Errorf("bus type: Sector = %q, want buses", node.Sector)
	}
}

func TestGraphBlockToNodeLabelTruncation(t *testing.T) {
	longContent := ""
	for i := 0; i < 100; i++ {
		longContent += "x"
	}

	node := GraphBlockToNode(GraphBlock{
		URI:     "test:1",
		Type:    "test",
		Payload: map[string]interface{}{"content": longContent},
	})

	if len(node.Label) > 64 { // 60 + "..."
		t.Errorf("Label too long: %d chars", len(node.Label))
	}
}

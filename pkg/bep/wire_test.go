package bep

import (
	"net"
	"testing"
)

// ─── Hello exchange ─────────────────────────────────────────────────────────────

func TestWireHelloRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	wireA := NewWire(a)
	wireB := NewWire(b)

	orig := &Hello{
		DeviceName:    "node-a",
		ClientName:    "cogos",
		ClientVersion: "2.1.1",
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- wireA.WriteHello(orig)
	}()

	got, err := wireB.ReadHello()
	if err != nil {
		t.Fatalf("ReadHello: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("WriteHello: %v", err)
	}

	if got.DeviceName != orig.DeviceName {
		t.Errorf("DeviceName = %q, want %q", got.DeviceName, orig.DeviceName)
	}
	if got.ClientName != orig.ClientName {
		t.Errorf("ClientName = %q, want %q", got.ClientName, orig.ClientName)
	}
}

// ─── Message exchange ───────────────────────────────────────────────────────────

func TestWireMessageRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	wireA := NewWire(a)
	wireB := NewWire(b)

	idx := &Index{
		Folder: "test-folder",
		Files: []*FileInfo{
			{Name: "test.agent.yaml", Size: 42},
		},
	}
	payload := idx.Marshal()

	errCh := make(chan error, 1)
	go func() {
		errCh <- wireA.WriteMessage(MessageTypeIndex, payload)
	}()

	msgType, gotPayload, err := wireB.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	if msgType != MessageTypeIndex {
		t.Errorf("msgType = %d, want %d", msgType, MessageTypeIndex)
	}

	gotIdx := &Index{}
	if err := gotIdx.Unmarshal(gotPayload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if gotIdx.Folder != "test-folder" {
		t.Errorf("Folder = %q, want %q", gotIdx.Folder, "test-folder")
	}
	if len(gotIdx.Files) != 1 {
		t.Fatalf("Files count = %d, want 1", len(gotIdx.Files))
	}
}

// ─── Ping message ───────────────────────────────────────────────────────────────

func TestWirePingRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	wireA := NewWire(a)
	wireB := NewWire(b)

	ping := &Ping{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- wireA.WriteMessage(MessageTypePing, ping.Marshal())
	}()

	msgType, _, err := wireB.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if msgType != MessageTypePing {
		t.Errorf("msgType = %d, want %d", msgType, MessageTypePing)
	}
}

// ─── Close message ──────────────────────────────────────────────────────────────

func TestWireCloseRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	wireA := NewWire(a)
	wireB := NewWire(b)

	cl := &Close{Reason: "test shutdown"}
	errCh := make(chan error, 1)
	go func() {
		errCh <- wireA.WriteMessage(MessageTypeClose, cl.Marshal())
	}()

	msgType, payload, err := wireB.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if msgType != MessageTypeClose {
		t.Errorf("msgType = %d, want %d", msgType, MessageTypeClose)
	}

	gotClose := &Close{}
	if err := gotClose.Unmarshal(payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if gotClose.Reason != "test shutdown" {
		t.Errorf("Reason = %q, want %q", gotClose.Reason, "test shutdown")
	}
}

package main

import (
	"net"
	"testing"
)

// ─── Hello exchange ─────────────────────────────────────────────────────────────

func TestBEPWireHelloRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	wireA := NewBEPWire(a)
	wireB := NewBEPWire(b)

	orig := &BEPHello{
		DeviceName:    "node-a",
		ClientName:    "cogos",
		ClientVersion: "2.1.1",
	}

	// Write hello from A, read on B.
	errCh := make(chan error, 1)
	go func() {
		errCh <- wireA.WriteHello(orig)
	}()

	got, err := wireB.ReadHello()
	if err != nil {
		t.Fatalf("ReadHello: %v", err)
	}
	if writeErr := <-errCh; writeErr != nil {
		t.Fatalf("WriteHello: %v", writeErr)
	}

	if got.DeviceName != orig.DeviceName {
		t.Errorf("DeviceName = %q, want %q", got.DeviceName, orig.DeviceName)
	}
	if got.ClientName != orig.ClientName {
		t.Errorf("ClientName = %q, want %q", got.ClientName, orig.ClientName)
	}
	if got.ClientVersion != orig.ClientVersion {
		t.Errorf("ClientVersion = %q, want %q", got.ClientVersion, orig.ClientVersion)
	}
}

func TestBEPWireHelloBadMagic(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	wireB := NewBEPWire(b)

	// Write garbage magic.
	go func() {
		a.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x00})
	}()

	_, err := wireB.ReadHello()
	if err == nil {
		t.Error("expected error for bad magic")
	}
}

// ─── Message exchange ───────────────────────────────────────────────────────────

func TestBEPWireMessageRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	wireA := NewBEPWire(a)
	wireB := NewBEPWire(b)

	idx := &BEPIndex{
		Folder: "cogos-agent-defs",
		Files: []*BEPFileInfo{
			{Name: "test.agent.yaml", Size: 512, Sequence: 1},
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
	if writeErr := <-errCh; writeErr != nil {
		t.Fatalf("WriteMessage: %v", writeErr)
	}

	if msgType != MessageTypeIndex {
		t.Errorf("type = %d, want %d", msgType, MessageTypeIndex)
	}

	gotIdx := &BEPIndex{}
	if err := gotIdx.Unmarshal(gotPayload); err != nil {
		t.Fatalf("Unmarshal Index: %v", err)
	}
	if gotIdx.Folder != idx.Folder {
		t.Errorf("Folder = %q", gotIdx.Folder)
	}
	if len(gotIdx.Files) != 1 || gotIdx.Files[0].Name != "test.agent.yaml" {
		t.Errorf("unexpected files: %v", gotIdx.Files)
	}
}

func TestBEPWirePingMessage(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	wireA := NewBEPWire(a)
	wireB := NewBEPWire(b)

	ping := &BEPPing{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- wireA.WriteMessage(MessageTypePing, ping.Marshal())
	}()

	msgType, payload, err := wireB.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if <-errCh != nil {
		t.Fatal("WriteMessage failed")
	}

	if msgType != MessageTypePing {
		t.Errorf("type = %d, want Ping (%d)", msgType, MessageTypePing)
	}
	if len(payload) != 0 {
		t.Errorf("Ping payload should be empty, got %d bytes", len(payload))
	}
}

func TestBEPWireMultipleMessages(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	wireA := NewBEPWire(a)
	wireB := NewBEPWire(b)

	messages := []struct {
		typ     MessageType
		payload []byte
	}{
		{MessageTypeClusterConfig, (&BEPClusterConfig{}).Marshal()},
		{MessageTypeIndex, (&BEPIndex{Folder: "test"}).Marshal()},
		{MessageTypePing, (&BEPPing{}).Marshal()},
		{MessageTypeClose, (&BEPClose{Reason: "done"}).Marshal()},
	}

	go func() {
		for _, m := range messages {
			if err := wireA.WriteMessage(m.typ, m.payload); err != nil {
				return
			}
		}
	}()

	for i, expected := range messages {
		msgType, _, err := wireB.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage[%d]: %v", i, err)
		}
		if msgType != expected.typ {
			t.Errorf("message[%d] type = %d, want %d", i, msgType, expected.typ)
		}
	}
}

// ─── Connection close handling ──────────────────────────────────────────────────

func TestBEPWireReadAfterClose(t *testing.T) {
	a, b := net.Pipe()
	wireB := NewBEPWire(b)

	a.Close()

	_, _, err := wireB.ReadMessage()
	if err == nil {
		t.Error("expected error reading from closed connection")
	}
}

func TestBEPWireHelloReadAfterClose(t *testing.T) {
	a, b := net.Pipe()
	wireB := NewBEPWire(b)

	a.Close()

	_, err := wireB.ReadHello()
	if err == nil {
		t.Error("expected error reading hello from closed connection")
	}
}

// ─── Full protocol sequence ─────────────────────────────────────────────────────

func TestBEPWireFullProtocolSequence(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	wireA := NewBEPWire(a)
	wireB := NewBEPWire(b)

	done := make(chan error, 1)

	// Peer A runs in goroutine: Write → Read → Write → Read → Write → Read.
	go func() {
		// 1. Send Hello.
		if err := wireA.WriteHello(&BEPHello{
			DeviceName: "A", ClientName: "cogos", ClientVersion: "2.1.1",
		}); err != nil {
			done <- err
			return
		}
		// 2. Read Hello from B.
		if _, err := wireA.ReadHello(); err != nil {
			done <- err
			return
		}
		// 3. Send ClusterConfig.
		cc := &BEPClusterConfig{Folders: []*BEPFolder{{ID: "test"}}}
		if err := wireA.WriteMessage(MessageTypeClusterConfig, cc.Marshal()); err != nil {
			done <- err
			return
		}
		// 4. Read ClusterConfig from B.
		if _, _, err := wireA.ReadMessage(); err != nil {
			done <- err
			return
		}
		// 5. Send Index.
		idx := &BEPIndex{Folder: "test", Files: []*BEPFileInfo{{Name: "a.agent.yaml", Size: 100}}}
		if err := wireA.WriteMessage(MessageTypeIndex, idx.Marshal()); err != nil {
			done <- err
			return
		}
		// 6. Read Index from B.
		if _, _, err := wireA.ReadMessage(); err != nil {
			done <- err
			return
		}
		done <- nil
	}()

	// Peer B mirrors: Read → Write → Read → Write → Read → Write.
	// (Alternates with A to avoid net.Pipe deadlock.)

	// 1. Read Hello from A.
	helloA, err := wireB.ReadHello()
	if err != nil {
		t.Fatalf("B ReadHello: %v", err)
	}
	if helloA.DeviceName != "A" {
		t.Errorf("hello device = %q, want A", helloA.DeviceName)
	}
	// 2. Send Hello to A.
	if err := wireB.WriteHello(&BEPHello{DeviceName: "B", ClientName: "cogos", ClientVersion: "2.1.1"}); err != nil {
		t.Fatalf("B WriteHello: %v", err)
	}
	// 3. Read ClusterConfig from A.
	_, ccPayload, err := wireB.ReadMessage()
	if err != nil {
		t.Fatalf("B ReadCC: %v", err)
	}
	gotCC := &BEPClusterConfig{}
	if err := gotCC.Unmarshal(ccPayload); err != nil {
		t.Fatalf("B UnmarshalCC: %v", err)
	}
	if len(gotCC.Folders) != 1 || gotCC.Folders[0].ID != "test" {
		t.Errorf("unexpected CC from A: %+v", gotCC)
	}
	// 4. Send ClusterConfig to A.
	cc := &BEPClusterConfig{Folders: []*BEPFolder{{ID: "test-b"}}}
	if err := wireB.WriteMessage(MessageTypeClusterConfig, cc.Marshal()); err != nil {
		t.Fatalf("B WriteCC: %v", err)
	}
	// 5. Read Index from A.
	_, idxPayload, err := wireB.ReadMessage()
	if err != nil {
		t.Fatalf("B ReadIndex: %v", err)
	}
	gotIdx := &BEPIndex{}
	if err := gotIdx.Unmarshal(idxPayload); err != nil {
		t.Fatalf("B UnmarshalIdx: %v", err)
	}
	if len(gotIdx.Files) != 1 || gotIdx.Files[0].Name != "a.agent.yaml" {
		t.Errorf("unexpected index from A: %+v", gotIdx)
	}
	// 6. Send Index to A.
	idx := &BEPIndex{Folder: "test-b"}
	if err := wireB.WriteMessage(MessageTypeIndex, idx.Marshal()); err != nil {
		t.Fatalf("B WriteIndex: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("peer A error: %v", err)
	}
}

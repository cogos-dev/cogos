package bep

import (
	"testing"
)

// ─── Hello round-trip ───────────────────────────────────────────────────────────

func TestHelloMarshalUnmarshal(t *testing.T) {
	orig := &Hello{
		DeviceName:    "test-node",
		ClientName:    "cogos",
		ClientVersion: "2.1.1",
	}

	data := orig.Marshal()
	if len(data) == 0 {
		t.Fatal("Marshal returned empty bytes")
	}

	got := &Hello{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
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

// ─── Header round-trip ──────────────────────────────────────────────────────────

func TestHeaderMarshalUnmarshal(t *testing.T) {
	orig := &Header{Type: MessageTypeIndex, Compression: CompressionNone}
	data := orig.Marshal()

	got := &Header{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Type != orig.Type {
		t.Errorf("Type = %d, want %d", got.Type, orig.Type)
	}
}

// ─── ClusterConfig round-trip ───────────────────────────────────────────────────

func TestClusterConfigMarshalUnmarshal(t *testing.T) {
	orig := &ClusterConfig{
		Folders: []*Folder{{
			ID:    "cogos-agent-defs",
			Label: "Agent Definitions",
			Devices: []*Device{
				{ID: make([]byte, 32), Name: "node-a"},
				{ID: make([]byte, 32), Name: "node-b"},
			},
		}},
	}

	data := orig.Marshal()
	got := &ClusterConfig{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(got.Folders) != 1 {
		t.Fatalf("Folders count = %d, want 1", len(got.Folders))
	}
	if got.Folders[0].ID != "cogos-agent-defs" {
		t.Errorf("Folder ID = %q, want %q", got.Folders[0].ID, "cogos-agent-defs")
	}
	if len(got.Folders[0].Devices) != 2 {
		t.Errorf("Devices count = %d, want 2", len(got.Folders[0].Devices))
	}
}

// ─── FileInfo round-trip ────────────────────────────────────────────────────────

func TestFileInfoMarshalUnmarshal(t *testing.T) {
	orig := &FileInfo{
		Name:       "test.agent.yaml",
		Size:       1234,
		ModifiedS:  1700000000,
		ModifiedNs: 500000000,
		ModifiedBy: 42,
		Deleted:    false,
		Sequence:   7,
		Version: Vector{
			Counters: []*Counter{{ID: 42, Value: 7}},
		},
		BlocksHash: []byte("abcdef0123456789abcdef0123456789"),
		Blocks: []*BlockInfo{{
			Offset: 0,
			Size:   1234,
			Hash:   []byte("blockhash"),
		}},
	}

	data := orig.Marshal()
	got := &FileInfo{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Name != orig.Name {
		t.Errorf("Name = %q, want %q", got.Name, orig.Name)
	}
	if got.Size != orig.Size {
		t.Errorf("Size = %d, want %d", got.Size, orig.Size)
	}
	if got.ModifiedS != orig.ModifiedS {
		t.Errorf("ModifiedS = %d, want %d", got.ModifiedS, orig.ModifiedS)
	}
	if got.Sequence != orig.Sequence {
		t.Errorf("Sequence = %d, want %d", got.Sequence, orig.Sequence)
	}
	if len(got.Version.Counters) != 1 {
		t.Fatalf("Version.Counters count = %d, want 1", len(got.Version.Counters))
	}
	if got.Version.Counters[0].Value != 7 {
		t.Errorf("Counter value = %d, want 7", got.Version.Counters[0].Value)
	}
	if len(got.Blocks) != 1 {
		t.Fatalf("Blocks count = %d, want 1", len(got.Blocks))
	}
	if got.Blocks[0].Size != 1234 {
		t.Errorf("Block size = %d, want 1234", got.Blocks[0].Size)
	}
}

func TestFileInfoDeletedRoundTrip(t *testing.T) {
	orig := &FileInfo{
		Name:    "deleted.agent.yaml",
		Deleted: true,
	}
	data := orig.Marshal()
	got := &FileInfo{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.Deleted {
		t.Error("Deleted should be true")
	}
}

// ─── Index round-trip ───────────────────────────────────────────────────────────

func TestIndexMarshalUnmarshal(t *testing.T) {
	orig := &Index{
		Folder: "cogos-agent-defs",
		Files: []*FileInfo{
			{Name: "a.agent.yaml", Size: 100},
			{Name: "b.agent.yaml", Size: 200},
		},
	}

	data := orig.Marshal()
	got := &Index{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Folder != orig.Folder {
		t.Errorf("Folder = %q, want %q", got.Folder, orig.Folder)
	}
	if len(got.Files) != 2 {
		t.Fatalf("Files count = %d, want 2", len(got.Files))
	}
}

// ─── Request/Response round-trip ────────────────────────────────────────────────

func TestRequestMarshalUnmarshal(t *testing.T) {
	orig := &Request{
		ID:     42,
		Folder: "cogos-agent-defs",
		Name:   "test.agent.yaml",
		Offset: 0,
		Size:   1024,
	}

	data := orig.Marshal()
	got := &Request{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.ID != orig.ID {
		t.Errorf("ID = %d, want %d", got.ID, orig.ID)
	}
	if got.Name != orig.Name {
		t.Errorf("Name = %q, want %q", got.Name, orig.Name)
	}
}

func TestResponseMarshalUnmarshal(t *testing.T) {
	orig := &Response{
		ID:   42,
		Data: []byte("file content here"),
		Code: ErrorCodeNoError,
	}

	data := orig.Marshal()
	got := &Response{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.ID != orig.ID {
		t.Errorf("ID = %d, want %d", got.ID, orig.ID)
	}
	if string(got.Data) != string(orig.Data) {
		t.Errorf("Data = %q, want %q", got.Data, orig.Data)
	}
	if got.Code != orig.Code {
		t.Errorf("Code = %d, want %d", got.Code, orig.Code)
	}
}

func TestResponseErrorCode(t *testing.T) {
	orig := &Response{ID: 1, Code: ErrorCodeNoSuchFile}
	data := orig.Marshal()
	got := &Response{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Code != ErrorCodeNoSuchFile {
		t.Errorf("Code = %d, want %d", got.Code, ErrorCodeNoSuchFile)
	}
}

// ─── Ping / Close ───────────────────────────────────────────────────────────────

func TestPingMarshalUnmarshal(t *testing.T) {
	p := &Ping{}
	data := p.Marshal()
	if data != nil {
		t.Errorf("Ping.Marshal should return nil, got %v", data)
	}
	if err := p.Unmarshal(nil); err != nil {
		t.Errorf("Ping.Unmarshal: %v", err)
	}
}

func TestCloseMarshalUnmarshal(t *testing.T) {
	orig := &Close{Reason: "shutdown"}
	data := orig.Marshal()
	got := &Close{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Reason != orig.Reason {
		t.Errorf("Reason = %q, want %q", got.Reason, orig.Reason)
	}
}

// ─── Message type constants ─────────────────────────────────────────────────────

func TestMessageTypeConstants(t *testing.T) {
	// Verify constants match the BEP spec values.
	cases := []struct {
		name string
		got  MessageType
		want int32
	}{
		{"ClusterConfig", MessageTypeClusterConfig, 0},
		{"Index", MessageTypeIndex, 1},
		{"IndexUpdate", MessageTypeIndexUpdate, 2},
		{"Request", MessageTypeRequest, 3},
		{"Response", MessageTypeResponse, 4},
		{"Ping", MessageTypePing, 6},
		{"Close", MessageTypeClose, 7},
	}
	for _, tc := range cases {
		if int32(tc.got) != tc.want {
			t.Errorf("MessageType%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

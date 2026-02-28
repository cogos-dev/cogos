package main

import (
	"testing"
)

// ─── Hello round-trip ───────────────────────────────────────────────────────────

func TestBEPHelloMarshalUnmarshal(t *testing.T) {
	orig := &BEPHello{
		DeviceName:    "test-node",
		ClientName:    "cogos",
		ClientVersion: "2.1.1",
	}

	data := orig.Marshal()
	if len(data) == 0 {
		t.Fatal("Marshal returned empty bytes")
	}

	got := &BEPHello{}
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

func TestBEPHelloEmptyFields(t *testing.T) {
	orig := &BEPHello{}
	data := orig.Marshal()
	// Empty proto = zero bytes.
	if len(data) != 0 {
		t.Errorf("empty Hello marshal should be nil/empty, got %d bytes", len(data))
	}

	got := &BEPHello{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal empty: %v", err)
	}
	if got.DeviceName != "" || got.ClientName != "" || got.ClientVersion != "" {
		t.Error("empty Hello should have all empty fields")
	}
}

// ─── Header round-trip ──────────────────────────────────────────────────────────

func TestBEPHeaderMarshalUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		typ  MessageType
	}{
		{"ClusterConfig", MessageTypeClusterConfig},
		{"Index", MessageTypeIndex},
		{"IndexUpdate", MessageTypeIndexUpdate},
		{"Request", MessageTypeRequest},
		{"Response", MessageTypeResponse},
		{"Ping", MessageTypePing},
		{"Close", MessageTypeClose},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := &BEPHeader{Type: tt.typ, Compression: CompressionNone}
			data := orig.Marshal()

			got := &BEPHeader{}
			if err := got.Unmarshal(data); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Type != tt.typ {
				t.Errorf("Type = %d, want %d", got.Type, tt.typ)
			}
		})
	}
}

// ─── ClusterConfig round-trip ───────────────────────────────────────────────────

func TestBEPClusterConfigRoundTrip(t *testing.T) {
	orig := &BEPClusterConfig{
		Folders: []*BEPFolder{
			{
				ID:    "cogos-agent-defs",
				Label: "Agent Definitions",
				Devices: []*BEPDevice{
					{ID: make([]byte, 32), Name: "node-a"},
					{ID: make([]byte, 32), Name: "node-b"},
				},
			},
		},
	}
	// Fill device IDs with distinct values.
	for i := range orig.Folders[0].Devices[0].ID {
		orig.Folders[0].Devices[0].ID[i] = byte(i)
	}
	for i := range orig.Folders[0].Devices[1].ID {
		orig.Folders[0].Devices[1].ID[i] = byte(i + 100)
	}

	data := orig.Marshal()
	got := &BEPClusterConfig{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(got.Folders) != 1 {
		t.Fatalf("got %d folders, want 1", len(got.Folders))
	}
	f := got.Folders[0]
	if f.ID != "cogos-agent-defs" {
		t.Errorf("folder ID = %q", f.ID)
	}
	if f.Label != "Agent Definitions" {
		t.Errorf("folder Label = %q", f.Label)
	}
	if len(f.Devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(f.Devices))
	}
	if f.Devices[0].Name != "node-a" || f.Devices[1].Name != "node-b" {
		t.Errorf("device names: %q, %q", f.Devices[0].Name, f.Devices[1].Name)
	}
}

// ─── FileInfo round-trip ────────────────────────────────────────────────────────

func TestBEPFileInfoRoundTrip(t *testing.T) {
	orig := &BEPFileInfo{
		Name:       "whirl.agent.yaml",
		Size:       1234,
		ModifiedS:  1700000000,
		ModifiedNs: 500000000,
		ModifiedBy: 42,
		Deleted:    false,
		Sequence:   7,
		Version: BEPVector{
			Counters: []*BEPCounter{
				{ID: 42, Value: 7},
				{ID: 99, Value: 3},
			},
		},
		Blocks: []*BEPBlockInfo{
			{Offset: 0, Size: 1234, Hash: []byte("hash-abc")},
		},
		BlocksHash: []byte("overall-hash"),
	}

	data := orig.Marshal()
	got := &BEPFileInfo{}
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
	if got.ModifiedNs != orig.ModifiedNs {
		t.Errorf("ModifiedNs = %d, want %d", got.ModifiedNs, orig.ModifiedNs)
	}
	if got.ModifiedBy != orig.ModifiedBy {
		t.Errorf("ModifiedBy = %d, want %d", got.ModifiedBy, orig.ModifiedBy)
	}
	if got.Deleted != orig.Deleted {
		t.Errorf("Deleted = %v", got.Deleted)
	}
	if got.Sequence != orig.Sequence {
		t.Errorf("Sequence = %d, want %d", got.Sequence, orig.Sequence)
	}
	if len(got.Version.Counters) != 2 {
		t.Fatalf("Version.Counters len = %d, want 2", len(got.Version.Counters))
	}
	if got.Version.Counters[0].ID != 42 || got.Version.Counters[0].Value != 7 {
		t.Errorf("Counter[0] = {%d, %d}", got.Version.Counters[0].ID, got.Version.Counters[0].Value)
	}
	if len(got.Blocks) != 1 {
		t.Fatalf("Blocks len = %d, want 1", len(got.Blocks))
	}
	if got.Blocks[0].Size != 1234 {
		t.Errorf("Block[0].Size = %d", got.Blocks[0].Size)
	}
	if string(got.BlocksHash) != "overall-hash" {
		t.Errorf("BlocksHash = %q", got.BlocksHash)
	}
}

func TestBEPFileInfoDeleted(t *testing.T) {
	orig := &BEPFileInfo{
		Name:    "removed.agent.yaml",
		Deleted: true,
		Version: BEPVector{
			Counters: []*BEPCounter{{ID: 1, Value: 5}},
		},
		Sequence: 5,
	}

	data := orig.Marshal()
	got := &BEPFileInfo{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.Deleted {
		t.Error("expected Deleted=true")
	}
	if got.Name != "removed.agent.yaml" {
		t.Errorf("Name = %q", got.Name)
	}
}

// ─── Index / IndexUpdate round-trip ─────────────────────────────────────────────

func TestBEPIndexRoundTrip(t *testing.T) {
	orig := &BEPIndex{
		Folder: "cogos-agent-defs",
		Files: []*BEPFileInfo{
			{Name: "a.agent.yaml", Size: 100, Sequence: 1},
			{Name: "b.agent.yaml", Size: 200, Sequence: 2},
		},
	}

	data := orig.Marshal()
	got := &BEPIndex{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Folder != orig.Folder {
		t.Errorf("Folder = %q", got.Folder)
	}
	if len(got.Files) != 2 {
		t.Fatalf("Files len = %d, want 2", len(got.Files))
	}
	if got.Files[0].Name != "a.agent.yaml" || got.Files[1].Name != "b.agent.yaml" {
		t.Errorf("file names: %q, %q", got.Files[0].Name, got.Files[1].Name)
	}
}

func TestBEPIndexEmpty(t *testing.T) {
	orig := &BEPIndex{Folder: "test"}
	data := orig.Marshal()
	got := &BEPIndex{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Folder != "test" {
		t.Errorf("Folder = %q", got.Folder)
	}
	if len(got.Files) != 0 {
		t.Errorf("expected 0 files, got %d", len(got.Files))
	}
}

// ─── Request / Response round-trip ──────────────────────────────────────────────

func TestBEPRequestRoundTrip(t *testing.T) {
	orig := &BEPRequest{
		ID:     42,
		Folder: "cogos-agent-defs",
		Name:   "whirl.agent.yaml",
		Offset: 0,
		Size:   1024,
		Hash:   []byte("hash123"),
	}

	data := orig.Marshal()
	got := &BEPRequest{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != orig.ID {
		t.Errorf("ID = %d, want %d", got.ID, orig.ID)
	}
	if got.Folder != orig.Folder {
		t.Errorf("Folder = %q", got.Folder)
	}
	if got.Name != orig.Name {
		t.Errorf("Name = %q", got.Name)
	}
	if got.Size != orig.Size {
		t.Errorf("Size = %d", got.Size)
	}
}

func TestBEPResponseRoundTrip(t *testing.T) {
	orig := &BEPResponse{
		ID:   42,
		Data: []byte("file-content-here"),
		Code: ErrorCodeNoError,
	}

	data := orig.Marshal()
	got := &BEPResponse{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != orig.ID {
		t.Errorf("ID = %d", got.ID)
	}
	if string(got.Data) != "file-content-here" {
		t.Errorf("Data = %q", got.Data)
	}
	if got.Code != ErrorCodeNoError {
		t.Errorf("Code = %d", got.Code)
	}
}

func TestBEPResponseErrorCode(t *testing.T) {
	orig := &BEPResponse{ID: 1, Code: ErrorCodeNoSuchFile}
	data := orig.Marshal()
	got := &BEPResponse{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Code != ErrorCodeNoSuchFile {
		t.Errorf("Code = %d, want %d", got.Code, ErrorCodeNoSuchFile)
	}
}

// ─── Ping / Close ───────────────────────────────────────────────────────────────

func TestBEPPingRoundTrip(t *testing.T) {
	p := &BEPPing{}
	data := p.Marshal()
	if len(data) != 0 {
		t.Errorf("Ping marshal should be empty, got %d bytes", len(data))
	}
	if err := p.Unmarshal(data); err != nil {
		t.Fatalf("Ping Unmarshal: %v", err)
	}
}

func TestBEPCloseRoundTrip(t *testing.T) {
	orig := &BEPClose{Reason: "shutting down"}
	data := orig.Marshal()
	got := &BEPClose{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Reason != orig.Reason {
		t.Errorf("Reason = %q, want %q", got.Reason, orig.Reason)
	}
}

// ─── Vector round-trip ──────────────────────────────────────────────────────────

func TestBEPVectorRoundTrip(t *testing.T) {
	orig := &BEPVector{
		Counters: []*BEPCounter{
			{ID: 1, Value: 10},
			{ID: 2, Value: 20},
			{ID: 3, Value: 30},
		},
	}
	data := orig.Marshal()
	got := &BEPVector{}
	if err := got.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got.Counters) != 3 {
		t.Fatalf("Counters len = %d, want 3", len(got.Counters))
	}
	for i, c := range got.Counters {
		if c.ID != orig.Counters[i].ID || c.Value != orig.Counters[i].Value {
			t.Errorf("Counter[%d] = {%d, %d}, want {%d, %d}",
				i, c.ID, c.Value, orig.Counters[i].ID, orig.Counters[i].Value)
		}
	}
}

// ─── pbDecode bad input ─────────────────────────────────────────────────────────

func TestPbDecodeInvalidInput(t *testing.T) {
	// Truncated tag.
	h := &BEPHello{}
	err := h.Unmarshal([]byte{0x80})
	if err == nil {
		t.Error("expected error on truncated input")
	}
}

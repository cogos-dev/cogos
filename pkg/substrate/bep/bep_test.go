package bep_test

import (
	"reflect"
	"testing"

	origin    "github.com/myrgic/cogos/pkg/bep"
	substrate "github.com/myrgic/cogos/pkg/substrate/bep"
)

// TestTypeIdentity verifies that type aliases in the substrate re-export layer
// are identical to their origin types via reflect.TypeOf.
func TestTypeIdentity(t *testing.T) {
	cases := []struct {
		name     string
		origin   reflect.Type
		reexport reflect.Type
	}{
		{"Ordering", reflect.TypeOf(origin.Ordering(0)), reflect.TypeOf(substrate.Ordering(0))},
		{"MessageType", reflect.TypeOf(origin.MessageType(0)), reflect.TypeOf(substrate.MessageType(0))},
		{"MessageCompression", reflect.TypeOf(origin.MessageCompression(0)), reflect.TypeOf(substrate.MessageCompression(0))},
		{"ErrorCode", reflect.TypeOf(origin.ErrorCode(0)), reflect.TypeOf(substrate.ErrorCode(0))},
		{"DeviceID", reflect.TypeOf(origin.DeviceID{}), reflect.TypeOf(substrate.DeviceID{})},
		{"Peer", reflect.TypeOf(origin.Peer{}), reflect.TypeOf(substrate.Peer{})},
		{"Config", reflect.TypeOf(origin.Config{}), reflect.TypeOf(substrate.Config{})},
		{"SyncStatus", reflect.TypeOf(origin.SyncStatus{}), reflect.TypeOf(substrate.SyncStatus{})},
		{"EngineStatus", reflect.TypeOf(origin.EngineStatus{}), reflect.TypeOf(substrate.EngineStatus{})},
		{"PeerStatusSummary", reflect.TypeOf(origin.PeerStatusSummary{}), reflect.TypeOf(substrate.PeerStatusSummary{})},
		{"ReceivedEvent", reflect.TypeOf(origin.ReceivedEvent{}), reflect.TypeOf(substrate.ReceivedEvent{})},
		{"VersionVector", reflect.TypeOf(origin.VersionVector{}), reflect.TypeOf(substrate.VersionVector{})},
		{"IndexEntry", reflect.TypeOf(origin.IndexEntry{}), reflect.TypeOf(substrate.IndexEntry{})},
		{"DiffResult", reflect.TypeOf(origin.DiffResult{}), reflect.TypeOf(substrate.DiffResult{})},
		{"PersistedIndex", reflect.TypeOf(origin.PersistedIndex{}), reflect.TypeOf(substrate.PersistedIndex{})},
		{"Hello", reflect.TypeOf(origin.Hello{}), reflect.TypeOf(substrate.Hello{})},
		{"Header", reflect.TypeOf(origin.Header{}), reflect.TypeOf(substrate.Header{})},
		{"Device", reflect.TypeOf(origin.Device{}), reflect.TypeOf(substrate.Device{})},
		{"Folder", reflect.TypeOf(origin.Folder{}), reflect.TypeOf(substrate.Folder{})},
		{"ClusterConfig", reflect.TypeOf(origin.ClusterConfig{}), reflect.TypeOf(substrate.ClusterConfig{})},
		{"Counter", reflect.TypeOf(origin.Counter{}), reflect.TypeOf(substrate.Counter{})},
		{"Vector", reflect.TypeOf(origin.Vector{}), reflect.TypeOf(substrate.Vector{})},
		{"BlockInfo", reflect.TypeOf(origin.BlockInfo{}), reflect.TypeOf(substrate.BlockInfo{})},
		{"FileInfo", reflect.TypeOf(origin.FileInfo{}), reflect.TypeOf(substrate.FileInfo{})},
		{"Index", reflect.TypeOf(origin.Index{}), reflect.TypeOf(substrate.Index{})},
		{"Request", reflect.TypeOf(origin.Request{}), reflect.TypeOf(substrate.Request{})},
		{"Response", reflect.TypeOf(origin.Response{}), reflect.TypeOf(substrate.Response{})},
		{"Ping", reflect.TypeOf(origin.Ping{}), reflect.TypeOf(substrate.Ping{})},
		{"Close", reflect.TypeOf(origin.Close{}), reflect.TypeOf(substrate.Close{})},
		{"SyncEvent", reflect.TypeOf(origin.SyncEvent{}), reflect.TypeOf(substrate.SyncEvent{})},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.origin != tc.reexport {
				t.Errorf("%s: origin type %v != substrate type %v (alias is broken)", tc.name, tc.origin, tc.reexport)
			}
		})
	}
}

// TestConstantIdentity verifies that re-exported constants have the same value.
func TestConstantIdentity(t *testing.T) {
	// Ordering constants
	if origin.OrderEqual != substrate.OrderEqual {
		t.Errorf("OrderEqual mismatch")
	}
	if origin.OrderGreater != substrate.OrderGreater {
		t.Errorf("OrderGreater mismatch")
	}
	if origin.OrderLesser != substrate.OrderLesser {
		t.Errorf("OrderLesser mismatch")
	}
	if origin.OrderConcurrent != substrate.OrderConcurrent {
		t.Errorf("OrderConcurrent mismatch")
	}

	// MessageType constants
	if origin.MessageTypeClusterConfig != substrate.MessageTypeClusterConfig {
		t.Errorf("MessageTypeClusterConfig mismatch")
	}
	if origin.MessageTypeIndex != substrate.MessageTypeIndex {
		t.Errorf("MessageTypeIndex mismatch")
	}
	if origin.MessageTypePing != substrate.MessageTypePing {
		t.Errorf("MessageTypePing mismatch")
	}
	if origin.MessageTypeClose != substrate.MessageTypeClose {
		t.Errorf("MessageTypeClose mismatch")
	}

	// ErrorCode constants
	if origin.ErrorCodeNoError != substrate.ErrorCodeNoError {
		t.Errorf("ErrorCodeNoError mismatch")
	}
	if origin.ErrorCodeGeneric != substrate.ErrorCodeGeneric {
		t.Errorf("ErrorCodeGeneric mismatch")
	}
	if origin.ErrorCodeNoSuchFile != substrate.ErrorCodeNoSuchFile {
		t.Errorf("ErrorCodeNoSuchFile mismatch")
	}
	if origin.ErrorCodeInvalidFile != substrate.ErrorCodeInvalidFile {
		t.Errorf("ErrorCodeInvalidFile mismatch")
	}

	// BEP magic
	if origin.BEPMagic != substrate.BEPMagic {
		t.Errorf("BEPMagic mismatch: 0x%08X != 0x%08X", origin.BEPMagic, substrate.BEPMagic)
	}

	// Event string constants
	if origin.SyncEventPeerConnected != substrate.SyncEventPeerConnected {
		t.Errorf("SyncEventPeerConnected mismatch")
	}
	if origin.SyncEventEngineStarted != substrate.SyncEventEngineStarted {
		t.Errorf("SyncEventEngineStarted mismatch")
	}
	if origin.SyncEventEngineStopped != substrate.SyncEventEngineStopped {
		t.Errorf("SyncEventEngineStopped mismatch")
	}

	// Wire size constants
	if origin.MaxMessageSize != substrate.MaxMessageSize {
		t.Errorf("MaxMessageSize mismatch")
	}
	if origin.MaxHelloSize != substrate.MaxHelloSize {
		t.Errorf("MaxHelloSize mismatch")
	}
}

// TestNewVersionVector verifies the re-exported constructor.
func TestNewVersionVector(t *testing.T) {
	vv := substrate.NewVersionVector()
	if vv == nil {
		t.Fatalf("NewVersionVector() returned nil")
	}
	if len(vv.Counters) != 0 {
		t.Errorf("NewVersionVector().Counters len = %d, want 0", len(vv.Counters))
	}
}

// TestVersionVectorCompare verifies the re-exported version vector comparison.
func TestVersionVectorCompare(t *testing.T) {
	a := substrate.NewVersionVector()
	b := substrate.NewVersionVector()
	if ord := a.Compare(b); ord != substrate.OrderEqual {
		t.Errorf("Compare(empty, empty) = %v, want OrderEqual", ord)
	}
	a.Increment(1)
	if ord := a.Compare(b); ord != substrate.OrderGreater {
		t.Errorf("Compare(incremented, empty) = %v, want OrderGreater", ord)
	}
}

// TestIsAgentCRDFile verifies the re-exported file name predicate.
func TestIsAgentCRDFile(t *testing.T) {
	if !substrate.IsAgentCRDFile("foo.agent.yaml") {
		t.Errorf("IsAgentCRDFile(\"foo.agent.yaml\") = false, want true")
	}
	if substrate.IsAgentCRDFile("foo.yaml") {
		t.Errorf("IsAgentCRDFile(\"foo.yaml\") = true, want false")
	}
}

// TestFormatParseDeviceID verifies the re-exported DeviceID formatting round-trip.
func TestFormatParseDeviceID(t *testing.T) {
	id := substrate.DeviceID{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	}
	formatted := substrate.FormatDeviceID(id)
	if formatted == "" {
		t.Fatalf("FormatDeviceID returned empty string")
	}
	parsed, err := substrate.ParseDeviceID(formatted)
	if err != nil {
		t.Fatalf("ParseDeviceID(%q) error: %v", formatted, err)
	}
	if parsed != id {
		t.Errorf("round-trip mismatch: got %v, want %v", parsed, id)
	}
}

// TestShortIDFromDeviceID verifies the re-exported short ID derivation.
func TestShortIDFromDeviceID(t *testing.T) {
	id := substrate.DeviceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	got := substrate.ShortIDFromDeviceID(id)
	if got == 0 {
		t.Errorf("ShortIDFromDeviceID returned 0 for non-zero DeviceID")
	}
}

// TestEmitSyncEvent verifies the re-exported event emission helper.
func TestEmitSyncEvent(t *testing.T) {
	evt := substrate.EmitSyncEvent(substrate.SyncEventEngineStarted, map[string]any{"test": true})
	if evt.Type != substrate.SyncEventEngineStarted {
		t.Errorf("SyncEvent.Type = %q, want %q", evt.Type, substrate.SyncEventEngineStarted)
	}
	if evt.Timestamp == "" {
		t.Errorf("SyncEvent.Timestamp is empty")
	}
}

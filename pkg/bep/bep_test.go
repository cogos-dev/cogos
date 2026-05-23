package bep_test

import (
	"reflect"
	"testing"

	shim "github.com/myrgic/cogos/pkg/bep"
	canonical "github.com/myrgic/cogos/pkg/substrate/bep"
)

// TestTypeIdentity verifies that type aliases in the legacy re-export shim
// are identical to their canonical types via reflect.TypeOf.
func TestTypeIdentity(t *testing.T) {
	cases := []struct {
		name      string
		canonical reflect.Type
		shim      reflect.Type
	}{
		{"Ordering", reflect.TypeOf(canonical.Ordering(0)), reflect.TypeOf(shim.Ordering(0))},
		{"MessageType", reflect.TypeOf(canonical.MessageType(0)), reflect.TypeOf(shim.MessageType(0))},
		{"MessageCompression", reflect.TypeOf(canonical.MessageCompression(0)), reflect.TypeOf(shim.MessageCompression(0))},
		{"ErrorCode", reflect.TypeOf(canonical.ErrorCode(0)), reflect.TypeOf(shim.ErrorCode(0))},
		{"DeviceID", reflect.TypeOf(canonical.DeviceID{}), reflect.TypeOf(shim.DeviceID{})},
		{"Peer", reflect.TypeOf(canonical.Peer{}), reflect.TypeOf(shim.Peer{})},
		{"Config", reflect.TypeOf(canonical.Config{}), reflect.TypeOf(shim.Config{})},
		{"SyncStatus", reflect.TypeOf(canonical.SyncStatus{}), reflect.TypeOf(shim.SyncStatus{})},
		{"EngineStatus", reflect.TypeOf(canonical.EngineStatus{}), reflect.TypeOf(shim.EngineStatus{})},
		{"PeerStatusSummary", reflect.TypeOf(canonical.PeerStatusSummary{}), reflect.TypeOf(shim.PeerStatusSummary{})},
		{"ReceivedEvent", reflect.TypeOf(canonical.ReceivedEvent{}), reflect.TypeOf(shim.ReceivedEvent{})},
		{"VersionVector", reflect.TypeOf(canonical.VersionVector{}), reflect.TypeOf(shim.VersionVector{})},
		{"IndexEntry", reflect.TypeOf(canonical.IndexEntry{}), reflect.TypeOf(shim.IndexEntry{})},
		{"DiffResult", reflect.TypeOf(canonical.DiffResult{}), reflect.TypeOf(shim.DiffResult{})},
		{"PersistedIndex", reflect.TypeOf(canonical.PersistedIndex{}), reflect.TypeOf(shim.PersistedIndex{})},
		{"Hello", reflect.TypeOf(canonical.Hello{}), reflect.TypeOf(shim.Hello{})},
		{"Header", reflect.TypeOf(canonical.Header{}), reflect.TypeOf(shim.Header{})},
		{"Device", reflect.TypeOf(canonical.Device{}), reflect.TypeOf(shim.Device{})},
		{"Folder", reflect.TypeOf(canonical.Folder{}), reflect.TypeOf(shim.Folder{})},
		{"ClusterConfig", reflect.TypeOf(canonical.ClusterConfig{}), reflect.TypeOf(shim.ClusterConfig{})},
		{"Counter", reflect.TypeOf(canonical.Counter{}), reflect.TypeOf(shim.Counter{})},
		{"Vector", reflect.TypeOf(canonical.Vector{}), reflect.TypeOf(shim.Vector{})},
		{"BlockInfo", reflect.TypeOf(canonical.BlockInfo{}), reflect.TypeOf(shim.BlockInfo{})},
		{"FileInfo", reflect.TypeOf(canonical.FileInfo{}), reflect.TypeOf(shim.FileInfo{})},
		{"Index", reflect.TypeOf(canonical.Index{}), reflect.TypeOf(shim.Index{})},
		{"Request", reflect.TypeOf(canonical.Request{}), reflect.TypeOf(shim.Request{})},
		{"Response", reflect.TypeOf(canonical.Response{}), reflect.TypeOf(shim.Response{})},
		{"Ping", reflect.TypeOf(canonical.Ping{}), reflect.TypeOf(shim.Ping{})},
		{"Close", reflect.TypeOf(canonical.Close{}), reflect.TypeOf(shim.Close{})},
		{"SyncEvent", reflect.TypeOf(canonical.SyncEvent{}), reflect.TypeOf(shim.SyncEvent{})},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.canonical != tc.shim {
				t.Errorf("%s: canonical type %v != shim type %v (alias is broken)", tc.name, tc.canonical, tc.shim)
			}
		})
	}
}

// TestConstantIdentity verifies that re-exported constants have the same value.
func TestConstantIdentity(t *testing.T) {
	// Ordering constants
	if canonical.OrderEqual != shim.OrderEqual {
		t.Errorf("OrderEqual mismatch")
	}
	if canonical.OrderGreater != shim.OrderGreater {
		t.Errorf("OrderGreater mismatch")
	}
	if canonical.OrderLesser != shim.OrderLesser {
		t.Errorf("OrderLesser mismatch")
	}
	if canonical.OrderConcurrent != shim.OrderConcurrent {
		t.Errorf("OrderConcurrent mismatch")
	}

	// MessageType constants
	if canonical.MessageTypeClusterConfig != shim.MessageTypeClusterConfig {
		t.Errorf("MessageTypeClusterConfig mismatch")
	}
	if canonical.MessageTypeIndex != shim.MessageTypeIndex {
		t.Errorf("MessageTypeIndex mismatch")
	}
	if canonical.MessageTypePing != shim.MessageTypePing {
		t.Errorf("MessageTypePing mismatch")
	}
	if canonical.MessageTypeClose != shim.MessageTypeClose {
		t.Errorf("MessageTypeClose mismatch")
	}

	// ErrorCode constants
	if canonical.ErrorCodeNoError != shim.ErrorCodeNoError {
		t.Errorf("ErrorCodeNoError mismatch")
	}
	if canonical.ErrorCodeGeneric != shim.ErrorCodeGeneric {
		t.Errorf("ErrorCodeGeneric mismatch")
	}
	if canonical.ErrorCodeNoSuchFile != shim.ErrorCodeNoSuchFile {
		t.Errorf("ErrorCodeNoSuchFile mismatch")
	}
	if canonical.ErrorCodeInvalidFile != shim.ErrorCodeInvalidFile {
		t.Errorf("ErrorCodeInvalidFile mismatch")
	}

	// BEP magic
	if canonical.BEPMagic != shim.BEPMagic {
		t.Errorf("BEPMagic mismatch: 0x%08X != 0x%08X", canonical.BEPMagic, shim.BEPMagic)
	}

	// Event string constants
	if canonical.SyncEventPeerConnected != shim.SyncEventPeerConnected {
		t.Errorf("SyncEventPeerConnected mismatch")
	}
	if canonical.SyncEventEngineStarted != shim.SyncEventEngineStarted {
		t.Errorf("SyncEventEngineStarted mismatch")
	}
	if canonical.SyncEventEngineStopped != shim.SyncEventEngineStopped {
		t.Errorf("SyncEventEngineStopped mismatch")
	}

	// Wire size constants
	if canonical.MaxMessageSize != shim.MaxMessageSize {
		t.Errorf("MaxMessageSize mismatch")
	}
	if canonical.MaxHelloSize != shim.MaxHelloSize {
		t.Errorf("MaxHelloSize mismatch")
	}
}

// TestNewVersionVector verifies the re-exported constructor.
func TestNewVersionVector(t *testing.T) {
	vv := shim.NewVersionVector()
	if vv == nil {
		t.Fatalf("NewVersionVector() returned nil")
	}
	if len(vv.Counters) != 0 {
		t.Errorf("NewVersionVector().Counters len = %d, want 0", len(vv.Counters))
	}
}

// TestVersionVectorCompare verifies the re-exported version vector comparison.
func TestVersionVectorCompare(t *testing.T) {
	a := shim.NewVersionVector()
	b := shim.NewVersionVector()
	if ord := a.Compare(b); ord != shim.OrderEqual {
		t.Errorf("Compare(empty, empty) = %v, want OrderEqual", ord)
	}
	a.Increment(1)
	if ord := a.Compare(b); ord != shim.OrderGreater {
		t.Errorf("Compare(incremented, empty) = %v, want OrderGreater", ord)
	}
}

// TestIsAgentCRDFile verifies the re-exported file name predicate.
func TestIsAgentCRDFile(t *testing.T) {
	if !shim.IsAgentCRDFile("foo.agent.yaml") {
		t.Errorf("IsAgentCRDFile(\"foo.agent.yaml\") = false, want true")
	}
	if shim.IsAgentCRDFile("foo.yaml") {
		t.Errorf("IsAgentCRDFile(\"foo.yaml\") = true, want false")
	}
}

// TestFormatParseDeviceID verifies the re-exported DeviceID formatting round-trip.
func TestFormatParseDeviceID(t *testing.T) {
	id := shim.DeviceID{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	}
	formatted := shim.FormatDeviceID(id)
	if formatted == "" {
		t.Fatalf("FormatDeviceID returned empty string")
	}
	parsed, err := shim.ParseDeviceID(formatted)
	if err != nil {
		t.Fatalf("ParseDeviceID(%q) error: %v", formatted, err)
	}
	if parsed != id {
		t.Errorf("round-trip mismatch: got %v, want %v", parsed, id)
	}
}

// TestShortIDFromDeviceID verifies the re-exported short ID derivation.
func TestShortIDFromDeviceID(t *testing.T) {
	id := shim.DeviceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	got := shim.ShortIDFromDeviceID(id)
	if got == 0 {
		t.Errorf("ShortIDFromDeviceID returned 0 for non-zero DeviceID")
	}
}

// TestEmitSyncEvent verifies the re-exported event emission helper.
func TestEmitSyncEvent(t *testing.T) {
	evt := shim.EmitSyncEvent(shim.SyncEventEngineStarted, map[string]any{"test": true})
	if evt.Type != shim.SyncEventEngineStarted {
		t.Errorf("SyncEvent.Type = %q, want %q", evt.Type, shim.SyncEventEngineStarted)
	}
	if evt.Timestamp == "" {
		t.Errorf("SyncEvent.Timestamp is empty")
	}
}

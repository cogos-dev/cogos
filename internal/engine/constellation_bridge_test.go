package engine

import "testing"

func TestNilBridgeTrustSnapshotHealthyDefaults(t *testing.T) {
	t.Parallel()

	bridge := NilBridge{}
	snapshot := bridge.TrustSnapshot()

	if !snapshot.SelfCoherencePass {
		t.Fatal("SelfCoherencePass = false; want true")
	}
	if snapshot.SelfTrustScore != 1.0 {
		t.Fatalf("SelfTrustScore = %v; want 1.0", snapshot.SelfTrustScore)
	}
	if snapshot.PeerCount != 0 {
		t.Fatalf("PeerCount = %d; want 0", snapshot.PeerCount)
	}
	if snapshot.TrustedPeerCount != 0 {
		t.Fatalf("TrustedPeerCount = %d; want 0", snapshot.TrustedPeerCount)
	}
	if !snapshot.ConstellationHealthy {
		t.Fatal("ConstellationHealthy = false; want true")
	}
	if snapshot.Timestamp.IsZero() {
		t.Fatal("Timestamp should be set")
	}
}

func TestNilBridgeEmitHeartbeatReturnsZeroReceipt(t *testing.T) {
	t.Parallel()

	bridge := NilBridge{}
	receipt, err := bridge.EmitHeartbeat(KernelHeartbeatPayload{})
	if err != nil {
		t.Fatalf("EmitHeartbeat returned error: %v", err)
	}
	if receipt.Hash != "" {
		t.Fatalf("Hash = %q; want empty", receipt.Hash)
	}
	if !receipt.Timestamp.IsZero() {
		t.Fatalf("Timestamp = %v; want zero", receipt.Timestamp)
	}
	if receipt.PeersSent != 0 {
		t.Fatalf("PeersSent = %d; want 0", receipt.PeersSent)
	}
}

func TestProcessEmitHeartbeatUsesNilBridgeWhenUnset(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	p := NewProcess(makeConfig(t, root), makeNucleus("Cog", "tester"))
	p.bridge = nil

	p.emitHeartbeat()

	events := mustReadAllEvents(t, root, p.SessionID())
	if len(events) < 2 {
		t.Fatalf("events len = %d; want at least heartbeat event + cogblock record", len(events))
	}
	if p.TrustSnapshot().LastHeartbeatHash == "" {
		t.Fatal("LastHeartbeatHash should be set after heartbeat")
	}
}

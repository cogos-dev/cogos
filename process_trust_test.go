package main

import "testing"

func TestProcessFingerprintStableAcrossCalls(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	p := NewProcess(makeConfig(t, root), makeNucleus("Cog", "tester"))
	p.TrustState.CoherenceFingerprint = "sha256:coherent"

	got1 := p.Fingerprint()
	got2 := p.Fingerprint()
	if got1 == "" {
		t.Fatal("Fingerprint should not be empty")
	}
	if got1 != got2 {
		t.Fatalf("Fingerprint changed across calls: %q != %q", got1, got2)
	}
}

func TestProcessHeartbeatRecordsCogBlock(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	p := NewProcess(makeConfig(t, root), makeNucleus("Cog", "tester"))

	p.emitHeartbeat()

	events := mustReadAllEvents(t, root, p.SessionID())
	if len(events) < 2 {
		t.Fatalf("events len = %d; want at least heartbeat event + cogblock record", len(events))
	}

	foundHeartbeat := false
	foundHeartbeatBlock := false
	for _, event := range events {
		if event.HashedPayload.Type == "heartbeat" {
			foundHeartbeat = true
		}
		if event.HashedPayload.Type != "cogblock.ingest" {
			continue
		}
		if kind, _ := event.HashedPayload.Data["kind"].(string); kind == string(BlockSystemEvent) {
			foundHeartbeatBlock = true
		}
	}
	if !foundHeartbeat {
		t.Fatal("expected heartbeat ledger event")
	}
	if !foundHeartbeatBlock {
		t.Fatal("expected heartbeat CogBlock ledger event")
	}
	if p.TrustSnapshot().LastHeartbeatHash == "" {
		t.Fatal("LastHeartbeatHash should be set after heartbeat")
	}
}

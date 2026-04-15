package coordination

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateClaimReleaseClaimed(t *testing.T) {
	root := t.TempDir()
	path := "memory/semantic/topic.md"

	// Initially not claimed
	if IsClaimed(root, path) {
		t.Fatal("expected path to not be claimed initially")
	}

	// Create claim
	claim, err := CreateClaim(root, path, "testing")
	if err != nil {
		t.Fatalf("CreateClaim failed: %v", err)
	}
	if claim.Path != path {
		t.Errorf("claim.Path = %q, want %q", claim.Path, path)
	}
	if claim.Reason != "testing" {
		t.Errorf("claim.Reason = %q, want %q", claim.Reason, "testing")
	}

	// Now it should be claimed
	if !IsClaimed(root, path) {
		t.Fatal("expected path to be claimed after CreateClaim")
	}

	// ReadClaim should return matching data
	read, err := ReadClaim(root, path)
	if err != nil {
		t.Fatalf("ReadClaim failed: %v", err)
	}
	if read.Path != path || read.Reason != "testing" {
		t.Errorf("ReadClaim returned unexpected data: %+v", read)
	}

	// ClaimOwner
	owner, err := ClaimOwner(root, path)
	if err != nil {
		t.Fatalf("ClaimOwner failed: %v", err)
	}
	if owner == "" {
		t.Error("ClaimOwner returned empty string")
	}

	// ListClaims should include our claim
	claims, err := ListClaims(root)
	if err != nil {
		t.Fatalf("ListClaims failed: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("ListClaims returned %d claims, want 1", len(claims))
	}

	// Release
	if err := ReleaseClaim(root, path); err != nil {
		t.Fatalf("ReleaseClaim failed: %v", err)
	}

	// Should no longer be claimed
	if IsClaimed(root, path) {
		t.Fatal("expected path to not be claimed after ReleaseClaim")
	}

	// Release again should be idempotent
	if err := ReleaseClaim(root, path); err != nil {
		t.Fatalf("second ReleaseClaim failed: %v", err)
	}
}

func TestCreateClaimConflict(t *testing.T) {
	root := t.TempDir()
	path := "shared/resource.md"

	// Create claim as agent A
	t.Setenv("COG_AGENT_ID", "agent-a")
	_, err := CreateClaim(root, path, "work A")
	if err != nil {
		t.Fatalf("agent-a CreateClaim failed: %v", err)
	}

	// Agent B should fail to claim the same path
	t.Setenv("COG_AGENT_ID", "agent-b")
	_, err = CreateClaim(root, path, "work B")
	if err == nil {
		t.Fatal("expected error when agent-b claims path held by agent-a")
	}
}

func TestCheckpointCreateAndWait(t *testing.T) {
	root := t.TempDir()

	t.Setenv("COG_AGENT_ID", "agent-x")
	agent, err := CreateCheckpoint(root, "phase-1")
	if err != nil {
		t.Fatalf("CreateCheckpoint failed: %v", err)
	}
	if agent != "agent-x" {
		t.Errorf("CreateCheckpoint returned agent %q, want %q", agent, "agent-x")
	}

	// Signal file should exist
	signalFile := filepath.Join(root, ".cog", "signals", "checkpoint", "phase-1", "agent-x")
	if _, err := os.Stat(signalFile); err != nil {
		t.Fatalf("checkpoint signal file not found: %v", err)
	}

	// WaitCheckpoint should succeed immediately since agent already checkpointed
	err = WaitCheckpoint(root, "phase-1", []string{"agent-x"}, 1*time.Second)
	if err != nil {
		t.Fatalf("WaitCheckpoint failed: %v", err)
	}
}

func TestWaitCheckpointTimeout(t *testing.T) {
	root := t.TempDir()

	// Wait for an agent that never checkpoints — should timeout
	err := WaitCheckpoint(root, "never", []string{"missing-agent"}, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestHandoffRoundTrip(t *testing.T) {
	root := t.TempDir()

	t.Setenv("COG_AGENT_ID", "sender")
	handoff, err := CreateHandoff(root, "receiver", "report.md", "please review")
	if err != nil {
		t.Fatalf("CreateHandoff failed: %v", err)
	}
	if handoff.From != "sender" || handoff.To != "receiver" {
		t.Errorf("unexpected handoff: %+v", handoff)
	}

	// List handoffs for receiver
	handoffs, err := ListHandoffs(root, "receiver")
	if err != nil {
		t.Fatalf("ListHandoffs failed: %v", err)
	}
	if len(handoffs) != 1 {
		t.Fatalf("ListHandoffs returned %d handoffs, want 1", len(handoffs))
	}

	// List handoffs for sender should be empty
	senderHandoffs, err := ListHandoffs(root, "sender")
	if err != nil {
		t.Fatalf("ListHandoffs for sender failed: %v", err)
	}
	if len(senderHandoffs) != 0 {
		t.Errorf("ListHandoffs for sender returned %d, want 0", len(senderHandoffs))
	}
}

func TestBroadcastRoundTrip(t *testing.T) {
	root := t.TempDir()

	t.Setenv("COG_AGENT_ID", "broadcaster")
	broadcast, err := CreateBroadcast(root, "status", "build complete")
	if err != nil {
		t.Fatalf("CreateBroadcast failed: %v", err)
	}
	if broadcast.Channel != "status" || broadcast.Message != "build complete" {
		t.Errorf("unexpected broadcast: %+v", broadcast)
	}

	// List broadcasts — use large window to include our message
	broadcasts, err := ListBroadcasts(root, "status", 1*time.Hour)
	if err != nil {
		t.Fatalf("ListBroadcasts failed: %v", err)
	}
	if len(broadcasts) != 1 {
		t.Fatalf("ListBroadcasts returned %d, want 1", len(broadcasts))
	}
	if broadcasts[0].Message != "build complete" {
		t.Errorf("broadcast message = %q, want %q", broadcasts[0].Message, "build complete")
	}
}

func TestAgentID(t *testing.T) {
	// With COG_AGENT_ID set
	t.Setenv("COG_AGENT_ID", "test-agent")
	if id := AgentID(); id != "test-agent" {
		t.Errorf("AgentID() = %q, want %q", id, "test-agent")
	}

	// Falls back to USER
	t.Setenv("COG_AGENT_ID", "")
	user := os.Getenv("USER")
	if user != "" {
		if id := AgentID(); id != user {
			t.Errorf("AgentID() = %q, want %q (USER env)", id, user)
		}
	}
}

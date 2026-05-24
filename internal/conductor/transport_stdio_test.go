package conductor

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestTransportPluggability_Stdio proves the SAME conductor (contract.Conductor
// backed by the SDK adapter) works over a real stdio-spawned subprocess, not just
// io.Pipe. It spawns ./cmd/fakeagent, wires the conductor to its stdin/stdout,
// and runs the initialize → new_session → prompt path, asserting the substrate
// `_meta` identity survived the OS pipe (the subprocess echoes sub back).
func TestTransportPluggability_Stdio(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess transport test in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "run", "./cmd/fakeagent")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start subprocess fake agent (offline `go run` build?): %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	sink := &captureSink{}
	cond := newConductorOverStreams(stdin, stdout, sink)

	callCtx, callCancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer callCancel()

	if _, err := cond.Initialize(callCtx, testIdentity()); err != nil {
		t.Fatalf("Initialize over stdio: %v", err)
	}
	sid, err := cond.NewSession(callCtx, defaultSpec("/x"))
	if err != nil {
		t.Fatalf("NewSession over stdio: %v", err)
	}
	if _, err := cond.Prompt(callCtx, sid, "echo identity"); err != nil {
		t.Fatalf("Prompt over stdio: %v", err)
	}

	// The subprocess echoed the substrate sub back as an agent_message_chunk.
	var saw bool
	for _, e := range sink.all() {
		if e.Kind == "agent_message_chunk" && e.Text == "sub="+testSub {
			saw = true
		}
	}
	if !saw {
		t.Errorf("did not observe _meta sub round-trip over stdio; ledger=%v", sink.all())
	}
}

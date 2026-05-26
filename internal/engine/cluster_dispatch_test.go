// cluster_dispatch_test.go — Phase 2 S4: in-process two-node remote dispatch.
//
// TestClusterDispatch proves that:
//
//  1. cog_dispatch_to_harness(target_node="B") on engine A is forwarded over
//     the authenticated BEP channel to engine B.
//  2. B executes the request against its stub local dispatcher.
//  3. B's result arrives back at A and is returned to the caller.
//  4. Concurrent dispatches from the same caller don't mix up their results
//     (each reply carries the correct correlation ID).
//  5. A dispatch with target_node set when no cluster router is wired fails
//     fast with code="cluster_disabled".
//
// The test uses a stub AgentDispatcher on B so it does not require a real model.
// Bounded waits (no infinite blocks) — safe for CI with the race detector.

package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	bep "github.com/myrgic/cogos/pkg/substrate/bep"
)

// ─── Stub dispatcher ─────────────────────────────────────────────────────────

// stubDispatcher is a minimal AgentDispatcher that returns a canned result
// echoing the task text back. Used as B's local harness so no real model is
// needed.
type stubDispatcher struct {
	mu      sync.Mutex
	history []DispatchRequest
}

func (s *stubDispatcher) DispatchToHarness(_ context.Context, req DispatchRequest) (*DispatchBatchResult, error) {
	s.mu.Lock()
	s.history = append(s.history, req)
	s.mu.Unlock()
	return &DispatchBatchResult{
		Results: []DispatchResult{{
			Index:   0,
			Success: true,
			Content: "stub-ok: " + req.Task,
		}},
		TotalDurationSec: 0.001,
	}, nil
}

// ─── Helper: build a two-engine loopback cluster ─────────────────────────────

type twoNodeCluster struct {
	engineA *BEPEngine
	engineB *BEPEngine
	stubB   *stubDispatcher
}

func buildTwoNodeCluster(t *testing.T) *twoNodeCluster {
	t.Helper()

	rootA, certDirA, idA := setupEngineWorkspace(t)
	rootB, certDirB, idB := setupEngineWorkspace(t)

	cfgB := &bep.Config{
		Enabled:    true,
		DeviceID:   bep.FormatDeviceID(idB),
		NodeName:   "nodeB",
		ListenPort: 0,
		CertDir:    certDirB,
		Peers: []bep.Peer{{
			DeviceID: bep.FormatDeviceID(idA),
			Name:     "nodeA",
			Trusted:  true,
		}},
		Discovery: "static",
	}

	providerB := NewBEPProvider(rootB)
	engineB, err := NewBEPEngine(rootB, cfgB, providerB)
	if err != nil {
		t.Fatalf("NewBEPEngine B: %v", err)
	}
	stubB := &stubDispatcher{}
	engineB.SetDispatcher(stubB)

	if err := engineB.Start(); err != nil {
		t.Fatalf("engineB.Start: %v", err)
	}
	t.Cleanup(engineB.Stop)

	bAddr := engineB.ListenerAddr()

	cfgA := &bep.Config{
		Enabled:    true,
		DeviceID:   bep.FormatDeviceID(idA),
		NodeName:   "nodeA",
		ListenPort: 0,
		CertDir:    certDirA,
		Peers: []bep.Peer{{
			DeviceID: bep.FormatDeviceID(idB),
			Name:     "nodeB",
			Address:  bAddr,
			Trusted:  true,
		}},
		Discovery: "static",
	}

	providerA := NewBEPProvider(rootA)
	engineA, err := NewBEPEngine(rootA, cfgA, providerA)
	if err != nil {
		t.Fatalf("NewBEPEngine A: %v", err)
	}
	if err := engineA.Start(); err != nil {
		t.Fatalf("engineA.Start: %v", err)
	}
	t.Cleanup(engineA.Stop)

	// Wait for A to connect to B.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		engineA.peersMu.RLock()
		n := len(engineA.peers)
		engineA.peersMu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	engineA.peersMu.RLock()
	connected := len(engineA.peers) > 0
	engineA.peersMu.RUnlock()
	if !connected {
		t.Fatal("engines failed to connect within 10s")
	}

	return &twoNodeCluster{engineA: engineA, engineB: engineB, stubB: stubB}
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestClusterDispatch_BasicRoundTrip sends one dispatch from A targeting B and
// verifies the result content comes from B's stub dispatcher.
func TestClusterDispatch_BasicRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cluster E2E in short mode")
	}

	cluster := buildTwoNodeCluster(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req := DispatchRequest{
		Task:           "hello-from-A",
		TimeoutSeconds: 10,
	}
	result, err := cluster.engineA.RemoteDispatch(ctx, "nodeB", req)
	if err != nil {
		t.Fatalf("RemoteDispatch: %v", err)
	}
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result slot, got %d", len(result.Results))
	}
	if !result.Results[0].Success {
		t.Errorf("result[0].Success = false; want true")
	}
	want := "stub-ok: hello-from-A"
	if result.Results[0].Content != want {
		t.Errorf("content = %q; want %q", result.Results[0].Content, want)
	}
}

// TestClusterDispatch_TargetNodeClearedOnReceive verifies that the request
// forwarded to B has TargetNode cleared (loop-prevention).
func TestClusterDispatch_TargetNodeClearedOnReceive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cluster E2E in short mode")
	}

	cluster := buildTwoNodeCluster(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req := DispatchRequest{
		Task:           "check-target-cleared",
		TargetNode:     "nodeB", // will be cleared before B's dispatcher sees it
		TimeoutSeconds: 10,
	}
	_, err := cluster.engineA.RemoteDispatch(ctx, "nodeB", req)
	if err != nil {
		t.Fatalf("RemoteDispatch: %v", err)
	}

	cluster.stubB.mu.Lock()
	defer cluster.stubB.mu.Unlock()
	if len(cluster.stubB.history) == 0 {
		t.Fatal("B's dispatcher was never called")
	}
	last := cluster.stubB.history[len(cluster.stubB.history)-1]
	if last.TargetNode != "" {
		t.Errorf("TargetNode not cleared on B; got %q", last.TargetNode)
	}
}

// TestClusterDispatch_ConcurrentRequests sends N dispatches in parallel from A
// to B and verifies each result matches its own task string (no cross-mixing).
func TestClusterDispatch_ConcurrentRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cluster E2E in short mode")
	}

	cluster := buildTwoNodeCluster(t)

	const n = 5
	type outcome struct {
		idx     int
		content string
		err     error
	}
	results := make([]outcome, n)
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			req := DispatchRequest{
				Task:           fmt.Sprintf("task-%d", i),
				TimeoutSeconds: 15,
			}
			res, err := cluster.engineA.RemoteDispatch(ctx, "nodeB", req)
			if err != nil {
				results[i] = outcome{idx: i, err: err}
				return
			}
			var content string
			if len(res.Results) > 0 {
				content = res.Results[0].Content
			}
			results[i] = outcome{idx: i, content: content}
		}()
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			t.Errorf("slot %d: error: %v", r.idx, r.err)
			continue
		}
		want := fmt.Sprintf("stub-ok: task-%d", r.idx)
		if r.content != want {
			t.Errorf("slot %d: content = %q; want %q", r.idx, r.content, want)
		}
	}
}

// TestClusterDispatch_NoRouterError verifies that QueryDispatchToHarnessRouted
// with a nil router and non-empty TargetNode returns code=cluster_disabled,
// with no connection or goroutine activity (pure unit gate).
func TestClusterDispatch_NoRouterError(t *testing.T) {
	t.Parallel()

	disp := &fakeAgentDispatcher{cannedOk: true}
	req := DispatchRequest{Task: "x", TargetNode: "remote"}

	_, err := QueryDispatchToHarnessRouted(context.Background(), disp, nil, req)
	if err == nil {
		t.Fatal("expected error when TargetNode set but no router")
	}
	ace, ok := err.(*AgentControllerError)
	if !ok {
		t.Fatalf("expected AgentControllerError, got %T: %v", err, err)
	}
	if ace.Code != "cluster_disabled" {
		t.Errorf("code = %q; want cluster_disabled", ace.Code)
	}
}

// TestClusterDispatch_PeerNotConnected verifies that RemoteDispatch with an
// unknown target name returns code=peer_not_connected immediately.
func TestClusterDispatch_PeerNotConnected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cluster E2E in short mode")
	}

	// Build a single-node engine with no peers connected.
	root, certDir, id := setupEngineWorkspace(t)
	cfg := &bep.Config{
		Enabled:    true,
		DeviceID:   bep.FormatDeviceID(id),
		NodeName:   "solo",
		ListenPort: 0,
		CertDir:    certDir,
		Discovery:  "static",
	}
	eng, err := NewBEPEngine(root, cfg, NewBEPProvider(root))
	if err != nil {
		t.Fatalf("NewBEPEngine: %v", err)
	}
	if err := eng.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer eng.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = eng.RemoteDispatch(ctx, "nonexistent", DispatchRequest{Task: "x", TimeoutSeconds: 5})
	if err == nil {
		t.Fatal("expected error for disconnected peer")
	}
	ace, ok := err.(*AgentControllerError)
	if !ok {
		t.Fatalf("expected AgentControllerError, got %T: %v", err, err)
	}
	if ace.Code != "peer_not_connected" {
		t.Errorf("code = %q; want peer_not_connected", ace.Code)
	}
}

// respond_gate_test.go — Unit tests for Gate A (cycle-level respond gating)
// and Gate B (per-turn at-most-once enforcement).
//
// Gate A: the respond tool must NOT appear in the model's visible tool set
// during a purely autonomic cycle (no pending user messages). The
// consolidation_no_respond scope enforces this structurally.
//
// Gate B: if the model invokes respond more than once in a single user turn,
// only the first invocation publishes; subsequent calls return a structured
// error without touching bus_dashboard_response.
//
// Test C (per specification): mock the model invoking respond three times in
// one turn; assert only the first published, 2nd and 3rd returned errors.
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestGateA_RespondAbsentFromAutonomicScope verifies the scope table and
// controller constructor both enforce the Gate A invariant structurally.
func TestGateA_RespondAbsentFromAutonomicScope(t *testing.T) {
	t.Parallel()

	// 1. The consolidation_no_respond scope must NOT include respond.
	noRespondScope, ok := harnessToolScopes["consolidation_no_respond"]
	if !ok {
		t.Fatal("consolidation_no_respond scope not registered in harnessToolScopes")
	}
	for _, name := range noRespondScope {
		if name == engineRespondToolName {
			t.Errorf("consolidation_no_respond scope unexpectedly contains %q", engineRespondToolName)
		}
	}

	// 2. The consolidation_with_respond scope MUST include respond.
	withRespondScope, ok := harnessToolScopes["consolidation_with_respond"]
	if !ok {
		t.Fatal("consolidation_with_respond scope not registered in harnessToolScopes")
	}
	found := false
	for _, name := range withRespondScope {
		if name == engineRespondToolName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("consolidation_with_respond scope missing %q", engineRespondToolName)
	}

	// 3. The controller's backgroundToolsNoRespond registry must omit respond;
	//    backgroundTools must include it.
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	proc := NewProcess(cfg, makeNucleus("Cog", "gate-a-scope"))
	srv := NewServer(cfg, makeNucleus("Cog", "gate-a-scope"), proc)

	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "gate-a-scope"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	fullDefs := ctrl.backgroundTools.Definitions()
	foundInFull := false
	for _, d := range fullDefs {
		if d.Name == engineRespondToolName {
			foundInFull = true
			break
		}
	}
	if !foundInFull {
		t.Error("backgroundTools (full scope) missing respond — Gate A wiring broken")
	}

	if ctrl.backgroundToolsNoRespond == nil {
		t.Fatal("backgroundToolsNoRespond is nil — Gate A not wired in constructor")
	}
	noDefs := ctrl.backgroundToolsNoRespond.Definitions()
	for _, d := range noDefs {
		if d.Name == engineRespondToolName {
			t.Errorf("backgroundToolsNoRespond unexpectedly contains %q", engineRespondToolName)
		}
	}
}

// TestGateA_AutonomicCycleDoesNotPublish verifies that runCycle with NO pending
// messages does not call engineRespondPublish, even when the mock LLM executes.
// The respond tool is structurally absent from the tool set so the executor is
// never called.
func TestGateA_AutonomicCycleDoesNotPublish(t *testing.T) {
	// Not parallel — modifies package-level seam.
	clearEnginePendingQueue(t)
	atomic.StoreUint64(&engineRespondPerTurnCount, 0)

	var publishCount int64
	origPublish := engineRespondPublish
	engineRespondPublish = func(text, reasoning, sessionID string) (int, error) {
		atomic.AddInt64(&publishCount, 1)
		return len(text), nil
	}
	t.Cleanup(func() {
		engineRespondPublish = origPublish
		atomic.StoreUint64(&engineRespondPerTurnCount, 0)
	})

	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.LocalModel = "gemma4:e4b"

	var callSeq atomic.Int64
	model := "gemma4:e4b"
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": model}},
			})
		case "/api/chat":
			seq := callSeq.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch seq {
			case 1:
				// assess: non-sleep action so executeCycleTask is called.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"message": map[string]any{
						"role":    "assistant",
						"content": `{"action":"consolidate","reason":"housekeeping","urgency":0.2,"target":"memory","task":"scan recent events"}`,
					},
					"done": true, "prompt_eval_count": 10, "eval_count": 20,
				})
			default:
				// execute: plain text, no tool calls.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"message": map[string]any{
						"role":    "assistant",
						"content": "housekeeping complete",
					},
					"done": true, "prompt_eval_count": 5, "eval_count": 10,
				})
			}
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer llm.Close()
	t.Setenv(localLLMEndpointEnv, llm.URL)

	proc := NewProcess(cfg, makeNucleus("Cog", "gate-a-autonomic"))
	srv := NewServer(cfg, makeNucleus("Cog", "gate-a-autonomic"), proc)

	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "gate-a-autonomic"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}
	ctrl.SetDashboardBus(srv.busSessions)

	// No pending messages enqueued — this is a pure autonomic cycle.
	result, err := ctrl.TriggerAgent(context.Background(), DefaultAgentID, "test-autonomic-gate-a", true)
	if err != nil {
		t.Fatalf("TriggerAgent: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	if n := atomic.LoadInt64(&publishCount); n != 0 {
		t.Errorf("Gate A failed: engineRespondPublish called %d time(s) on autonomic cycle (want 0)", n)
	}
}

// TestGateB_MultipleRespondCallsInOneTurn is Test C from the verification spec.
//
// Invoke engineRespondExecutor three times within a single simulated user turn.
// Expected behaviour:
//   - 1st call: publishes, returns {"ok":true}.
//   - 2nd call: does NOT publish, returns {"ok":false,"error":"respond already..."}
//   - 3rd call: same as 2nd.
//   - Global engineRespondInvokeCount increments by exactly 1.
func TestGateB_MultipleRespondCallsInOneTurn(t *testing.T) {
	// Not parallel — modifies package-level seams.
	atomic.StoreUint64(&engineRespondInvokeCount, 0)
	atomic.StoreUint64(&engineRespondPerTurnCount, 0)

	var publishCount int64
	origPublish := engineRespondPublish
	engineRespondPublish = func(text, reasoning, sessionID string) (int, error) {
		atomic.AddInt64(&publishCount, 1)
		return len(text), nil
	}
	t.Cleanup(func() {
		engineRespondPublish = origPublish
		atomic.StoreUint64(&engineRespondPerTurnCount, 0)
		atomic.StoreUint64(&engineRespondInvokeCount, 0)
	})

	ctx := WithSessionID(context.Background(), "sess-gate-b")

	invoke := func(t *testing.T, text string) map[string]interface{} {
		t.Helper()
		args, err := json.Marshal(map[string]string{"text": text})
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		raw, err := engineRespondExecutor(ctx, string(args))
		if err != nil {
			t.Fatalf("executor returned Go error: %v", err)
		}
		var out map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("decode result %q: %v", raw, err)
		}
		return out
	}

	// First invocation must succeed and publish.
	r1 := invoke(t, "first reply")
	if ok, _ := r1["ok"].(bool); !ok {
		t.Fatalf("1st invoke should succeed (Gate B not yet triggered): %v", r1)
	}
	if n := atomic.LoadInt64(&publishCount); n != 1 {
		t.Errorf("after 1st invoke: publishCount = %d, want 1", n)
	}

	// Second invocation must be rejected by Gate B.
	r2 := invoke(t, "second reply — must be blocked")
	if ok, _ := r2["ok"].(bool); ok {
		t.Errorf("2nd invoke should return ok=false (Gate B): %v", r2)
	}
	errMsg, _ := r2["error"].(string)
	if errMsg == "" {
		t.Errorf("2nd invoke: expected non-empty error field, got: %v", r2)
	}
	if n := atomic.LoadInt64(&publishCount); n != 1 {
		t.Errorf("after 2nd invoke: publishCount = %d, want still 1", n)
	}

	// Third invocation must also be rejected.
	r3 := invoke(t, "third reply — also blocked")
	if ok, _ := r3["ok"].(bool); ok {
		t.Errorf("3rd invoke should return ok=false (Gate B): %v", r3)
	}
	if n := atomic.LoadInt64(&publishCount); n != 1 {
		t.Errorf("after 3rd invoke: publishCount = %d, want still 1", n)
	}

	// Global invoke counter must be exactly 1 (only the first counted).
	if got := atomic.LoadUint64(&engineRespondInvokeCount); got != 1 {
		t.Errorf("engineRespondInvokeCount = %d, want 1 (only 1st invocation counts)", got)
	}
}

// TestGateB_PerTurnCounterResetBetweenTurns verifies that ResetEngineRespondPerTurnCount
// restores the ability to respond on the next user turn.
func TestGateB_PerTurnCounterResetBetweenTurns(t *testing.T) {
	// Not parallel — modifies per-turn counter.
	origPublish := engineRespondPublish
	engineRespondPublish = func(text, reasoning, sessionID string) (int, error) {
		return len(text), nil
	}
	t.Cleanup(func() {
		engineRespondPublish = origPublish
		atomic.StoreUint64(&engineRespondPerTurnCount, 0)
		atomic.StoreUint64(&engineRespondInvokeCount, 0)
	})

	atomic.StoreUint64(&engineRespondPerTurnCount, 0)
	atomic.StoreUint64(&engineRespondInvokeCount, 0)
	ctx := WithSessionID(context.Background(), "sess-reset-test")

	invoke := func(t *testing.T, text string) map[string]interface{} {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"text": text})
		raw, err := engineRespondExecutor(ctx, string(args))
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
		var out map[string]interface{}
		_ = json.Unmarshal([]byte(raw), &out)
		return out
	}

	// Turn 1: first call succeeds, second is rejected.
	r1a := invoke(t, "turn-1 reply")
	if ok, _ := r1a["ok"].(bool); !ok {
		t.Fatalf("turn 1, 1st call should succeed: %v", r1a)
	}
	r1b := invoke(t, "turn-1 second (blocked)")
	if ok, _ := r1b["ok"].(bool); ok {
		t.Errorf("turn 1, 2nd call should be blocked: %v", r1b)
	}

	// Simulate runCycle resetting the counter for the next user turn.
	ResetEngineRespondPerTurnCount()

	// Turn 2: first call must succeed again after reset.
	r2a := invoke(t, "turn-2 reply")
	if ok, _ := r2a["ok"].(bool); !ok {
		t.Errorf("turn 2, 1st call after reset should succeed: %v", r2a)
	}

	// Turn 2 second call must still be blocked.
	r2b := invoke(t, "turn-2 second (blocked)")
	if ok, _ := r2b["ok"].(bool); ok {
		t.Errorf("turn 2, 2nd call should be blocked after reset: %v", r2b)
	}
}

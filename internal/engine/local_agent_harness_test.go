package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestLocalHarnessControllerTriggerAndList(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.LocalModel = "gemma4:e4b"
	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)

	var call int
	model := "gemma4:e4b"
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": model}},
			})
		case "/api/chat":
			call++
			w.Header().Set("Content-Type", "application/json")
			if call == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"message": map[string]any{
						"role":    "assistant",
						"content": `{"action":"observe","reason":"field changed","urgency":0.4,"target":"memory","task":"summarize current state"}`,
					},
					"done":              true,
					"prompt_eval_count": 1,
					"eval_count":        1,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "local harness executed",
				},
				"done":              true,
				"prompt_eval_count": 1,
				"eval_count":        1,
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer llm.Close()
	t.Setenv(localLLMEndpointEnv, llm.URL)

	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	res, err := ctrl.TriggerAgent(context.Background(), DefaultAgentID, "test", true)
	if err != nil {
		t.Fatalf("TriggerAgent: %v", err)
	}
	if !res.Triggered {
		t.Fatalf("expected triggered=true, got %+v", res)
	}
	if res.Action != "observe" {
		t.Fatalf("Action = %q; want observe", res.Action)
	}

	list, err := ctrl.ListAgents(context.Background(), false)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListAgents len = %d; want 1", len(list))
	}
	if list[0].CycleCount != 1 {
		t.Fatalf("CycleCount = %d; want 1", list[0].CycleCount)
	}

	snap, err := ctrl.GetAgent(context.Background(), DefaultAgentID, true, 5)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if len(snap.Traces) != 1 {
		t.Fatalf("Traces len = %d; want 1", len(snap.Traces))
	}
	if snap.Traces[0].Result != "local harness executed" {
		t.Fatalf("trace result = %q; want local harness executed", snap.Traces[0].Result)
	}
}

func TestServerLegacyAgentStatusRoute(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	srv.SetAgentController(&fakeAgentController{
		GetResult: &AgentSnapshot{
			Summary: AgentSummary{
				AgentID:     DefaultAgentID,
				Alive:       true,
				CycleCount:  3,
				LastAction:  "sleep",
				LastCycle:   "2026-04-21T12:00:00Z",
				LastUrgency: 0.2,
				LastReason:  "idle",
				LastDurMs:   42,
				Model:       "gemma4:e4b",
				Interval:    "1m0s",
				UptimeSec:   60,
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/agent/status", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["cycle_count"].(float64) != 3 {
		t.Fatalf("cycle_count = %v; want 3", body["cycle_count"])
	}
	if body["uptime"].(string) != "1m0s" {
		t.Fatalf("uptime = %v; want 1m0s", body["uptime"])
	}
}

// TestLocalHarnessOllamaConcurrencySerialized verifies that ollamaMu prevents
// runCycle and DispatchToHarness from issuing concurrent /api/chat requests.
// It spins up a fake Ollama server that increments an in-flight counter on
// entry and decrements on exit; any counter value > 1 means concurrent calls
// leaked through and would load duplicate model copies on a real Ollama node.
func TestLocalHarnessOllamaConcurrencySerialized(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.LocalModel = "gemma4:e4b"
	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)

	model := "gemma4:e4b"

	var (
		inFlight    atomic.Int32
		maxInFlight atomic.Int32
	)

	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": model}},
			})
		case "/api/chat":
			cur := inFlight.Add(1)
			// Record the peak concurrency seen.
			for {
				old := maxInFlight.Load()
				if cur <= old {
					break
				}
				if maxInFlight.CompareAndSwap(old, cur) {
					break
				}
			}
			defer inFlight.Add(-1)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": `{"action":"sleep","reason":"idle","urgency":0.0,"target":"","task":""}`,
				},
				"done":              true,
				"prompt_eval_count": 1,
				"eval_count":        1,
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer llm.Close()
	t.Setenv(localLLMEndpointEnv, llm.URL)

	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	ctx := context.Background()

	// Fire a metabolic cycle and a dispatch concurrently. Without ollamaMu
	// both would immediately call buildLocalProvider and hit /api/chat at
	// the same time.
	errs := make(chan error, 2)
	go func() {
		_, err := ctrl.TriggerAgent(ctx, DefaultAgentID, "concurrency-test", true)
		errs <- err
	}()
	go func() {
		_, err := ctrl.DispatchToHarness(ctx, DispatchRequest{
			Task:           "concurrency-check",
			N:              1,
			TimeoutSeconds: 10,
		})
		errs <- err
	}()

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("goroutine %d error: %v", i, err)
		}
	}

	if got := maxInFlight.Load(); got > 1 {
		t.Errorf("max concurrent Ollama /api/chat calls = %d; want <= 1 (ollamaMu not working)", got)
	}
}

// TestDispatchToHarness_StateRouting_ReceptiveGoesToMLX verifies that when the
// process state is "receptive" and process_state_routing maps "receptive" to a
// configured openai-compat provider (simulating mlx-lm), DispatchToHarness
// without an explicit req.Provider routes to that provider rather than the
// legacy Ollama local-LLM path.
//
// This is the core behavioural requirement for Wave C W2: the autonomic loop's
// harness dispatches must honour process_state_routing, not hardcode Ollama.
func TestDispatchToHarness_StateRouting_ReceptiveGoesToMLX(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	// Stand up a lightweight openai-compat stub (simulates mlx-lm endpoint).
	var mlxCalled atomic.Bool
	mlxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gemma-4-e4b","object":"model"}]}`))
		case "/v1/chat/completions":
			mlxCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "chatcmpl-test",
				"object":  "chat.completion",
				"model":   "gemma-4-e4b",
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "mlx response"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
			})
		default:
			t.Errorf("mlxSrv: unexpected path %s", r.URL.Path)
		}
	}))
	defer mlxSrv.Close()

	// Write providers.yaml with process_state_routing: receptive -> mlx-lm.
	// The mlx-lm endpoint points at our httptest server.
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    endpoint: http://localhost:11434
    model: gemma4:e4b
  mlx-lm:
    type: openai
    endpoint: `+mlxSrv.URL+`
    model: gemma-4-e4b
routing:
  default: ollama
  fallback_chain: [mlx-lm, ollama]
  process_state_routing:
    receptive: mlx-lm
`)

	// NewProcess starts in StateReceptive — no transition needed.
	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	if got := proc.State().String(); got != "receptive" {
		t.Fatalf("process initial state = %q; want receptive", got)
	}

	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	ctx := context.Background()
	_, dispErr := ctrl.DispatchToHarness(ctx, DispatchRequest{
		Task:           "state-routing test",
		N:              1,
		TimeoutSeconds: 10,
	})
	// Dispatch may fail (mlx stub returns no tool results, etc.) but what
	// matters is whether the mlx-lm endpoint was called.
	_ = dispErr

	if !mlxCalled.Load() {
		t.Errorf("mlx-lm endpoint was NOT called; expected state-routing (receptive -> mlx-lm) to route there instead of Ollama legacy path")
	}
}

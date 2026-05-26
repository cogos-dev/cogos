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

// TestDispatchToHarness_StateRouting_UnknownStateFallsBackToLegacy verifies
// that a process in an unrecognised state (ProcessState outside the defined
// iota range) falls back to the legacy Ollama probe path instead of routing
// to process_state_routing["unknown"].
func TestDispatchToHarness_StateRouting_UnknownStateFallsBackToLegacy(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	var ollamaCalled atomic.Bool
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			ollamaCalled.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "gemma4:e4b"}},
			})
		case "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{"role": "assistant", "content": "ollama fallback"},
				"done":    true, "prompt_eval_count": 1, "eval_count": 1,
			})
		}
	}))
	defer ollamaSrv.Close()

	// providers.yaml maps "unknown" state to a provider; this MUST NOT fire.
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    endpoint: `+ollamaSrv.URL+`
    model: gemma4:e4b
routing:
  default: ollama
  process_state_routing:
    unknown: ollama
`)
	t.Setenv(localLLMEndpointEnv, ollamaSrv.URL)

	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	// Force the process into an out-of-range state so State().String() returns "unknown".
	proc.transitionWithReason(ProcessState(99), "test: force unknown state")

	if got := proc.State().String(); got != "unknown" {
		t.Fatalf("proc.State().String() = %q; want \"unknown\"", got)
	}

	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	_, _ = ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "unknown-state fallback test",
		N:              1,
		TimeoutSeconds: 10,
	})

	// The legacy Ollama probe should have been hit (via /api/tags), not state-routed.
	if !ollamaCalled.Load() {
		t.Errorf("Ollama /api/tags was NOT called; unknown state should fall back to legacy path, not route via process_state_routing[\"unknown\"]")
	}
}

// TestDispatchToHarness_StateRouting_NoMappingFallsBackToLegacy verifies that
// when a process state has no entry in process_state_routing, dispatch falls
// through to the legacy local-LLM probe.
func TestDispatchToHarness_StateRouting_NoMappingFallsBackToLegacy(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	var ollamaCalled atomic.Bool
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			ollamaCalled.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "gemma4:e4b"}},
			})
		case "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{"role": "assistant", "content": "ok"},
				"done":    true, "prompt_eval_count": 1, "eval_count": 1,
			})
		}
	}))
	defer ollamaSrv.Close()

	// process_state_routing has no "receptive" entry — state has no mapping.
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    endpoint: `+ollamaSrv.URL+`
    model: gemma4:e4b
routing:
  default: ollama
  process_state_routing:
    active: ollama
`)
	t.Setenv(localLLMEndpointEnv, ollamaSrv.URL)

	// NewProcess starts in StateReceptive — no mapping for "receptive" above.
	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	_, _ = ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "no-mapping fallback test",
		N:              1,
		TimeoutSeconds: 10,
	})

	if !ollamaCalled.Load() {
		t.Errorf("Ollama /api/tags was NOT called; receptive state with no mapping should fall back to legacy path")
	}
}

// TestDispatchToHarness_StateRouting_DisabledProviderFallsBackToLegacy verifies
// that when state_routing resolves to a provider that is disabled (enabled: false),
// dispatch falls through to the legacy local-LLM probe.
func TestDispatchToHarness_StateRouting_DisabledProviderFallsBackToLegacy(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	var ollamaCalled atomic.Bool
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			ollamaCalled.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "gemma4:e4b"}},
			})
		case "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{"role": "assistant", "content": "ok"},
				"done":    true, "prompt_eval_count": 1, "eval_count": 1,
			})
		}
	}))
	defer ollamaSrv.Close()

	// mlx-lm is mapped for receptive but marked enabled: false.
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    endpoint: `+ollamaSrv.URL+`
    model: gemma4:e4b
  mlx-lm:
    type: openai
    endpoint: http://127.0.0.1:1
    model: gemma-4-e4b
    enabled: false
routing:
  default: ollama
  process_state_routing:
    receptive: mlx-lm
`)
	t.Setenv(localLLMEndpointEnv, ollamaSrv.URL)

	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	_, _ = ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "disabled-provider fallback test",
		N:              1,
		TimeoutSeconds: 10,
	})

	if !ollamaCalled.Load() {
		t.Errorf("Ollama /api/tags was NOT called; disabled state-routed provider should fall back to legacy path")
	}
}

// TestDispatchToHarness_StateRouting_MissingProviderFallsBackToLegacy verifies
// that when state_routing names a provider that doesn't exist in the providers
// map, dispatch falls through to the legacy local-LLM probe.
func TestDispatchToHarness_StateRouting_MissingProviderFallsBackToLegacy(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	var ollamaCalled atomic.Bool
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			ollamaCalled.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "gemma4:e4b"}},
			})
		case "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{"role": "assistant", "content": "ok"},
				"done":    true, "prompt_eval_count": 1, "eval_count": 1,
			})
		}
	}))
	defer ollamaSrv.Close()

	// process_state_routing points "receptive" at "nonexistent" which is not
	// in the providers map.
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    endpoint: `+ollamaSrv.URL+`
    model: gemma4:e4b
routing:
  default: ollama
  process_state_routing:
    receptive: nonexistent
`)
	t.Setenv(localLLMEndpointEnv, ollamaSrv.URL)

	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	_, _ = ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "missing-provider fallback test",
		N:              1,
		TimeoutSeconds: 10,
	})

	if !ollamaCalled.Load() {
		t.Errorf("Ollama /api/tags was NOT called; missing provider in state_routing should fall back to legacy path")
	}
}

// TestDispatchToHarness_StateRouting_HappyPath_ProviderUsedAndNoError verifies
// the result shape on a successful state-routed dispatch:
// - provider_used is populated with the resolved provider name
// - error is empty on success
// - success is true
func TestDispatchToHarness_StateRouting_HappyPath_ProviderUsedAndNoError(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	mlxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gemma-4-e4b","object":"model"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "chatcmpl-test",
				"object":  "chat.completion",
				"model":   "gemma-4-e4b",
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "done"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
			})
		}
	}))
	defer mlxSrv.Close()

	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  mlx-lm:
    type: openai
    endpoint: `+mlxSrv.URL+`
    model: gemma-4-e4b
routing:
  process_state_routing:
    receptive: mlx-lm
`)

	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	batch, dispErr := ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "happy path provider_used test",
		N:              1,
		TimeoutSeconds: 10,
	})
	if dispErr != nil {
		t.Fatalf("DispatchToHarness: %v", dispErr)
	}
	if len(batch.Results) != 1 {
		t.Fatalf("batch.Results len = %d; want 1", len(batch.Results))
	}
	res := batch.Results[0]

	// provider_used must be populated so callers can distinguish state-routed from legacy.
	if res.ProviderUsed == "" {
		t.Errorf("ProviderUsed is empty; want \"mlx-lm\" on state-routed path")
	}
	if res.ProviderUsed != "mlx-lm" {
		t.Errorf("ProviderUsed = %q; want \"mlx-lm\"", res.ProviderUsed)
	}

	// error must be empty on a successful dispatch — routing notes must NOT
	// bleed into error.
	if res.Error != "" {
		t.Errorf("Error = %q; want empty on successful state-routed dispatch", res.Error)
	}

	if !res.Success {
		t.Errorf("Success = false; want true")
	}
}

// TestDispatchToHarness_StateRouting_LocalProviderSerializesWithCycle verifies
// that a state-routed dispatch to a local OpenAI-compatible provider (mlx-lm,
// vllm, lmstudio, etc.) still acquires ollamaMu and serialises against the
// metabolic cycle. Local providers compete for the same on-device
// accelerator/VRAM as the Ollama metabolic path; concurrent access can cause
// memory pressure. This test pins the contract that provider.Capabilities().IsLocal
// governs the lock, not the provider type name "ollama".
func TestDispatchToHarness_StateRouting_LocalProviderSerializesWithCycle(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32

	// Track calls across both the mlx-lm dispatch stub and the Ollama cycle
	// stub using a shared in-flight counter.
	recordInFlight := func() func() {
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old {
				break
			}
			if maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		return func() { inFlight.Add(-1) }
	}

	mlxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gemma-4-e4b","object":"model"}]}`))
		case "/v1/chat/completions":
			done := recordInFlight()
			defer done()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "chatcmpl-test",
				"object":  "chat.completion",
				"model":   "gemma-4-e4b",
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "done"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
		}
	}))
	defer mlxSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "gemma4:e4b"}},
			})
		case "/api/chat":
			done := recordInFlight()
			defer done()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{"role": "assistant", "content": `{"action":"sleep","reason":"idle","urgency":0.0,"target":"","task":""}`},
				"done": true, "prompt_eval_count": 1, "eval_count": 1,
			})
		}
	}))
	defer ollamaSrv.Close()

	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  mlx-lm:
    type: openai
    endpoint: `+mlxSrv.URL+`
    model: gemma-4-e4b
routing:
  process_state_routing:
    receptive: mlx-lm
`)
	t.Setenv(localLLMEndpointEnv, ollamaSrv.URL)

	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	ctx := context.Background()

	// Fire a state-routed dispatch (mlx-lm, IsLocal=true) and a metabolic
	// cycle concurrently. Both should acquire ollamaMu and therefore serialize;
	// max concurrent in-flight should be <= 1.
	errs := make(chan error, 2)
	go func() {
		_, e := ctrl.TriggerAgent(ctx, DefaultAgentID, "cycle", true)
		errs <- e
	}()
	go func() {
		_, e := ctrl.DispatchToHarness(ctx, DispatchRequest{
			Task:           "local non-Ollama serialization test",
			N:              1,
			TimeoutSeconds: 10,
		})
		errs <- e
	}()

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("goroutine %d error: %v", i, err)
		}
	}

	if got := maxInFlight.Load(); got > 1 {
		t.Errorf("max concurrent local inference calls = %d; want <= 1 (mlx-lm IsLocal=true should still serialize via ollamaMu)", got)
	}
}

// TestDispatchToHarness_Legacy26bDowngradeWarningPerSlot is a regression test
// ensuring that the "26b route unavailable, degraded to e4b" per-slot warning
// from the legacy path still appears in each DispatchResult.Error even when
// Success=true. This was silently dropped in an earlier iteration of the
// state-routing patch; this test pins the contract.
func TestDispatchToHarness_Legacy26bDowngradeWarningPerSlot(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.LocalModel = "gemma4:e4b"

	// Ollama stub that only lists gemma4:e4b — no 26b-class model available.
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "gemma4:e4b"}},
			})
		case "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{"role": "assistant", "content": "ok"},
				"done":    true, "prompt_eval_count": 1, "eval_count": 1,
			})
		}
	}))
	defer ollamaSrv.Close()
	t.Setenv(localLLMEndpointEnv, ollamaSrv.URL)

	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	batch, dispErr := ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "26b downgrade test",
		Model:          DispatchModel26B,
		N:              1,
		TimeoutSeconds: 10,
	})
	if dispErr != nil {
		t.Fatalf("DispatchToHarness: %v", dispErr)
	}
	if len(batch.Results) != 1 {
		t.Fatalf("batch.Results len = %d; want 1", len(batch.Results))
	}
	res := batch.Results[0]

	// Dispatch must succeed — it fell back to e4b.
	if !res.Success {
		t.Errorf("Success = false; want true (26b fallback to e4b should still succeed)")
	}
	// Per-slot Error must carry the downgrade warning so callers that inspect
	// individual slot results can see that the requested model was not honored.
	if res.Error == "" {
		t.Errorf("Error is empty; want downgrade warning (e.g. %q)", "26b route unavailable, degraded to e4b")
	}
	if res.ModelUsed != DispatchModelE4B {
		t.Errorf("ModelUsed = %q; want DispatchModelE4B", res.ModelUsed)
	}
}

// TestDispatchToHarness_TypelessOllamaStillAcquiresOllamaMu verifies that a
// provider named "ollama" WITHOUT an explicit type: field is still treated as
// Ollama for lock-acquisition purposes. The isOllamaProvider helper must mirror
// makeProvider's inference rule (empty type == provider name) so that the
// documented short-form config shape is not silently misclassified as non-Ollama.
func TestDispatchToHarness_TypelessOllamaStillAcquiresOllamaMu(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "gemma4:e4b"}},
			})
		case "/api/chat":
			cur := inFlight.Add(1)
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
				"message": map[string]any{"role": "assistant", "content": `{"action":"sleep","reason":"idle","urgency":0.0,"target":"","task":""}`},
				"done": true, "prompt_eval_count": 1, "eval_count": 1,
			})
		}
	}))
	defer ollamaSrv.Close()

	// Provider named "ollama" with NO explicit type: field — the documented
	// short-form config shape. isOllamaProvider must still return true.
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    endpoint: `+ollamaSrv.URL+`
    model: gemma4:e4b
routing:
  process_state_routing:
    receptive: ollama
`)
	t.Setenv(localLLMEndpointEnv, ollamaSrv.URL)

	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	ctx := context.Background()

	errs := make(chan error, 2)
	go func() {
		_, err := ctrl.TriggerAgent(ctx, DefaultAgentID, "cycle", true)
		errs <- err
	}()
	go func() {
		_, err := ctrl.DispatchToHarness(ctx, DispatchRequest{
			Task:           "typeless-ollama lock test",
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
		t.Errorf("max concurrent Ollama /api/chat = %d; want <= 1 (typeless ollama provider should still hold ollamaMu)", got)
	}
}

// TestDispatchToHarness_HarnessProvider_ResolvesNamedProvider verifies that
// when req.Provider is empty, req.Model resolves to nothing, no
// process_state_routing entry matches, and cfg.HarnessProvider names a provider,
// DispatchToHarness routes to that named provider instead of probing Ollama.
//
// This is the core behaviour for cross-node dispatch: a BEP-received remote
// dispatch arrives with empty Provider and is resolved using the EXECUTING
// node's harness_provider (e.g. eclipse -> lmstudio), not the legacy probe.
func TestDispatchToHarness_HarnessProvider_ResolvesNamedProvider(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	// Stand up an openai-compat stub (simulates the lmstudio endpoint).
	var lmsCalled atomic.Bool
	lmsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gemma-4-e4b","object":"model"}]}`))
		case "/v1/chat/completions":
			lmsCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "chatcmpl-test",
				"object":  "chat.completion",
				"model":   "gemma-4-e4b",
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "lmstudio response"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
			})
		default:
			t.Errorf("lmsSrv: unexpected path %s", r.URL.Path)
		}
	}))
	defer lmsSrv.Close()

	// No process_state_routing — only a named provider "lmstudio".
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  lmstudio:
    type: openai
    endpoint: `+lmsSrv.URL+`
    model: gemma-4-e4b
routing:
  default: lmstudio
`)

	// Point the legacy Ollama probe at a dead address so that if the resolution
	// chain wrongly falls through to Path 3, the test fails loudly (or at least
	// does not hit lmstudio). HarnessProvider must take precedence.
	t.Setenv(localLLMEndpointEnv, "http://127.0.0.1:1")

	cfg.HarnessProvider = "lmstudio"

	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	batch, dispErr := ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "harness-provider default test",
		N:              1,
		TimeoutSeconds: 10,
	})
	if dispErr != nil {
		t.Fatalf("DispatchToHarness: %v", dispErr)
	}

	if !lmsCalled.Load() {
		t.Errorf("lmstudio endpoint was NOT called; expected harness_provider to route there instead of the legacy Ollama probe")
	}
	if len(batch.Results) != 1 {
		t.Fatalf("batch.Results len = %d; want 1", len(batch.Results))
	}
	if got := batch.Results[0].ProviderUsed; got != "lmstudio" {
		t.Errorf("ProviderUsed = %q; want \"lmstudio\" (harness_provider path)", got)
	}
}

// TestDispatchToHarness_HarnessProvider_ExplicitProviderWins verifies that an
// explicit req.Provider takes precedence over cfg.HarnessProvider.
func TestDispatchToHarness_HarnessProvider_ExplicitProviderWins(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	var explicitCalled atomic.Bool
	var harnessCalled atomic.Bool

	explicitSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gemma-4-e4b","object":"model"}]}`))
		case "/v1/chat/completions":
			explicitCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion", "model": "gemma-4-e4b",
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "explicit"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
		}
	}))
	defer explicitSrv.Close()

	harnessSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gemma-4-e4b","object":"model"}]}`))
		case "/v1/chat/completions":
			harnessCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion", "model": "gemma-4-e4b",
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "harness"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
		}
	}))
	defer harnessSrv.Close()

	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  explicit-provider:
    type: openai
    endpoint: `+explicitSrv.URL+`
    model: gemma-4-e4b
  lmstudio:
    type: openai
    endpoint: `+harnessSrv.URL+`
    model: gemma-4-e4b
routing:
  default: lmstudio
`)

	cfg.HarnessProvider = "lmstudio"

	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	_, dispErr := ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "explicit-wins test",
		Provider:       "explicit-provider",
		N:              1,
		TimeoutSeconds: 10,
	})
	if dispErr != nil {
		t.Fatalf("DispatchToHarness: %v", dispErr)
	}

	if !explicitCalled.Load() {
		t.Errorf("explicit-provider endpoint was NOT called; explicit req.Provider must win over harness_provider")
	}
	if harnessCalled.Load() {
		t.Errorf("harness_provider endpoint WAS called; explicit req.Provider should have taken precedence")
	}
}

// TestDispatchToHarness_HarnessProvider_StateRoutingWins verifies that when a
// process_state_routing entry matches the current state, it takes precedence
// over cfg.HarnessProvider (state routing is Path 2, harness_provider is the
// Path 2.5 fallback above the legacy probe).
func TestDispatchToHarness_HarnessProvider_StateRoutingWins(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	var stateCalled atomic.Bool
	var harnessCalled atomic.Bool

	stateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gemma-4-e4b","object":"model"}]}`))
		case "/v1/chat/completions":
			stateCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion", "model": "gemma-4-e4b",
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "state"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
		}
	}))
	defer stateSrv.Close()

	harnessSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gemma-4-e4b","object":"model"}]}`))
		case "/v1/chat/completions":
			harnessCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion", "model": "gemma-4-e4b",
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "harness"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
		}
	}))
	defer harnessSrv.Close()

	// receptive -> state-provider via process_state_routing. lmstudio is the
	// harness_provider fallback that must NOT fire because state routing matched.
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  state-provider:
    type: openai
    endpoint: `+stateSrv.URL+`
    model: gemma-4-e4b
  lmstudio:
    type: openai
    endpoint: `+harnessSrv.URL+`
    model: gemma-4-e4b
routing:
  process_state_routing:
    receptive: state-provider
`)

	cfg.HarnessProvider = "lmstudio"

	// NewProcess starts in StateReceptive — matches the routing entry.
	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	if got := proc.State().String(); got != "receptive" {
		t.Fatalf("process initial state = %q; want receptive", got)
	}

	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	_, dispErr := ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "state-routing-wins test",
		N:              1,
		TimeoutSeconds: 10,
	})
	if dispErr != nil {
		t.Fatalf("DispatchToHarness: %v", dispErr)
	}

	if !stateCalled.Load() {
		t.Errorf("state-provider endpoint was NOT called; process_state_routing must win over harness_provider")
	}
	if harnessCalled.Load() {
		t.Errorf("harness_provider endpoint WAS called; process_state_routing should have taken precedence")
	}
}

// TestDispatchToHarness_EmptyHarnessProvider_FallsBackToLegacy verifies that
// when cfg.HarnessProvider is empty, dispatch falls through to the legacy
// Ollama probe path (unchanged behaviour).
func TestDispatchToHarness_EmptyHarnessProvider_FallsBackToLegacy(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	// HarnessProvider intentionally left empty.

	var ollamaCalled atomic.Bool
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			ollamaCalled.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "gemma4:e4b"}},
			})
		case "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{"role": "assistant", "content": "ollama fallback"},
				"done":    true, "prompt_eval_count": 1, "eval_count": 1,
			})
		}
	}))
	defer ollamaSrv.Close()
	t.Setenv(localLLMEndpointEnv, ollamaSrv.URL)

	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	_, _ = ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "empty-harness-provider legacy test",
		N:              1,
		TimeoutSeconds: 10,
	})

	if !ollamaCalled.Load() {
		t.Errorf("Ollama /api/tags was NOT called; empty harness_provider should fall back to the legacy probe path")
	}
}

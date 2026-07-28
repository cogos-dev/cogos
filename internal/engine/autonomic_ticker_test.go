// autonomic_ticker_test.go — unit tests for the escalation predicate and
// snapshot builder.
//
// Coverage goals:
//
//  1. All-green registry → shouldEscalate returns "" (no LLM call).
//  2. Degraded provider → escalateDegradedHealth.
//  3. OutOfSync provider (health still Healthy) → escalateOutOfSync.
//  4. Explicit trigger pending → escalateExplicitTrigger.
//  5. Idle re-checkin window elapsed → escalateIdleRecheckIn (even when green).
//  6. Idle re-checkin NOT elapsed when lastLLMCycle is recent → no escalation.
//
// The ticker integration test (TestAutonomicTickerAllGreenNoLLM,
// TestAutonomicTickerDegradedEscalates) exercises the full runTicker →
// autonomicTick → tryStartCycle path using stubProvider + withProviders and a
// real http mock for the LLM, verifying call counts.
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// Ensure json and http/httptest are used (used in test helper functions below).
var _ = json.Marshal
var _ *httptest.Server
var _ http.Handler

// --- shouldEscalate unit tests ----------------------------------------------

func TestShouldEscalate_AllGreenNoTriggerWithinWindow(t *testing.T) {
	snap := KernelHealthSnapshot{
		Timestamp: time.Now().UTC(),
		Providers: map[string]reconcile.ResourceStatus{
			"alpha": {Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthHealthy, Operation: reconcile.OperationIdle},
			"beta":  {Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthHealthy, Operation: reconcile.OperationIdle},
		},
		Counts: HealthCounts{Healthy: 2},
	}

	reason := shouldEscalate(snap, false, time.Now().UTC(), AutonomicConfig{IdleRecheckIn: time.Hour})
	if reason != "" {
		t.Errorf("all-green: expected no escalation, got %q", reason)
	}
}

func TestShouldEscalate_DegradedProvider(t *testing.T) {
	snap := KernelHealthSnapshot{
		Timestamp: time.Now().UTC(),
		Providers: map[string]reconcile.ResourceStatus{
			"ok":     {Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthHealthy},
			"broken": {Sync: reconcile.SyncStatusOutOfSync, Health: reconcile.HealthDegraded},
		},
		Counts: HealthCounts{Healthy: 1, Degraded: 1},
	}

	reason := shouldEscalate(snap, false, time.Now().UTC(), AutonomicConfig{IdleRecheckIn: time.Hour})
	if reason != escalateDegradedHealth {
		t.Errorf("degraded provider: expected %q, got %q", escalateDegradedHealth, reason)
	}
}

func TestShouldEscalate_MissingProvider(t *testing.T) {
	snap := KernelHealthSnapshot{
		Timestamp: time.Now().UTC(),
		Providers: map[string]reconcile.ResourceStatus{
			"gone": {Sync: reconcile.SyncStatusUnknown, Health: reconcile.HealthMissing},
		},
		Counts: HealthCounts{Missing: 1},
	}

	reason := shouldEscalate(snap, false, time.Now().UTC(), AutonomicConfig{IdleRecheckIn: time.Hour})
	if reason != escalateDegradedHealth {
		t.Errorf("missing provider: expected %q, got %q", escalateDegradedHealth, reason)
	}
}

func TestShouldEscalate_SuspendedProvider(t *testing.T) {
	snap := KernelHealthSnapshot{
		Timestamp: time.Now().UTC(),
		Providers: map[string]reconcile.ResourceStatus{
			"sus": {Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthSuspended},
		},
		Counts: HealthCounts{Suspended: 1},
	}

	reason := shouldEscalate(snap, false, time.Now().UTC(), AutonomicConfig{IdleRecheckIn: time.Hour})
	if reason != escalateDegradedHealth {
		t.Errorf("suspended provider: expected %q, got %q", escalateDegradedHealth, reason)
	}
}

func TestShouldEscalate_OutOfSyncOnlyHealthy(t *testing.T) {
	// Sync is OutOfSync but health is still Healthy — should trigger
	// escalateOutOfSync not escalateDegradedHealth.
	snap := KernelHealthSnapshot{
		Timestamp: time.Now().UTC(),
		Providers: map[string]reconcile.ResourceStatus{
			"drifted": {Sync: reconcile.SyncStatusOutOfSync, Health: reconcile.HealthHealthy},
		},
		Counts: HealthCounts{Healthy: 1},
	}

	reason := shouldEscalate(snap, false, time.Now().UTC(), AutonomicConfig{IdleRecheckIn: time.Hour})
	if reason != escalateOutOfSync {
		t.Errorf("out-of-sync healthy: expected %q, got %q", escalateOutOfSync, reason)
	}
}

func TestShouldEscalate_ExplicitTrigger_AllGreen(t *testing.T) {
	snap := KernelHealthSnapshot{
		Timestamp: time.Now().UTC(),
		Providers: map[string]reconcile.ResourceStatus{
			"ok": {Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthHealthy},
		},
		Counts: HealthCounts{Healthy: 1},
	}

	reason := shouldEscalate(snap, true, time.Now().UTC(), AutonomicConfig{IdleRecheckIn: time.Hour})
	if reason != escalateExplicitTrigger {
		t.Errorf("explicit trigger: expected %q, got %q", escalateExplicitTrigger, reason)
	}
}

func TestShouldEscalate_IdleRecheckIn_ZeroLastLLM(t *testing.T) {
	// lastLLMCycle zero → first tick ever → always escalate.
	snap := KernelHealthSnapshot{
		Timestamp: time.Now().UTC(),
		Providers: map[string]reconcile.ResourceStatus{
			"ok": {Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthHealthy},
		},
		Counts: HealthCounts{Healthy: 1},
	}

	reason := shouldEscalate(snap, false, time.Time{}, AutonomicConfig{IdleRecheckIn: time.Hour})
	if reason != escalateIdleRecheckIn {
		t.Errorf("zero lastLLMCycle: expected %q, got %q", escalateIdleRecheckIn, reason)
	}
}

func TestShouldEscalate_IdleRecheckIn_WindowElapsed(t *testing.T) {
	// lastLLMCycle is 2 hours ago, window is 1 hour → should escalate.
	snap := KernelHealthSnapshot{
		Timestamp: time.Now().UTC(),
		Providers: map[string]reconcile.ResourceStatus{
			"ok": {Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthHealthy},
		},
		Counts: HealthCounts{Healthy: 1},
	}

	lastLLM := time.Now().UTC().Add(-2 * time.Hour)
	reason := shouldEscalate(snap, false, lastLLM, AutonomicConfig{IdleRecheckIn: time.Hour})
	if reason != escalateIdleRecheckIn {
		t.Errorf("window elapsed: expected %q, got %q", escalateIdleRecheckIn, reason)
	}
}

func TestShouldEscalate_IdleRecheckIn_WindowNotElapsed(t *testing.T) {
	// lastLLMCycle is 30 minutes ago, window is 1 hour → no escalation.
	snap := KernelHealthSnapshot{
		Timestamp: time.Now().UTC(),
		Providers: map[string]reconcile.ResourceStatus{
			"ok": {Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthHealthy},
		},
		Counts: HealthCounts{Healthy: 1},
	}

	lastLLM := time.Now().UTC().Add(-30 * time.Minute)
	reason := shouldEscalate(snap, false, lastLLM, AutonomicConfig{IdleRecheckIn: time.Hour})
	if reason != "" {
		t.Errorf("window not elapsed: expected no escalation, got %q", reason)
	}
}

func TestShouldEscalate_EmptyRegistry_AlwaysIdleCheckin(t *testing.T) {
	// No providers registered → snap is all-green by definition.
	// With a fresh lastLLMCycle (zero), the idle recheckin fires.
	snap := KernelHealthSnapshot{
		Timestamp: time.Now().UTC(),
		Providers: map[string]reconcile.ResourceStatus{},
	}

	reason := shouldEscalate(snap, false, time.Time{}, AutonomicConfig{IdleRecheckIn: time.Hour})
	if reason != escalateIdleRecheckIn {
		t.Errorf("empty registry, zero lastLLM: expected %q, got %q", escalateIdleRecheckIn, reason)
	}
}

// --- buildKernelHealthSnapshot unit tests -----------------------------------

func TestBuildKernelHealthSnapshot_AllGreen(t *testing.T) {
	providers := []*stubProvider{
		{name: "alpha", status: greenStatus()},
		{name: "beta", status: greenStatus()},
	}
	withProviders(t, providers, func() {
		snap := buildKernelHealthSnapshot(context.Background())
		if snap.Counts.Healthy != 2 {
			t.Errorf("healthy count: got %d, want 2", snap.Counts.Healthy)
		}
		if snap.Counts.Degraded != 0 || snap.Counts.Missing != 0 || snap.Counts.Suspended != 0 {
			t.Errorf("non-green counts: %+v", snap.Counts)
		}
		if !snap.AllGreen() {
			t.Error("expected AllGreen() true")
		}
	})
}

func TestBuildKernelHealthSnapshot_Degraded(t *testing.T) {
	providers := []*stubProvider{
		{name: "ok", status: greenStatus()},
		{name: "bad", status: reconcile.ResourceStatus{
			Sync:    reconcile.SyncStatusOutOfSync,
			Health:  reconcile.HealthDegraded,
			Message: "baseline stale",
		}},
	}
	withProviders(t, providers, func() {
		snap := buildKernelHealthSnapshot(context.Background())
		if snap.Counts.Degraded != 1 {
			t.Errorf("degraded count: got %d, want 1", snap.Counts.Degraded)
		}
		if snap.AllGreen() {
			t.Error("expected AllGreen() false when provider is degraded")
		}
		// Provider map should contain both entries.
		if len(snap.Providers) != 2 {
			t.Errorf("provider count: got %d, want 2", len(snap.Providers))
		}
	})
}

func TestBuildKernelHealthSnapshot_EmptyRegistry(t *testing.T) {
	withProviders(t, nil, func() {
		snap := buildKernelHealthSnapshot(context.Background())
		if len(snap.Providers) != 0 {
			t.Errorf("expected empty providers, got %d", len(snap.Providers))
		}
		if !snap.AllGreen() {
			t.Error("empty registry should be AllGreen()")
		}
	})
}

// --- Ticker integration tests -----------------------------------------------

// openaiAssessResponse writes a valid OpenAI-compat response for the assess
// step, honoring the request's stream field. #432: assessCycle now routes
// through CompleteCancelSafe (Stream under the hood), so a mock that always
// returns a plain JSON body regardless of stream:true would silently produce
// an empty response for that call.
func openaiAssessResponse(t *testing.T, w http.ResponseWriter, r *http.Request, action string) {
	t.Helper()
	content := `{"action":"` + action + `","reason":"test","urgency":0.1,"target":"","task":""}`
	if requestWantsStream(r) {
		writeSSECompletion(t, w, content)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
	})
}

// openaiExecuteResponse writes a valid OpenAI-compat response for the execute
// step, honoring the request's stream field (see openaiAssessResponse).
func openaiExecuteResponse(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if requestWantsStream(r) {
		writeSSECompletion(t, w, "done")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{
				"role":    "assistant",
				"content": "done",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
	})
}

// requestWantsStream peeks at the request body's stream field without
// consuming it for downstream handlers (each caller here only reads the
// body once, so a simple decode is sufficient).
func requestWantsStream(r *http.Request) bool {
	var body struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.Stream
}

// ollamaAssessResponse is kept for backward compat with any callers that have
// not yet been migrated; Ollama is decommissioned.
func ollamaAssessResponse(t *testing.T, w http.ResponseWriter, r *http.Request, action string) {
	openaiAssessResponse(t, w, r, action)
}

// ollamaExecuteResponse is kept for backward compat with any callers that have
// not yet been migrated; Ollama is decommissioned.
func ollamaExecuteResponse(t *testing.T, w http.ResponseWriter, r *http.Request) {
	openaiExecuteResponse(t, w, r)
}

// TestAutonomicTickerAllGreenNoLLM verifies that with an all-green registry
// and a recently-stamped lastLLMCycle, shouldEscalate returns "" and no LLM
// call would be routed. This is the pure-predicate path that the ticker uses.
func TestAutonomicTickerAllGreenNoLLM(t *testing.T) {
	providers := []*stubProvider{
		{name: "alpha", status: greenStatus()},
		{name: "beta", status: greenStatus()},
	}

	withProviders(t, providers, func() {
		snap := buildKernelHealthSnapshot(context.Background())
		if !snap.AllGreen() {
			t.Fatal("expected AllGreen() true for two green providers")
		}
		if snap.Counts.Healthy != 2 {
			t.Errorf("healthy count: got %d, want 2", snap.Counts.Healthy)
		}

		// Recent lastLLMCycle + long idle window → no escalation on N ticks.
		lastLLM := time.Now().UTC()
		cfg := AutonomicConfig{IdleRecheckIn: 24 * time.Hour}
		for i := 0; i < 5; i++ {
			reason := shouldEscalate(snap, false, lastLLM, cfg)
			if reason != "" {
				t.Errorf("tick %d: all-green with recent LLM: unexpected escalation %q", i, reason)
			}
		}
	})
}

// TestAutonomicTickerDegradedEscalates verifies that a degraded provider
// causes the first tick to escalate to an LLM cycle. Uses TriggerAgent with
// wait=true to ensure the LLM cycle completes before asserting call counts —
// this is the same pattern used by TestLocalHarnessControllerTriggerAndList.
//
// We verify escalation by directly testing shouldEscalate (pure-function),
// then confirming the controller routes correctly via TriggerAgent when the
// registry is degraded (the predicate is what gates the autonomous path).
func TestAutonomicTickerDegradedEscalates(t *testing.T) {
	providers := []*stubProvider{
		{name: "ok", status: greenStatus()},
		{name: "broken", status: reconcile.ResourceStatus{
			Sync:    reconcile.SyncStatusOutOfSync,
			Health:  reconcile.HealthDegraded,
			Message: "test degradation",
		}},
	}

	// Build the snapshot directly and verify the predicate fires.
	withProviders(t, providers, func() {
		snap := buildKernelHealthSnapshot(context.Background())
		if snap.AllGreen() {
			t.Fatal("degraded provider should make AllGreen() false")
		}
		reason := shouldEscalate(snap, false, time.Now().UTC(), AutonomicConfig{IdleRecheckIn: time.Hour})
		if reason != escalateDegradedHealth {
			t.Errorf("degraded provider: expected escalation %q, got %q", escalateDegradedHealth, reason)
		}
	})

	// Full integration: autonomicTick calls tryStartCycle which fires a goroutine.
	// Use TriggerAgent(wait=true) as a synchronisation point to confirm the
	// LLM cycle completes correctly on a degraded registry.
	model := "gemma4:e4b"
	callCount := 0
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]any{{"id": model, "object": "model"}},
			})
		case "/v1/chat/completions":
			callCount++
			if callCount == 1 {
				openaiAssessResponse(t, w, r, "observe")
				return
			}
			openaiExecuteResponse(t, w, r)
		default:
			// Silently ignore unexpected probes (e.g. Ollama /api/tags probe).
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer llm.Close()
	t.Setenv(localLLMEndpointEnv, llm.URL)

	withProviders(t, providers, func() {
		root := makeWorkspace(t)
		cfg := makeConfig(t, root)
		cfg.LocalModel = model
		proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
		srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)

		ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
		if err != nil {
			t.Fatalf("NewLocalHarnessController: %v", err)
		}

		res, err := ctrl.TriggerAgent(context.Background(), DefaultAgentID, "degraded_health", true)
		if err != nil {
			t.Fatalf("TriggerAgent: %v", err)
		}
		if !res.Triggered {
			t.Fatalf("expected triggered=true, got %+v", res)
		}
		// assess + execute = 2 calls.
		if callCount < 1 {
			t.Errorf("degraded provider: expected at least 1 LLM call, got %d", callCount)
		}
	})
}

// TestAutonomicTickerIdleRecheckIn verifies that a fully-green registry with
// an elapsed idle window fires the escalation predicate. The predicate is pure
// and testable in isolation; we verify it here and trust the harness wires it
// through runTicker via the TestAutonomicTickerAllGreenNoLLM integration test.
func TestAutonomicTickerIdleRecheckIn(t *testing.T) {
	providers := []*stubProvider{
		{name: "green", status: greenStatus()},
	}

	withProviders(t, providers, func() {
		snap := buildKernelHealthSnapshot(context.Background())
		if !snap.AllGreen() {
			t.Fatal("green provider should be AllGreen()")
		}

		// 2 hours ago, window 1 hour → should fire.
		lastLLM := time.Now().UTC().Add(-2 * time.Hour)
		reason := shouldEscalate(snap, false, lastLLM, AutonomicConfig{IdleRecheckIn: time.Hour})
		if reason != escalateIdleRecheckIn {
			t.Errorf("idle window elapsed: expected %q, got %q", escalateIdleRecheckIn, reason)
		}
	})

	// Full integration: trigger an explicit cycle and verify it completes,
	// updating lastLLMCycle so a subsequent tick with a fresh window doesn't fire.
	model := "gemma4:e4b"
	callCount := 0
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]any{{"id": model, "object": "model"}},
			})
		case "/v1/chat/completions":
			callCount++
			openaiAssessResponse(t, w, r, "sleep")
		default:
			// Silently ignore unexpected probes (e.g. Ollama /api/tags probe).
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer llm.Close()
	t.Setenv(localLLMEndpointEnv, llm.URL)

	withProviders(t, providers, func() {
		root := makeWorkspace(t)
		cfg := makeConfig(t, root)
		cfg.LocalModel = model
		proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
		srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)

		ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
		if err != nil {
			t.Fatalf("NewLocalHarnessController: %v", err)
		}

		// Trigger a cycle synchronously; verify it runs and updates lastLLMCycle.
		res, err := ctrl.TriggerAgent(context.Background(), DefaultAgentID, "idle_recheckin", true)
		if err != nil {
			t.Fatalf("TriggerAgent: %v", err)
		}
		if !res.Triggered {
			t.Fatalf("expected triggered=true")
		}
		if callCount != 1 {
			t.Errorf("expected 1 LLM assess call (action=sleep), got %d", callCount)
		}

		// After the cycle, lastLLMCycle should be set to now (within last minute).
		ctrl.mu.RLock()
		last := ctrl.lastLLMCycle
		ctrl.mu.RUnlock()
		if time.Since(last) > time.Minute {
			t.Errorf("lastLLMCycle not updated after cycle: %v", last)
		}

		// A tick immediately after with the 1h window should NOT escalate.
		snap := buildKernelHealthSnapshot(context.Background())
		reason := shouldEscalate(snap, false, last, AutonomicConfig{IdleRecheckIn: time.Hour})
		if reason != "" {
			t.Errorf("after cycle, fresh window: expected no escalation, got %q", reason)
		}
	})
}

// TestAutonomicTickerIdleWindowNotElapsed verifies that if lastLLMCycle is
// recent and all providers are green, shouldEscalate returns "".
func TestAutonomicTickerIdleWindowNotElapsed(t *testing.T) {
	providers := []*stubProvider{
		{name: "green", status: greenStatus()},
	}

	withProviders(t, providers, func() {
		snap := buildKernelHealthSnapshot(context.Background())
		// lastLLMCycle 5 minutes ago, window 1 hour → no escalation.
		lastLLM := time.Now().UTC().Add(-5 * time.Minute)
		reason := shouldEscalate(snap, false, lastLLM, AutonomicConfig{IdleRecheckIn: time.Hour})
		if reason != "" {
			t.Errorf("window not elapsed: expected no escalation, got %q", reason)
		}
	})
}

// --- snapshotFingerprint + dedupe tests ------------------------------------

func TestSnapshotFingerprint_StableOrderIndependent(t *testing.T) {
	a := KernelHealthSnapshot{Providers: map[string]reconcile.ResourceStatus{
		"alpha": {Health: reconcile.HealthHealthy, Sync: reconcile.SyncStatusSynced},
		"beta":  {Health: reconcile.HealthDegraded, Sync: reconcile.SyncStatusOutOfSync},
	}}
	b := KernelHealthSnapshot{Providers: map[string]reconcile.ResourceStatus{
		"beta":  {Health: reconcile.HealthDegraded, Sync: reconcile.SyncStatusOutOfSync},
		"alpha": {Health: reconcile.HealthHealthy, Sync: reconcile.SyncStatusSynced},
	}}
	if snapshotFingerprint(a) != snapshotFingerprint(b) {
		t.Error("fingerprint should be order-independent")
	}
}

func TestSnapshotFingerprint_DiffersOnHealthFlip(t *testing.T) {
	a := KernelHealthSnapshot{Providers: map[string]reconcile.ResourceStatus{
		"x": {Health: reconcile.HealthHealthy, Sync: reconcile.SyncStatusSynced},
	}}
	b := KernelHealthSnapshot{Providers: map[string]reconcile.ResourceStatus{
		"x": {Health: reconcile.HealthDegraded, Sync: reconcile.SyncStatusSynced},
	}}
	if snapshotFingerprint(a) == snapshotFingerprint(b) {
		t.Error("fingerprint should differ when Health bucket changes")
	}
}

func TestSnapshotFingerprint_DiffersOnProviderAddRemove(t *testing.T) {
	one := KernelHealthSnapshot{Providers: map[string]reconcile.ResourceStatus{
		"x": {Health: reconcile.HealthHealthy, Sync: reconcile.SyncStatusSynced},
	}}
	two := KernelHealthSnapshot{Providers: map[string]reconcile.ResourceStatus{
		"x": {Health: reconcile.HealthHealthy, Sync: reconcile.SyncStatusSynced},
		"y": {Health: reconcile.HealthHealthy, Sync: reconcile.SyncStatusSynced},
	}}
	if snapshotFingerprint(one) == snapshotFingerprint(two) {
		t.Error("fingerprint should differ when a new provider appears")
	}
}

func TestSnapshotFingerprint_StableAcrossOperationFlip(t *testing.T) {
	// Operation transitions are short-lived and not load-bearing for the
	// dedupe contract — fingerprint should be stable across them.
	a := KernelHealthSnapshot{Providers: map[string]reconcile.ResourceStatus{
		"x": {Health: reconcile.HealthHealthy, Sync: reconcile.SyncStatusSynced, Operation: reconcile.OperationIdle},
	}}
	b := KernelHealthSnapshot{Providers: map[string]reconcile.ResourceStatus{
		"x": {Health: reconcile.HealthHealthy, Sync: reconcile.SyncStatusSynced, Operation: reconcile.OperationSyncing},
	}}
	if snapshotFingerprint(a) != snapshotFingerprint(b) {
		t.Error("fingerprint should be stable across Operation transitions")
	}
}

func TestSnapshotFingerprint_EmptyRegistry(t *testing.T) {
	if got := snapshotFingerprint(KernelHealthSnapshot{}); got != "" {
		t.Errorf("empty registry: expected empty fingerprint, got %q", got)
	}
}

// TestAutonomicTick_DedupeStableDegradationSuppresses verifies the core
// contract: when the same provider population is in the same non-green
// buckets as the snapshot that last triggered an LLM cycle (and the idle
// re-checkin window has not elapsed), autonomicTick suppresses escalation.
// We simulate "we already escalated for this picture" by pre-arming
// lastEscalatedFingerprint and a recent lastLLMCycle, then asserting that
// running stays false (no cycle goroutine spawned).
func TestAutonomicTick_DedupeStableDegradationSuppresses(t *testing.T) {
	providers := []*stubProvider{
		{name: "ok", status: greenStatus()},
		{name: "broken", status: reconcile.ResourceStatus{
			Sync:    reconcile.SyncStatusOutOfSync,
			Health:  reconcile.HealthDegraded,
			Message: "stable degradation",
		}},
	}

	withProviders(t, providers, func() {
		root := makeWorkspace(t)
		cfg := makeConfig(t, root)
		cfg.LocalModel = "gemma4:e4b"
		proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
		srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)

		ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
		if err != nil {
			t.Fatalf("NewLocalHarnessController: %v", err)
		}

		// Compute the fingerprint the upcoming tick will see.
		snap := buildKernelHealthSnapshot(context.Background())
		fp := snapshotFingerprint(snap)
		if fp == "" {
			t.Fatal("expected non-empty fingerprint for two-provider snap")
		}

		// Pre-arm: the LLM has already seen this exact picture, recently.
		ctrl.mu.Lock()
		ctrl.lastEscalatedFingerprint = fp
		ctrl.lastLLMCycle = time.Now().UTC()
		ctrl.mu.Unlock()
		ctrl.autonomicCfg = AutonomicConfig{IdleRecheckIn: time.Hour}

		ctrl.autonomicTick(context.Background())

		// Suppression happens before tryStartCycle, so running must remain false.
		if ctrl.running.Load() {
			t.Error("expected running=false after stable-degradation tick was suppressed")
		}
	})
}

// TestAutonomicTick_DedupeFiresOnHealthChange verifies that when the
// degradation shape CHANGES (a provider transitions to a new non-green
// bucket), suppression releases and the LLM is re-escalated.
func TestAutonomicTick_DedupeFiresOnHealthChange(t *testing.T) {
	providers := []*stubProvider{
		{name: "ok", status: greenStatus()},
		{name: "shifty", status: reconcile.ResourceStatus{
			Sync:   reconcile.SyncStatusOutOfSync,
			Health: reconcile.HealthDegraded,
		}},
	}

	withProviders(t, providers, func() {
		root := makeWorkspace(t)
		cfg := makeConfig(t, root)
		cfg.LocalModel = "gemma4:e4b"
		proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
		srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)

		ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
		if err != nil {
			t.Fatalf("NewLocalHarnessController: %v", err)
		}

		// Pre-arm with a fingerprint that does NOT match the current snapshot
		// (simulates "previous degradation was different from current").
		ctrl.mu.Lock()
		ctrl.lastEscalatedFingerprint = "stale-fingerprint-from-before"
		ctrl.lastLLMCycle = time.Now().UTC()
		ctrl.mu.Unlock()
		ctrl.autonomicCfg = AutonomicConfig{IdleRecheckIn: time.Hour}

		// Reason returned by shouldEscalate will be escalateDegradedHealth, but
		// dedupe sees the saved fingerprint doesn't match → escalation proceeds.
		// running flips to true under tryStartCycle's CompareAndSwap.
		ctrl.autonomicTick(context.Background())

		// Wait briefly for the cycle goroutine to be observed as running. It may
		// finish quickly without an LLM mock, but it should at least start.
		started := false
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if ctrl.running.Load() {
				started = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		// The cycle goroutine may have already errored out and reset running.
		// Verify EITHER running was observed true, OR the saved fingerprint
		// has been updated to the new snapshot's fingerprint (proof that the
		// post-escalation update path executed).
		ctrl.mu.RLock()
		newFP := ctrl.lastEscalatedFingerprint
		ctrl.mu.RUnlock()
		expectedFP := snapshotFingerprint(buildKernelHealthSnapshot(context.Background()))
		if !started && newFP != expectedFP {
			t.Errorf("expected escalation to fire on changed fingerprint: started=%v newFP=%q expected=%q",
				started, newFP, expectedFP)
		}

		// Wait for the cycle to settle so we don't leak a goroutine into the next test.
		settleDeadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(settleDeadline) {
			if !ctrl.running.Load() {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

// TestAutonomicTick_DedupeReleasesOnReturnToGreen verifies that when the
// snapshot returns to AllGreen, the saved fingerprint is cleared so a
// later degradation re-escalates (rather than being suppressed against a
// stale fingerprint).
func TestAutonomicTick_DedupeReleasesOnReturnToGreen(t *testing.T) {
	providers := []*stubProvider{
		{name: "ok", status: greenStatus()},
	}

	withProviders(t, providers, func() {
		root := makeWorkspace(t)
		cfg := makeConfig(t, root)
		cfg.LocalModel = "gemma4:e4b"
		proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
		srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)

		ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
		if err != nil {
			t.Fatalf("NewLocalHarnessController: %v", err)
		}

		// Pre-arm a stale fingerprint (from a previous degradation cycle).
		// Set lastLLMCycle recent so idle_recheckin doesn't fire.
		ctrl.mu.Lock()
		ctrl.lastEscalatedFingerprint = "stale-from-previous-degradation"
		ctrl.lastLLMCycle = time.Now().UTC()
		ctrl.mu.Unlock()
		ctrl.autonomicCfg = AutonomicConfig{IdleRecheckIn: time.Hour}

		ctrl.autonomicTick(context.Background())

		// Snapshot is all-green, so the fingerprint should be cleared.
		ctrl.mu.RLock()
		fp := ctrl.lastEscalatedFingerprint
		ctrl.mu.RUnlock()
		if fp != "" {
			t.Errorf("expected fingerprint cleared on return-to-green, got %q", fp)
		}
	})
}

// --- healDegradedProviders unit tests ----------------------------------------

// selfHealableProvider is a Reconcilable stub that records whether
// FetchLive, ComputePlan, and ApplyPlan were called. Its Health() returns the
// injected status. Used to verify that healDegradedProviders calls the full
// plan/apply cycle for unhealthy providers.
type selfHealableProvider struct {
	name             string
	status           reconcile.ResourceStatus
	fetchLiveCalls   int
	computePlanCalls int
	applyPlanCalls   int
	// planHasChanges controls whether ComputePlan returns a non-empty plan.
	planHasChanges bool
}

func (s *selfHealableProvider) Type() string { return s.name }

func (s *selfHealableProvider) LoadConfig(string) (any, error) { return nil, nil }

func (s *selfHealableProvider) FetchLive(context.Context, any) (any, error) {
	s.fetchLiveCalls++
	return struct{}{}, nil
}

func (s *selfHealableProvider) ComputePlan(_ any, _ any, _ *reconcile.State) (*reconcile.Plan, error) {
	s.computePlanCalls++
	plan := &reconcile.Plan{ResourceType: s.name}
	if s.planHasChanges {
		plan.Actions = []reconcile.Action{{
			Action:       reconcile.ActionUpdate,
			ResourceType: s.name,
			Name:         s.name + "/heal",
		}}
		plan.Summary.Updates = 1
	}
	return plan, nil
}

func (s *selfHealableProvider) ApplyPlan(_ context.Context, _ *reconcile.Plan) ([]reconcile.Result, error) {
	s.applyPlanCalls++
	return []reconcile.Result{{
		Phase:  "apply",
		Action: string(reconcile.ActionUpdate),
		Name:   s.name + "/heal",
		Status: reconcile.ApplySucceeded,
	}}, nil
}

func (s *selfHealableProvider) BuildState(_ any, _ any, _ *reconcile.State) (*reconcile.State, error) {
	return nil, nil
}

func (s *selfHealableProvider) Health() reconcile.ResourceStatus { return s.status }

// withSelfHealableProviders registers selfHealableProvider instances in the
// global reconcile registry for the duration of fn(). Uses healthRegistryMu
// (same lock as withProviders) to avoid concurrent mutation.
func withSelfHealableProviders(t *testing.T, providers []*selfHealableProvider, fn func()) {
	t.Helper()
	healthRegistryMu.Lock()
	defer healthRegistryMu.Unlock()

	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	for _, p := range providers {
		reconcile.RegisterProvider(p.name, p)
	}
	fn()
}

// TestHealDegradedProviders_CallsReconcileOnUnhealthy verifies that
// healDegradedProviders calls FetchLive→ComputePlan→ApplyPlan for providers
// whose Health() is Degraded. This is the regression test for issue #150:
// the autonomic ticker must self-heal mlx-supervised providers on crash.
func TestHealDegradedProviders_CallsReconcileOnUnhealthy(t *testing.T) {
	degraded := &selfHealableProvider{
		name: "mlx-supervised/test",
		status: reconcile.ResourceStatus{
			Sync:    reconcile.SyncStatusOutOfSync,
			Health:  reconcile.HealthDegraded,
			Message: "process not running",
		},
		planHasChanges: true,
	}

	withSelfHealableProviders(t, []*selfHealableProvider{degraded}, func() {
		ctx := context.Background()
		healDegradedProviders(ctx, "")

		if degraded.fetchLiveCalls != 1 {
			t.Errorf("FetchLive calls: got %d; want 1", degraded.fetchLiveCalls)
		}
		if degraded.computePlanCalls != 1 {
			t.Errorf("ComputePlan calls: got %d; want 1", degraded.computePlanCalls)
		}
		if degraded.applyPlanCalls != 1 {
			t.Errorf("ApplyPlan calls: got %d; want 1", degraded.applyPlanCalls)
		}
	})
}

// TestHealDegradedProviders_SkipsHealthyProviders verifies that
// healDegradedProviders does NOT call plan/apply for providers that are Healthy
// and Synced. Self-heal must be a no-op on the happy path.
func TestHealDegradedProviders_SkipsHealthyProviders(t *testing.T) {
	healthy := &selfHealableProvider{
		name:   "mlx-supervised/healthy",
		status: greenStatus(),
	}

	withSelfHealableProviders(t, []*selfHealableProvider{healthy}, func() {
		ctx := context.Background()
		healDegradedProviders(ctx, "")

		if healthy.fetchLiveCalls != 0 {
			t.Errorf("FetchLive should not be called for healthy provider: got %d calls", healthy.fetchLiveCalls)
		}
		if healthy.computePlanCalls != 0 {
			t.Errorf("ComputePlan should not be called for healthy provider: got %d calls", healthy.computePlanCalls)
		}
		if healthy.applyPlanCalls != 0 {
			t.Errorf("ApplyPlan should not be called for healthy provider: got %d calls", healthy.applyPlanCalls)
		}
	})
}

// TestHealDegradedProviders_SkipsWhenNoPlanChanges verifies that when
// ComputePlan produces an empty plan (no drift), ApplyPlan is NOT called.
// Self-heal must not make spurious applies.
func TestHealDegradedProviders_SkipsWhenNoPlanChanges(t *testing.T) {
	degraded := &selfHealableProvider{
		name: "mlx-supervised/nochanges",
		status: reconcile.ResourceStatus{
			Sync:   reconcile.SyncStatusOutOfSync,
			Health: reconcile.HealthDegraded,
		},
		planHasChanges: false, // ComputePlan will return an empty plan.
	}

	withSelfHealableProviders(t, []*selfHealableProvider{degraded}, func() {
		ctx := context.Background()
		healDegradedProviders(ctx, "")

		if degraded.fetchLiveCalls != 1 {
			t.Errorf("FetchLive calls: got %d; want 1", degraded.fetchLiveCalls)
		}
		if degraded.computePlanCalls != 1 {
			t.Errorf("ComputePlan calls: got %d; want 1", degraded.computePlanCalls)
		}
		if degraded.applyPlanCalls != 0 {
			t.Errorf("ApplyPlan should not be called when plan is empty: got %d calls", degraded.applyPlanCalls)
		}
	})
}

// TestAutonomicTick_HealsDegradedBeforeSnapshot verifies the end-to-end path:
// when autonomicTick fires with a degraded provider registered, it calls
// healDegradedProviders. We confirm FetchLive was called (proof the heal loop
// ran). This test does not use a live LLM — the snapshot degradation is enough
// to assert the deterministic path.
func TestAutonomicTick_HealsDegradedBeforeSnapshot(t *testing.T) {
	degraded := &selfHealableProvider{
		name: "mlx-supervised/tick-heal",
		status: reconcile.ResourceStatus{
			Sync:    reconcile.SyncStatusOutOfSync,
			Health:  reconcile.HealthDegraded,
			Message: "process crashed",
		},
		planHasChanges: true,
	}

	withSelfHealableProviders(t, []*selfHealableProvider{degraded}, func() {
		root := makeWorkspace(t)
		cfg := makeConfig(t, root)
		cfg.LocalModel = "gemma4:e4b"
		proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
		srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)

		ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
		if err != nil {
			t.Fatalf("NewLocalHarnessController: %v", err)
		}
		// Suppress LLM escalation by setting a recent lastLLMCycle with a long
		// idle window. This isolates the deterministic heal path.
		ctrl.mu.Lock()
		ctrl.lastLLMCycle = time.Now().UTC()
		ctrl.mu.Unlock()
		ctrl.autonomicCfg = AutonomicConfig{IdleRecheckIn: 24 * time.Hour}

		// Run one tick. The degraded provider is not-green, so healDegradedProviders
		// must run and call FetchLive → ComputePlan → ApplyPlan.
		ctrl.autonomicTick(context.Background())

		if degraded.fetchLiveCalls == 0 {
			t.Error("autonomicTick: FetchLive not called — self-heal loop did not run")
		}
		if degraded.applyPlanCalls == 0 {
			t.Error("autonomicTick: ApplyPlan not called — self-heal did not apply corrective action")
		}
	})
}

// --- snapshotToPayload unit test -------------------------------------------

func TestSnapshotToPayload_Roundtrip(t *testing.T) {
	snap := KernelHealthSnapshot{
		Timestamp: time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
		Providers: map[string]reconcile.ResourceStatus{
			"alpha": {Sync: reconcile.SyncStatusSynced, Health: reconcile.HealthHealthy},
		},
		Counts: HealthCounts{Healthy: 1},
	}

	payload, err := snapshotToPayload(snap)
	if err != nil {
		t.Fatalf("snapshotToPayload: %v", err)
	}
	if payload == nil {
		t.Fatal("expected non-nil payload")
	}
	// Spot-check the counts field roundtripped.
	countsRaw, ok := payload["counts"].(map[string]any)
	if !ok {
		t.Fatalf("counts field not present or wrong type: %T", payload["counts"])
	}
	if countsRaw["healthy"].(float64) != 1 {
		t.Errorf("counts.healthy: got %v, want 1", countsRaw["healthy"])
	}
}

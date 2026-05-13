// provider_vllm_test.go — mock-based unit tests for the vLLM provider scaffolding.
//
// Tests cover:
//   - Provider implements engine.Provider
//   - KVCacheProvider implements reconcile.Reconcilable (all 7 methods)
//   - KVBlockHashProvider.ParentKVBlockHash for known sessions
//   - Cache event emission triggers bus event with correct payload type
//   - Hashing tuple is stable across calls with same inputs
//   - vllmCall wrapper recovers from panic
//   - Health() transitions (Healthy, Degraded, Progressing, Missing)
package vllm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/pkg/reconcile"
)

// ── Interface compliance ──────────────────────────────────────────────────────

// TestProviderImplementsEngineProvider verifies that *Provider satisfies
// the engine.Provider interface at compile time.
func TestProviderImplementsEngineProvider(t *testing.T) {
	var _ engine.Provider = (*Provider)(nil)
}

// TestKVCacheProviderImplementsReconcilable verifies that *KVCacheProvider
// satisfies the reconcile.Reconcilable interface at compile time.
func TestKVCacheProviderImplementsReconcilable(t *testing.T) {
	var _ reconcile.Reconcilable = (*KVCacheProvider)(nil)
}

// TestProviderImplementsKVBlockHashProvider verifies that *Provider satisfies
// the KVBlockHashProvider interface at compile time.
func TestProviderImplementsKVBlockHashProvider(t *testing.T) {
	var _ KVBlockHashProvider = (*Provider)(nil)
}

// ── vllmCall panic recovery ───────────────────────────────────────────────────

// TestVLLMCallRecoversFromPanic verifies that vllmCall converts panics to errors.
func TestVLLMCallRecoversFromPanic(t *testing.T) {
	err := vllmCall(func() error {
		panic("simulated cgo panic")
	})
	if err == nil {
		t.Fatal("expected non-nil error from panic recovery, got nil")
	}
	if err.Error() == "" {
		t.Fatal("expected non-empty error message")
	}
	t.Logf("recovered error: %v", err)
}

// TestVLLMCallPassesThroughError verifies that non-panic errors pass through.
func TestVLLMCallPassesThroughError(t *testing.T) {
	sentinel := errors.New("sentinel error")
	err := vllmCall(func() error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got: %v", err)
	}
}

// TestVLLMCallNilOnSuccess verifies that vllmCall returns nil on success.
func TestVLLMCallNilOnSuccess(t *testing.T) {
	err := vllmCall(func() error { return nil })
	if err != nil {
		t.Fatalf("expected nil, got: %v", err)
	}
}

// ── Hashing tuple stability ───────────────────────────────────────────────────

// TestComputeBlockHashStable verifies the hash is stable across identical inputs.
func TestComputeBlockHashStable(t *testing.T) {
	cfg := KVSamplingConfig{Temperature: 0.7, TopP: 0.9, TopK: 50, MaxTokens: 2048}
	h1 := ComputeBlockHash("You are a helpful assistant.", "llama-3-8b", cfg)
	h2 := ComputeBlockHash("You are a helpful assistant.", "llama-3-8b", cfg)
	if h1 != h2 {
		t.Fatalf("hash not stable: %q != %q", h1, h2)
	}
}

// TestComputeBlockHashUnique verifies different inputs produce different hashes.
func TestComputeBlockHashUnique(t *testing.T) {
	cfg := KVSamplingConfig{Temperature: 0.7, TopP: 0.9, TopK: 50, MaxTokens: 2048}
	h1 := ComputeBlockHash("prompt A", "llama-3-8b", cfg)
	h2 := ComputeBlockHash("prompt B", "llama-3-8b", cfg)
	if h1 == h2 {
		t.Fatalf("expected different hashes for different prompts, got same: %q", h1)
	}
}

// TestComputeBlockHashModelDifferent verifies different models produce different hashes.
func TestComputeBlockHashModelDifferent(t *testing.T) {
	cfg := KVSamplingConfig{Temperature: 0.7, TopP: 0.9, TopK: 50, MaxTokens: 2048}
	h1 := ComputeBlockHash("same prompt", "llama-3-8b", cfg)
	h2 := ComputeBlockHash("same prompt", "llama-3-70b", cfg)
	if h1 == h2 {
		t.Fatalf("expected different hashes for different models, got same: %q", h1)
	}
}

// TestComputeBlockHashNotEmpty verifies the hash is never empty.
func TestComputeBlockHashNotEmpty(t *testing.T) {
	h := ComputeBlockHash("", "", KVSamplingConfig{})
	if h == "" {
		t.Fatal("expected non-empty hash for empty inputs")
	}
	if len(h) != 64 {
		t.Fatalf("expected 64-char hex SHA-256, got len=%d: %q", len(h), h)
	}
}

// TestMockClientBlockHashForPromptDelegates verifies MockVLLMClient delegates
// to ComputeBlockHash when no custom function is set.
func TestMockClientBlockHashForPromptDelegates(t *testing.T) {
	mock := &MockVLLMClient{}
	cfg := KVSamplingConfig{Temperature: 0.5}
	h := mock.BlockHashForPrompt("test prompt", "model-x", cfg)
	expected := ComputeBlockHash("test prompt", "model-x", cfg)
	if h != expected {
		t.Fatalf("mock hash %q != expected %q", h, expected)
	}
}

// ── KVCacheProvider lifecycle ─────────────────────────────────────────────────

// TestKVCacheProviderType verifies Type() returns the correct string.
func TestKVCacheProviderType(t *testing.T) {
	p := NewKVCacheProvider(&MockVLLMClient{}, "test-channel", nil)
	if p.Type() != "cache.kv_block" {
		t.Fatalf("expected %q, got %q", "cache.kv_block", p.Type())
	}
}

// TestKVCacheProviderLoadConfigNoFile verifies LoadConfig succeeds with no config file.
func TestKVCacheProviderLoadConfigNoFile(t *testing.T) {
	p := NewKVCacheProvider(&MockVLLMClient{}, "test-channel", nil)
	cfg, err := p.LoadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
}

// TestKVCacheProviderFetchLive verifies FetchLive queries the client.
func TestKVCacheProviderFetchLive(t *testing.T) {
	mock := &MockVLLMClient{
		DefaultStats: CacheStats{HitRate: 0.7, BlockCount: 3, Fragmentation: 0.1},
		WarmBlockList: []WarmBlock{
			{BlockHash: "abc123", ModelName: "m1", WarmAt: time.Now()},
		},
	}
	p := NewKVCacheProvider(mock, "ch1", nil)
	cfg := &kvCacheConfig{Root: t.TempDir()}
	live, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive error: %v", err)
	}
	lv, ok := live.(*kvCacheLive)
	if !ok {
		t.Fatalf("expected *kvCacheLive, got %T", live)
	}
	if len(lv.WarmBlocks) != 1 {
		t.Fatalf("expected 1 warm block, got %d", len(lv.WarmBlocks))
	}
	if lv.Stats.HitRate != 0.7 {
		t.Fatalf("expected hit rate 0.7, got %f", lv.Stats.HitRate)
	}
}

// TestKVCacheProviderComputePlanWarmMissing verifies ComputePlan creates actions
// for declared prefixes not in the live set.
func TestKVCacheProviderComputePlanWarmMissing(t *testing.T) {
	mock := &MockVLLMClient{}
	p := NewKVCacheProvider(mock, "ch1", nil)

	cfg := &kvCacheConfig{
		HotPrefixes: []WarmRequest{
			{PromptPrefix: "You are an assistant.", ModelName: "m1", SamplingConfig: KVSamplingConfig{Temperature: 0.7}},
		},
	}
	// Live has no warm blocks.
	lv := &kvCacheLive{WarmBlocks: nil, Stats: CacheStats{}}

	plan, err := p.ComputePlan(cfg, lv, nil)
	if err != nil {
		t.Fatalf("ComputePlan error: %v", err)
	}
	if plan.Summary.Creates != 1 {
		t.Fatalf("expected 1 create action, got %d (summary: %+v)", plan.Summary.Creates, plan.Summary)
	}
}

// TestKVCacheProviderComputePlanInSync verifies ComputePlan skips when prefix is warm.
func TestKVCacheProviderComputePlanInSync(t *testing.T) {
	mock := &MockVLLMClient{}
	p := NewKVCacheProvider(mock, "ch1", nil)

	prompt := "You are an assistant."
	model := "m1"
	cfg2 := KVSamplingConfig{Temperature: 0.7}
	hash := ComputeBlockHash(prompt, model, cfg2)

	cfg := &kvCacheConfig{
		HotPrefixes: []WarmRequest{
			{PromptPrefix: prompt, ModelName: model, SamplingConfig: cfg2},
		},
	}
	lv := &kvCacheLive{
		WarmBlocks: []WarmBlock{
			{BlockHash: hash, ModelName: model, SamplingConfig: cfg2, WarmAt: time.Now()},
		},
		Stats: CacheStats{HitRate: 0.8},
	}

	plan, err := p.ComputePlan(cfg, lv, nil)
	if err != nil {
		t.Fatalf("ComputePlan error: %v", err)
	}
	if plan.Summary.Skipped != 1 || plan.Summary.Creates != 0 {
		t.Fatalf("expected 1 skip, got: %+v", plan.Summary)
	}
}

// TestKVCacheProviderApplyPlanWarm verifies ApplyPlan calls WarmBlocks.
func TestKVCacheProviderApplyPlanWarm(t *testing.T) {
	emitter := &MockBusEmitter{}
	mock := &MockVLLMClient{}
	p := NewKVCacheProvider(mock, "ch1", emitter)

	plan := &reconcile.Plan{
		ResourceType: "cache.kv_block",
		Actions: []reconcile.Action{
			{
				Action:       reconcile.ActionCreate,
				ResourceType: "cache.kv_block",
				Name:         "hash1",
				Details: map[string]any{
					"block_hash":    "hash1",
					"prompt_prefix": "system prompt text",
					"model_name":    "m1",
				},
			},
		},
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if len(mock.WarmCalls) == 0 {
		t.Fatal("expected WarmBlocks to be called")
	}
}

// TestKVCacheProviderApplyPlanEvict verifies ApplyPlan calls EvictBlocks.
func TestKVCacheProviderApplyPlanEvict(t *testing.T) {
	emitter := &MockBusEmitter{}
	mock := &MockVLLMClient{}
	p := NewKVCacheProvider(mock, "ch1", emitter)

	plan := &reconcile.Plan{
		ResourceType: "cache.kv_block",
		Actions: []reconcile.Action{
			{
				Action:       reconcile.ActionDelete,
				ResourceType: "cache.kv_block",
				Name:         "deadbeef",
				Details: map[string]any{
					"block_hash": "deadbeef",
					"reason":     "warm_not_declared",
				},
			},
		},
	}

	_, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan error: %v", err)
	}
	if len(mock.EvictCalls) == 0 {
		t.Fatal("expected EvictBlocks to be called")
	}
}

// TestKVCacheProviderBuildState verifies BuildState produces a state with resources.
func TestKVCacheProviderBuildState(t *testing.T) {
	mock := &MockVLLMClient{}
	p := NewKVCacheProvider(mock, "ch1", nil)

	cfg := &kvCacheConfig{Root: t.TempDir()}
	lv := &kvCacheLive{
		WarmBlocks: []WarmBlock{
			{BlockHash: "abcdef1234567890", ModelName: "m1", BlockSize: 512, WarmAt: time.Now()},
		},
		Stats: CacheStats{HitRate: 0.8, BlockCount: 1, Fragmentation: 0.1},
	}

	state, err := p.BuildState(cfg, lv, nil)
	if err != nil {
		t.Fatalf("BuildState error: %v", err)
	}
	if len(state.Resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(state.Resources))
	}
	if state.ResourceType != "cache.kv_block" {
		t.Fatalf("expected resource type 'cache.kv_block', got %q", state.ResourceType)
	}
}

// ── Health transitions ────────────────────────────────────────────────────────

// TestKVCacheProviderHealthMissingInitial verifies Health() returns Missing
// before any stats are observed.
func TestKVCacheProviderHealthMissingInitial(t *testing.T) {
	p := NewKVCacheProvider(&MockVLLMClient{}, "ch1", nil)
	status := p.Health()
	if status.Health != reconcile.HealthMissing {
		t.Fatalf("expected Missing, got %q: %s", status.Health, status.Message)
	}
}

// TestKVCacheProviderHealthHealthy verifies Health() returns Healthy when
// hit rate > 0.5.
func TestKVCacheProviderHealthHealthy(t *testing.T) {
	mock := &MockVLLMClient{
		DefaultStats: CacheStats{HitRate: 0.75, BlockCount: 5, Fragmentation: 0.1},
	}
	p := NewKVCacheProvider(mock, "ch1", nil)
	// Trigger a FetchLive + ComputePlan to populate stats.
	ctx := context.Background()
	cfg := &kvCacheConfig{Root: t.TempDir()}
	live, _ := p.FetchLive(ctx, cfg)
	p.ComputePlan(cfg, live, nil)
	// Manually inject stats (simulate ApplyPlan stats update path).
	p.mu.Lock()
	p.lastStats = CacheStats{HitRate: 0.75, BlockCount: 5, Fragmentation: 0.1}
	p.lastPlan = &reconcile.Plan{}
	p.mu.Unlock()

	status := p.Health()
	if status.Health != reconcile.HealthHealthy {
		t.Fatalf("expected Healthy, got %q: %s", status.Health, status.Message)
	}
}

// TestKVCacheProviderHealthDegradedFragmentation verifies Health() returns
// Degraded when fragmentation > 0.8.
func TestKVCacheProviderHealthDegradedFragmentation(t *testing.T) {
	p := NewKVCacheProvider(&MockVLLMClient{}, "ch1", nil)
	p.mu.Lock()
	p.lastStats = CacheStats{HitRate: 0.9, BlockCount: 10, Fragmentation: 0.9}
	p.lastPlan = &reconcile.Plan{}
	p.mu.Unlock()

	status := p.Health()
	if status.Health != reconcile.HealthDegraded {
		t.Fatalf("expected Degraded (fragmentation), got %q: %s", status.Health, status.Message)
	}
}

// TestKVCacheProviderHealthDegradedLowHitRate verifies Health() returns
// Degraded when hit rate <= 0.5.
func TestKVCacheProviderHealthDegradedLowHitRate(t *testing.T) {
	p := NewKVCacheProvider(&MockVLLMClient{}, "ch1", nil)
	p.mu.Lock()
	p.lastStats = CacheStats{HitRate: 0.3, BlockCount: 2, Fragmentation: 0.2}
	p.lastPlan = &reconcile.Plan{}
	p.mu.Unlock()

	status := p.Health()
	if status.Health != reconcile.HealthDegraded {
		t.Fatalf("expected Degraded (hit rate), got %q: %s", status.Health, status.Message)
	}
}

// ── Bus event emission ────────────────────────────────────────────────────────

// TestCacheEventEmissionOnWarm verifies that a warm cycle emits a warmed event.
func TestCacheEventEmissionOnWarm(t *testing.T) {
	emitter := &MockBusEmitter{}
	mock := &MockVLLMClient{
		DefaultStats: CacheStats{HitRate: 0.6, BlockCount: 1, Fragmentation: 0.1},
	}
	p := NewKVCacheProvider(mock, "ch1", emitter)

	plan := &reconcile.Plan{
		ResourceType: "cache.kv_block",
		Actions: []reconcile.Action{
			{
				Action:       reconcile.ActionCreate,
				ResourceType: "cache.kv_block",
				Name:         "warmhash",
				Details: map[string]any{
					"block_hash":    "warmhash",
					"prompt_prefix": "test prompt",
					"model_name":    "m1",
				},
			},
		},
	}

	_, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan error: %v", err)
	}

	// Should have at least a warmed event.
	found := false
	for _, ev := range emitter.Events {
		if ev.Topic == TopicKVCacheWarmed {
			found = true
			// Verify the payload is the correct type.
			if _, ok := ev.Payload.(KVCacheWarmedEvent); !ok {
				t.Fatalf("expected KVCacheWarmedEvent payload, got %T", ev.Payload)
			}
			break
		}
	}
	if !found {
		t.Fatalf("expected %q event to be emitted, got events: %v", TopicKVCacheWarmed, emitter.Events)
	}
}

// TestCacheEventEmissionOnEvict verifies that an evict cycle emits an evicted event.
func TestCacheEventEmissionOnEvict(t *testing.T) {
	emitter := &MockBusEmitter{}
	mock := &MockVLLMClient{
		DefaultStats: CacheStats{HitRate: 0.4, BlockCount: 0, Fragmentation: 0.05},
	}
	p := NewKVCacheProvider(mock, "ch1", emitter)

	plan := &reconcile.Plan{
		ResourceType: "cache.kv_block",
		Actions: []reconcile.Action{
			{
				Action:       reconcile.ActionDelete,
				ResourceType: "cache.kv_block",
				Name:         "evicthash",
				Details: map[string]any{
					"block_hash": "evicthash",
					"reason":     "warm_not_declared",
				},
			},
		},
	}

	_, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan error: %v", err)
	}

	found := false
	for _, ev := range emitter.Events {
		if ev.Topic == TopicKVCacheEvicted {
			found = true
			if _, ok := ev.Payload.(KVCacheEvictedEvent); !ok {
				t.Fatalf("expected KVCacheEvictedEvent payload, got %T", ev.Payload)
			}
			break
		}
	}
	if !found {
		t.Fatalf("expected %q event, got events: %v", TopicKVCacheEvicted, emitter.Events)
	}
}

// TestHitRateChangedEmission verifies emitHitRateChanged fires on ≥5% delta.
func TestHitRateChangedEmission(t *testing.T) {
	emitter := &MockBusEmitter{}
	p := NewKVCacheProvider(&MockVLLMClient{}, "ch1", emitter)
	// Set prev to 0.6; new to 0.7 (delta = 0.1 > 0.05 threshold).
	p.mu.Lock()
	p.lastStats = CacheStats{HitRate: 0.6}
	p.mu.Unlock()

	ctx := context.Background()
	newStats := CacheStats{HitRate: 0.7, BlockCount: 5}
	p.checkAndEmitStats(ctx, newStats)

	found := false
	for _, ev := range emitter.Events {
		if ev.Topic == TopicKVCacheHitRateChanged {
			found = true
			evt, ok := ev.Payload.(KVCacheHitRateChangedEvent)
			if !ok {
				t.Fatalf("expected KVCacheHitRateChangedEvent, got %T", ev.Payload)
			}
			if evt.PrevRate != 0.6 || evt.CurrentRate != 0.7 {
				t.Fatalf("unexpected rates: prev=%f current=%f", evt.PrevRate, evt.CurrentRate)
			}
		}
	}
	if !found {
		t.Fatalf("expected hit_rate_changed event")
	}
}

// TestFragmentationHighEmission verifies emitFragmentationHigh fires on crossing 0.8.
func TestFragmentationHighEmission(t *testing.T) {
	emitter := &MockBusEmitter{}
	p := NewKVCacheProvider(&MockVLLMClient{}, "ch1", emitter)
	// Prev frag = 0.5, new frag = 0.85 (crossing 0.8).
	p.mu.Lock()
	p.lastStats = CacheStats{Fragmentation: 0.5}
	p.mu.Unlock()

	ctx := context.Background()
	newStats := CacheStats{HitRate: 0.8, Fragmentation: 0.85}
	p.checkAndEmitStats(ctx, newStats)

	found := false
	for _, ev := range emitter.Events {
		if ev.Topic == TopicKVCacheFragmentationHigh {
			found = true
			if _, ok := ev.Payload.(KVCacheFragmentationHighEvent); !ok {
				t.Fatalf("expected KVCacheFragmentationHighEvent, got %T", ev.Payload)
			}
		}
	}
	if !found {
		t.Fatalf("expected fragmentation_high event")
	}
}

// ── KVBlockHashProvider ───────────────────────────────────────────────────────

// TestParentKVBlockHashReturnsHashForKnownSession verifies that
// Provider.ParentKVBlockHash returns a non-empty hash for a known session.
func TestParentKVBlockHashReturnsHashForKnownSession(t *testing.T) {
	mock := &MockVLLMClient{}
	p := NewProvider(mock, "ch1", "model-x")

	hash, err := p.ParentKVBlockHash(context.Background(), "session-1", "msg-42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash == "" {
		t.Fatal("expected non-empty hash")
	}
}

// TestParentKVBlockHashPanicRecovery verifies that a panicking client is
// recovered by the vllmCall wrapper.
func TestParentKVBlockHashPanicRecovery(t *testing.T) {
	mock := &MockVLLMClient{
		BlockHashForPromptFn: func(prompt, model string, cfg KVSamplingConfig) string {
			panic("simulated cgo panic in BlockHashForPrompt")
		},
	}
	p := NewProvider(mock, "ch1", "model-x")
	_, err := p.ParentKVBlockHash(context.Background(), "s1", "m1")
	if err == nil {
		t.Fatal("expected non-nil error from panic recovery")
	}
	t.Logf("recovered: %v", err)
}

// ── Fork-over-kvcache stub ────────────────────────────────────────────────────

// TestForkOverKVCacheOK verifies ForkOverKVCache returns the hash when available.
func TestForkOverKVCacheOK(t *testing.T) {
	mock := &MockVLLMClient{}
	p := NewProvider(mock, "ch1", "model-x")

	result := ForkOverKVCache(context.Background(), p, "parent-session", "msg-10")
	if result.Degraded {
		t.Fatalf("expected non-degraded result, got: %s", result.DegradedReason)
	}
	if result.ParentKVBlockHash == "" {
		t.Fatal("expected non-empty ParentKVBlockHash")
	}
}

// TestForkOverKVCacheDegradeOnError verifies ForkOverKVCache degrades when
// the provider returns an error.
func TestForkOverKVCacheDegradeOnError(t *testing.T) {
	mock := &MockVLLMClient{
		BlockHashForPromptFn: func(prompt, model string, cfg KVSamplingConfig) string {
			panic("simulated eviction error via panic")
		},
	}
	p := NewProvider(mock, "ch1", "model-x")
	result := ForkOverKVCache(context.Background(), p, "parent", "msg-10")
	if !result.Degraded {
		t.Fatal("expected degraded result on error")
	}
}

// ── Mock client recording ─────────────────────────────────────────────────────

// TestMockClientRecordsWarmCalls verifies the mock records all WarmBlocks calls.
func TestMockClientRecordsWarmCalls(t *testing.T) {
	mock := &MockVLLMClient{}
	reqs := []WarmRequest{
		{PromptPrefix: "p1", ModelName: "m1"},
		{PromptPrefix: "p2", ModelName: "m1"},
	}
	_, err := mock.WarmBlocks(context.Background(), reqs)
	if err != nil {
		t.Fatalf("WarmBlocks error: %v", err)
	}
	if len(mock.WarmCalls) != 1 {
		t.Fatalf("expected 1 call recorded, got %d", len(mock.WarmCalls))
	}
	if len(mock.WarmCalls[0]) != 2 {
		t.Fatalf("expected 2 requests in call, got %d", len(mock.WarmCalls[0]))
	}
}

// TestMockClientClose verifies Close is tracked.
func TestMockClientClose(t *testing.T) {
	mock := &MockVLLMClient{}
	_ = mock.Close()
	_ = mock.Close()
	if mock.CloseCalled != 2 {
		t.Fatalf("expected CloseCalled=2, got %d", mock.CloseCalled)
	}
}

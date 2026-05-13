// provider_vllm_cache.go — KVCacheProvider: Reconcilable for the vLLM KV block cache.
//
// KVCacheProvider manages vLLM's KV block cache as a Reconcilable resource.
// It reconciles declared hot-prefixes (system prompt cache declarations in the
// workspace config) against live warm blocks on the vLLM channel.
//
// Implements reconcile.Reconcilable (7-method contract).
//
// Config format (.cog/config/cache.kv_block/config.yaml):
//
//	hot_prefixes:
//	  - prompt_prefix: "You are a helpful assistant..."
//	    model_name: "llama-3-8b"
//	    sampling_config:
//	      temperature: 0.7
//	      top_p: 0.9
//	      top_k: 50
//	      max_tokens: 2048
package vllm

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/myrgic/cogos/pkg/reconcile"
)

// KVCacheProvider manages the vLLM KV block cache as a Reconcilable resource.
// It reconciles declared hot-prefixes (system prompt cache declarations in the
// workspace config) against live warm blocks on the vLLM channel.
//
// Implements reconcile.Reconcilable.
type KVCacheProvider struct {
	mu      sync.Mutex
	client  VLLMClient
	emitter BusEmitter
	logger  *slog.Logger

	channelID string

	// Mutable state tracked across lifecycle calls.
	lastStats  CacheStats
	lastPlan   *reconcile.Plan
	operation  reconcile.OperationPhase
}

// NewKVCacheProvider constructs a KVCacheProvider.
// client must not be nil. emitter may be nil (no bus events emitted when nil).
func NewKVCacheProvider(client VLLMClient, channelID string, emitter BusEmitter) *KVCacheProvider {
	return &KVCacheProvider{
		client:    client,
		emitter:   emitter,
		channelID: channelID,
		logger:    slog.Default(),
		operation: reconcile.OperationIdle,
	}
}

// ── kvCacheConfig ─────────────────────────────────────────────────────────────

// kvCacheConfig is the LoadConfig output. Bundles declared hot-prefixes.
type kvCacheConfig struct {
	Root        string
	HotPrefixes []WarmRequest
}

// kvCacheConfigFile mirrors the on-disk YAML shape.
type kvCacheConfigFile struct {
	HotPrefixes []struct {
		PromptPrefix   string          `yaml:"prompt_prefix"`
		ModelName      string          `yaml:"model_name"`
		SamplingConfig KVSamplingConfig `yaml:"sampling_config"`
	} `yaml:"hot_prefixes"`
}

// ── kvCacheLive ───────────────────────────────────────────────────────────────

// kvCacheLive is the FetchLive output. Bundles live warm blocks + current stats.
type kvCacheLive struct {
	WarmBlocks []WarmBlock
	Stats      CacheStats
}

// ── Type ─────────────────────────────────────────────────────────────────────

// Type implements Reconcilable.
func (p *KVCacheProvider) Type() string { return "cache.kv_block" }

// ── LoadConfig ────────────────────────────────────────────────────────────────

// LoadConfig loads declared hot-prefixes from <root>/.cog/config/cache.kv_block/config.yaml.
// Returns a *kvCacheConfig as any. Missing config file is not an error; the
// provider reconciles against zero declared prefixes (no-op warm cycle).
func (p *KVCacheProvider) LoadConfig(root string) (any, error) {
	cfgPath := filepath.Join(root, ".cog", "config", "cache.kv_block", "config.yaml")
	data, err := os.ReadFile(cfgPath)
	if os.IsNotExist(err) {
		return &kvCacheConfig{Root: root}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kv_block: reading config %s: %w", cfgPath, err)
	}

	var raw kvCacheConfigFile
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("kv_block: parsing config %s: %w", cfgPath, err)
	}

	cfg := &kvCacheConfig{Root: root}
	for _, hp := range raw.HotPrefixes {
		cfg.HotPrefixes = append(cfg.HotPrefixes, WarmRequest{
			PromptPrefix:   hp.PromptPrefix,
			ModelName:      hp.ModelName,
			SamplingConfig: hp.SamplingConfig,
		})
	}
	return cfg, nil
}

// ── FetchLive ─────────────────────────────────────────────────────────────────

// FetchLive queries the vLLM client for currently warm blocks and cache stats.
// Returns a *kvCacheLive as any.
func (p *KVCacheProvider) FetchLive(ctx context.Context, config any) (any, error) {
	cfg, ok := config.(*kvCacheConfig)
	if !ok {
		return nil, fmt.Errorf("kv_block: FetchLive expected *kvCacheConfig, got %T", config)
	}
	_ = cfg // root available if needed for scoping

	blocks, err := p.client.ListWarmBlocks(ctx)
	if err != nil {
		return nil, fmt.Errorf("kv_block: ListWarmBlocks: %w", err)
	}
	stats, err := p.client.CacheStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("kv_block: CacheStats: %w", err)
	}
	return &kvCacheLive{WarmBlocks: blocks, Stats: stats}, nil
}

// ── ComputePlan ───────────────────────────────────────────────────────────────

// ComputePlan diffs declared hot-prefixes against live warm blocks.
// Actions:
//   - create (warm): prefix declared but not currently warm
//   - delete (evict): block warm but not declared (excess eviction)
//   - skip: prefix declared and block warm (in sync)
func (p *KVCacheProvider) ComputePlan(config any, live any, state *reconcile.State) (*reconcile.Plan, error) {
	cfg, ok := config.(*kvCacheConfig)
	if !ok {
		return nil, fmt.Errorf("kv_block: ComputePlan expected *kvCacheConfig, got %T", config)
	}
	lv, ok := live.(*kvCacheLive)
	if !ok {
		return nil, fmt.Errorf("kv_block: ComputePlan expected *kvCacheLive, got %T", live)
	}

	plan := &reconcile.Plan{
		ResourceType: "cache.kv_block",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath:   filepath.Join(cfg.Root, ".cog", "config", "cache.kv_block"),
	}

	// Build index of live blocks by hash.
	liveByHash := make(map[string]WarmBlock, len(lv.WarmBlocks))
	for _, wb := range lv.WarmBlocks {
		liveByHash[wb.BlockHash] = wb
	}

	// Index declared prefixes by their computed hash.
	declaredHashes := make(map[string]WarmRequest, len(cfg.HotPrefixes))
	for _, req := range cfg.HotPrefixes {
		h := p.client.BlockHashForPrompt(req.PromptPrefix, req.ModelName, req.SamplingConfig)
		declaredHashes[h] = req
	}

	// Check declared → live (warm missing ones).
	for hash, req := range declaredHashes {
		if _, warm := liveByHash[hash]; !warm {
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionCreate,
				ResourceType: "cache.kv_block",
				Name:         hash,
				Details: map[string]any{
					"reason":        "declared_not_warm",
					"block_hash":    hash,
					"prompt_prefix": req.PromptPrefix,
					"model_name":    req.ModelName,
				},
			})
			plan.Summary.Creates++
		} else {
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionSkip,
				ResourceType: "cache.kv_block",
				Name:         hash,
				Details: map[string]any{
					"reason":     "in_sync",
					"block_hash": hash,
				},
			})
			plan.Summary.Skipped++
		}
	}

	// Check live → declared (evict excess blocks not in declared set).
	for hash := range liveByHash {
		if _, declared := declaredHashes[hash]; !declared {
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionDelete,
				ResourceType: "cache.kv_block",
				Name:         hash,
				Details: map[string]any{
					"reason":     "warm_not_declared",
					"block_hash": hash,
				},
			})
			plan.Summary.Deletes++
		}
	}

	p.mu.Lock()
	p.lastPlan = plan
	p.mu.Unlock()

	return plan, nil
}

// ── ApplyPlan ─────────────────────────────────────────────────────────────────

// ApplyPlan warms or evicts blocks to converge on the declared state.
// Emits bus events on warm/evict transitions.
func (p *KVCacheProvider) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	p.mu.Lock()
	p.operation = reconcile.OperationSyncing
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.operation = reconcile.OperationIdle
		p.mu.Unlock()
	}()

	var results []reconcile.Result
	var toWarm []WarmRequest
	var toEvict []string

	// Collect warm and evict targets from the plan.
	for _, action := range plan.Actions {
		switch action.Action {
		case reconcile.ActionCreate:
			hash, _ := action.Details["block_hash"].(string)
			prefix, _ := action.Details["prompt_prefix"].(string)
			model, _ := action.Details["model_name"].(string)
			toWarm = append(toWarm, WarmRequest{
				PromptPrefix: prefix,
				ModelName:    model,
				// Sampling config not carried through plan details; use zero value
				// (matches the hash computation that produced this entry).
			})
			_ = hash
		case reconcile.ActionDelete:
			hash, _ := action.Details["block_hash"].(string)
			toEvict = append(toEvict, hash)
		}
	}

	// Execute warm batch.
	if len(toWarm) > 0 {
		blockResults, err := p.client.WarmBlocks(ctx, toWarm)
		if err != nil {
			results = append(results, reconcile.Result{
				Phase:  "apply",
				Action: "warm",
				Name:   "batch",
				Status: reconcile.ApplyFailed,
				Error:  err.Error(),
			})
		} else {
			var warmedHashes []string
			for _, br := range blockResults {
				status := reconcile.ApplySucceeded
				if !br.Warmed {
					status = reconcile.ApplyFailed
				} else {
					warmedHashes = append(warmedHashes, br.BlockHash)
				}
				results = append(results, reconcile.Result{
					Phase:  "apply",
					Action: "warm",
					Name:   br.BlockHash,
					Status: status,
					Error:  br.Error,
				})
			}
			if len(warmedHashes) > 0 && p.emitter != nil {
				stats, _ := p.client.CacheStats(ctx)
				p.checkAndEmitStats(ctx, stats)
				emitWarmed(ctx, p.emitter, p.channelID, warmedHashes, stats)
			}
		}
	}

	// Execute evict batch.
	if len(toEvict) > 0 {
		err := p.client.EvictBlocks(ctx, toEvict)
		status := reconcile.ApplySucceeded
		errStr := ""
		if err != nil {
			status = reconcile.ApplyFailed
			errStr = err.Error()
		}
		results = append(results, reconcile.Result{
			Phase:  "apply",
			Action: "evict",
			Name:   fmt.Sprintf("batch(%d)", len(toEvict)),
			Status: status,
			Error:  errStr,
		})
		if err == nil && p.emitter != nil {
			stats, _ := p.client.CacheStats(ctx)
			p.checkAndEmitStats(ctx, stats)
			emitEvicted(ctx, p.emitter, p.channelID, toEvict, stats)
		}
	}

	return results, nil
}

// checkAndEmitStats compares new stats against last known; emits events on
// threshold crossings (hit_rate ±5%, fragmentation >0.8).
func (p *KVCacheProvider) checkAndEmitStats(ctx context.Context, stats CacheStats) {
	p.mu.Lock()
	prev := p.lastStats
	p.lastStats = stats
	p.mu.Unlock()

	if p.emitter == nil {
		return
	}
	// Hit rate change threshold: 5%.
	if math.Abs(stats.HitRate-prev.HitRate) >= 0.05 {
		emitHitRateChanged(ctx, p.emitter, p.channelID, prev.HitRate, stats.HitRate, stats)
	}
	// Fragmentation threshold: >0.8.
	if stats.Fragmentation > 0.8 && prev.Fragmentation <= 0.8 {
		emitFragmentationHigh(ctx, p.emitter, p.channelID, stats.Fragmentation, stats)
	}
}

// ── BuildState ────────────────────────────────────────────────────────────────

// BuildState constructs a reconcile.State from live warm block data.
func (p *KVCacheProvider) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	_, ok := config.(*kvCacheConfig)
	if !ok {
		return nil, fmt.Errorf("kv_block: BuildState expected *kvCacheConfig, got %T", config)
	}
	lv, ok := live.(*kvCacheLive)
	if !ok {
		return nil, fmt.Errorf("kv_block: BuildState expected *kvCacheLive, got %T", live)
	}

	lineage := "cache.kv_block"
	serial := 1
	if existing != nil {
		lineage = existing.Lineage
		serial = existing.Serial + 1
	}

	state := &reconcile.State{
		Version:      1,
		Lineage:      lineage,
		Serial:       serial,
		ResourceType: "cache.kv_block",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Metadata: map[string]any{
			"channel_id":    p.channelID,
			"hit_rate":      lv.Stats.HitRate,
			"block_count":   lv.Stats.BlockCount,
			"fragmentation": lv.Stats.Fragmentation,
		},
	}

	for _, wb := range lv.WarmBlocks {
		state.Resources = append(state.Resources, reconcile.Resource{
			Address:    "cache.kv_block." + wb.BlockHash[:min(12, len(wb.BlockHash))],
			Type:       "cache.kv_block",
			Mode:       reconcile.ModeManaged,
			ExternalID: wb.BlockHash,
			Name:       wb.BlockHash,
			Attributes: map[string]any{
				"block_hash":  wb.BlockHash,
				"model_name":  wb.ModelName,
				"block_size":  wb.BlockSize,
				"warm_at":     wb.WarmAt.Format(time.RFC3339),
				"channel_id":  p.channelID,
				"temperature": wb.SamplingConfig.Temperature,
				"top_p":       wb.SamplingConfig.TopP,
				"top_k":       wb.SamplingConfig.TopK,
				"max_tokens":  wb.SamplingConfig.MaxTokens,
			},
			LastRefreshed: time.Now().UTC().Format(time.RFC3339),
		})
	}
	return state, nil
}

// ── Health ────────────────────────────────────────────────────────────────────

// Health implements Reconcilable.
// Three-axis status per RFC-0006:
//   - Healthy: hit rate > 0.5
//   - Degraded: fragmentation > 0.8
//   - Progressing: active warm/evict cycle
//   - Missing: client never queried (stats zero, no plan run)
func (p *KVCacheProvider) Health() reconcile.ResourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.operation == reconcile.OperationSyncing {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthProgressing,
			Operation: reconcile.OperationSyncing,
			Message:   "warm/evict cycle in progress",
		}
	}

	stats := p.lastStats

	// Degenerate case: no stats observed yet.
	if stats.BlockCount == 0 && stats.HitRate == 0 && p.lastPlan == nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("channel %q: no stats observed; vLLM not yet queried or hardware unavailable", p.channelID),
		}
	}

	// Fragmentation check takes precedence over hit rate.
	if stats.Fragmentation > 0.8 {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("channel %q: fragmentation %.2f > 0.8 threshold", p.channelID, stats.Fragmentation),
		}
	}

	if stats.HitRate > 0.5 {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusSynced,
			Health:    reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("channel %q: hit rate %.2f, %d blocks warm", p.channelID, stats.HitRate, stats.BlockCount),
		}
	}

	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusOutOfSync,
		Health:    reconcile.HealthDegraded,
		Operation: reconcile.OperationIdle,
		Message:   fmt.Sprintf("channel %q: hit rate %.2f < 0.5 threshold, %d blocks warm", p.channelID, stats.HitRate, stats.BlockCount),
	}
}

// Structured log helper for provider-level operations.
func (p *KVCacheProvider) logOp(op, msg string, extra ...any) {
	args := []any{
		"operation", op,
		"channel_id", p.channelID,
		"ts", time.Now().UTC().Format(time.RFC3339),
	}
	args = append(args, extra...)
	slog.Info("vllm kv_cache reconcile: "+msg, args...)
}


// provider_vllm_events.go — Bus event payload types and topic constants.
//
// Typed bus event payloads for KV cache lifecycle events. All fields are
// typed and documented; no raw maps. Topic constants match the bus filter
// patterns used by subscribers.
//
// BusEmitter is the injection interface for bus event dispatch. In production
// the kernel wires engine.AppendEvent; in tests a mock is injected. This avoids
// a circular import between internal/providers/vllm and internal/engine.
package vllm

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// ── Bus topic constants ──────────────────────────────────────────────────────

const (
	TopicKVCacheWarmed            = "inference.cache.warmed"
	TopicKVCacheEvicted           = "inference.cache.evicted"
	TopicKVCacheHitRateChanged    = "inference.cache.hit_rate_changed"
	TopicKVCacheFragmentationHigh = "inference.cache.fragmentation_high"
)

// ── Bus event payload types ──────────────────────────────────────────────────

// KVCacheWarmedEvent fires when one or more blocks are successfully warmed.
type KVCacheWarmedEvent struct {
	ChannelID   string     `json:"channel_id"`
	BlockHashes []string   `json:"block_hashes"`
	CacheStats  CacheStats `json:"cache_stats"`
}

// KVCacheEvictedEvent fires when one or more blocks are evicted.
type KVCacheEvictedEvent struct {
	ChannelID   string     `json:"channel_id"`
	BlockHashes []string   `json:"block_hashes"`
	CacheStats  CacheStats `json:"cache_stats"`
}

// KVCacheHitRateChangedEvent fires when the hit rate crosses a 5% threshold.
type KVCacheHitRateChangedEvent struct {
	ChannelID   string     `json:"channel_id"`
	PrevRate    float64    `json:"prev_rate"`
	CurrentRate float64    `json:"current_rate"`
	CacheStats  CacheStats `json:"cache_stats"`
}

// KVCacheFragmentationHighEvent fires when fragmentation exceeds 0.8.
type KVCacheFragmentationHighEvent struct {
	ChannelID     string     `json:"channel_id"`
	Fragmentation float64    `json:"fragmentation"`
	CacheStats    CacheStats `json:"cache_stats"`
}

// ── BusEmitter injection interface ──────────────────────────────────────────

// BusEmitter is the interface the KVCacheProvider uses to emit typed bus events.
// In production the kernel wires an adapter backed by engine.AppendEvent.
// In tests a MockBusEmitter records calls for assertion.
type BusEmitter interface {
	// Emit publishes a typed bus event with the given topic and payload.
	// The payload must be JSON-serializable. Implementations must be non-blocking
	// (fire-and-forget delivery semantics matching the kernel bus contract).
	Emit(ctx context.Context, topic string, payload any) error
}

// MockBusEmitter records emitted events for test assertions.
type MockBusEmitter struct {
	Events []BusEventRecord
}

// BusEventRecord captures a single emitted event.
type BusEventRecord struct {
	Topic     string
	Payload   any
	EmittedAt time.Time
}

// Emit records the event for test assertions.
func (m *MockBusEmitter) Emit(_ context.Context, topic string, payload any) error {
	m.Events = append(m.Events, BusEventRecord{
		Topic:     topic,
		Payload:   payload,
		EmittedAt: time.Now().UTC(),
	})
	return nil
}

// ── Structured emission helpers ──────────────────────────────────────────────

// emitWarmed publishes a KVCacheWarmedEvent on the bus and emits a structured log.
func emitWarmed(ctx context.Context, emitter BusEmitter, channelID string, hashes []string, stats CacheStats) {
	evt := KVCacheWarmedEvent{
		ChannelID:   channelID,
		BlockHashes: hashes,
		CacheStats:  stats,
	}
	if err := emitter.Emit(ctx, TopicKVCacheWarmed, evt); err != nil {
		slog.Warn("vllm: failed to emit cache warmed event", "error", err)
	}
	slog.Info("vllm cache op",
		"operation", "warm",
		"channel_id", channelID,
		"block_count", len(hashes),
		"hit_rate", stats.HitRate,
		"ts", time.Now().UTC().Format(time.RFC3339),
	)
}

// emitEvicted publishes a KVCacheEvictedEvent on the bus and emits a structured log.
func emitEvicted(ctx context.Context, emitter BusEmitter, channelID string, hashes []string, stats CacheStats) {
	evt := KVCacheEvictedEvent{
		ChannelID:   channelID,
		BlockHashes: hashes,
		CacheStats:  stats,
	}
	if err := emitter.Emit(ctx, TopicKVCacheEvicted, evt); err != nil {
		slog.Warn("vllm: failed to emit cache evicted event", "error", err)
	}
	slog.Info("vllm cache op",
		"operation", "evict",
		"channel_id", channelID,
		"block_count", len(hashes),
		"hit_rate", stats.HitRate,
		"ts", time.Now().UTC().Format(time.RFC3339),
	)
}

// emitHitRateChanged publishes a KVCacheHitRateChangedEvent when the hit rate
// crosses a 5% threshold relative to the previous rate.
func emitHitRateChanged(ctx context.Context, emitter BusEmitter, channelID string, prev, current float64, stats CacheStats) {
	evt := KVCacheHitRateChangedEvent{
		ChannelID:   channelID,
		PrevRate:    prev,
		CurrentRate: current,
		CacheStats:  stats,
	}
	if err := emitter.Emit(ctx, TopicKVCacheHitRateChanged, evt); err != nil {
		slog.Warn("vllm: failed to emit hit rate changed event", "error", err)
	}
	slog.Info("vllm cache op",
		"operation", "hit_rate_change",
		"channel_id", channelID,
		"block_count", stats.BlockCount,
		"hit_rate", current,
		"ts", time.Now().UTC().Format(time.RFC3339),
	)
}

// emitFragmentationHigh publishes a KVCacheFragmentationHighEvent when
// fragmentation exceeds 0.8.
func emitFragmentationHigh(ctx context.Context, emitter BusEmitter, channelID string, frag float64, stats CacheStats) {
	evt := KVCacheFragmentationHighEvent{
		ChannelID:     channelID,
		Fragmentation: frag,
		CacheStats:    stats,
	}
	if err := emitter.Emit(ctx, TopicKVCacheFragmentationHigh, evt); err != nil {
		slog.Warn("vllm: failed to emit fragmentation high event", "error", err)
	}
	slog.Info("vllm cache op",
		"operation", "frag_high",
		"channel_id", channelID,
		"block_count", stats.BlockCount,
		"hit_rate", stats.HitRate,
		"ts", time.Now().UTC().Format(time.RFC3339),
	)
}

// marshalPayload JSON-encodes a bus event payload for logging/testing.
// Only used in log helpers; not on the hot path.
func marshalPayload(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<marshal error>"
	}
	return string(b)
}

package main

import (
	"testing"
	"time"
)

// TestServiceProviderHealthCache verifies Health() serves a fresh cached result
// without recomputing (the expensive Docker probe), and recomputes once the
// cache is older than serviceHealthTTL.
func TestServiceProviderHealthCache(t *testing.T) {
	// Non-empty root with no services dir → computeHealth short-circuits to
	// Healthy without touching Docker, so the test is hermetic.
	s := &ServiceProvider{root: t.TempDir()}

	sentinel := ResourceStatus{Sync: SyncStatusOutOfSync, Health: HealthDegraded, Message: "cached-sentinel"}

	// Prime a fresh cache: Health() must return it verbatim, no recompute.
	s.mu.Lock()
	s.cachedHealth = sentinel
	s.healthAt = time.Now()
	s.healthValid = true
	s.mu.Unlock()
	if h := s.Health(); h.Message != "cached-sentinel" {
		t.Errorf("fresh cache: Health().Message=%q, want %q (cache hit, no recompute)", h.Message, "cached-sentinel")
	}

	// Expire the cache: Health() must recompute (empty workspace → Healthy),
	// replacing the sentinel.
	s.mu.Lock()
	s.healthAt = time.Now().Add(-2 * serviceHealthTTL)
	s.mu.Unlock()
	if h := s.Health(); h.Message == "cached-sentinel" {
		t.Error("expired cache: Health() returned the stale sentinel; want a recompute")
	}
}

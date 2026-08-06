package engine

import (
	"fmt"
	"sync"
	"testing"
)

// TestRegisterBus_ConcurrentManagersAllSurvive simulates the cross-process
// race on registry.json: multiple BusSessionManager instances (as a daemon
// and concurrent CLI processes would be — no shared in-process mutex)
// concurrently register distinct buses. Without the cross-process filelock
// around the load-modify-save cycle, last-writer-wins drops entries; with it,
// every registration must survive.
func TestRegisterBus_ConcurrentManagersAllSurvive(t *testing.T) {
	root := t.TempDir()
	const n = 8

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := NewBusSessionManager(root)
			if err := m.RegisterBus(fmt.Sprintf("bus-%d", i), fmt.Sprintf("sess-%d", i), "test"); err != nil {
				errs <- fmt.Errorf("register bus-%d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	m := NewBusSessionManager(root)
	registry := m.LoadRegistry()
	if len(registry) != n {
		t.Fatalf("expected %d registry entries to survive concurrent registration, got %d (lost updates)", n, len(registry))
	}
}

// TestUpdateRegistrySeqIfNewer_NoRegression verifies the monotonic guard:
// because seq updates run outside m.mu they can arrive out of order, and a
// stale update must be a no-op rather than regressing LastEventSeq.
func TestUpdateRegistrySeqIfNewer_NoRegression(t *testing.T) {
	root := t.TempDir()
	m := NewBusSessionManager(root)
	if err := m.RegisterBus("bus-a", "sess-a", "test"); err != nil {
		t.Fatal(err)
	}

	m.updateRegistrySeqIfNewer("bus-a", 5, 0, "2026-07-07T00:00:05Z")
	m.updateRegistrySeqIfNewer("bus-a", 3, 0, "2026-07-07T00:00:03Z") // stale, out of order

	registry := m.LoadRegistry()
	if len(registry) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(registry))
	}
	if registry[0].LastEventSeq != 5 {
		t.Fatalf("seq regressed: expected LastEventSeq=5 after stale update, got %d", registry[0].LastEventSeq)
	}
}

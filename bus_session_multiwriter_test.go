package main

import (
	"fmt"
	"sync"
	"testing"
)

// TestRegisterBus_ConcurrentManagersAllSurvive simulates the cross-process
// race on registry.json: multiple busSessionManager instances (as separate
// `cog bus`/`cog infer` processes would be — no shared in-process state)
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
			// A fresh manager per goroutine: no shared mutex, mirroring
			// separate OS processes contending on the same file.
			m := newBusSessionManager(root)
			if err := m.registerBus(fmt.Sprintf("bus-%d", i), fmt.Sprintf("sess-%d", i), "test"); err != nil {
				errs <- fmt.Errorf("register bus-%d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	m := newBusSessionManager(root)
	registry := m.loadRegistry()
	if len(registry) != n {
		t.Fatalf("expected %d registry entries to survive concurrent registration, got %d (lost updates)", n, len(registry))
	}
	seen := map[string]bool{}
	for _, e := range registry {
		seen[e.BusID] = true
	}
	for i := 0; i < n; i++ {
		if !seen[fmt.Sprintf("bus-%d", i)] {
			t.Fatalf("registry entry bus-%d was dropped", i)
		}
	}
}

// constellation_singleton.go - Per-workspace constellation DB connection pool
//
// Avoids 2-5ms overhead per request from repeated Open/PRAGMA/migration checks.
// Connections are cached per workspace root and stay open for the process lifetime.

package main

import (
	"sync"

	"github.com/cogos-dev/cogos/sdk/constellation"
)

var constellationCache sync.Map // map[string]*constellation.Constellation

// getConstellationForWorkspace returns a constellation connection for the given workspace root.
// Connections are cached per-workspace and created lazily on first access.
func getConstellationForWorkspace(root string) (*constellation.Constellation, error) {
	if v, ok := constellationCache.Load(root); ok {
		return v.(*constellation.Constellation), nil
	}
	db, err := constellation.Open(root)
	if err != nil {
		return nil, err
	}
	// Use LoadOrStore for thread-safety — if another goroutine raced us, use theirs
	actual, loaded := constellationCache.LoadOrStore(root, db)
	if loaded {
		// Another goroutine won the race, close our duplicate
		db.Close()
	}
	return actual.(*constellation.Constellation), nil
}

// getConstellation returns the constellation for the default workspace.
// For multi-workspace use, prefer getConstellationForWorkspace().
func getConstellation() (*constellation.Constellation, error) {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return nil, err
	}
	return getConstellationForWorkspace(root)
}

// CloseConstellation closes all cached constellation connections.
// Call this during graceful shutdown if needed.
func CloseConstellation() {
	constellationCache.Range(func(key, value any) bool {
		if db, ok := value.(*constellation.Constellation); ok {
			db.Close()
		}
		constellationCache.Delete(key)
		return true
	})
}

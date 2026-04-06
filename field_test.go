package main

import (
	"sync"
	"testing"
	"time"
)

func TestNewAttentionalField(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	if f.Len() != 0 {
		t.Errorf("new field Len = %d; want 0", f.Len())
	}
	if !f.LastUpdated().IsZero() {
		t.Error("new field LastUpdated should be zero")
	}
}

func TestFieldFovea(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	// Inject scores directly (white-box access is fine in package main tests).
	f.mu.Lock()
	f.scores = map[string]float64{
		"/a.md": 0.9,
		"/b.md": 0.5,
		"/c.md": 0.7,
		"/d.md": 0.1,
	}
	f.lastUpdated = time.Now()
	f.mu.Unlock()

	// Fovea(2) should return the top-2.
	top2 := f.Fovea(2)
	if len(top2) != 2 {
		t.Fatalf("Fovea(2) len = %d; want 2", len(top2))
	}
	if top2[0].Score != 0.9 {
		t.Errorf("Fovea[0].Score = %.2f; want 0.9", top2[0].Score)
	}
	if top2[1].Score != 0.7 {
		t.Errorf("Fovea[1].Score = %.2f; want 0.7", top2[1].Score)
	}
}

func TestFieldFoveaAll(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	f.mu.Lock()
	f.scores = map[string]float64{"/x.md": 0.5, "/y.md": 0.3}
	f.mu.Unlock()

	// n=0 returns all.
	all := f.Fovea(0)
	if len(all) != 2 {
		t.Errorf("Fovea(0) len = %d; want 2", len(all))
	}
}

func TestFieldScore(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	f.mu.Lock()
	f.scores = map[string]float64{"/known.md": 0.42}
	f.mu.Unlock()

	if got := f.Score("/known.md"); got != 0.42 {
		t.Errorf("Score known = %.2f; want 0.42", got)
	}
	if got := f.Score("/missing.md"); got != 0.0 {
		t.Errorf("Score missing = %.2f; want 0.0", got)
	}
}

func TestFieldUpdateEmptyWorkspace(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	f := NewAttentionalField(cfg)

	// With no git history, Update should not error
	// (files may just score 0 or not appear at all).
	if err := f.Update(); err != nil {
		t.Errorf("Update on empty workspace: %v", err)
	}
	if f.LastUpdated().IsZero() {
		t.Error("LastUpdated still zero after Update")
	}
}

func TestFieldConcurrentReadWrite(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	f.mu.Lock()
	f.scores = map[string]float64{"/file.md": 0.5}
	f.mu.Unlock()

	var wg sync.WaitGroup
	const readers = 10
	wg.Add(readers + 1)

	// One writer.
	go func() {
		defer wg.Done()
		for i := range 20 {
			f.mu.Lock()
			f.scores["/file.md"] = float64(i) / 20.0
			f.mu.Unlock()
		}
	}()

	// Multiple concurrent readers.
	for range readers {
		go func() {
			defer wg.Done()
			for range 20 {
				_ = f.Score("/file.md")
				_ = f.Len()
				_ = f.Fovea(5)
			}
		}()
	}

	wg.Wait()
}

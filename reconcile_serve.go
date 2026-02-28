// reconcile_serve.go
// Active reconciliation loop for cog serve. Runs the meta-reconciler
// on a timer, acting as the kubelet for CogOS declarative resources.
//
// Integration: Created in cmdServeForeground() after BEP engine block.
//   reconciler := NewServeReconciler(root)
//   reconciler.Start()
//   defer reconciler.Stop()

package main

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// ServeReconciler runs periodic reconciliation inside cog serve.
type ServeReconciler struct {
	root     string
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup

	// Metrics
	lastRun    time.Time
	cycleCount int64
}

// NewServeReconciler creates a reconciler for the given workspace.
// Reads resources.yaml for interval configuration; defaults to 5 minutes.
func NewServeReconciler(root string) *ServeReconciler {
	interval := 5 * time.Minute

	// Try to read interval from resources.yaml
	if cfg, err := loadMetaConfig(root); err == nil && len(cfg.Resources) > 0 {
		// Use the shortest non-manual interval
		for _, r := range cfg.Resources {
			if r.Interval == "manual" || r.Suspended {
				continue
			}
			if d, err := time.ParseDuration(r.Interval); err == nil && d > 0 && d < interval {
				interval = d
			}
		}
	}

	return &ServeReconciler{
		root:     root,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Start launches the reconciliation loop in a goroutine.
func (sr *ServeReconciler) Start() error {
	log.Printf("[reconciler] starting (interval=%s)", sr.interval)

	sr.wg.Add(1)
	go sr.runLoop()

	return nil
}

// Stop signals the loop to stop and waits for completion.
func (sr *ServeReconciler) Stop() {
	close(sr.stopCh)
	sr.wg.Wait()
	log.Printf("[reconciler] stopped after %d cycles", atomic.LoadInt64(&sr.cycleCount))
}

// runLoop is the main ticker loop.
func (sr *ServeReconciler) runLoop() {
	defer sr.wg.Done()

	// Run an initial cycle after a short delay
	select {
	case <-time.After(10 * time.Second):
		sr.runCycle()
	case <-sr.stopCh:
		return
	}

	ticker := time.NewTicker(sr.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			sr.runCycle()
		case <-sr.stopCh:
			return
		}
	}
}

// runCycle executes a single reconciliation pass.
func (sr *ServeReconciler) runCycle() {
	start := time.Now()
	atomic.AddInt64(&sr.cycleCount, 1)
	cycle := atomic.LoadInt64(&sr.cycleCount)

	cfg, err := loadMetaConfig(sr.root)
	if err != nil {
		log.Printf("[reconciler] cycle %d: config error: %v", cycle, err)
		return
	}

	opts := MetaReconcileOpts{
		DryRun: false,
	}

	results, err := RunMetaReconcile(sr.root, cfg, opts)
	if err != nil {
		log.Printf("[reconciler] cycle %d: error: %v", cycle, err)
		return
	}

	sr.lastRun = time.Now()
	duration := time.Since(start)

	// Log summary
	applied, drifted, failed := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "applied":
			applied++
		case "drifted":
			drifted++
		case "failed":
			failed++
		}
	}

	if applied > 0 || drifted > 0 || failed > 0 {
		log.Printf("[reconciler] cycle %d: %d applied, %d drifted, %d failed (%s)",
			cycle, applied, drifted, failed, duration.Round(time.Millisecond))
	} else {
		log.Printf("[reconciler] cycle %d: all synced (%s)", cycle, duration.Round(time.Millisecond))
	}
}

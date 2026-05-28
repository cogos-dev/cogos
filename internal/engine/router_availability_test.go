// router_availability_test.go — tests for the off-hot-path availability cache.
//
// These lock in the contract that Route reads maintained readiness (the
// background-probed snapshot) when it is fresh, and falls back to a bounded
// inline probe when the snapshot is cold (no maintainer) or stale (ticker
// stalled). The fast path is what removes the per-request blocking probe.
package engine

import (
	"context"
	"testing"
	"time"
)

// When a fresh snapshot exists, Route trusts it over a live probe — even if the
// provider has since recovered at the live layer. This is the property that
// keeps a dead provider's TCP timeout off the request path.
func TestRouterUsesAvailabilityCache(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{
		Default:       "primary",
		FallbackChain: []string{"primary", "backup"},
	})
	primary := NewStubProvider("primary", "primary reply")
	primary.available = false // down when the maintainer probes
	backup := NewStubProvider("backup", "backup reply")
	r.RegisterProvider(primary)
	r.RegisterProvider(backup)

	r.probeAll(context.Background()) // maintainer records primary=down, backup=up

	// primary "recovers" at the live layer *after* the probe; the fresh cache
	// still says down, so Route must not select it.
	primary.available = true

	p, dec, err := r.Route(context.Background(), &CompletionRequest{
		Metadata: RequestMetadata{RequestID: "cache-1"},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if p.Name() != "backup" {
		t.Errorf("selected = %q; want backup (fresh cache says primary down)", p.Name())
	}
	if !dec.FallbackUsed {
		t.Error("FallbackUsed should be true (primary cached down)")
	}
}

// With no maintainer ever started, the snapshot is nil and Route must inline
// probe so short-lived callers (one-shot CLIs, tests) still filter dead
// providers. Guards against a regression where cold cache == "all available".
func TestRouterColdCacheInlineFallback(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{
		Default:       "primary",
		FallbackChain: []string{"primary", "backup"},
	})
	primary := NewStubProvider("primary", "")
	primary.available = false
	backup := NewStubProvider("backup", "backup reply")
	r.RegisterProvider(primary)
	r.RegisterProvider(backup)

	if r.avail.Load() != nil {
		t.Fatal("precondition: snapshot should be nil before Start/probeAll")
	}

	p, dec, err := r.Route(context.Background(), &CompletionRequest{
		Metadata: RequestMetadata{RequestID: "cold-1"},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if p.Name() != "backup" {
		t.Errorf("selected = %q; want backup (inline probe sees primary down)", p.Name())
	}
	if !dec.FallbackUsed {
		t.Error("FallbackUsed should be true")
	}
}

// A stale entry (older than 3×TTL) must be ignored and re-probed inline, so a
// stalled maintainer can't pin routing to outdated "down" verdicts.
func TestRouterStaleCacheFallsBackToInline(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{Default: "primary"})
	primary := NewStubProvider("primary", "primary reply")
	primary.available = true
	r.RegisterProvider(primary)

	stale := availSnapshot{
		"primary": {ready: false, lastSeen: time.Now().Add(-time.Hour)},
	}
	r.avail.Store(&stale)

	p, _, err := r.Route(context.Background(), &CompletionRequest{
		Metadata: RequestMetadata{RequestID: "stale-1"},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if p.Name() != "primary" {
		t.Errorf("selected = %q; want primary (stale 'down' ignored, inline sees up)", p.Name())
	}
}

// probeAll records every registered provider's readiness in the snapshot.
func TestRouterProbeAllPopulatesSnapshot(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{Default: "a"})
	a := NewStubProvider("a", "")
	a.available = true
	b := NewStubProvider("b", "")
	b.available = false
	r.RegisterProvider(a)
	r.RegisterProvider(b)

	r.probeAll(context.Background())

	snap := r.avail.Load()
	if snap == nil {
		t.Fatal("snapshot nil after probeAll")
	}
	if st, ok := (*snap)["a"]; !ok || !st.ready {
		t.Errorf("a: got %+v, ok=%v; want ready", st, ok)
	}
	if st, ok := (*snap)["b"]; !ok || st.ready {
		t.Errorf("b: got %+v, ok=%v; want not ready", st, ok)
	}
}

// Close is safe to call repeatedly and when Start was never invoked.
func TestRouterCloseIdempotent(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{Default: "a"})
	r.Close()
	r.Close()
}

// Start primes the cache before returning and stops on context cancellation.
func TestRouterStartPrimesAndStops(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{Default: "a"})
	a := NewStubProvider("a", "")
	a.available = true
	r.RegisterProvider(a)

	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx) // primes synchronously

	if snap := r.avail.Load(); snap == nil {
		t.Fatal("Start should prime the snapshot synchronously")
	} else if st, ok := (*snap)["a"]; !ok || !st.ready {
		t.Errorf("primed entry for a: got %+v, ok=%v; want ready", st, ok)
	}
	cancel() // maintainer goroutine observes ctx.Done and returns
}

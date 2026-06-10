// coverage.go — per-source coverage metric for the ontology enforcement layer.
//
// Coverage is the v0.2 "Prometheus move": unmapped-component count per source
// is itself a metric. This file defines the types and accumulates counters
// during ingest; the provider exposes them via the /v1/observatory/coverage
// HTTP route and via provider.Coverage().
//
// Coverage shape per spec (§v0.2 coverage metric):
//   { mapped, degenerate?, quarantined, unmapped_component_counts }
//
// "Mapped" = records successfully emitted as a known L1 component.
// "Degenerate" = records emitted under the wrong component class (per L2
//   quality: degenerate flag, e.g. role=tool rows mapped to session.turn).
// "Quarantined" = records routed to the quarantine surface.
// "Unmapped component counts" = per-source component-class counts for
//   components that no mapping speaks.
package conversations

import "sync"

// SourceCoverage holds the coverage metric for one ingest source.
type SourceCoverage struct {
	// Mapped is the count of records successfully emitted as a known L1 component.
	Mapped int `json:"mapped"`

	// Degenerate is the count of records emitted under the wrong L1 class
	// (quality: degenerate in L2 mapping).
	Degenerate int `json:"degenerate,omitempty"`

	// Quarantined is the count of records routed to the quarantine surface
	// because no L2 mapping speaks their component class.
	Quarantined int `json:"quarantined"`

	// UnmappedComponentCounts is a per-component-class count of records that
	// were quarantined. The key is the source-declared or inferred component
	// identifier (e.g. "tool_result", "attachment", "system/turn_duration").
	UnmappedComponentCounts map[string]int `json:"unmapped_component_counts,omitempty"`

	// OntologyRef is the L1 version that was enforced (e.g. "cogos.conversations@1.0.0").
	OntologyRef string `json:"ontology_ref,omitempty"`

	// MappingRef is the L2 mapping version used (e.g. "claude-code-jsonl@1.0.0").
	MappingRef string `json:"mapping_ref,omitempty"`
}

// CoverageTracker accumulates coverage counters across ingest runs.
// Thread-safe via an internal mutex.
type CoverageTracker struct {
	mu      sync.Mutex
	sources map[string]*SourceCoverage
}

// NewCoverageTracker returns an empty tracker.
func NewCoverageTracker() *CoverageTracker {
	return &CoverageTracker{
		sources: make(map[string]*SourceCoverage),
	}
}

// RecordMapped increments the mapped counter for source.
func (t *CoverageTracker) RecordMapped(source string) {
	t.mu.Lock()
	t.getOrCreate(source).Mapped++
	t.mu.Unlock()
}

// RecordDegenerate increments the degenerate counter for source.
func (t *CoverageTracker) RecordDegenerate(source string) {
	t.mu.Lock()
	sc := t.getOrCreate(source)
	sc.Degenerate++
	sc.Mapped++ // degenerate records ARE emitted, so they count toward mapped too
	t.mu.Unlock()
}

// RecordQuarantined increments quarantined and the unmapped-component count
// for the given component class.
func (t *CoverageTracker) RecordQuarantined(source, component string) {
	t.mu.Lock()
	sc := t.getOrCreate(source)
	sc.Quarantined++
	if sc.UnmappedComponentCounts == nil {
		sc.UnmappedComponentCounts = make(map[string]int)
	}
	sc.UnmappedComponentCounts[component]++
	t.mu.Unlock()
}

// SetRefs records the ontology and mapping version refs for a source.
// Safe to call multiple times; last write wins (refs are stable within a run).
func (t *CoverageTracker) SetRefs(source, ontologyRef, mappingRef string) {
	t.mu.Lock()
	sc := t.getOrCreate(source)
	sc.OntologyRef = ontologyRef
	sc.MappingRef = mappingRef
	t.mu.Unlock()
}

// All returns a snapshot of all source coverage records. The returned map
// is a copy — safe to read without holding the lock.
func (t *CoverageTracker) All() map[string]SourceCoverage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]SourceCoverage, len(t.sources))
	for src, sc := range t.sources {
		// Deep-copy the unmapped component counts map.
		sc2 := *sc
		if sc.UnmappedComponentCounts != nil {
			sc2.UnmappedComponentCounts = make(map[string]int, len(sc.UnmappedComponentCounts))
			for k, v := range sc.UnmappedComponentCounts {
				sc2.UnmappedComponentCounts[k] = v
			}
		}
		out[src] = sc2
	}
	return out
}

// Reset clears all counters (used between reconcile runs if desired).
func (t *CoverageTracker) Reset() {
	t.mu.Lock()
	t.sources = make(map[string]*SourceCoverage)
	t.mu.Unlock()
}

// getOrCreate returns the SourceCoverage for source, creating it if absent.
// Must be called with t.mu held.
func (t *CoverageTracker) getOrCreate(source string) *SourceCoverage {
	sc, ok := t.sources[source]
	if !ok {
		sc = &SourceCoverage{}
		t.sources[source] = sc
	}
	return sc
}

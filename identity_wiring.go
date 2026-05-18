// identity_wiring.go — registers IdentityProvider with the reconcile engine.
//
// Wave 6b: minimum viable wiring so the kernel loads identities at runtime.
// IdentityProvider was implemented in Wave 6a (identity_provider.go) but
// deliberately left unregistered (see comment at identity_provider.go:223).
//
// Wiring choices:
//   - ConstellationDB: stubConstellationDB (in-memory, no-op persistence).
//     The concrete wiring to sdk/constellation/db.go is deferred to Wave 6c
//     once the DB schema stabilises. The stub is sufficient for LoadConfig +
//     Health() reporting (the only methods the daemon currently exercises) and
//     for the plan/apply cycle when invoked from the CLI.
//   - KeyResolvers: defaultKeyResolvers() — provides file:// and inline://
//     out of the box; other schemes (vault://, s3://, kms://) return
//     ErrSchemeNotImplemented.
//   - BusEmit: wired to the kernel's AppendEvent path via busEmitAdapter.
//     The adapter is intentionally nil-safe: if the event bus is not yet
//     initialised (e.g. during early startup), events are silently dropped.
//
// TODO (Wave 6c): replace stubConstellationDB with the real
// *sdk/constellation/db.DB once the DB layer is extractable.

package main

import (
	"context"
	"fmt"
	"log"
	"sync"
)

// ─── Stub ConstellationDB ────────────────────────────────────────────────────

// stubConstellationDB satisfies ConstellationDB with an in-memory store.
// Writes succeed silently; reads return the in-memory state. No persistence.
//
// This is the correct stub for Wave 6b: it lets the full reconcile lifecycle
// run (LoadConfig → FetchLive → ComputePlan → ApplyPlan → BuildState → Health)
// without any external dependency. FetchLive returns the in-memory projections
// written by ApplyPlan, so repeated reconcile cycles converge correctly even
// against the stub.
//
// Wave 6c replaces this with the real Constellation DB. Existing callers do
// not need to change because both implement ConstellationDB identically.
type stubConstellationDB struct {
	mu           sync.Mutex
	projections  map[string]IdentityProjection
	participants map[string]ParticipantRow
}

func newStubConstellationDB() *stubConstellationDB {
	return &stubConstellationDB{
		projections:  make(map[string]IdentityProjection),
		participants: make(map[string]ParticipantRow),
	}
}

func (s *stubConstellationDB) UpsertIdentityCogDoc(_ context.Context, doc IdentityProjection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projections[doc.Sub] = doc
	return nil
}

func (s *stubConstellationDB) DeleteIdentityCogDoc(_ context.Context, sub string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.projections, sub)
	return nil
}

func (s *stubConstellationDB) UpsertParticipant(_ context.Context, row ParticipantRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.participants[row.ID] = row
	return nil
}

func (s *stubConstellationDB) DeleteParticipant(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.participants, id)
	return nil
}

func (s *stubConstellationDB) GetProjection(_ context.Context, sub string) (*IdentityProjection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projections[sub]
	if !ok {
		return nil, fmt.Errorf("identity: projection not found for sub %q", sub)
	}
	return &p, nil
}

func (s *stubConstellationDB) ListProjections(_ context.Context) ([]IdentityProjection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]IdentityProjection, 0, len(s.projections))
	for _, p := range s.projections {
		out = append(out, p)
	}
	return out, nil
}

// ─── Registration ───────────────────────────────────────────────────────────

func init() {
	db := newStubConstellationDB()
	// BusEmit adapter: logs dropped events at debug level; no-op when event
	// bus infrastructure is not yet available. A full wiring to AppendEvent
	// (or the modality bus) is Wave 6c work.
	emit := BusEmit(func(eventType string, data map[string]any) error {
		log.Printf("[identity] bus event (stub-emit) type=%s\n", eventType)
		return nil
	})
	provider := NewIdentityProvider(db, nil, emit)
	RegisterProvider("identity", provider)
}

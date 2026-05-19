// identity_provider_test.go
// Covers IdentityProvider's full reconcile lifecycle (LoadConfig → FetchLive →
// ComputePlan → ApplyPlan → BuildState → Health) against an in-memory
// ConstellationDB fake and a captured event bus. Also exercises key
// resolvers, integrity-hash verification, and the three-axis health surface.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ─── In-memory fakes ─────────────────────────────────────────────────────────────

// memDB is a thread-safe in-memory ConstellationDB implementation for tests.
// The provider only depends on the six methods of ConstellationDB, so this
// single struct is enough to drive the whole reconcile lifecycle.
type memDB struct {
	mu           sync.Mutex
	projections  map[string]IdentityProjection
	participants map[string]ParticipantRow
}

func newMemDB() *memDB {
	return &memDB{
		projections:  make(map[string]IdentityProjection),
		participants: make(map[string]ParticipantRow),
	}
}

func (m *memDB) UpsertIdentityCogDoc(_ context.Context, doc IdentityProjection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.projections[doc.Sub] = doc
	return nil
}

func (m *memDB) DeleteIdentityCogDoc(_ context.Context, sub string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.projections, sub)
	return nil
}

func (m *memDB) UpsertParticipant(_ context.Context, row ParticipantRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.participants[row.ID] = row
	return nil
}

func (m *memDB) DeleteParticipant(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.participants, id)
	return nil
}

func (m *memDB) GetProjection(_ context.Context, sub string) (*IdentityProjection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.projections[sub]; ok {
		cp := p
		return &cp, nil
	}
	return nil, nil
}

func (m *memDB) ListProjections(_ context.Context) ([]IdentityProjection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]IdentityProjection, 0, len(m.projections))
	for _, p := range m.projections {
		out = append(out, p)
	}
	return out, nil
}

// capturedEvent is one entry in the test event bus.
type capturedEvent struct {
	Type string
	Data map[string]any
}

// eventRecorder is a BusEmit that appends into a slice under a mutex.
func eventRecorder(out *[]capturedEvent, mu *sync.Mutex) BusEmit {
	return func(t string, d map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		// Copy the map so test mutations don't affect history.
		cp := make(map[string]any, len(d))
		for k, v := range d {
			cp[k] = v
		}
		*out = append(*out, capturedEvent{Type: t, Data: cp})
		return nil
	}
}

// ─── Fixture builders ────────────────────────────────────────────────────────────

// writeIdentityCRD materializes a minimal valid identity YAML in the given
// workspace root. `extra` is appended to the spec block verbatim (trailing
// newline included) for tests that need to add private_key or auth_factors.
func writeIdentityCRD(t *testing.T, root, sub, iss, displayName, extra string) {
	t.Helper()
	dir := identityCRDDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := fmt.Sprintf(`apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: %s
spec:
  iss: %s
  sub: %s
  type: agent
%s  expressions:
    - aud: "*"
      display_name: %q
`, sub, iss, sub, extra, displayName)
	path := filepath.Join(dir, sub+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// setupProvider builds a fresh provider with a fresh in-memory DB and event
// recorder. Returns everything the tests commonly need.
type providerFixture struct {
	root   string
	db     *memDB
	prov   *IdentityProvider
	events *[]capturedEvent
	evMu   *sync.Mutex
}

func setupProvider(t *testing.T) *providerFixture {
	t.Helper()
	root := t.TempDir()
	db := newMemDB()
	var events []capturedEvent
	var mu sync.Mutex
	prov := NewIdentityProvider(db, nil, eventRecorder(&events, &mu))
	return &providerFixture{
		root:   root,
		db:     db,
		prov:   prov,
		events: &events,
		evMu:   &mu,
	}
}

// ─── Tests ───────────────────────────────────────────────────────────────────────

func TestIdentityProvider_Type(t *testing.T) {
	p := NewIdentityProvider(nil, nil, nil)
	if got := p.Type(); got != "identity" {
		t.Errorf("Type() = %q, want %q", got, "identity")
	}
}

func TestIdentityProvider_LoadConfig_Empty(t *testing.T) {
	fx := setupProvider(t)

	cfg, err := fx.prov.LoadConfig(fx.root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	idCfg, ok := cfg.(*identityConfig)
	if !ok {
		t.Fatalf("LoadConfig returned %T, want *identityConfig", cfg)
	}
	if len(idCfg.CRDs) != 0 {
		t.Errorf("CRDs len = %d, want 0", len(idCfg.CRDs))
	}
}

func TestIdentityProvider_LoadConfig_Multiple(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")
	writeIdentityCRD(t, fx.root, "claude", "cogos-dev", "Claude", "")
	writeIdentityCRD(t, fx.root, "sandy", "cogos-dev", "Sandy", "")

	cfg, err := fx.prov.LoadConfig(fx.root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	idCfg := cfg.(*identityConfig)
	if len(idCfg.CRDs) != 3 {
		t.Fatalf("CRDs len = %d, want 3", len(idCfg.CRDs))
	}
	for _, sub := range []string{"cog", "claude", "sandy"} {
		h, ok := idCfg.SpecHash[sub]
		if !ok || !strings.HasPrefix(h, "sha256:") {
			t.Errorf("SpecHash[%q] = %q, want sha256:...", sub, h)
		}
	}
}

func TestIdentityProvider_FetchLive_EmptyDB(t *testing.T) {
	fx := setupProvider(t)

	live, err := fx.prov.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	snap := live.(*identityLive)
	if len(snap.Projections) != 0 {
		t.Errorf("Projections = %d, want 0", len(snap.Projections))
	}
}

func TestIdentityProvider_FetchLive_ExistingProjections(t *testing.T) {
	fx := setupProvider(t)
	// Prime the DB directly so FetchLive has something to read back.
	_ = fx.db.UpsertIdentityCogDoc(context.Background(), IdentityProjection{
		Sub: "cog", Iss: "cogos-dev", Type: "agent", SpecHash: "sha256:deadbeef",
	})

	live, err := fx.prov.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	snap := live.(*identityLive)
	if got, ok := snap.Projections["cog"]; !ok || got.SpecHash != "sha256:deadbeef" {
		t.Errorf("Projections[cog] = %+v, want spec_hash sha256:deadbeef", got)
	}
}

func TestIdentityProvider_ComputePlan_AllCreates(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")
	writeIdentityCRD(t, fx.root, "claude", "cogos-dev", "Claude", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)

	plan, err := fx.prov.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 2 {
		t.Errorf("Creates = %d, want 2", plan.Summary.Creates)
	}
	if plan.Summary.Updates != 0 || plan.Summary.Deletes != 0 {
		t.Errorf("Updates/Deletes = %d/%d, want 0/0", plan.Summary.Updates, plan.Summary.Deletes)
	}
}

func TestIdentityProvider_ComputePlan_NoChanges(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	// First pass: create.
	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan (create): %v", err)
	}

	// Second pass: re-compute, should be all-skip.
	live2, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan2, err := fx.prov.ComputePlan(cfg, live2, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan2.Summary.HasChanges() {
		t.Errorf("expected no changes, got %+v", plan2.Summary)
	}
	if plan2.Summary.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", plan2.Summary.Skipped)
	}
}

func TestIdentityProvider_ComputePlan_ExpressionUpdate(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan (create): %v", err)
	}

	// Change display_name.
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog v2", "")

	cfg2, _ := fx.prov.LoadConfig(fx.root)
	live2, _ := fx.prov.FetchLive(context.Background(), cfg2)
	plan2, err := fx.prov.ComputePlan(cfg2, live2, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan2.Summary.Updates != 1 {
		t.Errorf("Updates = %d, want 1", plan2.Summary.Updates)
	}
}

func TestIdentityProvider_ComputePlan_Delete(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	// Apply create.
	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan (create): %v", err)
	}

	// Remove the spec file.
	if err := os.Remove(filepath.Join(identityCRDDir(fx.root), "cog.yaml")); err != nil {
		t.Fatalf("remove spec: %v", err)
	}

	cfg2, _ := fx.prov.LoadConfig(fx.root)
	live2, _ := fx.prov.FetchLive(context.Background(), cfg2)
	plan2, err := fx.prov.ComputePlan(cfg2, live2, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan2.Summary.Deletes != 1 {
		t.Errorf("Deletes = %d, want 1", plan2.Summary.Deletes)
	}
}

func TestIdentityProvider_ApplyPlan_Create_EmitsProjectedEvent(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	results, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != ApplySucceeded {
		t.Fatalf("results = %+v, want 1 succeeded", results)
	}

	// The projection cogdoc must exist on disk.
	if _, err := os.Stat(filepath.Join(fx.root, ".cog", "id", "cog.cog.md")); err != nil {
		t.Errorf("projection cogdoc not written: %v", err)
	}
	// DB has the participant row.
	if _, ok := fx.db.participants["agent:cog"]; !ok {
		t.Errorf("participants[agent:cog] missing")
	}
	// Event emitted.
	assertEmitted(t, fx, EventIdentityProjected, 1)
}

func TestIdentityProvider_ApplyPlan_UpdateEmitsExpressionUpdatedEvent(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan (create): %v", err)
	}
	// Reset events so the update assertion is unambiguous.
	fx.evMu.Lock()
	*fx.events = nil
	fx.evMu.Unlock()

	// Change the spec, re-plan, apply as update.
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog v2", "")
	cfg2, _ := fx.prov.LoadConfig(fx.root)
	live2, _ := fx.prov.FetchLive(context.Background(), cfg2)
	plan2, _ := fx.prov.ComputePlan(cfg2, live2, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan2); err != nil {
		t.Fatalf("ApplyPlan (update): %v", err)
	}

	assertEmitted(t, fx, EventIdentityExpressionUpdated, 1)
}

func TestIdentityProvider_ApplyPlan_DeleteEmitsDeregisteredEvent(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan (create): %v", err)
	}
	fx.evMu.Lock()
	*fx.events = nil
	fx.evMu.Unlock()

	// Remove the spec, re-plan, apply as delete.
	if err := os.Remove(filepath.Join(identityCRDDir(fx.root), "cog.yaml")); err != nil {
		t.Fatalf("remove spec: %v", err)
	}
	cfg2, _ := fx.prov.LoadConfig(fx.root)
	live2, _ := fx.prov.FetchLive(context.Background(), cfg2)
	plan2, _ := fx.prov.ComputePlan(cfg2, live2, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan2); err != nil {
		t.Fatalf("ApplyPlan (delete): %v", err)
	}

	assertEmitted(t, fx, EventIdentityDeregistered, 1)
	// Participant row removed.
	if _, ok := fx.db.participants["agent:cog"]; ok {
		t.Errorf("participants[agent:cog] still present after delete")
	}
	// Projection cogdoc removed from disk.
	if _, err := os.Stat(filepath.Join(fx.root, ".cog", "id", "cog.cog.md")); !os.IsNotExist(err) {
		t.Errorf("projection cogdoc still on disk after delete: err=%v", err)
	}
}

func TestIdentityProvider_ApplyPlan_KeyHashMismatch_RefusesApply_EmitsMismatchEvent(t *testing.T) {
	fx := setupProvider(t)

	// Write a real key bytes file, but state a bogus hash in the CRD.
	keyBytes := []byte("fake-pem-bytes")
	keyPath := filepath.Join(fx.root, "key.pem")
	if err := os.WriteFile(keyPath, keyBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	extra := fmt.Sprintf(`  private_key:
    ref: "file://%s"
    integrity_hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
`, keyPath)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", extra)

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	results, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan returned top-level error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Status != ApplyFailed {
		t.Errorf("Status = %q, want %q", results[0].Status, ApplyFailed)
	}
	if !strings.Contains(results[0].Error, "mismatch") {
		t.Errorf("Error = %q, want substring 'mismatch'", results[0].Error)
	}

	assertEmitted(t, fx, EventIdentityKeyMismatch, 1)
	// Projection cogdoc must NOT have been written.
	if _, err := os.Stat(filepath.Join(fx.root, ".cog", "id", "cog.cog.md")); !os.IsNotExist(err) {
		t.Errorf("projection cogdoc written despite mismatch: err=%v", err)
	}
	// DB must NOT have a row.
	if _, ok := fx.db.projections["cog"]; ok {
		t.Errorf("projection in DB despite mismatch")
	}
}

// ─── Wave 6a-4 idempotency tests ────────────────────────────────────────────────
//
// These cover the ApplyPlan idempotency contract required by the scheduler-
// driven interior harness: calling ApplyPlan twice with the same plan must
// produce zero new events, zero DB writes, and leave the cogdoc file
// byte-identical on the second call. See project_cogos_reconciler_is_agent.md
// for the framing: the reconciler tick IS the agent's cognitive loop, and an
// agent that can't re-tick without churning its own substrate is broken.

// TestIdentityProvider_ApplyPlan_DoubleApply_IsIdempotent is the core
// idempotency contract: same plan applied twice → first creates, second
// skips entirely (no writes, no events, file unchanged).
func TestIdentityProvider_ApplyPlan_DoubleApply_IsIdempotent(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)

	// First apply: creates.
	results1, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("first ApplyPlan: %v", err)
	}
	if len(results1) != 1 || results1[0].Status != ApplySucceeded {
		t.Fatalf("first apply = %+v, want 1 succeeded", results1)
	}

	// Snapshot state.
	fx.evMu.Lock()
	eventsBefore := len(*fx.events)
	fx.evMu.Unlock()

	projBefore, _ := fx.db.GetProjection(context.Background(), "cog")
	partBefore := fx.db.participants["agent:cog"]

	cogdocPath := filepath.Join(fx.root, ".cog", "id", "cog.cog.md")
	contentBefore, err := os.ReadFile(cogdocPath)
	if err != nil {
		t.Fatalf("read cogdoc: %v", err)
	}

	// Second apply of identical plan.
	results2, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("second ApplyPlan: %v", err)
	}
	if len(results2) != 1 || results2[0].Status != ApplySkipped {
		t.Fatalf("second apply = %+v, want 1 skipped", results2)
	}

	// Zero new events.
	fx.evMu.Lock()
	eventsAfter := len(*fx.events)
	fx.evMu.Unlock()
	if eventsAfter != eventsBefore {
		t.Errorf("second apply emitted %d new events, want 0", eventsAfter-eventsBefore)
	}

	// Projection row unchanged (including timestamp — signature of no upsert).
	projAfter, _ := fx.db.GetProjection(context.Background(), "cog")
	if projBefore == nil || projAfter == nil {
		t.Fatalf("projection missing: before=%v after=%v", projBefore, projAfter)
	}
	if !projBefore.ProjectedAt.Equal(projAfter.ProjectedAt) {
		t.Errorf("ProjectedAt mutated on re-apply: %v → %v", projBefore.ProjectedAt, projAfter.ProjectedAt)
	}
	if projBefore.SpecHash != projAfter.SpecHash {
		t.Errorf("SpecHash mutated: %q → %q", projBefore.SpecHash, projAfter.SpecHash)
	}

	// Participant row unchanged (timestamps stable).
	partAfter := fx.db.participants["agent:cog"]
	if !partBefore.LastSeen.Equal(partAfter.LastSeen) {
		t.Errorf("participant LastSeen mutated: %v → %v", partBefore.LastSeen, partAfter.LastSeen)
	}
	if !partBefore.RegisteredAt.Equal(partAfter.RegisteredAt) {
		t.Errorf("participant RegisteredAt mutated: %v → %v", partBefore.RegisteredAt, partAfter.RegisteredAt)
	}

	// Cogdoc file byte-identical (the applied_at churn fix).
	contentAfter, err := os.ReadFile(cogdocPath)
	if err != nil {
		t.Fatalf("read cogdoc after: %v", err)
	}
	if !bytes.Equal(contentBefore, contentAfter) {
		t.Errorf("cogdoc rewritten on re-apply\nbefore:\n%s\nafter:\n%s", string(contentBefore), string(contentAfter))
	}
}

// TestIdentityProvider_ApplyPlan_ReapplyAfterFileDelete_RewritesFile ensures
// the idempotency short-circuit respects disk state — if the cogdoc file is
// manually deleted between applies, the re-apply must rewrite it, not blindly
// skip based on DB row alone. This is what keeps the reconciler actually
// reconciling rather than silently drifting off disk.
func TestIdentityProvider_ApplyPlan_ReapplyAfterFileDelete_RewritesFile(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)

	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("first ApplyPlan: %v", err)
	}

	// Simulate drift: remove the cogdoc file out from under the reconciler.
	cogdocPath := filepath.Join(fx.root, ".cog", "id", "cog.cog.md")
	if err := os.Remove(cogdocPath); err != nil {
		t.Fatalf("remove cogdoc: %v", err)
	}

	// Re-apply — should detect missing file and rewrite, NOT skip.
	results, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("second ApplyPlan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("second apply: got %d results, want 1", len(results))
	}
	if results[0].Status != ApplySucceeded {
		t.Errorf("re-apply after file delete: Status = %v, want ApplySucceeded (should rewrite, not skip)", results[0].Status)
	}
	if _, err := os.Stat(cogdocPath); err != nil {
		t.Errorf("cogdoc not rewritten after delete: %v", err)
	}
}

// TestIdentityProvider_ApplyPlan_FullReconcileCycle_DoubleApply_IsIdempotent
// runs the full reconcile cycle twice (LoadConfig → FetchLive → ComputePlan →
// ApplyPlan) and asserts the second cycle produces no new events. This matches
// the scheduler-driven pattern: every tick re-runs the full cycle from disk
// state, so the full cycle must be idempotent end-to-end.
func TestIdentityProvider_ApplyPlan_FullReconcileCycle_DoubleApply_IsIdempotent(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "alice", "cogos-dev", "Alice", "")
	writeIdentityCRD(t, fx.root, "bob", "cogos-dev", "Bob", "")

	// Cycle 1 — both creates.
	cfg1, _ := fx.prov.LoadConfig(fx.root)
	live1, _ := fx.prov.FetchLive(context.Background(), cfg1)
	plan1, _ := fx.prov.ComputePlan(cfg1, live1, nil)
	results1, _ := fx.prov.ApplyPlan(context.Background(), plan1)
	if len(results1) != 2 {
		t.Fatalf("cycle 1: got %d results, want 2", len(results1))
	}
	for _, r := range results1 {
		if r.Status != ApplySucceeded {
			t.Fatalf("cycle 1 result %+v, want succeeded", r)
		}
	}

	fx.evMu.Lock()
	cycle1Events := len(*fx.events)
	fx.evMu.Unlock()

	// Cycle 2 — fresh full reload. ComputePlan should return all-skip.
	cfg2, _ := fx.prov.LoadConfig(fx.root)
	live2, _ := fx.prov.FetchLive(context.Background(), cfg2)
	plan2, _ := fx.prov.ComputePlan(cfg2, live2, nil)

	if plan2.Summary.HasChanges() {
		t.Errorf("cycle 2 plan has changes: %+v, want all-skip", plan2.Summary)
	}
	if plan2.Summary.Skipped != 2 {
		t.Errorf("cycle 2 Skipped = %d, want 2", plan2.Summary.Skipped)
	}

	// Apply the all-skip plan — all actions have ActionSkip, which ApplyPlan
	// filters with `continue` at the top of the loop, so results should be empty.
	results2, _ := fx.prov.ApplyPlan(context.Background(), plan2)
	if len(results2) != 0 {
		t.Errorf("cycle 2 apply produced %d results, want 0 (all actions skip)", len(results2))
	}

	// Zero new events across the entire second cycle.
	fx.evMu.Lock()
	cycle2Events := len(*fx.events)
	fx.evMu.Unlock()
	if cycle2Events != cycle1Events {
		t.Errorf("cycle 2 emitted %d new events, want 0", cycle2Events-cycle1Events)
	}
}

// TestIdentityProvider_ApplyPlan_RepeatedSamePlan_AllSkippedAfterFirst is the
// strictest form of the contract: applying the same plan object N times in a
// row produces exactly one effective apply. Every subsequent call returns
// ApplySkipped, and exactly one identity.projected event is emitted total.
func TestIdentityProvider_ApplyPlan_RepeatedSamePlan_AllSkippedAfterFirst(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)

	// First apply — creates.
	r0, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("apply 0: %v", err)
	}
	if len(r0) != 1 || r0[0].Status != ApplySucceeded {
		t.Fatalf("apply 0 = %+v, want 1 succeeded", r0)
	}

	// Five more applies — every one should be skipped.
	for i := 1; i <= 5; i++ {
		ri, err := fx.prov.ApplyPlan(context.Background(), plan)
		if err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		if len(ri) != 1 {
			t.Fatalf("apply %d: got %d results, want 1", i, len(ri))
		}
		if ri[0].Status != ApplySkipped {
			t.Errorf("apply %d: Status = %v, want ApplySkipped", i, ri[0].Status)
		}
	}

	// Exactly one identity.projected event across all six applies.
	assertEmitted(t, fx, EventIdentityProjected, 1)
}

func TestIdentityProvider_KeyResolvers_FileScheme(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "k.pem")
	want := []byte("hello")
	if err := os.WriteFile(keyPath, want, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := (fileKeyResolver{}).ResolveKey(context.Background(), "file://"+keyPath)
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIdentityProvider_KeyResolvers_InlineScheme(t *testing.T) {
	payload := []byte("inline-data")
	ref := "inline://" + base64.StdEncoding.EncodeToString(payload)
	got, err := (inlineKeyResolver{}).ResolveKey(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestIdentityProvider_KeyResolvers_UnsupportedScheme_Errors(t *testing.T) {
	p := NewIdentityProvider(nil, nil, nil)
	for _, scheme := range []string{"vault", "s3", "keychain", "env", "kms"} {
		ref := scheme + "://whatever"
		if _, err := p.resolveKey(context.Background(), ref); !errors.Is(err, ErrSchemeNotImplemented) {
			t.Errorf("%s://: err = %v, want ErrSchemeNotImplemented", scheme, err)
		}
	}
}

func TestIdentityProvider_IntegrityHash_SHA256(t *testing.T) {
	data := []byte("some-key-bytes")
	sum := sha256.Sum256(data)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	if err := verifyKeyIntegrity(data, hash); err != nil {
		t.Errorf("verifyKeyIntegrity passed-case: %v", err)
	}

	// sha512 passes too.
	sum5 := sha512.Sum512(data)
	hash5 := "sha512:" + hex.EncodeToString(sum5[:])
	if err := verifyKeyIntegrity(data, hash5); err != nil {
		t.Errorf("verifyKeyIntegrity sha512: %v", err)
	}
}

func TestIdentityProvider_IntegrityHash_Mismatch(t *testing.T) {
	data := []byte("some-key-bytes")
	bogus := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := verifyKeyIntegrity(data, bogus); err == nil {
		t.Errorf("verifyKeyIntegrity(mismatch) returned nil, want error")
	}
}

func TestIdentityProvider_Health_ThreeAxes(t *testing.T) {
	// Scenario 1: fresh provider with no config → Synced / Healthy / Idle.
	p := NewIdentityProvider(newMemDB(), nil, nil)
	got := p.Health()
	if got.Sync != SyncStatusSynced || got.Health != HealthHealthy || got.Operation != OperationIdle {
		t.Errorf("scenario 1: %+v, want Synced/Healthy/Idle", got)
	}

	// Scenario 2: plan has pending creates → OutOfSync / Healthy / Idle.
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")
	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	if _, err := fx.prov.ComputePlan(cfg, live, nil); err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	got = fx.prov.Health()
	if got.Sync != SyncStatusOutOfSync {
		t.Errorf("scenario 2: Sync = %q, want OutOfSync", got.Sync)
	}
	if got.Health != HealthHealthy {
		t.Errorf("scenario 2: Health = %q, want Healthy", got.Health)
	}

	// Scenario 3: projection present but spec deleted → Health = Missing.
	fx3 := setupProvider(t)
	writeIdentityCRD(t, fx3.root, "cog", "cogos-dev", "Cog", "")
	cfg3, _ := fx3.prov.LoadConfig(fx3.root)
	live3, _ := fx3.prov.FetchLive(context.Background(), cfg3)
	plan3, _ := fx3.prov.ComputePlan(cfg3, live3, nil)
	if _, err := fx3.prov.ApplyPlan(context.Background(), plan3); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	// Remove spec, re-plan; projection now orphaned.
	_ = os.Remove(filepath.Join(identityCRDDir(fx3.root), "cog.yaml"))
	cfg3b, _ := fx3.prov.LoadConfig(fx3.root)
	live3b, _ := fx3.prov.FetchLive(context.Background(), cfg3b)
	if _, err := fx3.prov.ComputePlan(cfg3b, live3b, nil); err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	got = fx3.prov.Health()
	if got.Health != HealthMissing {
		t.Errorf("scenario 3: Health = %q, want Missing", got.Health)
	}

	// Scenario 4: key-hash mismatch → Health = Degraded.
	fx4 := setupProvider(t)
	keyBytes := []byte("fake-pem-bytes")
	keyPath := filepath.Join(fx4.root, "k.pem")
	_ = os.WriteFile(keyPath, keyBytes, 0o600)
	extra := fmt.Sprintf(`  private_key:
    ref: "file://%s"
    integrity_hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
`, keyPath)
	writeIdentityCRD(t, fx4.root, "cog", "cogos-dev", "Cog", extra)
	cfg4, _ := fx4.prov.LoadConfig(fx4.root)
	live4, _ := fx4.prov.FetchLive(context.Background(), cfg4)
	plan4, _ := fx4.prov.ComputePlan(cfg4, live4, nil)
	_, _ = fx4.prov.ApplyPlan(context.Background(), plan4)
	got = fx4.prov.Health()
	if got.Health != HealthDegraded {
		t.Errorf("scenario 4: Health = %q, want Degraded", got.Health)
	}
}

func TestIdentityProvider_BuildState_LineageSurvives(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	live1, _ := fx.prov.FetchLive(context.Background(), cfg)
	state1, err := fx.prov.BuildState(cfg, live1, nil)
	if err != nil {
		t.Fatalf("BuildState #1: %v", err)
	}
	if state1.Lineage == "" {
		t.Fatalf("state1.Lineage empty")
	}
	if state1.Serial != 1 {
		t.Errorf("state1.Serial = %d, want 1", state1.Serial)
	}

	// Rebuild with the previous state as existing.
	state2, err := fx.prov.BuildState(cfg, live1, state1)
	if err != nil {
		t.Fatalf("BuildState #2: %v", err)
	}
	if state2.Lineage != state1.Lineage {
		t.Errorf("Lineage changed: %q → %q", state1.Lineage, state2.Lineage)
	}
	if state2.Serial != state1.Serial+1 {
		t.Errorf("Serial = %d, want %d", state2.Serial, state1.Serial+1)
	}
	if len(state2.Resources) != 1 {
		t.Errorf("Resources len = %d, want 1", len(state2.Resources))
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────────

// assertEmitted verifies that exactly `want` events of type `eventType` were
// captured by the fixture's recorder.
func assertEmitted(t *testing.T, fx *providerFixture, eventType string, want int) {
	t.Helper()
	fx.evMu.Lock()
	defer fx.evMu.Unlock()
	got := 0
	for _, e := range *fx.events {
		if e.Type == eventType {
			got++
		}
	}
	if got != want {
		t.Errorf("event %q: got %d, want %d (all events: %v)", eventType, got, want, eventTypes(*fx.events))
	}
}

func eventTypes(evs []capturedEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

// ─── WorkspaceRoot projection (Primitive 1) ──────────────────────────────────

// writeIdentityCRDWithWorkspaceRoot writes a CRD YAML that includes a
// workspace_root in its catch-all expression.
func writeIdentityCRDWithWorkspaceRoot(t *testing.T, root, sub, workspaceRoot string) {
	t.Helper()
	dir := identityCRDDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := fmt.Sprintf(`apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: %s
spec:
  iss: cogos-dev
  sub: %s
  type: agent
  expressions:
    - aud: "*"
      display_name: %q
      workspace_root: %q
`, sub, sub, sub, workspaceRoot)
	path := filepath.Join(dir, sub+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestIdentityProvider_WorkspaceRoot_ProjectionPopulated verifies that after
// a full reconcile cycle, IdentityProjection.WorkspaceRoot matches the value
// declared in the CRD's catch-all expression.
func TestIdentityProvider_WorkspaceRoot_ProjectionPopulated(t *testing.T) {
	fx := setupProvider(t)
	const wantRoot = "cog://workspaces/cog"
	writeIdentityCRDWithWorkspaceRoot(t, fx.root, "cog", wantRoot)

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	results, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != ApplySucceeded {
		t.Fatalf("results = %+v, want 1 succeeded", results)
	}

	proj, err := fx.db.GetProjection(context.Background(), "cog")
	if err != nil {
		t.Fatalf("GetProjection: %v", err)
	}
	if proj == nil {
		t.Fatal("projection is nil")
	}
	if proj.WorkspaceRoot != wantRoot {
		t.Errorf("WorkspaceRoot = %q, want %q", proj.WorkspaceRoot, wantRoot)
	}

	// Verify the cogdoc on disk also carries the workspace_root line.
	cogdocPath := filepath.Join(fx.root, ".cog", "id", "cog.cog.md")
	data, err := os.ReadFile(cogdocPath)
	if err != nil {
		t.Fatalf("read cogdoc: %v", err)
	}
	if !strings.Contains(string(data), wantRoot) {
		t.Errorf("cogdoc does not contain workspace_root %q\ncontent:\n%s", wantRoot, string(data))
	}
}

// TestIdentityProvider_WorkspaceRoot_AbsentIsEmpty verifies that reconciling
// an identity with no workspace_root set produces an empty WorkspaceRoot on
// the projection (not an error).
func TestIdentityProvider_WorkspaceRoot_AbsentIsEmpty(t *testing.T) {
	fx := setupProvider(t)
	// writeIdentityCRD writes a CRD with no workspace_root.
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	results, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != ApplySucceeded {
		t.Fatalf("results = %+v, want 1 succeeded", results)
	}

	proj, err := fx.db.GetProjection(context.Background(), "cog")
	if err != nil {
		t.Fatalf("GetProjection: %v", err)
	}
	if proj == nil {
		t.Fatal("projection is nil")
	}
	if proj.WorkspaceRoot != "" {
		t.Errorf("WorkspaceRoot = %q, want empty string when unset", proj.WorkspaceRoot)
	}
}

// ─── VoiceProfile projection (Primitive 3) ──────────────────────────────────────

// writeIdentityCRDWithVoiceProfile writes a CRD that includes a full
// voice_profile (both generative and discriminative heads) in its catch-all
// expression.
func writeIdentityCRDWithVoiceProfile(t *testing.T, root, sub string) {
	t.Helper()
	dir := identityCRDDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := fmt.Sprintf(`apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: %s
spec:
  iss: cogos-dev
  sub: %s
  type: agent
  expressions:
    - aud: "*"
      display_name: %q
      voice_profile:
        generative:
          engine: chatterbox-turbo
          conditionals_ref: "cog://voices/%s"
        discriminative:
          model: "speechbrain/spkrec-ecapa-voxceleb"
          embedding_ref: "cog://voices/%s/ecapa-embedding"
`, sub, sub, sub, sub, sub)
	path := filepath.Join(dir, sub+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestIdentityProvider_VoiceProfile_ProjectionPopulated verifies that after a
// full reconcile cycle, IdentityProjection.VoiceProfile carries the generative
// and discriminative heads declared in the CRD's catch-all expression.
func TestIdentityProvider_VoiceProfile_ProjectionPopulated(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRDWithVoiceProfile(t, fx.root, "cog")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	results, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != ApplySucceeded {
		t.Fatalf("results = %+v, want 1 succeeded", results)
	}

	proj, err := fx.db.GetProjection(context.Background(), "cog")
	if err != nil {
		t.Fatalf("GetProjection: %v", err)
	}
	if proj == nil {
		t.Fatal("projection is nil")
	}
	if proj.VoiceProfile == nil {
		t.Fatal("VoiceProfile is nil; expected populated")
	}
	if proj.VoiceProfile.Generative == nil {
		t.Fatal("VoiceProfile.Generative is nil")
	}
	if proj.VoiceProfile.Generative.Engine != "chatterbox-turbo" {
		t.Errorf("Engine = %q, want chatterbox-turbo", proj.VoiceProfile.Generative.Engine)
	}
	if proj.VoiceProfile.Generative.ConditionalsRef != "cog://voices/cog" {
		t.Errorf("ConditionalsRef = %q, want cog://voices/cog", proj.VoiceProfile.Generative.ConditionalsRef)
	}
	if proj.VoiceProfile.Discriminative == nil {
		t.Fatal("VoiceProfile.Discriminative is nil")
	}
	if proj.VoiceProfile.Discriminative.EmbeddingRef != "cog://voices/cog/ecapa-embedding" {
		t.Errorf("EmbeddingRef = %q, want cog://voices/cog/ecapa-embedding",
			proj.VoiceProfile.Discriminative.EmbeddingRef)
	}
}

// TestIdentityProvider_VoiceProfile_AbsentIsNil verifies that reconciling an
// identity with no voice_profile yields VoiceProfile == nil on the projection.
func TestIdentityProvider_VoiceProfile_AbsentIsNil(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	results, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != ApplySucceeded {
		t.Fatalf("results = %+v, want 1 succeeded", results)
	}

	proj, err := fx.db.GetProjection(context.Background(), "cog")
	if err != nil {
		t.Fatalf("GetProjection: %v", err)
	}
	if proj == nil {
		t.Fatal("projection is nil")
	}
	if proj.VoiceProfile != nil {
		t.Errorf("VoiceProfile should be nil when absent, got %+v", proj.VoiceProfile)
	}
}

// oscillator.go — First Instruments Module D3: the oscillating-provider
// class + the falsifiable N_conv≡1 contract test.
//
// D3 (IMPL-SPEC): the prior spec said injectors "will confirm N_conv≡1 by
// construction" — a test that confirms by construction cannot fail and
// discharges nothing. This is rewritten as an actual falsifiable CONTRACT
// TEST of the unenforced provider contract: ApplyPlan idempotence and
// single-cycle completeness are a provider contract the daemon assumes but
// does NOT check (ADR-092 §3 — no contraction bound, no in-cycle retry).
//
// The existing fakeReconcilable (isolated_registry_test.go) is Skip-only
// (always Summary{Skipped:1}, no diffing) — a tautology. oscillatingProvider
// and diffingProvider below implement REAL ComputePlan diffing so the
// contract test is a genuine probe.
package experiment

import (
	"context"
	"sync/atomic"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// diffingProvider is a minimal Reconcilable whose FetchLive returns a
// caller-controlled "live" value, and whose ComputePlan does a REAL diff
// against the declared config: if live != config, the plan has one Update
// action; if they match, the plan is a single Skip. BuildState always
// records the live value as the new state. This is a genuine
// diffing/converging provider (not the tautological Skip-always fake), so
// the D3 contract test is a real probe: after ApplyPlan is (simulated to)
// converge live to config, the NEXT ComputePlan must show HasChanges()==false.
type diffingProvider struct {
	typeName string

	// desired is the declared config value ComputePlan diffs FetchLive
	// against.
	desired string

	// live is mutated by the test/injector between cycles to simulate
	// external drift or the effect of a (possibly partial) ApplyPlan.
	live atomic.Value // string

	// applyBehavior controls what ApplyPlan does when there is a diff:
	//   applyFullyConverges: sets live = desired (a correct, idempotent
	//     provider — the contract test must PASS for this behavior).
	//   applyPartialOnly: leaves live unchanged (a broken provider that
	//     never actually applies — the contract test must FAIL for this
	//     behavior, proving the test can fail).
	applyBehavior applyBehavior

	computeCount atomic.Int32
	applyCount   atomic.Int32
}

type applyBehavior int

const (
	applyFullyConverges applyBehavior = iota
	applyPartialOnly
)

func newDiffingProvider(typeName, desired, initialLive string, behavior applyBehavior) *diffingProvider {
	p := &diffingProvider{typeName: typeName, desired: desired, applyBehavior: behavior}
	p.live.Store(initialLive)
	return p
}

func (p *diffingProvider) Type() string { return p.typeName }

func (p *diffingProvider) LoadConfig(_ string) (any, error) {
	return p.desired, nil
}

func (p *diffingProvider) FetchLive(_ context.Context, _ any) (any, error) {
	return p.live.Load().(string), nil
}

func (p *diffingProvider) ComputePlan(config any, live any, _ *reconcile.State) (*reconcile.Plan, error) {
	p.computeCount.Add(1)
	desired := config.(string)
	current := live.(string)

	if desired == current {
		return &reconcile.Plan{
			ResourceType: p.typeName,
			Actions: []reconcile.Action{{
				Action:       reconcile.ActionSkip,
				ResourceType: p.typeName,
				Name:         p.typeName,
				Details:      map[string]any{"reason": "in sync"},
			}},
			Summary: reconcile.Summary{Skipped: 1},
		}, nil
	}
	return &reconcile.Plan{
		ResourceType: p.typeName,
		Actions: []reconcile.Action{{
			Action:       reconcile.ActionUpdate,
			ResourceType: p.typeName,
			Name:         p.typeName,
			Details:      map[string]any{"from": current, "to": desired},
		}},
		Summary: reconcile.Summary{Updates: 1},
	}, nil
}

func (p *diffingProvider) ApplyPlan(_ context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	p.applyCount.Add(1)
	if !plan.Summary.HasChanges() {
		return nil, nil
	}
	switch p.applyBehavior {
	case applyFullyConverges:
		p.live.Store(p.desired)
	case applyPartialOnly:
		// Deliberately does nothing — simulates a provider whose ApplyPlan
		// only partially applies (or silently no-ops) per cycle.
	}
	return []reconcile.Result{{
		Phase:  "apply",
		Action: string(reconcile.ActionUpdate),
		Name:   p.typeName,
		Status: reconcile.ApplySucceeded,
	}}, nil
}

func (p *diffingProvider) BuildState(_ any, live any, existing *reconcile.State) (*reconcile.State, error) {
	s := reconcile.NewState(p.typeName)
	if existing != nil {
		s.Lineage = existing.Lineage
		s.Serial = existing.Serial
	}
	return s, nil
}

func (p *diffingProvider) Health() reconcile.ResourceStatus {
	return reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
}

// SetLive externally mutates the provider's live value (simulating a
// perturbation injector writing to the watched filesystem/git tree — D1).
func (p *diffingProvider) SetLive(v string) {
	p.live.Store(v)
}

// ─── D3: the oscillating-provider class ────────────────────────────────────

// oscillatingProvider is a Reconcilable whose FetchLive alternates between
// two states across ticks — the ONLY class where single-pass reconcile
// cannot converge in 1 tick (K2). Each call to FetchLive flips the observed
// state, so ComputePlan always sees a diff (never HasChanges()==false)
// under a correct single-pass reconciler; if it EVER reports
// HasChanges()==false within the frozen 8-tick budget, that is a KC-2
// finding (a contradiction of single-pass).
type oscillatingProvider struct {
	typeName string
	flip     atomic.Bool // current observed state
	fetches  atomic.Int32
}

func newOscillatingProvider(typeName string) *oscillatingProvider {
	return &oscillatingProvider{typeName: typeName}
}

func (p *oscillatingProvider) Type() string { return p.typeName }

func (p *oscillatingProvider) LoadConfig(_ string) (any, error) {
	return "steady-state", nil
}

func (p *oscillatingProvider) FetchLive(_ context.Context, _ any) (any, error) {
	p.fetches.Add(1)
	flipped := !p.flip.Load()
	p.flip.Store(flipped)
	if flipped {
		return "state-A", nil
	}
	return "state-B", nil
}

func (p *oscillatingProvider) ComputePlan(config any, live any, _ *reconcile.State) (*reconcile.Plan, error) {
	desired := config.(string)
	current := live.(string)
	if desired == current {
		return &reconcile.Plan{
			ResourceType: p.typeName,
			Actions:      []reconcile.Action{{Action: reconcile.ActionSkip, ResourceType: p.typeName, Name: p.typeName}},
			Summary:      reconcile.Summary{Skipped: 1},
		}, nil
	}
	return &reconcile.Plan{
		ResourceType: p.typeName,
		Actions:      []reconcile.Action{{Action: reconcile.ActionUpdate, ResourceType: p.typeName, Name: p.typeName}},
		Summary:      reconcile.Summary{Updates: 1},
	}, nil
}

func (p *oscillatingProvider) ApplyPlan(_ context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	// Single-pass: applying never "catches up" to the next flip — the next
	// FetchLive call (next cycle) flips again regardless of what was just
	// applied. This is the structural reason a 2-state oscillator can never
	// converge under single-pass reconcile.
	var results []reconcile.Result
	for _, a := range plan.Actions {
		results = append(results, reconcile.Result{Phase: "apply", Action: string(a.Action), Name: a.Name, Status: reconcile.ApplySucceeded})
	}
	return results, nil
}

func (p *oscillatingProvider) BuildState(_ any, _ any, existing *reconcile.State) (*reconcile.State, error) {
	s := reconcile.NewState(p.typeName)
	if existing != nil {
		s.Lineage = existing.Lineage
		s.Serial = existing.Serial
	}
	return s, nil
}

func (p *oscillatingProvider) Health() reconcile.ResourceStatus {
	return reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
}

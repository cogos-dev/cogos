// first_instruments_b_test.go — Module B tests (First Instruments Stage-2).
//
// B2-gate correctness for ReconcileDaemon.LastCoherence (M1-B): all-Skipped
// -> C_B==1.0, all-empty (Total()==0) -> C_B==1.0, zero-Skipped-AND-Total()>0
// -> C_B==0.0, mixed -> strictly between. Also exercises the HTTP surface
// (GET /v1/reconcile/coherence) end to end.
package testkernel_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/testkernel"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// summaryFake is a Reconcilable whose ComputePlan returns a caller-supplied
// fixed Summary, letting each test drive LastCoherence's B2-gate branches
// directly without needing a real diff-producing provider.
type summaryFake struct {
	typeName string
	summary  reconcile.Summary
}

func (f *summaryFake) Type() string { return f.typeName }

func (f *summaryFake) LoadConfig(_ string) (any, error) {
	return map[string]any{"type": f.typeName}, nil
}

func (f *summaryFake) FetchLive(_ context.Context, _ any) (any, error) {
	return map[string]any{"live": true}, nil
}

func (f *summaryFake) ComputePlan(_ any, _ any, _ *reconcile.State) (*reconcile.Plan, error) {
	actions := make([]reconcile.Action, 0, f.summary.Total())
	addActions := func(action reconcile.ActionType, n int) {
		for i := 0; i < n; i++ {
			actions = append(actions, reconcile.Action{
				Action:       action,
				ResourceType: f.typeName,
				Name:         "test",
				Details:      map[string]any{},
			})
		}
	}
	addActions(reconcile.ActionCreate, f.summary.Creates)
	addActions(reconcile.ActionUpdate, f.summary.Updates)
	addActions(reconcile.ActionDelete, f.summary.Deletes)
	addActions(reconcile.ActionSkip, f.summary.Skipped)

	return &reconcile.Plan{
		ResourceType: f.typeName,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Actions:      actions,
		Summary:      f.summary,
	}, nil
}

func (f *summaryFake) ApplyPlan(_ context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	var results []reconcile.Result
	for _, a := range plan.Actions {
		results = append(results, reconcile.Result{
			Phase:  "apply",
			Action: string(a.Action),
			Name:   a.Name,
			Status: reconcile.ApplySucceeded,
		})
	}
	return results, nil
}

func (f *summaryFake) BuildState(_ any, _ any, existing *reconcile.State) (*reconcile.State, error) {
	s := reconcile.NewState(f.typeName)
	if existing != nil {
		s.Lineage = existing.Lineage
		s.Serial = existing.Serial
	}
	return s, nil
}

func (f *summaryFake) Health() reconcile.ResourceStatus {
	return reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
}

// runOneCycleAndReadCoherence boots a kernel with the given fakes, triggers
// exactly one cycle per fake, waits for completion, and returns the
// aggregate C_B.
func runOneCycleAndReadCoherence(t *testing.T, fakes ...*summaryFake) float64 {
	t.Helper()
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	providers := make([]reconcile.Reconcilable, len(fakes))
	for i, f := range fakes {
		providers[i] = f
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	k, err := testkernel.Boot(ctx, t,
		testkernel.WithIsolatedRegistry(providers...),
		testkernel.WithPollInterval(1*time.Hour),
	)
	if err != nil {
		t.Fatalf("testkernel.Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := k.Stop(); err != nil {
			t.Errorf("testkernel.Stop: %v", err)
		}
	})

	for _, f := range fakes {
		k.ReconcileDaemon().Trigger(f.typeName)
	}
	for _, f := range fakes {
		if err := testkernel.WaitForCycle(ctx, k, f.typeName, 1); err != nil {
			t.Fatalf("WaitForCycle(%s): %v", f.typeName, err)
		}
	}

	cB, _ := k.ReconcileDaemon().LastCoherence()
	return cB
}

// TestLastCoherence_AllSkipped_CB1 covers B2-gate: all providers all-Skipped
// (Creates=Updates=Deletes=0, Skipped>0) => C_B == 1.0.
func TestLastCoherence_AllSkipped_CB1(t *testing.T) {
	fake := &summaryFake{typeName: "b2-all-skipped", summary: reconcile.Summary{Skipped: 3}}
	cB := runOneCycleAndReadCoherence(t, fake)
	if cB != 1.0 {
		t.Errorf("C_B = %v; want 1.0 for all-Skipped", cB)
	}
}

// TestLastCoherence_EmptyPlan_CB1 covers the B2 empty-plan boundary fix: all
// providers empty (Creates=Updates=Deletes=Skipped=0, Total()==0) => C_B ==
// 1.0 (a fully-in-sync idle provider is coherent, not maximally drifted).
func TestLastCoherence_EmptyPlan_CB1(t *testing.T) {
	fake := &summaryFake{typeName: "b2-empty-plan", summary: reconcile.Summary{}}
	cB := runOneCycleAndReadCoherence(t, fake)
	if cB != 1.0 {
		t.Errorf("C_B = %v; want 1.0 for an empty plan (Total()==0)", cB)
	}
}

// TestLastCoherence_ZeroSkippedNonEmpty_CB0 covers B2-gate: all providers
// zero-Skipped AND Total()>0 (all actions real) => C_B == 0.0. The
// Total()>0 conjunct is required so this does not collide with the
// empty-plan case.
func TestLastCoherence_ZeroSkippedNonEmpty_CB0(t *testing.T) {
	fake := &summaryFake{typeName: "b2-zero-skipped", summary: reconcile.Summary{Creates: 2, Updates: 1}}
	cB := runOneCycleAndReadCoherence(t, fake)
	if cB != 0.0 {
		t.Errorf("C_B = %v; want 0.0 for zero-Skipped AND Total()>0", cB)
	}
}

// TestLastCoherence_Mixed_StrictlyBetween covers B2-gate: a mix of drifted
// and skipped actions on one provider lands strictly between 0 and 1.
func TestLastCoherence_Mixed_StrictlyBetween(t *testing.T) {
	fake := &summaryFake{typeName: "b2-mixed", summary: reconcile.Summary{Creates: 1, Skipped: 3}}
	cB := runOneCycleAndReadCoherence(t, fake)
	if cB <= 0.0 || cB >= 1.0 {
		t.Errorf("C_B = %v; want strictly between 0 and 1 for a mixed plan", cB)
	}
}

// TestLastCoherence_MultiProvider_Averages confirms C_B averages
// drift_fraction across providers (1 - mean(drift_fraction)), not just the
// last-observed provider.
func TestLastCoherence_MultiProvider_Averages(t *testing.T) {
	allSkipped := &summaryFake{typeName: "b2-multi-skipped", summary: reconcile.Summary{Skipped: 2}}
	allDrifted := &summaryFake{typeName: "b2-multi-drifted", summary: reconcile.Summary{Creates: 2}}
	cB := runOneCycleAndReadCoherence(t, allSkipped, allDrifted)
	// drift_fraction: 0 for allSkipped, 1 for allDrifted => mean 0.5 => C_B=0.5.
	const want = 0.5
	const tol = 1e-9
	if cB < want-tol || cB > want+tol {
		t.Errorf("C_B = %v; want %v (average of 0 and 1 drift_fraction)", cB, want)
	}
}

// TestLastCoherence_NoCycleYet_CB1 confirms LastCoherence reports C_B==1.0
// with no per-provider detail when no cycle has completed (vacuously
// coherent — nothing observed to have drifted).
func TestLastCoherence_NoCycleYet_CB1(t *testing.T) {
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	k, err := testkernel.Boot(ctx, t, testkernel.WithPollInterval(1*time.Hour))
	if err != nil {
		t.Fatalf("testkernel.Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := k.Stop(); err != nil {
			t.Errorf("testkernel.Stop: %v", err)
		}
	})

	cB, detail := k.ReconcileDaemon().LastCoherence()
	if cB != 1.0 {
		t.Errorf("C_B = %v; want 1.0 before any cycle completes", cB)
	}
	if len(detail) != 0 {
		t.Errorf("detail = %v; want empty before any cycle completes", detail)
	}
}

// TestReconcileCoherenceHTTP_ReflectsLastCoherence exercises the HTTP
// surface (GET /v1/reconcile/coherence) end to end and confirms it agrees
// with the daemon's own LastCoherence().
func TestReconcileCoherenceHTTP_ReflectsLastCoherence(t *testing.T) {
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	fake := &summaryFake{typeName: "b2-http", summary: reconcile.Summary{Creates: 1, Skipped: 1}}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	k, err := testkernel.Boot(ctx, t,
		testkernel.WithIsolatedRegistry(fake),
		testkernel.WithPollInterval(1*time.Hour),
	)
	if err != nil {
		t.Fatalf("testkernel.Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := k.Stop(); err != nil {
			t.Errorf("testkernel.Stop: %v", err)
		}
	})

	k.ReconcileDaemon().Trigger(fake.typeName)
	if err := testkernel.WaitForCycle(ctx, k, fake.typeName, 1); err != nil {
		t.Fatalf("WaitForCycle: %v", err)
	}

	wantCB, _ := k.ReconcileDaemon().LastCoherence()

	resp, err := http.Get(k.Endpoint() + "/v1/reconcile/coherence")
	if err != nil {
		t.Fatalf("GET /v1/reconcile/coherence: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	var body struct {
		CB                   float64 `json:"c_b"`
		PerProviderDriftFrac []any   `json:"per_provider_drift_fraction"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.CB != wantCB {
		t.Errorf("HTTP c_b = %v; want %v (matching daemon.LastCoherence())", body.CB, wantCB)
	}
	if len(body.PerProviderDriftFrac) == 0 {
		t.Error("per_provider_drift_fraction is empty; want one entry for the triggered provider")
	}
}

package selfupdate

import (
	"context"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// fixtureLive builds a selfUpdateLive for ComputePlan table tests.
func fixtureLive(running, target string, disabled bool, fetchErr string) *selfUpdateLive {
	lv := &selfUpdateLive{RunningVersion: running, Disabled: disabled, FetchErr: fetchErr}
	if target != "" {
		lv.Target = &resolvedRelease{Tag: target}
	}
	return lv
}

func TestComputePlanGates(t *testing.T) {
	cases := []struct {
		name       string
		running    string
		target     string
		disabled   bool
		fetchErr   string
		pin        string
		autoApply  bool
		wantAction reconcile.ActionType
		wantReason string
	}{
		{name: "disabled", disabled: true, wantAction: reconcile.ActionSkip, wantReason: "disabled"},
		{name: "fetch_error", running: "v0.16.4", fetchErr: "boom", wantAction: reconcile.ActionSkip, wantReason: "fetch_error"},
		{name: "dev_build", running: "dev", target: "v0.16.5", wantAction: reconcile.ActionSkip, wantReason: "dev_build"},
		{name: "dev_build_with_pin", running: "dev", target: "v0.16.3", pin: "v0.16.3", wantAction: reconcile.ActionSkip, wantReason: "dev_build"},
		{name: "up_to_date", running: "v0.16.5", target: "v0.16.5", wantAction: reconcile.ActionSkip, wantReason: "up_to_date"},
		{name: "running_ahead", running: "v0.16.6", target: "v0.16.5", wantAction: reconcile.ActionSkip, wantReason: "running_ahead"},
		{name: "behind_update", running: "v0.16.4", target: "v0.16.5", wantAction: reconcile.ActionUpdate},
		{name: "pinned_downgrade", running: "v0.16.5", target: "v0.16.3", pin: "v0.16.3", wantAction: reconcile.ActionUpdate},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New()
			p.root = t.TempDir()
			cfg := &SelfUpdateConfig{Enabled: !c.disabled, Channel: channelStable, Repo: defaultRepo, Pin: c.pin, AutoApply: c.autoApply}
			cfg.root = p.root
			lv := fixtureLive(c.running, c.target, c.disabled, c.fetchErr)

			plan, err := p.ComputePlan(cfg, lv, nil)
			if err != nil {
				t.Fatalf("ComputePlan: %v", err)
			}
			if len(plan.Actions) != 1 {
				t.Fatalf("got %d actions; want 1", len(plan.Actions))
			}
			act := plan.Actions[0]
			if act.Action != c.wantAction {
				t.Errorf("action = %q; want %q", act.Action, c.wantAction)
			}
			if c.wantReason != "" {
				if reason, _ := act.Details["reason"].(string); reason != c.wantReason {
					t.Errorf("reason = %q; want %q", reason, c.wantReason)
				}
			}
		})
	}
}

func TestComputePlanIsPure(t *testing.T) {
	p := New()
	p.root = t.TempDir()
	cfg := &SelfUpdateConfig{Enabled: true, Channel: channelStable, Repo: defaultRepo, AutoApply: true}
	cfg.root = p.root
	lv := fixtureLive("v0.16.4", "v0.16.5", false, "")

	// Snapshot mutable provider fields.
	p.mu.Lock()
	statusBefore := p.status
	inProgBefore := p.inProgress
	p.mu.Unlock()

	plan1, err := p.ComputePlan(cfg, lv, nil)
	if err != nil {
		t.Fatalf("ComputePlan #1: %v", err)
	}
	plan2, err := p.ComputePlan(cfg, lv, nil)
	if err != nil {
		t.Fatalf("ComputePlan #2: %v", err)
	}

	// Identical inputs → identical actions (ignore GeneratedAt timestamp).
	if !reflect.DeepEqual(plan1.Actions, plan2.Actions) {
		t.Errorf("ComputePlan not deterministic:\n#1 %+v\n#2 %+v", plan1.Actions, plan2.Actions)
	}

	// No provider mutation.
	p.mu.Lock()
	defer p.mu.Unlock()
	if !reflect.DeepEqual(p.status, statusBefore) {
		t.Errorf("ComputePlan mutated status: before=%+v after=%+v", statusBefore, p.status)
	}
	if p.inProgress != inProgBefore {
		t.Errorf("ComputePlan mutated inProgress: before=%v after=%v", inProgBefore, p.inProgress)
	}
}

func TestApplyPlanAutoApplyOffSkips(t *testing.T) {
	var spawned int32
	prev := spawnFn
	spawnFn = func(root, toTag, repo string, port int) error {
		atomic.AddInt32(&spawned, 1)
		return nil
	}
	t.Cleanup(func() { spawnFn = prev })

	p := New()
	p.root = t.TempDir()
	plan := updatePlan(false /* autoApply */)

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != reconcile.ApplySkipped {
		t.Fatalf("want one skipped result, got %+v", results)
	}
	if atomic.LoadInt32(&spawned) != 0 {
		t.Error("auto_apply off must not spawn the updater")
	}
}

func TestApplyPlanNonDarwinSkips(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this asserts the non-darwin notify-only branch")
	}
	var spawned int32
	prev := spawnFn
	spawnFn = func(root, toTag, repo string, port int) error {
		atomic.AddInt32(&spawned, 1)
		return nil
	}
	t.Cleanup(func() { spawnFn = prev })

	p := New()
	p.root = t.TempDir()
	plan := updatePlan(true /* autoApply */)

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != reconcile.ApplySkipped {
		t.Fatalf("want one skipped result (auto-apply unsupported), got %+v", results)
	}
	if atomic.LoadInt32(&spawned) != 0 {
		t.Error("non-darwin must not spawn the updater")
	}
}

func TestApplyPlanDarwinSpawns(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("spawn path is darwin-only")
	}
	var spawned int32
	prev := spawnFn
	spawnFn = func(root, toTag, repo string, port int) error {
		atomic.AddInt32(&spawned, 1)
		return nil
	}
	t.Cleanup(func() { spawnFn = prev })

	p := New()
	p.root = t.TempDir()
	plan := updatePlan(true /* autoApply */)

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != reconcile.ApplySucceeded {
		t.Fatalf("want one succeeded result, got %+v", results)
	}
	if atomic.LoadInt32(&spawned) != 1 {
		t.Errorf("darwin auto-apply spawned %d times; want 1", spawned)
	}
}

// updatePlan builds a one-action Update plan for ApplyPlan tests.
func updatePlan(autoApply bool) *reconcile.Plan {
	return &reconcile.Plan{
		ResourceType: "self-update",
		Actions: []reconcile.Action{{
			Action:       reconcile.ActionUpdate,
			ResourceType: "self-update",
			Name:         "cogos",
			Details: map[string]any{
				"from":       "v0.16.4",
				"to":         "v0.16.5",
				"auto_apply": autoApply,
				"os":         runtime.GOOS,
				"arch":       runtime.GOARCH,
				"repo":       defaultRepo,
				"pinned":     false,
			},
		}},
	}
}

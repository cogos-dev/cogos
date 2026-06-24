// provider_test.go — Tests for the EvalProvider Phase C implementation.
//
// Verification gates (per task brief):
//  1. LoadConfig parses real experiment cogdocs (exp-001-anti-pattern-placement)
//  2. FetchLive groups TrialRecords correctly from bus JSONL events
//  3. ComputePlan returns expected actions for empty/full/regression scenarios
//  4. ApplyPlan with stub dispatcher emits CogBlocks and updates state
//  5. Health surfaces the right axes
//  6. Score() port from Python matches expected verdicts
//
// No actual Ollama dispatches — all tests use stub AgentDispatcher.
package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

const realExperimentCogdocPath = "/Users/slowbro/workspaces/cog/.cog/mem/semantic/architecture/tournament/experiments/exp-001-anti-pattern-placement.cog.md"
const realBusEventsPath = "/Users/slowbro/workspaces/cog/.cog/.state/buses/bus_tournament/events.jsonl"
const realWorkspaceRoot = "/Users/slowbro/workspaces/cog"

// stubDispatcher is a no-op AgentDispatcher for tests.
type stubDispatcher struct {
	results []DispatchResult
	err     error
}

func (s *stubDispatcher) DispatchToHarness(_ context.Context, req DispatchRequest) (*DispatchBatchResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	results := s.results
	if len(results) == 0 {
		results = []DispatchResult{{
			Index:   0,
			Success: true,
			Content: "stub response",
		}}
	}
	return &DispatchBatchResult{Results: results}, nil
}

// stubEmitter records emitted CogBlocks for inspection.
type stubEmitter struct {
	emitted []map[string]any
}

func (s *stubEmitter) EmitCogBlock(_ context.Context, channel string, block any) error {
	b, _ := json.Marshal(block)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]any{}
	}
	m["_channel"] = channel
	s.emitted = append(s.emitted, m)
	return nil
}

// stubBusReader replays a fixed set of events.
type stubBusReader struct {
	events []BusEvent
}

func (s *stubBusReader) ReadChannel(_ context.Context, _ string, _ string) ([]BusEvent, error) {
	return s.events, nil
}

// buildTestProvider returns an EvalProvider with stub dependencies.
func buildTestProvider(dispatcher AgentDispatcher, emitter BusEmitter, busReader BusReader) *EvalProvider {
	p := New(dispatcher, emitter)
	p.busReader = busReader
	return p
}

// makeTrialRecord creates a minimal TrialRecord for testing.
func makeTrialRecord(experimentID, sp, td, taskID string, passed bool) TrialRecord {
	return TrialRecord{
		TrialID:      experimentID + "__" + sp + "+" + td + "__" + taskID,
		ExperimentID: experimentID,
		VariantIDs:   map[string]string{"system_prompt": sp, "tool_description": td},
		TaskID:       taskID,
		Passed:       passed,
		Timestamp:    "2026-04-26T00:00:00Z",
	}
}

// busEventFromTrialRecord wraps a TrialRecord as a bus event (matching persist.py shape).
func busEventFromTrialRecord(tr TrialRecord) BusEvent {
	trialPayload := map[string]any{
		"type":          "tournament.trial.v1",
		"trial":         tr,
		"experiment_id": tr.ExperimentID,
	}
	b, _ := json.Marshal(trialPayload)

	return BusEvent{
		V:    2,
		Type: "tournament.trial.v1",
		From: "tournament/" + tr.ExperimentID,
		Payload: map[string]interface{}{
			"content": string(b),
		},
	}
}

// ---------------------------------------------------------------------------
// TestLoadConfig_RealExperiment: parse the actual exp-001 cogdoc
// ---------------------------------------------------------------------------

func TestLoadConfig_RealExperiment(t *testing.T) {
	if _, err := os.Stat(realExperimentCogdocPath); os.IsNotExist(err) {
		t.Skipf("real experiment cogdoc not found at %s", realExperimentCogdocPath)
	}

	p := buildTestProvider(nil, nil, nil)
	cfgAny, err := p.LoadConfig(realWorkspaceRoot)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg, ok := cfgAny.(*EvalConfig)
	if !ok || cfg == nil {
		t.Fatal("LoadConfig returned nil or wrong type")
	}

	exp, exists := cfg.Experiments["exp-001-anti-pattern-placement"]
	if !exists {
		t.Errorf("experiment exp-001-anti-pattern-placement not found; got: %v", keys(cfg.Experiments))
		return
	}

	// Validate parsed fields
	if exp.ID != "exp-001-anti-pattern-placement" {
		t.Errorf("ID = %q, want exp-001-anti-pattern-placement", exp.ID)
	}
	if exp.BaselineVariant != "sp-1-production+td-1-current" {
		t.Errorf("BaselineVariant = %q, want sp-1-production+td-1-current", exp.BaselineVariant)
	}
	if exp.Target != "laptop-lms" {
		t.Errorf("Target = %q, want laptop-lms", exp.Target)
	}
	if len(exp.TaskIDs) != 4 {
		t.Errorf("TaskIDs = %v (len %d), want 4", exp.TaskIDs, len(exp.TaskIDs))
	}
	spIDs := exp.VariantAxes["system_prompt"]
	if len(spIDs) != 2 {
		t.Errorf("system_prompt axis = %v, want 2 variants", spIDs)
	}
	tdIDs := exp.VariantAxes["tool_description"]
	if len(tdIDs) != 2 {
		t.Errorf("tool_description axis = %v, want 2 variants", tdIDs)
	}

	// TournamentRoot should be set
	if cfg.TournamentRoot == "" {
		t.Error("TournamentRoot should be non-empty")
	}
	t.Logf("Loaded experiment: id=%s tasks=%v sp=%v td=%v", exp.ID, exp.TaskIDs, spIDs, tdIDs)
}

// ---------------------------------------------------------------------------
// TestLoadConfig_EmptyDir: non-existent dir returns empty config, not error
// ---------------------------------------------------------------------------

func TestLoadConfig_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	p := buildTestProvider(nil, nil, nil)
	cfgAny, err := p.LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig on empty dir: %v", err)
	}
	cfg, ok := cfgAny.(*EvalConfig)
	if !ok || cfg == nil {
		t.Fatal("expected *EvalConfig")
	}
	if len(cfg.Experiments) != 0 {
		t.Errorf("expected 0 experiments, got %d", len(cfg.Experiments))
	}
}

// ---------------------------------------------------------------------------
// TestFetchLive_RealBusEvents: deserialize actual bus_tournament events
// ---------------------------------------------------------------------------

func TestFetchLive_RealBusEvents(t *testing.T) {
	if _, err := os.Stat(realBusEventsPath); os.IsNotExist(err) {
		t.Skipf("real bus events not found at %s", realBusEventsPath)
	}

	reader := NewFileBusReader(realBusEventsPath)
	p := buildTestProvider(nil, nil, reader)
	// Also need root for LoadConfig
	_ = p.root

	liveAny, err := p.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	ls, ok := liveAny.(*EvalLiveState)
	if !ok || ls == nil {
		t.Fatal("expected *EvalLiveState")
	}

	t.Logf("FetchLive: %d trials fetched, %d scorecards", len(ls.Trials), len(ls.Scorecards))

	// We know there are 35 events in the real bus; some are trial records
	if len(ls.Trials) == 0 {
		t.Error("expected non-zero trials from real bus events")
	}

	// Count trials with ExperimentID set (the first bus event is a test record with no experiment_id)
	withExp := 0
	for _, tr := range ls.Trials {
		if tr.ExperimentID != "" {
			withExp++
		}
	}
	if withExp == 0 {
		t.Error("expected at least some trials with non-empty ExperimentID")
	}
	t.Logf("trials with ExperimentID: %d / %d", withExp, len(ls.Trials))

	// Should have a scorecard for exp-001
	sc, ok := ls.Scorecards["exp-001-anti-pattern-placement"]
	if !ok {
		t.Errorf("no scorecard for exp-001-anti-pattern-placement; got: %v", keys(ls.Scorecards))
	} else {
		t.Logf("exp-001 scorecard: %d variant keys, %d task IDs", len(sc.VariantKeys), len(sc.TaskIDs))
	}
}

// ---------------------------------------------------------------------------
// TestFetchLive_GroupsTrialsByExperiment: stub bus reader
// ---------------------------------------------------------------------------

func TestFetchLive_GroupsTrialsByExperiment(t *testing.T) {
	tr1 := makeTrialRecord("exp-001", "sp-1-production", "td-1-current", "task-1-state-probe", true)
	tr2 := makeTrialRecord("exp-001", "sp-1-production", "td-1-current", "task-2-two-tool-chain", false)
	tr3 := makeTrialRecord("exp-002", "sp-1", "td-1", "task-a", true)

	events := []BusEvent{
		busEventFromTrialRecord(tr1),
		busEventFromTrialRecord(tr2),
		busEventFromTrialRecord(tr3),
		// Non-trial event should be ignored
		{V: 2, Type: "tournament.experiment.v1", Payload: map[string]interface{}{"experiment_id": "exp-001"}},
	}

	reader := &stubBusReader{events: events}
	p := buildTestProvider(nil, nil, reader)
	liveAny, err := p.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	ls := liveAny.(*EvalLiveState)

	if len(ls.Trials) != 3 {
		t.Errorf("expected 3 trials, got %d", len(ls.Trials))
	}
	if len(ls.Scorecards) != 2 {
		t.Errorf("expected 2 scorecards (exp-001, exp-002), got %d: %v", len(ls.Scorecards), keys(ls.Scorecards))
	}

	sc001 := ls.Scorecards["exp-001"]
	if sc001 == nil {
		t.Fatal("no scorecard for exp-001")
	}
	if len(sc001.TaskIDs) != 2 {
		t.Errorf("exp-001 scorecard: expected 2 task IDs, got %v", sc001.TaskIDs)
	}
}

// ---------------------------------------------------------------------------
// TestComputePlan_EmptyLive: no prior runs → EvalActionRun
// ---------------------------------------------------------------------------

func TestComputePlan_EmptyLive(t *testing.T) {
	p := buildTestProvider(nil, nil, nil)

	cfg := &EvalConfig{
		Experiments: map[string]*Experiment{
			"exp-001": {
				ID:            "exp-001",
				AutoReconcile: true,
				TaskIDs:       []string{"task-1"},
				VariantAxes:   map[string][]string{"system_prompt": {"sp-1"}},
				Target:        "test",
			},
		},
		BaselinePins: map[string]string{},
	}
	ls := &EvalLiveState{
		Scorecards: map[string]*Scorecard{},
	}

	plan, err := p.ComputePlan(cfg, ls, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan == nil {
		t.Fatal("plan is nil")
	}

	// Should have exactly one action: EvalActionRun for exp-001
	actions := filterNonSkip(plan.Actions)
	if len(actions) != 1 {
		t.Errorf("expected 1 non-skip action, got %d: %v", len(actions), actionSummary(plan.Actions))
		return
	}
	ea, _ := actions[0].Details["eval_action"].(string)
	if ea != string(EvalActionRun) {
		t.Errorf("expected eval_action=%q, got %q", EvalActionRun, ea)
	}
}

// ---------------------------------------------------------------------------
// TestComputePlan_FullLive: all cells present, no regression → all skip
// ---------------------------------------------------------------------------

func TestComputePlan_FullLive(t *testing.T) {
	p := buildTestProvider(nil, nil, nil)

	cfg := &EvalConfig{
		Experiments: map[string]*Experiment{
			"exp-001": {
				ID:            "exp-001",
				AutoReconcile: true,
				TaskIDs:       []string{"task-1"},
				VariantAxes:   map[string][]string{"system_prompt": {"sp-1"}},
				Target:        "test",
			},
		},
		BaselinePins: map[string]string{
			"exp-001": "some-run-id",
		},
	}

	// Build a scorecard with all cells populated
	passed := true
	sc := &Scorecard{
		ExperimentID: "exp-001",
		VariantKeys:  []string{"sp-1 / td-1-current"},
		TaskIDs:      []string{"task-1"},
		Cells: map[[2]string]ScorecardCell{
			{"sp-1 / td-1-current", "task-1"}: &passed,
		},
	}
	// Include a recent trial so isBaselineStale sees a non-stale run timestamp.
	// Without this, the pin is set but latestRunAt is "" → stale → refresh_baseline planned.
	recentTimestamp := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	ls := &EvalLiveState{
		Trials: []TrialRecord{
			{
				TrialID:      "exp-001__sp-1+td-1__task-1",
				ExperimentID: "exp-001",
				TaskID:       "task-1",
				Passed:       true,
				Timestamp:    recentTimestamp,
			},
		},
		Scorecards: map[string]*Scorecard{"exp-001": sc},
	}

	plan, err := p.ComputePlan(cfg, ls, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	// All actions should be skips (in sync)
	for _, a := range plan.Actions {
		if a.Action != reconcile.ActionSkip {
			ea, _ := a.Details["eval_action"].(string)
			t.Errorf("expected skip but got action=%q eval_action=%q", a.Action, ea)
		}
	}
}

// ---------------------------------------------------------------------------
// TestComputePlan_AutoReconcileFalse: skips unless triggered
// ---------------------------------------------------------------------------

func TestComputePlan_AutoReconcileFalse(t *testing.T) {
	p := buildTestProvider(nil, nil, nil)

	cfg := &EvalConfig{
		Experiments: map[string]*Experiment{
			"exp-001": {
				ID:            "exp-001",
				AutoReconcile: false, // on-demand only
				TaskIDs:       []string{"task-1"},
				VariantAxes:   map[string][]string{"system_prompt": {"sp-1"}},
				Target:        "test",
			},
		},
		BaselinePins: map[string]string{},
	}
	ls := &EvalLiveState{Scorecards: map[string]*Scorecard{}}

	plan, err := p.ComputePlan(cfg, ls, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	// Should be skipped (auto_reconcile=false, no trigger)
	for _, a := range plan.Actions {
		reason, _ := a.Details["reason"].(string)
		if a.Action != reconcile.ActionSkip {
			t.Errorf("expected skip, got %q (reason: %s)", a.Action, reason)
		}
		if !strings.Contains(reason, "on-demand") && !strings.Contains(reason, "auto_reconcile") {
			t.Errorf("expected reason to mention auto_reconcile, got %q", reason)
		}
	}
}

// ---------------------------------------------------------------------------
// TestComputePlan_CircuitBreaker: suspended experiments skip
// ---------------------------------------------------------------------------

func TestComputePlan_CircuitBreaker(t *testing.T) {
	p := buildTestProvider(nil, nil, nil)

	cfg := &EvalConfig{
		Experiments: map[string]*Experiment{
			"exp-001": {
				ID:            "exp-001",
				AutoReconcile: true,
				TaskIDs:       []string{"task-1"},
				VariantAxes:   map[string][]string{"system_prompt": {"sp-1"}},
				Target:        "test",
			},
		},
		BaselinePins: map[string]string{},
	}
	ls := &EvalLiveState{Scorecards: map[string]*Scorecard{}}

	// Simulate circuit breaker tripped (> 3 failures)
	eps := EvalProviderState{
		RecentFailureCounts:     map[string]int{"exp-001": 5},
		CircuitBreakerThreshold: 3,
	}
	epsJSON, _ := json.Marshal(eps)
	var epsMap map[string]any
	_ = json.Unmarshal(epsJSON, &epsMap)
	state := &reconcile.State{
		Metadata: map[string]any{"eval_state": epsMap},
	}

	plan, err := p.ComputePlan(cfg, ls, state)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	for _, a := range plan.Actions {
		reason, _ := a.Details["reason"].(string)
		if a.Action != reconcile.ActionSkip {
			t.Errorf("expected skip (circuit breaker), got %q", a.Action)
		}
		if !strings.Contains(reason, "suspended") && !strings.Contains(reason, "circuit") {
			t.Errorf("expected reason to mention suspended/circuit, got %q", reason)
		}
	}
}

// ---------------------------------------------------------------------------
// TestApplyPlan_StubDispatcher: dispatches one trial, emits CogBlock
// ---------------------------------------------------------------------------

func TestApplyPlan_StubDispatcher(t *testing.T) {
	emitter := &stubEmitter{}
	dispatcher := &stubDispatcher{
		results: []DispatchResult{{
			Index:   0,
			Success: true,
			Content: "The kernel trust score is 1.0",
		}},
	}
	p := buildTestProvider(dispatcher, emitter, nil)
	p.root = t.TempDir() // needed for expandExperimentMatrix fallback

	// Build a plan with one trial to run
	spec := TrialSpec{
		TrialID:      "exp-001__sp-1+td-1__task-1",
		ExperimentID: "exp-001",
		TaskVariant: Variant{
			ID:    "task-1",
			Class: "task",
			Content: map[string]interface{}{
				"prompt": "What is the trust score?",
				"rubric": map[string]interface{}{
					"content_contains": []interface{}{"trust"},
				},
			},
		},
		VariantIDs: map[string]string{"system_prompt": "sp-1", "tool_description": "td-1"},
		Target:     "test",
	}

	specsJSON, _ := json.Marshal([]TrialSpec{spec})
	var specsAny interface{}
	_ = json.Unmarshal(specsJSON, &specsAny)

	plan := &reconcile.Plan{
		ResourceType: "eval",
		Actions: []reconcile.Action{
			{
				Action:       reconcile.ActionCreate,
				ResourceType: "eval",
				Name:         "eval.exp-001",
				Details: map[string]any{
					"experiment_id": "exp-001",
					"eval_action":   string(EvalActionRunIncremental),
					"trial_specs":   specsAny,
				},
			},
		},
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].Status == reconcile.ApplyFailed {
		t.Errorf("trial failed: %s", results[0].Error)
	}

	// Should have emitted at least one CogBlock
	if len(emitter.emitted) == 0 {
		t.Error("expected at least one CogBlock emitted")
	} else {
		ch, _ := emitter.emitted[0]["_channel"].(string)
		if ch != "bus_tournament" {
			t.Errorf("CogBlock emitted to channel %q, want bus_tournament", ch)
		}
	}
}

// ---------------------------------------------------------------------------
// TestApplyPlan_NoDispatcher: graceful degradation
// ---------------------------------------------------------------------------

func TestApplyPlan_NoDispatcher(t *testing.T) {
	p := buildTestProvider(nil, nil, nil)
	plan := &reconcile.Plan{
		Actions: []reconcile.Action{
			{
				Action: reconcile.ActionCreate,
				Name:   "eval.exp-001",
				Details: map[string]any{
					"experiment_id": "exp-001",
					"eval_action":   string(EvalActionRun),
				},
			},
		},
	}
	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != reconcile.ApplySkipped {
		t.Errorf("expected ApplySkipped when dispatcher=nil, got %v", results[0].Status)
	}
}

// ---------------------------------------------------------------------------
// TestBuildState: constructs resources per experiment
// ---------------------------------------------------------------------------

func TestBuildState(t *testing.T) {
	p := buildTestProvider(nil, nil, nil)

	passed := true
	failed := false
	cfg := &EvalConfig{
		Experiments: map[string]*Experiment{
			"exp-001": {ID: "exp-001", Title: "Test Experiment"},
		},
		BaselinePins: map[string]string{"exp-001": "run-abc"},
	}
	ls := &EvalLiveState{
		Trials: []TrialRecord{
			makeTrialRecord("exp-001", "sp-1", "td-1", "task-1", true),
			makeTrialRecord("exp-001", "sp-1", "td-1", "task-2", false),
		},
		Scorecards: map[string]*Scorecard{
			"exp-001": {
				ExperimentID: "exp-001",
				VariantKeys:  []string{"sp-1 / td-1"},
				TaskIDs:      []string{"task-1", "task-2"},
				Cells: map[[2]string]ScorecardCell{
					{"sp-1 / td-1", "task-1"}: &passed,
					{"sp-1 / td-1", "task-2"}: &failed,
				},
			},
		},
	}

	state, err := p.BuildState(cfg, ls, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if state == nil {
		t.Fatal("state is nil")
	}

	// Find the resource for exp-001
	var res *reconcile.Resource
	for i := range state.Resources {
		if state.Resources[i].Address == "eval.exp-001" {
			res = &state.Resources[i]
			break
		}
	}
	if res == nil {
		t.Fatalf("no resource for eval.exp-001; resources: %v", state.Resources)
	}

	pin, _ := res.Attributes["baseline_pinned"].(string)
	if pin != "run-abc" {
		t.Errorf("baseline_pinned = %q, want run-abc", pin)
	}

	trialCount, _ := res.Attributes["trial_count"].(int)
	if trialCount != 2 {
		t.Errorf("trial_count = %d, want 2", trialCount)
	}

	pr, _ := res.Attributes["pass_rate"].(float64)
	if pr != 0.5 {
		t.Errorf("pass_rate = %f, want 0.5", pr)
	}

	// eval_state should be in metadata
	if _, ok := state.Metadata["eval_state"]; !ok {
		t.Error("expected eval_state in metadata")
	}
}

// ---------------------------------------------------------------------------
// TestHealth_Initial: reports unknown until reconciled
// ---------------------------------------------------------------------------

func TestHealth_Initial(t *testing.T) {
	p := buildTestProvider(nil, nil, nil)
	h := p.Health()
	// After zero reconcile cycles, health is the zero value from New()
	if h.Message == "" {
		t.Error("expected non-empty health message")
	}
}

// ---------------------------------------------------------------------------
// TestScore_BasicRubric: port from Python scoring.py
// ---------------------------------------------------------------------------

func TestScore_BasicRubric(t *testing.T) {
	tests := []struct {
		name      string
		rubric    Rubric
		content   string
		toolCalls []string
		wantPass  bool
	}{
		{
			name:      "content_contains pass",
			rubric:    Rubric{ContentContains: []string{"trust"}},
			content:   "The trust score is 1.0",
			wantPass:  true,
		},
		{
			name:     "content_contains fail",
			rubric:   Rubric{ContentContains: []string{"trust"}},
			content:  "The score is 1.0",
			wantPass: false,
		},
		{
			name:     "content_contains_ci pass",
			rubric:   Rubric{ContentContainsCI: []string{"TRUST"}},
			content:  "The trust score is high",
			wantPass: true,
		},
		{
			name:     "content_contains_ci fail",
			rubric:   Rubric{ContentContainsCI: []string{"TRUST"}},
			content:  "The score is high",
			wantPass: false,
		},
		{
			name:      "expected_tools pass",
			rubric:    Rubric{ExpectedTools: []string{"cog_get_state"}},
			toolCalls: []string{"cog_get_state"},
			content:   "",
			wantPass:  true,
		},
		{
			name:      "expected_tools fail",
			rubric:    Rubric{ExpectedTools: []string{"cog_get_state"}},
			toolCalls: []string{"cog_read_cogdoc"},
			content:   "",
			wantPass:  false,
		},
		{
			name:      "forbidden_tools pass",
			rubric:    Rubric{ForbiddenTools: []string{"cog_write_cogdoc"}},
			toolCalls: []string{"cog_read_cogdoc"},
			content:   "",
			wantPass:  true,
		},
		{
			name:      "forbidden_tools fail",
			rubric:    Rubric{ForbiddenTools: []string{"cog_write_cogdoc"}},
			toolCalls: []string{"cog_write_cogdoc", "cog_read_cogdoc"},
			content:   "",
			wantPass:  false,
		},
		{
			name:      "first_tool_one_of pass",
			rubric:    Rubric{FirstToolOneOf: []string{"cog_read_cogdoc"}},
			toolCalls: []string{"cog_read_cogdoc", "cog_get_state"},
			content:   "",
			wantPass:  true,
		},
		{
			name:      "first_tool_one_of fail",
			rubric:    Rubric{FirstToolOneOf: []string{"cog_read_cogdoc"}},
			toolCalls: []string{"cog_get_state", "cog_read_cogdoc"},
			content:   "",
			wantPass:  false,
		},
		{
			name:      "expected_tools_any_of pass",
			rubric:    Rubric{ExpectedToolsAnyOf: []string{"cog_get_state", "cog_check_coherence"}},
			toolCalls: []string{"cog_check_coherence"},
			content:   "",
			wantPass:  true,
		},
		{
			name:      "expected_tools_any_of fail",
			rubric:    Rubric{ExpectedToolsAnyOf: []string{"cog_get_state", "cog_check_coherence"}},
			toolCalls: []string{"cog_read_cogdoc"},
			content:   "",
			wantPass:  false,
		},
		{
			name:     "content_must_not_contain pass",
			rubric:   Rubric{ContentMustNotContain: []string{"error"}},
			content:  "success",
			wantPass: true,
		},
		{
			name:     "content_must_not_contain fail",
			rubric:   Rubric{ContentMustNotContain: []string{"error"}},
			content:  "an error occurred",
			wantPass: false,
		},
		{
			name:     "content_must_not_contain_ci fail",
			rubric:   Rubric{ContentMustNotContainCI: []string{"ERROR"}},
			content:  "An error occurred",
			wantPass: false,
		},
		{
			name:     "all pass combined",
			rubric: Rubric{
				ExpectedTools:    []string{"cog_get_state"},
				ContentContains:  []string{"trust"},
				ForbiddenTools:   []string{"bad_tool"},
				FirstToolOneOf:   []string{"cog_get_state"},
			},
			toolCalls: []string{"cog_get_state"},
			content:   "The trust score is 1",
			wantPass:  true,
		},
		{
			name:     "empty rubric always passes",
			rubric:   Rubric{},
			content:  "",
			wantPass: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &staticScoredResult{
				content:   tt.content,
				toolCalls: tt.toolCalls,
			}
			verdict := Score(tt.rubric, result)
			if verdict.Passed != tt.wantPass {
				t.Errorf("Score: passed=%v, want %v; failures=%v", verdict.Passed, tt.wantPass, verdict.Failures)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestBuildScorecard: aggregation logic port from Python compare.py
// ---------------------------------------------------------------------------

func TestBuildScorecard_Aggregation(t *testing.T) {
	// Multiple trials for same cell: pass if any pass
	trials := []TrialRecord{
		makeTrialRecord("exp-001", "sp-1-production", "td-1-current", "task-1", false),
		makeTrialRecord("exp-001", "sp-1-production", "td-1-current", "task-1", true), // second attempt passes
		makeTrialRecord("exp-001", "sp-1-production", "td-1-current", "task-2", false),
	}

	sc := buildScorecard("exp-001", trials)
	if sc == nil {
		t.Fatal("scorecard is nil")
	}
	if sc.ExperimentID != "exp-001" {
		t.Errorf("ExperimentID = %q, want exp-001", sc.ExperimentID)
	}

	// task-1: any pass → true
	key1 := [2]string{"sp-1-production / td-1-current", "task-1"}
	cell1 := sc.Cells[key1]
	if cell1 == nil || !*cell1 {
		t.Errorf("task-1 should be true (any-pass aggregation), got %v", cell1)
	}

	// task-2: all fail → false
	key2 := [2]string{"sp-1-production / td-1-current", "task-2"}
	cell2 := sc.Cells[key2]
	if cell2 == nil || *cell2 {
		t.Errorf("task-2 should be false (all-fail aggregation), got %v", cell2)
	}
}

// ---------------------------------------------------------------------------
// TestParseEvalProviderState: round-trips through state metadata
// ---------------------------------------------------------------------------

func TestParseEvalProviderState_RoundTrip(t *testing.T) {
	eps := EvalProviderState{
		InFlightTrialIDs:        []string{"trial-1", "trial-2"},
		RecentFailureCounts:     map[string]int{"exp-001": 2},
		LastReconcileAt:         "2026-04-25T10:00:00Z",
		CircuitBreakerThreshold: 5,
	}

	b, _ := json.Marshal(eps)
	var epsMap map[string]any
	_ = json.Unmarshal(b, &epsMap)

	state := &reconcile.State{
		Metadata: map[string]any{"eval_state": epsMap},
	}

	restored := parseEvalProviderState(state)
	if len(restored.InFlightTrialIDs) != 2 {
		t.Errorf("InFlightTrialIDs = %v, want 2", restored.InFlightTrialIDs)
	}
	if restored.CircuitBreakerThreshold != 5 {
		t.Errorf("CircuitBreakerThreshold = %d, want 5", restored.CircuitBreakerThreshold)
	}
	if restored.RecentFailureCounts["exp-001"] != 2 {
		t.Errorf("RecentFailureCounts[exp-001] = %d, want 2", restored.RecentFailureCounts["exp-001"])
	}
}

// ---------------------------------------------------------------------------
// TestWritePinBaseline: writes JSON file correctly
// ---------------------------------------------------------------------------

func TestWritePinBaseline(t *testing.T) {
	dir := t.TempDir()
	err := writePinBaseline(dir, "exp-001", "run-abc123")
	if err != nil {
		t.Fatalf("writePinBaseline: %v", err)
	}

	pinsPath := filepath.Join(dir, ".cog", "state", "eval-baselines.json")
	data, err := os.ReadFile(pinsPath)
	if err != nil {
		t.Fatalf("read pins file: %v", err)
	}

	var pins map[string]string
	if err := json.Unmarshal(data, &pins); err != nil {
		t.Fatalf("unmarshal pins: %v", err)
	}
	if pins["exp-001"] != "run-abc123" {
		t.Errorf("pin for exp-001 = %q, want run-abc123", pins["exp-001"])
	}

	// Second write merges
	err = writePinBaseline(dir, "exp-002", "run-xyz")
	if err != nil {
		t.Fatalf("writePinBaseline (second): %v", err)
	}
	data, _ = os.ReadFile(pinsPath)
	var pins2 map[string]string
	_ = json.Unmarshal(data, &pins2)
	if pins2["exp-001"] != "run-abc123" {
		t.Error("exp-001 pin should be preserved after second write")
	}
	if pins2["exp-002"] != "run-xyz" {
		t.Errorf("exp-002 pin = %q, want run-xyz", pins2["exp-002"])
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// staticScoredResult implements ScoredResult with fixed values.
type staticScoredResult struct {
	content   string
	toolCalls []string
}

func (r *staticScoredResult) Content() string        { return r.content }
func (r *staticScoredResult) ToolCallNames() []string { return r.toolCalls }
func (r *staticScoredResult) FinishReason() string {
	if len(r.toolCalls) > 0 && r.content == "" {
		return "tool_calls"
	}
	return "stop"
}

// keys returns the sorted keys of any map.
func keys[K comparable, V any](m map[K]V) []K {
	ks := make([]K, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// filterNonSkip returns actions that are not ActionSkip.
func filterNonSkip(actions []reconcile.Action) []reconcile.Action {
	var result []reconcile.Action
	for _, a := range actions {
		if a.Action != reconcile.ActionSkip {
			result = append(result, a)
		}
	}
	return result
}

// actionSummary returns a human-readable summary of actions for debugging.
func actionSummary(actions []reconcile.Action) []string {
	var s []string
	for _, a := range actions {
		ea, _ := a.Details["eval_action"].(string)
		reason, _ := a.Details["reason"].(string)
		s = append(s, string(a.Action)+"/"+ea+":"+reason)
	}
	return s
}

// ---------------------------------------------------------------------------
// TestIsBaselineStale: Bug 1 — verifies the 7-day staleness check is applied
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// TestWriteDispatchTrigger / TestComputePlan_DispatchTrigger: Bug 2
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// TestExtractToolCallNamesFromResult: Bug 3 — tool call extraction
// ---------------------------------------------------------------------------

func TestExtractToolCallNamesFromResult_WithToolCalls(t *testing.T) {
	dr := DispatchResult{
		Index:   0,
		Success: true,
		Content: "done",
		ToolCalls: []DispatchToolCallSummary{
			{Name: "cog_get_state", ArgsDigest: "abc"},
			{Name: "cog_read_cogdoc", ArgsDigest: "def"},
		},
	}
	names := extractToolCallNamesFromResult(dr)
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %v", names)
	}
	if names[0] != "cog_get_state" || names[1] != "cog_read_cogdoc" {
		t.Errorf("unexpected names: %v", names)
	}
}

func TestExtractToolCallNamesFromResult_NoToolCalls(t *testing.T) {
	dr := DispatchResult{Index: 0, Success: true, Content: "done"}
	names := extractToolCallNamesFromResult(dr)
	if names != nil {
		t.Errorf("expected nil for empty tool calls, got %v", names)
	}
}

func TestExtractToolCallNamesFromResult_EmptyNames(t *testing.T) {
	// ToolCalls present but all names empty — should return nil
	dr := DispatchResult{
		ToolCalls: []DispatchToolCallSummary{{Name: ""}, {Name: ""}},
	}
	names := extractToolCallNamesFromResult(dr)
	if names != nil {
		t.Errorf("expected nil when all names are empty, got %v", names)
	}
}

func TestApplyPlan_ToolCallsReachRubric(t *testing.T) {
	// Verify that DispatchResult.ToolCalls flow into the rubric scorer.
	// The trial should PASS because cog_get_state appears in the result.
	emitter := &stubEmitter{}
	dispatcher := &stubDispatcher{
		results: []DispatchResult{{
			Index:   0,
			Success: true,
			Content: "state retrieved",
			ToolCalls: []DispatchToolCallSummary{
				{Name: "cog_get_state"},
			},
		}},
	}
	p := buildTestProvider(dispatcher, emitter, nil)
	p.root = t.TempDir()

	specsJSON, _ := json.Marshal([]TrialSpec{{
		TrialID:      "exp-tc__sp-1+td-1__task-1",
		ExperimentID: "exp-tc",
		TaskVariant: Variant{
			ID:    "task-1",
			Class: "task",
			Content: map[string]interface{}{
				"prompt": "Call cog_get_state",
				"rubric": map[string]interface{}{
					"expected_tools": []interface{}{"cog_get_state"},
				},
			},
		},
		VariantIDs: map[string]string{"system_prompt": "sp-1", "tool_description": "td-1"},
		Target:     "test",
	}})
	var specsAny interface{}
	_ = json.Unmarshal(specsJSON, &specsAny)

	plan := &reconcile.Plan{
		ResourceType: "eval",
		Actions: []reconcile.Action{{
			Action:       reconcile.ActionCreate,
			ResourceType: "eval",
			Name:         "eval.exp-tc",
			Details: map[string]any{
				"experiment_id": "exp-tc",
				"eval_action":   string(EvalActionRunIncremental),
				"trial_specs":   specsAny,
			},
		}},
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no results")
	}
	if results[0].Status == reconcile.ApplyFailed {
		t.Errorf("trial failed (tool call rubric not satisfied): %s", results[0].Error)
	}
	// The trial must have passed (cog_get_state present in ToolCalls)
	if len(emitter.emitted) == 0 {
		t.Fatal("no CogBlock emitted")
	}
	trialRaw := emitter.emitted[0]["trial"]
	b, _ := json.Marshal(trialRaw)
	var tr TrialRecord
	_ = json.Unmarshal(b, &tr)
	if !tr.Passed {
		t.Errorf("trial should have passed (cog_get_state in expected_tools), failures: %v", tr.Failures)
	}
}

func TestWriteDispatchTrigger_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	if err := writeDispatchTrigger(dir, "exp-001", false); err != nil {
		t.Fatalf("writeDispatchTrigger: %v", err)
	}

	triggers := readAndClearDispatchTriggers(dir)
	if !triggers["exp-001"] && triggers["exp-001"] != false {
		// key must be present (false = non-force trigger)
	}
	if _, ok := triggers["exp-001"]; !ok {
		t.Error("expected exp-001 key in triggers")
	}
}

func TestWriteDispatchTrigger_ForceUpgrade(t *testing.T) {
	dir := t.TempDir()
	// Write non-force first
	_ = writeDispatchTrigger(dir, "exp-001", false)
	// Upgrade to force
	_ = writeDispatchTrigger(dir, "exp-001", true)
	triggers := readAndClearDispatchTriggers(dir)
	if !triggers["exp-001"] {
		t.Error("expected force=true after upgrade")
	}
}

func TestReadAndClearDispatchTriggers_ClearsFile(t *testing.T) {
	dir := t.TempDir()
	_ = writeDispatchTrigger(dir, "exp-001", false)
	// First read consumes the trigger
	triggers1 := readAndClearDispatchTriggers(dir)
	if _, ok := triggers1["exp-001"]; !ok {
		t.Error("expected trigger on first read")
	}
	// Second read should find no triggers (file cleared)
	triggers2 := readAndClearDispatchTriggers(dir)
	if _, ok := triggers2["exp-001"]; ok {
		t.Error("expected trigger cleared after first read")
	}
}

func TestComputePlan_DispatchTriggerFromFile(t *testing.T) {
	dir := t.TempDir()

	// Write a trigger for exp-001
	if err := writeDispatchTrigger(dir, "exp-001", false); err != nil {
		t.Fatalf("writeDispatchTrigger: %v", err)
	}

	p := buildTestProvider(nil, nil, nil)
	p.root = dir

	cfg := &EvalConfig{
		Experiments: map[string]*Experiment{
			"exp-001": {
				ID:            "exp-001",
				AutoReconcile: false, // on-demand only — needs trigger
				TaskIDs:       []string{"task-1"},
				VariantAxes:   map[string][]string{"system_prompt": {"sp-1"}},
				Target:        "test",
			},
		},
		BaselinePins: map[string]string{},
	}
	// FetchLive drains the trigger sidecar into the live state (the read-and-
	// clear moved here from ComputePlan to keep ComputePlan pure); ComputePlan
	// then reads the trigger from live.
	liveAny, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	ls := liveAny.(*EvalLiveState)
	if _, ok := ls.DispatchTriggers["exp-001"]; !ok {
		t.Fatalf("FetchLive did not drain the trigger into live state: %+v", ls.DispatchTriggers)
	}

	plan, err := p.ComputePlan(cfg, ls, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	// Should have a non-skip action for exp-001 (trigger overrides auto_reconcile=false)
	actions := filterNonSkip(plan.Actions)
	if len(actions) == 0 {
		t.Errorf("expected non-skip action when trigger is set, got all skips: %v", actionSummary(plan.Actions))
	}

	// And the sidecar must be drained — a second FetchLive sees no trigger.
	if t2 := readAndClearDispatchTriggers(dir); len(t2) != 0 {
		t.Errorf("trigger not cleared by FetchLive: %v", t2)
	}
}

func TestIsBaselineStale_NoPin(t *testing.T) {
	exp := &Experiment{ID: "exp-001"}
	pins := map[string]string{} // no pin set
	if !isBaselineStale(exp, pins, "") {
		t.Error("expected stale=true when no pin set")
	}
}

func TestIsBaselineStale_PinSetRecentRun(t *testing.T) {
	exp := &Experiment{ID: "exp-001"}
	pins := map[string]string{"exp-001": "run-abc"}
	// Recent run (1 hour ago) — should NOT be stale
	recent := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	if isBaselineStale(exp, pins, recent) {
		t.Error("expected stale=false for recent run with pin set")
	}
}

func TestIsBaselineStale_PinSetOldRun(t *testing.T) {
	exp := &Experiment{ID: "exp-001"}
	pins := map[string]string{"exp-001": "run-abc"}
	// Old run (8 days ago) — should be stale
	old := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339)
	if !isBaselineStale(exp, pins, old) {
		t.Error("expected stale=true for run older than 7 days with pin set")
	}
}

func TestIsBaselineStale_PinSetNeverRun(t *testing.T) {
	exp := &Experiment{ID: "exp-001"}
	pins := map[string]string{"exp-001": "run-abc"}
	// Pin set but latestRunAt="" means never run — should be stale
	if !isBaselineStale(exp, pins, "") {
		t.Error("expected stale=true when pin is set but no run has occurred")
	}
}

func TestIsBaselineStale_PinSetBoundary(t *testing.T) {
	exp := &Experiment{ID: "exp-001"}
	pins := map[string]string{"exp-001": "run-abc"}
	// Exactly at the boundary: 7 days - 1 minute ago, should NOT be stale
	justUnder := time.Now().UTC().Add(-(7*24*time.Hour - time.Minute)).Format(time.RFC3339)
	if isBaselineStale(exp, pins, justUnder) {
		t.Error("expected stale=false for run just under 7 days ago")
	}
}

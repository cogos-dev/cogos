// provider_impl.go — Phase C implementation of EvalProvider Reconcilable methods.
//
// Implements the six Reconcilable methods for EvalProvider:
//   - LoadConfig: parse experiment cogdocs + baseline pins
//   - FetchLive: read bus_tournament events, build scorecards
//   - ComputePlan: 8-rule priority chain
//   - ApplyPlan: trial dispatch loop (one trial per cycle, budget-gated)
//   - BuildState: one reconcile.Resource per experiment
//   - Health: three-axis status
//
// Also implements parseEvalProviderState, variantKey, and buildScorecard helpers.
//
// Python reference implementations:
//   - evals/tournament/variants.py — variant loader + cogdoc parsing
//   - evals/tournament/matrix.py   — Experiment + matrix expansion
//   - evals/tournament/runner.py   — trial dispatch loop
//   - evals/tournament/compare.py  — Scorecard + regression detection
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/reconcile"
	"gopkg.in/yaml.v3"
)

// nowISO delegates to the wired NowISO function, falling back to time.Now().
// Pattern mirrors component_provider.go nowISO() (lines 390-396).
func nowISO() string {
	if NowISO != nil {
		return NowISO()
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// trialsPerCycle is the maximum number of trials dispatched per ApplyPlan call.
// Per design memo Q4: fine-grain (one trial per cycle) is the default; bump
// to 3 for faster lab runs when Ollama has capacity.
const trialsPerCycle = 1

// regressionThreshold is the pass-rate drop (0.0–1.0) that triggers a retry.
// 10pp = 0.10, matching the Python compare.py default.
const regressionThreshold = 0.10

// baselineStaleDays is the number of days after which a baseline pin is stale.
const baselineStaleDays = 7

// circuitBreakerDefaultThreshold is the default consecutive-failure count
// above which an experiment is suspended. See design memo Q9.
const circuitBreakerDefaultThreshold = 3

// applyMu serializes trial dispatch so only one eval trial hits Ollama at
// a time (per design memo Q5 / feedback_ollama_single_thread_constraint.md).
var applyMu sync.Mutex

// ---------------------------------------------------------------------------
// LoadConfig
// ---------------------------------------------------------------------------

// LoadConfig loads declared eval configuration from the workspace.
//
// Reads:
//  1. All .cog.md files under <root>/.cog/mem/semantic/architecture/tournament/experiments/
//  2. Baseline pins from <root>/.cog/state/eval-baselines.json
//
// Returns *EvalConfig.
func (e *EvalProvider) LoadConfig(root string) (any, error) {
	e.root = root

	experimentsDir := filepath.Join(root, ".cog", "mem", "semantic", "architecture", "tournament", "experiments")

	entries, err := os.ReadDir(experimentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No experiments yet — return empty config
			return &EvalConfig{
				Experiments:    map[string]*Experiment{},
				BaselinePins:   map[string]string{},
				TournamentRoot: filepath.Join(root, ".cog", "mem", "semantic", "architecture", "tournament"),
			}, nil
		}
		return nil, fmt.Errorf("eval: read experiments dir %s: %w", experimentsDir, err)
	}

	experiments := make(map[string]*Experiment)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".cog.md") {
			continue
		}
		path := filepath.Join(experimentsDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // skip unreadable files
		}
		exp, err := parseCogdocExperiment(raw, path)
		if err != nil || exp == nil {
			continue
		}
		experiments[exp.ID] = exp
	}

	// Load baseline pins from .cog/state/eval-baselines.json
	pins := map[string]string{}
	pinsPath := filepath.Join(root, ".cog", "state", "eval-baselines.json")
	if data, err := os.ReadFile(pinsPath); err == nil {
		_ = json.Unmarshal(data, &pins) // ignore parse errors — start fresh
	}

	// Apply pins to experiment structs
	for expID, runID := range pins {
		if exp, ok := experiments[expID]; ok {
			exp.BaselinePinned = runID
		}
	}

	return &EvalConfig{
		Experiments:    experiments,
		BaselinePins:   pins,
		TournamentRoot: filepath.Join(root, ".cog", "mem", "semantic", "architecture", "tournament"),
	}, nil
}

// parseCogdocExperiment parses a .cog.md file's YAML frontmatter into an Experiment.
// Returns nil if the frontmatter is missing or the file isn't an experiment cogdoc.
func parseCogdocExperiment(raw []byte, path string) (*Experiment, error) {
	text := string(raw)
	fm, err := splitFrontmatter(text)
	if err != nil || fm == nil {
		return nil, nil
	}

	// Only parse experiment-type cogdocs
	typ, _ := fm["type"].(string)
	if typ != "workflow.experiment" {
		return nil, nil
	}

	id, _ := fm["id"].(string)
	if id == "" {
		// Fall back to file stem
		base := filepath.Base(path)
		id = strings.TrimSuffix(base, ".cog.md")
	}

	title, _ := fm["title"].(string)
	baselineVariant, _ := fm["baseline_variant"].(string)
	target, _ := fm["target"].(string)
	if target == "" {
		target = "laptop-lms"
	}

	// Parse variant_axes — YAML may nest as map[string]interface{}
	variantAxes := map[string][]string{}
	if raw, ok := fm["variants"]; ok {
		if varMap, ok := raw.(map[string]interface{}); ok {
			for k, v := range varMap {
				// Normalize key: yaml may use system_prompt or system-prompt
				key := strings.ReplaceAll(k, "-", "_")
				switch vals := v.(type) {
				case []interface{}:
					strs := make([]string, 0, len(vals))
					for _, s := range vals {
						if str, ok := s.(string); ok {
							strs = append(strs, str)
						}
					}
					variantAxes[key] = strs
				case string:
					variantAxes[key] = []string{vals}
				}
			}
		}
	}

	// Parse task_ids from "tasks" key
	taskIDs := []string{}
	if rawTasks, ok := fm["tasks"]; ok {
		switch v := rawTasks.(type) {
		case []interface{}:
			for _, t := range v {
				if s, ok := t.(string); ok {
					taskIDs = append(taskIDs, s)
				}
			}
		case []string:
			taskIDs = append(taskIDs, v...)
		}
	}

	// Parse tags
	tags := []string{}
	if rawTags, ok := fm["tags"]; ok {
		if tagSlice, ok := rawTags.([]interface{}); ok {
			for _, t := range tagSlice {
				if s, ok := t.(string); ok {
					tags = append(tags, s)
				}
			}
		}
	}

	autoReconcile, _ := fm["auto_reconcile"].(bool)

	return &Experiment{
		ID:              id,
		Title:           title,
		BaselineVariant: baselineVariant,
		VariantAxes:     variantAxes,
		TaskIDs:         taskIDs,
		Target:          target,
		Tags:            tags,
		AutoReconcile:   autoReconcile,
	}, nil
}

// splitFrontmatter parses YAML frontmatter from a cogdoc string.
// Returns the frontmatter map and any parse error.
// Port of evals/tournament/variants.py _split_frontmatter().
func splitFrontmatter(text string) (map[string]interface{}, error) {
	if !strings.HasPrefix(text, "---") {
		return nil, nil
	}
	rest := text[3:]
	end := strings.Index(rest, "\n---")
	if end == -1 {
		return nil, nil
	}
	fmText := rest[:end]

	var fm map[string]interface{}
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return nil, err
	}
	return fm, nil
}

// ---------------------------------------------------------------------------
// FetchLive
// ---------------------------------------------------------------------------

// FetchLive reads all completed trial records from bus_tournament.
// Re-materializes scorecards inline per reconcile cycle (all-time, per design memo Q2).
func (e *EvalProvider) FetchLive(ctx context.Context, config any) (any, error) {
	if e.busReader == nil {
		// No reader wired — return empty live state (FetchLive degrades gracefully)
		return &EvalLiveState{
			Trials:     []TrialRecord{},
			Scorecards: map[string]*Scorecard{},
			FetchedAt:  nowISO(),
		}, nil
	}

	events, err := e.busReader.ReadChannel(ctx, "bus_tournament", "")
	if err != nil {
		// Degrade gracefully — kernel may not be reachable during tests
		return &EvalLiveState{
			Trials:     []TrialRecord{},
			Scorecards: map[string]*Scorecard{},
			FetchedAt:  nowISO(),
		}, nil
	}
	if events == nil {
		events = []BusEvent{}
	}

	// Deserialize TrialRecord from each event payload
	var trials []TrialRecord
	for _, ev := range events {
		if ev.Type != "tournament.trial.v1" {
			continue
		}
		tr, err := extractTrialRecord(ev)
		if err != nil || tr == nil {
			continue
		}
		trials = append(trials, *tr)
	}

	// Group by experiment and build scorecards
	byExp := map[string][]TrialRecord{}
	for _, tr := range trials {
		if tr.ExperimentID != "" {
			byExp[tr.ExperimentID] = append(byExp[tr.ExperimentID], tr)
		}
	}

	scorecards := make(map[string]*Scorecard, len(byExp))
	for expID, expTrials := range byExp {
		scorecards[expID] = buildScorecard(expID, expTrials)
	}

	return &EvalLiveState{
		Trials:     trials,
		Scorecards: scorecards,
		FetchedAt:  nowISO(),
	}, nil
}

// extractTrialRecord deserializes a TrialRecord from a bus event payload.
// The bus stores the trial nested as payload.content (JSON string) or payload.trial.
func extractTrialRecord(ev BusEvent) (*TrialRecord, error) {
	if ev.Payload == nil {
		return nil, nil
	}

	// The Python persist.py wraps the trial inside payload.content (a JSON string)
	// or as payload.trial (direct object). Try content first.
	if contentRaw, ok := ev.Payload["content"]; ok {
		if contentStr, ok := contentRaw.(string); ok {
			// content is a JSON string — unmarshal it
			var wrapper struct {
				Trial *TrialRecord `json:"trial"`
			}
			if err := json.Unmarshal([]byte(contentStr), &wrapper); err == nil && wrapper.Trial != nil {
				return wrapper.Trial, nil
			}
			// Try as a direct TrialRecord
			var tr TrialRecord
			if err := json.Unmarshal([]byte(contentStr), &tr); err == nil && tr.TrialID != "" {
				return &tr, nil
			}
		}
	}

	// Try payload.trial directly
	if trialRaw, ok := ev.Payload["trial"]; ok {
		b, err := json.Marshal(trialRaw)
		if err != nil {
			return nil, nil
		}
		var tr TrialRecord
		if err := json.Unmarshal(b, &tr); err == nil && tr.TrialID != "" {
			return &tr, nil
		}
	}

	return nil, nil
}

// variantKey produces the scorecard key for a trial.
// Mirrors compare.py _variant_key(): "sp-id / td-id".
func variantKey(tr TrialRecord) string {
	sp := tr.VariantIDs["system_prompt"]
	if sp == "" {
		sp = "unknown-sp"
	}
	td := tr.VariantIDs["tool_description"]
	if td == "" {
		td = "td-1-current"
	}
	return sp + " / " + td
}

// buildScorecard constructs a Scorecard from a list of TrialRecords.
// Port of evals/tournament/compare.py build_scorecard().
// Multiple trials for the same (variant_key, task_id) aggregate:
// cell is true if ANY trial passed, false if ALL failed.
func buildScorecard(experimentID string, trials []TrialRecord) *Scorecard {
	if len(trials) == 0 {
		return &Scorecard{ExperimentID: experimentID, Cells: map[[2]string]ScorecardCell{}}
	}

	variantKeys := map[string]struct{}{}
	taskIDs := map[string]struct{}{}
	raw := map[[2]string][]bool{}

	for _, tr := range trials {
		vk := variantKey(tr)
		variantKeys[vk] = struct{}{}
		taskIDs[tr.TaskID] = struct{}{}
		key := [2]string{vk, tr.TaskID}
		raw[key] = append(raw[key], tr.Passed)
	}

	sortedVKs := sortedKeys(variantKeys)
	sortedTIDs := sortedKeys(taskIDs)

	cells := make(map[[2]string]ScorecardCell, len(sortedVKs)*len(sortedTIDs))
	for _, vk := range sortedVKs {
		for _, tid := range sortedTIDs {
			key := [2]string{vk, tid}
			results, ok := raw[key]
			if !ok {
				cells[key] = nil
			} else {
				anyPassed := false
				for _, r := range results {
					if r {
						anyPassed = true
						break
					}
				}
				v := anyPassed
				cells[key] = &v
			}
		}
	}

	return &Scorecard{
		ExperimentID: experimentID,
		Cells:        cells,
		VariantKeys:  sortedVKs,
		TaskIDs:      sortedTIDs,
	}
}

// passRate computes the pass rate for a variant key across all tasks.
// Returns math.NaN() if no data.
func passRate(sc *Scorecard, variantKey string) float64 {
	if sc == nil {
		return math.NaN()
	}
	var total, passed int
	for _, tid := range sc.TaskIDs {
		cell := sc.Cells[[2]string{variantKey, tid}]
		if cell == nil {
			continue
		}
		total++
		if *cell {
			passed++
		}
	}
	if total == 0 {
		return math.NaN()
	}
	return float64(passed) / float64(total)
}

// ---------------------------------------------------------------------------
// ComputePlan
// ---------------------------------------------------------------------------

// ComputePlan implements the 8-rule priority chain.
// Rules 2 and 4 are additive (both can fire on one experiment per design memo).
func (e *EvalProvider) ComputePlan(config any, live any, state *reconcile.State) (*reconcile.Plan, error) {
	cfg, ok := config.(*EvalConfig)
	if !ok || cfg == nil {
		return nil, fmt.Errorf("eval: ComputePlan: config is not *EvalConfig")
	}
	ls, ok := live.(*EvalLiveState)
	if !ok || ls == nil {
		ls = &EvalLiveState{Scorecards: map[string]*Scorecard{}}
	}

	eps := parseEvalProviderState(state)
	if eps.CircuitBreakerThreshold == 0 {
		eps.CircuitBreakerThreshold = circuitBreakerDefaultThreshold
	}

	// Build set of in-flight trial IDs for fast lookup
	inFlight := map[string]struct{}{}
	for _, id := range eps.InFlightTrialIDs {
		inFlight[id] = struct{}{}
	}

	// Check for explicit dispatch triggers in state metadata
	dispatchTriggers := map[string]bool{}
	forcedExperiments := map[string]bool{}
	if state != nil && state.Metadata != nil {
		if raw, ok := state.Metadata["dispatch_triggers"]; ok {
			if b, err := json.Marshal(raw); err == nil {
				_ = json.Unmarshal(b, &dispatchTriggers)
			}
		}
		if raw, ok := state.Metadata["forced_experiments"]; ok {
			if b, err := json.Marshal(raw); err == nil {
				_ = json.Unmarshal(b, &forcedExperiments)
			}
		}
	}

	// Merge in triggers from the sidecar file written by cog_run_experiment.
	// This bridges the MCP-tool invocation → reconcile-cycle gap. The file is
	// cleared by readAndClearDispatchTriggers so each trigger fires exactly once.
	if e.root != "" {
		for expID, force := range readAndClearDispatchTriggers(e.root) {
			dispatchTriggers[expID] = true
			if force {
				forcedExperiments[expID] = true
			}
		}
	}

	var actions []reconcile.Action
	summary := reconcile.Summary{}

	// Sort experiment IDs for deterministic output
	expIDs := make([]string, 0, len(cfg.Experiments))
	for id := range cfg.Experiments {
		expIDs = append(expIDs, id)
	}
	sort.Strings(expIDs)

	for _, expID := range expIDs {
		exp := cfg.Experiments[expID]
		sc := ls.Scorecards[expID]
		failures := eps.RecentFailureCounts[expID]

		// Rule 5: in-flight guard — skip if any trials for this experiment are in flight
		hasInFlight := false
		for _, tid := range eps.InFlightTrialIDs {
			if strings.HasPrefix(tid, expID) {
				hasInFlight = true
				break
			}
		}
		if hasInFlight {
			actions = append(actions, skipAction(expID, "in-flight trials pending"))
			summary.Skipped++
			continue
		}

		// Rule 6: circuit breaker — skip suspended experiments
		if failures > eps.CircuitBreakerThreshold {
			if !forcedExperiments[expID] {
				actions = append(actions, skipAction(expID, "suspended: circuit breaker tripped"))
				summary.Skipped++
				continue
			}
			// force resets the circuit breaker
		}

		// Rule 7: auto_reconcile gate
		triggered := dispatchTriggers[expID] || forcedExperiments[expID]
		if !exp.AutoReconcile && !triggered {
			actions = append(actions, skipAction(expID, "on-demand only (auto_reconcile=false)"))
			summary.Skipped++
			continue
		}

		// Rule 1: no prior runs — plan a full run
		if sc == nil || len(sc.VariantKeys) == 0 {
			actions = append(actions, evalAction(expID, EvalActionRun, EvalPlanDetail{
				ExperimentID: expID,
				EvalAction:   EvalActionRun,
			}))
			summary.Creates++
			continue
		}

		// Rules 2 and 4 are additive — collect all that fire for this experiment
		var additiveActions []reconcile.Action

		// Rule 2: incremental cells — new variant cells not yet in scorecard
		newCells := findNewVariantCells(exp, sc)
		if len(newCells) > 0 {
			specs := buildTrialSpecsForCells(cfg, exp, newCells)
			additiveActions = append(additiveActions, evalAction(expID, EvalActionRunIncremental, EvalPlanDetail{
				ExperimentID: expID,
				EvalAction:   EvalActionRunIncremental,
				TrialSpecs:   specs,
			}))
			summary.Updates++
		}

		// Rule 3: stale baseline — run full refresh if baseline pin is stale/missing
		// Compute the latest run timestamp for this experiment from live trials.
		latestRunAt := ""
		for _, tr := range ls.Trials {
			if tr.ExperimentID == expID && tr.Timestamp > latestRunAt {
				latestRunAt = tr.Timestamp
			}
		}
		if isBaselineStale(exp, cfg.BaselinePins, latestRunAt) {
			additiveActions = append(additiveActions, evalAction(expID, EvalActionRefreshBaseline, EvalPlanDetail{
				ExperimentID: expID,
				EvalAction:   EvalActionRefreshBaseline,
			}))
			summary.Updates++
		}

		// Rule 4: regression — retry cells that regressed vs pinned baseline
		regressionCells := detectRegressions(exp, sc, cfg.BaselinePins)
		if len(regressionCells) > 0 {
			specs := buildTrialSpecsForCells(cfg, exp, regressionCells)
			additiveActions = append(additiveActions, evalAction(expID, EvalActionRetryRegression, EvalPlanDetail{
				ExperimentID:    expID,
				EvalAction:      EvalActionRetryRegression,
				TrialSpecs:      specs,
				RegressionCells: regressionCells,
			}))
			summary.Updates++
		}

		if len(additiveActions) > 0 {
			actions = append(actions, additiveActions...)
		} else {
			// Rule 8: healthy — no action needed
			actions = append(actions, skipAction(expID, "in sync"))
			summary.Skipped++
		}
	}

	return &reconcile.Plan{
		ResourceType: "eval",
		GeneratedAt:  nowISO(),
		Actions:      actions,
		Summary:      summary,
	}, nil
}

// skipAction builds a reconcile.Action with ActionSkip.
func skipAction(expID, reason string) reconcile.Action {
	return reconcile.Action{
		Action:       reconcile.ActionSkip,
		ResourceType: "eval",
		Name:         "eval." + expID,
		Details: map[string]any{
			"experiment_id": expID,
			"eval_action":   string(EvalActionSkip),
			"reason":        reason,
		},
	}
}

// evalAction builds a reconcile.Action with the given eval action type.
func evalAction(expID string, action EvalActionType, detail EvalPlanDetail) reconcile.Action {
	detailMap := map[string]any{
		"experiment_id": detail.ExperimentID,
		"eval_action":   string(detail.EvalAction),
	}
	if len(detail.TrialSpecs) > 0 {
		detailMap["trial_specs"] = detail.TrialSpecs
	}
	if len(detail.RegressionCells) > 0 {
		detailMap["regression_cells"] = detail.RegressionCells
	}
	if detail.StaleAfter != "" {
		detailMap["stale_after"] = detail.StaleAfter
	}

	at := reconcile.ActionCreate
	if action == EvalActionRunIncremental || action == EvalActionRetryRegression || action == EvalActionRefreshBaseline {
		at = reconcile.ActionUpdate
	}

	return reconcile.Action{
		Action:       at,
		ResourceType: "eval",
		Name:         "eval." + expID,
		Details:      detailMap,
	}
}

// findNewVariantCells returns (variantKey, taskID) pairs that exist in the
// experiment config but have no data in the scorecard.
func findNewVariantCells(exp *Experiment, sc *Scorecard) [][2]string {
	if sc == nil {
		return nil
	}
	existing := map[[2]string]bool{}
	for _, vk := range sc.VariantKeys {
		for _, tid := range sc.TaskIDs {
			cell := sc.Cells[[2]string{vk, tid}]
			if cell != nil {
				existing[[2]string{vk, tid}] = true
			}
		}
	}

	// Build expected variant keys from the experiment's axes
	spIDs := exp.VariantAxes["system_prompt"]
	tdIDs := exp.VariantAxes["tool_description"]
	if len(tdIDs) == 0 {
		tdIDs = []string{"td-1-current"}
	}
	if len(spIDs) == 0 {
		spIDs = []string{"unknown-sp"}
	}

	var newCells [][2]string
	for _, sp := range spIDs {
		for _, td := range tdIDs {
			vk := sp + " / " + td
			for _, taskID := range exp.TaskIDs {
				key := [2]string{vk, taskID}
				if !existing[key] {
					newCells = append(newCells, key)
				}
			}
		}
	}
	return newCells
}

// detectRegressions compares the current scorecard against the pinned baseline.
// Returns cells where pass rate dropped more than regressionThreshold.
func detectRegressions(exp *Experiment, sc *Scorecard, pins map[string]string) [][2]string {
	if sc == nil || len(pins) == 0 {
		return nil
	}
	pin := pins[exp.ID]
	if pin == "" {
		return nil
	}

	// Determine baseline variant key from experiment.BaselineVariant
	// BaselineVariant is like "sp-1-production+td-1-current" — convert to scorecard key
	baselineKey := baselineVariantKey(exp.BaselineVariant)
	if baselineKey == "" || !containsStr(sc.VariantKeys, baselineKey) {
		return nil
	}

	baselineRate := passRate(sc, baselineKey)
	if math.IsNaN(baselineRate) {
		return nil
	}

	var regressionCells [][2]string
	for _, vk := range sc.VariantKeys {
		if vk == baselineKey {
			continue
		}
		vkRate := passRate(sc, vk)
		if math.IsNaN(vkRate) {
			continue
		}
		if baselineRate-vkRate > regressionThreshold {
			// This variant regressed vs baseline — flag the failing cells
			for _, tid := range sc.TaskIDs {
				blCell := sc.Cells[[2]string{baselineKey, tid}]
				vkCell := sc.Cells[[2]string{vk, tid}]
				if blCell != nil && *blCell && vkCell != nil && !*vkCell {
					regressionCells = append(regressionCells, [2]string{vk, tid})
				}
			}
		}
	}
	return regressionCells
}

// baselineVariantKey converts a BaselineVariant string like "sp-1-production+td-1-current"
// into a scorecard key "sp-1-production / td-1-current".
func baselineVariantKey(bv string) string {
	if bv == "" {
		return ""
	}
	parts := strings.SplitN(bv, "+", 2)
	if len(parts) == 2 {
		return parts[0] + " / " + parts[1]
	}
	return bv
}

// isBaselineStale returns true if the experiment's baseline pin is missing, or
// if the pin is set but no trial has run within baselineStaleDays days.
// latestRunAt is the ISO-8601 timestamp of the most recent trial for this
// experiment (empty string = never run).
func isBaselineStale(exp *Experiment, pins map[string]string, latestRunAt string) bool {
	pin := pins[exp.ID]
	if pin == "" {
		return true
	}
	// Pin is set — check whether any run happened within the staleness window.
	if latestRunAt == "" {
		// Pinned but never run — treat as stale so a fresh baseline can be gathered.
		return true
	}
	ts, err := time.Parse(time.RFC3339, latestRunAt)
	if err != nil {
		// Unparseable timestamp — treat as stale rather than silently suppress.
		return true
	}
	return time.Since(ts) > baselineStaleDays*24*time.Hour
}

// buildTrialSpecsForCells constructs TrialSpec objects for the given (variantKey, taskID) cells.
// This is a simplified version — the full matrix expansion happens at apply time.
func buildTrialSpecsForCells(cfg *EvalConfig, exp *Experiment, cells [][2]string) []TrialSpec {
	var specs []TrialSpec
	for _, cell := range cells {
		vk, taskID := cell[0], cell[1]
		parts := strings.SplitN(vk, " / ", 2)
		spID, tdID := "", ""
		if len(parts) == 2 {
			spID, tdID = parts[0], parts[1]
		}

		variantIDs := map[string]string{}
		if spID != "" && spID != "unknown-sp" {
			variantIDs["system_prompt"] = spID
		}
		if tdID != "" && tdID != "td-1-current" {
			variantIDs["tool_description"] = tdID
		}

		trialID := exp.ID + "__" + spID + "+" + tdID + "__" + taskID
		specs = append(specs, TrialSpec{
			TrialID:      trialID,
			ExperimentID: exp.ID,
			VariantIDs:   variantIDs,
			Target:       exp.Target,
		})
	}
	return specs
}

// ---------------------------------------------------------------------------
// ApplyPlan
// ---------------------------------------------------------------------------

// ApplyPlan executes planned eval actions, one trial at a time (design memo Q4).
func (e *EvalProvider) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	if e.dispatcher == nil {
		// No dispatcher wired — report all as skipped
		var results []reconcile.Result
		for _, action := range plan.Actions {
			results = append(results, reconcile.Result{
				Phase:  "eval",
				Action: string(action.Action),
				Name:   action.Name,
				Status: reconcile.ApplySkipped,
				Error:  "dispatcher not wired",
			})
		}
		return results, nil
	}

	var results []reconcile.Result
	trialsDispatched := 0

	for _, action := range plan.Actions {
		if action.Action == reconcile.ActionSkip {
			results = append(results, reconcile.Result{
				Phase:  "eval",
				Action: string(action.Action),
				Name:   action.Name,
				Status: reconcile.ApplySkipped,
			})
			continue
		}

		// Extract eval-specific detail
		expID, _ := action.Details["experiment_id"].(string)
		evalActionStr, _ := action.Details["eval_action"].(string)
		evalActionType := EvalActionType(evalActionStr)

		// Check budget — stop if we've dispatched enough trials this cycle
		if trialsDispatched >= trialsPerCycle {
			results = append(results, reconcile.Result{
				Phase:  "eval",
				Action: string(action.Action),
				Name:   action.Name,
				Status: reconcile.ApplySkipped,
				Error:  "budget exhausted for this cycle",
			})
			continue
		}

		switch evalActionType {
		case EvalActionRun:
			// Full matrix expansion — load variants from tournament dir and expand
			specs, err := expandExperimentMatrix(e.root, expID)
			if err != nil {
				results = append(results, errorResult(action, err))
				continue
			}
			n, res := e.dispatchTrials(ctx, specs, &trialsDispatched)
			results = append(results, res...)
			_ = n

		case EvalActionRunIncremental, EvalActionRetryRegression, EvalActionRefreshBaseline:
			// Use pre-computed trial specs from detail
			specs := extractTrialSpecsFromDetail(action.Details)
			if len(specs) == 0 && evalActionType == EvalActionRefreshBaseline {
				// For refresh, re-expand the full matrix
				var err error
				specs, err = expandExperimentMatrix(e.root, expID)
				if err != nil {
					results = append(results, errorResult(action, err))
					continue
				}
			}
			n, res := e.dispatchTrials(ctx, specs, &trialsDispatched)
			results = append(results, res...)
			_ = n

		default:
			results = append(results, reconcile.Result{
				Phase:  "eval",
				Action: string(action.Action),
				Name:   action.Name,
				Status: reconcile.ApplySkipped,
				Error:  "unknown eval_action: " + evalActionStr,
			})
		}
	}

	return results, nil
}

// dispatchTrials dispatches TrialSpecs up to the per-cycle budget.
// Returns the number of trials dispatched and the results.
func (e *EvalProvider) dispatchTrials(ctx context.Context, specs []TrialSpec, dispatched *int) (int, []reconcile.Result) {
	var results []reconcile.Result
	count := 0

	for _, spec := range specs {
		if *dispatched >= trialsPerCycle {
			break
		}

		// Acquire the eval mutex to serialize Ollama access
		applyMu.Lock()
		record, err := e.runTrial(ctx, spec)
		applyMu.Unlock()

		if err != nil {
			results = append(results, reconcile.Result{
				Phase:  "eval",
				Action: "run",
				Name:   spec.TrialID,
				Status: reconcile.ApplyFailed,
				Error:  err.Error(),
			})
			*dispatched++
			count++
			continue
		}

		// Emit the trial record as a CogBlock (best-effort)
		if e.emitter != nil {
			payload := map[string]any{
				"type":          "tournament.trial.v1",
				"trial":         record,
				"experiment_id": record.ExperimentID,
			}
			_ = e.emitter.EmitCogBlock(ctx, "bus_tournament", payload)
		}

		results = append(results, reconcile.Result{
			Phase:     "eval",
			Action:    "run",
			Name:      spec.TrialID,
			Status:    reconcile.ApplySucceeded,
			CreatedID: spec.TrialID,
		})
		*dispatched++
		count++
	}

	return count, results
}

// runTrial dispatches a single trial and returns the scored TrialRecord.
func (e *EvalProvider) runTrial(ctx context.Context, spec TrialSpec) (*TrialRecord, error) {
	// Build the system prompt from the variant
	var systemPrompt string
	if spec.SystemPromptVariant != nil {
		if sp, ok := spec.SystemPromptVariant.Content.(string); ok {
			systemPrompt = sp
		}
	}

	// Extract task prompt and rubric from the task variant's content
	prompt, rubric := extractTaskContent(spec)

	// Dispatch the trial
	req := DispatchRequest{
		Task:           prompt,
		SystemPrompt:   systemPrompt,
		N:              1,
		TimeoutSeconds: 120,
	}

	ts := nowISO()
	result, err := e.dispatcher.DispatchToHarness(ctx, req)
	if err != nil || result == nil || len(result.Results) == 0 {
		// Build a failed record
		errStr := "dispatch error"
		if err != nil {
			errStr = err.Error()
		}
		return &TrialRecord{
			TrialID:      spec.TrialID,
			ExperimentID: spec.ExperimentID,
			VariantIDs:   spec.VariantIDs,
			TaskID:       spec.TaskVariant.ID,
			Target:       spec.Target,
			Passed:       false,
			Failures:     []string{"dispatch: " + errStr},
			Timestamp:    ts,
		}, nil
	}

	dr := result.Results[0]

	// Extract tool call names from the structured ToolCalls field populated by
	// the harness. This replaces the old content-parsing stub which always
	// returned nil, breaking all tool-call rubric assertions.
	toolCallNames := extractToolCallNamesFromResult(dr)
	scored := NewDispatchScoredResult(dr, toolCallNames)
	verdict := Score(rubric, scored)

	toolCalls := make([]ToolCallRecord, 0, len(toolCallNames))
	for _, name := range toolCallNames {
		toolCalls = append(toolCalls, ToolCallRecord{Name: name})
	}

	return &TrialRecord{
		TrialID:      spec.TrialID,
		ExperimentID: spec.ExperimentID,
		VariantIDs:   spec.VariantIDs,
		TaskID:       spec.TaskVariant.ID,
		Target:       spec.Target,
		Passed:       verdict.Passed,
		Failures:     verdict.Failures,
		Notes:        verdict.Notes,
		ToolCalls:    toolCalls,
		Content:      dr.Content,
		DurationSec:  dr.DurationSec,
		Timestamp:    ts,
		Model:        dr.ModelUsed,
	}, nil
}

// extractTaskContent extracts the prompt and rubric from a TrialSpec's task variant.
func extractTaskContent(spec TrialSpec) (string, Rubric) {
	if spec.TaskVariant.Content == nil {
		return "", Rubric{}
	}

	content, ok := spec.TaskVariant.Content.(map[string]interface{})
	if !ok {
		return "", Rubric{}
	}

	prompt, _ := content["prompt"].(string)
	rubricRaw, _ := content["rubric"].(map[string]interface{})

	rubric := parseRubricFromMap(rubricRaw)
	return prompt, rubric
}

// parseRubricFromMap converts a map[string]interface{} into a Rubric.
func parseRubricFromMap(m map[string]interface{}) Rubric {
	if m == nil {
		return Rubric{}
	}
	return Rubric{
		ExpectedTools:           toStringSlice(m["expected_tools"]),
		ExpectedToolsAnyOf:      toStringSlice(m["expected_tools_any_of"]),
		ForbiddenTools:          toStringSlice(m["forbidden_tools"]),
		ContentContains:         toStringSlice(m["content_contains"]),
		ContentMustNotContain:   toStringSlice(m["content_must_not_contain"]),
		ContentContainsCI:       toStringSlice(m["content_contains_ci"]),
		ContentMustNotContainCI: toStringSlice(m["content_must_not_contain_ci"]),
		FirstToolOneOf:          toStringSlice(m["first_tool_one_of"]),
	}
}

// toStringSlice converts an interface{} to []string.
func toStringSlice(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch s := v.(type) {
	case []string:
		return s
	case []interface{}:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// extractToolCallNamesFromResult extracts tool call names from a DispatchResult.
// The harness populates DispatchResult.ToolCalls with per-invocation summaries;
// each summary carries the Name field. Returns nil if no tool calls were made.
func extractToolCallNamesFromResult(dr DispatchResult) []string {
	if len(dr.ToolCalls) == 0 {
		return nil
	}
	names := make([]string, 0, len(dr.ToolCalls))
	for _, tc := range dr.ToolCalls {
		if tc.Name != "" {
			names = append(names, tc.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// extractTrialSpecsFromDetail extracts TrialSpec objects from action.Details.
func extractTrialSpecsFromDetail(details map[string]any) []TrialSpec {
	raw, ok := details["trial_specs"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var specs []TrialSpec
	_ = json.Unmarshal(b, &specs)
	return specs
}

// expandExperimentMatrix loads the experiment's variant cogdocs and expands
// into a full trial matrix. Used by EvalActionRun and EvalActionRefreshBaseline.
func expandExperimentMatrix(root, experimentID string) ([]TrialSpec, error) {
	// Load the experiment cogdoc
	experimentsDir := filepath.Join(root, ".cog", "mem", "semantic", "architecture", "tournament", "experiments")
	tournamentRoot := filepath.Join(root, ".cog", "mem", "semantic", "architecture", "tournament")

	var expPath string
	entries, err := os.ReadDir(experimentsDir)
	if err != nil {
		return nil, fmt.Errorf("eval: read experiments dir: %w", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), experimentID) && strings.HasSuffix(entry.Name(), ".cog.md") {
			expPath = filepath.Join(experimentsDir, entry.Name())
			break
		}
	}
	if expPath == "" {
		return nil, fmt.Errorf("eval: experiment %q not found", experimentID)
	}

	raw, err := os.ReadFile(expPath)
	if err != nil {
		return nil, fmt.Errorf("eval: read experiment %s: %w", expPath, err)
	}
	exp, err := parseCogdocExperiment(raw, expPath)
	if err != nil || exp == nil {
		return nil, fmt.Errorf("eval: parse experiment %s: %w", expPath, err)
	}

	// Load all variant cogdocs
	variantsByID, err := loadVariantsFromDir(tournamentRoot)
	if err != nil {
		return nil, err
	}

	return expandMatrix(exp, variantsByID, tournamentRoot), nil
}

// loadVariantsFromDir loads all .cog.md variant cogdocs from the tournament directory.
// Port of evals/tournament/variants.py load_variants().
func loadVariantsFromDir(tournamentRoot string) (map[string]*Variant, error) {
	variants := map[string]*Variant{}
	err := filepath.Walk(tournamentRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".cog.md") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		v := parseVariantCogdoc(raw, path)
		if v != nil && v.ID != "" {
			if _, dup := variants[v.ID]; !dup {
				variants[v.ID] = v
			}
		}
		return nil
	})
	return variants, err
}

// parseVariantCogdoc parses a .cog.md file into a Variant.
// Port of evals/tournament/variants.py load_variant_from_file().
func parseVariantCogdoc(raw []byte, path string) *Variant {
	text := string(raw)
	fm, err := splitFrontmatter(text)
	if err != nil || fm == nil {
		return nil
	}

	id, _ := fm["id"].(string)
	if id == "" {
		base := filepath.Base(path)
		id = strings.TrimSuffix(base, ".cog.md")
	}

	vc, _ := fm["variant_class"].(string)
	var content interface{}

	switch vc {
	case "system-prompt":
		content = extractSection(text, "Variant content")
	case "tool-description":
		content = fm["overrides"]
	case "task":
		content = fm["case"]
	default:
		content = fm
	}

	baselineOf, _ := fm["baseline_of"].(string)
	ablation, _ := fm["ablation"].(string)

	return &Variant{
		ID:         id,
		Class:      VariantClass(vc),
		Content:    content,
		BaselineOf: baselineOf,
		Ablation:   ablation,
		Tags:       toStringSlice(fm["tags"]),
		SourcePath: path,
	}
}

// extractSection extracts content under a markdown heading.
// Port of evals/tournament/variants.py _extract_section().
var sectionRe = regexp.MustCompile(`(?s)## Variant content\s*\n(.*?)(?:\n## |\z)`)

func extractSection(text, sectionName string) string {
	if sectionName != "Variant content" {
		// Generic fallback
		pat := regexp.MustCompile(`(?s)## ` + regexp.QuoteMeta(sectionName) + `\s*\n(.*?)(?:\n## |\z)`)
		m := pat.FindStringSubmatch(text)
		if m != nil {
			return strings.TrimSpace(m[1])
		}
		return ""
	}
	m := sectionRe.FindStringSubmatch(text)
	if m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// expandMatrix builds the Cartesian product of variant axes × tasks.
// Port of evals/tournament/matrix.py expand_matrix().
func expandMatrix(exp *Experiment, variantsByID map[string]*Variant, tournamentRoot string) []TrialSpec {
	spIDs := exp.VariantAxes["system_prompt"]
	tdIDs := exp.VariantAxes["tool_description"]
	taskIDs := exp.TaskIDs

	spVariants := resolveVariants(spIDs, variantsByID)
	tdVariants := resolveVariants(tdIDs, variantsByID)
	taskVariants := resolveVariants(taskIDs, variantsByID)

	if len(taskVariants) == 0 {
		return nil
	}

	spList := spVariants
	if len(spList) == 0 {
		spList = []*Variant{nil}
	}
	tdList := tdVariants
	if len(tdList) == 0 {
		tdList = []*Variant{nil}
	}

	var specs []TrialSpec
	for _, spV := range spList {
		for _, tdV := range tdList {
			for _, taskV := range taskVariants {
				variantIDs := map[string]string{}
				var spVPtr, tdVPtr *Variant

				if spV != nil {
					variantIDs["system_prompt"] = spV.ID
					spVPtr = spV
				}
				if tdV != nil {
					variantIDs["tool_description"] = tdV.ID
					tdVPtr = tdV
				}

				spID := variantIDs["system_prompt"]
				if spID == "" {
					spID = "sp-default"
				}
				tdID := variantIDs["tool_description"]
				if tdID == "" {
					tdID = "td-default"
				}

				trialID := exp.ID + "__" + spID + "+" + tdID + "__" + taskV.ID

				specs = append(specs, TrialSpec{
					TrialID:                trialID,
					ExperimentID:           exp.ID,
					TaskVariant:            *taskV,
					VariantIDs:             variantIDs,
					SystemPromptVariant:    spVPtr,
					ToolDescriptionVariant: tdVPtr,
					Target:                 exp.Target,
				})
			}
		}
	}
	return specs
}

// resolveVariants looks up variant IDs from the loaded variant map.
func resolveVariants(ids []string, byID map[string]*Variant) []*Variant {
	var result []*Variant
	for _, id := range ids {
		v, ok := byID[id]
		if ok {
			result = append(result, v)
		}
	}
	return result
}

func errorResult(action reconcile.Action, err error) reconcile.Result {
	return reconcile.Result{
		Phase:  "eval",
		Action: string(action.Action),
		Name:   action.Name,
		Status: reconcile.ApplyFailed,
		Error:  err.Error(),
	}
}

// ---------------------------------------------------------------------------
// BuildState
// ---------------------------------------------------------------------------

// BuildState constructs reconcile state from live trial data.
// Pattern mirrors component_provider.go BuildState() (lines 293-334).
func (e *EvalProvider) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	ls, ok := live.(*EvalLiveState)
	if !ok {
		ls = &EvalLiveState{Scorecards: map[string]*Scorecard{}}
	}
	cfg, ok := config.(*EvalConfig)
	if !ok {
		cfg = &EvalConfig{Experiments: map[string]*Experiment{}}
	}

	state := &reconcile.State{
		Version:      1,
		ResourceType: "eval",
		GeneratedAt:  nowISO(),
	}
	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	} else {
		state.Lineage = "eval-" + nowISO()
	}

	// Carry forward eval provider state from existing metadata
	eps := parseEvalProviderState(existing)
	if eps.CircuitBreakerThreshold == 0 {
		eps.CircuitBreakerThreshold = circuitBreakerDefaultThreshold
	}

	// Build one resource per experiment
	// Merge experiment IDs from both config and live state
	allExpIDs := map[string]struct{}{}
	for id := range cfg.Experiments {
		allExpIDs[id] = struct{}{}
	}
	for id := range ls.Scorecards {
		allExpIDs[id] = struct{}{}
	}

	sortedIDs := make([]string, 0, len(allExpIDs))
	for id := range allExpIDs {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)

	for _, expID := range sortedIDs {
		sc := ls.Scorecards[expID]
		pin := cfg.BaselinePins[expID]

		// Compute trial count and pass rate from scorecard
		trialCount := 0
		var passedCount int
		if sc != nil {
			for _, vk := range sc.VariantKeys {
				for _, tid := range sc.TaskIDs {
					cell := sc.Cells[[2]string{vk, tid}]
					if cell != nil {
						trialCount++
						if *cell {
							passedCount++
						}
					}
				}
			}
		}

		var pr float64
		if trialCount > 0 {
			pr = float64(passedCount) / float64(trialCount)
		}

		// Compute latest run timestamp from trials
		lastRunAt := ""
		for _, tr := range ls.Trials {
			if tr.ExperimentID == expID && tr.Timestamp > lastRunAt {
				lastRunAt = tr.Timestamp
			}
		}

		cbCount := 0
		if eps.RecentFailureCounts != nil {
			cbCount = eps.RecentFailureCounts[expID]
		}

		resource := reconcile.Resource{
			Address:       "eval." + expID,
			Type:          "experiment",
			Mode:          reconcile.ModeManaged,
			ExternalID:    latestRunID(ls.Trials, expID),
			Name:          expID,
			LastRefreshed: nowISO(),
			Attributes: map[string]any{
				"trial_count":     trialCount,
				"pass_rate":       pr,
				"last_run_at":     lastRunAt,
				"baseline_pinned": pin,
				"circuit_breaker": cbCount,
			},
		}
		state.Resources = append(state.Resources, resource)
	}

	// Encode EvalProviderState into metadata
	epsJSON, _ := json.Marshal(eps)
	var epsMap map[string]any
	_ = json.Unmarshal(epsJSON, &epsMap)
	state.Metadata = map[string]any{
		"eval_state": epsMap,
	}

	return state, nil
}

// latestRunID finds the most recent run ID from a list of trials for an experiment.
func latestRunID(trials []TrialRecord, expID string) string {
	latest := ""
	for _, tr := range trials {
		if tr.ExperimentID == expID && tr.Timestamp > latest {
			latest = tr.Timestamp
		}
	}
	return latest
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

// Health returns the three-axis status of the eval subsystem.
func (e *EvalProvider) Health() reconcile.ResourceStatus {
	return e.lastHealth
}

// updateHealth refreshes the cached health status.
// Called at the end of ApplyPlan.
func (e *EvalProvider) updateHealth(pending, inFlight, suspended int) {
	msg := fmt.Sprintf("%d pending, %d in-flight, %d suspended", pending, inFlight, suspended)

	sync := reconcile.SyncStatusSynced
	if pending > 0 {
		sync = reconcile.SyncStatusOutOfSync
	}

	health := reconcile.HealthHealthy
	if suspended > 0 {
		health = reconcile.HealthDegraded
	} else if inFlight > 0 {
		health = reconcile.HealthProgressing
	}

	op := reconcile.OperationIdle
	if inFlight > 0 {
		op = reconcile.OperationSyncing
	}

	e.lastHealth = reconcile.ResourceStatus{
		Sync:      sync,
		Health:    health,
		Operation: op,
		Message:   msg,
	}
}

// ---------------------------------------------------------------------------
// parseEvalProviderState (Phase C implementation)
// ---------------------------------------------------------------------------

// parseEvalProviderState deserializes EvalProviderState from reconcile.State.Metadata.
// Returns a zero-value EvalProviderState with default threshold if absent/unparseable.
func parseEvalProviderState(state *reconcile.State) EvalProviderState {
	eps := EvalProviderState{
		CircuitBreakerThreshold: circuitBreakerDefaultThreshold,
	}
	if state == nil || state.Metadata == nil {
		return eps
	}
	raw, ok := state.Metadata["eval_state"]
	if !ok {
		return eps
	}

	// Re-serialize to JSON (it may have been round-tripped through interface{})
	b, err := json.Marshal(raw)
	if err != nil {
		return eps
	}
	if err := json.Unmarshal(b, &eps); err != nil {
		return eps
	}
	if eps.CircuitBreakerThreshold == 0 {
		eps.CircuitBreakerThreshold = circuitBreakerDefaultThreshold
	}
	return eps
}

// ---------------------------------------------------------------------------
// Utility helpers
// ---------------------------------------------------------------------------

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

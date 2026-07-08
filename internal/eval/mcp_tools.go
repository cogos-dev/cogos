// mcp_tools.go — MCP tool surface for the eval harness (design memo Q10).
//
// Registers four MCP tools on a provided *mcp.Server:
//
//	cog_run_experiment        — trigger a full experiment run
//	cog_list_experiments      — list declared experiments with health status
//	cog_get_experiment_status — full status for one experiment
//	cog_pin_baseline          — write a baseline pin to eval-baselines.json
//
// Registration pattern: the caller (kernel boot or eval_wiring.go) calls
// RegisterEvalTools(server, provider) after wiring the EvalProvider.
// Mirrors the pattern in internal/engine/mcp_server.go registerTools().
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myrgic/cogos/pkg/filelock"
)

// RegisterEvalTools registers the four eval MCP tools on the given server.
// provider may be nil if the eval subsystem is not wired — tools return a
// clean "not configured" error in that case.
func RegisterEvalTools(server *mcp.Server, provider *EvalProvider) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "cog_run_experiment",
		Description: "Trigger a full eval experiment run. Sets a one-cycle dispatch trigger so the next reconcile cycle dispatches trials even if auto_reconcile=false. force=true resets the circuit breaker.",
	}, makeRunExperimentHandler(provider))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cog_list_experiments",
		Description: "List all declared eval experiments with their health status (pending/running/synced/suspended).",
	}, makeListExperimentsHandler(provider))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cog_get_experiment_status",
		Description: "Get full status for a single eval experiment: last run, pass rate, scorecard, baseline pin, in-flight count.",
	}, makeGetExperimentStatusHandler(provider))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cog_pin_baseline",
		Description: "Pin a run ID as the baseline for an experiment. Writes to .cog/state/eval-baselines.json.",
	}, makePinBaselineHandler(provider))
}

// ---------------------------------------------------------------------------
// cog_run_experiment
// ---------------------------------------------------------------------------

type runExperimentInput struct {
	ExperimentID string `json:"experiment_id"`
	Target       string `json:"target,omitempty"`
	Force        bool   `json:"force,omitempty"`
}

func makeRunExperimentHandler(p *EvalProvider) mcp.ToolHandlerFor[runExperimentInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input runExperimentInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return evalErrorResult("eval provider not wired"), nil, nil
		}
		if input.ExperimentID == "" {
			return evalErrorResult("experiment_id is required"), nil, nil
		}

		// Load config to validate the experiment exists
		cfgAny, err := p.LoadConfig(p.root)
		if err != nil {
			return evalErrorResult(fmt.Sprintf("load config: %v", err)), nil, nil
		}
		cfg, ok := cfgAny.(*EvalConfig)
		if !ok || cfg == nil {
			return evalErrorResult("eval config not available"), nil, nil
		}
		if _, exists := cfg.Experiments[input.ExperimentID]; !exists {
			return evalErrorResult(fmt.Sprintf("experiment %q not found", input.ExperimentID)), nil, nil
		}

		// Write a dispatch trigger so ComputePlan picks it up on the next cycle.
		// Mirrors the writePinBaseline pattern: persist to a JSON sidecar in
		// .cog/state/ so the trigger survives across the MCP→reconcile boundary.
		if err := writeDispatchTrigger(p.root, input.ExperimentID, input.Force); err != nil {
			return evalErrorResult(fmt.Sprintf("write dispatch trigger: %v", err)), nil, nil
		}

		resp := map[string]any{
			"ok":            true,
			"experiment_id": input.ExperimentID,
			"message":       fmt.Sprintf("experiment %q queued for next reconcile cycle", input.ExperimentID),
			"force":         input.Force,
			"trigger_file":  "eval-dispatch-triggers.json",
		}
		b, _ := json.Marshal(resp)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}, resp, nil
	}
}

// ---------------------------------------------------------------------------
// cog_list_experiments
// ---------------------------------------------------------------------------

type listExperimentsInput struct{}

func makeListExperimentsHandler(p *EvalProvider) mcp.ToolHandlerFor[listExperimentsInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input listExperimentsInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return evalErrorResult("eval provider not wired"), nil, nil
		}

		cfgAny, err := p.LoadConfig(p.root)
		if err != nil {
			return evalErrorResult(fmt.Sprintf("load config: %v", err)), nil, nil
		}
		cfg, _ := cfgAny.(*EvalConfig)

		// Fetch live state for status
		liveAny, _ := p.FetchLive(ctx, cfg)
		ls, _ := liveAny.(*EvalLiveState)

		type experimentStatus struct {
			ID             string  `json:"id"`
			Title          string  `json:"title"`
			AutoReconcile  bool    `json:"auto_reconcile"`
			BaselinePinned string  `json:"baseline_pinned,omitempty"`
			TrialCount     int     `json:"trial_count"`
			PassRate       float64 `json:"pass_rate,omitempty"`
			HasPassRate    bool    `json:"has_pass_rate"`
			LastRunAt      string  `json:"last_run_at,omitempty"`
		}

		var items []experimentStatus
		if cfg != nil {
			ids := make([]string, 0, len(cfg.Experiments))
			for id := range cfg.Experiments {
				ids = append(ids, id)
			}
			sort.Strings(ids)

			for _, id := range ids {
				exp := cfg.Experiments[id]
				sc := map[string]*Scorecard{}
				if ls != nil {
					sc = ls.Scorecards
				}
				scorecard := sc[id]

				trials := 0
				var pr float64
				hasPR := false
				if scorecard != nil {
					for _, vk := range scorecard.VariantKeys {
						for _, tid := range scorecard.TaskIDs {
							if cell := scorecard.Cells[[2]string{vk, tid}]; cell != nil {
								trials++
							}
						}
					}
					if len(scorecard.VariantKeys) > 0 {
						pr = passRate(scorecard, scorecard.VariantKeys[0])
						hasPR = !math.IsNaN(pr)
					}
				}

				lastRunAt := ""
				if ls != nil {
					for _, tr := range ls.Trials {
						if tr.ExperimentID == id && tr.Timestamp > lastRunAt {
							lastRunAt = tr.Timestamp
						}
					}
				}

				items = append(items, experimentStatus{
					ID:             id,
					Title:          exp.Title,
					AutoReconcile:  exp.AutoReconcile,
					BaselinePinned: exp.BaselinePinned,
					TrialCount:     trials,
					PassRate:       pr,
					HasPassRate:    hasPR,
					LastRunAt:      lastRunAt,
				})
			}
		}

		resp := map[string]any{
			"experiments": items,
			"count":       len(items),
		}
		b, _ := json.MarshalIndent(resp, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}, resp, nil
	}
}

// ---------------------------------------------------------------------------
// cog_get_experiment_status
// ---------------------------------------------------------------------------

type getExperimentStatusInput struct {
	ExperimentID string `json:"experiment_id"`
}

func makeGetExperimentStatusHandler(p *EvalProvider) mcp.ToolHandlerFor[getExperimentStatusInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input getExperimentStatusInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return evalErrorResult("eval provider not wired"), nil, nil
		}
		if input.ExperimentID == "" {
			return evalErrorResult("experiment_id is required"), nil, nil
		}

		cfgAny, err := p.LoadConfig(p.root)
		if err != nil {
			return evalErrorResult(fmt.Sprintf("load config: %v", err)), nil, nil
		}
		cfg, _ := cfgAny.(*EvalConfig)

		exp := (*Experiment)(nil)
		if cfg != nil {
			exp = cfg.Experiments[input.ExperimentID]
		}
		if exp == nil {
			return evalErrorResult(fmt.Sprintf("experiment %q not found", input.ExperimentID)), nil, nil
		}

		liveAny, _ := p.FetchLive(ctx, cfg)
		ls, _ := liveAny.(*EvalLiveState)

		sc := (*Scorecard)(nil)
		if ls != nil {
			sc = ls.Scorecards[input.ExperimentID]
		}

		// Build scorecard summary
		cells := map[string]interface{}{}
		if sc != nil {
			for _, vk := range sc.VariantKeys {
				taskResults := map[string]interface{}{}
				for _, tid := range sc.TaskIDs {
					cell := sc.Cells[[2]string{vk, tid}]
					if cell == nil {
						taskResults[tid] = nil
					} else {
						taskResults[tid] = *cell
					}
				}
				cells[vk] = taskResults
			}
		}

		lastRunAt := ""
		if ls != nil {
			for _, tr := range ls.Trials {
				if tr.ExperimentID == input.ExperimentID && tr.Timestamp > lastRunAt {
					lastRunAt = tr.Timestamp
				}
			}
		}

		pin := ""
		if cfg != nil {
			pin = cfg.BaselinePins[input.ExperimentID]
		}

		variantKeys := []string{}
		taskIDs := []string{}
		if sc != nil {
			variantKeys = sc.VariantKeys
			taskIDs = sc.TaskIDs
		}

		resp := map[string]any{
			"experiment_id":   exp.ID,
			"title":           exp.Title,
			"auto_reconcile":  exp.AutoReconcile,
			"baseline_pinned": pin,
			"last_run_at":     lastRunAt,
			"in_flight":       0,
			"scorecard":       cells,
			"variant_keys":    variantKeys,
			"task_ids":        taskIDs,
		}

		b, _ := json.MarshalIndent(resp, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}, resp, nil
	}
}

// ---------------------------------------------------------------------------
// cog_pin_baseline
// ---------------------------------------------------------------------------

type pinBaselineInput struct {
	ExperimentID string `json:"experiment_id"`
	RunID        string `json:"run_id"`
}

func makePinBaselineHandler(p *EvalProvider) mcp.ToolHandlerFor[pinBaselineInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input pinBaselineInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return evalErrorResult("eval provider not wired"), nil, nil
		}
		if input.ExperimentID == "" || input.RunID == "" {
			return evalErrorResult("experiment_id and run_id are required"), nil, nil
		}

		if p.root == "" {
			return evalErrorResult("eval provider root not set (LoadConfig not called)"), nil, nil
		}

		err := writePinBaseline(p.root, input.ExperimentID, input.RunID)
		if err != nil {
			return evalErrorResult(fmt.Sprintf("write baseline pin: %v", err)), nil, nil
		}

		resp := map[string]any{
			"ok":            true,
			"experiment_id": input.ExperimentID,
			"run_id":        input.RunID,
			"message":       fmt.Sprintf("baseline pin written for %s → %s", input.ExperimentID, input.RunID),
		}
		b, _ := json.Marshal(resp)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}, resp, nil
	}
}

// dispatchTriggersLockTimeout bounds how long writeDispatchTrigger /
// readAndClearDispatchTriggers wait to acquire the cross-process lock on
// eval-dispatch-triggers.json. Mirrors metaLockTimeout in
// internal/conversations/index.go / StateLockTimeout in
// pkg/substrate/reconcile/state.go (same bug class, same timeout budget).
const dispatchTriggersLockTimeout = 5 * time.Second

// dispatchTriggersLockPath returns the advisory cross-process lock file
// guarding the read-modify-write cycle on eval-dispatch-triggers.json. A
// sibling file, not the triggers file itself, so the lock's own lifecycle
// never touches the JSON content file readAndClearDispatchTriggers reads.
func dispatchTriggersLockPath(root string) string {
	return filepath.Join(root, ".cog", "state", "eval-dispatch-triggers.json.lock")
}

// writeDispatchTrigger records an on-demand experiment trigger in
// .cog/state/eval-dispatch-triggers.json. The map value is "force" (bool).
// ComputePlan calls readAndClearDispatchTriggers() to consume and atomically
// remove entries so they fire exactly once.
//
// Concurrency the lock actually guards (corrected, review round on ee61416):
// in the SHIPPED cmd/cogos binary the reconcile provider for "eval" is a
// no-op stub (internal/providers/daemon/daemon.go) — the real EvalProvider is
// wired only to the MCP tool handlers plus a one-time boot LoadConfig, so the
// daemon's reconcile loop does NOT touch this file today. The live races are
// (a) two concurrent MCP tool invocations (multiple sessions) hitting the
// same trigger file, and (b) any future wiring that registers the real
// provider for reconcile (the root-package parallel tree already does).
// Same read-modify-write class as issue #449, same fix: atomic tmp+rename
// write plus a cross-process filelock across the full cycle.
func writeDispatchTrigger(root, experimentID string, force bool) error {
	stateDir := filepath.Join(root, ".cog", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", stateDir, err)
	}

	lock, err := filelock.Acquire(dispatchTriggersLockPath(root), dispatchTriggersLockTimeout)
	if err != nil {
		return fmt.Errorf("acquire dispatch-triggers lock: %w", err)
	}
	defer lock.Release()

	triggersPath := filepath.Join(stateDir, "eval-dispatch-triggers.json")
	triggers := map[string]bool{}

	if data, err := os.ReadFile(triggersPath); err == nil {
		_ = json.Unmarshal(data, &triggers) // ignore parse errors — start fresh
	}

	// A force=true trigger takes priority; never downgrade from force to non-force.
	if existing, ok := triggers[experimentID]; !ok || force && !existing {
		triggers[experimentID] = force
	}

	b, err := json.MarshalIndent(triggers, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(triggersPath, b)
}

// readAndClearDispatchTriggers reads the pending trigger file and clears it.
// Returns map[experimentID]force. The file is removed (not just emptied) so
// a missing file is the normal steady state. Called by ComputePlan before
// rule evaluation.
//
// Takes the same cross-process lock as writeDispatchTrigger around the full
// read-then-clear cycle, so a concurrent writeDispatchTrigger call (e.g. a
// user invoking cog_run_experiment via MCP at the same moment the daemon's
// eval reconcile cycle is draining triggers) can't have its just-added
// trigger silently wiped by the clear, or have the clear itself clobbered
// back to non-empty.
func readAndClearDispatchTriggers(root string) map[string]bool {
	lock, err := filelock.Acquire(dispatchTriggersLockPath(root), dispatchTriggersLockTimeout)
	if err != nil {
		// Lock contention/timeout: treat like "no triggers pending" rather
		// than blocking the reconcile cycle indefinitely or erroring — the
		// next cycle will retry. Mirrors the pre-existing best-effort
		// posture of this function (write errors were already ignored).
		return map[string]bool{}
	}
	defer lock.Release()

	triggersPath := filepath.Join(root, ".cog", "state", "eval-dispatch-triggers.json")
	data, err := os.ReadFile(triggersPath)
	if err != nil {
		return map[string]bool{} // no triggers pending
	}

	triggers := map[string]bool{}
	_ = json.Unmarshal(data, &triggers)

	// Clear the file atomically — write an empty map so a partial read can't
	// see stale triggers on the next cycle. Ignore write errors (best-effort).
	_ = writeFileAtomic(triggersPath, []byte("{}"))

	return triggers
}

// writeFileAtomic writes data to path via tmp+rename, same pattern as
// BusSessionManager.saveRegistry (internal/engine/bus_session.go) and
// writeMetaFileLocked (internal/conversations/index.go) — a plain
// os.WriteFile can leave the file empty or torn if the process is killed
// mid-write, which a subsequent json.Unmarshal would either fail on or
// silently read as a valid-but-wrong empty map.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing tmp %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming %s: %w", path, err)
	}
	return nil
}

// pinBaselineLockPath returns the advisory cross-process lock file guarding
// the read-modify-write cycle on eval-baselines.json.
func pinBaselineLockPath(root string) string {
	return filepath.Join(root, ".cog", "state", "eval-baselines.json.lock")
}

// writePinBaseline writes a baseline pin to .cog/state/eval-baselines.json.
// The file is a JSON map[string]string: experiment_id -> run_id.
// Implements cog_pin_baseline's storage logic (design memo Q1 / Q10).
//
// This was originally left unlocked/non-atomic on the (incorrect) claim
// that no concurrent reader/writer exists. Corrected justification (review
// round on ee61416): in the SHIPPED cmd/cogos binary the "eval" reconcile
// provider is a no-op stub, so the daemon reconcile loop does NOT read this
// file each cycle — the real EvalProvider's LoadConfig runs once at boot and
// via MCP handlers only. The live race is two concurrent cog_pin_baseline
// MCP calls interleaving with each other (and with any future real-provider
// reconcile wiring, which the root-package parallel tree already has). Same
// bug class, same fix: atomic tmp+rename write plus a cross-process filelock
// spanning the full read-modify-write cycle.
func writePinBaseline(root, experimentID, runID string) error {
	stateDir := filepath.Join(root, ".cog", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", stateDir, err)
	}

	lock, err := filelock.Acquire(pinBaselineLockPath(root), dispatchTriggersLockTimeout)
	if err != nil {
		return fmt.Errorf("acquire pin-baseline lock: %w", err)
	}
	defer lock.Release()

	pinsPath := filepath.Join(stateDir, "eval-baselines.json")
	pins := map[string]string{}

	if data, err := os.ReadFile(pinsPath); err == nil {
		_ = json.Unmarshal(data, &pins) // ignore parse errors — start fresh
	}

	pins[experimentID] = runID
	b, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(pinsPath, b)
}

// evalErrorResult builds a CallToolResult carrying an error message.
func evalErrorResult(msg string) *mcp.CallToolResult {
	resp := map[string]any{"error": msg, "ok": false}
	b, _ := json.Marshal(resp)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		IsError: true,
	}
}

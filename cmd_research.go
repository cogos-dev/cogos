package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cmdResearch dispatches `cog research <subcommand>`.
func cmdResearch(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found\n")
		return 1
	}

	if len(args) < 1 {
		printResearchHelp()
		return 0
	}

	switch args[0] {
	case "start":
		return cmdResearchStart(root, args[1:])
	case "status":
		return cmdResearchStatus(root, args[1:])
	case "eval":
		return cmdResearchEval(root, args[1:])
	case "keep":
		return cmdResearchKeep(root, args[1:])
	case "discard":
		return cmdResearchDiscard(root, args[1:])
	case "results":
		return cmdResearchResults(root, args[1:])
	case "pause":
		return cmdResearchPause(root, args[1:])
	case "resume":
		return cmdResearchResume(root, args[1:])
	case "stop":
		return cmdResearchStop(root, args[1:])
	case "list":
		return cmdResearchList(root, args[1:])
	case "help", "--help", "-h":
		printResearchHelp()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown research command: %s\n", args[0])
		printResearchHelp()
		return 1
	}
}

func printResearchHelp() {
	fmt.Print(`Research — autonomous experiment orchestration

Usage: cog research <command> [args...]

Commands:
  start   --program <path> [--timeline <duration>] [--branch <name>]
          Create and start a research run

  status  Show status of the active research run

  eval    --commit <hash> --description <text>
          Run a full build → test-kernel → eval cycle

  keep    --commit <hash>
          Record experiment as kept (RWE improved)

  discard --commit <hash>
          Record experiment as discarded and git reset

  results [--json]
          Print the results log for the active run

  pause   Pause the active run (stops timeline)
  resume  Resume a paused run
  stop    Stop the active run and clean up

  list    List all research runs

Examples:
  cog research start --program program.md --timeline 8h
  cog research eval --commit HEAD --description "blend embedding at 0.2"
  cog research keep --commit a1b2c3d
  cog research results
`)
}

// ---------------------------------------------------------------------------
// start
// ---------------------------------------------------------------------------

func cmdResearchStart(root string, args []string) int {
	program := ""
	timeline := ""
	branch := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--program":
			if i+1 < len(args) {
				program = args[i+1]
				i++
			}
		case "--timeline":
			if i+1 < len(args) {
				timeline = args[i+1]
				i++
			}
		case "--branch":
			if i+1 < len(args) {
				branch = args[i+1]
				i++
			}
		}
	}

	if program == "" {
		fmt.Fprintf(os.Stderr, "Error: --program is required\n")
		return 1
	}

	// Check no active run
	existing, _ := activeResearchRun(root)
	if existing != nil {
		fmt.Fprintf(os.Stderr, "Error: active run %s already exists (state: %s). Stop it first.\n", existing.ID, existing.State)
		return 1
	}

	// Generate run ID from timestamp
	runID := time.Now().Format("20060102-150405")

	// Parse timeline duration
	totalSeconds := 0
	if timeline != "" {
		d, err := parseDuration(timeline)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid timeline %q: %v\n", timeline, err)
			return 1
		}
		totalSeconds = int(d.Seconds())
	}

	// Auto-generate branch if not specified
	if branch == "" {
		branch = fmt.Sprintf("autoresearch/%s", time.Now().Format("jan2"))
	}

	run := &ResearchRun{
		ID:      runID,
		Program: program,
		Branch:  branch,
		State:   ResearchCreated,
		Timeline: ResearchTimeline{
			TotalSeconds: totalSeconds,
			StartedAt:    nowISO(),
		},
		CreatedAt: nowISO(),
	}

	// Compute deadline
	if totalSeconds > 0 {
		deadline := time.Now().Add(time.Duration(totalSeconds) * time.Second)
		run.Timeline.Deadline = deadline.UTC().Format(time.RFC3339)
	}

	// Save run state
	if err := saveResearchRun(root, run); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Initialize results.tsv
	resultsPath := researchResultsPath(root, runID)
	os.MkdirAll(filepath.Dir(resultsPath), 0755)
	os.WriteFile(resultsPath, []byte("commit\trwe\tretention\ttokens\tstatus\tdescription\n"), 0644)

	fmt.Printf("Research run started: %s\n", runID)
	fmt.Printf("  Program:  %s\n", program)
	fmt.Printf("  Branch:   %s\n", branch)
	if totalSeconds > 0 {
		fmt.Printf("  Timeline: %s (deadline: %s)\n", timeline, run.Timeline.Deadline)
	} else {
		fmt.Printf("  Timeline: unlimited\n")
	}
	fmt.Printf("  State:    %s → %s\n", researchRunDir(root, runID), "state.json")
	fmt.Printf("  Results:  %s\n", resultsPath)

	return 0
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func cmdResearchStatus(root string, args []string) int {
	jsonOutput := hasFlag(args, "--json")

	run, err := activeResearchRun(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	if run == nil {
		fmt.Println("No active research run.")
		return 0
	}

	if jsonOutput {
		data, _ := json.MarshalIndent(run, "", "  ")
		fmt.Println(string(data))
		return 0
	}

	fmt.Printf("Research Run: %s\n", run.ID)
	fmt.Printf("  State:       %s\n", run.State)
	fmt.Printf("  Branch:      %s\n", run.Branch)
	fmt.Printf("  Program:     %s\n", run.Program)
	fmt.Printf("  Timeline:    %s elapsed, %s remaining\n",
		run.Timeline.FormatElapsed(), run.Timeline.FormatRemaining())
	fmt.Printf("  Best RWE:    %.1f (commit: %s)\n", run.BestRWE, run.BestCommit)
	fmt.Printf("  Baseline:    %.1f\n", run.BaselineRWE)
	fmt.Printf("  Experiments: %d total — %d kept, %d discarded, %d crashed\n",
		run.Experiments, run.Kept, run.Discarded, run.Crashed)
	if run.BestRWE > 0 && run.BaselineRWE > 0 {
		improvement := ((run.BestRWE - run.BaselineRWE) / run.BaselineRWE) * 100
		fmt.Printf("  Improvement: %+.1f%%\n", improvement)
	}
	return 0
}

// ---------------------------------------------------------------------------
// eval
// ---------------------------------------------------------------------------

func cmdResearchEval(root string, args []string) int {
	commit := ""
	description := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--commit":
			if i+1 < len(args) {
				commit = args[i+1]
				i++
			}
		case "--description":
			if i+1 < len(args) {
				description = args[i+1]
				i++
			}
		}
	}

	if commit == "" {
		fmt.Fprintf(os.Stderr, "Error: --commit is required\n")
		return 1
	}

	// Resolve HEAD to short hash if needed
	if commit == "HEAD" {
		out, err := exec.Command("git", "-C", root, "rev-parse", "--short", "HEAD").Output()
		if err == nil {
			commit = strings.TrimSpace(string(out))
		}
	} else if len(commit) > 7 {
		commit = commit[:7]
	}

	run, err := activeResearchRun(root)
	if err != nil || run == nil {
		fmt.Fprintf(os.Stderr, "Error: no active research run\n")
		return 1
	}

	// Check timeline
	if run.Timeline.Expired() {
		fmt.Fprintf(os.Stderr, "Error: timeline expired\n")
		run.State = ResearchCompleted
		saveResearchRun(root, run)
		return 1
	}

	// Transition to running if needed
	if run.State == ResearchCreated || run.State == ResearchBaseline {
		run.State = ResearchRunning
		saveResearchRun(root, run)
	}

	fmt.Printf("Running experiment: %s — %s\n", commit, description)
	fmt.Printf("  Building test kernel...\n")

	rm := newResearchManager(root, nil)
	result, err := rm.runExperiment(commit, description)

	if err != nil {
		fmt.Fprintf(os.Stderr, "  Experiment failed: %v\n", err)
		if result != nil {
			result.Status = "crash"
			appendExperimentResult(root, run, result)
			run.Experiments++
			run.Crashed++
			saveResearchRun(root, run)
		}
		return 1
	}

	fmt.Printf("  RWE: %.1f  retention: %.3f  tokens: %d\n",
		result.RWE, result.Retention, result.MeanTokens)

	// Return result as JSON for HTTP callers, human-readable for CLI
	output := map[string]interface{}{
		"result": map[string]interface{}{
			"rwe":         result.RWE,
			"retention":   result.Retention,
			"mean_tokens": result.MeanTokens,
		},
		"timeline": map[string]interface{}{
			"remaining": run.Timeline.FormatRemaining(),
			"elapsed":   run.Timeline.FormatElapsed(),
		},
		"best_rwe":    run.BestRWE,
		"experiments": run.Experiments,
		"kept":        run.Kept,
	}
	data, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(data))

	return 0
}

// ---------------------------------------------------------------------------
// keep / discard
// ---------------------------------------------------------------------------

func cmdResearchKeep(root string, args []string) int {
	commit := ""
	description := ""
	rwe := 0.0
	retention := 0.0
	tokens := 0

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--commit":
			if i+1 < len(args) {
				commit = args[i+1]
				i++
			}
		case "--description":
			if i+1 < len(args) {
				description = args[i+1]
				i++
			}
		case "--rwe":
			if i+1 < len(args) {
				rwe, _ = strconv.ParseFloat(args[i+1], 64)
				i++
			}
		case "--retention":
			if i+1 < len(args) {
				retention, _ = strconv.ParseFloat(args[i+1], 64)
				i++
			}
		case "--tokens":
			if i+1 < len(args) {
				tokens, _ = strconv.Atoi(args[i+1])
				i++
			}
		}
	}

	if commit == "" {
		fmt.Fprintf(os.Stderr, "Error: --commit is required\n")
		return 1
	}

	run, err := activeResearchRun(root)
	if err != nil || run == nil {
		fmt.Fprintf(os.Stderr, "Error: no active research run\n")
		return 1
	}

	result := &ExperimentResult{
		Commit:      commit,
		RWE:         rwe,
		Retention:   retention,
		MeanTokens:  tokens,
		Status:      "keep",
		Description: description,
		Timestamp:   nowISO(),
	}

	if err := appendExperimentResult(root, run, result); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing result: %v\n", err)
		return 1
	}

	run.Experiments++
	run.Kept++
	if rwe > run.BestRWE {
		run.BestRWE = rwe
		run.BestCommit = commit
	}
	saveResearchRun(root, run)

	fmt.Printf("Kept: %s (RWE: %.1f) — %s\n", commit, rwe, description)
	return 0
}

func cmdResearchDiscard(root string, args []string) int {
	commit := ""
	description := ""
	rwe := 0.0
	retention := 0.0
	tokens := 0

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--commit":
			if i+1 < len(args) {
				commit = args[i+1]
				i++
			}
		case "--description":
			if i+1 < len(args) {
				description = args[i+1]
				i++
			}
		case "--rwe":
			if i+1 < len(args) {
				rwe, _ = strconv.ParseFloat(args[i+1], 64)
				i++
			}
		case "--retention":
			if i+1 < len(args) {
				retention, _ = strconv.ParseFloat(args[i+1], 64)
				i++
			}
		case "--tokens":
			if i+1 < len(args) {
				tokens, _ = strconv.Atoi(args[i+1])
				i++
			}
		}
	}

	if commit == "" {
		fmt.Fprintf(os.Stderr, "Error: --commit is required\n")
		return 1
	}

	run, err := activeResearchRun(root)
	if err != nil || run == nil {
		fmt.Fprintf(os.Stderr, "Error: no active research run\n")
		return 1
	}

	result := &ExperimentResult{
		Commit:      commit,
		RWE:         rwe,
		Retention:   retention,
		MeanTokens:  tokens,
		Status:      "discard",
		Description: description,
		Timestamp:   nowISO(),
	}

	if err := appendExperimentResult(root, run, result); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing result: %v\n", err)
		return 1
	}

	run.Experiments++
	run.Discarded++
	saveResearchRun(root, run)

	// Git reset to discard the experiment commit
	cmd := exec.Command("git", "-C", root, "reset", "--hard", "HEAD~1")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: git reset failed: %v\n%s\n", err, string(out))
	}

	fmt.Printf("Discarded: %s (RWE: %.1f) — %s\n", commit, rwe, description)
	return 0
}

// ---------------------------------------------------------------------------
// results
// ---------------------------------------------------------------------------

func cmdResearchResults(root string, args []string) int {
	jsonOutput := hasFlag(args, "--json")

	run, err := activeResearchRun(root)
	if err != nil || run == nil {
		// Try to find the most recent run
		runs, _ := listResearchRuns(root)
		if len(runs) > 0 {
			run = runs[len(runs)-1]
		} else {
			fmt.Fprintf(os.Stderr, "Error: no research runs found\n")
			return 1
		}
	}

	results, err := loadExperimentResults(root, run.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	if jsonOutput {
		data, _ := json.MarshalIndent(results, "", "  ")
		fmt.Println(string(data))
		return 0
	}

	if len(results) == 0 {
		fmt.Println("No experiments recorded yet.")
		return 0
	}

	fmt.Printf("%-8s  %6s  %6s  %6s  %-8s  %s\n", "commit", "rwe", "ret", "tok", "status", "description")
	fmt.Println(strings.Repeat("-", 70))
	for _, r := range results {
		fmt.Printf("%-8s  %6.1f  %5.3f  %6d  %-8s  %s\n",
			r.Commit, r.RWE, r.Retention, r.MeanTokens, r.Status, r.Description)
	}
	return 0
}

// ---------------------------------------------------------------------------
// pause / resume / stop
// ---------------------------------------------------------------------------

func cmdResearchPause(root string, _ []string) int {
	run, err := activeResearchRun(root)
	if err != nil || run == nil {
		fmt.Fprintf(os.Stderr, "Error: no active research run\n")
		return 1
	}
	if run.State != ResearchRunning {
		fmt.Fprintf(os.Stderr, "Error: run is %s, not running\n", run.State)
		return 1
	}

	run.State = ResearchPaused
	run.Timeline.PauseNow()
	saveResearchRun(root, run)

	fmt.Printf("Paused research run %s (elapsed: %s)\n", run.ID, run.Timeline.FormatElapsed())
	return 0
}

func cmdResearchResume(root string, _ []string) int {
	run, err := activeResearchRun(root)
	if err != nil || run == nil {
		fmt.Fprintf(os.Stderr, "Error: no active research run\n")
		return 1
	}
	if run.State != ResearchPaused {
		fmt.Fprintf(os.Stderr, "Error: run is %s, not paused\n", run.State)
		return 1
	}

	run.State = ResearchRunning
	run.Timeline.ResumeNow()
	saveResearchRun(root, run)

	fmt.Printf("Resumed research run %s (remaining: %s)\n", run.ID, run.Timeline.FormatRemaining())
	return 0
}

func cmdResearchStop(root string, _ []string) int {
	run, err := activeResearchRun(root)
	if err != nil || run == nil {
		fmt.Fprintf(os.Stderr, "Error: no active research run\n")
		return 1
	}

	// Clean up test kernel if running
	stopTestKernel(root)

	run.State = ResearchStopped
	saveResearchRun(root, run)

	fmt.Printf("Stopped research run %s\n", run.ID)
	fmt.Printf("  Experiments: %d (%d kept, %d discarded, %d crashed)\n",
		run.Experiments, run.Kept, run.Discarded, run.Crashed)
	fmt.Printf("  Best RWE:    %.1f (%s)\n", run.BestRWE, run.BestCommit)
	fmt.Printf("  Elapsed:     %s\n", run.Timeline.FormatElapsed())
	return 0
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdResearchList(root string, args []string) int {
	jsonOutput := hasFlag(args, "--json")

	runs, err := listResearchRuns(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	if len(runs) == 0 {
		fmt.Println("No research runs found.")
		return 0
	}

	if jsonOutput {
		data, _ := json.MarshalIndent(runs, "", "  ")
		fmt.Println(string(data))
		return 0
	}

	fmt.Printf("%-18s  %-10s  %6s  %4s  %4s  %4s  %s\n",
		"id", "state", "rwe", "kept", "disc", "tot", "branch")
	fmt.Println(strings.Repeat("-", 70))
	for _, r := range runs {
		fmt.Printf("%-18s  %-10s  %6.1f  %4d  %4d  %4d  %s\n",
			r.ID, r.State, r.BestRWE, r.Kept, r.Discarded, r.Experiments, r.Branch)
	}
	return 0
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// parseDuration parses human-friendly durations like "8h", "30m", "1h30m", "2d".
func parseDuration(s string) (time.Duration, error) {
	// Handle "d" suffix (days) by converting to hours
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Research state machine
// ---------------------------------------------------------------------------

type ResearchState string

const (
	ResearchCreated   ResearchState = "created"
	ResearchBaseline  ResearchState = "baseline"
	ResearchRunning   ResearchState = "running"
	ResearchPaused    ResearchState = "paused"
	ResearchCompleted ResearchState = "completed"
	ResearchStopped   ResearchState = "stopped"
	ResearchFailed    ResearchState = "failed"
)

// ResearchRun holds the full state of a research run.
type ResearchRun struct {
	ID       string        `json:"id"`
	Program  string        `json:"program"`
	Branch   string        `json:"branch"`
	State    ResearchState `json:"state"`
	BusID    string        `json:"bus_id,omitempty"`
	Timeline ResearchTimeline `json:"timeline"`

	BaselineRWE float64 `json:"baseline_rwe"`
	BestRWE     float64 `json:"best_rwe"`
	BestCommit  string  `json:"best_commit,omitempty"`

	Experiments int `json:"experiments"`
	Kept        int `json:"kept"`
	Discarded   int `json:"discarded"`
	Crashed     int `json:"crashed"`

	AgentPID int `json:"agent_pid,omitempty"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ResearchTimeline tracks wall-clock budget for a research run.
type ResearchTimeline struct {
	TotalSeconds   int    `json:"total_seconds"`
	PausedDuration int    `json:"paused_duration"` // accumulated paused seconds
	StartedAt      string `json:"started_at,omitempty"`
	PausedAt       string `json:"paused_at,omitempty"`
	Deadline       string `json:"deadline,omitempty"`
}

// Elapsed returns how many seconds have elapsed (excluding paused time).
func (t *ResearchTimeline) Elapsed() int {
	if t.StartedAt == "" {
		return 0
	}
	start, err := time.Parse(time.RFC3339, t.StartedAt)
	if err != nil {
		return 0
	}
	total := int(time.Since(start).Seconds())
	return total - t.PausedDuration
}

// Remaining returns seconds left on the timeline. Negative means expired.
func (t *ResearchTimeline) Remaining() int {
	if t.TotalSeconds <= 0 {
		return 999999 // no deadline
	}
	return t.TotalSeconds - t.Elapsed()
}

// Expired returns true if the timeline has run out.
func (t *ResearchTimeline) Expired() bool {
	if t.TotalSeconds <= 0 {
		return false
	}
	return t.Remaining() <= 0
}

// PauseNow records the current time as the pause start.
func (t *ResearchTimeline) PauseNow() {
	t.PausedAt = nowISO()
}

// ResumeNow adds the paused duration and clears the pause marker.
func (t *ResearchTimeline) ResumeNow() {
	if t.PausedAt == "" {
		return
	}
	pauseStart, err := time.Parse(time.RFC3339, t.PausedAt)
	if err == nil {
		t.PausedDuration += int(time.Since(pauseStart).Seconds())
	}
	t.PausedAt = ""
}

// FormatRemaining returns a human-readable remaining time string.
func (t *ResearchTimeline) FormatRemaining() string {
	rem := t.Remaining()
	if rem > 86400 {
		return fmt.Sprintf("%dd%dh", rem/86400, (rem%86400)/3600)
	}
	if rem > 3600 {
		return fmt.Sprintf("%dh%02dm", rem/3600, (rem%3600)/60)
	}
	if rem > 60 {
		return fmt.Sprintf("%dm%02ds", rem/60, rem%60)
	}
	return fmt.Sprintf("%ds", rem)
}

// FormatElapsed returns a human-readable elapsed time string.
func (t *ResearchTimeline) FormatElapsed() string {
	e := t.Elapsed()
	if e > 3600 {
		return fmt.Sprintf("%dh%02dm", e/3600, (e%3600)/60)
	}
	if e > 60 {
		return fmt.Sprintf("%dm%02ds", e/60, e%60)
	}
	return fmt.Sprintf("%ds", e)
}

// ExperimentResult is one row in the results log.
type ExperimentResult struct {
	Commit      string  `json:"commit"`
	RWE         float64 `json:"rwe"`
	Retention   float64 `json:"retention"`
	MeanTokens  int     `json:"mean_tokens"`
	Status      string  `json:"status"` // keep, discard, crash
	Description string  `json:"description"`
	Timestamp   string  `json:"timestamp"`
}

// EvalResult holds parsed output from eval-foveated.py.
type EvalResult struct {
	RWE        float64 `json:"rwe"`
	Retention  float64 `json:"retention"`
	MeanTokens int     `json:"mean_tokens"`
	Efficiency float64 `json:"efficiency"`
	Queries    int     `json:"queries"`
	Pressures  int     `json:"pressures"`
	Pairs      int     `json:"pairs"`
}

// ---------------------------------------------------------------------------
// Persistence — per-run state files
// ---------------------------------------------------------------------------

const researchBaseDir = ".cog/run/research/runs"

func researchRunDir(root, runID string) string {
	return filepath.Join(root, researchBaseDir, runID)
}

func researchStatePath(root, runID string) string {
	return filepath.Join(researchRunDir(root, runID), "state.json")
}

func researchResultsPath(root, runID string) string {
	return filepath.Join(researchRunDir(root, runID), "results.tsv")
}

func loadResearchRun(root, runID string) (*ResearchRun, error) {
	data, err := os.ReadFile(researchStatePath(root, runID))
	if err != nil {
		return nil, fmt.Errorf("load research run %s: %w", runID, err)
	}
	var run ResearchRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("parse research run %s: %w", runID, err)
	}
	return &run, nil
}

func saveResearchRun(root string, run *ResearchRun) error {
	run.UpdatedAt = nowISO()
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal research run: %w", err)
	}
	return writeAtomic(researchStatePath(root, run.ID), data, 0644)
}

// activeResearchRun finds a run in running or paused state.
func activeResearchRun(root string) (*ResearchRun, error) {
	runsDir := filepath.Join(root, researchBaseDir)
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		run, err := loadResearchRun(root, e.Name())
		if err != nil {
			continue
		}
		if run.State == ResearchCreated || run.State == ResearchRunning || run.State == ResearchPaused || run.State == ResearchBaseline {
			return run, nil
		}
	}
	return nil, nil
}

// listResearchRuns returns all runs sorted by creation time (newest first).
func listResearchRuns(root string) ([]*ResearchRun, error) {
	runsDir := filepath.Join(root, researchBaseDir)
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var runs []*ResearchRun
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		run, err := loadResearchRun(root, e.Name())
		if err != nil {
			continue
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// initResultsLog creates the results TSV with header if it doesn't exist.
func initResultsLog(root string, run *ResearchRun) error {
	resultsPath := researchResultsPath(root, run.ID)
	if _, err := os.Stat(resultsPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(resultsPath), 0755); err != nil {
			return err
		}
		return os.WriteFile(resultsPath, []byte("commit\trwe\tretention\ttokens\tstatus\tdescription\n"), 0644)
	}
	return nil
}

// appendExperimentResult appends a TSV row to the results log.
func appendExperimentResult(root string, run *ResearchRun, result *ExperimentResult) error {
	resultsPath := researchResultsPath(root, run.ID)

	// Ensure header exists
	initResultsLog(root, run)

	if result == nil {
		return nil
	}

	line := fmt.Sprintf("%s\t%.1f\t%.3f\t%d\t%s\t%s\n",
		result.Commit, result.RWE, result.Retention, result.MeanTokens,
		result.Status, result.Description)

	f, err := os.OpenFile(resultsPath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// loadExperimentResults reads back all results from the TSV log.
func loadExperimentResults(root, runID string) ([]ExperimentResult, error) {
	data, err := os.ReadFile(researchResultsPath(root, runID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var results []ExperimentResult
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue // skip header
		}
		line := scanner.Text()
		parts := strings.SplitN(line, "\t", 6)
		if len(parts) < 6 {
			continue
		}
		rwe, _ := strconv.ParseFloat(parts[1], 64)
		retention, _ := strconv.ParseFloat(parts[2], 64)
		tokens, _ := strconv.Atoi(parts[3])
		results = append(results, ExperimentResult{
			Commit:      parts[0],
			RWE:         rwe,
			Retention:   retention,
			MeanTokens:  tokens,
			Status:      parts[4],
			Description: parts[5],
		})
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// Test kernel lifecycle
// ---------------------------------------------------------------------------

const (
	testKernelPort    = 5102
	testKernelBinName = "cogos-test"
	testKernelPIDFile = ".cog/run/research/test-kernel.pid"
)

func testKernelBinPath(root string) string {
	return filepath.Join(root, ".cog/run/research", testKernelBinName)
}

// buildTestKernel compiles the cogos binary from the current source into the
// test kernel path. Returns nil on success.
func buildTestKernel(root string) error {
	binPath := testKernelBinPath(root)
	if err := os.MkdirAll(filepath.Dir(binPath), 0755); err != nil {
		return fmt.Errorf("create test kernel dir: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "build", "-tags", "fts5", "-o", binPath, ".")
	cmd.Dir = filepath.Join(root, "apps", "cogos")
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	cmd.Stdout = os.Stderr // build output goes to stderr for logging
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}

	// Codesign on macOS (required for execution without xattr issues)
	if runtime.GOOS == "darwin" {
		sign := exec.Command("codesign", "--sign", "-", "--force", binPath)
		sign.Stdout = os.Stderr
		sign.Stderr = os.Stderr
		if err := sign.Run(); err != nil {
			return fmt.Errorf("codesign failed: %w", err)
		}
	}

	return nil
}

// startTestKernel launches the test kernel on testKernelPort.
// Returns the PID on success. Blocks until healthy (up to 15s).
func startTestKernel(root string) (int, error) {
	binPath := testKernelBinPath(root)
	if _, err := os.Stat(binPath); err != nil {
		return 0, fmt.Errorf("test kernel binary not found: %w", err)
	}

	// Kill any existing process on the test port
	stopTestKernel(root)

	logPath := filepath.Join(root, ".cog/run/research/test-kernel.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return 0, fmt.Errorf("create test kernel log: %w", err)
	}

	cmd := exec.Command(binPath, "serve", "--port", strconv.Itoa(testKernelPort))
	cmd.Dir = root
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setSysProcAttr(cmd) // detach process group

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return 0, fmt.Errorf("start test kernel: %w", err)
	}

	pid := cmd.Process.Pid
	logFile.Close()

	// Write PID file
	pidPath := filepath.Join(root, testKernelPIDFile)
	os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644)

	// Wait for cleanup in background
	go cmd.Wait()

	// Poll for health
	endpoint := fmt.Sprintf("http://localhost:%d/health", testKernelPort)
	for i := 0; i < 15; i++ {
		time.Sleep(1 * time.Second)
		resp, err := httpGetSimple(endpoint)
		if err == nil && strings.Contains(resp, "healthy") {
			return pid, nil
		}
	}

	// Timed out — kill and fail
	stopTestKernel(root)
	return 0, fmt.Errorf("test kernel did not become healthy within 15s (check %s)", logPath)
}

// stopTestKernel kills the test kernel process.
func stopTestKernel(root string) error {
	pidPath := filepath.Join(root, testKernelPIDFile)
	data, err := os.ReadFile(pidPath)
	if err != nil {
		// Also try lsof as fallback
		killByPort(testKernelPort)
		return nil
	}
	os.Remove(pidPath)

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}

	// SIGTERM → wait 3s → SIGKILL
	proc.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		proc.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		proc.Kill()
		<-done
	}

	return nil
}

// killByPort kills whatever is listening on the given port via lsof.
func killByPort(port int) {
	out, err := exec.Command("lsof", "-ti", fmt.Sprintf(":%d", port)).Output()
	if err != nil || len(out) == 0 {
		return
	}
	pids := strings.Fields(strings.TrimSpace(string(out)))
	for _, p := range pids {
		pid, _ := strconv.Atoi(p)
		if pid > 0 {
			if proc, err := os.FindProcess(pid); err == nil {
				proc.Kill()
			}
		}
	}
}

// httpGetSimple does a simple HTTP GET and returns the body as a string.
func httpGetSimple(url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "curl", "-sf", url)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// setSysProcAttr sets Setsid on Unix to detach the process group.
func setSysProcAttr(cmd *exec.Cmd) {
	// Platform-specific — see research_unix.go / research_other.go
	setSysProcAttrImpl(cmd)
}

// ---------------------------------------------------------------------------
// Eval runner
// ---------------------------------------------------------------------------

var evalOutputPattern = regexp.MustCompile(`^(\w[\w_]+):\s+(.+)$`)

// runEval executes the eval harness against the given endpoint and parses results.
func runEval(root, endpoint string) (*EvalResult, error) {
	evalPath := filepath.Join(root, ".cog/run/eval-foveated.py")
	if _, err := os.Stat(evalPath); err != nil {
		return nil, fmt.Errorf("eval harness not found: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", evalPath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), fmt.Sprintf("EVAL_ENDPOINT=%s", endpoint))

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("eval failed: %w\noutput: %s", err, string(out))
	}

	return parseEvalOutput(string(out))
}

// parseEvalOutput extracts the greppable metrics from eval harness output.
func parseEvalOutput(output string) (*EvalResult, error) {
	result := &EvalResult{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		m := evalOutputPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, val := m[1], strings.TrimSpace(m[2])
		switch key {
		case "rwe":
			result.RWE, _ = strconv.ParseFloat(val, 64)
		case "mean_retention":
			result.Retention, _ = strconv.ParseFloat(val, 64)
		case "mean_tokens":
			f, _ := strconv.ParseFloat(val, 64)
			result.MeanTokens = int(f)
		case "mean_efficiency":
			result.Efficiency, _ = strconv.ParseFloat(val, 64)
		case "queries":
			result.Queries, _ = strconv.Atoi(val)
		case "pressures":
			result.Pressures, _ = strconv.Atoi(val)
		case "pairs":
			result.Pairs, _ = strconv.Atoi(val)
		}
	}
	if result.RWE == 0 && result.Pairs == 0 {
		return nil, fmt.Errorf("failed to parse eval output (no rwe or pairs found)")
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Research manager — coordinates the full experiment lifecycle
// ---------------------------------------------------------------------------

type researchManager struct {
	mu   sync.Mutex
	root string
	bus  *busSessionManager // may be nil in CLI-only mode
}

func newResearchManager(root string, bus *busSessionManager) *researchManager {
	return &researchManager{root: root, bus: bus}
}

// runExperiment executes a full build → start → eval → stop cycle.
// Returns the eval result (or error). The test kernel is always stopped
// on return, even on error.
func (rm *researchManager) runExperiment(commit, description string) (*ExperimentResult, error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	result := &ExperimentResult{
		Commit:      commit,
		Description: description,
		Timestamp:   nowISO(),
	}

	// 1. Build
	rm.emitEvent("research.experiment.build", map[string]interface{}{"commit": commit})
	if err := buildTestKernel(rm.root); err != nil {
		result.Status = "crash"
		return result, fmt.Errorf("build: %w", err)
	}

	// 2. Start test kernel
	rm.emitEvent("research.experiment.start", map[string]interface{}{"commit": commit})
	pid, err := startTestKernel(rm.root)
	if err != nil {
		result.Status = "crash"
		return result, fmt.Errorf("start: %w", err)
	}
	_ = pid

	// 3. Eval against test kernel
	endpoint := fmt.Sprintf("http://localhost:%d", testKernelPort)
	rm.emitEvent("research.experiment.eval", map[string]interface{}{"commit": commit, "endpoint": endpoint})
	evalResult, err := runEval(rm.root, endpoint)

	// 4. Always stop test kernel
	stopTestKernel(rm.root)

	if err != nil {
		result.Status = "crash"
		return result, fmt.Errorf("eval: %w", err)
	}

	result.RWE = evalResult.RWE
	result.Retention = evalResult.Retention
	result.MeanTokens = evalResult.MeanTokens

	return result, nil
}

// emitEvent sends a bus event if a bus manager is available.
func (rm *researchManager) emitEvent(eventType string, payload map[string]interface{}) {
	if rm.bus == nil {
		return
	}
	// Find active research run's bus ID
	run, err := activeResearchRun(rm.root)
	if err != nil || run == nil || run.BusID == "" {
		return
	}
	rm.bus.appendBusEvent(run.BusID, eventType, "research-mgr", payload)
}

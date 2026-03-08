package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Research HTTP handlers — mounted on serveServer
// ---------------------------------------------------------------------------

// POST /v1/research/start
func (s *serveServer) handleResearchStart(w http.ResponseWriter, r *http.Request) {
	root := s.workspaceRoot()
	if root == "" {
		s.writeError(w, http.StatusInternalServerError, "no workspace root", "server_error")
		return
	}

	var req struct {
		Program  string `json:"program"`
		Timeline string `json:"timeline,omitempty"`
		Branch   string `json:"branch,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request")
		return
	}
	if req.Program == "" {
		s.writeError(w, http.StatusBadRequest, "program is required", "invalid_request")
		return
	}

	// Check no active run
	existing, _ := activeResearchRun(root)
	if existing != nil {
		s.writeError(w, http.StatusConflict,
			fmt.Sprintf("active run %s already exists (state: %s)", existing.ID, existing.State),
			"conflict")
		return
	}

	runID := time.Now().Format("20060102-150405")

	totalSeconds := 0
	if req.Timeline != "" {
		d, err := parseDuration(req.Timeline)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid timeline: "+err.Error(), "invalid_request")
			return
		}
		totalSeconds = int(d.Seconds())
	}

	branch := req.Branch
	if branch == "" {
		branch = fmt.Sprintf("autoresearch/%s", time.Now().Format("jan2"))
	}

	run := &ResearchRun{
		ID:      runID,
		Program: req.Program,
		Branch:  branch,
		State:   ResearchCreated,
		Timeline: ResearchTimeline{
			TotalSeconds: totalSeconds,
			StartedAt:    nowISO(),
		},
		CreatedAt: nowISO(),
	}

	if totalSeconds > 0 {
		deadline := time.Now().Add(time.Duration(totalSeconds) * time.Second)
		run.Timeline.Deadline = deadline.UTC().Format(time.RFC3339)
	}

	if err := saveResearchRun(root, run); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}

	// Initialize results.tsv
	initResultsLog(root, run)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(run)
}

// GET /v1/research/status
func (s *serveServer) handleResearchStatus(w http.ResponseWriter, r *http.Request) {
	root := s.workspaceRoot()
	if root == "" {
		s.writeError(w, http.StatusInternalServerError, "no workspace root", "server_error")
		return
	}

	run, err := activeResearchRun(root)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	if run == nil {
		s.writeError(w, http.StatusNotFound, "no active research run", "not_found")
		return
	}

	resp := map[string]interface{}{
		"run": run,
		"timeline": map[string]interface{}{
			"remaining": run.Timeline.FormatRemaining(),
			"elapsed":   run.Timeline.FormatElapsed(),
			"expired":   run.Timeline.Expired(),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// POST /v1/research/eval
func (s *serveServer) handleResearchEval(w http.ResponseWriter, r *http.Request) {
	root := s.workspaceRoot()
	if root == "" {
		s.writeError(w, http.StatusInternalServerError, "no workspace root", "server_error")
		return
	}

	var req struct {
		Commit      string `json:"commit"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request")
		return
	}
	if req.Commit == "" {
		s.writeError(w, http.StatusBadRequest, "commit is required", "invalid_request")
		return
	}

	run, err := activeResearchRun(root)
	if err != nil || run == nil {
		s.writeError(w, http.StatusNotFound, "no active research run", "not_found")
		return
	}

	if run.Timeline.Expired() {
		run.State = ResearchCompleted
		saveResearchRun(root, run)
		s.writeError(w, http.StatusConflict, "timeline expired", "expired")
		return
	}

	// Resolve HEAD
	commit := req.Commit
	if commit == "HEAD" {
		out, err := exec.Command("git", "-C", root, "rev-parse", "--short", "HEAD").Output()
		if err == nil {
			commit = strings.TrimSpace(string(out))
		}
	} else if len(commit) > 7 {
		commit = commit[:7]
	}

	// Transition to running
	if run.State == ResearchCreated || run.State == ResearchBaseline {
		run.State = ResearchRunning
		saveResearchRun(root, run)
	}

	rm := s.researchMgr
	if rm == nil {
		rm = newResearchManager(root, nil)
	}

	result, err := rm.runExperiment(commit, req.Description)
	if err != nil {
		// Record crash
		if result != nil {
			result.Status = "crash"
			appendExperimentResult(root, run, result)
			run.Experiments++
			run.Crashed++
			saveResearchRun(root, run)
		}
		s.writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("experiment failed: %v", err), "experiment_error")
		return
	}

	// Build response with iris signal (timeline + metrics)
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"rwe":         result.RWE,
			"retention":   result.Retention,
			"mean_tokens": result.MeanTokens,
			"commit":      commit,
		},
		"timeline": map[string]interface{}{
			"remaining": run.Timeline.FormatRemaining(),
			"elapsed":   run.Timeline.FormatElapsed(),
			"expired":   run.Timeline.Expired(),
		},
		"best_rwe":    run.BestRWE,
		"experiments": run.Experiments,
		"kept":        run.Kept,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// POST /v1/research/keep
func (s *serveServer) handleResearchKeep(w http.ResponseWriter, r *http.Request) {
	root := s.workspaceRoot()
	if root == "" {
		s.writeError(w, http.StatusInternalServerError, "no workspace root", "server_error")
		return
	}

	var req struct {
		Commit      string  `json:"commit"`
		RWE         float64 `json:"rwe"`
		Retention   float64 `json:"retention"`
		MeanTokens  int     `json:"mean_tokens"`
		Description string  `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request")
		return
	}

	run, _ := activeResearchRun(root)
	if run == nil {
		s.writeError(w, http.StatusNotFound, "no active research run", "not_found")
		return
	}

	result := &ExperimentResult{
		Commit:      req.Commit,
		RWE:         req.RWE,
		Retention:   req.Retention,
		MeanTokens:  req.MeanTokens,
		Status:      "keep",
		Description: req.Description,
		Timestamp:   nowISO(),
	}

	appendExperimentResult(root, run, result)
	run.Experiments++
	run.Kept++
	if req.RWE > run.BestRWE {
		run.BestRWE = req.RWE
		run.BestCommit = req.Commit
	}
	saveResearchRun(root, run)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "kept",
		"best_rwe": run.BestRWE,
		"kept":     run.Kept,
	})
}

// POST /v1/research/discard
func (s *serveServer) handleResearchDiscard(w http.ResponseWriter, r *http.Request) {
	root := s.workspaceRoot()
	if root == "" {
		s.writeError(w, http.StatusInternalServerError, "no workspace root", "server_error")
		return
	}

	var req struct {
		Commit      string  `json:"commit"`
		RWE         float64 `json:"rwe"`
		Retention   float64 `json:"retention"`
		MeanTokens  int     `json:"mean_tokens"`
		Description string  `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request")
		return
	}

	run, _ := activeResearchRun(root)
	if run == nil {
		s.writeError(w, http.StatusNotFound, "no active research run", "not_found")
		return
	}

	result := &ExperimentResult{
		Commit:      req.Commit,
		RWE:         req.RWE,
		Retention:   req.Retention,
		MeanTokens:  req.MeanTokens,
		Status:      "discard",
		Description: req.Description,
		Timestamp:   nowISO(),
	}

	appendExperimentResult(root, run, result)
	run.Experiments++
	run.Discarded++
	saveResearchRun(root, run)

	// Git reset
	cmd := exec.Command("git", "-C", root, "reset", "--hard", "HEAD~1")
	gitOut, gitErr := cmd.CombinedOutput()

	resp := map[string]interface{}{
		"status":    "discarded",
		"discarded": run.Discarded,
	}
	if gitErr != nil {
		resp["git_warning"] = fmt.Sprintf("reset failed: %v — %s", gitErr, string(gitOut))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// POST /v1/research/pause
func (s *serveServer) handleResearchPause(w http.ResponseWriter, r *http.Request) {
	root := s.workspaceRoot()
	if root == "" {
		s.writeError(w, http.StatusInternalServerError, "no workspace root", "server_error")
		return
	}

	run, _ := activeResearchRun(root)
	if run == nil {
		s.writeError(w, http.StatusNotFound, "no active research run", "not_found")
		return
	}
	if run.State != ResearchRunning {
		s.writeError(w, http.StatusConflict,
			fmt.Sprintf("run is %s, not running", run.State), "invalid_state")
		return
	}

	run.State = ResearchPaused
	run.Timeline.PauseNow()
	saveResearchRun(root, run)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "paused",
		"elapsed": run.Timeline.FormatElapsed(),
	})
}

// POST /v1/research/resume
func (s *serveServer) handleResearchResume(w http.ResponseWriter, r *http.Request) {
	root := s.workspaceRoot()
	if root == "" {
		s.writeError(w, http.StatusInternalServerError, "no workspace root", "server_error")
		return
	}

	run, _ := activeResearchRun(root)
	if run == nil {
		s.writeError(w, http.StatusNotFound, "no active research run", "not_found")
		return
	}
	if run.State != ResearchPaused {
		s.writeError(w, http.StatusConflict,
			fmt.Sprintf("run is %s, not paused", run.State), "invalid_state")
		return
	}

	run.State = ResearchRunning
	run.Timeline.ResumeNow()
	saveResearchRun(root, run)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "resumed",
		"remaining": run.Timeline.FormatRemaining(),
	})
}

// POST /v1/research/stop
func (s *serveServer) handleResearchStop(w http.ResponseWriter, r *http.Request) {
	root := s.workspaceRoot()
	if root == "" {
		s.writeError(w, http.StatusInternalServerError, "no workspace root", "server_error")
		return
	}

	run, _ := activeResearchRun(root)
	if run == nil {
		s.writeError(w, http.StatusNotFound, "no active research run", "not_found")
		return
	}

	stopTestKernel(root)

	run.State = ResearchStopped
	saveResearchRun(root, run)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "stopped",
		"experiments": run.Experiments,
		"kept":        run.Kept,
		"best_rwe":    run.BestRWE,
		"elapsed":     run.Timeline.FormatElapsed(),
	})
}

// GET /v1/research/results
func (s *serveServer) handleResearchResults(w http.ResponseWriter, r *http.Request) {
	root := s.workspaceRoot()
	if root == "" {
		s.writeError(w, http.StatusInternalServerError, "no workspace root", "server_error")
		return
	}

	run, _ := activeResearchRun(root)
	if run == nil {
		// Try most recent
		runs, _ := listResearchRuns(root)
		if len(runs) > 0 {
			run = runs[len(runs)-1]
		}
	}
	if run == nil {
		s.writeError(w, http.StatusNotFound, "no research runs found", "not_found")
		return
	}

	results, err := loadExperimentResults(root, run.ID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}

	resp := map[string]interface{}{
		"run_id":  run.ID,
		"results": results,
		"summary": map[string]interface{}{
			"experiments": run.Experiments,
			"kept":        run.Kept,
			"discarded":   run.Discarded,
			"crashed":     run.Crashed,
			"best_rwe":    run.BestRWE,
			"best_commit": run.BestCommit,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// workspaceRoot returns the root path of the default workspace.
func (s *serveServer) workspaceRoot() string {
	if ws := s.getWorkspace(""); ws != nil {
		return ws.root
	}
	if s.kernel != nil {
		return s.kernel.Root()
	}
	return ""
}

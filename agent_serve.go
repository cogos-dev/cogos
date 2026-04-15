// agent_serve.go — Homeostatic agent loop for cog serve.
//
// Runs the native Go agent harness on a 30-minute ticker inside the kernel
// process. Each cycle: gathers workspace observations, calls E4B for
// assessment, and executes actions through kernel-native tools.
//
// Integration: Created in cmdServeForeground() alongside the reconciler.
//   agent := NewServeAgent(root)
//   agent.SetBus(busManager)
//   agent.Start()
//   defer agent.Stop()
//
// The reconciler and agent loop are complementary:
//   - Reconciler: declarative state convergence (every 5 min)
//   - Agent: observation-driven assessment and action (every 30 min)

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	agentIntervalMin     = 5 * time.Minute  // start fast
	agentIntervalMax     = 30 * time.Minute // relax to this after consecutive sleeps
	agentBusID           = "bus_agent_harness"
)

// AgentStatusResponse is the JSON payload for GET /v1/agent/status.
type AgentStatusResponse struct {
	Alive      bool    `json:"alive"`
	Uptime     string  `json:"uptime"`
	UptimeSec  int64   `json:"uptime_sec"`
	CycleCount int64   `json:"cycle_count"`
	LastCycle  string  `json:"last_cycle,omitempty"`  // RFC3339
	LastAction string  `json:"last_action,omitempty"`
	LastUrgency float64 `json:"last_urgency"`
	LastReason string  `json:"last_reason,omitempty"`
	LastDurMs  int64   `json:"last_duration_ms"`
	Interval   string  `json:"interval"`
	Model      string  `json:"model"`
}

// ServeAgent runs the homeostatic agent loop inside cog serve.
type ServeAgent struct {
	root     string
	interval time.Duration
	harness  *AgentHarness
	bus      *busSessionManager
	stopCh   chan struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// Metrics (read via Status())
	mu          sync.RWMutex
	startedAt   time.Time
	lastRun     time.Time
	cycleCount  int64
	lastAction  string
	lastUrgency float64
	lastReason  string
	lastDurMs   int64
}

// Status returns the current agent loop status for the API.
func (sa *ServeAgent) Status() AgentStatusResponse {
	sa.mu.RLock()
	defer sa.mu.RUnlock()

	uptime := time.Since(sa.startedAt)
	resp := AgentStatusResponse{
		Alive:       true,
		Uptime:      agentFormatDuration(uptime),
		UptimeSec:   int64(uptime.Seconds()),
		CycleCount:  sa.cycleCount,
		LastAction:  sa.lastAction,
		LastUrgency: sa.lastUrgency,
		LastReason:  sa.lastReason,
		LastDurMs:   sa.lastDurMs,
		Interval:    sa.interval.String(),
		Model:       sa.harness.model,
	}
	if !sa.lastRun.IsZero() {
		resp.LastCycle = sa.lastRun.Format(time.RFC3339)
	}
	return resp
}

// agentFormatDuration returns a human-readable duration like "4h 23m".
func agentFormatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// NewServeAgent creates an agent loop for the given workspace.
// Starts at agentIntervalMin (5m) and relaxes toward agentIntervalMax (30m)
// when the model reports consecutive "sleep" assessments.
func NewServeAgent(root string) *ServeAgent {
	interval := agentIntervalMin

	// Allow override via env var (disables adaptive interval)
	if v := os.Getenv("COG_AGENT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}

	// Build harness pointing at Ollama
	ollamaURL := os.Getenv("OLLAMA_HOST")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	ollamaURL = strings.TrimRight(ollamaURL, "/")

	model := os.Getenv("COG_AGENT_MODEL")
	if model == "" {
		model = "gemma4:e4b"
	}

	harness := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: ollamaURL,
		Model:     model,
	})
	RegisterCoreTools(harness, root)

	return &ServeAgent{
		root:     root,
		interval: interval,
		harness:  harness,
		stopCh:   make(chan struct{}),
	}
}

// SetBus attaches a bus session manager for emitting agent events.
func (sa *ServeAgent) SetBus(mgr *busSessionManager) {
	sa.bus = mgr
}

// Start launches the agent loop in a goroutine.
func (sa *ServeAgent) Start() error {
	log.Printf("[agent] starting homeostatic loop (interval=%s, model=%s)", sa.interval, sa.harness.model)

	sa.mu.Lock()
	sa.startedAt = time.Now()
	sa.mu.Unlock()

	// Ensure the agent bus exists and is registered so events appear
	// in /v1/bus/list and cross-bus queries.
	if sa.bus != nil {
		sa.ensureBus()
	}

	ctx, cancel := context.WithCancel(context.Background())
	sa.cancel = cancel

	sa.wg.Add(1)
	go sa.runLoop(ctx)

	return nil
}

// ensureBus creates the bus directory, events file, and registry entry
// for the agent harness bus if they don't already exist.
func (sa *ServeAgent) ensureBus() {
	busDir := filepath.Join(sa.bus.busesDir(), agentBusID)
	if err := os.MkdirAll(busDir, 0755); err != nil {
		log.Printf("[agent] failed to create bus dir: %v", err)
		return
	}
	eventsFile := filepath.Join(busDir, "events.jsonl")
	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		f, err := os.Create(eventsFile)
		if err != nil {
			log.Printf("[agent] failed to create events file: %v", err)
			return
		}
		f.Close()
	}
	if err := sa.bus.registerBus(agentBusID, "kernel:agent", "kernel:agent"); err != nil {
		log.Printf("[agent] failed to register bus: %v", err)
	}
}

// Stop signals the loop to stop and waits for completion.
func (sa *ServeAgent) Stop() {
	sa.cancel()
	close(sa.stopCh)
	sa.wg.Wait()
	sa.mu.RLock()
	count := sa.cycleCount
	sa.mu.RUnlock()
	log.Printf("[agent] stopped after %d cycles", count)
}

// runLoop is the main ticker loop with adaptive interval.
// Starts at agentIntervalMin, doubles toward agentIntervalMax on consecutive
// "sleep" assessments, resets to agentIntervalMin on any non-sleep action.
//
// The loop is resilient: panics in runCycle are recovered and logged,
// and the loop continues after a backoff delay.
func (sa *ServeAgent) runLoop(ctx context.Context) {
	defer sa.wg.Done()

	consecutiveSleeps := 0

	// Run initial cycle after a short delay (let the kernel fully initialize)
	select {
	case <-time.After(60 * time.Second):
		action := sa.safeCycle(ctx)
		consecutiveSleeps = sa.updateSleepCount(action, consecutiveSleeps)
	case <-sa.stopCh:
		return
	}

	for {
		// Adaptive interval: double on each consecutive sleep, cap at max
		interval := sa.interval
		for i := 0; i < consecutiveSleeps && interval < agentIntervalMax; i++ {
			interval *= 2
		}
		if interval > agentIntervalMax {
			interval = agentIntervalMax
		}

		log.Printf("[agent] next cycle in %s (consecutive sleeps: %d)", interval, consecutiveSleeps)

		select {
		case <-time.After(interval):
			action := sa.safeCycle(ctx)
			consecutiveSleeps = sa.updateSleepCount(action, consecutiveSleeps)
		case <-sa.stopCh:
			return
		}
	}
}

// safeCycle wraps runCycle with panic recovery so the loop survives crashes.
func (sa *ServeAgent) safeCycle(ctx context.Context) (action string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] PANIC recovered in cycle: %v", r)
			action = "error"
		}
	}()
	return sa.runCycle(ctx)
}

// updateSleepCount returns the new consecutive sleep counter.
func (sa *ServeAgent) updateSleepCount(action string, current int) int {
	if action == "sleep" {
		return current + 1
	}
	return 0
}

// runCycle executes a single observe-assess-execute pass.
// Returns the assessment action string for adaptive interval logic.
func (sa *ServeAgent) runCycle(ctx context.Context) string {
	start := time.Now()
	sa.mu.Lock()
	sa.cycleCount++
	cycle := sa.cycleCount
	sa.mu.Unlock()

	log.Printf("[agent] cycle %d: starting", cycle)

	// Build observation from workspace state
	observation := sa.gatherObservation()

	// System prompt: concise, no thinking tags (Gemma E4B doesn't need them).
	// JSON mode is enforced by the harness via response_format.
	systemPrompt := fmt.Sprintf(`You are the CogOS kernel agent on a local node. Workspace: %s

Respond ONLY with a JSON object. No markdown, no explanation, no thinking.

{"action": "<sleep|consolidate|repair|observe|escalate>", "reason": "<brief reason>", "urgency": <0.0-1.0>, "target": "<URI or path or empty>"}

Actions:
- sleep: nothing needs attention
- consolidate: organize memory, clean stale docs
- repair: fix coherence drift or broken state
- observe: gather more info before acting (use tools)
- escalate: beyond local capability, needs cloud model`, sa.root)

	assessment, executeResult, err := sa.harness.RunCycle(ctx, systemPrompt, observation)
	duration := time.Since(start)

	if err != nil {
		log.Printf("[agent] cycle %d: error: %v (%s)", cycle, err, duration.Round(time.Millisecond))
		sa.emitEvent("agent.error", map[string]interface{}{
			"cycle": cycle,
			"error": err.Error(),
		})
		return "error"
	}

	// Update status fields for the API
	sa.mu.Lock()
	sa.lastRun = time.Now()
	sa.cycleCount = cycle
	sa.lastAction = assessment.Action
	sa.lastUrgency = assessment.Urgency
	sa.lastReason = assessment.Reason
	sa.lastDurMs = duration.Milliseconds()
	sa.mu.Unlock()

	log.Printf("[agent] cycle %d: action=%s urgency=%.1f reason=%q (%s)",
		cycle, assessment.Action, assessment.Urgency, assessment.Reason, duration.Round(time.Millisecond))

	sa.emitEvent("agent.cycle", map[string]interface{}{
		"cycle":       cycle,
		"action":      assessment.Action,
		"reason":      assessment.Reason,
		"urgency":     assessment.Urgency,
		"target":      assessment.Target,
		"duration_ms": duration.Milliseconds(),
		"executed":    executeResult != "",
	})

	if assessment.Action == "escalate" {
		log.Printf("[agent] cycle %d: escalation requested — %s (target: %s)",
			cycle, assessment.Reason, assessment.Target)
		sa.emitEvent("agent.escalation", map[string]interface{}{
			"cycle":  cycle,
			"reason": assessment.Reason,
			"target": assessment.Target,
		})
	}

	return assessment.Action
}

// gatherObservation builds a compact observation string from workspace state.
func (sa *ServeAgent) gatherObservation() string {
	var sb strings.Builder

	sb.WriteString("=== Workspace Observation ===\n")
	sb.WriteString(fmt.Sprintf("Time: %s\n", time.Now().Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("Workspace: %s\n\n", sa.root))

	// Git status (quick)
	if status, err := runQuietCommand(sa.root, "git", "status", "--porcelain"); err == nil {
		lines := strings.Split(strings.TrimSpace(status), "\n")
		if len(lines) > 0 && lines[0] != "" {
			sb.WriteString(fmt.Sprintf("Git: %d modified files\n", len(lines)))
		} else {
			sb.WriteString("Git: clean\n")
		}
	}

	// Recent memory activity
	if recent, err := runQuietCommand(sa.root, "./scripts/cog", "memory", "search", "--recent", "1h"); err == nil && recent != "" {
		// Just note whether there was recent activity
		lines := strings.Split(strings.TrimSpace(recent), "\n")
		sb.WriteString(fmt.Sprintf("Memory: %d recent docs\n", len(lines)))
	}

	// Coherence check
	if coh, err := runQuietCommand(sa.root, "./scripts/cog", "coherence", "check"); err == nil {
		if strings.Contains(coh, "coherent") {
			sb.WriteString("Coherence: OK\n")
		} else {
			sb.WriteString(fmt.Sprintf("Coherence: DRIFT — %s\n", strings.TrimSpace(coh)))
		}
	}

	// Kernel uptime
	sa.mu.RLock()
	currentCycle := sa.cycleCount
	sa.mu.RUnlock()
	sb.WriteString(fmt.Sprintf("Agent cycle: %d\n", currentCycle+1))
	if !sa.lastRun.IsZero() {
		sb.WriteString(fmt.Sprintf("Last cycle: %s ago\n", time.Since(sa.lastRun).Round(time.Second)))
	}

	return sb.String()
}

// emitEvent sends an event to the CogBus (best-effort).
func (sa *ServeAgent) emitEvent(eventType string, payload map[string]interface{}) {
	if sa.bus == nil {
		return
	}
	if _, err := sa.bus.appendBusEvent(agentBusID, eventType, "kernel:agent", payload); err != nil {
		log.Printf("[agent] bus event emit error: %v", err)
	}
}

// handleAgentStatus serves GET /v1/agent/status.
func (s *serveServer) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.agent == nil {
		json.NewEncoder(w).Encode(AgentStatusResponse{Alive: false, Model: "none"})
		return
	}
	json.NewEncoder(w).Encode(s.agent.Status())
}

// runQuietCommand runs a command and returns stdout, suppressing stderr.
func runQuietCommand(dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

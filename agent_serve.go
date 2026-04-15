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
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAgentInterval = 30 * time.Minute
	agentBusID           = "bus_agent_harness"
)

// ServeAgent runs the homeostatic agent loop inside cog serve.
type ServeAgent struct {
	root     string
	interval time.Duration
	harness  *AgentHarness
	bus      *busSessionManager
	stopCh   chan struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// Metrics
	lastRun    time.Time
	cycleCount int64
}

// NewServeAgent creates an agent loop for the given workspace.
func NewServeAgent(root string) *ServeAgent {
	interval := defaultAgentInterval

	// Allow override via env var
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
	ollamaURL = strings.TrimRight(ollamaURL, "/") + "/v1"

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

	ctx, cancel := context.WithCancel(context.Background())
	sa.cancel = cancel

	sa.wg.Add(1)
	go sa.runLoop(ctx)

	return nil
}

// Stop signals the loop to stop and waits for completion.
func (sa *ServeAgent) Stop() {
	sa.cancel()
	close(sa.stopCh)
	sa.wg.Wait()
	log.Printf("[agent] stopped after %d cycles", atomic.LoadInt64(&sa.cycleCount))
}

// runLoop is the main ticker loop.
func (sa *ServeAgent) runLoop(ctx context.Context) {
	defer sa.wg.Done()

	// Run initial cycle after a delay (let the kernel fully initialize)
	select {
	case <-time.After(60 * time.Second):
		sa.runCycle(ctx)
	case <-sa.stopCh:
		return
	}

	ticker := time.NewTicker(sa.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			sa.runCycle(ctx)
		case <-sa.stopCh:
			return
		}
	}
}

// runCycle executes a single observe-assess-execute pass.
func (sa *ServeAgent) runCycle(ctx context.Context) {
	start := time.Now()
	cycle := atomic.AddInt64(&sa.cycleCount, 1)

	log.Printf("[agent] cycle %d: starting", cycle)

	// Build observation from workspace state
	observation := sa.gatherObservation()

	systemPrompt := fmt.Sprintf(`You are the CogOS kernel agent running on a local node.
Your workspace is at: %s
You observe the workspace state and decide what needs attention.

Respond with a JSON object:
{"action": "<sleep|consolidate|repair|observe|escalate>", "reason": "<why>", "urgency": <0-1>, "target": "<what to act on>"}

Actions:
- sleep: nothing needs attention right now
- consolidate: memory needs organizing, stale docs need cleanup
- repair: coherence drift detected, something is broken
- observe: need more information before acting (use tools)
- escalate: this is beyond local model capability, needs cloud model`, sa.root)

	assessment, executeResult, err := sa.harness.RunCycle(ctx, systemPrompt, observation)
	duration := time.Since(start)

	if err != nil {
		log.Printf("[agent] cycle %d: error: %v (%s)", cycle, err, duration.Round(time.Millisecond))
		sa.emitEvent("agent.error", map[string]interface{}{
			"cycle": cycle,
			"error": err.Error(),
		})
		return
	}

	sa.lastRun = time.Now()

	log.Printf("[agent] cycle %d: action=%s urgency=%.1f reason=%q (%s)",
		cycle, assessment.Action, assessment.Urgency, assessment.Reason, duration.Round(time.Millisecond))

	sa.emitEvent("agent.cycle", map[string]interface{}{
		"cycle":      cycle,
		"action":     assessment.Action,
		"reason":     assessment.Reason,
		"urgency":    assessment.Urgency,
		"target":     assessment.Target,
		"duration_ms": duration.Milliseconds(),
		"executed":   executeResult != "",
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
	sb.WriteString(fmt.Sprintf("Agent cycle: %d\n", atomic.LoadInt64(&sa.cycleCount)+1))
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

// runQuietCommand runs a command and returns stdout, suppressing stderr.
func runQuietCommand(dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

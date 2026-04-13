// procmgr.go — Process lifecycle manager for Claude Code subprocesses.
//
// Tracks all spawned claude processes (foreground, background, agent).
// Handles:
//   - Client disconnect / cancellation (SIGTERM → SIGKILL escalation)
//   - Background process lifecycle (outlive the HTTP request)
//   - Concurrent process limits (per-identity and global)
//   - Process inventory for observability
//   - Callback delivery when background tasks complete
//
// Process kinds:
//   - Foreground: tied to an HTTP request. Killed on client disconnect.
//   - Background: fire-and-forget. Has its own timeout. Reports via callback.
//   - Agent: runs in a Docker container. Trust-bounded. Future implementation.
package engine

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// ProcessKind classifies how a process is managed.
type ProcessKind int

const (
	// ProcessForeground is tied to an HTTP request and killed on disconnect.
	ProcessForeground ProcessKind = iota

	// ProcessBackground outlives the request and reports via callback.
	ProcessBackground

	// ProcessAgent runs in a sandboxed Docker container.
	ProcessAgent
)

func (k ProcessKind) String() string {
	switch k {
	case ProcessForeground:
		return "foreground"
	case ProcessBackground:
		return "background"
	case ProcessAgent:
		return "agent"
	default:
		return "unknown"
	}
}

// ProcessStatus tracks the lifecycle state of a managed process.
type ProcessStatus int

const (
	ProcessRunning ProcessStatus = iota
	ProcessCompleted
	ProcessFailed
	ProcessCancelled
	ProcessTimedOut
)

func (s ProcessStatus) String() string {
	switch s {
	case ProcessRunning:
		return "running"
	case ProcessCompleted:
		return "completed"
	case ProcessFailed:
		return "failed"
	case ProcessCancelled:
		return "cancelled"
	case ProcessTimedOut:
		return "timed_out"
	default:
		return "unknown"
	}
}

// ManagedProcess tracks a single Claude Code subprocess.
type ManagedProcess struct {
	mu sync.Mutex

	ID              string        `json:"id"`
	Kind            ProcessKind   `json:"kind"`
	Status          ProcessStatus `json:"status"`
	Source          string        `json:"source"`           // "http", "discord", "signal", etc.
	CallbackChannel string        `json:"callback_channel"` // where to deliver results
	Identity        string        `json:"identity"`         // NodeID of requestor
	StartedAt       time.Time     `json:"started_at"`
	FinishedAt      *time.Time    `json:"finished_at,omitempty"`
	Error           string        `json:"error,omitempty"`

	// Internal — not serialized.
	cmd    *exec.Cmd
	cancel func() // context cancel function
	Usage  *TokenUsage
}

// SetError records an error on the process.
func (p *ManagedProcess) SetError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.Error = err.Error()
		p.Status = ProcessFailed
	}
}

// ManagedProcessOpts configures tracking for a new process.
type ManagedProcessOpts struct {
	Kind            ProcessKind
	Source          string
	CallbackChannel string
	Identity        string
	Cancel          func()
}

// ProcessManager tracks all active Claude Code subprocesses.
type ProcessManager struct {
	mu        sync.RWMutex
	processes map[string]*ManagedProcess

	// Limits.
	maxGlobal     int // max concurrent processes across all identities
	maxPerIdentity int // max concurrent processes per NodeID

	// Callback handler — called when a background process finishes.
	// Nil means no callbacks (results are just logged).
	onComplete func(proc *ManagedProcess)

	// Graceful shutdown signal.
	shutdownCh chan struct{}
}

// ProcessManagerConfig configures the process manager.
type ProcessManagerConfig struct {
	MaxGlobal      int // 0 = unlimited
	MaxPerIdentity int // 0 = unlimited
}

// NewProcessManager creates a process manager.
func NewProcessManager(cfg ProcessManagerConfig) *ProcessManager {
	maxGlobal := cfg.MaxGlobal
	if maxGlobal == 0 {
		maxGlobal = 20 // sensible default
	}
	maxPerIdentity := cfg.MaxPerIdentity
	if maxPerIdentity == 0 {
		maxPerIdentity = 5
	}
	return &ProcessManager{
		processes:      make(map[string]*ManagedProcess),
		maxGlobal:      maxGlobal,
		maxPerIdentity: maxPerIdentity,
		shutdownCh:     make(chan struct{}),
	}
}

// SetOnComplete registers a callback for when background processes finish.
func (pm *ProcessManager) SetOnComplete(fn func(*ManagedProcess)) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onComplete = fn
}

// Track registers a new process with the manager. Call before cmd.Start().
func (pm *ProcessManager) Track(cmd *exec.Cmd, opts ManagedProcessOpts) *ManagedProcess {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	proc := &ManagedProcess{
		ID:              uuid.New().String(),
		Kind:            opts.Kind,
		Status:          ProcessRunning,
		Source:          opts.Source,
		CallbackChannel: opts.CallbackChannel,
		Identity:        opts.Identity,
		StartedAt:       time.Now(),
		cmd:             cmd,
		cancel:          opts.Cancel,
	}

	pm.processes[proc.ID] = proc

	slog.Info("procmgr: tracked",
		"id", proc.ID[:8],
		"kind", proc.Kind,
		"source", proc.Source,
		"identity", truncID(proc.Identity),
	)

	return proc
}

// Remove unregisters a process. Called when a foreground process completes.
func (pm *ProcessManager) Remove(id string) {
	pm.mu.Lock()
	proc, ok := pm.processes[id]
	if ok && proc.Kind == ProcessForeground {
		delete(pm.processes, id)
	}
	pm.mu.Unlock()

	if ok {
		slog.Info("procmgr: removed", "id", id[:8], "kind", proc.Kind)
	}
}

// Finish marks a background process as complete and fires the callback.
func (pm *ProcessManager) Finish(id string) {
	pm.mu.Lock()
	proc, ok := pm.processes[id]
	var callback func(*ManagedProcess)
	if ok {
		proc.mu.Lock()
		if proc.Status == ProcessRunning {
			proc.Status = ProcessCompleted
		}
		now := time.Now()
		proc.FinishedAt = &now
		proc.mu.Unlock()
		callback = pm.onComplete
	}
	pm.mu.Unlock()

	if ok && callback != nil {
		// Fire callback outside the lock.
		go callback(proc)
	}

	if ok {
		slog.Info("procmgr: finished",
			"id", id[:8],
			"kind", proc.Kind,
			"status", proc.Status,
			"duration", time.Since(proc.StartedAt).Round(time.Millisecond),
		)
	}
}

// Kill sends SIGTERM to a process, then SIGKILL after 5 seconds.
func (pm *ProcessManager) Kill(id string) {
	pm.mu.RLock()
	proc, ok := pm.processes[id]
	pm.mu.RUnlock()
	if !ok {
		return
	}

	proc.mu.Lock()
	proc.Status = ProcessCancelled
	proc.mu.Unlock()

	slog.Info("procmgr: killing", "id", id[:8])

	// Cancel the context first (this sends SIGKILL via exec.CommandContext).
	if proc.cancel != nil {
		proc.cancel()
		return
	}

	// Manual escalation: SIGTERM → wait → SIGKILL.
	if proc.cmd != nil && proc.cmd.Process != nil {
		_ = proc.cmd.Process.Signal(syscall.SIGTERM)
		go func() {
			time.Sleep(5 * time.Second)
			if proc.cmd.ProcessState == nil || !proc.cmd.ProcessState.Exited() {
				slog.Warn("procmgr: SIGKILL escalation", "id", id[:8])
				_ = proc.cmd.Process.Signal(os.Kill)
			}
		}()
	}
}

// KillBySource cancels all processes from a given source (e.g., when a
// Discord channel is closed or a client session ends).
func (pm *ProcessManager) KillBySource(source string) int {
	pm.mu.RLock()
	var targets []string
	for id, proc := range pm.processes {
		if proc.Source == source && proc.Status == ProcessRunning {
			// Only kill foreground processes. Background processes survive.
			if proc.Kind == ProcessForeground {
				targets = append(targets, id)
			}
		}
	}
	pm.mu.RUnlock()

	for _, id := range targets {
		pm.Kill(id)
	}
	return len(targets)
}

// KillByIdentity cancels all foreground processes for a given NodeID.
// Background processes are NOT killed — they were explicitly requested.
func (pm *ProcessManager) KillByIdentity(identity string) int {
	pm.mu.RLock()
	var targets []string
	for id, proc := range pm.processes {
		if proc.Identity == identity && proc.Status == ProcessRunning && proc.Kind == ProcessForeground {
			targets = append(targets, id)
		}
	}
	pm.mu.RUnlock()

	for _, id := range targets {
		pm.Kill(id)
	}
	return len(targets)
}

// CanSpawn checks whether a new process is allowed under the concurrency limits.
func (pm *ProcessManager) CanSpawn(identity string) error {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	running := 0
	identityRunning := 0
	for _, proc := range pm.processes {
		if proc.Status == ProcessRunning {
			running++
			if proc.Identity == identity {
				identityRunning++
			}
		}
	}

	if running >= pm.maxGlobal {
		return fmt.Errorf("global process limit reached (%d/%d)", running, pm.maxGlobal)
	}
	if identity != "" && identityRunning >= pm.maxPerIdentity {
		return fmt.Errorf("per-identity process limit reached (%d/%d) for %s",
			identityRunning, pm.maxPerIdentity, truncID(identity))
	}
	return nil
}

// ── Observability ───────────────────────────────────────────────────────────

// ProcessSummary is a JSON-friendly snapshot of a managed process.
type ProcessSummary struct {
	ID              string  `json:"id"`
	Kind            string  `json:"kind"`
	Status          string  `json:"status"`
	Source          string  `json:"source"`
	Identity        string  `json:"identity,omitempty"`
	StartedAt       string  `json:"started_at"`
	Duration        string  `json:"duration"`
	CallbackChannel string  `json:"callback_channel,omitempty"`
	Error           string  `json:"error,omitempty"`
}

// List returns a snapshot of all tracked processes.
func (pm *ProcessManager) List() []ProcessSummary {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	result := make([]ProcessSummary, 0, len(pm.processes))
	for _, proc := range pm.processes {
		duration := time.Since(proc.StartedAt)
		if proc.FinishedAt != nil {
			duration = proc.FinishedAt.Sub(proc.StartedAt)
		}
		result = append(result, ProcessSummary{
			ID:              proc.ID[:8],
			Kind:            proc.Kind.String(),
			Status:          proc.Status.String(),
			Source:          proc.Source,
			Identity:        truncID(proc.Identity),
			StartedAt:       proc.StartedAt.Format(time.RFC3339),
			Duration:        duration.Round(time.Millisecond).String(),
			CallbackChannel: proc.CallbackChannel,
			Error:           proc.Error,
		})
	}
	return result
}

// Stats returns aggregate counts.
type ProcessStats struct {
	Total      int            `json:"total"`
	Running    int            `json:"running"`
	Completed  int            `json:"completed"`
	Failed     int            `json:"failed"`
	Cancelled  int            `json:"cancelled"`
	ByKind     map[string]int `json:"by_kind"`
	BySource   map[string]int `json:"by_source"`
}

func (pm *ProcessManager) Stats() ProcessStats {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	stats := ProcessStats{
		Total:    len(pm.processes),
		ByKind:   make(map[string]int),
		BySource: make(map[string]int),
	}
	for _, proc := range pm.processes {
		switch proc.Status {
		case ProcessRunning:
			stats.Running++
		case ProcessCompleted:
			stats.Completed++
		case ProcessFailed:
			stats.Failed++
		case ProcessCancelled:
			stats.Cancelled++
		}
		stats.ByKind[proc.Kind.String()]++
		if proc.Source != "" {
			stats.BySource[proc.Source]++
		}
	}
	return stats
}

// ── Shutdown ────────────────────────────────────────────────────────────────

// Shutdown gracefully terminates all running processes.
// Sends SIGTERM to all, waits up to timeout, then SIGKILL.
func (pm *ProcessManager) Shutdown(timeout time.Duration) {
	pm.mu.RLock()
	var running []string
	for id, proc := range pm.processes {
		if proc.Status == ProcessRunning {
			running = append(running, id)
		}
	}
	pm.mu.RUnlock()

	if len(running) == 0 {
		return
	}

	slog.Info("procmgr: shutting down", "running", len(running))

	// SIGTERM all running processes.
	for _, id := range running {
		pm.Kill(id)
	}

	// Wait for processes to exit, up to timeout.
	deadline := time.After(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			slog.Warn("procmgr: shutdown timeout, some processes may be orphaned")
			return
		case <-ticker.C:
			pm.mu.RLock()
			stillRunning := 0
			for _, proc := range pm.processes {
				if proc.Status == ProcessRunning {
					stillRunning++
				}
			}
			pm.mu.RUnlock()
			if stillRunning == 0 {
				slog.Info("procmgr: all processes terminated")
				return
			}
		}
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func truncID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

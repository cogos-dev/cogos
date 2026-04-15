// supervisor.go — Process supervisor for Python inference subprocesses.

package modality

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SupervisorConfig configures a managed subprocess.
type SupervisorConfig struct {
	Name           string
	Command        string
	Args           []string
	MaxRestarts    int           // default 5
	HealthInterval time.Duration // default 10s
	StderrLogDir   string        // default ".cog/run/modality/{name}"
}

// ManagedModule tracks a supervised subprocess.
type ManagedModule struct {
	Name, Command string
	Args          []string
	Conn          *SubprocessConn
	Status        ModuleStatus
	PID           int
	StartedAt     time.Time
	Restarts      int
	MaxRestarts   int
	LastError     string
	healthInt     time.Duration
	logDir        string
	cancel        context.CancelFunc
}

// ProcessSupervisor manages subprocess lifecycles with health monitoring.
type ProcessSupervisor struct {
	mu        sync.RWMutex
	processes map[string]*ManagedModule
	rootDir   string
}

// NewProcessSupervisor creates a new supervisor rooted at rootDir.
func NewProcessSupervisor(rootDir string) *ProcessSupervisor {
	return &ProcessSupervisor{processes: make(map[string]*ManagedModule), rootDir: rootDir}
}

// Register registers a subprocess configuration.
func (s *ProcessSupervisor) Register(cfg *SupervisorConfig) error {
	if cfg.Name == "" || cfg.Command == "" {
		return fmt.Errorf("supervisor: name and command are required")
	}
	if cfg.MaxRestarts <= 0 {
		cfg.MaxRestarts = 5
	}
	if cfg.HealthInterval <= 0 {
		cfg.HealthInterval = 10 * time.Second
	}
	logDir := cfg.StderrLogDir
	if logDir == "" {
		logDir = filepath.Join(s.rootDir, ".cog", "run", "modality", cfg.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.processes[cfg.Name]; ok {
		return fmt.Errorf("supervisor: module %q already registered", cfg.Name)
	}
	s.processes[cfg.Name] = &ManagedModule{
		Name: cfg.Name, Command: cfg.Command, Args: cfg.Args,
		Status: StatusStopped, MaxRestarts: cfg.MaxRestarts,
		healthInt: cfg.HealthInterval, logDir: logDir,
	}
	return nil
}

// Start spawns a subprocess, waits for its ready signal, and starts health monitoring.
func (s *ProcessSupervisor) Start(ctx context.Context, name string) error {
	s.mu.Lock()
	m, ok := s.processes[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: module %q not registered", name)
	}
	if m.Status == StatusHealthy || m.Status == StatusStarting {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: module %q already running", name)
	}
	m.Status = StatusStarting
	s.mu.Unlock()
	if err := os.MkdirAll(m.logDir, 0755); err != nil {
		s.setStatus(name, StatusCrashed, err.Error())
		return err
	}
	conn, err := NewSubprocessConn(name, m.Command, filepath.Join(m.logDir, "stderr.log"), m.Args...)
	if err != nil {
		s.setStatus(name, StatusCrashed, err.Error())
		return fmt.Errorf("supervisor: start %s: %w", name, err)
	}
	readyCh := make(chan error, 1)
	go func() {
		msg, err := conn.Receive()
		if err != nil {
			readyCh <- err
		} else if msg.Event != "ready" && msg.Status != "ready" {
			readyCh <- fmt.Errorf("supervisor: %s sent %q instead of ready", name, msg.Event)
		} else {
			readyCh <- nil
		}
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			_ = conn.Close()
			s.setStatus(name, StatusCrashed, err.Error())
			return err
		}
	case <-time.After(30 * time.Second):
		_ = conn.Close()
		s.setStatus(name, StatusCrashed, "ready timeout")
		return fmt.Errorf("supervisor: %s did not become ready within 30s", name)
	case <-ctx.Done():
		_ = conn.Close()
		s.setStatus(name, StatusStopped, "cancelled")
		return ctx.Err()
	}
	hctx, hcancel := context.WithCancel(context.Background())
	s.mu.Lock()
	m.Conn, m.PID = conn, conn.PID()
	m.StartedAt, m.Status, m.LastError, m.cancel = time.Now(), StatusHealthy, "", hcancel
	s.mu.Unlock()
	go s.healthLoop(hctx, name, m.healthInt)
	return nil
}

// Stop gracefully stops a managed subprocess.
func (s *ProcessSupervisor) Stop(_ context.Context, name string) error {
	s.mu.Lock()
	m, ok := s.processes[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("supervisor: module %q not registered", name)
	}
	conn, cancel := m.Conn, m.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var closeErr error
	if conn != nil {
		closeErr = conn.Close()
	}
	s.mu.Lock()
	m.Conn, m.PID, m.Status, m.cancel = nil, 0, StatusStopped, nil
	s.mu.Unlock()
	return closeErr
}

// Restart stops and restarts a managed subprocess.
func (s *ProcessSupervisor) Restart(ctx context.Context, name string) error {
	_ = s.Stop(ctx, name)
	return s.Start(ctx, name)
}

// ModuleStatus returns the current state of a named module.
func (s *ProcessSupervisor) ModuleStatus(name string) (*ModuleState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.processes[name]
	if !ok {
		return nil, fmt.Errorf("supervisor: module %q not registered", name)
	}
	return modState(m), nil
}

// StatusAll returns the state of all managed modules.
func (s *ProcessSupervisor) StatusAll() map[string]*ModuleState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*ModuleState, len(s.processes))
	for k, m := range s.processes {
		out[k] = modState(m)
	}
	return out
}

// Conn returns the SubprocessConn for a named module, or nil.
func (s *ProcessSupervisor) Conn(name string) *SubprocessConn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.processes[name]; ok {
		return m.Conn
	}
	return nil
}

func (s *ProcessSupervisor) healthLoop(ctx context.Context, name string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.mu.RLock()
		m, ok := s.processes[name]
		if !ok || m.Conn == nil {
			s.mu.RUnlock()
			return
		}
		conn := m.Conn
		s.mu.RUnlock()
		if _, err := conn.HealthCheck(2 * time.Second); err == nil {
			if failures > 0 {
				failures = 0
				s.setStatus(name, StatusHealthy, "")
			}
			continue
		} else {
			failures++
			s.mu.Lock()
			m.LastError = err.Error()
			s.mu.Unlock()
		}
		if failures == 2 {
			s.setStatus(name, StatusDegraded, "health check failing")
		}
		if failures >= 3 {
			s.mu.RLock()
			restarts, maxR := m.Restarts, m.MaxRestarts
			s.mu.RUnlock()
			if restarts >= maxR {
				s.setStatus(name, StatusDegraded, "max restarts exceeded")
				return
			}
			backoff := time.Second << uint(restarts) // 1s, 2s, 4s, 8s ...
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			s.mu.Lock()
			m.Restarts++
			s.mu.Unlock()
			_ = s.Stop(context.Background(), name)
			if err := s.Start(context.Background(), name); err != nil {
				s.setStatus(name, StatusCrashed, err.Error())
			}
			return // Start spawns a new health loop.
		}
	}
}

func (s *ProcessSupervisor) setStatus(name string, status ModuleStatus, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.processes[name]; ok {
		m.Status = status
		if lastErr != "" {
			m.LastError = lastErr
		}
	}
}

func modState(m *ManagedModule) *ModuleState {
	var up time.Duration
	if !m.StartedAt.IsZero() && m.Status == StatusHealthy {
		up = time.Since(m.StartedAt)
	}
	return &ModuleState{Status: m.Status, PID: m.PID, Uptime: up, LastError: m.LastError}
}

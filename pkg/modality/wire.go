// wire.go — Wire protocol client for Go-Python subprocess communication.
//
// Implements the D2 wire protocol: JSON-lines over stdin/stdout pipes.
// Each SubprocessConn manages one Python inference subprocess (TTS, VAD, STT).
// Go kernel serializes per connection; parallelism via multiple instances.

package modality

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// MaxWireLineSize is the scanner buffer limit (1 MB) for base64 audio chunks.
const MaxWireLineSize = 1024 * 1024

// WireMessage is the JSON-lines envelope for all subprocess communication.
// Fields are a flat union keyed by Type.
type WireMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "request", "response", "error", "command", "event"
	Ts   string `json:"ts"`
	// Request
	Module    string         `json:"module,omitempty"`
	Operation string         `json:"op,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	// Response
	Result map[string]any `json:"result,omitempty"`
	// Streaming (reserved)
	Chunk int  `json:"chunk,omitempty"`
	Done  bool `json:"done,omitempty"`
	// Command
	Command string `json:"command,omitempty"`
	// Event
	Event  string `json:"event,omitempty"`
	Status string `json:"status,omitempty"`
	// Error
	Error       string `json:"error,omitempty"`
	ErrorType   string `json:"error_type,omitempty"`
	Recoverable bool   `json:"recoverable,omitempty"`
}

// SubprocessConn manages a single subprocess connected via stdin/stdout JSON-lines.
type SubprocessConn struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	pid    int
	module string // "tts", "vad", "stt"
	nextID uint64 // atomic counter for request IDs
}

// NewSubprocessConn spawns a child process and wires stdin/stdout pipes.
// Stderr is appended to stderrLogPath (pass "" to discard).
func NewSubprocessConn(module, command, stderrLogPath string, args ...string) (*SubprocessConn, error) {
	cmd := exec.Command(command, args...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("wire: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("wire: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("wire: stderr pipe: %w", err)
	}

	if stderrLogPath != "" {
		go drainStderr(stderrPipe, stderrLogPath)
	} else {
		go func() { _, _ = io.Copy(io.Discard, stderrPipe) }()
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("wire: start %s: %w", module, err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, MaxWireLineSize), MaxWireLineSize)

	return &SubprocessConn{
		cmd:    cmd,
		stdin:  stdinPipe,
		stdout: scanner,
		pid:    cmd.Process.Pid,
		module: module,
	}, nil
}

// PID returns the OS process ID of the subprocess.
func (c *SubprocessConn) PID() int { return c.pid }

// Module returns the module name (e.g. "tts", "vad", "stt").
func (c *SubprocessConn) Module() string { return c.module }

// Send writes a single WireMessage as a JSON line to subprocess stdin.
func (c *SubprocessConn) Send(msg *WireMessage) error {
	if msg.Ts == "" {
		msg.Ts = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("wire: marshal: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	data = append(data, '\n')
	_, err = c.stdin.Write(data)
	if err != nil {
		return fmt.Errorf("wire: write to %s (pid %d): %w", c.module, c.pid, err)
	}
	return nil
}

// Receive reads one JSON line from subprocess stdout and decodes it.
func (c *SubprocessConn) Receive() (*WireMessage, error) {
	if !c.stdout.Scan() {
		if err := c.stdout.Err(); err != nil {
			return nil, fmt.Errorf("wire: read from %s (pid %d): %w", c.module, c.pid, err)
		}
		return nil, fmt.Errorf("wire: %s (pid %d): stdout closed", c.module, c.pid)
	}
	var msg WireMessage
	if err := json.Unmarshal(c.stdout.Bytes(), &msg); err != nil {
		return nil, fmt.Errorf("wire: decode from %s (pid %d): %w", c.module, c.pid, err)
	}
	return &msg, nil
}

// NextRequestID generates a unique request ID for this connection.
func (c *SubprocessConn) NextRequestID() string {
	n := atomic.AddUint64(&c.nextID, 1)
	return fmt.Sprintf("%s-%d", c.module, n)
}

// Request sends a request and blocks until the matching response arrives.
// Intermediate events are skipped. Error-type responses become Go errors.
func (c *SubprocessConn) Request(module, operation string, data map[string]any) (*WireMessage, error) {
	id := c.NextRequestID()
	if err := c.Send(&WireMessage{
		ID: id, Type: "request",
		Module: module, Operation: operation, Data: data,
	}); err != nil {
		return nil, err
	}
	for {
		msg, err := c.Receive()
		if err != nil {
			return nil, err
		}
		if msg.Type == "event" || msg.ID != id {
			continue
		}
		if msg.Type == "error" {
			return msg, fmt.Errorf("wire: %s error (%s): %s", c.module, msg.ErrorType, msg.Error)
		}
		return msg, nil
	}
}

// SendCommand sends a command message (e.g. "shutdown", "config").
func (c *SubprocessConn) SendCommand(command string) error {
	return c.Send(&WireMessage{
		ID: c.NextRequestID(), Type: "command", Command: command,
	})
}

// HealthCheck sends a health command and waits for the response within timeout.
func (c *SubprocessConn) HealthCheck(timeout time.Duration) (*WireMessage, error) {
	id := c.NextRequestID()
	if err := c.Send(&WireMessage{
		ID: id, Type: "command", Command: "health",
	}); err != nil {
		return nil, err
	}

	type result struct {
		msg *WireMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			resp, err := c.Receive()
			if err != nil {
				ch <- result{nil, err}
				return
			}
			if resp.ID == id && (resp.Type == "event" || resp.Type == "response") {
				ch <- result{resp, nil}
				return
			}
		}
	}()

	select {
	case r := <-ch:
		return r.msg, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("wire: health timeout for %s (pid %d) after %v", c.module, c.pid, timeout)
	}
}

// Close gracefully shuts down the subprocess. Escalation sequence:
// shutdown command -> 5s wait -> SIGTERM -> 2s wait -> SIGKILL.
func (c *SubprocessConn) Close() error {
	_ = c.SendCommand("shutdown")

	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
	}

	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(os.Interrupt)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
	}

	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return <-done
}

// drainStderr copies subprocess stderr to a log file.
func drainStderr(r io.Reader, path string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = io.Copy(f, r)
}

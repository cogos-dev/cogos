// provider_codex.go — CodexProvider
//
// Implements Provider by spawning `codex exec` subprocesses (OpenAI Codex CLI).
// Parses the NDJSON event stream (--json flag) and extracts agent_message items.
//
// Authentication: uses the host's ChatGPT Pro subscription via codex CLI auth.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// CodexProvider implements Provider by spawning codex exec processes.
type CodexProvider struct {
	name    string
	model   string // "gpt-5.4", "gpt-5.3-codex-spark", etc.
	effort  string // "xhigh", "high", "medium", "low"
	sandbox string // "read-only", "workspace-write", "danger-full-access"
	timeout time.Duration
	binary  string // path to codex binary (default: "codex")
	workDir string // working directory for codex exec
}

// NewCodexProvider creates a CodexProvider from a ProviderConfig.
func NewCodexProvider(name string, cfg ProviderConfig) *CodexProvider {
	model := cfg.Model
	if model == "" {
		model = "gpt-5.4"
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	binary := "codex"
	if cfg.Endpoint != "" {
		binary = cfg.Endpoint
	}

	var effort, sandbox, workDir string
	if cfg.Options != nil {
		if e, ok := cfg.Options["effort"].(string); ok {
			effort = e
		}
		if s, ok := cfg.Options["sandbox"].(string); ok {
			sandbox = s
		}
		if d, ok := cfg.Options["work_dir"].(string); ok {
			workDir = d
		}
	}
	if effort == "" {
		effort = "high"
	}
	if sandbox == "" {
		sandbox = "read-only"
	}

	return &CodexProvider{
		name:    name,
		model:   model,
		effort:  effort,
		sandbox: sandbox,
		timeout: timeout,
		binary:  binary,
		workDir: workDir,
	}
}

func (p *CodexProvider) Name() string { return p.name }

func (p *CodexProvider) Available(ctx context.Context) bool {
	path, err := exec.LookPath(p.binary)
	return err == nil && path != ""
}

func (p *CodexProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Capabilities: []Capability{
			CapStreaming,
			CapToolUse,
		},
		MaxContextTokens:   200_000,
		MaxOutputTokens:    32_000,
		ModelsAvailable:    []string{"gpt-5.4", "gpt-5.3-codex-spark", "gpt-5.3-codex"},
		IsLocal:            true, // runs as local process
		AgenticHarness:     true,
		CostPerInputToken:  0, // Pro sub, no per-token cost
		CostPerOutputToken: 0,
	}
}

func (p *CodexProvider) Ping(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, p.binary, "--version")
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("codex binary not available: %w", err)
	}
	return time.Since(start), nil
}

// buildArgs constructs codex exec arguments.
func (p *CodexProvider) buildArgs(req *CompletionRequest) []string {
	args := []string{"exec"}

	model := p.model
	if req.ModelOverride != "" {
		model = req.ModelOverride
	}
	args = append(args, "-m", model)
	args = append(args, "--config", fmt.Sprintf("model_reasoning_effort=%q", p.effort))
	args = append(args, "--sandbox", p.sandbox)
	args = append(args, "--full-auto")
	args = append(args, "--skip-git-repo-check")
	args = append(args, "--json")

	return args
}

// buildPrompt renders the full substrate packet into a single prompt body.
func (p *CodexProvider) buildPrompt(req *CompletionRequest) string {
	var sb strings.Builder

	if strings.TrimSpace(req.SystemPrompt) != "" {
		sb.WriteString("## Context\n\n")
		sb.WriteString(strings.TrimSpace(req.SystemPrompt))
		sb.WriteString("\n\n---")
	}

	for _, m := range req.Messages {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		switch m.Role {
		case "assistant":
			sb.WriteString("## Assistant\n\n")
		case "system":
			sb.WriteString("## System\n\n")
		case "tool":
			sb.WriteString("## Tool\n\n")
		default:
			sb.WriteString("## User\n\n")
		}
		sb.WriteString(m.Content)
	}

	return strings.TrimSpace(sb.String())
}

// Complete sends a prompt and waits for the full response.
func (p *CodexProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()
	prompt := p.buildPrompt(req)
	if prompt == "" {
		return nil, fmt.Errorf("no user message in request")
	}

	args := p.buildArgs(req)
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, p.binary, args...)
	if p.workDir != "" {
		cmd.Dir = p.workDir
	}

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("cancelled: %w", ctx.Err())
		}
		return nil, fmt.Errorf("codex exited with error: %w", err)
	}

	// Parse NDJSON, collect agent_message text.
	var sb strings.Builder
	var usage TokenUsage
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		text, u, _ := p.parseEventLine([]byte(line))
		if text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(text)
		}
		if u != nil {
			usage = *u
		}
	}

	return &CompletionResponse{
		Content:    sb.String(),
		StopReason: "end_turn",
		Usage:      usage,
		ProviderMeta: ProviderMeta{
			Provider: p.name,
			Model:    p.model,
			Latency:  time.Since(start),
		},
	}, nil
}

// Stream spawns a codex exec process and returns incremental chunks.
func (p *CodexProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	prompt := p.buildPrompt(req)
	if prompt == "" {
		return nil, fmt.Errorf("no user message in request")
	}

	args := p.buildArgs(req)
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, p.binary, args...)
	if p.workDir != "" {
		cmd.Dir = p.workDir
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	// Suppress stderr (thinking tokens)
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex: %w", err)
	}

	ch := make(chan StreamChunk, 32)
	start := time.Now()

	go func() {
		defer close(ch)

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			text, usage, done := p.parseEventLine(line)
			if text != "" {
				select {
				case ch <- StreamChunk{Delta: text}:
				case <-ctx.Done():
					if cmd.Process != nil {
						cmd.Process.Kill()
					}
					ch <- StreamChunk{Error: ctx.Err(), Done: true}
					return
				}
			}
			if usage != nil || done {
				final := StreamChunk{
					Done: true,
					ProviderMeta: &ProviderMeta{
						Provider: p.name,
						Model:    p.model,
						Latency:  time.Since(start),
					},
				}
				if usage != nil {
					final.Usage = usage
				}
				select {
				case ch <- final:
				default:
				}
				if done {
					break
				}
			}
		}

		exitErr := cmd.Wait()
		if exitErr != nil && ctx.Err() == nil {
			select {
			case ch <- StreamChunk{Error: fmt.Errorf("codex process exited: %w", exitErr), Done: true}:
			default:
			}
		}
	}()

	return ch, nil
}

// ── NDJSON event parsing ──────────────────────────────────────────────────────

// codexEvent is the top-level NDJSON envelope from codex exec --json.
type codexEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id,omitempty"`
	Item     json.RawMessage `json:"item,omitempty"`
	Usage    *codexUsage     `json:"usage,omitempty"`
}

type codexItem struct {
	ID     string `json:"id"`
	Type   string `json:"type"`   // "agent_message", "command_execution"
	Text   string `json:"text"`   // for agent_message
	Status string `json:"status"` // "completed", "in_progress"
}

type codexUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

// parseEventLine parses a single NDJSON line from codex's stream.
// Returns (text, usage, done).
func (p *CodexProvider) parseEventLine(line []byte) (string, *TokenUsage, bool) {
	var evt codexEvent
	if err := json.Unmarshal(line, &evt); err != nil {
		slog.Debug("codex: unparseable event line", "err", err)
		return "", nil, false
	}

	switch evt.Type {
	case "item.completed":
		var item codexItem
		if err := json.Unmarshal(evt.Item, &item); err != nil {
			return "", nil, false
		}
		if item.Type == "agent_message" && item.Text != "" {
			return item.Text, nil, false
		}
		return "", nil, false

	case "turn.completed":
		if evt.Usage != nil {
			usage := &TokenUsage{
				InputTokens:     evt.Usage.InputTokens,
				OutputTokens:    evt.Usage.OutputTokens,
				CacheReadTokens: evt.Usage.CachedInputTokens,
			}
			return "", usage, true
		}
		return "", nil, true

	default:
		return "", nil, false
	}
}

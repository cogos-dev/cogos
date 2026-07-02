// provider_pi.go — PiProvider
//
// Implements Provider by spawning `pi -p` subprocesses for local agentic
// inference. Pi handles the tool loop (read, bash, edit, write) against local
// models, while the kernel handles context assembly.
//
// This is the local counterpart to ClaudeCodeProvider:
//   - ClaudeCodeProvider: cloud agentic inference (Claude Max via OAuth)
//   - PiProvider: local agentic inference via Pi
//
// The kernel assembles foveated context and injects it via --system-prompt.
// Pi runs the agent loop; the local backend runs the model. The default local
// backend is LM Studio (lmstudio-darkstar, resident gemma-4-26b at
// 127.0.0.1:1234) — see PR #417, which decommissioned Ollama as the default.
//
// Output: parsed from `--mode json` which emits NDJSON AgentSessionEvents.
package engine

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

// defaultLocalPiProvider is the pi `--provider` value used when a PiProvider
// config does not specify one. Repointed from "ollama" to "lmstudio" to match
// the engine's live local backend after PR #417 (Ollama decommissioned).
const defaultLocalPiProvider = "lmstudio"

// defaultLocalModel is the model used when a PiProvider config does not specify
// one. The resident local model is google/gemma-4-26b-a4b via LM Studio
// (lmstudio-darkstar), consistent with defaults/providers.yaml. Repointed from
// defaultOllamaModel per PR #417.
const defaultLocalModel = "google/gemma-4-26b-a4b"

// PiProvider implements Provider by spawning pi CLI processes.
type PiProvider struct {
	name     string
	provider string // lmstudio, openrouter, etc.
	model    string // e.g. google/gemma-4-26b-a4b
	thinking string // off, minimal, low, medium, high, xhigh
	timeout  time.Duration
	piBinary string // path to pi binary (default: "pi")
	tools    string // comma-separated tools (default: "read,bash,edit,write")
	procMgr  *ProcessManager
}

// NewPiProvider creates a PiProvider from a ProviderConfig.
func NewPiProvider(name string, cfg ProviderConfig, procMgr *ProcessManager) *PiProvider {
	model := cfg.Model
	if model == "" {
		model = defaultLocalModel
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 300 * time.Second
	}
	binary := "pi"
	if cfg.Endpoint != "" {
		binary = cfg.Endpoint
	}

	provider := defaultLocalPiProvider
	thinking := "off"
	tools := "read,bash,edit,write"

	if cfg.Options != nil {
		if p, ok := cfg.Options["provider"].(string); ok {
			provider = p
		}
		if t, ok := cfg.Options["thinking"].(string); ok {
			thinking = t
		}
		if t, ok := cfg.Options["tools"].(string); ok {
			tools = t
		}
	}

	return &PiProvider{
		name:     name,
		provider: provider,
		model:    model,
		thinking: thinking,
		timeout:  timeout,
		piBinary: binary,
		tools:    tools,
		procMgr:  procMgr,
	}
}

func (p *PiProvider) Name() string  { return p.name }
func (p *PiProvider) Model() string { return p.model }

func (p *PiProvider) Available(ctx context.Context) bool {
	path, err := exec.LookPath(p.piBinary)
	return err == nil && path != ""
}

func (p *PiProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Capabilities: []Capability{
			CapStreaming,
			CapToolUse,
		},
		MaxContextTokens:   32768,
		MaxOutputTokens:    8192,
		ModelsAvailable:    []string{p.model},
		IsLocal:            true,
		AgenticHarness:     true,
		CostPerInputToken:  0,
		CostPerOutputToken: 0,
	}
}

func (p *PiProvider) Ping(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, p.piBinary, "--help")
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("pi binary not available: %w", err)
	}
	return time.Since(start), nil
}

// Complete sends a prompt and waits for the full response.
func (p *PiProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()

	prompt := p.buildPrompt(req)
	args := p.buildArgs(req)

	cmd := NewProviderCommandContext(ctx, ManagedCommandOpts{EnvPolicy: EnvPolicyProviderChild}, p.piBinary, args...)
	cmd.Stdin = strings.NewReader(prompt)

	proc := p.procMgr.Track(cmd, ManagedProcessOpts{
		Kind:   ProcessForeground,
		Source: req.Metadata.Source,
	})
	defer p.procMgr.Remove(proc.ID)

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("cancelled: %w", ctx.Err())
		}
		return nil, fmt.Errorf("pi exited with error: %w", err)
	}

	// Pi print mode outputs the response text directly to stdout.
	content := strings.TrimSpace(string(out))

	return &CompletionResponse{
		Content:    content,
		StopReason: "end_turn",
		ProviderMeta: ProviderMeta{
			Provider: p.name,
			Model:    p.model,
			Latency:  time.Since(start),
		},
	}, nil
}

// Stream spawns a pi process in JSON mode and returns incremental chunks.
func (p *PiProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	prompt := p.buildPrompt(req)
	args := p.buildArgs(req)
	// Replace --print with --mode json for streaming NDJSON output.
	for i, a := range args {
		if a == "-p" || a == "--print" {
			args[i] = "--mode"
			args = append(args[:i+1], append([]string{"json"}, args[i+1:]...)...)
			break
		}
	}

	cmd := NewProviderCommandContext(ctx, ManagedCommandOpts{EnvPolicy: EnvPolicyProviderChild}, p.piBinary, args...)
	cmd.Stdin = strings.NewReader(prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start pi: %w", err)
	}

	proc := p.procMgr.Track(cmd, ManagedProcessOpts{
		Kind:   ProcessForeground,
		Source: req.Metadata.Source,
	})

	ch := make(chan StreamChunk, 32)
	start := time.Now()

	go func() {
		defer close(ch)
		defer p.procMgr.Remove(proc.ID)

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			chunk, done := p.parseStreamLine(line)
			if chunk != nil {
				select {
				case ch <- *chunk:
				case <-ctx.Done():
					p.procMgr.Kill(proc.ID)
					ch <- StreamChunk{Error: ctx.Err(), Done: true}
					return
				}
			}
			if done {
				break
			}
		}

		exitErr := cmd.Wait()

		finalChunk := StreamChunk{
			Done: true,
			ProviderMeta: &ProviderMeta{
				Provider: p.name,
				Model:    p.model,
				Latency:  time.Since(start),
			},
		}
		if exitErr != nil && ctx.Err() == nil {
			finalChunk.Error = fmt.Errorf("pi process exited: %w", exitErr)
		}

		select {
		case ch <- finalChunk:
		default:
		}
	}()

	return ch, nil
}

// ── Prompt & argument construction ──────────────────────────────────────────

func (p *PiProvider) buildPrompt(req *CompletionRequest) string {
	var history []string
	var lastUserMsg string

	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			if lastUserMsg != "" {
				history = append(history, fmt.Sprintf("[user]: %s", truncateForHistory(lastUserMsg, 200)))
			}
			lastUserMsg = m.Content
		case "assistant":
			content := strings.TrimSpace(m.Content)
			if content != "" {
				history = append(history, fmt.Sprintf("[assistant]: %s", truncateForHistory(content, 200)))
			}
		}
	}

	if lastUserMsg == "" {
		return ""
	}

	if len(history) > 0 {
		var sb strings.Builder
		sb.WriteString("<conversation_history>\n")
		for _, h := range history {
			sb.WriteString(h)
			sb.WriteByte('\n')
		}
		sb.WriteString("</conversation_history>\n\n")
		sb.WriteString(lastUserMsg)
		return sb.String()
	}

	return lastUserMsg
}

func (p *PiProvider) buildArgs(req *CompletionRequest) []string {
	args := []string{"-p"}

	args = append(args, "--provider", p.provider)

	model := p.model
	if req.ModelOverride != "" {
		model = req.ModelOverride
	}
	args = append(args, "--model", model)

	if p.thinking != "" && p.thinking != "off" {
		args = append(args, "--thinking", p.thinking)
	}

	if p.tools != "" {
		args = append(args, "--tools", p.tools)
	}

	// Kernel-assembled context injected as system prompt.
	if req.SystemPrompt != "" {
		args = append(args, "--system-prompt", req.SystemPrompt)
	}

	// No session persistence for kernel-routed requests.
	args = append(args, "--no-session")

	// No extensions — the kernel handles context and tools.
	args = append(args, "--no-extensions")

	return args
}

// ── NDJSON stream parsing ───────────────────────────────────────────────────

// piStreamEvent is a single NDJSON line from pi --mode json.
type piStreamEvent struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message,omitempty"`
	// message_update fields
	AssistantMessageEvent *piAssistantEvent `json:"assistantMessageEvent,omitempty"`
	// agent_end fields
	Messages json.RawMessage `json:"messages,omitempty"`
}

type piAssistantEvent struct {
	Type  string `json:"type"`  // text_delta, tool_call_delta, etc.
	Delta string `json:"delta"` // text content for text_delta
}

func (p *PiProvider) parseStreamLine(line []byte) (*StreamChunk, bool) {
	var evt piStreamEvent
	if err := json.Unmarshal(line, &evt); err != nil {
		slog.Debug("pi: unparseable stream line", "err", err)
		return nil, false
	}

	switch evt.Type {
	case "message_update":
		if evt.AssistantMessageEvent != nil && evt.AssistantMessageEvent.Type == "text_delta" {
			delta := evt.AssistantMessageEvent.Delta
			if delta != "" {
				return &StreamChunk{Delta: delta}, false
			}
		}
		return nil, false

	case "agent_end":
		return &StreamChunk{
			Done: true,
			ProviderMeta: &ProviderMeta{
				Provider: p.name,
				Model:    p.model,
			},
		}, true

	case "session", "agent_start", "turn_start", "turn_end",
		"message_start", "message_end", "queue_update",
		"tool_execution_start", "tool_execution_update", "tool_execution_end":
		return nil, false

	default:
		return nil, false
	}
}

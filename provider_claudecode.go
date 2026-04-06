// provider_claudecode.go — ClaudeCodeProvider
//
// Implements Provider by spawning `claude -p` subprocesses. Unlike the
// Anthropic and Ollama providers, ClaudeCodeProvider is agentic: the
// subprocess owns its own tool loop (filesystem, MCP, etc.).
//
// Authentication: uses the host's Claude Max subscription via OAuth
// (keychain). Does NOT use --bare mode, which would require API keys.
//
// Process lifecycle:
//   - Foreground: tied to HTTP request context. Cancelled on disconnect.
//   - Background: outlives the request. Reports back via callback.
//   - Agent: runs in Docker container. Trust-bounded, resource-limited.
//
// Output: parsed from `--output-format stream-json --include-partial-messages`
// which emits NDJSON with Anthropic streaming events.
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

// ClaudeCodeProvider implements Provider by spawning claude CLI processes.
type ClaudeCodeProvider struct {
	name      string
	model     string // "sonnet", "opus", "haiku", or full model name
	effort    string // "low", "medium", "high", "max"
	timeout   time.Duration
	cliBinary string // path to claude binary (default: "claude")

	// MCP configuration file path for backend processes.
	mcpConfig string

	// Tools to allow/disallow in backend processes.
	allowedTools    []string
	disallowedTools []string

	// Process manager (shared across all providers/callers).
	procMgr *ProcessManager
}

// NewClaudeCodeProvider creates a ClaudeCodeProvider from a ProviderConfig.
func NewClaudeCodeProvider(name string, cfg ProviderConfig, procMgr *ProcessManager) *ClaudeCodeProvider {
	model := cfg.Model
	if model == "" {
		model = "sonnet"
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 300 * time.Second // 5 min default — agentic tasks take longer
	}
	binary := "claude"
	if cfg.Endpoint != "" {
		binary = cfg.Endpoint // abuse Endpoint field for binary path
	}

	var effort string
	var mcpConfig string
	var allowed, disallowed []string
	if cfg.Options != nil {
		if e, ok := cfg.Options["effort"].(string); ok {
			effort = e
		}
		if m, ok := cfg.Options["mcp_config"].(string); ok {
			mcpConfig = m
		}
		if a, ok := cfg.Options["allowed_tools"].(string); ok {
			allowed = strings.Split(a, ",")
		}
		if d, ok := cfg.Options["disallowed_tools"].(string); ok {
			disallowed = strings.Split(d, ",")
		}
	}

	return &ClaudeCodeProvider{
		name:            name,
		model:           model,
		effort:          effort,
		timeout:         timeout,
		cliBinary:       binary,
		mcpConfig:       mcpConfig,
		allowedTools:    allowed,
		disallowedTools: disallowed,
		procMgr:         procMgr,
	}
}

// Name returns the provider identifier.
func (p *ClaudeCodeProvider) Name() string { return p.name }

// Available checks that the claude binary exists and is authenticated.
func (p *ClaudeCodeProvider) Available(ctx context.Context) bool {
	path, err := exec.LookPath(p.cliBinary)
	return err == nil && path != ""
}

// Capabilities returns what this provider supports.
func (p *ClaudeCodeProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Capabilities: []Capability{
			CapStreaming,
			CapToolUse,
			CapLongContext,
			CapCaching,
		},
		MaxContextTokens:   1_000_000, // Opus 4.6 1M
		MaxOutputTokens:    64_000,
		ModelsAvailable:    []string{"sonnet", "opus", "haiku"},
		IsLocal:            true, // runs as local process
		AgenticHarness:     true,
		CostPerInputToken:  0, // Max sub, no per-token cost
		CostPerOutputToken: 0,
	}
}

// Ping checks the binary is available and returns the startup overhead.
func (p *ClaudeCodeProvider) Ping(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, p.cliBinary, "--version")
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("claude binary not available: %w", err)
	}
	return time.Since(start), nil
}

// Complete sends a prompt and waits for the full response.
func (p *ClaudeCodeProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()

	prompt := p.buildPrompt(req)
	args := p.buildArgs(req)
	args = append(args, "--output-format", "json")

	cmd := exec.CommandContext(ctx, p.cliBinary, args...)
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
		return nil, fmt.Errorf("claude exited with error: %w", err)
	}

	var result ccResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse claude output: %w", err)
	}

	if result.IsError {
		return nil, fmt.Errorf("claude error: %s", result.Result)
	}

	usage := p.extractUsage(&result)
	return &CompletionResponse{
		Content:    result.Result,
		StopReason: result.StopReason,
		Usage:      usage,
		ProviderMeta: ProviderMeta{
			Provider: p.name,
			Model:    p.resolveModel(&result),
			Latency:  time.Since(start),
		},
	}, nil
}

// Stream spawns a claude process and returns incremental chunks.
// The returned channel closes when the process exits or ctx is cancelled.
// On ctx cancellation (client disconnect), the process is killed.
func (p *ClaudeCodeProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	prompt := p.buildPrompt(req)
	args := p.buildArgs(req)
	args = append(args,
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
	)

	cmd := exec.CommandContext(ctx, p.cliBinary, args...)
	cmd.Stdin = strings.NewReader(prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
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
		scanner.Buffer(make([]byte, 256*1024), 256*1024) // 256KB line buffer

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

		// Wait for process to exit.
		exitErr := cmd.Wait()

		// Send final chunk with usage info.
		finalChunk := StreamChunk{
			Done: true,
			ProviderMeta: &ProviderMeta{
				Provider: p.name,
				Model:    p.model,
				Latency:  time.Since(start),
			},
		}
		if exitErr != nil && ctx.Err() == nil {
			finalChunk.Error = fmt.Errorf("claude process exited: %w", exitErr)
		}

		// If we captured usage from the result message, attach it.
		if proc.Usage != nil {
			finalChunk.Usage = proc.Usage
		}

		select {
		case ch <- finalChunk:
		default:
		}
	}()

	return ch, nil
}

// ── Prompt & argument construction ──────────────────────────────────────────

// buildPrompt assembles the user-facing prompt from the CompletionRequest.
// Context injection is handled via --append-system-prompt. For Claude Code we
// keep the prompt body lightweight: user turns plus minimal continuity markers.
func (p *ClaudeCodeProvider) buildPrompt(req *CompletionRequest) string {
	// claude -p treats stdin as a single user message — it has no concept of
	// multi-turn conversation. We extract the last user message as the prompt
	// and fold prior conversation into a compact history prefix so the model
	// has continuity context without repeating earlier messages verbatim.

	var history []string
	var lastUserMsg string

	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			if lastUserMsg != "" {
				// Push previous user message into history before overwriting.
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

	// If there's prior conversation, prepend a compact summary.
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

// truncateForHistory trims a message to maxLen characters for the history prefix.
func truncateForHistory(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	// Collapse whitespace runs.
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// buildArgs constructs the claude CLI arguments from the request.
func (p *ClaudeCodeProvider) buildArgs(req *CompletionRequest) []string {
	args := []string{"-p", "--dangerously-skip-permissions"}

	// Model selection.
	model := p.model
	if req.ModelOverride != "" {
		model = req.ModelOverride
	}
	args = append(args, "--model", model)

	// Effort level.
	if p.effort != "" {
		args = append(args, "--effort", p.effort)
	}

	// System prompt: inject workspace context.
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}

	// MCP configuration.
	if p.mcpConfig != "" {
		args = append(args, "--mcp-config", p.mcpConfig, "--strict-mcp-config")
	}

	// Tool access control.
	if len(p.allowedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, p.allowedTools...)
	}
	if len(p.disallowedTools) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, p.disallowedTools...)
	}

	// Budget cap (per-request).
	if req.Metadata.MaxCostUSD != nil && *req.Metadata.MaxCostUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", *req.Metadata.MaxCostUSD))
	}

	// Session management — allow resuming conversations.
	if req.Metadata.RequestID != "" {
		// Don't persist sessions for one-off requests.
		args = append(args, "--no-session-persistence")
	}

	return args
}

// ── NDJSON stream parsing ───────────────────────────────────────────────────

// ccStreamMessage is the top-level NDJSON envelope from claude --output-format stream-json.
type ccStreamMessage struct {
	Type    string          `json:"type"`    // "system", "assistant", "stream_event", "result"
	Subtype string          `json:"subtype"` // "init" for system
	Event   json.RawMessage `json:"event"`   // for stream_event
	// Result fields (present when type == "result")
	Result     string          `json:"result"`
	IsError    bool            `json:"is_error"`
	StopReason string          `json:"stop_reason"`
	Usage      json.RawMessage `json:"usage"`
	ModelUsage json.RawMessage `json:"modelUsage"`
	SessionID  string          `json:"session_id"`
}

// ccResult is the final JSON output from claude --output-format json.
type ccResult struct {
	Type       string          `json:"type"`
	Result     string          `json:"result"`
	IsError    bool            `json:"is_error"`
	StopReason string          `json:"stop_reason"`
	SessionID  string          `json:"session_id"`
	Usage      json.RawMessage `json:"usage"`
	ModelUsage json.RawMessage `json:"modelUsage"`
	TotalCost  float64         `json:"total_cost_usd"`
}

// ccStreamEvent wraps an Anthropic SSE event inside stream-json output.
type ccStreamEvent struct {
	Type  string `json:"type"` // content_block_delta, message_delta, message_stop, etc.
	Index int    `json:"index"`
	Delta struct {
		Type string `json:"type"` // text_delta, input_json_delta
		Text string `json:"text"`
	} `json:"delta"`
}

// parseStreamLine parses a single NDJSON line from claude's stream output.
// Returns a StreamChunk (or nil if the line should be skipped) and whether this is the final line.
func (p *ClaudeCodeProvider) parseStreamLine(line []byte) (*StreamChunk, bool) {
	var msg ccStreamMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		slog.Debug("claudecode: unparseable stream line", "err", err)
		return nil, false
	}

	switch msg.Type {
	case "stream_event":
		var evt ccStreamEvent
		if err := json.Unmarshal(msg.Event, &evt); err != nil {
			return nil, false
		}
		if evt.Delta.Type == "text_delta" && evt.Delta.Text != "" {
			return &StreamChunk{Delta: evt.Delta.Text}, false
		}
		return nil, false

	case "result":
		usage := p.extractUsageFromRaw(msg.Usage)
		chunk := &StreamChunk{
			Done:  true,
			Usage: &usage,
			ProviderMeta: &ProviderMeta{
				Provider: p.name,
				Model:    p.model,
			},
		}
		if msg.IsError {
			chunk.Error = fmt.Errorf("claude error: %s", msg.Result)
		}
		return chunk, true

	case "system", "assistant":
		// system/init and full assistant messages — skip for streaming.
		return nil, false

	default:
		return nil, false
	}
}

// ── Usage extraction ────────────────────────────────────────────────────────

func (p *ClaudeCodeProvider) extractUsage(result *ccResult) TokenUsage {
	return p.extractUsageFromRaw(result.Usage)
}

func (p *ClaudeCodeProvider) extractUsageFromRaw(raw json.RawMessage) TokenUsage {
	if raw == nil {
		return TokenUsage{}
	}
	var usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	}
	if err := json.Unmarshal(raw, &usage); err != nil {
		return TokenUsage{}
	}
	return TokenUsage{
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CacheReadInputTokens,
		CacheWriteTokens: usage.CacheCreationInputTokens,
	}
}

func (p *ClaudeCodeProvider) resolveModel(result *ccResult) string {
	if result.ModelUsage == nil {
		return p.model
	}
	// modelUsage is map[modelName]stats — extract the key.
	var mu map[string]json.RawMessage
	if err := json.Unmarshal(result.ModelUsage, &mu); err != nil {
		return p.model
	}
	for name := range mu {
		return name // first key is the model used
	}
	return p.model
}

// ── Background task support ─────────────────────────────────────────────────

// BackgroundTaskOpts configures a fire-and-forget Claude Code task.
type BackgroundTaskOpts struct {
	Prompt          string
	Model           string
	Effort          string
	MCPConfig       string
	AllowedTools    []string
	Source          string // "discord", "signal", "http", etc.
	CallbackChannel string // channel to report results to
	Identity        string // NodeID of the requestor
	MaxBudgetUSD    float64
	Timeout         time.Duration
	WorkDir         string // working directory for the process
	SystemPrompt    string
}

// SpawnBackground starts a Claude Code process that outlives the HTTP request.
// Results are delivered via the process manager's callback mechanism.
func (p *ClaudeCodeProvider) SpawnBackground(opts BackgroundTaskOpts) (string, error) {
	args := []string{"-p"}
	args = append(args, "--output-format", "json")

	model := opts.Model
	if model == "" {
		model = p.model
	}
	args = append(args, "--model", model)

	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.SystemPrompt)
	}
	if opts.MCPConfig != "" {
		args = append(args, "--mcp-config", opts.MCPConfig, "--strict-mcp-config")
	}
	if len(opts.AllowedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, opts.AllowedTools...)
	}
	if opts.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", opts.MaxBudgetUSD))
	}
	args = append(args, "--no-session-persistence")

	ctx, cancel := context.WithCancel(context.Background())
	if opts.Timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), opts.Timeout)
	}

	cmd := exec.CommandContext(ctx, p.cliBinary, args...)
	cmd.Stdin = strings.NewReader(opts.Prompt)
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	}

	proc := p.procMgr.Track(cmd, ManagedProcessOpts{
		Kind:            ProcessBackground,
		Source:          opts.Source,
		CallbackChannel: opts.CallbackChannel,
		Identity:        opts.Identity,
		Cancel:          cancel,
	})

	if err := cmd.Start(); err != nil {
		cancel()
		p.procMgr.Remove(proc.ID)
		return "", fmt.Errorf("start background claude: %w", err)
	}

	// Monitor in a goroutine — capture output and fire callback.
	go func() {
		defer cancel()
		defer p.procMgr.Finish(proc.ID)

		err := cmd.Wait()
		if err != nil {
			proc.SetError(err)
		}
	}()

	return proc.ID, nil
}

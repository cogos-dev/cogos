// abstract.go — LLM abstract generation for the conversations-observatory ingest path.
//
// AbstractGenerator takes a conversation JSONL fixture (or any text source),
// resolves the appropriate inference tier from the node's CoreInferenceConfig,
// calls the LLM, and returns a ≤200-token abstract capturing:
//   - Topic (what the conversation is about)
//   - Key URIs or identifiers referenced
//   - Decision-shape (what was decided, built, or deferred)
//
// The abstract is written into cogdoc frontmatter alongside the JSONL source.
//
// Integration point with PR1 (core inference contract):
//   - AbstractGenerator accepts an inference.Resolver and uses it to pick the tier.
//   - For TierClaudeCodeProvider tiers: spawns `claude -p` subprocess (same as harness).
//   - For TierKeptWarmLocal / TierWarmWithColdStart tiers: POSTs to Ollama /api/generate.
//   - For TierExternalSelfHosted: POSTs to the tier's endpoint (OpenAI-compat generate).
//
// The tier selection decision is logged so the calling operator can observe which
// tier the default resolver selected at runtime.
package cogdoc_review

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/myrgic/cogos/internal/engine/inference"
)

// abstractSystemPrompt is the instruction sent to the LLM for abstract generation.
// It constrains the output to ≤200 tokens and requires the three required fields.
const abstractSystemPrompt = `You are a precise summarizer for a software engineering substrate (CogOS).
Given a conversation log, produce a concise abstract in this EXACT JSON format:
{
  "topic": "<one sentence describing what the conversation is about>",
  "key_uris": ["<cog://... or github URL or file path if mentioned>"],
  "decision_shape": "<what was decided, built, deferred, or left open — one sentence>"
}

Rules:
- topic: ≤20 words, factual, no filler
- key_uris: only URIs that appear verbatim in the conversation; empty array if none
- decision_shape: ≤25 words; if nothing was decided, say "no decision — <what was explored>"
- Output ONLY the JSON object, no markdown fences, no preamble`

// ConversationMessage is a single turn in a JSONL conversation fixture.
// This matches the format exported by Claude.ai and other chat systems.
type ConversationMessage struct {
	Role    string `json:"role"`    // "user" or "assistant"
	Content string `json:"content"` // message text
}

// ConversationAbstract is the structured output of abstract generation.
type ConversationAbstract struct {
	// Topic is a one-sentence description of the conversation topic.
	Topic string `json:"topic"`

	// KeyURIs is a list of URIs referenced in the conversation.
	// May be empty.
	KeyURIs []string `json:"key_uris"`

	// DecisionShape describes what was decided, built, or deferred.
	DecisionShape string `json:"decision_shape"`

	// SelectedTier is the inference tier that generated this abstract.
	// Populated by AbstractGenerator for observability.
	SelectedTier string `json:"selected_tier,omitempty"`

	// GeneratedAt is the RFC3339 timestamp of generation.
	GeneratedAt string `json:"generated_at,omitempty"`
}

// AbstractGeneratorConfig configures the AbstractGenerator.
type AbstractGeneratorConfig struct {
	// MaxMessages is the maximum number of messages to include in the prompt.
	// Oldest messages are trimmed first. Default: 20.
	MaxMessages int

	// OllamaEndpoint is the Ollama server URL for local-model tiers.
	// Default: http://localhost:11434
	OllamaEndpoint string

	// Timeout is the call timeout for the LLM. Default: 90s (per substrate eval constraints).
	Timeout time.Duration
}

// LLMCallHook is an injectable function for testing. When non-nil, it replaces
// the actual LLM call (claude subprocess or Ollama HTTP) with a test double.
// Signature matches callLLM: (ctx, provider, tierName, prompt) → (rawJSON, error).
type LLMCallHook func(ctx context.Context, provider inference.ProviderLike, tierName, prompt string) (string, error)

// AbstractGenerator generates abstracts from conversation JSONL using the
// N-tier inference resolver from the node's CoreInferenceConfig.
type AbstractGenerator struct {
	resolver *inference.Resolver
	cfg      AbstractGeneratorConfig
	callHook LLMCallHook // nil in production; non-nil in tests
}

// NewAbstractGenerator creates an AbstractGenerator.
//
// resolver is built from the node's CoreInferenceConfig (loaded in PR1).
// Use inference.NewResolver(cfg, providers) to construct it.
func NewAbstractGenerator(resolver *inference.Resolver, cfg AbstractGeneratorConfig) *AbstractGenerator {
	return newAbstractGenerator(resolver, cfg, nil)
}

// NewAbstractGeneratorWithCallHook creates an AbstractGenerator with an injectable
// LLM call hook for testing. The hook replaces the actual claude/Ollama call.
func NewAbstractGeneratorWithCallHook(resolver *inference.Resolver, cfg AbstractGeneratorConfig, hook LLMCallHook) *AbstractGenerator {
	return newAbstractGenerator(resolver, cfg, hook)
}

func newAbstractGenerator(resolver *inference.Resolver, cfg AbstractGeneratorConfig, hook LLMCallHook) *AbstractGenerator {
	if cfg.MaxMessages == 0 {
		cfg.MaxMessages = 20
	}
	if cfg.OllamaEndpoint == "" {
		cfg.OllamaEndpoint = "http://localhost:11434"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 90 * time.Second
	}
	return &AbstractGenerator{
		resolver: resolver,
		cfg:      cfg,
		callHook: hook,
	}
}

// GenerateFromJSONL reads a JSONL file where each line is a ConversationMessage,
// selects the inference tier, calls the LLM, and returns a ConversationAbstract.
//
// The JSONL format: one JSON object per line, each with "role" and "content" fields.
func (g *AbstractGenerator) GenerateFromJSONL(ctx context.Context, jsonlPath string) (*ConversationAbstract, error) {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return nil, fmt.Errorf("open conversation JSONL: %w", err)
	}
	defer f.Close()

	messages, err := parseConversationJSONL(f)
	if err != nil {
		return nil, fmt.Errorf("parse conversation JSONL: %w", err)
	}

	return g.GenerateFromMessages(ctx, messages)
}

// GenerateFromMessages generates an abstract from a slice of ConversationMessages.
func (g *AbstractGenerator) GenerateFromMessages(ctx context.Context, messages []ConversationMessage) (*ConversationAbstract, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("cannot generate abstract: no messages")
	}

	// Resolve the inference tier.
	provider, tierName, err := g.resolver.ResolveForOperation(ctx, "conversation-abstract-generation", "abstract-generation")
	if err != nil {
		return nil, fmt.Errorf("inference resolver: %w", err)
	}

	slog.Info("abstract generator: tier selected",
		"tier", tierName,
		"provider", provider.Name(),
		"model", provider.Model(),
		"message_count", len(messages),
	)

	// Trim to MaxMessages (keep most recent).
	if len(messages) > g.cfg.MaxMessages {
		messages = messages[len(messages)-g.cfg.MaxMessages:]
	}

	// Build the conversation text for the prompt.
	prompt := buildConversationPrompt(messages)

	// Dispatch to the appropriate backend.
	callCtx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	var rawJSON string
	if g.callHook != nil {
		rawJSON, err = g.callHook(callCtx, provider, tierName, prompt)
	} else {
		rawJSON, err = g.callLLM(callCtx, provider, tierName, prompt)
	}
	if err != nil {
		return nil, fmt.Errorf("LLM call (tier=%s): %w", tierName, err)
	}

	abstract, err := parseAbstractJSON(rawJSON)
	if err != nil {
		return nil, fmt.Errorf("parse abstract JSON: %w", err)
	}
	abstract.SelectedTier = tierName
	abstract.GeneratedAt = time.Now().UTC().Format(time.RFC3339)

	return abstract, nil
}

// callLLM dispatches the prompt to the appropriate backend based on the tier.
func (g *AbstractGenerator) callLLM(ctx context.Context, provider inference.ProviderLike, tierName, prompt string) (string, error) {
	// Determine dispatch path by provider name heuristic (same as resolver matching).
	name := strings.ToLower(provider.Name())
	switch {
	case strings.Contains(name, "claude"):
		return callClaudeCode(ctx, provider.Model(), abstractSystemPrompt, prompt)
	case strings.Contains(name, "ollama"):
		return callOllamaGenerate(ctx, g.cfg.OllamaEndpoint, provider.Model(), abstractSystemPrompt, prompt)
	default:
		// External self-hosted: treat as OpenAI-compat endpoint.
		// For now, fall back to claude-code as the universal path.
		// A future iteration can add the vLLM/mlx-lm HTTP path here.
		slog.Warn("abstract generator: unknown provider type, falling back to claude-code path",
			"provider", provider.Name(), "tier", tierName)
		return callClaudeCode(ctx, provider.Model(), abstractSystemPrompt, prompt)
	}
}

// callClaudeCode spawns `claude -p <prompt>` and returns the response text.
// This mirrors the harness/provider_claudecode.go subprocess pattern.
func callClaudeCode(ctx context.Context, model, systemPrompt, userPrompt string) (string, error) {
	args := []string{
		"-p", userPrompt,
		"--output-format", "text",
		"--append-system-prompt", systemPrompt,
		"--dangerously-skip-permissions",
	}
	if model != "" && model != "claude" {
		args = append(args, "--model", model)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Include stderr in error for diagnosability.
		stderrSnip := stderr.String()
		if len(stderrSnip) > 200 {
			stderrSnip = stderrSnip[:200]
		}
		return "", fmt.Errorf("claude subprocess: %w (stderr: %s)", err, stderrSnip)
	}

	return strings.TrimSpace(stdout.String()), nil
}

// ollamaGenerateRequest is the Ollama /api/generate request body.
type ollamaGenerateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	System string `json:"system,omitempty"`
	Stream bool   `json:"stream"`
	Format string `json:"format,omitempty"` // "json" for structured output
}

// ollamaGenerateResponse is a single streaming chunk from Ollama /api/generate.
type ollamaGenerateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// callOllamaGenerate calls Ollama /api/generate with stream:false and returns the response.
func callOllamaGenerate(ctx context.Context, endpoint, model, systemPrompt, userPrompt string) (string, error) {
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}

	body := ollamaGenerateRequest{
		Model:  model,
		Prompt: userPrompt,
		System: systemPrompt,
		Stream: false,
		Format: "json",
	}

	reqBytes, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal ollama request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint+"/api/generate", bytes.NewReader(reqBytes))
	if err != nil {
		return "", fmt.Errorf("build ollama request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama generate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("ollama generate: status %d: %s", resp.StatusCode, string(body))
	}

	var gen ollamaGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&gen); err != nil {
		return "", fmt.Errorf("decode ollama response: %w", err)
	}

	return strings.TrimSpace(gen.Response), nil
}

// ParseConversationJSONLReader reads newline-delimited JSON, each line a ConversationMessage.
// Exported for testing.
func ParseConversationJSONLReader(r io.Reader) ([]ConversationMessage, error) {
	return parseConversationJSONL(r)
}

// ParseAbstractJSONExported parses raw LLM JSON output into a ConversationAbstract.
// Exported for testing.
func ParseAbstractJSONExported(raw string) (*ConversationAbstract, error) {
	return parseAbstractJSON(raw)
}

// BuildConversationPromptExported formats messages into a text prompt for the LLM.
// Exported for testing.
func BuildConversationPromptExported(messages []ConversationMessage) string {
	return buildConversationPrompt(messages)
}

// parseConversationJSONL reads newline-delimited JSON, each line a ConversationMessage.
func parseConversationJSONL(r io.Reader) ([]ConversationMessage, error) {
	var messages []ConversationMessage
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue // skip blank lines and comment lines
		}
		var msg ConversationMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNum, err)
		}
		if msg.Role != "" && msg.Content != "" {
			messages = append(messages, msg)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

// buildConversationPrompt formats messages into a text prompt for the LLM.
func buildConversationPrompt(messages []ConversationMessage) string {
	var sb strings.Builder
	sb.WriteString("Conversation to summarize:\n\n")
	for _, m := range messages {
		role := strings.ToUpper(m.Role)
		content := m.Content
		// Truncate very long messages to keep prompt size reasonable.
		if len(content) > 800 {
			content = content[:800] + "... [truncated]"
		}
		sb.WriteString(fmt.Sprintf("[%s]: %s\n\n", role, content))
	}
	return sb.String()
}

// parseAbstractJSON parses the LLM's JSON output into a ConversationAbstract.
// It is lenient: if the LLM wraps the JSON in markdown fences, those are stripped.
func parseAbstractJSON(raw string) (*ConversationAbstract, error) {
	// Strip markdown code fences if present.
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		// Find the end fence.
		lines := strings.Split(raw, "\n")
		var inner []string
		inFence := false
		for _, line := range lines {
			if strings.HasPrefix(line, "```") {
				inFence = !inFence
				continue
			}
			if inFence || !strings.HasPrefix(line, "```") {
				inner = append(inner, line)
			}
		}
		raw = strings.Join(inner, "\n")
	}

	// Find the JSON object boundaries.
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found in LLM output (len=%d): %.200s", len(raw), raw)
	}
	raw = raw[start : end+1]

	var abstract ConversationAbstract
	if err := json.Unmarshal([]byte(raw), &abstract); err != nil {
		return nil, fmt.Errorf("unmarshal abstract: %w (raw=%.200s)", err, raw)
	}
	if abstract.Topic == "" {
		return nil, fmt.Errorf("abstract missing topic field (raw=%.200s)", raw)
	}
	return &abstract, nil
}

// WriteAbstractToFrontmatter appends the abstract to a cogdoc's YAML frontmatter.
// It reads the file, inserts the abstract fields after the opening ---, and rewrites.
//
// If the file does not have YAML frontmatter (no opening ---), the abstract is
// written as a comment block at the top of the file instead.
func WriteAbstractToFrontmatter(path string, abstract *ConversationAbstract) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read cogdoc: %w", err)
	}

	// Serialize abstract as YAML fields for insertion.
	abstractYAML := fmt.Sprintf("abstract: %q\nkey_uris: %s\ndecision_shape: %q\nabstract_tier: %q\nabstract_generated_at: %q\n",
		abstract.Topic,
		marshalStringSliceYAML(abstract.KeyURIs),
		abstract.DecisionShape,
		abstract.SelectedTier,
		abstract.GeneratedAt,
	)

	s := string(content)
	const fence = "---"

	if strings.HasPrefix(s, fence+"\n") {
		// Insert after the opening ---.
		rest := s[len(fence)+1:]
		newContent := fence + "\n" + abstractYAML + rest
		return os.WriteFile(path, []byte(newContent), 0644)
	}

	// No frontmatter: prepend a comment block.
	header := fmt.Sprintf("<!-- abstract: %s | decision: %s | tier: %s -->\n\n",
		abstract.Topic, abstract.DecisionShape, abstract.SelectedTier)
	return os.WriteFile(path, []byte(header+s), 0644)
}

// marshalStringSliceYAML returns a YAML inline list or "[]".
func marshalStringSliceYAML(ss []string) string {
	if len(ss) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteString("[")
	for i, s := range ss {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(fmt.Sprintf("%q", s))
	}
	b.WriteString("]")
	return b.String()
}

//go:build mcpserver

package engine

import (
	"context"
	"testing"
	"time"
)

func TestValidateToolCallValid(t *testing.T) {
	t.Parallel()

	result := ValidateToolCall(ToolCall{
		Name:      "lookup",
		Arguments: `{"query":"kernel"}`,
	}, []ToolDefinition{testToolDefinition()})

	if !result.Valid {
		t.Fatalf("Valid = false; want true (reason: %s)", result.Reason)
	}
}

func TestValidateToolCallUnknownTool(t *testing.T) {
	t.Parallel()

	result := ValidateToolCall(ToolCall{
		Name:      "missing",
		Arguments: `{"query":"kernel"}`,
	}, []ToolDefinition{testToolDefinition()})

	if result.Valid {
		t.Fatal("Valid = true; want false")
	}
	if result.Reason != `unknown tool "missing"` {
		t.Fatalf("Reason = %q; want unknown tool rejection", result.Reason)
	}
}

func TestValidateToolCallMissingRequiredParam(t *testing.T) {
	t.Parallel()

	result := ValidateToolCall(ToolCall{
		Name:      "lookup",
		Arguments: `{}`,
	}, []ToolDefinition{testToolDefinition()})

	if result.Valid {
		t.Fatal("Valid = true; want false")
	}
	if result.Reason != `missing required parameter "query"` {
		t.Fatalf("Reason = %q; want missing required parameter rejection", result.Reason)
	}
}

func TestValidateToolCallEmbeddedResult(t *testing.T) {
	t.Parallel()

	result := ValidateToolCall(ToolCall{
		Name:      "lookup",
		Arguments: `{"query":"kernel","result":"fabricated"}`,
	}, []ToolDefinition{testToolDefinition()})

	if result.Valid {
		t.Fatal("Valid = true; want false")
	}
	if result.Reason != "embedded result field is not allowed in tool arguments" {
		t.Fatalf("Reason = %q; want embedded result rejection", result.Reason)
	}
}

func TestRunToolLoopRejectsInvalidToolCall(t *testing.T) {
	root := makeWorkspace(t)
	registry := &KernelToolRegistry{
		cfg: &Config{
			WorkspaceRoot:             root,
			CogDir:                    root + "/.cog",
			ToolCallValidationEnabled: true,
		},
		definitions: []ToolDefinition{testToolDefinition()},
		executors: map[string]toolExecutor{
			"lookup": func(context.Context, string) (string, error) {
				t.Fatal("executor should not run for rejected tool call")
				return "", nil
			},
		},
	}

	provider := &scriptedToolLoopProvider{
		name: "validator-provider",
		caps: ProviderCapabilities{
			Capabilities: []Capability{CapToolCallValidation},
			IsLocal:      true,
		},
		responses: []*CompletionResponse{{
			Content:    "retry succeeded",
			StopReason: "end_turn",
			ProviderMeta: ProviderMeta{
				Provider: "validator-provider",
				Model:    "test",
			},
		}},
	}

	req := &CompletionRequest{}
	initial := &CompletionResponse{
		ToolCalls: []ToolCall{{
			ID:        "call-1",
			Name:      "lookup",
			Arguments: `{"query":"kernel","result":"fabricated"}`,
		}},
		StopReason: "tool_use",
		ProviderMeta: ProviderMeta{
			Provider: "validator-provider",
			Model:    "test",
		},
	}

	resp, clientCalls, err := RunToolLoop(context.Background(), provider, req, initial, registry)
	if err != nil {
		t.Fatalf("RunToolLoop: %v", err)
	}
	if len(clientCalls) != 0 {
		t.Fatalf("clientCalls len = %d; want 0", len(clientCalls))
	}
	if resp.Content != "retry succeeded" {
		t.Fatalf("final content = %q; want retry succeeded", resp.Content)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider recalls = %d; want 1", len(provider.requests))
	}
	if len(provider.requests[0].Messages) != 2 {
		t.Fatalf("messages sent on retry = %d; want 2", len(provider.requests[0].Messages))
	}
	if provider.requests[0].Messages[1].Role != "system" {
		t.Fatalf("retry message role = %q; want system", provider.requests[0].Messages[1].Role)
	}
	want := "Tool call rejected: embedded result field is not allowed in tool arguments. Please try again with valid parameters."
	if provider.requests[0].Messages[1].Content != want {
		t.Fatalf("retry message = %q; want %q", provider.requests[0].Messages[1].Content, want)
	}
}

func TestRunToolLoopSkipsValidationForTrustedProviders(t *testing.T) {
	executed := 0
	registry := &KernelToolRegistry{
		cfg: &Config{
			ToolCallValidationEnabled: true,
		},
		definitions: []ToolDefinition{testToolDefinition()},
		executors: map[string]toolExecutor{
			"lookup": func(_ context.Context, arguments string) (string, error) {
				executed++
				return `{"ok":true}`, nil
			},
		},
	}

	provider := &scriptedToolLoopProvider{
		name: "trusted-provider",
		caps: ProviderCapabilities{
			Capabilities: []Capability{CapToolUse, CapToolCallValidation},
			IsLocal:      true,
		},
		responses: []*CompletionResponse{{
			Content:    "tool completed",
			StopReason: "end_turn",
			ProviderMeta: ProviderMeta{
				Provider: "trusted-provider",
				Model:    "test",
			},
		}},
	}

	req := &CompletionRequest{}
	initial := &CompletionResponse{
		ToolCalls: []ToolCall{{
			ID:        "call-1",
			Name:      "lookup",
			Arguments: `{"query":"kernel","result":"fabricated"}`,
		}},
		StopReason: "tool_use",
		ProviderMeta: ProviderMeta{
			Provider: "trusted-provider",
			Model:    "test",
		},
	}

	resp, clientCalls, err := RunToolLoop(context.Background(), provider, req, initial, registry)
	if err != nil {
		t.Fatalf("RunToolLoop: %v", err)
	}
	if len(clientCalls) != 0 {
		t.Fatalf("clientCalls len = %d; want 0", len(clientCalls))
	}
	if executed != 1 {
		t.Fatalf("executor ran %d times; want 1", executed)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider recalls = %d; want 1", len(provider.requests))
	}
	if len(provider.requests[0].Messages) != 2 {
		t.Fatalf("messages sent on retry = %d; want 2", len(provider.requests[0].Messages))
	}
	if provider.requests[0].Messages[1].Role != "tool" {
		t.Fatalf("second message role = %q; want tool", provider.requests[0].Messages[1].Role)
	}
	if resp.Content != "tool completed" {
		t.Fatalf("final content = %q; want tool completed", resp.Content)
	}
}

func testToolDefinition() ToolDefinition {
	return ToolDefinition{
		Name: "lookup",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type": "string",
				},
			},
			"required": []string{"query"},
		},
	}
}

type scriptedToolLoopProvider struct {
	name      string
	caps      ProviderCapabilities
	responses []*CompletionResponse
	requests  []*CompletionRequest
}

func (p *scriptedToolLoopProvider) Complete(_ context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	clone := *req
	clone.Messages = append([]ProviderMessage(nil), req.Messages...)
	p.requests = append(p.requests, &clone)

	if len(p.responses) == 0 {
		return &CompletionResponse{
			StopReason: "end_turn",
			ProviderMeta: ProviderMeta{
				Provider: p.name,
				Model:    "test",
			},
		}, nil
	}

	resp := p.responses[0]
	p.responses = p.responses[1:]
	return resp, nil
}

func (p *scriptedToolLoopProvider) Stream(context.Context, *CompletionRequest) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk)
	close(ch)
	return ch, nil
}

func (p *scriptedToolLoopProvider) Name() string { return p.name }

func (p *scriptedToolLoopProvider) Available(context.Context) bool { return true }

func (p *scriptedToolLoopProvider) Capabilities() ProviderCapabilities { return p.caps }

func (p *scriptedToolLoopProvider) Ping(context.Context) (time.Duration, error) { return 0, nil }

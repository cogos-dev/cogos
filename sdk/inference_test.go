package sdk

import (
	"testing"
	"time"

	"github.com/cogos-dev/cogos/sdk/types"
)

func TestModelAlias(t *testing.T) {
	tests := []struct {
		alias    string
		expected string
	}{
		{"sonnet", "claude-sonnet-4-20250514"},
		{"opus", "claude-opus-4-20250514"},
		{"haiku", "claude-haiku-3-20240307"},
		{"claude-sonnet-4-20250514", "claude-sonnet-4-20250514"}, // Pass through full IDs
		{"unknown-model", "unknown-model"},                       // Pass through unknowns
	}

	for _, tt := range tests {
		t.Run(tt.alias, func(t *testing.T) {
			got := types.ResolveModelAlias(tt.alias)
			if got != tt.expected {
				t.Errorf("ResolveModelAlias(%q) = %q, want %q", tt.alias, got, tt.expected)
			}
		})
	}
}

func TestDefaultInferenceConfig(t *testing.T) {
	config := types.DefaultInferenceConfig()

	if config.DefaultTimeout != 2*time.Minute {
		t.Errorf("DefaultTimeout = %v, want 2m", config.DefaultTimeout)
	}

	if config.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", config.MaxRetries)
	}

	if config.BaseRetryDelay != time.Second {
		t.Errorf("BaseRetryDelay = %v, want 1s", config.BaseRetryDelay)
	}

	if config.ClaudeCommand != "claude" {
		t.Errorf("ClaudeCommand = %q, want %q", config.ClaudeCommand, "claude")
	}
}

func TestInferenceErrorType(t *testing.T) {
	// Just verify the constants exist and are distinct
	errorTypes := []types.InferenceErrorType{
		types.ErrorNone,
		types.ErrorRateLimit,
		types.ErrorContextOverflow,
		types.ErrorAuth,
		types.ErrorTransient,
		types.ErrorFatal,
	}

	seen := make(map[types.InferenceErrorType]bool)
	for _, et := range errorTypes {
		if seen[et] && et != types.ErrorNone {
			t.Errorf("Duplicate error type: %v", et)
		}
		seen[et] = true
	}
}

func TestInferenceRequest(t *testing.T) {
	req := types.InferenceRequest{
		Prompt:       "Test prompt",
		Model:        "sonnet",
		MaxTokens:    1000,
		Temperature:  0.7,
		Context:      []string{"cog://mem/semantic"},
		SystemPrompt: "You are a helpful assistant",
		Stream:       false,
		Origin:       "test",
	}

	if req.Prompt != "Test prompt" {
		t.Error("Prompt not set correctly")
	}

	if req.Model != "sonnet" {
		t.Error("Model not set correctly")
	}

	if len(req.Context) != 1 {
		t.Error("Context not set correctly")
	}
}

func TestInferenceResponse(t *testing.T) {
	resp := types.InferenceResponse{
		ID:           "req-test-123",
		Content:      "Test response content",
		Model:        "claude-sonnet-4-20250514",
		InputTokens:  100,
		OutputTokens: 50,
		StopReason:   "stop",
		Duration:     time.Second,
		Timestamp:    time.Now(),
	}

	if resp.ID != "req-test-123" {
		t.Error("ID not set correctly")
	}

	if resp.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", resp.InputTokens)
	}

	if resp.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", resp.OutputTokens)
	}
}

func TestStreamChunk(t *testing.T) {
	// Test regular chunk
	chunk := types.StreamChunk{
		ID:      "req-123",
		Content: "Hello",
		Done:    false,
		Seq:     1,
	}

	if chunk.Done {
		t.Error("Regular chunk should not be Done")
	}

	// Test final chunk
	finalChunk := types.StreamChunk{
		ID:           "req-123",
		Content:      "",
		Done:         true,
		FinishReason: "stop",
		Seq:          10,
	}

	if !finalChunk.Done {
		t.Error("Final chunk should be Done")
	}

	if finalChunk.FinishReason != "stop" {
		t.Error("Final chunk should have FinishReason")
	}
}

func TestInferenceStatus(t *testing.T) {
	status := types.InferenceStatus{
		Available:       true,
		ClaudeInstalled: true,
		ClaudePath:      "/usr/local/bin/claude",
		ActiveRequests:  2,
		TotalRequests:   100,
		TotalTokens:     50000,
	}

	if !status.Available {
		t.Error("Status should be Available")
	}

	if status.ActiveRequests != 2 {
		t.Errorf("ActiveRequests = %d, want 2", status.ActiveRequests)
	}
}

func TestRequestEntry(t *testing.T) {
	entry := types.RequestEntry{
		ID:      "req-test-123",
		Origin:  "cli",
		Model:   "sonnet",
		Started: time.Now(),
		Status:  "running",
		Prompt:  "Test prompt...",
	}

	if entry.Status != "running" {
		t.Error("Status should be running")
	}

	if entry.Origin != "cli" {
		t.Error("Origin should be cli")
	}
}

func TestInferenceProjectorClassifyError(t *testing.T) {
	proj := &inferenceProjector{}

	tests := []struct {
		errMsg   string
		expected types.InferenceErrorType
	}{
		{"error: 429 rate limit exceeded", types.ErrorRateLimit},
		{"context_length exceeded", types.ErrorContextOverflow},
		{"unauthorized: invalid API key", types.ErrorAuth},
		{"connection timeout", types.ErrorTransient},
		{"some other error", types.ErrorTransient},
	}

	for _, tt := range tests {
		t.Run(tt.errMsg, func(t *testing.T) {
			err := &testError{msg: tt.errMsg}
			got := proj.classifyError(err)
			if got != tt.expected {
				t.Errorf("classifyError(%q) = %v, want %v", tt.errMsg, got, tt.expected)
			}
		})
	}
}

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}

func TestInferenceProjectorGenerateRequestID(t *testing.T) {
	proj := &inferenceProjector{}

	id1 := proj.generateRequestID("cli")
	id2 := proj.generateRequestID("http")

	// IDs should be unique
	if id1 == id2 {
		t.Error("Request IDs should be unique")
	}

	// IDs should contain origin
	if len(id1) < 10 {
		t.Error("Request ID should be reasonably long")
	}
}

func TestInferenceProjectorGetClaudeCommand(t *testing.T) {
	// Default command
	proj := &inferenceProjector{}
	if proj.getClaudeCommand() != "claude" {
		t.Error("Default command should be 'claude'")
	}

	// Custom command
	proj2 := &inferenceProjector{
		config: &types.InferenceConfig{
			ClaudeCommand: "/custom/path/claude",
		},
	}
	if proj2.getClaudeCommand() != "/custom/path/claude" {
		t.Error("Custom command not respected")
	}
}

func TestContextMetricsInResponse(t *testing.T) {
	resp := types.InferenceResponse{
		ID:      "req-123",
		Content: "Response",
		ContextMetrics: &types.ContextMetrics{
			TotalTokens: 1000,
			TierBreakdown: map[string]int{
				"identity": 400,
				"temporal": 300,
				"present":  200,
				"feedback": 100,
			},
			CoherenceScore: 0.95,
		},
	}

	if resp.ContextMetrics == nil {
		t.Fatal("ContextMetrics should not be nil")
	}

	if resp.ContextMetrics.TotalTokens != 1000 {
		t.Errorf("TotalTokens = %d, want 1000", resp.ContextMetrics.TotalTokens)
	}

	if resp.ContextMetrics.CoherenceScore < 0.9 {
		t.Error("CoherenceScore too low")
	}

	total := 0
	for _, tokens := range resp.ContextMetrics.TierBreakdown {
		total += tokens
	}
	if total != 1000 {
		t.Errorf("TierBreakdown sum = %d, want 1000", total)
	}
}

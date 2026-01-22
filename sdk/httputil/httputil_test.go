package httputil

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/cogos-dev/cogos/sdk"
)

// testKernel creates a kernel connected to a temporary workspace.
func testKernel(t *testing.T) (*sdk.Kernel, string) {
	t.Helper()

	tmpDir := t.TempDir()
	cogDir := filepath.Join(tmpDir, ".cog")
	memDir := filepath.Join(cogDir, "mem", "semantic", "insights")

	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create id.cog
	idPath := filepath.Join(cogDir, "id.cog")
	if err := os.WriteFile(idPath, []byte("---\nid: test\ntype: identity\n---\n# Test Workspace"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create state directory for coherence
	stateDir := filepath.Join(cogDir, ".state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a test mem file
	testDoc := `---
type: insight
id: test-insight
title: Test Insight
created: 2026-01-10
---
# Test Insight

This is a test insight for testing.
`
	if err := os.WriteFile(filepath.Join(memDir, "test-insight.cog.md"), []byte(testDoc), 0644); err != nil {
		t.Fatal(err)
	}

	kernel, err := sdk.Connect(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	return kernel, tmpDir
}

func TestServer_HandleResolve(t *testing.T) {
	kernel, _ := testKernel(t)
	defer kernel.Close()

	server := NewServer(kernel)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	tests := []struct {
		name       string
		uri        string
		wantStatus int
	}{
		{
			name:       "resolve identity",
			uri:        "cog://identity",
			wantStatus: http.StatusOK,
		},
		{
			name:       "resolve src constants",
			uri:        "cog://src",
			wantStatus: http.StatusOK,
		},
		{
			name:       "resolve coherence",
			uri:        "cog://coherence",
			wantStatus: http.StatusOK,
		},
		{
			name:       "resolve mem",
			uri:        "cog://mem/semantic/insights/test-insight",
			wantStatus: http.StatusOK,
		},
		{
			name:       "resolve not found",
			uri:        "cog://mem/nonexistent",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "missing uri parameter",
			uri:        "",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := ts.URL + "/resolve"
			if tt.uri != "" {
				url += "?uri=" + tt.uri
			}

			resp, err := http.Get(url)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body = %s", resp.StatusCode, tt.wantStatus, body)
			}
		})
	}
}

func TestServer_HandleMutate(t *testing.T) {
	kernel, tmpDir := testKernel(t)
	defer kernel.Close()

	server := NewServer(kernel)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// Test creating a new mem file
	t.Run("create mem", func(t *testing.T) {
		content := `---
type: insight
id: new-insight
title: New Insight
created: 2026-01-10
---
# New Insight

Created via HTTP.
`
		req := MutateRequest{
			URI:     "cog://mem/semantic/insights/new-insight",
			Op:      "set",
			Content: content,
		}

		body, _ := json.Marshal(req)
		resp, err := http.Post(ts.URL+"/mutate", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want %d; body = %s", resp.StatusCode, http.StatusOK, respBody)
		}

		// Verify file was created
		filePath := filepath.Join(tmpDir, ".cog", "mem", "semantic", "insights", "new-insight.cog.md")
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			t.Error("file was not created")
		}
	})

	// Test invalid operation
	t.Run("invalid op", func(t *testing.T) {
		req := MutateRequest{
			URI:     "cog://mem/semantic/insights/test",
			Op:      "invalid",
			Content: "test",
		}

		body, _ := json.Marshal(req)
		resp, err := http.Post(ts.URL+"/mutate", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
	})

	// Test missing URI
	t.Run("missing uri", func(t *testing.T) {
		req := MutateRequest{
			Op:      "set",
			Content: "test",
		}

		body, _ := json.Marshal(req)
		resp, err := http.Post(ts.URL+"/mutate", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
	})
}

func TestServer_HandleHealth(t *testing.T) {
	kernel, tmpDir := testKernel(t)
	defer kernel.Close()

	server := NewServer(kernel)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}

	if health["status"] != "healthy" {
		t.Errorf("status = %q, want %q", health["status"], "healthy")
	}

	if health["version"] != sdk.Version {
		t.Errorf("version = %q, want %q", health["version"], sdk.Version)
	}

	if health["root"] != tmpDir {
		t.Errorf("root = %q, want %q", health["root"], tmpDir)
	}
}

func TestServer_HandleRoot(t *testing.T) {
	kernel, _ := testKernel(t)
	defer kernel.Close()

	server := NewServer(kernel)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var info map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}

	if info["name"] != "CogOS SDK Server" {
		t.Errorf("name = %q, want %q", info["name"], "CogOS SDK Server")
	}
}

func TestClient_Resolve(t *testing.T) {
	kernel, _ := testKernel(t)
	defer kernel.Close()

	server := NewServer(kernel)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := NewClient(ts.URL)

	t.Run("resolve identity", func(t *testing.T) {
		resource, err := client.Resolve(context.Background(), "cog://identity")
		if err != nil {
			t.Fatal(err)
		}

		if resource.URI != "cog://identity" {
			t.Errorf("URI = %q, want %q", resource.URI, "cog://identity")
		}

		if len(resource.Content) == 0 {
			t.Error("expected content")
		}
	})

	t.Run("resolve not found", func(t *testing.T) {
		_, err := client.Resolve(context.Background(), "cog://mem/nonexistent")
		if err == nil {
			t.Error("expected error")
		}
	})
}

func TestClient_Mutate(t *testing.T) {
	kernel, _ := testKernel(t)
	defer kernel.Close()

	server := NewServer(kernel)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := NewClient(ts.URL)

	content := `---
type: insight
id: client-insight
title: Client Insight
created: 2026-01-10
---
# Client Insight

Created via client.
`
	mutation := sdk.NewSetMutation([]byte(content))

	err := client.Mutate(context.Background(), "cog://mem/semantic/insights/client-insight", mutation)
	if err != nil {
		t.Fatal(err)
	}

	// Verify by resolving
	resource, err := client.Resolve(context.Background(), "cog://mem/semantic/insights/client-insight")
	if err != nil {
		t.Fatal(err)
	}

	if resource.URI != "cog://mem/semantic/insights/client-insight" {
		t.Errorf("URI = %q, want %q", resource.URI, "cog://mem/semantic/insights/client-insight")
	}
}

func TestClient_Health(t *testing.T) {
	kernel, _ := testKernel(t)
	defer kernel.Close()

	server := NewServer(kernel)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := NewClient(ts.URL)

	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if health["status"] != "healthy" {
		t.Errorf("status = %q, want %q", health["status"], "healthy")
	}
}

func TestOpenAIHandler_ChatCompletion(t *testing.T) {
	kernel, _ := testKernel(t)
	defer kernel.Close()

	handler := NewOpenAIHandler(kernel)

	t.Run("missing messages", func(t *testing.T) {
		req := ChatCompletionRequest{
			Model: "sonnet",
		}

		body, _ := json.Marshal(req)
		httpReq := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		httpReq.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, httpReq)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}

		var errResp ErrorResponse
		json.NewDecoder(w.Body).Decode(&errResp)
		if errResp.Error.Type != "invalid_request_error" {
			t.Errorf("error type = %q, want %q", errResp.Error.Type, "invalid_request_error")
		}
	})

	t.Run("streaming not supported", func(t *testing.T) {
		req := ChatCompletionRequest{
			Model: "sonnet",
			Messages: []Message{
				{Role: "user", Content: "Hello"},
			},
			Stream: true,
		}

		body, _ := json.Marshal(req)
		httpReq := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		httpReq.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, httpReq)

		if w.Code != http.StatusNotImplemented {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		httpReq := httptest.NewRequest("GET", "/v1/chat/completions", nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, httpReq)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
	})
}

func TestOpenAIHandler_ConvertToSDKRequest(t *testing.T) {
	kernel, _ := testKernel(t)
	defer kernel.Close()

	handler := NewOpenAIHandler(kernel)

	temp := 0.7
	req := &ChatCompletionRequest{
		Model: "sonnet",
		Messages: []Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "What is 2+2?"},
		},
		MaxTokens:   100,
		Temperature: &temp,
	}

	sdkReq := handler.convertToSDKRequest(req)

	if sdkReq.Prompt != "What is 2+2?" {
		t.Errorf("Prompt = %q, want %q", sdkReq.Prompt, "What is 2+2?")
	}

	if sdkReq.SystemPrompt != "You are a helpful assistant." {
		t.Errorf("SystemPrompt = %q, want %q", sdkReq.SystemPrompt, "You are a helpful assistant.")
	}

	if sdkReq.Model != "sonnet" {
		t.Errorf("Model = %q, want %q", sdkReq.Model, "sonnet")
	}

	if sdkReq.MaxTokens != 100 {
		t.Errorf("MaxTokens = %d, want %d", sdkReq.MaxTokens, 100)
	}

	if sdkReq.Temperature != 0.7 {
		t.Errorf("Temperature = %f, want %f", sdkReq.Temperature, 0.7)
	}
}

func TestGenerateCompletionID(t *testing.T) {
	id1 := generateCompletionID()
	id2 := generateCompletionID()

	if id1 == id2 {
		t.Error("IDs should be unique")
	}

	if len(id1) < 20 {
		t.Errorf("ID too short: %q", id1)
	}

	if id1[:9] != "chatcmpl-" {
		t.Errorf("ID should start with 'chatcmpl-': %q", id1)
	}
}

func TestServer_Shutdown(t *testing.T) {
	kernel, _ := testKernel(t)
	defer kernel.Close()

	server := NewServer(kernel)

	// Start server in background
	go func() {
		server.ListenAndServe(":0") // Use port 0 for random available port
	}()

	// Give it time to start
	time.Sleep(50 * time.Millisecond)

	// Shutdown should work
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown() error = %v", err)
	}
}

func TestNewClientWithHTTP(t *testing.T) {
	customClient := &http.Client{
		Timeout: 30 * time.Second,
	}

	client := NewClientWithHTTP("http://localhost:8080", customClient)

	if client.httpClient != customClient {
		t.Error("expected custom HTTP client to be used")
	}
}

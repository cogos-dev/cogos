package httputil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	sdk "github.com/cogos-dev/cogos/sdk"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Client connects to a remote CogOS SDK server.
//
// The client provides the same interface as a local Kernel,
// but forwards operations to an HTTP server.
//
// Example:
//
//	client := httputil.NewClient("http://localhost:8080")
//	resource, err := client.Resolve(ctx, "cog://mem/semantic/insights")
type Client struct {
	baseURL    string
	httpClient *http.Client
	mu         sync.Mutex
}

// NewClient creates a new HTTP client for the given server URL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// NewClientWithHTTP creates a client with a custom HTTP client.
func NewClientWithHTTP(baseURL string, httpClient *http.Client) *Client {
	return &Client{
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}

// Resolve fetches a resource from the remote server.
func (c *Client) Resolve(ctx context.Context, uri string) (*sdk.Resource, error) {
	reqURL := fmt.Sprintf("%s/resolve?uri=%s", c.baseURL, url.QueryEscape(uri))

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var resource sdk.Resource
	if err := json.NewDecoder(resp.Body).Decode(&resource); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &resource, nil
}

// Mutate sends a mutation to the remote server.
func (c *Client) Mutate(ctx context.Context, uri string, m *sdk.Mutation) error {
	reqURL := fmt.Sprintf("%s/mutate", c.baseURL)

	body := MutateRequest{
		URI:      uri,
		Op:       string(m.Op),
		Content:  string(m.Content),
		Metadata: m.Metadata,
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return c.parseError(resp)
	}

	return nil
}

// Watch subscribes to changes on a URI pattern via WebSocket.
func (c *Client) Watch(ctx context.Context, uriPattern string) (*sdk.Watcher, error) {
	// Convert http(s) to ws(s)
	wsURL := c.baseURL
	if wsURL[:7] == "http://" {
		wsURL = "ws://" + wsURL[7:]
	} else if wsURL[:8] == "https://" {
		wsURL = "wss://" + wsURL[8:]
	}
	wsURL = fmt.Sprintf("%s/ws/watch?uri=%s", wsURL, url.QueryEscape(uriPattern))

	// Create WebSocket connection
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}

	// Create internal context for cancellation
	ctx, cancel := context.WithCancel(ctx)

	// Create event channel
	events := make(chan sdk.WatchEvent, 100)

	// Create watcher
	watcher := &sdk.Watcher{
		URI:    uriPattern,
		Events: events,
	}

	// Start event loop
	go c.watchEventLoop(ctx, cancel, conn, events)

	return watcher, nil
}

// watchEventLoop reads events from WebSocket and forwards to channel.
func (c *Client) watchEventLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, events chan sdk.WatchEvent) {
	defer close(events)
	defer conn.CloseNow()
	defer cancel()

	for {
		var msg map[string]any
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}

		msgType, _ := msg["type"].(string)
		if msgType != "event" {
			continue
		}

		uri, _ := msg["uri"].(string)
		eventType, _ := msg["eventType"].(string)

		var timestamp time.Time
		if ts, ok := msg["timestamp"].(string); ok {
			timestamp, _ = time.Parse(time.RFC3339, ts)
		}

		event := sdk.WatchEvent{
			URI:       uri,
			Type:      sdk.WatchEventType(eventType),
			Timestamp: timestamp,
		}

		// Parse resource if present
		if resData, ok := msg["resource"].(map[string]any); ok {
			resBytes, _ := json.Marshal(resData)
			var resource sdk.Resource
			if json.Unmarshal(resBytes, &resource) == nil {
				event.Resource = &resource
			}
		}

		select {
		case events <- event:
		case <-ctx.Done():
			return
		default:
			// Channel full, drop event
		}
	}
}

// Health checks the server health.
func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	reqURL := fmt.Sprintf("%s/health", c.baseURL)

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return health, nil
}

// ChatCompletion sends an OpenAI-compatible chat completion request.
func (c *Client) ChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	reqURL := fmt.Sprintf("%s/v1/chat/completions", c.baseURL)

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var completion ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &completion, nil
}

// parseError extracts an error message from a response.
func (c *Client) parseError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)

	var errResp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
		return fmt.Errorf("server error (%d): %s", resp.StatusCode, errResp.Error)
	}

	if len(body) > 0 {
		return fmt.Errorf("server error (%d): %s", resp.StatusCode, string(body))
	}

	return fmt.Errorf("server error (%d)", resp.StatusCode)
}

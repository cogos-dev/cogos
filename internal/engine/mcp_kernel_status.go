// mcp_kernel_status.go — `cog://kernel/status` MCP resource (RFC Phase 1).
//
// Exposes local kernel reachability as a declarative MCP resource so a harness
// (or its model) knows whether substrate tools will work THIS turn rather than
// discovering it only when a kernel-dependent call fails on invocation.
//
// Registration: m.server.AddResource(&mcp.Resource{URI: "cog://kernel/status", …}, m.resourceKernelStatus)
// is called from registerResources in mcp_server.go.
//
// Probe mechanics:
//   - Target: GET http://localhost:<cfg.Port>/health with a 2 s timeout.
//   - The Port field in Config defaults to 6931 (ln(2)×10⁴).
//   - Results are cached for kernelStatusCacheTTL (3 s) to avoid per-turn
//     /health spam when multiple tool reads arrive in quick succession.
//   - The cache lives on the MCPServer instance (kernelStatusProber), so each
//     server has an independent cache — critical for both isolation in tests
//     and correct behaviour when multiple sidecar instances run in the same
//     process (e.g. integration test harnesses).
//   - The cache mutex is held for the full probe duration so concurrent readers
//     block on the single in-flight HTTP call rather than issuing duplicates.
//
// Wire shape (reachable):
//
//	{
//	  "reachable":  true,
//	  "endpoint":   "http://localhost:6931",
//	  "version":    "0.13.0",
//	  "identity":   "cog",
//	  "node_id":    "…",
//	  "checked_at": "2026-05-28T22:00:00Z",
//	  "latency_ms": 7
//	}
//
// Wire shape (unreachable):
//
//	{
//	  "reachable":  false,
//	  "endpoint":   "http://localhost:6931",
//	  "checked_at": "2026-05-28T22:00:00Z",
//	  "error":      "connection refused"
//	}
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// kernelStatusCacheTTL is how long a probe result is considered fresh.
	// 3 s is long enough to absorb per-turn resource reads without hammering
	// /health, short enough not to mask a daemon restart.
	kernelStatusCacheTTL = 3 * time.Second

	// kernelStatusProbeTimeout is the per-probe HTTP deadline.
	// 2 s matches the RFC spec and keeps the MCP turn responsive even when
	// the daemon is completely unresponsive.
	kernelStatusProbeTimeout = 2 * time.Second
)

// kernelStatusEntry is the cached probe outcome stored on each MCPServer.
type kernelStatusEntry struct {
	// payload is the JSON-encoded response to return to the MCP client.
	payload   []byte
	expiresAt time.Time
}

// kernelStatusProber holds the per-MCPServer probe cache. It is embedded in
// MCPServer (as kernelProber) so each server instance has its own independent
// TTL cache. This avoids cross-test pollution when multiple MCPServer instances
// exist in the same process (e.g. parallel unit tests).
type kernelStatusProber struct {
	mu    sync.Mutex
	entry *kernelStatusEntry
	// httpClient is overridable in tests; the zero-value builds a real client
	// on first use via client().
	httpClient *http.Client
}

// client returns the HTTP client to use for /health probes.
// Defaults to a client with kernelStatusProbeTimeout.
func (p *kernelStatusProber) client() *http.Client {
	if p.httpClient != nil {
		return p.httpClient
	}
	return &http.Client{Timeout: kernelStatusProbeTimeout}
}

// probe returns a cached JSON payload when still fresh; otherwise fires
// GET <endpoint>/health and caches the result for kernelStatusCacheTTL.
//
// The mutex is held for the full duration of the HTTP call so that concurrent
// readers wait on the single in-flight probe. Latency is bounded by
// kernelStatusProbeTimeout (2 s), keeping waits short even when the daemon
// is unresponsive.
func (p *kernelStatusProber) probe(endpoint string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	if p.entry != nil && now.Before(p.entry.expiresAt) {
		return p.entry.payload, nil
	}

	payload, err := kernelDoProbe(p.client(), endpoint, now)
	if err != nil {
		// json.Marshal error — programming bug, surface immediately.
		return nil, err
	}

	p.entry = &kernelStatusEntry{
		payload:   payload,
		expiresAt: now.Add(kernelStatusCacheTTL),
	}
	return payload, nil
}

// resourceKernelStatus is the MCP resource handler for cog://kernel/status.
// It is registered via server.AddResource in registerResources (mcp_server.go).
func (m *MCPServer) resourceKernelStatus(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	payload, err := m.kernelProber.probe(m.kernelEndpoint())
	if err != nil {
		return nil, fmt.Errorf("kernel status probe: %w", err)
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(payload),
		}},
	}, nil
}

// kernelEndpoint derives the HTTP base URL for the local kernel from cfg.Port.
// Falls back to the default port (6931) when cfg is nil or Port is unset.
func (m *MCPServer) kernelEndpoint() string {
	if m.cfg != nil && m.cfg.Port > 0 {
		return fmt.Sprintf("http://localhost:%d", m.cfg.Port)
	}
	return "http://localhost:6931"
}

// kernelDoProbe fires a single GET <endpoint>/health and marshals the outcome
// into a JSON payload. It does NOT read or write the cache.
func kernelDoProbe(client *http.Client, endpoint string, probeTime time.Time) ([]byte, error) {
	start := time.Now()
	resp, err := client.Get(endpoint + "/health")
	latencyMS := time.Since(start).Milliseconds()
	checkedAt := probeTime.UTC().Format(time.RFC3339)

	if err != nil {
		// Kernel unreachable — connection refused, timeout, DNS failure, etc.
		result := map[string]any{
			"reachable":  false,
			"endpoint":   endpoint,
			"checked_at": checkedAt,
			"error":      kernelStripURLFromError(err.Error()),
		}
		return json.Marshal(result)
	}
	defer resp.Body.Close()

	// Read up to 4 KB of the health response body.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if readErr != nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable) {
		// Unexpected transport error or non-health HTTP status.
		errMsg := fmt.Sprintf("unexpected status %d", resp.StatusCode)
		if readErr != nil {
			errMsg = readErr.Error()
		}
		result := map[string]any{
			"reachable":  false,
			"endpoint":   endpoint,
			"checked_at": checkedAt,
			"error":      errMsg,
		}
		return json.Marshal(result)
	}

	// Parse version/identity/node_id from the health response. Best-effort:
	// if parsing fails we still return reachable:true with empty string fields
	// rather than surfacing a false-negative.
	var healthResp struct {
		Version  string `json:"version"`
		Identity string `json:"identity"`
		NodeID   string `json:"node_id"`
	}
	_ = json.Unmarshal(body, &healthResp)

	result := map[string]any{
		"reachable":  true,
		"endpoint":   endpoint,
		"version":    healthResp.Version,
		"identity":   healthResp.Identity,
		"node_id":    healthResp.NodeID,
		"checked_at": checkedAt,
		"latency_ms": latencyMS,
	}
	return json.Marshal(result)
}

// kernelStripURLFromError removes the embedded URL from Go net/http error
// messages so the error field in the resource payload reads cleanly.
//
// Go wraps dial errors as: `Get "http://localhost:6931/health": connection refused`
// This function returns `connection refused`.
func kernelStripURLFromError(msg string) string {
	const sep = `": `
	for i := 0; i <= len(msg)-len(sep); i++ {
		if msg[i:i+len(sep)] == sep {
			tail := msg[i+len(sep):]
			if tail != "" {
				return tail
			}
		}
	}
	return msg
}

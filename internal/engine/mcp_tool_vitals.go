// mcp_tool_vitals.go — cog_vitals_window (RFC-040 S2): the MCP counterpart
// to GET /v1/vitals (serve_vitals.go). Both surfaces call the exact same
// vitalsretention.Window helper — RFC-040 N2 ships exactly one query shape,
// and this file adds no second one.
package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/myrgic/cogos/internal/providers/vitalsretention"
)

// vitalsWindowInput is the cog_vitals_window input schema.
type vitalsWindowInput struct {
	Metric     string `json:"metric" jsonschema:"Metric name — one of disk_free_bytes, mem_free_bytes, load1, uptime_seconds, providers_healthy, providers_degraded, providers_missing, providers_suspended, anomalies, anomalies_total"`
	Since      string `json:"since" jsonschema:"RFC3339 timestamp or duration shorthand (e.g. '24h', '30m'), subtracted from now"`
	Resolution string `json:"resolution" jsonschema:"One of raw (tick resolution), 5m, or 1h"`
}

// toolVitalsWindow implements cog_vitals_window.
func (m *MCPServer) toolVitalsWindow(ctx context.Context, req *mcp.CallToolRequest, input vitalsWindowInput) (*mcp.CallToolResult, any, error) {
	metric := strings.TrimSpace(input.Metric)
	if metric == "" {
		return fallbackResult("cog_vitals_window: metric is required",
			"GET /v1/vitals?metric=&since=&resolution=")
	}

	sinceRaw := strings.TrimSpace(input.Since)
	if sinceRaw == "" {
		return fallbackResult("cog_vitals_window: since is required",
			"GET /v1/vitals?metric=&since=&resolution=")
	}
	since, err := parseTimeOrDuration(sinceRaw)
	if err != nil {
		return fallbackResult(fmt.Sprintf("cog_vitals_window: bad since: %v", err),
			"use RFC3339 or a duration shorthand like '24h'")
	}

	resolution := strings.TrimSpace(input.Resolution)
	if resolution == "" {
		return fallbackResult("cog_vitals_window: resolution is required (raw, 5m, or 1h)",
			"GET /v1/vitals?metric=&since=&resolution=")
	}

	points, err := vitalsretention.Window(metric, since, resolution)
	if err != nil {
		// Mirrors serve_vitals.go's 400-vs-500 split (cog-review finding on
		// PR #493, commit 4dcbd00, which fixed the HTTP handler but missed
		// this sibling caller of the same Window() function): a bad
		// metric/resolution is the caller's mistake — fallbackResult's
		// "here's a fix / CLI equivalent" framing fits. A genuine storage
		// failure is not something retrying with different arguments would
		// fix, so it goes back as a real MCP-protocol error instead of the
		// same undifferentiated IsError:true content blob.
		if errors.Is(err, vitalsretention.ErrInvalidQuery) {
			return fallbackResult(fmt.Sprintf("cog_vitals_window: %v", err),
				"GET /v1/vitals?metric=&since=&resolution=")
		}
		return nil, nil, fmt.Errorf("cog_vitals_window: %w", err)
	}
	if points == nil {
		points = []vitalsretention.Point{}
	}

	return m.cappedMarshal(map[string]any{
		"metric":     metric,
		"since":      since.UTC(),
		"resolution": resolution,
		"points":     points,
	})
}

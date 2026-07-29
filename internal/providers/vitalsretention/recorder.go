// recorder.go — the bus_kernel_proprio subscriber.
//
// HandleBusEvent is wired onto BusSessionManager.AddEventHandler (see
// internal/providers/all/all.go's engine.WireProviderRuntime closure) and
// therefore runs synchronously, once per autonomic tick, INSIDE
// BusSessionManager.AppendEvent's handler-dispatch loop — after the event is
// durably written to the bus but before AppendEvent returns to its caller
// (the autonomic ticker's emitHealthSnapshot). Per host_vitals.go's existing
// contract for the same tick ("never blocks or panics"), every operation
// here is best-effort: a failed write is logged and recorded for Health(),
// never returned as an error the ticker would have to handle, and never a
// panic.
package vitalsretention

import (
	"time"
)

// metricNames lists every leaf numeric field this recorder extracts from a
// KernelHealthSnapshot payload, matching exactly what RFC-040 S1's /metrics
// exporter exposes (serve_metrics.go) so the two surfaces describe the same
// vocabulary. Host-gauge metrics are extracted only when present in the
// payload (HostVitals fields are `omitempty` pointers — see host_vitals.go —
// so a gauge the platform can't sample is simply absent from a given tick's
// payload, exactly as it's absent from the JSON snapshot and the Prometheus
// exposition).
const (
	metricDiskFreeBytes      = "disk_free_bytes"
	metricMemFreeBytes       = "mem_free_bytes"
	metricLoad1              = "load1"
	metricUptimeSeconds      = "uptime_seconds"
	metricProvidersHealthy   = "providers_healthy"
	metricProvidersDegraded  = "providers_degraded"
	metricProvidersMissing   = "providers_missing"
	metricProvidersSuspended = "providers_suspended"
	metricAnomalies          = "anomalies"
	metricAnomaliesTotal     = "anomalies_total"
)

// AllMetricNames returns every metric name this recorder knows how to
// extract, in a stable order. Window() (window.go) validates every
// caller-supplied metric against exactly this list before it can reach a
// filesystem path — metric is not just documentation here, it is the
// allowlist. Also exposed so the HTTP/MCP surfaces (serve_vitals.go,
// mcp_tool_vitals.go) and their tool-description text stay in sync with
// what the recorder actually writes, instead of drifting from it.
func AllMetricNames() []string {
	return []string{
		metricDiskFreeBytes, metricMemFreeBytes, metricLoad1, metricUptimeSeconds,
		metricProvidersHealthy, metricProvidersDegraded, metricProvidersMissing, metricProvidersSuspended,
		metricAnomalies, metricAnomaliesTotal,
	}
}

// extractMetrics decomposes a decoded kernel.health.snapshot.v1 payload
// (the map[string]any produced by snapshotToPayload's JSON round-trip — see
// internal/engine/autonomic_ticker.go) into a flat metric-name -> value map.
// A key that's absent, non-numeric, or the wrong shape is silently omitted
// rather than recorded as a fabricated zero (mirrors HostVitals' own
// soft-degrade contract).
func extractMetrics(payload map[string]any) map[string]float64 {
	out := make(map[string]float64, len(AllMetricNames()))

	if hv, ok := payload["host_vitals"].(map[string]any); ok {
		putNumeric(out, metricDiskFreeBytes, hv["disk_free_bytes"])
		putNumeric(out, metricMemFreeBytes, hv["mem_free_bytes"])
		putNumeric(out, metricLoad1, hv["load1"])
		putNumeric(out, metricUptimeSeconds, hv["uptime_seconds"])
	}
	if counts, ok := payload["counts"].(map[string]any); ok {
		putNumeric(out, metricProvidersHealthy, counts["healthy"])
		putNumeric(out, metricProvidersDegraded, counts["degraded"])
		putNumeric(out, metricProvidersMissing, counts["missing"])
		putNumeric(out, metricProvidersSuspended, counts["suspended"])
	}
	putNumeric(out, metricAnomalies, payload["anomalies"])
	putNumeric(out, metricAnomaliesTotal, payload["anomalies_total"])

	return out
}

// putNumeric writes v into out[key] when v decodes as a JSON number.
// encoding/json decodes all JSON numbers as float64 into `any`, so this
// covers uint64/int64/float64-sourced fields uniformly.
func putNumeric(out map[string]float64, key string, v any) {
	if f, ok := v.(float64); ok {
		out[key] = f
	}
}

// HandleBusEvent records one bus_kernel_proprio tick. Any event on a
// different bus, or a different event type on this bus, is ignored — this
// recorder is scoped exactly to RFC-040's snapshot event, not a general bus
// archiver (N2/N3: this is a recorder for one known shape, not infrastructure
// for arbitrary future event types).
func (r *Recorder) HandleBusEvent(busID string, block *Block) {
	if block == nil || busID != ProprioBusID || block.Type != ProprioEventType {
		return
	}

	base, err := r.baseDir()
	if err != nil {
		r.recordAppendResult(err)
		return
	}

	ts, err := time.Parse(time.RFC3339Nano, block.Ts)
	if err != nil {
		// Fall back to now() rather than dropping the tick entirely — a
		// malformed timestamp on an otherwise-valid snapshot shouldn't lose
		// the whole tick's data, and "recorded slightly late" is far less
		// surprising than "silently missing."
		warnf("vitals-retention: unparseable event ts %q, using now(): %v", block.Ts, err)
		ts = time.Now().UTC()
	}
	ts = ts.UTC()

	nodeKey := currentNodeKey()
	metrics := extractMetrics(block.Payload)

	var firstErr error
	for name, value := range metrics {
		v := value
		rw := row{Ts: block.Ts, V: v}
		if err := appendRow(base, nodeKey, tierRaw, name, ts, rw); err != nil {
			warnf("vitals-retention: append failed metric=%s: %v", name, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	r.recordAppendResult(firstErr)

	// Compaction rides the same per-tick dispatch (no new loop/daemon — see
	// package doc), throttled internally by maybeCompact so it doesn't
	// actually do file work on every single tick.
	r.maybeCompact(base, nodeKey)
}

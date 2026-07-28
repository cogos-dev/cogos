package engine

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// promSampleLine matches one exposition-format sample: a metric name
// (letters/digits/underscore, not starting with a digit), whitespace, then
// a value that parses as a Prometheus-legal float (int, decimal, or
// scientific notation — strconv.ParseFloat covers all three).
var promSampleLine = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*\s+\S+$`)

// TestRenderPrometheusMetrics_ValidExpositionShape asserts the text
// renderPrometheusMetrics produces parses as valid Prometheus text
// exposition format 0.0.4: every metric block is exactly a # HELP line, a
// # TYPE line, and one sample line, in that order, with the # HELP/# TYPE
// metric name matching the sample's metric name and TYPE one of the
// exposition format's legal values.
func TestRenderPrometheusMetrics_ValidExpositionShape(t *testing.T) {
	disk := uint64(123456789)
	mem := uint64(987654321)
	load := 1.25
	uptime := uint64(3600)
	snap := KernelHealthSnapshot{
		Counts:         HealthCounts{Healthy: 2, Degraded: 1, Missing: 0, Suspended: 0},
		Anomalies:      3,
		AnomaliesTotal: 42,
		HostVitals: HostVitals{
			DiskFreeBytes: &disk,
			MemFreeBytes:  &mem,
			Load1:         &load,
			UptimeSeconds: &uptime,
		},
	}

	out := renderPrometheusMetrics(snap)
	if out == "" {
		t.Fatal("expected non-empty exposition output")
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines)%3 != 0 {
		t.Fatalf("expected a multiple-of-3 line count (HELP/TYPE/sample per metric), got %d lines:\n%s", len(lines), out)
	}

	seenSamples := map[string]string{}
	for i := 0; i < len(lines); i += 3 {
		helpLine, typeLine, sampleLine := lines[i], lines[i+1], lines[i+2]

		if !strings.HasPrefix(helpLine, "# HELP ") {
			t.Fatalf("line %d: expected a # HELP line, got %q", i, helpLine)
		}
		if !strings.HasPrefix(typeLine, "# TYPE ") {
			t.Fatalf("line %d: expected a # TYPE line, got %q", i+1, typeLine)
		}
		if !promSampleLine.MatchString(sampleLine) {
			t.Fatalf("line %d: sample line %q does not match the expected `name value` shape", i+2, sampleLine)
		}

		helpName := strings.Fields(strings.TrimPrefix(helpLine, "# HELP "))[0]
		typeFields := strings.Fields(strings.TrimPrefix(typeLine, "# TYPE "))
		if len(typeFields) != 2 {
			t.Fatalf("line %d: expected `# TYPE <name> <type>`, got %q", i+1, typeLine)
		}
		typeName, typeKind := typeFields[0], typeFields[1]
		sampleFields := strings.Fields(sampleLine)
		sampleName, sampleValue := sampleFields[0], sampleFields[1]

		if helpName != typeName || typeName != sampleName {
			t.Fatalf("metric name mismatch across HELP/TYPE/sample: %q / %q / %q", helpName, typeName, sampleName)
		}
		if typeKind != "gauge" && typeKind != "counter" {
			t.Fatalf("metric %q: TYPE %q is not a legal Prometheus exposition type", sampleName, typeKind)
		}
		if _, err := strconv.ParseFloat(sampleValue, 64); err != nil {
			t.Fatalf("metric %q: sample value %q does not parse as a number: %v", sampleName, sampleValue, err)
		}

		seenSamples[sampleName] = sampleValue
	}

	// Provider counts and anomalies are always present.
	for _, want := range []struct {
		name  string
		value string
	}{
		{"cogos_providers_healthy", "2"},
		{"cogos_providers_degraded", "1"},
		{"cogos_providers_missing", "0"},
		{"cogos_providers_suspended", "0"},
		{"cogos_anomalies", "3"},
		{"cogos_anomalies_total", "42"},
		{"cogos_disk_free_bytes", "123456789"},
		{"cogos_mem_free_bytes", "987654321"},
		{"cogos_load1", "1.25"},
		{"cogos_uptime_seconds", "3600"},
	} {
		got, ok := seenSamples[want.name]
		if !ok {
			t.Errorf("expected metric %q in output, was absent", want.name)
			continue
		}
		if got != want.value {
			t.Errorf("metric %q: got value %q, want %q", want.name, got, want.value)
		}
	}
}

// TestRenderPrometheusMetrics_OmittedGaugeHasNoBlock verifies the S0
// soft-degrade contract carries through to S1: a HostVitals field that
// wasn't sampled (nil pointer) produces no HELP/TYPE/sample block at all —
// never a metric line with a fabricated zero value.
func TestRenderPrometheusMetrics_OmittedGaugeHasNoBlock(t *testing.T) {
	snap := KernelHealthSnapshot{
		Counts:     HealthCounts{Healthy: 1},
		HostVitals: HostVitals{}, // every gauge unsampled
	}

	out := renderPrometheusMetrics(snap)

	for _, absent := range []string{
		"cogos_disk_free_bytes",
		"cogos_mem_free_bytes",
		"cogos_load1",
		"cogos_uptime_seconds",
	} {
		if strings.Contains(out, absent) {
			t.Errorf("expected no %q block when the gauge is unsampled, but found one in:\n%s", absent, out)
		}
	}
	// Provider counts must still render even with zero providers registered.
	if !strings.Contains(out, "cogos_providers_healthy 1") {
		t.Errorf("expected cogos_providers_healthy 1 in output:\n%s", out)
	}
}

// TestHandleMetrics_ContentTypeAndBody exercises the actual HTTP handler
// (not just the pure renderer) end to end: correct Content-Type per the
// Prometheus text exposition format 0.0.4 spec, 200 status, and a body that
// contains the always-present provider-count metrics.
func TestHandleMetrics_ContentTypeAndBody(t *testing.T) {
	s := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()

	s.handleMetrics(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; version=0.0.4" {
		t.Fatalf("expected Content-Type %q, got %q", "text/plain; version=0.0.4", ct)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "# TYPE cogos_providers_healthy gauge") {
		t.Errorf("expected cogos_providers_healthy TYPE line in body:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE cogos_anomalies_total counter") {
		t.Errorf("expected cogos_anomalies_total TYPE line in body:\n%s", body)
	}
}

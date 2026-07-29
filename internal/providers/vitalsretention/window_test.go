package vitalsretention

import (
	"testing"
	"time"
)

func TestWindow_RejectsUnknownResolution(t *testing.T) {
	root := withWorkspace(t, "node-a")
	_ = root
	r := &Recorder{}
	if _, err := r.Window("disk_free_bytes", time.Now(), "15m"); err == nil {
		t.Fatal("expected an error for an unsupported resolution")
	}
}

func TestWindow_RejectsEmptyMetric(t *testing.T) {
	_ = withWorkspace(t, "node-a")
	r := &Recorder{}
	if _, err := r.Window("", time.Now(), tierRaw); err == nil {
		t.Fatal("expected an error for an empty metric")
	}
}

func TestWindow_ReturnsRawPointsSinceFilter(t *testing.T) {
	root := withWorkspace(t, "node-a")
	base := vitalsBaseDir(root)
	metric := "disk_free_bytes"
	day := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	ts1 := day.Add(1 * time.Hour)
	ts2 := day.Add(2 * time.Hour)
	ts3 := day.Add(3 * time.Hour)
	for i, ts := range []time.Time{ts1, ts2, ts3} {
		if err := appendRow(base, "node-a", tierRaw, metric, ts, row{Ts: ts.Format(time.RFC3339Nano), V: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	r := &Recorder{}
	points, err := r.Window(metric, ts2, tierRaw)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("want 2 points (>= ts2), got %d: %+v", len(points), points)
	}
	if !points[0].Ts.Equal(ts2) || !points[1].Ts.Equal(ts3) {
		t.Errorf("unexpected points: %+v", points)
	}
	if points[0].Value != 1 || points[1].Value != 2 {
		t.Errorf("unexpected values: %+v", points)
	}
}

func TestWindow_SpansMultipleDayFiles(t *testing.T) {
	root := withWorkspace(t, "node-a")
	base := vitalsBaseDir(root)
	metric := "disk_free_bytes"

	day1 := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	day3 := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	for i, ts := range []time.Time{day1, day2, day3} {
		if err := appendRow(base, "node-a", tierRaw, metric, ts, row{Ts: ts.Format(time.RFC3339Nano), V: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	r := &Recorder{}
	points, err := r.Window(metric, day1, tierRaw)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("want 3 points spanning 3 day-files, got %d", len(points))
	}
}

func TestWindow_ReadsCompactedTiersWithAggregateFields(t *testing.T) {
	root := withWorkspace(t, "node-a")
	base := vitalsBaseDir(root)
	metric := "disk_free_bytes"
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	if err := writeRows(base, "node-a", tier5m, metric, day, []row{
		{Ts: day.Format(time.RFC3339Nano), V: 15, Min: ptr(10), Max: ptr(20), N: 3},
	}); err != nil {
		t.Fatal(err)
	}

	r := &Recorder{}
	points, err := r.Window(metric, day, tier5m)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("want 1 point, got %d", len(points))
	}
	p := points[0]
	if p.Value != 15 || p.Min == nil || *p.Min != 10 || p.Max == nil || *p.Max != 20 || p.Count != 3 {
		t.Errorf("unexpected point: %+v", p)
	}
}

func TestWindow_MissingDataReturnsEmptyNotError(t *testing.T) {
	_ = withWorkspace(t, "node-a")
	r := &Recorder{}
	points, err := r.Window("disk_free_bytes", time.Now().Add(-24*time.Hour), tierRaw)
	if err != nil {
		t.Fatalf("Window should not error on a metric with no recorded data: %v", err)
	}
	if len(points) != 0 {
		t.Fatalf("want 0 points, got %d", len(points))
	}
}

func TestAllMetricNames_MatchesExtractedKeys(t *testing.T) {
	payload := map[string]any{
		"host_vitals": map[string]any{
			"disk_free_bytes": float64(1),
			"mem_free_bytes":  float64(2),
			"load1":           float64(3),
			"uptime_seconds":  float64(4),
		},
		"counts": map[string]any{
			"healthy":   float64(1),
			"degraded":  float64(0),
			"missing":   float64(0),
			"suspended": float64(0),
		},
		"anomalies":       float64(0),
		"anomalies_total": float64(0),
	}
	metrics := extractMetrics(payload)
	known := map[string]bool{}
	for _, name := range AllMetricNames() {
		known[name] = true
	}
	for name := range metrics {
		if !known[name] {
			t.Errorf("extracted metric %q is not in AllMetricNames()", name)
		}
	}
	if len(metrics) != len(AllMetricNames()) {
		t.Errorf("expected extractMetrics to populate every known metric when the payload is fully populated: got %d want %d", len(metrics), len(AllMetricNames()))
	}
}

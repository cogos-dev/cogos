package vitalsretention

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withWorkspace points the package-level workspace-root/node-key seams at a
// fresh temp dir + fixed node key for the duration of the test, restoring
// prior global state afterward. Tests in this file exercise Recorder methods
// (HandleBusEvent, Health) that go through the global resolveWorkspaceRoot/
// currentNodeKey seams rather than taking base/nodeKey as explicit
// parameters — this helper keeps that global-state juggling in one place.
func withWorkspace(t *testing.T, nodeKey string) (root string) {
	t.Helper()
	root = t.TempDir()

	SetWorkspaceRoot(root)
	SetNodeKeySource(NodeKeyFunc(func() string { return nodeKey }))
	ReloadConfig(root)

	t.Cleanup(func() {
		SetWorkspaceRoot("")
		SetNodeKeySource(nil)
		ReloadConfig(root)
	})
	return root
}

func snapshotBlock(ts time.Time, diskFree, uptime float64, healthy, degraded int, anomalies, anomaliesTotal float64) *Block {
	return &Block{
		BusID: ProprioBusID,
		Type:  ProprioEventType,
		Ts:    ts.UTC().Format(time.RFC3339Nano),
		Payload: map[string]any{
			"host_vitals": map[string]any{
				"disk_free_bytes": diskFree,
				"uptime_seconds":  uptime,
			},
			"counts": map[string]any{
				"healthy":   float64(healthy),
				"degraded":  float64(degraded),
				"missing":   float64(0),
				"suspended": float64(0),
			},
			"anomalies":       anomalies,
			"anomalies_total": anomaliesTotal,
		},
	}
}

func TestHandleBusEvent_AppendsRawRows(t *testing.T) {
	root := withWorkspace(t, "node-a")

	r := &Recorder{}
	ts := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	r.HandleBusEvent(ProprioBusID, snapshotBlock(ts, 123456, 789, 3, 1, 0, 5))

	base := vitalsBaseDir(root)
	path := dayFilePath(base, "node-a", tierRaw, metricDiskFreeBytes, ts)
	rows, err := readDayFile(path)
	if err != nil {
		t.Fatalf("readDayFile: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].V != 123456 {
		t.Errorf("want v=123456, got %v", rows[0].V)
	}
	parsedTs, err := rows[0].parseTs()
	if err != nil {
		t.Fatalf("parseTs: %v", err)
	}
	if !parsedTs.Equal(ts) {
		t.Errorf("want ts=%v, got %v", ts, parsedTs)
	}

	// Every metric present in the payload got its own file.
	for _, metric := range []string{metricUptimeSeconds, metricProvidersHealthy, metricProvidersDegraded, metricAnomalies, metricAnomaliesTotal} {
		p := dayFilePath(base, "node-a", tierRaw, metric, ts)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file for metric %s: %v", metric, err)
		}
	}
}

func TestHandleBusEvent_IgnoresOtherBusesAndTypes(t *testing.T) {
	root := withWorkspace(t, "node-a")
	base := vitalsBaseDir(root)

	r := &Recorder{}
	ts := time.Now().UTC()

	other := snapshotBlock(ts, 1, 2, 1, 0, 0, 0)
	other.BusID = "bus_something_else"
	r.HandleBusEvent("bus_something_else", other)

	wrongType := snapshotBlock(ts, 1, 2, 1, 0, 0, 0)
	wrongType.Type = "some.other.event.v1"
	r.HandleBusEvent(ProprioBusID, wrongType)

	if _, err := os.Stat(filepath.Join(base, "node-a")); !os.IsNotExist(err) {
		t.Fatalf("expected no files written for non-matching events, stat err=%v", err)
	}
}

func TestHandleBusEvent_RotatesAcrossMetricDays(t *testing.T) {
	root := withWorkspace(t, "node-a")
	base := vitalsBaseDir(root)

	r := &Recorder{}
	day1 := time.Date(2026, 7, 28, 23, 59, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 29, 0, 1, 0, 0, time.UTC)

	r.HandleBusEvent(ProprioBusID, snapshotBlock(day1, 100, 1, 1, 0, 0, 0))
	r.HandleBusEvent(ProprioBusID, snapshotBlock(day2, 200, 2, 1, 0, 0, 0))

	p1 := dayFilePath(base, "node-a", tierRaw, metricDiskFreeBytes, day1)
	p2 := dayFilePath(base, "node-a", tierRaw, metricDiskFreeBytes, day2)
	if p1 == p2 {
		t.Fatalf("expected distinct day files, got the same path %s", p1)
	}

	rows1, err := readDayFile(p1)
	if err != nil || len(rows1) != 1 || rows1[0].V != 100 {
		t.Fatalf("day1 rows=%v err=%v", rows1, err)
	}
	rows2, err := readDayFile(p2)
	if err != nil || len(rows2) != 1 || rows2[0].V != 200 {
		t.Fatalf("day2 rows=%v err=%v", rows2, err)
	}
}

func TestHandleBusEvent_MultipleTicksAppendSameDayFile(t *testing.T) {
	root := withWorkspace(t, "node-a")
	base := vitalsBaseDir(root)

	r := &Recorder{}
	ts := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		r.HandleBusEvent(ProprioBusID, snapshotBlock(ts.Add(time.Duration(i)*time.Minute), float64(100+i), 1, 1, 0, 0, 0))
	}

	path := dayFilePath(base, "node-a", tierRaw, metricDiskFreeBytes, ts)
	rows, err := readDayFile(path)
	if err != nil {
		t.Fatalf("readDayFile: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d: %+v", len(rows), rows)
	}
	for i, rw := range rows {
		if rw.V != float64(100+i) {
			t.Errorf("row %d: want v=%v got %v", i, 100+i, rw.V)
		}
	}
}

func TestEnsureVitalsGitignore_WrittenOnce(t *testing.T) {
	root := withWorkspace(t, "node-a")
	base := vitalsBaseDir(root)

	r := &Recorder{}
	ts := time.Now().UTC()
	r.HandleBusEvent(ProprioBusID, snapshotBlock(ts, 1, 1, 1, 0, 0, 0))

	marker := filepath.Join(base, ".gitignore")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf(".gitignore not written: %v", err)
	}
	got := string(data)
	if got == "" {
		t.Fatal(".gitignore is empty")
	}
	// Must ignore everything except itself.
	if !contains(got, "*\n") || !contains(got, "!.gitignore") {
		t.Errorf(".gitignore content unexpected: %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestExtractMetrics_OmitsAbsentHostGauges(t *testing.T) {
	payload := map[string]any{
		"host_vitals": map[string]any{
			"disk_free_bytes": float64(42),
			// mem_free_bytes, load1 intentionally absent (e.g. darwin).
			"uptime_seconds": float64(10),
		},
		"counts": map[string]any{
			"healthy": float64(1),
		},
		"anomalies": float64(0),
	}
	metrics := extractMetrics(payload)

	if _, ok := metrics[metricMemFreeBytes]; ok {
		t.Errorf("expected mem_free_bytes absent, got %v", metrics[metricMemFreeBytes])
	}
	if _, ok := metrics[metricLoad1]; ok {
		t.Errorf("expected load1 absent, got %v", metrics[metricLoad1])
	}
	if v, ok := metrics[metricDiskFreeBytes]; !ok || v != 42 {
		t.Errorf("expected disk_free_bytes=42, got %v ok=%v", v, ok)
	}
	if _, ok := metrics[metricAnomaliesTotal]; ok {
		t.Errorf("expected anomalies_total absent when not in payload, got %v", metrics[metricAnomaliesTotal])
	}
}

func TestHealth_DegradedAfterAppendFailure(t *testing.T) {
	root := withWorkspace(t, "node-a")
	base := vitalsBaseDir(root)

	r := &Recorder{}
	r.HandleBusEvent(ProprioBusID, snapshotBlock(time.Now(), 1, 1, 1, 0, 0, 0))
	if h := r.Health(); h.Health != "Healthy" {
		t.Fatalf("expected Healthy after a clean write, got %+v", h)
	}

	// Simulate a write failure for one metric by replacing its (already
	//-created, by the write above) directory with a plain file, so the next
	// append's os.MkdirAll for that metric's day-file parent fails.
	metricDir := filepath.Join(base, "node-a", tierRaw, metricDiskFreeBytes)
	if err := os.RemoveAll(metricDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metricDir, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	r.HandleBusEvent(ProprioBusID, snapshotBlock(time.Now(), 2, 2, 1, 0, 0, 0))
	h := r.Health()
	if h.Health != "Degraded" {
		t.Fatalf("expected Degraded after a write failure, got %+v", h)
	}
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── jaccardDistance ────────────────────────────────────────────────────────

func TestJaccardDistance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b map[string]bool
		want float64
	}{
		{
			name: "both empty",
			a:    map[string]bool{},
			b:    map[string]bool{},
			want: 0.0,
		},
		{
			name: "identical",
			a:    map[string]bool{"x": true, "y": true},
			b:    map[string]bool{"x": true, "y": true},
			want: 0.0,
		},
		{
			name: "disjoint",
			a:    map[string]bool{"x": true},
			b:    map[string]bool{"y": true},
			want: 1.0,
		},
		{
			name: "a empty b non-empty",
			a:    map[string]bool{},
			b:    map[string]bool{"x": true},
			want: 1.0,
		},
		{
			name: "half overlap: {a,b} vs {a,c} = 1 - 1/3",
			a:    map[string]bool{"a": true, "b": true},
			b:    map[string]bool{"a": true, "c": true},
			want: 2.0 / 3.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := jaccardDistance(tc.a, tc.b)
			if absF(got-tc.want) > 1e-9 {
				t.Errorf("jaccardDistance = %.9f; want %.9f", got, tc.want)
			}
		})
	}
}

func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// ── TrajectoryModel ────────────────────────────────────────────────────────

func TestNewTrajectoryModel(t *testing.T) {
	t.Parallel()
	m := NewTrajectoryModel()
	cycles, meanErr := m.Stats()
	if cycles != 0 || meanErr != 0 {
		t.Errorf("initial stats = (%d, %.4f); want (0, 0)", cycles, meanErr)
	}
	if pred := m.LastPrediction(); len(pred) != 0 {
		t.Errorf("initial prediction non-empty: %v", pred)
	}
}

func TestTrajectoryModelFirstCycleEmptyAll(t *testing.T) {
	t.Parallel()
	m := NewTrajectoryModel()

	// No attended paths, no field scores.
	u := m.Update(nil, nil)

	// Both sets empty → Jaccard = 0.
	if u.PredictionError != 0.0 {
		t.Errorf("first cycle empty error = %.4f; want 0.0", u.PredictionError)
	}
	if u.Cycle != 1 {
		t.Errorf("cycle = %d; want 1", u.Cycle)
	}
	if len(u.Prediction) != 0 {
		t.Errorf("prediction non-empty with empty field: %v", u.Prediction)
	}
}

func TestTrajectoryModelMomentumBuildsUp(t *testing.T) {
	t.Parallel()
	m := NewTrajectoryModel()
	field := map[string]float64{"/a": 0.5, "/b": 0.5}

	// Attend /a twice; /b never.
	m.Update([]string{"/a"}, field)
	m.Update([]string{"/a"}, field)

	mom := m.Momentum()
	if mom["/a"] <= mom["/b"] {
		t.Errorf("momentum[/a]=%.4f should exceed momentum[/b]=%.4f", mom["/a"], mom["/b"])
	}
}

func TestTrajectoryModelPredictionFavorsAttended(t *testing.T) {
	t.Parallel()
	m := NewTrajectoryModel()
	field := map[string]float64{"/a": 0.5, "/b": 0.5, "/c": 0.5}

	// Attend /c enough times that its momentum dominates.
	for range 5 {
		m.Update([]string{"/c"}, field)
	}
	u := m.Update([]string{"/c"}, field)

	if len(u.Prediction) == 0 {
		t.Fatal("prediction is empty")
	}
	if u.Prediction[0] != "/c" {
		t.Errorf("top prediction = %q; want /c (highest momentum)", u.Prediction[0])
	}
}

func TestTrajectoryModelRecessingPaths(t *testing.T) {
	t.Parallel()
	m := NewTrajectoryModel()

	// We need more paths than predictionSize (10) so that /a can actually
	// fall out of the top-10 when its score collapses.
	//
	// Cycle 1: /a has highest salience and is attended → enters top-10 prediction.
	field1 := map[string]float64{
		"/a":  0.9,
		"/b1": 0.05, "/b2": 0.05, "/b3": 0.05, "/b4": 0.05,
		"/b5": 0.05, "/b6": 0.05, "/b7": 0.05, "/b8": 0.05,
		"/b9": 0.05, "/b10": 0.05, "/b11": 0.05,
	}
	m.Update([]string{"/a"}, field1)

	// Cycle 2: /a collapses to near-zero; all 11 /b paths score 0.9.
	// /a falls out of the top-10 → should appear in receding.
	field2 := map[string]float64{
		"/a":  0.01,
		"/b1": 0.9, "/b2": 0.9, "/b3": 0.9, "/b4": 0.9,
		"/b5": 0.9, "/b6": 0.9, "/b7": 0.9, "/b8": 0.9,
		"/b9": 0.9, "/b10": 0.9, "/b11": 0.9,
	}
	u := m.Update(nil, field2)

	found := false
	for _, p := range u.Receding {
		if p == "/a" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected /a in receding; got %v (prediction=%v)", u.Receding, u.Prediction)
	}
}

func TestTrajectoryModelMomentumDecays(t *testing.T) {
	t.Parallel()
	m := NewTrajectoryModel()
	field := map[string]float64{"/a": 0.5}

	// Attend once, then stop.
	m.Update([]string{"/a"}, field)
	momFirst := m.Momentum()["/a"]

	for range 10 {
		m.Update(nil, field)
	}
	momLater := m.Momentum()["/a"]

	if momLater >= momFirst {
		t.Errorf("momentum did not decay: first=%.4f later=%.4f", momFirst, momLater)
	}
}

func TestTrajectoryModelStatsAccumulate(t *testing.T) {
	t.Parallel()
	m := NewTrajectoryModel()
	field := map[string]float64{"/a": 0.5}

	for range 7 {
		m.Update(nil, field)
	}
	cycles, _ := m.Stats()
	if cycles != 7 {
		t.Errorf("cycles = %d; want 7", cycles)
	}
}

func TestTrajectoryModelPredictionErrorInRange(t *testing.T) {
	t.Parallel()
	m := NewTrajectoryModel()
	field := map[string]float64{"/a": 0.8, "/b": 0.8}

	m.Update([]string{"/a"}, field)
	u := m.Update([]string{"/b"}, field) // mismatch expected

	if u.PredictionError < 0 || u.PredictionError > 1.0 {
		t.Errorf("predictionError = %.4f; must be in [0,1]", u.PredictionError)
	}
}

// ── readRecentAttentionSignals ─────────────────────────────────────────────

func TestReadRecentAttentionSignalsMissingLog(t *testing.T) {
	t.Parallel()
	// No log file — should return nil without panic.
	got := readRecentAttentionSignals(t.TempDir(), time.Now())
	if got != nil {
		t.Errorf("expected nil for missing log; got %v", got)
	}
}

func TestReadRecentAttentionSignalsFiltersTime(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	logDir := filepath.Join(root, ".cog", "run")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "attention.jsonl")

	oldTime := time.Now().Add(-10 * time.Minute)
	recentTime := time.Now()

	writeAttentionSignal(t, logPath, attentionSignal{
		TargetURI:  "cog://mem/old.md",
		OccurredAt: oldTime.UTC().Format(time.RFC3339),
	})
	writeAttentionSignal(t, logPath, attentionSignal{
		TargetURI:  "cog://mem/recent.md",
		OccurredAt: recentTime.UTC().Format(time.RFC3339),
	})

	// Cutoff is between old and recent.
	cutoff := oldTime.Add(5 * time.Minute)
	got := readRecentAttentionSignals(root, cutoff)

	if len(got) != 1 {
		t.Fatalf("got %d signals; want 1: %v", len(got), got)
	}
	if !strings.Contains(got[0], "recent") {
		t.Errorf("expected path containing 'recent'; got %q", got[0])
	}
}

func TestReadRecentAttentionSignalsAllOld(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	logDir := filepath.Join(root, ".cog", "run")
	_ = os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "attention.jsonl")

	old := time.Now().Add(-1 * time.Hour)
	writeAttentionSignal(t, logPath, attentionSignal{
		TargetURI:  "cog://mem/old.md",
		OccurredAt: old.UTC().Format(time.RFC3339),
	})

	got := readRecentAttentionSignals(root, time.Now())
	if len(got) != 0 {
		t.Errorf("expected no signals after all-old log; got %v", got)
	}
}

// ── applyObserverActions ───────────────────────────────────────────────────

func TestApplyObserverActionsWarmsAndAttenuates(t *testing.T) {
	t.Parallel()
	field := NewAttentionalField(makeConfig(t, t.TempDir()))
	field.Boost("/warmed", 0.5)
	field.Boost("/receding", 0.5)

	model := NewTrajectoryModel()

	beforeWarmed := field.Score("/warmed")
	beforeReceding := field.Score("/receding")

	warmed, attenuated := applyObserverActions(field, model,
		[]string{"/warmed"}, []string{"/receding"})

	if warmed != 1 {
		t.Errorf("warmed count = %d; want 1", warmed)
	}
	if attenuated != 1 {
		t.Errorf("attenuated count = %d; want 1", attenuated)
	}
	if field.Score("/warmed") <= beforeWarmed {
		t.Errorf("/warmed score did not increase: %.4f → %.4f", beforeWarmed, field.Score("/warmed"))
	}
	if field.Score("/receding") >= beforeReceding {
		t.Errorf("/receding score did not decrease: %.4f → %.4f", beforeReceding, field.Score("/receding"))
	}
}

func TestApplyObserverActionsEmpty(t *testing.T) {
	t.Parallel()
	field := NewAttentionalField(makeConfig(t, t.TempDir()))
	model := NewTrajectoryModel()

	w, a := applyObserverActions(field, model, nil, nil)
	if w != 0 || a != 0 {
		t.Errorf("got (%d, %d); want (0, 0)", w, a)
	}
}

// ── writeConsolidationDoc ─────────────────────────────────────────────────

func TestWriteConsolidationDoc(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := makeConfig(t, root)

	u := ObserverUpdate{
		PredictionError: 0.42,
		MeanError:       0.35,
		Prediction:      []string{"/a.md", "/b.md"},
		Receding:        []string{"/c.md"},
		Cycle:           3,
	}
	if err := writeConsolidationDoc(cfg, u, true); err != nil {
		t.Fatalf("writeConsolidationDoc: %v", err)
	}

	dir := filepath.Join(root, ".cog", "var", "observer")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read observer dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file; got %d", len(entries))
	}

	content, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	s := string(content)

	for _, want := range []string{
		"cycle: 3",
		"prediction_error: 0.4200",
		"surprise: true",
		"SURPRISE",
		"/a.md",
		"/c.md",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("consolidation doc missing %q\n---\n%s\n---", want, s)
		}
	}
}

func TestWriteConsolidationDocNoSurprise(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := makeConfig(t, root)

	u := ObserverUpdate{Cycle: 1}
	if err := writeConsolidationDoc(cfg, u, false); err != nil {
		t.Fatalf("writeConsolidationDoc: %v", err)
	}

	dir := filepath.Join(root, ".cog", "var", "observer")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 file; err=%v entries=%d", err, len(entries))
	}

	content, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if strings.Contains(string(content), "SURPRISE") {
		t.Error("doc should not contain SURPRISE when surprise=false")
	}
}

// ── Test helpers ───────────────────────────────────────────────────────────

// writeAttentionSignal appends a JSON-encoded signal to the log file.
func writeAttentionSignal(t *testing.T, path string, sig attentionSignal) {
	t.Helper()
	b, err := json.Marshal(sig)
	if err != nil {
		t.Fatalf("marshal attention signal: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open attention log: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatalf("write attention signal: %v", err)
	}
}

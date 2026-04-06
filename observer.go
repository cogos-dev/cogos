// observer.go — CogOS v3 observer loop (Field → Observer → Model → Field)
//
// Implements the trefoil closed loop that makes the daemon a true observer:
//
//	Loop 1 (Field → Observer): Each consolidation tick reads attention signals
//	from the attention log and current field scores — the raw percept.
//
//	Loop 2 (Observer → Model): TrajectoryModel updates attention momentum via
//	EMA, computes Jaccard prediction error against the previous cycle, and
//	generates a new prediction. Both error and prediction are recorded in the
//	ledger (hash-chained, irreversible — this is the arrow of time).
//
//	Loop 3 (Model → Field): Predictions pre-warm the field (salience boost).
//	Paths that drop out of the prediction are attenuated. Prediction errors
//	above the surprise threshold emit an observer.surprise coherence signal.
//
// The consolidation CogDoc written each cycle is the model's trace in the
// field — a legible record that the observer existed and acted.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// momentumDecay is the EMA multiplier per consolidation tick.
	// 0.7 means each tick retains 70% of prior momentum.
	momentumDecay = 0.7

	// predictionSize is the number of paths in each predicted set.
	predictionSize = 10

	// surpriseThreshold is the Jaccard distance (0–1) above which a
	// coherence signal fires. 0.7 = 70% mismatch between predicted and
	// actually attended.
	surpriseThreshold = 0.7

	// warmBoost is the base field boost applied to each predicted path.
	// Scaled up by momentum strength, capped at 2× base.
	warmBoost = 0.15

	// attenuationPenalty is subtracted from paths that drop out of the
	// prediction (were expected, then stopped being attended).
	attenuationPenalty = 0.05
)

// TrajectoryModel tracks attention momentum and generates predictions.
// It is the "model" in the trefoil — built from observations of the field,
// generating anticipations that act back on the field.
//
// The model is safe for concurrent reads (Stats, Momentum) and periodic
// writes (Update, called from the single consolidation goroutine).
type TrajectoryModel struct {
	mu sync.RWMutex

	// momentum maps absolute file path → current attention momentum.
	// Each attention event adds 1.0; EMA decay is applied each cycle.
	momentum map[string]float64

	// lastPrediction is the predicted set from the previous cycle.
	// Used to compute prediction error and identify receding paths.
	lastPrediction map[string]bool

	// cycleCount is the total number of Update() calls completed.
	cycleCount int64

	// cumulativeError is the sum of Jaccard distances across all cycles.
	cumulativeError float64
}

// NewTrajectoryModel constructs an empty, uninitialized model.
func NewTrajectoryModel() *TrajectoryModel {
	return &TrajectoryModel{
		momentum:       make(map[string]float64),
		lastPrediction: make(map[string]bool),
	}
}

// ObserverUpdate is the result of a single TrajectoryModel.Update() call.
type ObserverUpdate struct {
	// PredictionError is the Jaccard distance between the previous prediction
	// and the paths actually attended this cycle (0 = perfect, 1 = total miss).
	PredictionError float64

	// Prediction is the set of paths the model expects to be attended next cycle.
	Prediction []string

	// Receding is the set of paths that were predicted last cycle but dropped
	// out this cycle (expected, then stopped being attended).
	Receding []string

	// Cycle is the cycle number that just completed.
	Cycle int64

	// MeanError is the running mean prediction error across all cycles.
	MeanError float64
}

// Update feeds a new cycle's observations into the model.
// attended is the list of filesystem paths observed in the attention log
// since the last tick. fieldScores is the current salience map.
// Update is NOT safe for concurrent calls — it is called only from the
// single consolidation goroutine in process.go.
func (m *TrajectoryModel) Update(attended []string, fieldScores map[string]float64) ObserverUpdate {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Build attended set for error computation.
	attendedSet := make(map[string]bool, len(attended))
	for _, p := range attended {
		attendedSet[p] = true
	}

	// Compute prediction error: Jaccard distance between last prediction and
	// actually attended paths. On the very first cycle lastPrediction is empty,
	// so we get 0 if attendance is also empty, or 1 if attendance is non-empty.
	predErr := jaccardDistance(m.lastPrediction, attendedSet)

	// Decay all existing momentum entries.
	for path := range m.momentum {
		m.momentum[path] *= momentumDecay
		if m.momentum[path] < 0.001 {
			delete(m.momentum, path)
		}
	}
	// Add fresh signal for each attended path (1.0 per event).
	for _, path := range attended {
		m.momentum[path] += 1.0
	}

	// Generate new prediction: rank by field_score × (1 + momentum).
	// This blends the git-derived salience with live attention signal.
	type candidate struct {
		path  string
		score float64
	}
	candidates := make([]candidate, 0, len(fieldScores))
	for path, score := range fieldScores {
		combined := score * (1.0 + m.momentum[path])
		candidates = append(candidates, candidate{path, combined})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	n := predictionSize
	if n > len(candidates) {
		n = len(candidates)
	}
	prediction := make([]string, n)
	newPredSet := make(map[string]bool, n)
	for i := range n {
		prediction[i] = candidates[i].path
		newPredSet[candidates[i].path] = true
	}

	// Identify receding paths: predicted last cycle but not this cycle.
	var receding []string
	for path := range m.lastPrediction {
		if !newPredSet[path] {
			receding = append(receding, path)
		}
	}

	m.lastPrediction = newPredSet
	m.cycleCount++
	m.cumulativeError += predErr

	meanErr := float64(0)
	if m.cycleCount > 0 {
		meanErr = m.cumulativeError / float64(m.cycleCount)
	}

	return ObserverUpdate{
		PredictionError: predErr,
		Prediction:      prediction,
		Receding:        receding,
		Cycle:           m.cycleCount,
		MeanError:       meanErr,
	}
}

// Momentum returns a copy of the current momentum map.
// Safe for concurrent reads (e.g. from the HTTP handler goroutine).
func (m *TrajectoryModel) Momentum() map[string]float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]float64, len(m.momentum))
	for k, v := range m.momentum {
		out[k] = v
	}
	return out
}

// Stats returns the total cycle count and mean prediction error.
func (m *TrajectoryModel) Stats() (cycles int64, meanError float64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cycleCount == 0 {
		return 0, 0
	}
	return m.cycleCount, m.cumulativeError / float64(m.cycleCount)
}

// LastPrediction returns a copy of the most recent prediction set.
func (m *TrajectoryModel) LastPrediction() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.lastPrediction))
	for p := range m.lastPrediction {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ── Helpers ────────────────────────────────────────────────────────────────

// jaccardDistance computes 1 - |A∩B| / |A∪B|.
//
//   - Both empty → 0.0  (no prediction, no attendance: no surprise)
//   - One empty, one non-empty → 1.0  (total miss)
func jaccardDistance(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0.0
	}
	intersection := 0
	for k := range a {
		if b[k] {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0.0
	}
	return 1.0 - float64(intersection)/float64(union)
}

// readRecentAttentionSignals reads the attention log and returns the filesystem
// paths for all signals that arrived strictly after `since`.
// URI strings in the log are converted to FS paths via uriToFSPath.
// Returns nil (not an error) if the log file doesn't exist yet.
func readRecentAttentionSignals(workspaceRoot string, since time.Time) []string {
	logPath := filepath.Join(workspaceRoot, ".cog", "run", "attention.jsonl")
	f, err := os.Open(logPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var paths []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var sig attentionSignal
		if json.Unmarshal(line, &sig) != nil {
			continue
		}
		if sig.OccurredAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, sig.OccurredAt)
		if err != nil {
			continue
		}
		if !t.After(since) {
			continue
		}
		if path := uriToFSPath(workspaceRoot, sig.TargetURI); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// applyObserverActions applies the model's output to the attentional field.
//
//   - prediction: each path receives a warmBoost scaled by its momentum
//     (approaching paths get up to 2× the base boost)
//   - receding: each path receives -attenuationPenalty (they were predicted
//     but are no longer relevant)
//
// Returns the count of warmed and attenuated paths.
func applyObserverActions(field *AttentionalField, model *TrajectoryModel, prediction, receding []string) (warmed, attenuated int) {
	momentum := model.Momentum()

	for _, path := range prediction {
		m := momentum[path]
		// Scale boost by momentum strength, capped at 2× base.
		boost := warmBoost * math.Min(1.0+m, 2.0)
		field.Boost(path, boost)
		warmed++
	}
	for _, path := range receding {
		field.Boost(path, -attenuationPenalty)
		attenuated++
	}
	return warmed, attenuated
}

// writeConsolidationDoc writes a markdown observer trace for this cycle.
// Path: .cog/var/observer/cycle-{YYYYMMDDTHHMMSSZ}.md
//
// This is the model's trace in the field — a legible record that the
// observer existed, predicted, erred, and acted. Older docs are not
// pruned here; a retention policy can trim by mtime.
func writeConsolidationDoc(cfg *Config, u ObserverUpdate, surprise bool) error {
	dir := filepath.Join(cfg.WorkspaceRoot, ".cog", "var", "observer")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir observer: %w", err)
	}

	ts := time.Now().UTC()
	filename := fmt.Sprintf("cycle-%s.md", ts.Format("20060102T150405Z"))
	path := filepath.Join(dir, filename)

	var sb strings.Builder
	fmt.Fprintf(&sb, "---\n")
	fmt.Fprintf(&sb, "type: observer.cycle\n")
	fmt.Fprintf(&sb, "cycle: %d\n", u.Cycle)
	fmt.Fprintf(&sb, "timestamp: %s\n", ts.Format(time.RFC3339))
	fmt.Fprintf(&sb, "prediction_error: %.4f\n", u.PredictionError)
	fmt.Fprintf(&sb, "mean_error: %.4f\n", u.MeanError)
	fmt.Fprintf(&sb, "surprise: %v\n", surprise)
	fmt.Fprintf(&sb, "---\n\n")

	fmt.Fprintf(&sb, "# Observer Cycle %d\n\n", u.Cycle)
	fmt.Fprintf(&sb, "**Error:** %.4f | **Mean:** %.4f | **Surprise:** %v\n\n",
		u.PredictionError, u.MeanError, surprise)

	if surprise {
		fmt.Fprintf(&sb,
			"> **SURPRISE** — prediction error %.4f exceeds threshold %.2f.\n"+
				"> Coherence signal emitted.\n\n",
			u.PredictionError, surpriseThreshold)
	}

	fmt.Fprintf(&sb, "## Prediction (next cycle)\n\n")
	if len(u.Prediction) == 0 {
		fmt.Fprintf(&sb, "_field not yet populated_\n")
	} else {
		for i, p := range u.Prediction {
			fmt.Fprintf(&sb, "%d. `%s`\n", i+1, p)
		}
	}

	if len(u.Receding) > 0 {
		fmt.Fprintf(&sb, "\n## Receding (dropped from prediction)\n\n")
		for _, p := range u.Receding {
			fmt.Fprintf(&sb, "- `%s`\n", p)
		}
	}

	return os.WriteFile(path, []byte(sb.String()), 0644)
}

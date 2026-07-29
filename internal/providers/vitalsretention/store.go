// store.go — on-disk NDJSON layout for the vitals-retention recorder.
//
// Layout (RFC-040 §S2):
//
//	<base>/<node_key>/<tier>/<metric>/<YYYY-MM-DD>.ndjson
//
// base defaults to <workspace>/.cog/observatory/vitals. tier is one of
// "raw" (tick resolution), "5m", or "1h" (see compact.go). One file per
// metric-day per RFC-040's exact wording, so window() queries touch only
// the files that can possibly contain the requested range.
package vitalsretention

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// warnf is a thin printf-style wrapper over slog.Warn used throughout this
// package for best-effort operations (a failed append/compaction must never
// panic or block the tick — see package doc).
func warnf(format string, args ...any) {
	slog.Warn(fmt.Sprintf(format, args...))
}

const (
	tierRaw = "raw"
	tier5m  = "5m"
	tier1h  = "1h"
)

// validTiers is used to validate a caller-supplied resolution string —
// RFC-040 N2 ships exactly window(metric, since, resolution), so resolution
// must be one of the tiers that actually exist on disk, not an open-ended
// aggregation spec.
var validTiers = map[string]bool{tierRaw: true, tier5m: true, tier1h: true}

// dayLayout is the on-disk day-file date format.
const dayLayout = "2006-01-02"

// row is the on-disk NDJSON shape. Raw rows carry only ts+v (Min/Max/N
// absent — omitempty drops them, one tick is its own min/max/count-of-1).
// Compacted rows (5m/1h) carry the full aggregate so a window() caller can
// see spread, not just the mean.
type row struct {
	Ts  string   `json:"ts"`
	V   float64  `json:"v"`
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
	N   int      `json:"n,omitempty"`
}

func (rw row) parseTs() (time.Time, error) {
	return time.Parse(time.RFC3339Nano, rw.Ts)
}

// tierMetricDir returns the directory holding one metric's day-files within
// one tier.
func tierMetricDir(base, nodeKey, tier, metric string) string {
	return filepath.Join(base, nodeKey, tier, metric)
}

// dayFilePath returns the path for one metric-day file.
func dayFilePath(base, nodeKey, tier, metric string, day time.Time) string {
	return filepath.Join(tierMetricDir(base, nodeKey, tier, metric), day.UTC().Format(dayLayout)+".ndjson")
}

// dayFromFilename parses the YYYY-MM-DD.ndjson filename back into a day
// boundary (UTC midnight). Returns an error for anything that doesn't match
// — callers use this to filter directory listings down to real day-files.
func dayFromFilename(name string) (time.Time, error) {
	base := strings.TrimSuffix(name, ".ndjson")
	if base == name {
		return time.Time{}, fmt.Errorf("not an .ndjson file: %s", name)
	}
	return time.ParseInLocation(dayLayout, base, time.UTC)
}

// ensureVitalsGitignore writes a self-ignoring .gitignore at the vitals base
// dir the first time this process creates it. The kernel does not manage any
// workspace-level .gitignore (checked: no code path writes to the repo's
// .gitignore programmatically) — this directory-local marker is RFC-040
// N3's "retention files are gitignored" clause applied the only way
// available without reaching into a file this package doesn't own.
//
// "*\n!.gitignore\n" ignores everything under the vitals dir except the
// marker itself, so `git status` shows nothing after S2 starts writing, on
// a repo that has never seen this path before.
func ensureVitalsGitignore(base string) {
	marker := filepath.Join(base, ".gitignore")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return
	}
	content := "" +
		"# RFC-040 S2 (vitals-retention): dense per-tick NDJSON lives here.\n" +
		"# The kernel does not manage a workspace-level .gitignore, so this\n" +
		"# directory ignores itself (N3: no dense appends in git-tracked paths).\n" +
		"*\n" +
		"!.gitignore\n"
	// Best-effort: a failed write here is not fatal to recording (the
	// caller's append still proceeds), but IS worth a warning since it
	// means a workspace scan could pick up raw vitals files.
	if err := os.WriteFile(marker, []byte(content), 0o644); err != nil {
		warnf("vitals-retention: failed to write .gitignore marker at %s: %v", marker, err)
	}
}

// appendRow appends one NDJSON row to the metric-day file, creating parent
// directories (and the base-dir .gitignore, once) as needed.
func appendRow(base, nodeKey, tier, metric string, day time.Time, rw row) error {
	dir := tierMetricDir(base, nodeKey, tier, metric)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	ensureVitalsGitignore(base)

	line, err := json.Marshal(rw)
	if err != nil {
		return fmt.Errorf("marshal row: %w", err)
	}
	path := dayFilePath(base, nodeKey, tier, metric, day)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// writeRows overwrites a metric-day file with exactly rows (used by
// compaction to write a coarser tier's day-file from scratch — see
// compact.go's downsampleOneDay, which is the sole writer of the 5m/1h
// tiers and always regenerates a day-file fully, so overwrite is safe and
// idempotent: a day is compacted from raw exactly once, after which the raw
// source is pruned and can't be reprocessed).
func writeRows(base, nodeKey, tier, metric string, day time.Time, rows []row) error {
	dir := tierMetricDir(base, nodeKey, tier, metric)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	ensureVitalsGitignore(base)

	path := dayFilePath(base, nodeKey, tier, metric, day)
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	w := bufio.NewWriter(f)
	for _, rw := range rows {
		line, err := json.Marshal(rw)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("marshal row: %w", err)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("write %s: %w", tmp, err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("flush %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// readDayFile reads every row in a metric-day file. A missing file returns
// (nil, nil) — no data for that day is not an error (it may be un-recorded,
// pruned, or not-yet-compacted into this tier).
func readDayFile(path string) ([]row, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var rows []row
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rw row
		if err := json.Unmarshal(line, &rw); err != nil {
			// A single malformed line (e.g. truncated by a crash mid-write)
			// should not lose the rest of the day's history.
			warnf("vitals-retention: skipping malformed row in %s: %v", path, err)
			continue
		}
		rows = append(rows, rw)
	}
	if err := scanner.Err(); err != nil {
		return rows, err
	}
	return rows, nil
}

// listMetrics returns the metric names (subdirectory names) present under
// <base>/<nodeKey>/<tier>.
func listMetrics(base, nodeKey, tier string) ([]string, error) {
	dir := filepath.Join(base, nodeKey, tier)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var metrics []string
	for _, e := range entries {
		if e.IsDir() {
			metrics = append(metrics, e.Name())
		}
	}
	sort.Strings(metrics)
	return metrics, nil
}

// listDayFiles returns the days (ascending) that have a file under
// <base>/<nodeKey>/<tier>/<metric>.
func listDayFiles(base, nodeKey, tier, metric string) ([]time.Time, error) {
	dir := tierMetricDir(base, nodeKey, tier, metric)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var days []time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		d, err := dayFromFilename(e.Name())
		if err != nil {
			continue // e.g. the .gitignore marker
		}
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	return days, nil
}

// removeIfExists deletes path, treating "already gone" as success.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// dirSize returns the total size in bytes of all regular files under dir,
// recursively. Used by N3 raw-tier budget enforcement.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil && os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}

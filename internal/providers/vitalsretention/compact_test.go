package vitalsretention

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func ptr(f float64) *float64 { return &f }

func TestAggregateRows_ComputesAvgMinMaxCount(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rows := []row{
		{Ts: base.Format(time.RFC3339Nano), V: 10},
		{Ts: base.Add(1 * time.Minute).Format(time.RFC3339Nano), V: 20},
		{Ts: base.Add(2 * time.Minute).Format(time.RFC3339Nano), V: 30},
		// A separate 5m bucket.
		{Ts: base.Add(5 * time.Minute).Format(time.RFC3339Nano), V: 100},
	}

	out := aggregateRows(rows, 5*time.Minute)
	if len(out) != 2 {
		t.Fatalf("want 2 buckets, got %d: %+v", len(out), out)
	}

	first := out[0]
	if first.V != 20 { // (10+20+30)/3
		t.Errorf("bucket1 avg: want 20, got %v", first.V)
	}
	if first.Min == nil || *first.Min != 10 {
		t.Errorf("bucket1 min: want 10, got %v", first.Min)
	}
	if first.Max == nil || *first.Max != 30 {
		t.Errorf("bucket1 max: want 30, got %v", first.Max)
	}
	if first.N != 3 {
		t.Errorf("bucket1 count: want 3, got %d", first.N)
	}

	second := out[1]
	if second.V != 100 || second.N != 1 {
		t.Errorf("bucket2: want v=100 n=1, got v=%v n=%d", second.V, second.N)
	}
}

func TestAggregateRows_FoldsExistingAggregates(t *testing.T) {
	// Simulates re-bucketing 5m rows (which already carry min/max/n) into 1h
	// buckets — the weighted-by-N path must not silently treat a 5m
	// aggregate as a single unweighted sample.
	hour := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rows := []row{
		{Ts: hour.Format(time.RFC3339Nano), V: 10, Min: ptr(5), Max: ptr(15), N: 5},
		{Ts: hour.Add(30 * time.Minute).Format(time.RFC3339Nano), V: 20, Min: ptr(18), Max: ptr(25), N: 5},
	}

	out := aggregateRows(rows, time.Hour)
	if len(out) != 1 {
		t.Fatalf("want 1 bucket, got %d", len(out))
	}
	b := out[0]
	// weighted avg = (10*5 + 20*5) / 10 = 15
	if b.V != 15 {
		t.Errorf("want weighted avg 15, got %v", b.V)
	}
	if b.Min == nil || *b.Min != 5 {
		t.Errorf("want min 5, got %v", b.Min)
	}
	if b.Max == nil || *b.Max != 25 {
		t.Errorf("want max 25, got %v", b.Max)
	}
	if b.N != 10 {
		t.Errorf("want n=10, got %d", b.N)
	}
}

func TestDownsampleOneDay_MovesDataAndPrunesRawSource(t *testing.T) {
	base := t.TempDir()
	nodeKey := "node-a"
	metric := "disk_free_bytes"
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		ts := day.Add(time.Duration(i) * time.Minute)
		if err := appendRow(base, nodeKey, tierRaw, metric, ts, row{Ts: ts.Format(time.RFC3339Nano), V: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	rawPath := dayFilePath(base, nodeKey, tierRaw, metric, day)
	if _, err := os.Stat(rawPath); err != nil {
		t.Fatalf("raw file should exist before compaction: %v", err)
	}

	if err := downsampleOneDay(base, nodeKey, metric, tierRaw, tier5m, day); err != nil {
		t.Fatalf("downsampleOneDay: %v", err)
	}

	if _, err := os.Stat(rawPath); !os.IsNotExist(err) {
		t.Fatalf("raw source should be pruned after compaction, stat err=%v", err)
	}

	midPath := dayFilePath(base, nodeKey, tier5m, metric, day)
	rows, err := readDayFile(midPath)
	if err != nil {
		t.Fatalf("readDayFile(5m): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 aggregated bucket (all 3 ticks within 5m), got %d", len(rows))
	}
	if rows[0].N != 3 {
		t.Errorf("want n=3, got %d", rows[0].N)
	}
	if rows[0].V != 1 { // avg(0,1,2) == 1
		t.Errorf("want avg=1, got %v", rows[0].V)
	}
}

func TestAgeTier_OnlyCompactsDaysFullyOlderThanCutoff(t *testing.T) {
	base := t.TempDir()
	nodeKey := "node-a"
	metric := "disk_free_bytes"

	oldDay := time.Now().UTC().AddDate(0, 0, -10).Truncate(24 * time.Hour)
	recentDay := time.Now().UTC().Truncate(24 * time.Hour)

	mustAppend := func(day time.Time, v float64) {
		if err := appendRow(base, nodeKey, tierRaw, metric, day, row{Ts: day.Format(time.RFC3339Nano), V: v}); err != nil {
			t.Fatal(err)
		}
	}
	mustAppend(oldDay, 1)
	mustAppend(recentDay, 2)

	r := &Recorder{}
	cutoff := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour)
	if err := r.ageTier(base, nodeKey, tierRaw, tier5m, cutoff); err != nil {
		t.Fatalf("ageTier: %v", err)
	}

	if _, err := os.Stat(dayFilePath(base, nodeKey, tierRaw, metric, oldDay)); !os.IsNotExist(err) {
		t.Errorf("old day should have been compacted away, stat err=%v", err)
	}
	if _, err := os.Stat(dayFilePath(base, nodeKey, tierRaw, metric, recentDay)); err != nil {
		t.Errorf("recent day should still be raw: %v", err)
	}
	if _, err := os.Stat(dayFilePath(base, nodeKey, tier5m, metric, oldDay)); err != nil {
		t.Errorf("old day should now exist in the 5m tier: %v", err)
	}
}

func TestEnforceRawBudget_CompactsOldestFirstUntilUnderBudget(t *testing.T) {
	base := t.TempDir()
	nodeKey := "node-a"
	metric := "disk_free_bytes"

	day1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) // oldest
	day2 := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC) // newest

	// ~13,500 rows/day at ~46 bytes/line lands each day file a bit above
	// 600KB, so two days together exceed a 1MB budget but either day alone
	// does not — enough to prove "oldest-first, stop once under budget"
	// rather than "compact everything."
	writeManyRows := func(day time.Time) {
		rows := make([]row, 0, 13500)
		for i := 0; i < 13500; i++ {
			ts := day.Add(time.Duration(i) * time.Second)
			rows = append(rows, row{Ts: ts.Format(time.RFC3339Nano), V: float64(i)})
		}
		if err := writeRows(base, nodeKey, tierRaw, metric, day, rows); err != nil {
			t.Fatal(err)
		}
	}
	writeManyRows(day1)
	writeManyRows(day2)

	sizeBefore, err := dirSize(fmt.Sprintf("%s/%s/%s", base, nodeKey, tierRaw))
	if err != nil {
		t.Fatal(err)
	}
	if sizeBefore <= 1024*1024 {
		t.Fatalf("test fixture too small to exceed a 1MB budget: %d bytes", sizeBefore)
	}

	r := &Recorder{}
	cfg := Config{RawBudgetMB: 1}
	if err := r.enforceRawBudget(base, nodeKey, cfg); err != nil {
		t.Fatalf("enforceRawBudget: %v", err)
	}

	sizeAfter, err := dirSize(fmt.Sprintf("%s/%s/%s", base, nodeKey, tierRaw))
	if err != nil {
		t.Fatal(err)
	}
	if sizeAfter > cfg.rawBudgetBytes() {
		t.Fatalf("raw tier still over budget after enforcement: %d > %d", sizeAfter, cfg.rawBudgetBytes())
	}

	if _, err := os.Stat(dayFilePath(base, nodeKey, tierRaw, metric, day1)); !os.IsNotExist(err) {
		t.Errorf("oldest day (day1) should have been compacted away, stat err=%v", err)
	}
	if _, err := os.Stat(dayFilePath(base, nodeKey, tierRaw, metric, day2)); err != nil {
		t.Errorf("newest day (day2) should remain raw once under budget: %v", err)
	}
	if _, err := os.Stat(dayFilePath(base, nodeKey, tier5m, metric, day1)); err != nil {
		t.Errorf("day1 should have been downsampled into the 5m tier: %v", err)
	}
}

// TestOldestRawDay_ExcludesGivenDay is the #497 fix-review regression test
// for oldestRawDay directly: given an older day-file and one matching
// excludeDay (today, in production use), the older one must win even though
// it isn't chronologically last, and excludeDay must never be returned even
// when it is a metric's only remaining raw day.
func TestOldestRawDay_ExcludesGivenDay(t *testing.T) {
	base := t.TempDir()
	nodeKey := "node-a"

	today := time.Now().UTC().Truncate(24 * time.Hour)
	older := today.AddDate(0, 0, -5)

	// metric "a" has both an older day and today's actively-written file.
	if err := appendRow(base, nodeKey, tierRaw, "metric_a", older, row{Ts: older.Format(time.RFC3339Nano), V: 1}); err != nil {
		t.Fatal(err)
	}
	if err := appendRow(base, nodeKey, tierRaw, "metric_a", today, row{Ts: today.Format(time.RFC3339Nano), V: 2}); err != nil {
		t.Fatal(err)
	}
	// metric "b" has ONLY today's file — must be excluded entirely for this
	// metric, not just skipped in favor of a's older day.
	if err := appendRow(base, nodeKey, tierRaw, "metric_b", today, row{Ts: today.Format(time.RFC3339Nano), V: 3}); err != nil {
		t.Fatal(err)
	}

	metric, day, ok, err := oldestRawDay(base, nodeKey, today)
	if err != nil {
		t.Fatalf("oldestRawDay: %v", err)
	}
	if !ok {
		t.Fatal("expected a candidate (metric_a's older day)")
	}
	if metric != "metric_a" || !day.Equal(older) {
		t.Fatalf("want (metric_a, %v), got (%s, %v)", older, metric, day)
	}

	// Now the only remaining raw data anywhere is today's — no candidate at
	// all, not even a mistaken fallback to today.
	if err := removeDayFile(base, nodeKey, tierRaw, "metric_a", older); err != nil {
		t.Fatal(err)
	}
	_, _, ok, err = oldestRawDay(base, nodeKey, today)
	if err != nil {
		t.Fatalf("oldestRawDay: %v", err)
	}
	if ok {
		t.Fatal("expected no candidate once only today's day-files remain")
	}
}

// TestEnforceRawBudget_NeverCompactsTodaysActivelyWrittenDay is the #497
// fix-review regression test at the enforceRawBudget level: this is the
// exact scenario the reviewer flagged — reaching the read-then-delete path
// (downsampleOneDay) on the day-file HandleBusEvent may still be appending
// to concurrently, now that compaction runs off its own goroutine instead
// of blocking the tick. Even when today's file is the ONLY raw data and is
// itself over budget, enforceRawBudget must leave it alone rather than
// racing a concurrent append.
//
// A cog-review pass on an earlier version of this test (head 4a0decd) caught
// that 13,500 rows lands a single day-file at ~517KiB — comfortably under a
// 1MB budget on its own (see TestEnforceRawBudget_CompactsOldestFirstUntilUnderBudget's
// comment: two such files are needed to cross 1MB, not one) — so
// enforceRawBudget's very first `size <= budget` check returned true and the
// exclusion logic under test was never reached; the test passed whether or
// not the fix existed. This version writes enough rows that today's single
// file alone exceeds the budget, and asserts the tier is STILL over budget
// afterward (not just that the file survives), so a regression that let
// enforceRawBudget silently give up early for any reason would also be
// caught.
func TestEnforceRawBudget_NeverCompactsTodaysActivelyWrittenDay(t *testing.T) {
	base := t.TempDir()
	nodeKey := "node-a"
	metric := "disk_free_bytes"

	today := time.Now().UTC().Truncate(24 * time.Hour)

	// 30,000 rows of this shape land comfortably over 1MB on their own
	// (13,500 rows ≈ 517KiB per the sibling test's measurement, so 30,000
	// ≈ 1.15MB) — budget enforcement must still refuse to touch this file
	// because it's today's, not because it happens to fit under budget.
	const rowCount = 30000
	rows := make([]row, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		ts := today.Add(time.Duration(i) * time.Second)
		rows = append(rows, row{Ts: ts.Format(time.RFC3339Nano), V: float64(i)})
	}
	if err := writeRows(base, nodeKey, tierRaw, metric, today, rows); err != nil {
		t.Fatal(err)
	}

	rawDir := fmt.Sprintf("%s/%s/%s", base, nodeKey, tierRaw)
	cfg := Config{RawBudgetMB: 1}
	sizeBefore, err := dirSize(rawDir)
	if err != nil {
		t.Fatal(err)
	}
	if sizeBefore <= cfg.rawBudgetBytes() {
		t.Fatalf("test fixture too small to exceed the 1MB budget on its own: %d bytes", sizeBefore)
	}

	r := &Recorder{}
	if err := r.enforceRawBudget(base, nodeKey, cfg); err != nil {
		t.Fatalf("enforceRawBudget: %v", err)
	}

	// Today's raw file must survive untouched — no 5m sibling created, no
	// deletion — and the tier must STILL be over budget, proving
	// enforceRawBudget actually reached (and declined to act on) today's
	// file rather than returning early for an unrelated reason.
	if _, err := os.Stat(dayFilePath(base, nodeKey, tierRaw, metric, today)); err != nil {
		t.Fatalf("today's raw file must survive enforceRawBudget: %v", err)
	}
	if _, err := os.Stat(dayFilePath(base, nodeKey, tier5m, metric, today)); !os.IsNotExist(err) {
		t.Fatalf("today's file must not have been downsampled, stat err=%v", err)
	}
	sizeAfter, err := dirSize(rawDir)
	if err != nil {
		t.Fatal(err)
	}
	if sizeAfter != sizeBefore {
		t.Fatalf("raw tier size changed (%d -> %d) even though the only file present is today's", sizeBefore, sizeAfter)
	}
	if sizeAfter <= cfg.rawBudgetBytes() {
		t.Fatal("raw tier should still be over budget after enforcement, since today's file was the only (excluded) candidate")
	}
}

func TestPruneTier_DeletesOnlyOlderThanCutoff(t *testing.T) {
	base := t.TempDir()
	nodeKey := "node-a"
	metric := "disk_free_bytes"

	oldDay := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newDay := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	for _, d := range []time.Time{oldDay, newDay} {
		if err := writeRows(base, nodeKey, tier1h, metric, d, []row{{Ts: d.Format(time.RFC3339Nano), V: 1}}); err != nil {
			t.Fatal(err)
		}
	}

	r := &Recorder{}
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := r.pruneTier(base, nodeKey, tier1h, cutoff); err != nil {
		t.Fatalf("pruneTier: %v", err)
	}

	if _, err := os.Stat(dayFilePath(base, nodeKey, tier1h, metric, oldDay)); !os.IsNotExist(err) {
		t.Errorf("old day should be pruned, stat err=%v", err)
	}
	if _, err := os.Stat(dayFilePath(base, nodeKey, tier1h, metric, newDay)); err != nil {
		t.Errorf("new day should survive prune: %v", err)
	}
}

func TestCompactNode_FullPassIsIdempotentOnSecondRun(t *testing.T) {
	base := t.TempDir()
	nodeKey := "node-a"
	metric := "disk_free_bytes"
	oldDay := time.Now().UTC().AddDate(0, 0, -10).Truncate(24 * time.Hour)

	if err := appendRow(base, nodeKey, tierRaw, metric, oldDay, row{Ts: oldDay.Format(time.RFC3339Nano), V: 1}); err != nil {
		t.Fatal(err)
	}

	r := &Recorder{}
	cfg := Config{}
	if err := r.compactNode(base, nodeKey, cfg); err != nil {
		t.Fatalf("first compactNode: %v", err)
	}
	if err := r.compactNode(base, nodeKey, cfg); err != nil {
		t.Fatalf("second compactNode should be a safe no-op: %v", err)
	}

	rows, err := readDayFile(dayFilePath(base, nodeKey, tier5m, metric, oldDay))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one compacted row after two passes, got %d", len(rows))
	}
}

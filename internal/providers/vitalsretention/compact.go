// compact.go — RFC-040 §S2 compaction: downsample raw->5m after 48h,
// 5m->1h after 30d, then prune per config; enforce N3's provisional raw-tier
// budget (100MB/node default) by engaging compaction early when exceeded.
//
// Compaction runs on the same per-tick dispatch as recording (see
// recorder.go's HandleBusEvent), throttled by a compactCheckInterval
// watermark so repeated ticks don't repeatedly walk the vitals tree. This
// keeps with RFC-040's "no new loop, no new daemon" doctrine: the tick that
// already fires is both the scrape loop (S0) and, now, the compaction
// cadence (S2) — there is exactly one clock in this subsystem.
package vitalsretention

import (
	"path/filepath"
	"time"
)

// bucketDuration returns the aggregation window for a target tier, or 0 for
// an unrecognized tier (callers treat 0 as "don't downsample into this").
func bucketDuration(tier string) time.Duration {
	switch tier {
	case tier5m:
		return 5 * time.Minute
	case tier1h:
		return time.Hour
	default:
		return 0
	}
}

// maybeCompact hands a compaction pass for nodeKey off to its own goroutine
// if the check interval has elapsed since the last pass and no compaction is
// already in flight. It never blocks on compaction I/O — the caller is the
// bus-handler dispatch context (HandleBusEvent, synchronous inside
// BusSessionManager.AppendEvent), and per #497 that path must return as soon
// as the (cheap) sample append is durable, without waiting on compaction's
// file I/O. Outcomes are recorded via recordCompactResult for Health() to
// surface instead of returning an error here.
func (r *Recorder) maybeCompact(base, nodeKey string) {
	root, err := resolveWorkspaceRoot()
	if err != nil {
		return
	}
	cfg := loadConfigCached(root)

	if !r.claimCompactSlot(cfg) {
		return
	}

	go func() {
		err := compactHook(r, base, nodeKey, cfg)
		r.recordCompactResult(err)
		if err != nil {
			warnf("vitals-retention: compaction pass failed for node=%s: %v", nodeKey, err)
		}
	}()
}

// claimCompactSlot atomically checks whether a compaction pass is due
// (interval elapsed) and, if so and none is already in flight, claims the
// single-flight slot and stamps lastCompactAt immediately — before the pass
// itself has run. Doing the due-check and the claim inside one critical
// section closes the check-then-act race on lastCompactAt flagged
// non-blocking in #493's final review: previously two concurrent callers
// could both observe "due" before either updated lastCompactAt, since the
// update only happened after compactNode returned. Now the claim and the
// stamp are the same atomic step, so at most one caller ever proceeds per
// interval, and the `compacting` guard additionally covers the case where a
// single pass runs longer than the check interval itself.
func (r *Recorder) claimCompactSlot(cfg Config) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.compacting {
		return false
	}
	if time.Since(r.lastCompactAt) < cfg.compactCheckInterval() {
		return false
	}
	r.compacting = true
	r.lastCompactAt = time.Now()
	return true
}

// compactHook performs the actual compaction pass. It is a package-level
// seam (rather than calling r.compactNode directly) so tests can inject a
// slow or instrumented compaction without real slow disk I/O — see
// SetCompactHookForTest.
var compactHook = func(r *Recorder, base, nodeKey string, cfg Config) error {
	return r.compactNode(base, nodeKey, cfg)
}

// SetCompactHookForTest overrides compactHook for the duration of a test.
// Callers must invoke the returned restore func (typically via t.Cleanup)
// to avoid leaking the override into other tests.
func SetCompactHookForTest(f func(r *Recorder, base, nodeKey string, cfg Config) error) (restore func()) {
	prev := compactHook
	compactHook = f
	return func() { compactHook = prev }
}

// compactNode runs one full compaction pass: raw->5m aging, 5m->1h aging,
// N3 raw-budget enforcement, then the optional final 1h prune.
func (r *Recorder) compactNode(base, nodeKey string, cfg Config) error {
	now := time.Now().UTC()

	// 1. Age raw -> 5m: any raw day-file whose entire day is older than
	// RawRetentionHours is eligible. Using whole-day cutoffs (rather than a
	// mid-day boundary) means a day currently being written is never
	// touched, and a compacted day-file is never revisited (its raw source
	// is gone), which is what makes this idempotent.
	rawCutoffDay := now.Add(-cfg.rawRetentionHours()).Truncate(24 * time.Hour)
	if err := r.ageTier(base, nodeKey, tierRaw, tier5m, rawCutoffDay); err != nil {
		return err
	}

	// 2. Age 5m -> 1h.
	midCutoffDay := now.AddDate(0, 0, -cfg.midResRetentionDays()).Truncate(24 * time.Hour)
	if err := r.ageTier(base, nodeKey, tier5m, tier1h, midCutoffDay); err != nil {
		return err
	}

	// 3. N3: raw tier must not exceed the per-node budget. If step 1 wasn't
	// enough (e.g. a burst of very frequent ticks, or RawRetentionHours
	// configured generously), compact the oldest remaining raw day-files
	// regardless of age until under budget.
	if err := r.enforceRawBudget(base, nodeKey, cfg); err != nil {
		return err
	}

	// 4. "prunes per config" — delete 1h data older than PruneAfterDays, if
	// configured. This is the only step that deletes without a compacted
	// replacement: PruneAfterDays==0 (default) means never, matching the
	// principle that a provisional budget number should not silently start
	// discarding history nobody asked to discard.
	if cfg.PruneAfterDays > 0 {
		pruneCutoffDay := now.AddDate(0, 0, -cfg.PruneAfterDays).Truncate(24 * time.Hour)
		if err := r.pruneTier(base, nodeKey, tier1h, pruneCutoffDay); err != nil {
			return err
		}
	}

	return nil
}

// ageTier downsamples every metric's day-files in fromTier that are fully
// older than cutoffDay into toTier, pruning the fromTier source file on
// success.
func (r *Recorder) ageTier(base, nodeKey, fromTier, toTier string, cutoffDay time.Time) error {
	metrics, err := listMetrics(base, nodeKey, fromTier)
	if err != nil {
		return err
	}
	var firstErr error
	for _, metric := range metrics {
		days, err := listDayFiles(base, nodeKey, fromTier, metric)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, day := range days {
			if !day.Before(cutoffDay) {
				continue // not old enough yet
			}
			if err := downsampleOneDay(base, nodeKey, metric, fromTier, toTier, day); err != nil {
				warnf("vitals-retention: downsample failed metric=%s day=%s %s->%s: %v",
					metric, day.Format(dayLayout), fromTier, toTier, err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

// downsampleOneDay reads fromTier's day-file, buckets its rows by toTier's
// bucket duration, writes the aggregated rows to toTier, and — only after
// that write succeeds — deletes the fromTier source. This ordering means a
// crash mid-compaction leaves the raw source intact (re-processable next
// pass) rather than losing data.
func downsampleOneDay(base, nodeKey, metric, fromTier, toTier string, day time.Time) error {
	bucket := bucketDuration(toTier)
	if bucket <= 0 {
		return nil
	}
	srcPath := dayFilePath(base, nodeKey, fromTier, metric, day)
	rows, err := readDayFile(srcPath)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		// Nothing to compact — still prune the (empty or unreadable) source
		// so it doesn't get walked forever.
		return removeDayFile(base, nodeKey, fromTier, metric, day)
	}

	agg := aggregateRows(rows, bucket)
	if err := writeRows(base, nodeKey, toTier, metric, day, agg); err != nil {
		return err
	}
	return removeDayFile(base, nodeKey, fromTier, metric, day)
}

// aggregateRows buckets rows by bucket duration (UTC-aligned via
// time.Truncate, so 5m/1h buckets fall on clock boundaries) and returns one
// aggregated row per bucket, sorted by time. Each source row contributes its
// own value once; a row that is itself already an aggregate (Min/Max/N set,
// e.g. re-bucketing 5m into 1h) is folded in using its existing min/max/n
// rather than treating V as a single unweighted sample, so 5m->1h
// aggregation doesn't quietly under-count.
func aggregateRows(rows []row, bucket time.Duration) []row {
	type acc struct {
		sum        float64
		min, max   float64
		n          int
		bucketTime time.Time
	}
	buckets := make(map[int64]*acc)
	var order []int64

	for _, rw := range rows {
		ts, err := rw.parseTs()
		if err != nil {
			continue
		}
		bt := ts.Truncate(bucket)
		key := bt.UnixNano()
		a, ok := buckets[key]
		if !ok {
			a = &acc{bucketTime: bt, min: rw.V, max: rw.V}
			if rw.Min != nil {
				a.min = *rw.Min
			}
			if rw.Max != nil {
				a.max = *rw.Max
			}
			buckets[key] = a
			order = append(order, key)
		}
		n := 1
		if rw.N > 0 {
			n = rw.N
		}
		a.sum += rw.V * float64(n)
		a.n += n
		lo, hi := rw.V, rw.V
		if rw.Min != nil {
			lo = *rw.Min
		}
		if rw.Max != nil {
			hi = *rw.Max
		}
		if lo < a.min {
			a.min = lo
		}
		if hi > a.max {
			a.max = hi
		}
	}

	sortInt64s(order)

	out := make([]row, 0, len(order))
	for _, key := range order {
		a := buckets[key]
		avg := a.sum / float64(a.n)
		min, max := a.min, a.max
		out = append(out, row{
			Ts:  a.bucketTime.Format(time.RFC3339Nano),
			V:   avg,
			Min: &min,
			Max: &max,
			N:   a.n,
		})
	}
	return out
}

func sortInt64s(s []int64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// removeDayFile deletes one metric-day file. Missing is not an error.
func removeDayFile(base, nodeKey, tier, metric string, day time.Time) error {
	path := dayFilePath(base, nodeKey, tier, metric, day)
	if err := removeIfExists(path); err != nil {
		return err
	}
	return nil
}

// enforceRawBudget implements N3's "on-disk raw tier ≤100MB/node before
// compaction MUST engage" — provisional numbers pending OQ-3's measurement.
// Repeatedly compacts the single oldest raw day-file across all metrics
// (oldest first, regardless of RawRetentionHours) until the raw tier's total
// size is at or under budget, or there is nothing left to compact.
//
// today's day-file is never a candidate — see oldestRawDay's doc for why
// this exemption became load-bearing once compaction moved off the
// synchronous tick path (#497 fix-review finding): before that change, the
// ticker's own next append could not run until a prior tick's synchronous
// compaction fully returned, so this loop could never observe a
// concurrent appendRow to the same day-file it was about to read-then-
// delete. Compaction now runs on its own goroutine while the tick path
// keeps appending, so that race is reachable unless the actively-written
// day is excluded outright.
func (r *Recorder) enforceRawBudget(base, nodeKey string, cfg Config) error {
	budget := cfg.rawBudgetBytes()
	rawDir := filepath.Join(base, nodeKey, tierRaw)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for {
		size, err := dirSize(rawDir)
		if err != nil {
			return err
		}
		if size <= budget {
			return nil
		}
		metric, day, ok, err := oldestRawDay(base, nodeKey, today)
		if err != nil {
			return err
		}
		if !ok {
			// Over budget with nothing left to compact (e.g. a single
			// metric's one remaining day already exceeds the budget alone,
			// or the only remaining raw data is today's actively-written
			// day-file, which is deliberately never a candidate — see doc
			// above). Nothing more this pass can do; Health() surfaces this
			// via the caller's error path if downsampling itself is
			// failing, but an unsatisfiable budget on a small, healthy
			// history is not itself an error condition worth failing the
			// pass over.
			return nil
		}
		if err := downsampleOneDay(base, nodeKey, metric, tierRaw, tier5m, day); err != nil {
			return err
		}
	}
}

// oldestRawDay scans every metric's raw tier and returns the single
// earliest (metric, day) pair across all of them, excluding excludeDay.
//
// excludeDay is always the caller's current UTC day: downsampleOneDay reads
// a day-file's rows, writes the aggregate, then deletes the raw source
// (compact.go's downsampleOneDay doc) — read-then-delete with no locking
// against a concurrent writer. HandleBusEvent's appendRow can land a new
// row in today's file at any moment now that compaction runs off its own
// goroutine (see enforceRawBudget's doc), so a raw-budget pass that picked
// today's file could read it, race a concurrent append, and then delete the
// file — silently losing the just-appended row with no trace in either the
// raw source or the 5m aggregate written from the earlier snapshot. Every
// day strictly before today is safe: HandleBusEvent only ever appends to
// the day-file matching the event's own timestamp (store.go/appendRow), so
// a prior day's file is immutable once the day has rolled over.
func oldestRawDay(base, nodeKey string, excludeDay time.Time) (metric string, day time.Time, ok bool, err error) {
	metrics, err := listMetrics(base, nodeKey, tierRaw)
	if err != nil {
		return "", time.Time{}, false, err
	}
	var (
		bestMetric string
		bestDay    time.Time
		found      bool
	)
	for _, m := range metrics {
		days, err := listDayFiles(base, nodeKey, tierRaw, m)
		if err != nil {
			return "", time.Time{}, false, err
		}
		for _, d := range days {
			if d.Equal(excludeDay) {
				continue
			}
			if !found || d.Before(bestDay) {
				bestMetric, bestDay, found = m, d, true
			}
			break // days is ascending; the first non-excluded entry is this metric's oldest
		}
	}
	return bestMetric, bestDay, found, nil
}

// pruneTier deletes every day-file older than cutoffDay across all metrics
// in tier, with no compacted replacement — the final, config-gated discard
// step ("prunes per config").
func (r *Recorder) pruneTier(base, nodeKey, tier string, cutoffDay time.Time) error {
	metrics, err := listMetrics(base, nodeKey, tier)
	if err != nil {
		return err
	}
	var firstErr error
	for _, metric := range metrics {
		days, err := listDayFiles(base, nodeKey, tier, metric)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, day := range days {
			if !day.Before(cutoffDay) {
				continue
			}
			if err := removeDayFile(base, nodeKey, tier, metric, day); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

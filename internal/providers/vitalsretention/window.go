// window.go — RFC-040 N2: the ONE read helper. window(metric, since,
// resolution) — nothing else. No query DSL, no extra operators; a caller
// wanting aggregation beyond what a tier's stored rows already carry
// (min/max/count alongside the mean) does client-side math over the
// returned points, per N2's explicit allowance.
package vitalsretention

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// validMetricNames is the allowlist Window() checks metric against before
// it ever reaches a filesystem path. Built once from AllMetricNames()
// (recorder.go) — the same list HandleBusEvent uses to decide what to
// record, so a query can never name a metric the recorder wouldn't have
// written in the first place.
//
// This check exists here, not only at the HTTP/MCP call sites, because this
// function — not its callers — is the actual security boundary: metric
// flows unsanitized into dayFilePath -> filepath.Join(base, nodeKey, tier,
// metric, ...), exactly like nodeKey (sanitizeNodeKey, nodekey.go) and
// resolution (validTiers, below) are already guarded at this same layer.
// Without this check a caller-supplied metric containing ".." segments
// could escape the .cog/observatory/vitals sandbox entirely.
var (
	validMetricNamesOnce sync.Once
	validMetricNames     map[string]bool
)

func isValidMetricName(metric string) bool {
	validMetricNamesOnce.Do(func() {
		names := AllMetricNames()
		validMetricNames = make(map[string]bool, len(names))
		for _, n := range names {
			validMetricNames[n] = true
		}
	})
	return validMetricNames[metric]
}

// Point is one returned sample. Min/Max/Count are populated for compacted
// tiers (5m/1h) and nil/zero for raw points, which are already
// tick-resolution singletons.
type Point struct {
	Ts    time.Time `json:"ts"`
	Value float64   `json:"value"`
	Min   *float64  `json:"min,omitempty"`
	Max   *float64  `json:"max,omitempty"`
	Count int       `json:"count,omitempty"`
}

// Window returns every recorded point for metric at or after since, read
// from exactly the named resolution tier ("raw", "5m", or "1h") — no
// resolution auto-selection, no cross-tier stitching. If the requested range
// includes days whose raw data has already aged into a coarser tier (or been
// pruned), those days simply contribute nothing at the "raw" resolution;
// re-querying at "5m" or "1h" is the caller's job, not this function's (N2:
// exactly window(metric, since, resolution), no smarter DSL underneath it).
//
// This queries the CURRENT node's local history only. RFC-040's spine is
// exactly window(metric, since, resolution) with no node argument — fleet-
// wide/cross-node merge (reading a BEP-synced peer's files under the same
// base dir) is deliberately out of scope for v1: it is exactly the kind of
// query surface growth N2 warns against building before it's asked for. A
// caller wanting another node's history can, today, point at that node's own
// kernel; a fleet-aggregating query is a future RFC's justification to make,
// not this one's default.
func (r *Recorder) Window(metric string, since time.Time, resolution string) ([]Point, error) {
	if metric == "" {
		return nil, fmt.Errorf("vitals-retention: metric is required")
	}
	if !isValidMetricName(metric) {
		return nil, fmt.Errorf("vitals-retention: unknown metric %q (see AllMetricNames)", metric)
	}
	if !validTiers[resolution] {
		return nil, fmt.Errorf("vitals-retention: unknown resolution %q (want raw, 5m, or 1h)", resolution)
	}

	base, err := r.baseDir()
	if err != nil {
		return nil, err
	}
	nodeKey := currentNodeKey()

	since = since.UTC()
	now := time.Now().UTC()

	var points []Point
	for d := since.Truncate(24 * time.Hour); !d.After(now); d = d.AddDate(0, 0, 1) {
		path := dayFilePath(base, nodeKey, resolution, metric, d)
		rows, err := readDayFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		for _, rw := range rows {
			ts, err := rw.parseTs()
			if err != nil {
				continue
			}
			if ts.Before(since) {
				continue
			}
			points = append(points, Point{
				Ts:    ts,
				Value: rw.V,
				Min:   rw.Min,
				Max:   rw.Max,
				Count: rw.N,
			})
		}
	}

	sort.Slice(points, func(i, j int) bool { return points[i].Ts.Before(points[j].Ts) })
	return points, nil
}

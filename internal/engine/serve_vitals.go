// serve_vitals.go — GET /v1/vitals (RFC-040 S2).
//
// The retained-history counterpart to S1's GET /metrics (serve_metrics.go):
// where /metrics renders only the CURRENT tick's snapshot, /v1/vitals reads
// the vitals-retention recorder's on-disk history via the one query helper
// RFC-040 N2 allows — window(metric, since, resolution) — nothing else. No
// query DSL, no aggregation operators beyond what the stored rows already
// carry (min/max/count for compacted tiers).
//
// This handler is a thin HTTP wrapper: all the actual reading lives in
// internal/providers/vitalsretention (a leaf package this file imports
// directly, same as internal/providers/pin/selfupdate elsewhere in this
// package — see ADR-085's leaf-package discipline).
package engine

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/myrgic/cogos/internal/providers/vitalsretention"
)

// vitalsWindowResponse is the GET /v1/vitals response shape.
type vitalsWindowResponse struct {
	Metric     string                  `json:"metric"`
	Since      string                  `json:"since"`
	Resolution string                  `json:"resolution"`
	Points     []vitalsretention.Point `json:"points"`
}

// handleVitals serves GET /v1/vitals?metric=&since=&resolution=. Required:
// metric, since (RFC3339 or duration shorthand like "5m"/"24h" — subtracted
// from now), resolution (one of raw, 5m, 1h). Missing/invalid params are a
// 400; a metric/window with no recorded data is a 200 with an empty points
// array (RFC-040: absent history is not an error — see
// vitalsretention.Window's doc).
func (s *Server) handleVitals(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	q := r.URL.Query()

	metric := strings.TrimSpace(q.Get("metric"))
	if metric == "" {
		writeVitalsError(w, http.StatusBadRequest, "metric is required")
		return
	}

	sinceRaw := strings.TrimSpace(q.Get("since"))
	if sinceRaw == "" {
		writeVitalsError(w, http.StatusBadRequest, "since is required (RFC3339 timestamp or duration shorthand like '24h')")
		return
	}
	since, err := parseTimeOrDuration(sinceRaw)
	if err != nil {
		writeVitalsError(w, http.StatusBadRequest, "invalid since: "+err.Error())
		return
	}

	resolution := strings.TrimSpace(q.Get("resolution"))
	if resolution == "" {
		writeVitalsError(w, http.StatusBadRequest, "resolution is required (raw, 5m, or 1h)")
		return
	}

	points, err := vitalsretention.Window(metric, since, resolution)
	if err != nil {
		writeVitalsError(w, http.StatusBadRequest, err.Error())
		return
	}
	if points == nil {
		points = []vitalsretention.Point{}
	}

	_ = json.NewEncoder(w).Encode(vitalsWindowResponse{
		Metric:     metric,
		Since:      since.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		Resolution: resolution,
		Points:     points,
	})
}

func writeVitalsError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

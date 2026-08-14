// serve_queue.go — GET /v1/queue: a live snapshot of every kernel-owned
// local-backend FIFO queue (#556). Registered in serve.go's main route
// table, not serve_compat.go — that file is explicitly the deprecated
// v2-migration layer, the wrong home for a new first-class observable.
package engine

import (
	"encoding/json"
	"net/http"
)

// queueSnapshotResponse is the GET /v1/queue response body: one entry per
// registered backend queue. An empty backendQueues registry (no local
// OpenAI-compat backends configured, or none have handled a request yet —
// the registry populates at router-construction time in makeProvider, so in
// practice it's populated whenever any local backend is configured at all)
// yields an empty-but-valid "backends": [] rather than 404/500.
type queueSnapshotResponse struct {
	Backends []queueSnapshot `json:"backends"`
}

// handleQueue serves GET /v1/queue: per-backend concurrency/in-flight/
// waiting/oldest-wait-ms plus a bounded, best-effort list of waiting callers
// (attribution, position, waiting_ms) — a live diagnostic read, not a
// paginated resource. See provider_queue.go's queueSnapshot/Snapshot.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	resp := queueSnapshotResponse{Backends: allBackendQueueSnapshots()}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

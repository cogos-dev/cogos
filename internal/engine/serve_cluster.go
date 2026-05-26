// serve_cluster.go — GET /v1/cluster/status
//
// Returns the current BEP cluster engine status when cluster.enabled=true,
// or a clean {"enabled":false} when the cluster subsystem is dark (the
// shipped default).
//
// The endpoint is always registered regardless of cluster configuration so
// callers can probe it without knowing whether the cluster was compiled in.
// Dark default guarantee: when cluster.enabled=false the Server.bepEngine
// field is nil, no engine state is accessed, and the response is identical
// to what existed before S2 landed.
package engine

import (
	"encoding/json"
	"net/http"
)

// handleClusterStatus serves GET /v1/cluster/status.
//
//	200 + {"enabled":false}              — cluster.enabled=false (default)
//	200 + bep.EngineStatus + enabled:true — cluster is running
func (s *Server) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if s.bepEngine == nil {
		// Dark path — cluster subsystem was not started.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled": false,
		})
		return
	}

	// Enabled path — return live engine status augmented with enabled:true.
	status := s.bepEngine.Status()

	type clusterStatusResponse struct {
		Enabled    bool   `json:"enabled"`
		Running    bool   `json:"running"`
		DeviceID   string `json:"device_id"`
		ListenAddr string `json:"listen_addr"`
		PeerCount  int    `json:"peer_count"`
		Peers      any    `json:"peers"`
	}

	_ = json.NewEncoder(w).Encode(clusterStatusResponse{
		Enabled:    true,
		Running:    status.Running,
		DeviceID:   status.DeviceID,
		ListenAddr: status.ListenAddr,
		PeerCount:  status.PeerCount,
		Peers:      status.Peers,
	})
}

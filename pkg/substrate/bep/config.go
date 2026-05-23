// config.go — BEP configuration and status types.
//
// These types are used by both the kernel engine and external consumers.
// Extracted from apps/cogos/bep_provider.go (BEPPeer, BEPConfig, BEPSyncStatus)
// and bep_engine.go (EngineStatus, PeerStatusSummary).

package bep

import "time"

// Peer represents a known peer node in the BEP cluster.
type Peer struct {
	DeviceID string    `json:"deviceId" yaml:"deviceId"`
	Address  string    `json:"address" yaml:"address"`     // host:port or tailscale address
	Name     string    `json:"name" yaml:"name"`
	Trusted  bool      `json:"trusted" yaml:"trusted"`
	LastSeen time.Time `json:"lastSeen,omitempty" yaml:"lastSeen,omitempty"`
}

// Config holds cluster configuration loaded from .cog/config/cluster.yaml.
type Config struct {
	Enabled    bool   `yaml:"enabled"`
	DeviceID   string `yaml:"deviceId,omitempty"`   // this node's ID
	NodeName   string `yaml:"nodeName,omitempty"`   // human-readable node name
	ListenPort int    `yaml:"listenPort,omitempty"` // BEP listen port (default 22000)
	CertDir    string `yaml:"certDir,omitempty"`    // TLS cert directory (default ~/.cog/etc)
	Peers      []Peer `yaml:"peers,omitempty"`
	SyncDirs   []string `yaml:"syncDirs,omitempty"` // directories to sync
	Discovery  string `yaml:"discovery,omitempty"`   // "static", "tailscale", "mdns"
}

// SyncStatus returns current sync state for the BEP provider.
type SyncStatus struct {
	Enabled   bool      `json:"enabled"`
	DeviceID  string    `json:"deviceId"`
	PeerCount int       `json:"peerCount"`
	WatchDir  string    `json:"watchDir"`
	LastSync  time.Time `json:"lastSync,omitempty"`
}

// EngineStatus holds the current engine status for CLI/API.
type EngineStatus struct {
	Running    bool                `json:"running"`
	DeviceID   string              `json:"device_id"`
	ListenAddr string              `json:"listen_addr"`
	PeerCount  int                 `json:"peer_count"`
	Peers      []PeerStatusSummary `json:"peers"`
}

// PeerStatusSummary holds status info for a single connected peer.
type PeerStatusSummary struct {
	DeviceID  string `json:"device_id"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Connected bool   `json:"connected"`
}

// ReceivedEvent represents a sync event from a remote peer.
type ReceivedEvent struct {
	PeerID    string    `json:"peerId"`
	Filename  string    `json:"filename"`
	Action    string    `json:"action"` // "create", "update", "delete"
	Timestamp time.Time `json:"timestamp"`
}

// config.go — BEP configuration and status types.
//
// These types are used by both the kernel engine and external consumers.
// Extracted from apps/cogos/bep_provider.go (BEPPeer, BEPConfig, BEPSyncStatus)
// and bep_engine.go (EngineStatus, PeerStatusSummary).

package bep

import "time"

// DefaultListenPort is the BEP listen port assigned to an enabled node that
// does not specify one in cluster.yaml.
//
// It is deliberately NOT 22000. BEP is Syncthing-derived, and 22000 is
// Syncthing's canonical sync port — so inheriting it collided out of the box
// for anyone already running personal Syncthing (observed during a second-
// node bring-up, #336). 6932 sits one above the kernel daemon's HTTP port
// (6931 = ln(2)×10⁴) and is clear of the Syncthing range (22000/21027/22067).
//
// A node can still bind 22000 explicitly via `listenPort: 22000` if it wants
// to share Syncthing's port deliberately. Per-node port negotiation/discovery
// (rather than a fixed default) is left as follow-up.
const DefaultListenPort = 6932

// Peer represents a known peer node in the BEP cluster.
type Peer struct {
	DeviceID string `json:"deviceId" yaml:"deviceId"`
	Address  string `json:"address" yaml:"address"` // host:port or tailscale address
	Name     string `json:"name" yaml:"name"`
	Trusted  bool   `json:"trusted" yaml:"trusted"`

	// NodeIdentityHash is the sha256 digest ("sha256:<hex>") of the peer's
	// sealed node-identity cogdoc self_hash. It is structurally
	// audit/provenance only, not a step toward a future trust binding: it's
	// a content digest of a document, with no key material and no BEP
	// handshake step that presents anything derived from it, so it cannot
	// support a challenge-response and rotates silently whenever the source
	// cogdoc is re-sealed. Today's (and any near-term) trust decision is
	// DeviceID+Trusted (TLS client-cert match) — see RFC-036 §11.5's
	// "neither is in the trust path" note. A real deviceId-to-L1-identity
	// binding, if built, would need the peer's L1 NodeID or a signature
	// over it, not this hash; that is a distinct, larger piece of work
	// (constellation L1 import, ADR-099), not this field maturing.
	//
	// Note this is a second, independent seam from #474 (RFC-036 Seam B),
	// which already anchors this kernel's own process.NodeID to its BEP
	// DeviceID on mint (internal/engine/process.go). That anchors *this
	// node's* kernel identity to its own device cert; it says nothing about
	// a *peer's* identity, which is what this field carries provenance for.
	//
	// Promotes what was previously a comment-only convention in
	// cluster.yaml into schema so it round-trips instead of relying on an
	// operator to keep prose in sync (myrgic/cogos#536).
	NodeIdentityHash string `json:"nodeIdentityHash,omitempty" yaml:"nodeIdentityHash,omitempty"`

	LastSeen time.Time `json:"lastSeen,omitempty" yaml:"lastSeen,omitempty"`
}

// Config holds cluster configuration loaded from .cog/config/cluster.yaml.
type Config struct {
	Enabled    bool     `yaml:"enabled"`
	DeviceID   string   `yaml:"deviceId,omitempty"`   // this node's ID
	NodeName   string   `yaml:"nodeName,omitempty"`   // human-readable node name
	ListenPort int      `yaml:"listenPort,omitempty"` // BEP listen port (default DefaultListenPort; 0 = OS-assigned random)
	CertDir    string   `yaml:"certDir,omitempty"`    // TLS cert directory (default ~/.cog/etc)
	Peers      []Peer   `yaml:"peers,omitempty"`
	SyncDirs   []string `yaml:"syncDirs,omitempty"`  // directories to sync
	Discovery  string   `yaml:"discovery,omitempty"` // "static", "tailscale", "mdns"
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

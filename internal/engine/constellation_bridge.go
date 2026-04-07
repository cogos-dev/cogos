package engine

import (
	"context"
	"time"
)

// KernelHeartbeatPayload captures the kernel state exported to the constellation bridge.
type KernelHeartbeatPayload struct {
	ProcessState         string    `json:"process_state"`
	FieldSize            int       `json:"field_size"`
	CoherenceFingerprint string    `json:"coherence_fingerprint"`
	NucleusFingerprint   string    `json:"nucleus_fingerprint"`
	LedgerHead           string    `json:"ledger_head,omitempty"`
	Timestamp            time.Time `json:"timestamp"`
}

// HeartbeatReceipt summarizes the result of a bridge heartbeat emission.
type HeartbeatReceipt struct {
	Hash      string    `json:"hash,omitempty"`
	Timestamp time.Time `json:"timestamp,omitempty"`
	PeersSent int       `json:"peers_sent"`
}

// ConstellationBridge defines the kernel-side integration point for constellation.
type ConstellationBridge interface {
	EmitHeartbeat(payload KernelHeartbeatPayload) (HeartbeatReceipt, error)
	TrustSnapshot() ConstellationTrustSnapshot
	Start(ctx context.Context) error
	Stop()
}

// ConstellationTrustSnapshot summarizes current local and peer trust state.
type ConstellationTrustSnapshot struct {
	SelfCoherencePass    bool      `json:"self_coherence_pass"`
	SelfTrustScore       float64   `json:"self_trust_score"`
	PeerTrustMean        float64   `json:"peer_trust_mean"`
	PeerCount            int       `json:"peer_count"`
	TrustedPeerCount     int       `json:"trusted_peer_count"`
	ConstellationHealthy bool      `json:"constellation_healthy"`
	Timestamp            time.Time `json:"timestamp"`
}

// NilBridge provides neutral standalone-mode behavior when no constellation is configured.
type NilBridge struct{}

func (NilBridge) EmitHeartbeat(KernelHeartbeatPayload) (HeartbeatReceipt, error) {
	return HeartbeatReceipt{}, nil
}

func (NilBridge) TrustSnapshot() ConstellationTrustSnapshot {
	return ConstellationTrustSnapshot{
		SelfCoherencePass:    true,
		SelfTrustScore:       1.0,
		PeerTrustMean:        0.0,
		PeerCount:            0,
		TrustedPeerCount:     0,
		ConstellationHealthy: true,
		Timestamp:            time.Now().UTC(),
	}
}

func (NilBridge) Start(context.Context) error {
	return nil
}

func (NilBridge) Stop() {}

func (p *Process) constellationBridge() ConstellationBridge {
	if p == nil || p.bridge == nil {
		return NilBridge{}
	}
	return p.bridge
}

func (p *Process) currentLedgerHead() string {
	if p == nil || p.cfg == nil || p.sessionID == "" {
		return ""
	}
	last, err := GetLastEvent(p.cfg.WorkspaceRoot, p.sessionID)
	if err != nil || last == nil {
		return ""
	}
	return last.Metadata.Hash
}

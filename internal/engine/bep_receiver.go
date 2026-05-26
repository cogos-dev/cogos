// bep_receiver.go — Remote agent CRD receiver for the BEP sync provider.
// Extracted from root package main into internal/engine as Phase 2 S1.
//
// Handles the receiving side of agent CRD synchronization: validates incoming
// CRDs, writes them locally via atomic file operations, and records events.

package engine

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	bep "github.com/myrgic/cogos/pkg/substrate/bep"
)

// ─── Receiver state ──────────────────────────────────────────────────────────────

// receiverState holds the event history ring buffer.
// Embedded in BEPProvider; guarded by its own mutex.
type receiverState struct {
	history    []bep.ReceivedEvent
	historyMu  sync.Mutex
	maxHistory int
}

// ReceiverMaxHistory is the maximum number of received CRD events retained in
// the ring buffer. Exported so tests in other packages can reference the cap.
const ReceiverMaxHistory = 100

// receiverMaxHistory is the internal alias used throughout this file.
const receiverMaxHistory = ReceiverMaxHistory

// ─── Local CRD validation struct ─────────────────────────────────────────────────

// agentCRDValidation is a minimal struct for validating incoming agent CRD YAML.
// Only the fields needed for validation are declared; full AgentCRD is in root package.
type agentCRDValidation struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
}

// ─── Receive Operations ─────────────────────────────────────────────────────────

// ReceiveAgentCRD handles an agent CRD received from a remote peer.
// Validates the CRD, writes it to the local definitions directory via an
// atomic write (tmp + rename), and records the event for observability.
func (p *BEPProvider) ReceiveAgentCRD(peerID string, filename string, data []byte) error {
	// 1. Validate filename: must be *.agent.yaml
	if !bep.IsAgentCRDFile(filename) {
		return fmt.Errorf("invalid agent CRD filename %q: must end in .agent.yaml", filename)
	}

	// Reject path traversal attempts.
	if strings.Contains(filename, "/") || strings.Contains(filename, "\\") || filename != filepath.Base(filename) {
		return fmt.Errorf("invalid agent CRD filename %q: must be a plain filename, not a path", filename)
	}

	// 2. Parse YAML to verify it's a valid AgentCRD.
	var crd agentCRDValidation
	if err := yaml.Unmarshal(data, &crd); err != nil {
		return fmt.Errorf("invalid agent CRD from peer %s: YAML parse error: %w", peerID, err)
	}
	if crd.APIVersion != "cog.os/v1alpha1" {
		return fmt.Errorf("invalid agent CRD from peer %s: unexpected apiVersion %q (want cog.os/v1alpha1)", peerID, crd.APIVersion)
	}
	if crd.Kind != "Agent" {
		return fmt.Errorf("invalid agent CRD from peer %s: unexpected kind %q (want Agent)", peerID, crd.Kind)
	}
	if crd.Metadata.Name == "" {
		return fmt.Errorf("invalid agent CRD from peer %s: metadata.name is required", peerID)
	}

	// 3. Atomic write: write to .tmp, then rename.
	destPath := filepath.Join(p.watchDir, filename)
	tmpPath := destPath + ".tmp"

	if err := os.MkdirAll(p.watchDir, 0755); err != nil {
		return fmt.Errorf("ensure watch dir: %w", err)
	}

	// Determine action before overwriting.
	action := "create"
	if _, err := os.Stat(destPath); err == nil {
		action = "update"
	}

	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		// Clean up the tmp file on rename failure.
		os.Remove(tmpPath)
		return fmt.Errorf("atomic rename: %w", err)
	}

	// 4. Log receipt.
	log.Printf("[BEP] Received agent CRD %s from peer %s", filename, peerID)

	// 5. Record event.
	p.recordEvent(bep.ReceivedEvent{
		PeerID:    peerID,
		Filename:  filename,
		Action:    action,
		Timestamp: time.Now(),
	})

	return nil
}

// RemoveAgentCRD handles deletion of an agent CRD from a remote peer.
func (p *BEPProvider) RemoveAgentCRD(peerID string, filename string) error {
	// 1. Validate filename.
	if !bep.IsAgentCRDFile(filename) {
		return fmt.Errorf("invalid agent CRD filename %q: must end in .agent.yaml", filename)
	}

	if strings.Contains(filename, "/") || strings.Contains(filename, "\\") || filename != filepath.Base(filename) {
		return fmt.Errorf("invalid agent CRD filename %q: must be a plain filename, not a path", filename)
	}

	// 2. Remove file from watch directory.
	target := filepath.Join(p.watchDir, filename)
	err := os.Remove(target)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove agent CRD %q: %w", filename, err)
	}

	// 3. Log deletion.
	if os.IsNotExist(err) {
		log.Printf("[BEP] Remove agent CRD %s from peer %s (already absent)", filename, peerID)
	} else {
		log.Printf("[BEP] Removed agent CRD %s from peer %s", filename, peerID)
	}

	// 4. Record event.
	p.recordEvent(bep.ReceivedEvent{
		PeerID:    peerID,
		Filename:  filename,
		Action:    "delete",
		Timestamp: time.Now(),
	})

	return nil
}

// ─── Event History ──────────────────────────────────────────────────────────────

// History returns recent sync events for observability.
func (p *BEPProvider) History() []bep.ReceivedEvent {
	p.receiver.historyMu.Lock()
	defer p.receiver.historyMu.Unlock()

	out := make([]bep.ReceivedEvent, len(p.receiver.history))
	copy(out, p.receiver.history)
	return out
}

// recordEvent adds an event to the ring buffer, evicting the oldest entry
// when the buffer is full.
func (p *BEPProvider) recordEvent(evt bep.ReceivedEvent) {
	p.receiver.historyMu.Lock()
	defer p.receiver.historyMu.Unlock()

	if len(p.receiver.history) >= p.receiver.maxHistory {
		copy(p.receiver.history, p.receiver.history[1:])
		p.receiver.history[len(p.receiver.history)-1] = evt
	} else {
		p.receiver.history = append(p.receiver.history, evt)
	}
}

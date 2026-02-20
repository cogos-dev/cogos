package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Multi-agent coordination primitives
//
// This module provides stigmergic coordination patterns for the cognitive workspace:
// - Claims: File-level locks for exclusive work
// - Checkpoints: Synchronization points for multi-agent coordination
// - Handoffs: Sequential coordination between agents
// - Broadcasts: One-to-many messaging
//
// Based on ant colony / pheromone trail model.

// =============================================================================
// DATA STRUCTURES
// =============================================================================

// Claim represents a file-level lock for exclusive work
type Claim struct {
	Path      string    `json:"path"`
	Agent     string    `json:"agent"`
	Reason    string    `json:"reason"`
	ClaimedAt time.Time `json:"claimed_at"`
}

// Handoff represents an agent-to-agent handoff
type Handoff struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Artifact  string    `json:"artifact"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// Broadcast represents a channel broadcast message
type Broadcast struct {
	Channel   string    `json:"channel"`
	Agent     string    `json:"agent"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// =============================================================================
// CLAIMS - File-level coordination
// =============================================================================

// pathToClaim converts a file path to a claim filename
func pathToClaim(path string) string {
	// Replace / with _ for filesystem safety
	return strings.ReplaceAll(path, "/", "_") + ".claim"
}

// CreateClaim acquires a file lock for exclusive work
func CreateClaim(workspaceRoot, path, reason string) error {
	agent := getAgentID()
	claimDir := filepath.Join(workspaceRoot, ".cog", "claims")
	if err := os.MkdirAll(claimDir, 0755); err != nil {
		return fmt.Errorf("failed to create claims directory: %w", err)
	}

	claimFile := filepath.Join(claimDir, pathToClaim(path))

	// Check if already claimed
	if existing, err := ReadClaim(workspaceRoot, path); err == nil {
		if existing.Agent != agent {
			return fmt.Errorf("already claimed by: %s", existing.Agent)
		}
		// Same agent re-claiming - update timestamp
	}

	// Write claim
	claim := Claim{
		Path:      path,
		Agent:     agent,
		Reason:    reason,
		ClaimedAt: time.Now(),
	}

	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal claim: %w", err)
	}

	if err := os.WriteFile(claimFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write claim: %w", err)
	}

	fmt.Printf("Claimed: %s\n", PathToURI(workspaceRoot, path))
	return nil
}

// ReleaseClaim releases a file lock
func ReleaseClaim(workspaceRoot, path string) error {
	claimFile := filepath.Join(workspaceRoot, ".cog", "claims", pathToClaim(path))

	if _, err := os.Stat(claimFile); os.IsNotExist(err) {
		// Already released - no error
		return nil
	}

	if err := os.Remove(claimFile); err != nil {
		return fmt.Errorf("failed to release claim: %w", err)
	}

	fmt.Printf("Released: %s\n", PathToURI(workspaceRoot, path))
	return nil
}

// IsClaimed checks if a path is claimed
func IsClaimed(workspaceRoot, path string) bool {
	claimFile := filepath.Join(workspaceRoot, ".cog", "claims", pathToClaim(path))
	_, err := os.Stat(claimFile)
	return err == nil
}

// ReadClaim reads the claim for a path
func ReadClaim(workspaceRoot, path string) (*Claim, error) {
	claimFile := filepath.Join(workspaceRoot, ".cog", "claims", pathToClaim(path))

	data, err := os.ReadFile(claimFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no claim for path: %s", path)
		}
		return nil, fmt.Errorf("failed to read claim: %w", err)
	}

	var claim Claim
	if err := json.Unmarshal(data, &claim); err != nil {
		return nil, fmt.Errorf("failed to parse claim: %w", err)
	}

	return &claim, nil
}

// ClaimOwner returns the agent that owns the claim
func ClaimOwner(workspaceRoot, path string) (string, error) {
	claim, err := ReadClaim(workspaceRoot, path)
	if err != nil {
		return "", err
	}
	return claim.Agent, nil
}

// ListClaims returns all active claims
func ListClaims(workspaceRoot string) ([]Claim, error) {
	claimDir := filepath.Join(workspaceRoot, ".cog", "claims")

	entries, err := os.ReadDir(claimDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No claims yet
		}
		return nil, fmt.Errorf("failed to read claims directory: %w", err)
	}

	var claims []Claim
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".claim") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(claimDir, entry.Name()))
		if err != nil {
			continue // Skip unreadable claims
		}

		var claim Claim
		if err := json.Unmarshal(data, &claim); err != nil {
			continue // Skip malformed claims
		}

		claims = append(claims, claim)
	}

	return claims, nil
}

// =============================================================================
// CHECKPOINTS - Synchronization points
// =============================================================================

// CreateCheckpoint creates a checkpoint signal for the current agent
// Uses the signal system (sdk/signals.go) for stigmergic coordination
func CreateCheckpoint(workspaceRoot, name string) error {
	agent := getAgentID()

	// Create signal directory
	signalDir := filepath.Join(workspaceRoot, ".cog", "signals", "checkpoint", name)
	if err := os.MkdirAll(signalDir, 0755); err != nil {
		return fmt.Errorf("failed to create checkpoint directory: %w", err)
	}

	// Write timestamp as signal
	signalFile := filepath.Join(signalDir, agent)
	timestamp := time.Now().Format(time.RFC3339)

	if err := os.WriteFile(signalFile, []byte(timestamp), 0644); err != nil {
		return fmt.Errorf("failed to write checkpoint signal: %w", err)
	}

	fmt.Printf("Checkpoint created: %s/%s\n", name, agent)
	return nil
}

// WaitCheckpoint waits for all agents to reach a checkpoint
func WaitCheckpoint(workspaceRoot, name string, agents []string, timeout time.Duration) error {
	start := time.Now()
	signalDir := filepath.Join(workspaceRoot, ".cog", "signals", "checkpoint", name)

	for {
		allReady := true
		for _, agent := range agents {
			signalFile := filepath.Join(signalDir, agent)
			if _, err := os.Stat(signalFile); os.IsNotExist(err) {
				allReady = false
				break
			}
		}

		if allReady {
			fmt.Printf("Checkpoint reached: %s (all agents ready)\n", name)
			return nil
		}

		if time.Since(start) > timeout {
			return fmt.Errorf("checkpoint timeout: %s (waited %v)", name, timeout)
		}

		time.Sleep(2 * time.Second)
	}
}

// =============================================================================
// HANDOFFS - Sequential coordination
// =============================================================================

// CreateHandoff creates a handoff to another agent
func CreateHandoff(workspaceRoot, toAgent, artifact, message string) error {
	fromAgent := getAgentID()
	handoffDir := filepath.Join(workspaceRoot, ".cog", "handoffs")

	if err := os.MkdirAll(handoffDir, 0755); err != nil {
		return fmt.Errorf("failed to create handoffs directory: %w", err)
	}

	handoff := Handoff{
		From:      fromAgent,
		To:        toAgent,
		Artifact:  artifact,
		Message:   message,
		Timestamp: time.Now(),
	}

	data, err := json.MarshalIndent(handoff, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal handoff: %w", err)
	}

	filename := fmt.Sprintf("%s-%d.json", toAgent, time.Now().Unix())
	handoffFile := filepath.Join(handoffDir, filename)

	if err := os.WriteFile(handoffFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write handoff: %w", err)
	}

	fmt.Printf("Handoff created: %s -> %s (%s)\n", fromAgent, toAgent, artifact)
	return nil
}

// ListHandoffs returns pending handoffs for an agent
func ListHandoffs(workspaceRoot, agent string) ([]Handoff, error) {
	if agent == "" {
		agent = getAgentID()
	}

	handoffDir := filepath.Join(workspaceRoot, ".cog", "handoffs")
	entries, err := os.ReadDir(handoffDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No handoffs yet
		}
		return nil, fmt.Errorf("failed to read handoffs directory: %w", err)
	}

	prefix := agent + "-"
	var handoffs []Handoff

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(handoffDir, entry.Name()))
		if err != nil {
			continue // Skip unreadable handoffs
		}

		var handoff Handoff
		if err := json.Unmarshal(data, &handoff); err != nil {
			continue // Skip malformed handoffs
		}

		handoffs = append(handoffs, handoff)
	}

	return handoffs, nil
}

// AcceptHandoff archives a handoff (marks it as accepted)
func AcceptHandoff(workspaceRoot, handoffFile string) error {
	archiveDir := filepath.Join(workspaceRoot, ".cog", "handoffs", "archive")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return fmt.Errorf("failed to create archive directory: %w", err)
	}

	sourcePath := filepath.Join(workspaceRoot, ".cog", "handoffs", handoffFile)
	destPath := filepath.Join(archiveDir, handoffFile)

	if err := os.Rename(sourcePath, destPath); err != nil {
		return fmt.Errorf("failed to archive handoff: %w", err)
	}

	fmt.Printf("Handoff accepted: %s\n", handoffFile)
	return nil
}

// =============================================================================
// BROADCASTS - One-to-many signals
// =============================================================================

// CreateBroadcast broadcasts a message to a channel
func CreateBroadcast(workspaceRoot, channel, message string) error {
	agent := getAgentID()
	broadcastDir := filepath.Join(workspaceRoot, ".cog", "broadcasts", channel)

	if err := os.MkdirAll(broadcastDir, 0755); err != nil {
		return fmt.Errorf("failed to create broadcast directory: %w", err)
	}

	broadcast := Broadcast{
		Channel:   channel,
		Agent:     agent,
		Message:   message,
		Timestamp: time.Now(),
	}

	data, err := json.MarshalIndent(broadcast, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal broadcast: %w", err)
	}

	filename := fmt.Sprintf("%s-%d.msg", agent, time.Now().Unix())
	broadcastFile := filepath.Join(broadcastDir, filename)

	if err := os.WriteFile(broadcastFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write broadcast: %w", err)
	}

	fmt.Printf("Broadcast sent: [%s] %s\n", channel, message)
	return nil
}

// ListBroadcasts returns broadcasts on a channel since a time window
func ListBroadcasts(workspaceRoot, channel string, since time.Duration) ([]Broadcast, error) {
	broadcastDir := filepath.Join(workspaceRoot, ".cog", "broadcasts", channel)
	entries, err := os.ReadDir(broadcastDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No broadcasts yet
		}
		return nil, fmt.Errorf("failed to read broadcasts directory: %w", err)
	}

	cutoff := time.Now().Add(-since)
	var broadcasts []Broadcast

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".msg") {
			continue
		}

		// Check file modification time
		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			continue // Too old
		}

		data, err := os.ReadFile(filepath.Join(broadcastDir, entry.Name()))
		if err != nil {
			continue // Skip unreadable broadcasts
		}

		var broadcast Broadcast
		if err := json.Unmarshal(data, &broadcast); err != nil {
			continue // Skip malformed broadcasts
		}

		broadcasts = append(broadcasts, broadcast)
	}

	return broadcasts, nil
}

// =============================================================================
// UTILITIES
// =============================================================================

// getAgentID returns the current agent identifier
func getAgentID() string {
	if agent := os.Getenv("COG_AGENT_ID"); agent != "" {
		return agent
	}
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	return "root"
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SessionTracking represents session tracking information
type SessionTracking struct {
	SessionID     string   `json:"sessionId"`
	Branch        string   `json:"branch"`
	StartedAt     string   `json:"startedAt"`
	EndedAt       *string  `json:"endedAt,omitempty"`
	Status        *string  `json:"status,omitempty"`
	RootAgent     string   `json:"rootAgent"`
	SpawnedAgents []string `json:"spawnedAgents"`
	ActiveAgents  []string `json:"activeAgents"`
	ReapedAgents  []string `json:"reapedAgents"`
}

// getSessionFile returns the path to the session tracking file
func getSessionFile(root string) string {
	return filepath.Join(root, ".cog", "status", ".session")
}

// getSessionArchiveDir returns the path to the session archive directory
func getSessionArchiveDir(root string) string {
	return filepath.Join(root, ".cog", "status", "archive", "sessions")
}

// getCurrentBranch returns the current git branch name
func getCurrentBranch(root string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

// sessionInit initializes session tracking
func sessionInit(root string, sessionID string) error {
	if sessionID == "" {
		sessionID = getCurrentBranch(root)
	}

	sessionFile := getSessionFile(root)

	// Check if session already exists
	if _, err := os.Stat(sessionFile); err == nil {
		// Read existing session
		data, err := os.ReadFile(sessionFile)
		if err == nil {
			var existing SessionInfo
			if err := json.Unmarshal(data, &existing); err == nil {
				if existing.SessionID == sessionID {
					// Already initialized
					return nil
				}
			}
		}
	}

	// Create session directory
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0755); err != nil {
		return err
	}

	// Create new session
	session := SessionTracking{
		SessionID:     sessionID,
		Branch:        getCurrentBranch(root),
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		RootAgent:     "root",
		SpawnedAgents: []string{},
		ActiveAgents:  []string{},
		ReapedAgents:  []string{},
	}

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(sessionFile, data, 0644); err != nil {
		return err
	}

	fmt.Printf("Session initialized: %s\n", sessionID)
	return nil
}

// sessionTrackSpawn tracks agent spawn in session
func sessionTrackSpawn(root string, agentID string) error {
	sessionFile := getSessionFile(root)

	// Initialize if needed
	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		if err := sessionInit(root, ""); err != nil {
			return err
		}
	}

	// Read session
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return err
	}

	var session SessionTracking
	if err := json.Unmarshal(data, &session); err != nil {
		return err
	}

	// Add agent to spawned and active lists
	session.SpawnedAgents = append(session.SpawnedAgents, agentID)
	session.ActiveAgents = append(session.ActiveAgents, agentID)

	// Write back
	data, err = json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(sessionFile, data, 0644)
}

// sessionTrackReap tracks agent reap in session
func sessionTrackReap(root string, agentID string) error {
	sessionFile := getSessionFile(root)

	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		// No session file, nothing to do
		return nil
	}

	// Read session
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return err
	}

	var session SessionTracking
	if err := json.Unmarshal(data, &session); err != nil {
		return err
	}

	// Remove from active, add to reaped
	newActive := []string{}
	for _, id := range session.ActiveAgents {
		if id != agentID {
			newActive = append(newActive, id)
		}
	}
	session.ActiveAgents = newActive
	session.ReapedAgents = append(session.ReapedAgents, agentID)

	// Write back
	data, err = json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(sessionFile, data, 0644)
}

// sessionAllReaped checks if all agents are reaped
func sessionAllReaped(root string) (bool, error) {
	sessionFile := getSessionFile(root)

	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		// No session file, consider all reaped
		return true, nil
	}

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return false, err
	}

	var session SessionTracking
	if err := json.Unmarshal(data, &session); err != nil {
		return false, err
	}

	return len(session.ActiveAgents) == 0, nil
}

// sessionActive lists active agents
func sessionActive(root string) ([]string, error) {
	sessionFile := getSessionFile(root)

	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		return []string{}, nil
	}

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return nil, err
	}

	var session SessionTracking
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}

	return session.ActiveAgents, nil
}

// sessionStatus displays current session status
func sessionStatus(root string) error {
	sessionFile := getSessionFile(root)

	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		fmt.Println("No active session")
		return nil
	}

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return err
	}

	var session SessionTracking
	if err := json.Unmarshal(data, &session); err != nil {
		return err
	}

	fmt.Printf("Session: %s\n", session.SessionID)
	fmt.Printf("Started: %s\n", session.StartedAt)
	fmt.Printf("Agents: %d spawned, %d active, %d reaped\n",
		len(session.SpawnedAgents), len(session.ActiveAgents), len(session.ReapedAgents))

	if len(session.ActiveAgents) > 0 {
		fmt.Println()
		fmt.Println("Active agents:")
		for _, agent := range session.ActiveAgents {
			fmt.Printf("  - %s\n", agent)
		}
	}

	return nil
}

// sessionEnd ends the current session with cleanup
func sessionEnd(root string, force bool) error {
	sessionFile := getSessionFile(root)

	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		fmt.Println("No active session")
		return nil
	}

	// Read session
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return err
	}

	var session SessionTracking
	if err := json.Unmarshal(data, &session); err != nil {
		return err
	}

	// Check for active agents
	if len(session.ActiveAgents) > 0 {
		if !force {
			fmt.Fprintln(os.Stderr, "Active agents must be reaped first:")
			for _, agent := range session.ActiveAgents {
				fmt.Fprintf(os.Stderr, "  - %s\n", agent)
			}
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "Use 'cog session end --force' to force reap, or reap manually.")
			return fmt.Errorf("active agents present")
		}

		// Force reap all active agents
		fmt.Println("Force reaping active agents...")
		// Note: This would need to call actual reap function
		// For now just clear the list
		session.ActiveAgents = []string{}
	}

	// Archive session
	archiveDir := getSessionArchiveDir(root)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return err
	}

	endedAt := time.Now().UTC().Format(time.RFC3339)
	status := "ended"
	session.EndedAt = &endedAt
	session.Status = &status

	timestamp := time.Now().Format("20060102-150405")
	sessionIDSafe := strings.ReplaceAll(session.SessionID, "/", "_")
	archivePath := filepath.Join(archiveDir, fmt.Sprintf("%s-%s.json", sessionIDSafe, timestamp))

	data, err = json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(archivePath, data, 0644); err != nil {
		return err
	}

	// Remove session file
	if err := os.Remove(sessionFile); err != nil {
		return err
	}

	fmt.Printf("Session ended: %s\n", session.SessionID)
	return nil
}

// validateHierarchy validates that agent hierarchy is acyclic
func validateHierarchy(root string, agentID string) error {
	visited := make(map[string]bool)
	current := agentID

	for current != "" && current != "root" && current != "null" {
		// Check for cycle
		if visited[current] {
			return fmt.Errorf("cycle detected in agent hierarchy at: %s", current)
		}
		visited[current] = true

		// Get parent
		statusFile := filepath.Join(root, ".cog", "status", current+".json")
		data, err := os.ReadFile(statusFile)
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			return err
		}

		var status map[string]interface{}
		if err := json.Unmarshal(data, &status); err != nil {
			return err
		}

		parent, ok := status["parentAgent"].(string)
		if !ok {
			break
		}
		current = parent
	}

	return nil
}

// cmdSession handles the session command
func cmdSession(args []string) error {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("no workspace found (run from workspace or use -w flag): %w", err)
	}

	subCmd := "status"
	if len(args) > 0 {
		subCmd = args[0]
		args = args[1:]
	}

	switch subCmd {
	case "init":
		sessionID := ""
		if len(args) > 0 {
			sessionID = args[0]
		}
		return sessionInit(root, sessionID)

	case "track-spawn":
		if len(args) == 0 {
			return fmt.Errorf("agent ID required")
		}
		return sessionTrackSpawn(root, args[0])

	case "track-reap":
		if len(args) == 0 {
			return fmt.Errorf("agent ID required")
		}
		return sessionTrackReap(root, args[0])

	case "all-reaped":
		reaped, err := sessionAllReaped(root)
		if err != nil {
			return err
		}
		if reaped {
			fmt.Println("true")
		} else {
			fmt.Println("false")
		}
		return nil

	case "active":
		agents, err := sessionActive(root)
		if err != nil {
			return err
		}
		for _, agent := range agents {
			fmt.Println(agent)
		}
		return nil

	case "status":
		return sessionStatus(root)

	case "end":
		force := false
		if len(args) > 0 && (args[0] == "--force" || args[0] == "true") {
			force = true
		}
		return sessionEnd(root, force)

	case "validate-hierarchy":
		if len(args) == 0 {
			return fmt.Errorf("agent ID required")
		}
		return validateHierarchy(root, args[0])

	default:
		return fmt.Errorf("unknown session command: %s", subCmd)
	}
}

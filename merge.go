// .cog/merge.go
// Branch/Merge semantics with explicit merge events and conflict detection
//
// Part of CogOS Kernel - Squad D implementation

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// === MERGE EVENT TYPES ===

// ParentHead represents a branch being merged
type ParentHead struct {
	Branch        string `json:"branch"`           // Branch name (e.g., "main", "agent/researcher-abc123")
	SessionID     string `json:"session_id"`       // Last session on this branch
	LastEventHash string `json:"last_event_hash"`  // Hash of last event in session
	CommitHash    string `json:"commit_hash"`      // Git commit hash at merge point
	TreeHash      string `json:"tree_hash"`        // Git tree hash of .cog/ directory
}

// ConflictType categorizes merge conflicts
type ConflictType string

const (
	ConflictText     ConflictType = "text"     // Same file, same lines modified differently
	ConflictTree     ConflictType = "tree"     // Same file, different content hashes
	ConflictSemantic ConflictType = "semantic" // Semantic conflict (e.g., contradictory memory)
)

// Conflict represents a detected merge conflict
type Conflict struct {
	Type         ConflictType `json:"type"`
	FilePath     string       `json:"file_path"`      // Relative path from workspace root
	BranchA      string       `json:"branch_a"`       // First branch in conflict
	BranchB      string       `json:"branch_b"`       // Second branch in conflict
	ContentHashA string       `json:"content_hash_a"` // Content hash from branch A
	ContentHashB string       `json:"content_hash_b"` // Content hash from branch B
	LineRangeA   string       `json:"line_range_a,omitempty"` // For text conflicts
	LineRangeB   string       `json:"line_range_b,omitempty"` // For text conflicts
	Description  string       `json:"description"`    // Human-readable description
}

// ResolutionMethod describes how a conflict was resolved
type ResolutionMethod string

const (
	ResolutionTakeA   ResolutionMethod = "take_a"   // Use version from branch A
	ResolutionTakeB   ResolutionMethod = "take_b"   // Use version from branch B
	ResolutionManual  ResolutionMethod = "manual"   // Manually merged content
	ResolutionMerge3  ResolutionMethod = "merge3"   // Three-way merge (base + A + B)
	ResolutionDeleted ResolutionMethod = "deleted"  // File deleted in merge
)

// Resolution represents how a conflict was resolved
type Resolution struct {
	ConflictFile     string           `json:"conflict_file"`
	ResolutionMethod ResolutionMethod `json:"resolution_method"`
	ChosenVersion    string           `json:"chosen_version,omitempty"` // Which branch if take_a/take_b
	ResolvedContent  string           `json:"resolved_content,omitempty"` // For manual/merge3
	ResolvedHash     string           `json:"resolved_hash"`    // SHA256 of resolved content
	Timestamp        time.Time        `json:"timestamp"`
	Arbiter          string           `json:"arbiter"` // Who/what resolved (user, agent, auto)
}

// MergeEvent represents a merge commit event in the ledger
type MergeEvent struct {
	ID                  string       `json:"id"`                     // Event ID
	Type                string       `json:"type"`                   // "merge.commit"
	Timestamp           time.Time    `json:"timestamp"`
	SessionID           string       `json:"session_id"`             // Session where merge happened
	ParentHeads         []ParentHead `json:"parent_heads"`           // Branches being merged (2 for MVP)
	ResolutionArtifacts []Resolution `json:"resolution_artifacts"`   // How conflicts were resolved
	MergedCommitHash    string       `json:"merged_commit_hash"`     // Git commit created by merge
	MergedTreeHash      string       `json:"merged_tree_hash"`       // Git tree hash after merge
	MergeStrategy       string       `json:"merge_strategy"`         // "explicit", "fast-forward", etc.
	ConflictsDetected   int          `json:"conflicts_detected"`     // Number of conflicts
	ConflictsResolved   int          `json:"conflicts_resolved"`     // Number resolved
}

// ConflictDetectedEvent is emitted when conflicts are found
type ConflictDetectedEvent struct {
	ID           string       `json:"id"`
	Type         string       `json:"type"` // "conflict.detected"
	Timestamp    time.Time    `json:"timestamp"`
	SessionID    string       `json:"session_id"`
	ConflictFile string       `json:"conflict_file"`
	ConflictType ConflictType `json:"conflict_type"`
	BranchA      string       `json:"branch_a"`
	BranchB      string       `json:"branch_b"`
	Description  string       `json:"description"`
}

// ConflictResolvedEvent is emitted when a conflict is resolved
type ConflictResolvedEvent struct {
	ID               string           `json:"id"`
	Type             string           `json:"type"` // "conflict.resolved"
	Timestamp        time.Time        `json:"timestamp"`
	SessionID        string           `json:"session_id"`
	ConflictFile     string           `json:"conflict_file"`
	ResolutionMethod ResolutionMethod `json:"resolution_method"`
	ChosenVersion    string           `json:"chosen_version,omitempty"`
	Arbiter          string           `json:"arbiter"`
}

// === CONFLICT DETECTION ===

// DetectConflicts analyzes two branches and returns list of conflicts
func DetectConflicts(workspaceRoot, branchA, branchB string) ([]Conflict, error) {
	var conflicts []Conflict

	// Get list of all files in .cog/ from both branches
	filesA, err := getTreeFiles(workspaceRoot, branchA, ".cog/")
	if err != nil {
		return nil, fmt.Errorf("failed to get files from %s: %w", branchA, err)
	}

	filesB, err := getTreeFiles(workspaceRoot, branchB, ".cog/")
	if err != nil {
		return nil, fmt.Errorf("failed to get files from %s: %w", branchB, err)
	}

	// Build file set for faster lookup
	fileSetA := make(map[string]string) // path -> hash
	for path, hash := range filesA {
		fileSetA[path] = hash
	}

	fileSetB := make(map[string]string)
	for path, hash := range filesB {
		fileSetB[path] = hash
	}

	// Check for conflicts: same file, different hash
	for path, hashA := range fileSetA {
		if hashB, existsInB := fileSetB[path]; existsInB {
			if hashA != hashB {
				// Tree conflict: same file, different content
				conflict := Conflict{
					Type:         ConflictTree,
					FilePath:     path,
					BranchA:      branchA,
					BranchB:      branchB,
					ContentHashA: hashA,
					ContentHashB: hashB,
					Description:  fmt.Sprintf("File modified in both branches with different content"),
				}

				// Try to detect text-level conflicts
				textConflict, err := detectTextConflict(workspaceRoot, branchA, branchB, path)
				if err == nil && textConflict != nil {
					conflict.Type = ConflictText
					conflict.LineRangeA = textConflict.LineRangeA
					conflict.LineRangeB = textConflict.LineRangeB
					conflict.Description = textConflict.Description
				}

				conflicts = append(conflicts, conflict)
			}
		}
	}

	return conflicts, nil
}

// getTreeFiles returns map of file paths to content hashes for a branch
func getTreeFiles(workspaceRoot, branch, prefix string) (map[string]string, error) {
	// Use git ls-tree to get file list
	cmd := exec.Command("git", "ls-tree", "-r", branch, prefix)
	cmd.Dir = workspaceRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree failed: %w", err)
	}

	files := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		// Format: <mode> <type> <hash>\t<path>
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		hash := parts[2]
		pathPart := strings.Join(parts[3:], " ")
		// Remove prefix from path
		relPath := strings.TrimPrefix(pathPart, prefix)
		files[relPath] = hash
	}

	return files, nil
}

// detectTextConflict performs line-by-line comparison to detect text conflicts
func detectTextConflict(workspaceRoot, branchA, branchB, filePath string) (*Conflict, error) {
	// Get file content from both branches
	contentA, err := getFileContent(workspaceRoot, branchA, ".cog/"+filePath)
	if err != nil {
		return nil, err
	}

	contentB, err := getFileContent(workspaceRoot, branchB, ".cog/"+filePath)
	if err != nil {
		return nil, err
	}

	// Simple line-based diff (not a full 3-way merge algorithm)
	linesA := strings.Split(contentA, "\n")
	linesB := strings.Split(contentB, "\n")

	// Find first differing line
	var firstDiff int = -1
	var lastDiff int = -1
	maxLen := len(linesA)
	if len(linesB) > maxLen {
		maxLen = len(linesB)
	}

	for i := 0; i < maxLen; i++ {
		lineA := ""
		lineB := ""
		if i < len(linesA) {
			lineA = linesA[i]
		}
		if i < len(linesB) {
			lineB = linesB[i]
		}
		if lineA != lineB {
			if firstDiff == -1 {
				firstDiff = i
			}
			lastDiff = i
		}
	}

	if firstDiff == -1 {
		// No text difference (shouldn't happen if hashes differ, but defensive)
		return nil, nil
	}

	return &Conflict{
		Type:         ConflictText,
		FilePath:     filePath,
		BranchA:      branchA,
		BranchB:      branchB,
		LineRangeA:   fmt.Sprintf("lines %d-%d", firstDiff+1, lastDiff+1),
		LineRangeB:   fmt.Sprintf("lines %d-%d", firstDiff+1, lastDiff+1),
		Description:  fmt.Sprintf("Text conflict: lines %d-%d differ", firstDiff+1, lastDiff+1),
	}, nil
}

// getFileContent retrieves file content from a specific branch
func getFileContent(workspaceRoot, branch, filePath string) (string, error) {
	cmd := exec.Command("git", "show", branch+":"+filePath)
	cmd.Dir = workspaceRoot
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git show failed: %w", err)
	}
	return string(output), nil
}

// === MERGE EVENT CREATION ===

// CreateMergeEvent creates a merge event after conflicts are resolved
func CreateMergeEvent(workspaceRoot, sessionID string, parentHeads []ParentHead, resolutions []Resolution) (*MergeEvent, error) {
	// MVP: Only support 2-way merge
	if len(parentHeads) != 2 {
		return nil, fmt.Errorf("MVP only supports 2-way merge (got %d parents)", len(parentHeads))
	}

	// Verify all conflicts are resolved
	conflicts, err := DetectConflicts(workspaceRoot, parentHeads[0].Branch, parentHeads[1].Branch)
	if err != nil {
		return nil, fmt.Errorf("failed to detect conflicts: %w", err)
	}

	// Check that number of resolutions matches conflicts
	if len(conflicts) != len(resolutions) {
		return nil, fmt.Errorf("conflict count mismatch: %d conflicts, %d resolutions", len(conflicts), len(resolutions))
	}

	// Get current commit hash (should be merge commit)
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workspaceRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD commit: %w", err)
	}
	commitHash := strings.TrimSpace(string(output))

	// Get tree hash
	cmd = exec.Command("git", "rev-parse", "HEAD^{tree}")
	cmd.Dir = workspaceRoot
	output, err = cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree hash: %w", err)
	}
	treeHash := strings.TrimSpace(string(output))

	// Generate event ID
	eventID := fmt.Sprintf("merge_%s_%d", sessionID[:8], time.Now().UnixNano())

	mergeEvent := &MergeEvent{
		ID:                  eventID,
		Type:                "merge.commit",
		Timestamp:           time.Now(),
		SessionID:           sessionID,
		ParentHeads:         parentHeads,
		ResolutionArtifacts: resolutions,
		MergedCommitHash:    commitHash,
		MergedTreeHash:      treeHash,
		MergeStrategy:       "explicit",
		ConflictsDetected:   len(conflicts),
		ConflictsResolved:   len(resolutions),
	}

	return mergeEvent, nil
}

// === CONFLICT RESOLUTION ===

// ResolveConflict creates a resolution for a conflict
func ResolveConflict(conflict Conflict, method ResolutionMethod, chosenVersion string, resolvedContent string, arbiter string) Resolution {
	// Hash the resolved content
	hash := sha256.Sum256([]byte(resolvedContent))
	resolvedHash := hex.EncodeToString(hash[:])

	return Resolution{
		ConflictFile:     conflict.FilePath,
		ResolutionMethod: method,
		ChosenVersion:    chosenVersion,
		ResolvedContent:  resolvedContent,
		ResolvedHash:     resolvedHash,
		Timestamp:        time.Now(),
		Arbiter:          arbiter,
	}
}

// ApplyResolution applies a resolution to the working directory
func ApplyResolution(workspaceRoot string, resolution Resolution) error {
	filePath := filepath.Join(workspaceRoot, ".cog", resolution.ConflictFile)

	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Write resolved content
	if err := os.WriteFile(filePath, []byte(resolution.ResolvedContent), 0644); err != nil {
		return fmt.Errorf("failed to write resolved file: %w", err)
	}

	return nil
}

// === EVENT LOGGING ===

// EmitConflictDetectedEvent logs a conflict detection event to the ledger
func EmitConflictDetectedEvent(ledgerDir, sessionID string, conflict Conflict) error {
	event := ConflictDetectedEvent{
		ID:           fmt.Sprintf("evt_%d", time.Now().UnixNano()),
		Type:         "conflict.detected",
		Timestamp:    time.Now(),
		SessionID:    sessionID,
		ConflictFile: conflict.FilePath,
		ConflictType: conflict.Type,
		BranchA:      conflict.BranchA,
		BranchB:      conflict.BranchB,
		Description:  conflict.Description,
	}

	return writeEventToLedger(ledgerDir, sessionID, event)
}

// EmitConflictResolvedEvent logs a conflict resolution event to the ledger
func EmitConflictResolvedEvent(ledgerDir, sessionID string, resolution Resolution) error {
	event := ConflictResolvedEvent{
		ID:               fmt.Sprintf("evt_%d", time.Now().UnixNano()),
		Type:             "conflict.resolved",
		Timestamp:        time.Now(),
		SessionID:        sessionID,
		ConflictFile:     resolution.ConflictFile,
		ResolutionMethod: resolution.ResolutionMethod,
		ChosenVersion:    resolution.ChosenVersion,
		Arbiter:          resolution.Arbiter,
	}

	return writeEventToLedger(ledgerDir, sessionID, event)
}

// EmitMergeEvent logs a merge commit event to the ledger
func EmitMergeEvent(ledgerDir string, mergeEvent *MergeEvent) error {
	return writeEventToLedger(ledgerDir, mergeEvent.SessionID, mergeEvent)
}

// writeEventToLedger appends an event to the session's events.jsonl
func writeEventToLedger(ledgerDir, sessionID string, event interface{}) error {
	sessionDir := filepath.Join(ledgerDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	eventsFile := filepath.Join(sessionDir, "events.jsonl")
	f, err := os.OpenFile(eventsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open events file: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("failed to write event: %w", err)
	}
	if _, err := f.Write([]byte("\n")); err != nil {
		return fmt.Errorf("failed to write newline: %w", err)
	}

	return nil
}

// === MERGE WORKFLOW ===

// PerformMerge executes a complete merge workflow
func PerformMerge(workspaceRoot, sessionID, branchA, branchB string) (*MergeEvent, error) {
	ledgerDir := filepath.Join(workspaceRoot, ".cog", "ledger")

	// Step 1: Detect conflicts
	conflicts, err := DetectConflicts(workspaceRoot, branchA, branchB)
	if err != nil {
		return nil, fmt.Errorf("conflict detection failed: %w", err)
	}

	// Emit conflict.detected events
	for _, conflict := range conflicts {
		if err := EmitConflictDetectedEvent(ledgerDir, sessionID, conflict); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to emit conflict.detected event: %v\n", err)
		}
	}

	if len(conflicts) > 0 {
		return nil, fmt.Errorf("merge blocked: %d conflicts detected, manual resolution required", len(conflicts))
	}

	// Step 2: No conflicts - proceed with merge
	// Get parent heads
	parentHeads := []ParentHead{
		{
			Branch:     branchA,
			SessionID:  sessionID,
			CommitHash: getCommitHash(workspaceRoot, branchA),
			TreeHash:   getTreeHash(workspaceRoot, branchA),
		},
		{
			Branch:     branchB,
			SessionID:  sessionID,
			CommitHash: getCommitHash(workspaceRoot, branchB),
			TreeHash:   getTreeHash(workspaceRoot, branchB),
		},
	}

	// Step 3: Execute git merge
	cmd := exec.Command("git", "merge", "--no-ff", branchB, "-m", fmt.Sprintf("Merge %s into %s", branchB, branchA))
	cmd.Dir = workspaceRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git merge failed: %w\nOutput: %s", err, string(output))
	}

	// Step 4: Create merge event
	mergeEvent, err := CreateMergeEvent(workspaceRoot, sessionID, parentHeads, []Resolution{})
	if err != nil {
		return nil, fmt.Errorf("failed to create merge event: %w", err)
	}

	// Step 5: Emit merge.commit event
	if err := EmitMergeEvent(ledgerDir, mergeEvent); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to emit merge.commit event: %v\n", err)
	}

	return mergeEvent, nil
}

// getCommitHash retrieves commit hash for a branch
func getCommitHash(workspaceRoot, branch string) string {
	cmd := exec.Command("git", "rev-parse", branch)
	cmd.Dir = workspaceRoot
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// getTreeHash retrieves tree hash for a branch
func getTreeHash(workspaceRoot, branch string) string {
	cmd := exec.Command("git", "rev-parse", branch+"^{tree}")
	cmd.Dir = workspaceRoot
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

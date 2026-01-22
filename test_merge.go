// .cog/test_merge.go
// Test suite for branch/merge semantics
//
// Part of CogOS Kernel - Squad D implementation

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// === TEST HELPERS ===

// setupTestRepo creates a temporary git repository for testing
func setupTestRepo(t *testing.T) string {
	tempDir := t.TempDir()

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Configure git
	exec.Command("git", "config", "user.name", "Test User").Run()
	exec.Command("git", "config", "user.email", "test@example.com").Run()

	// Create .cog directory structure
	cogDir := filepath.Join(tempDir, ".cog")
	if err := os.MkdirAll(filepath.Join(cogDir, "ledger"), 0755); err != nil {
		t.Fatalf("failed to create .cog/ledger: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cogDir, "memory"), 0755); err != nil {
		t.Fatalf("failed to create .cog/memory: %v", err)
	}

	// Create initial commit
	testFile := filepath.Join(cogDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content\n"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	cmd = exec.Command("git", "add", ".")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to add files: %v", err)
	}

	cmd = exec.Command("git", "commit", "-m", "Initial commit")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	return tempDir
}

// createBranch creates a new branch and switches to it
func createBranch(t testing.TB, repoPath, branchName string) {
	cmd := exec.Command("git", "checkout", "-b", branchName)
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create branch %s: %v", branchName, err)
	}
}

// commitFile creates a file and commits it
func commitFile(t *testing.T, repoPath, filePath, content, commitMsg string) {
	fullPath := filepath.Join(repoPath, ".cog", filePath)

	// Ensure directory exists
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	cmd := exec.Command("git", "add", ".cog/"+filePath)
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}

	cmd = exec.Command("git", "commit", "-m", commitMsg)
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}
}

// switchBranch switches to a different branch
func switchBranch(t testing.TB, repoPath, branchName string) {
	cmd := exec.Command("git", "checkout", branchName)
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to switch to branch %s: %v", branchName, err)
	}
}

// === TEST CASES ===

// TestNonConflictingMerge tests merging branches with different files
func TestNonConflictingMerge(t *testing.T) {
	repo := setupTestRepo(t)
	sessionID := "test-session-1"

	// Create branch A and add file
	createBranch(t, repo, "branch-a")
	commitFile(t, repo, "mem/file-a.md", "Content from branch A\n", "Add file-a")

	// Switch to main and create branch B
	switchBranch(t, repo, "main")
	createBranch(t, repo, "branch-b")
	commitFile(t, repo, "mem/file-b.md", "Content from branch B\n", "Add file-b")

	// Switch back to main
	switchBranch(t, repo, "main")

	// Detect conflicts (should be none)
	conflicts, err := DetectConflicts(repo, "branch-a", "branch-b")
	if err != nil {
		t.Fatalf("DetectConflicts failed: %v", err)
	}

	if len(conflicts) != 0 {
		t.Errorf("Expected 0 conflicts, got %d", len(conflicts))
	}

	// Merge should succeed
	mergeEvent, err := PerformMerge(repo, sessionID, "main", "branch-a")
	if err != nil {
		t.Fatalf("PerformMerge failed: %v", err)
	}

	if mergeEvent == nil {
		t.Fatal("Expected merge event, got nil")
	}

	if mergeEvent.ConflictsDetected != 0 {
		t.Errorf("Expected 0 conflicts detected, got %d", mergeEvent.ConflictsDetected)
	}
}

// TestTextConflictDetection tests detection of same-line modifications
func TestTextConflictDetection(t *testing.T) {
	repo := setupTestRepo(t)

	// Create file on main
	commitFile(t, repo, "mem/shared.md", "Line 1\nLine 2\nLine 3\n", "Add shared file")

	// Create branch A and modify
	createBranch(t, repo, "branch-a")
	commitFile(t, repo, "mem/shared.md", "Line 1\nModified by A\nLine 3\n", "Modify line 2 in A")

	// Switch to main and create branch B
	switchBranch(t, repo, "main")
	createBranch(t, repo, "branch-b")
	commitFile(t, repo, "mem/shared.md", "Line 1\nModified by B\nLine 3\n", "Modify line 2 in B")

	// Detect conflicts
	conflicts, err := DetectConflicts(repo, "branch-a", "branch-b")
	if err != nil {
		t.Fatalf("DetectConflicts failed: %v", err)
	}

	if len(conflicts) == 0 {
		t.Fatal("Expected conflicts, got none")
	}

	conflict := conflicts[0]
	if conflict.Type != ConflictText && conflict.Type != ConflictTree {
		t.Errorf("Expected text or tree conflict, got %s", conflict.Type)
	}

	if conflict.FilePath != "mem/shared.md" {
		t.Errorf("Expected conflict in mem/shared.md, got %s", conflict.FilePath)
	}
}

// TestTreeConflictDetection tests detection of different content hashes
func TestTreeConflictDetection(t *testing.T) {
	repo := setupTestRepo(t)

	// Create file on main
	commitFile(t, repo, "mem/doc.md", "Original content\n", "Add doc")

	// Create branch A and modify
	createBranch(t, repo, "branch-a")
	commitFile(t, repo, "mem/doc.md", "Content from branch A\n", "Modify in A")

	// Switch to main and create branch B
	switchBranch(t, repo, "main")
	createBranch(t, repo, "branch-b")
	commitFile(t, repo, "mem/doc.md", "Content from branch B\n", "Modify in B")

	// Detect conflicts
	conflicts, err := DetectConflicts(repo, "branch-a", "branch-b")
	if err != nil {
		t.Fatalf("DetectConflicts failed: %v", err)
	}

	if len(conflicts) == 0 {
		t.Fatal("Expected conflicts, got none")
	}

	conflict := conflicts[0]
	if conflict.Type != ConflictTree && conflict.Type != ConflictText {
		t.Errorf("Expected tree or text conflict, got %s", conflict.Type)
	}

	if conflict.ContentHashA == conflict.ContentHashB {
		t.Error("Expected different content hashes")
	}
}

// TestMergeBlockedWithoutResolution tests that merge is blocked when conflicts exist
func TestMergeBlockedWithoutResolution(t *testing.T) {
	repo := setupTestRepo(t)
	sessionID := "test-session-2"

	// Create conflicting branches
	commitFile(t, repo, "mem/conflict.md", "Original\n", "Add conflict file")

	createBranch(t, repo, "branch-a")
	commitFile(t, repo, "mem/conflict.md", "Version A\n", "Modify in A")

	switchBranch(t, repo, "main")
	createBranch(t, repo, "branch-b")
	commitFile(t, repo, "mem/conflict.md", "Version B\n", "Modify in B")

	switchBranch(t, repo, "main")

	// Attempt merge (should be blocked)
	_, err := PerformMerge(repo, sessionID, "main", "branch-a")
	if err == nil {
		t.Fatal("Expected merge to be blocked due to conflicts")
	}

	if !contains(err.Error(), "conflict") {
		t.Errorf("Expected error to mention conflicts, got: %v", err)
	}
}

// TestTopologicalReplay tests that replay preserves per-branch order
func TestTopologicalReplay(t *testing.T) {
	repo := setupTestRepo(t)
	ledgerDir := filepath.Join(repo, ".cog", "ledger")

	// Create session with events
	sessionID := "test-session-topo"
	events := []*Event{
		{ID: "evt1", Type: "test", Timestamp: "2026-01-16T10:00:00Z", SessionID: sessionID, Seq: 1},
		{ID: "evt2", Type: "test", Timestamp: "2026-01-16T10:00:01Z", SessionID: sessionID, Seq: 2},
		{ID: "evt3", Type: "test", Timestamp: "2026-01-16T10:00:02Z", SessionID: sessionID, Seq: 3},
	}

	// Write events to ledger
	sessionDir := filepath.Join(ledgerDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatalf("failed to create session dir: %v", err)
	}

	eventsFile := filepath.Join(sessionDir, "events.jsonl")
	f, err := os.Create(eventsFile)
	if err != nil {
		t.Fatalf("failed to create events file: %v", err)
	}
	defer f.Close()

	for _, event := range events {
		data, _ := event.MarshalJSON()
		f.Write(data)
		f.Write([]byte("\n"))
	}

	// Build DAG
	dag, err := BuildEventDAG(repo, []string{sessionID})
	if err != nil {
		t.Fatalf("BuildEventDAG failed: %v", err)
	}

	if len(dag.Nodes) != 3 {
		t.Errorf("Expected 3 nodes, got %d", len(dag.Nodes))
	}

	// Topological sort
	sorted, err := TopologicalSort(dag)
	if err != nil {
		t.Fatalf("TopologicalSort failed: %v", err)
	}

	if len(sorted) != 3 {
		t.Errorf("Expected 3 sorted events, got %d", len(sorted))
	}

	// Verify order preserved
	for i := 0; i < len(sorted)-1; i++ {
		if sorted[i].Seq > sorted[i+1].Seq {
			t.Errorf("Event order not preserved: evt%d (seq=%d) comes after evt%d (seq=%d)",
				i, sorted[i].Seq, i+1, sorted[i+1].Seq)
		}
	}
}

// TestReplayDeterminism tests that replay produces same order on repeated runs
func TestReplayDeterminism(t *testing.T) {
	repo := setupTestRepo(t)
	ledgerDir := filepath.Join(repo, ".cog", "ledger")

	// Create session with events
	sessionID := "test-session-determ"
	events := []*Event{
		{ID: "evt1", Type: "test", Timestamp: "2026-01-16T10:00:00Z", SessionID: sessionID, Seq: 1},
		{ID: "evt2", Type: "test", Timestamp: "2026-01-16T10:00:01Z", SessionID: sessionID, Seq: 2},
		{ID: "evt3", Type: "test", Timestamp: "2026-01-16T10:00:02Z", SessionID: sessionID, Seq: 3},
	}

	// Write events
	sessionDir := filepath.Join(ledgerDir, sessionID)
	os.MkdirAll(sessionDir, 0755)
	eventsFile := filepath.Join(sessionDir, "events.jsonl")
	f, _ := os.Create(eventsFile)
	for _, event := range events {
		data, _ := event.MarshalJSON()
		f.Write(data)
		f.Write([]byte("\n"))
	}
	f.Close()

	// Run twice and compare
	dag1, _ := BuildEventDAG(repo, []string{sessionID})
	sorted1, _ := TopologicalSort(dag1)
	hash1 := ComputeReplayHash(sorted1)

	dag2, _ := BuildEventDAG(repo, []string{sessionID})
	sorted2, _ := TopologicalSort(dag2)
	hash2 := ComputeReplayHash(sorted2)

	if hash1 != hash2 {
		t.Errorf("Replay non-deterministic: hash1=%s, hash2=%s", hash1, hash2)
	}
}

// TestConflictResolution tests the resolution workflow
func TestConflictResolution(t *testing.T) {
	conflict := Conflict{
		Type:         ConflictText,
		FilePath:     "mem/test.md",
		BranchA:      "branch-a",
		BranchB:      "branch-b",
		ContentHashA: "hash-a",
		ContentHashB: "hash-b",
		Description:  "Test conflict",
	}

	resolvedContent := "Manually resolved content\n"
	resolution := ResolveConflict(conflict, ResolutionManual, "", resolvedContent, "test-user")

	if resolution.ConflictFile != conflict.FilePath {
		t.Errorf("Expected conflict file %s, got %s", conflict.FilePath, resolution.ConflictFile)
	}

	if resolution.ResolutionMethod != ResolutionManual {
		t.Errorf("Expected manual resolution, got %s", resolution.ResolutionMethod)
	}

	if resolution.ResolvedHash == "" {
		t.Error("Expected resolved hash to be computed")
	}
}

// TestMergeEventCreation tests creating merge events
func TestMergeEventCreation(t *testing.T) {
	repo := setupTestRepo(t)
	sessionID := "test-session-merge-event"

	// Create simple commit
	commitFile(t, repo, "mem/test.md", "test content\n", "Test commit")

	parentHeads := []ParentHead{
		{Branch: "main", SessionID: sessionID, CommitHash: getCommitHash(repo, "main")},
		{Branch: "main", SessionID: sessionID, CommitHash: getCommitHash(repo, "main")}, // Same for simplicity
	}

	resolutions := []Resolution{}

	mergeEvent, err := CreateMergeEvent(repo, sessionID, parentHeads, resolutions)
	if err != nil {
		t.Fatalf("CreateMergeEvent failed: %v", err)
	}

	if mergeEvent.Type != "merge.commit" {
		t.Errorf("Expected type merge.commit, got %s", mergeEvent.Type)
	}

	if len(mergeEvent.ParentHeads) != 2 {
		t.Errorf("Expected 2 parent heads, got %d", len(mergeEvent.ParentHeads))
	}
}

// === HELPER FUNCTIONS ===

// MarshalJSON custom marshaler for Event (to support writing to JSONL)
func (e *Event) MarshalJSON() ([]byte, error) {
	// Create a map with the fields we want to serialize
	m := map[string]interface{}{
		"id":         e.ID,
		"type":       e.Type,
		"timestamp":  e.Timestamp,
		"session_id": e.SessionID,
	}
	if e.Seq != 0 {
		m["seq"] = e.Seq
	}
	if e.ParentID != "" {
		m["parent_id"] = e.ParentID
	}
	if e.Data != nil {
		m["data"] = e.Data
	}
	if len(e.ParentHeads) > 0 {
		m["parent_heads"] = e.ParentHeads
	}
	return json.Marshal(m)
}

// contains checks if string contains substring
func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && (s == substr || len(s) > len(substr) &&
		(strings.Contains(s, substr)))
}

// === BENCHMARK TESTS ===

func BenchmarkConflictDetection(b *testing.B) {
	repo := setupTestRepoForBench(b)

	// Create branches with many files
	for i := 0; i < 100; i++ {
		commitFileForBench(b, repo, filepath.Join("memory", fmt.Sprintf("file%d.md", i)),
			fmt.Sprintf("content %d\n", i), "Add files")
	}

	createBranch(b, repo, "bench-a")
	commitFileForBench(b, repo, "mem/conflict.md", "Version A\n", "Add conflict")

	switchBranch(b, repo, "main")
	createBranch(b, repo, "bench-b")
	commitFileForBench(b, repo, "mem/conflict.md", "Version B\n", "Add conflict")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DetectConflicts(repo, "bench-a", "bench-b")
	}
}

func setupTestRepoForBench(b *testing.B) string {
	tempDir := b.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = tempDir
	cmd.Run()

	cogDir := filepath.Join(tempDir, ".cog")
	os.MkdirAll(filepath.Join(cogDir, "memory"), 0755)

	testFile := filepath.Join(cogDir, "test.txt")
	os.WriteFile(testFile, []byte("initial\n"), 0644)

	cmd = exec.Command("git", "add", ".")
	cmd.Dir = tempDir
	cmd.Run()

	cmd = exec.Command("git", "commit", "-m", "Initial")
	cmd.Dir = tempDir
	cmd.Run()

	return tempDir
}

func commitFileForBench(b *testing.B, repoPath, filePath, content, commitMsg string) {
	fullPath := filepath.Join(repoPath, ".cog", filePath)
	dir := filepath.Dir(fullPath)
	os.MkdirAll(dir, 0755)
	os.WriteFile(fullPath, []byte(content), 0644)

	cmd := exec.Command("git", "add", ".cog/"+filePath)
	cmd.Dir = repoPath
	cmd.Run()

	cmd = exec.Command("git", "commit", "-m", commitMsg)
	cmd.Dir = repoPath
	cmd.Run()
}

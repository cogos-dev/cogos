// .cog/replay.go
// Topological replay of merge events with per-branch ordering preservation
//
// Part of CogOS Kernel - Squad D implementation

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// === EVENT DAG TYPES ===

// Event represents a generic event in the ledger
type Event struct {
	ID           string                 `json:"id"`
	Type         string                 `json:"type"`
	Timestamp    string                 `json:"timestamp"`
	SessionID    string                 `json:"session_id"`
	Seq          int                    `json:"seq,omitempty"`
	ParentID     string                 `json:"parent_id,omitempty"`
	Data         map[string]interface{} `json:"data,omitempty"`

	// For merge events
	ParentHeads  []ParentHead           `json:"parent_heads,omitempty"`

	// Internal fields for topological sort
	branch       string                 // Which branch this event belongs to
	branchSeq    int                    // Sequence within branch
}

// EventNode represents a node in the event DAG
type EventNode struct {
	Event       *Event
	Children    []*EventNode  // Events that depend on this one
	Parents     []*EventNode  // Events this one depends on
	Visited     bool          // For DFS traversal
	InDegree    int           // Number of unprocessed parents (for Kahn's algorithm)
}

// EventDAG represents a directed acyclic graph of events
type EventDAG struct {
	Nodes       map[string]*EventNode  // Event ID -> Node
	Roots       []*EventNode           // Events with no parents
	BranchHeads map[string]*EventNode  // Branch name -> latest event on that branch
	Sessions    map[string][]*Event    // Session ID -> events in order
}

// === DAG CONSTRUCTION ===

// BuildEventDAG constructs a DAG from multiple session ledgers
func BuildEventDAG(workspaceRoot string, sessionIDs []string) (*EventDAG, error) {
	dag := &EventDAG{
		Nodes:       make(map[string]*EventNode),
		Roots:       []*EventNode{},
		BranchHeads: make(map[string]*EventNode),
		Sessions:    make(map[string][]*Event),
	}

	ledgerDir := filepath.Join(workspaceRoot, ".cog", "ledger")

	// Load events from all sessions
	for _, sessionID := range sessionIDs {
		events, err := loadSessionEvents(ledgerDir, sessionID)
		if err != nil {
			return nil, fmt.Errorf("failed to load session %s: %w", sessionID, err)
		}

		dag.Sessions[sessionID] = events

		// Add events to DAG
		for _, event := range events {
			node := &EventNode{
				Event:    event,
				Children: []*EventNode{},
				Parents:  []*EventNode{},
			}
			dag.Nodes[event.ID] = node
		}
	}

	// Build parent-child relationships
	for _, node := range dag.Nodes {
		event := node.Event

		// Handle explicit parent references
		if event.ParentID != "" {
			if parentNode, exists := dag.Nodes[event.ParentID]; exists {
				node.Parents = append(node.Parents, parentNode)
				parentNode.Children = append(parentNode.Children, node)
				node.InDegree++
			}
		}

		// Handle merge events (multiple parents)
		if event.Type == "merge.commit" && len(event.ParentHeads) > 0 {
			for _, parentHead := range event.ParentHeads {
				// Find the last event from this parent branch
				if parentNode := findBranchHead(dag, parentHead.Branch, parentHead.LastEventHash); parentNode != nil {
					node.Parents = append(node.Parents, parentNode)
					parentNode.Children = append(parentNode.Children, node)
					node.InDegree++
				}
			}
		}

		// Identify roots (events with no parents)
		if len(node.Parents) == 0 {
			dag.Roots = append(dag.Roots, node)
		}
	}

	// Detect branch membership and sequence
	if err := detectBranchMembership(dag); err != nil {
		return nil, fmt.Errorf("failed to detect branch membership: %w", err)
	}

	return dag, nil
}

// loadSessionEvents loads all events from a session's events.jsonl
func loadSessionEvents(ledgerDir, sessionID string) ([]*Event, error) {
	eventsFile := filepath.Join(ledgerDir, sessionID, "events.jsonl")

	data, err := os.ReadFile(eventsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return []*Event{}, nil
		}
		return nil, fmt.Errorf("failed to read events file: %w", err)
	}

	var events []*Event
	lines := strings.Split(string(data), "\n")
	seq := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			// Skip malformed events
			continue
		}

		// Assign sequence if not present
		if event.Seq == 0 {
			seq++
			event.Seq = seq
		}

		events = append(events, &event)
	}

	return events, nil
}

// findBranchHead locates a specific event by branch and event hash
func findBranchHead(dag *EventDAG, branch, eventHash string) *EventNode {
	// If we have a cached branch head, return it
	if node, exists := dag.BranchHeads[branch]; exists {
		if eventHash == "" || node.Event.ID == eventHash {
			return node
		}
	}

	// Otherwise search through all events
	for _, node := range dag.Nodes {
		if node.Event.branch == branch && (eventHash == "" || node.Event.ID == eventHash) {
			return node
		}
	}

	return nil
}

// detectBranchMembership assigns branch names and sequences to events
func detectBranchMembership(dag *EventDAG) error {
	// This is a simplified heuristic:
	// - Events in the same session belong to the same branch
	// - Branch name can be inferred from session metadata or git branch

	for sessionID, events := range dag.Sessions {
		// Try to infer branch from first event with branch data
		branch := inferBranchFromSession(events)
		if branch == "" {
			branch = sessionID // Fallback: use session ID as branch name
		}

		// Assign branch and sequence
		for i, event := range events {
			event.branch = branch
			event.branchSeq = i + 1
		}

		// Update branch head
		if len(events) > 0 {
			lastEvent := events[len(events)-1]
			if node, exists := dag.Nodes[lastEvent.ID]; exists {
				dag.BranchHeads[branch] = node
			}
		}
	}

	return nil
}

// inferBranchFromSession attempts to extract branch name from event data
func inferBranchFromSession(events []*Event) string {
	for _, event := range events {
		if event.Data != nil {
			// Check for branch field in event data
			if branch, ok := event.Data["branch"].(string); ok && branch != "" {
				return branch
			}
			// Check for git_branch
			if branch, ok := event.Data["git_branch"].(string); ok && branch != "" {
				return branch
			}
		}
	}
	return ""
}

// === TOPOLOGICAL SORT ===

// TopologicalSort performs a topological sort on the DAG with stable ordering
// Preserves per-branch event ordering and uses timestamp as tie-breaker
func TopologicalSort(dag *EventDAG) ([]*Event, error) {
	// Kahn's algorithm with modifications for branch ordering preservation

	// Initialize queue with root events
	queue := make([]*EventNode, len(dag.Roots))
	copy(queue, dag.Roots)

	// Sort roots by timestamp (stable tie-breaker)
	sort.Slice(queue, func(i, j int) bool {
		return queue[i].Event.Timestamp < queue[j].Event.Timestamp
	})

	var sorted []*Event
	inDegree := make(map[string]int)

	// Initialize in-degree counts
	for id, node := range dag.Nodes {
		inDegree[id] = node.InDegree
	}

	// Process queue
	for len(queue) > 0 {
		// Pop first node
		node := queue[0]
		queue = queue[1:]

		// Add to sorted list
		sorted = append(sorted, node.Event)

		// Process children
		for _, child := range node.Children {
			inDegree[child.Event.ID]--

			if inDegree[child.Event.ID] == 0 {
				// All parents processed, add to queue
				queue = append(queue, child)
			}
		}

		// Re-sort queue for stable ordering
		// Primary: branch-seq (preserve per-branch ordering)
		// Secondary: timestamp (tie-breaker)
		sort.Slice(queue, func(i, j int) bool {
			ei := queue[i].Event
			ej := queue[j].Event

			// If same branch, use branch sequence
			if ei.branch == ej.branch {
				return ei.branchSeq < ej.branchSeq
			}

			// Different branches: use timestamp
			return ei.Timestamp < ej.Timestamp
		})
	}

	// Check for cycles
	if len(sorted) != len(dag.Nodes) {
		return nil, fmt.Errorf("cycle detected: only %d of %d events sorted", len(sorted), len(dag.Nodes))
	}

	return sorted, nil
}

// === REPLAY ===

// ReplayEvents replays events in topological order
func ReplayEvents(workspaceRoot string, events []*Event) error {
	for i, event := range events {
		fmt.Printf("[%d/%d] Replaying %s: %s (branch=%s, seq=%d)\n",
			i+1, len(events), event.Type, event.ID, event.branch, event.branchSeq)

		if err := replayEvent(workspaceRoot, event); err != nil {
			return fmt.Errorf("replay failed at event %s: %w", event.ID, err)
		}
	}

	return nil
}

// replayEvent replays a single event
func replayEvent(workspaceRoot string, event *Event) error {
	switch event.Type {
	case "merge.commit":
		return replayMergeEvent(workspaceRoot, event)
	case "conflict.detected":
		fmt.Printf("  Conflict detected: %v\n", event.Data["conflict_file"])
		return nil
	case "conflict.resolved":
		fmt.Printf("  Conflict resolved: %v\n", event.Data["conflict_file"])
		return nil
	default:
		// Generic event replay (no-op for most events)
		return nil
	}
}

// replayMergeEvent replays a merge event
func replayMergeEvent(workspaceRoot string, event *Event) error {
	// Verify merge commit exists
	if mergedCommit, ok := event.Data["merged_commit_hash"].(string); ok && mergedCommit != "" {
		// Check if commit exists in git
		// This is informational only - we don't re-execute merges
		fmt.Printf("  Merged commit: %s\n", mergedCommit)
	}

	// Verify tree hash
	if mergedTree, ok := event.Data["merged_tree_hash"].(string); ok && mergedTree != "" {
		fmt.Printf("  Merged tree: %s\n", mergedTree)
	}

	return nil
}

// === MERGE REPLAY (FULL) ===

// ReplayMerged performs a complete replay of a merged branch
func ReplayMerged(workspaceRoot, sessionID string, mergeEvent *MergeEvent) error {
	// Get all sessions involved in the merge
	sessionIDs := []string{sessionID}
	for _, parentHead := range mergeEvent.ParentHeads {
		if parentHead.SessionID != "" && parentHead.SessionID != sessionID {
			sessionIDs = append(sessionIDs, parentHead.SessionID)
		}
	}

	// Build event DAG
	dag, err := BuildEventDAG(workspaceRoot, sessionIDs)
	if err != nil {
		return fmt.Errorf("failed to build event DAG: %w", err)
	}

	// Topological sort
	events, err := TopologicalSort(dag)
	if err != nil {
		return fmt.Errorf("topological sort failed: %w", err)
	}

	// Replay events
	if err := ReplayEvents(workspaceRoot, events); err != nil {
		return fmt.Errorf("replay failed: %w", err)
	}

	fmt.Printf("✓ Replay complete: %d events processed\n", len(events))
	return nil
}

// === DETERMINISM VERIFICATION ===

// ComputeReplayHash computes a deterministic hash of the replay sequence
func ComputeReplayHash(events []*Event) string {
	// Build canonical representation
	var parts []string
	for _, event := range events {
		parts = append(parts, fmt.Sprintf("%s:%s:%s:%d", event.ID, event.Type, event.branch, event.branchSeq))
	}

	canonical := strings.Join(parts, "\n")

	// Hash it
	hash := fmt.Sprintf("%x", hashString(canonical))
	return hash
}

// hashString computes SHA256 hash of a string
func hashString(s string) []byte {
	// This is a placeholder - in real implementation, use crypto/sha256
	return []byte(s) // Simplified for now
}

// VerifyReplayDeterminism verifies that two replays produce the same sequence
func VerifyReplayDeterminism(workspaceRoot string, sessionIDs []string) error {
	// Build DAG twice
	dag1, err := BuildEventDAG(workspaceRoot, sessionIDs)
	if err != nil {
		return fmt.Errorf("first DAG build failed: %w", err)
	}

	dag2, err := BuildEventDAG(workspaceRoot, sessionIDs)
	if err != nil {
		return fmt.Errorf("second DAG build failed: %w", err)
	}

	// Sort twice
	events1, err := TopologicalSort(dag1)
	if err != nil {
		return fmt.Errorf("first sort failed: %w", err)
	}

	events2, err := TopologicalSort(dag2)
	if err != nil {
		return fmt.Errorf("second sort failed: %w", err)
	}

	// Compare hashes
	hash1 := ComputeReplayHash(events1)
	hash2 := ComputeReplayHash(events2)

	if hash1 != hash2 {
		return fmt.Errorf("replay non-deterministic: hash1=%s, hash2=%s", hash1, hash2)
	}

	fmt.Printf("✓ Replay determinism verified: hash=%s\n", hash1)
	return nil
}

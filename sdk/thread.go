package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cogos-dev/cogos/sdk/internal/fs"
	"github.com/cogos-dev/cogos/sdk/types"
)

// threadProjector handles cog://thread/* namespace.
// Provides conversation thread storage and retrieval.
type threadProjector struct {
	BaseProjector
	kernel *Kernel
}

// CanMutate returns true - threads can be appended and archived.
func (p *threadProjector) CanMutate() bool {
	return true
}

// Resolve reads threads from storage.
//
// URI patterns:
//   - cog://thread - List all threads
//   - cog://thread/current - Get the current active thread
//   - cog://thread/{id} - Get a specific thread by ID
//   - cog://thread/{id}?last=10 - Get last 10 messages only
//   - cog://thread/{id}?summarize=true - Get thread summary only
func (p *threadProjector) Resolve(ctx context.Context, uri *ParsedURI) (*Resource, error) {
	if uri.Path == "" {
		return p.listThreads(uri)
	}

	threadID := uri.Path

	// Handle "current" alias
	if threadID == "current" {
		activeID, err := p.getActiveThreadID()
		if err != nil {
			return nil, NewPathError("Resolve", p.activeThreadPath(), err)
		}
		if activeID == "" {
			return nil, NotFoundError("Resolve", uri.Raw).
				WithRecover("No active thread. Create one with cog://thread mutation.")
		}
		threadID = activeID
	}

	thread, err := p.loadThread(threadID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NotFoundError("Resolve", uri.Raw)
		}
		return nil, NewPathError("Resolve", p.threadPath(threadID), err)
	}

	// Handle summarize query param
	if uri.GetQueryBool("summarize") {
		summary := thread.ToSummary()
		return NewJSONResource(uri.Raw, summary)
	}

	// Handle last N query param
	lastN := uri.GetQueryInt("last", 0)
	if lastN > 0 && lastN < len(thread.Messages) {
		thread.Messages = thread.LastN(lastN)
	}

	return NewJSONResource(uri.Raw, thread)
}

// Mutate writes or updates threads.
//
// Operations:
//   - Append: Append a message to the thread
//   - Set: Create or replace a thread
//   - Patch: Update thread metadata (title, status)
//   - Delete: Archive a thread
//
// For Append, content should be a Message:
//
//	{"role": "user", "content": "Hello"}
//
// For Set, content should be a Thread:
//
//	{"id": "thread-123", "title": "New Thread"}
//
// For Patch, content should be partial Thread:
//
//	{"status": "archived"}
func (p *threadProjector) Mutate(ctx context.Context, uri *ParsedURI, m *Mutation) error {
	switch m.Op {
	case MutationAppend:
		return p.appendMessage(uri, m.Content)
	case MutationSet:
		return p.setThread(uri, m.Content)
	case MutationPatch:
		return p.patchThread(uri, m.Content)
	case MutationDelete:
		return p.archiveThread(uri)
	default:
		return NewURIError("Mutate", uri.Raw, fmt.Errorf("unsupported op: %s", m.Op))
	}
}

// appendMessage appends a message to a thread.
func (p *threadProjector) appendMessage(uri *ParsedURI, content []byte) error {
	threadID := uri.Path
	if threadID == "" || threadID == "current" {
		activeID, err := p.getActiveThreadID()
		if err != nil || activeID == "" {
			// Create new thread
			threadID = fmt.Sprintf("thread-%d", time.Now().UnixNano())
			if err := p.setActiveThreadID(threadID); err != nil {
				return err
			}
		} else {
			threadID = activeID
		}
	}

	// Parse the message
	var msg types.Message
	if err := json.Unmarshal(content, &msg); err != nil {
		return NewError("Mutate", fmt.Errorf("invalid message data: %w", err))
	}

	// Set timestamp if not provided
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}

	// Append to thread file (JSONL format)
	threadPath := p.threadPath(threadID)
	line, err := json.Marshal(&msg)
	if err != nil {
		return NewError("Mutate", fmt.Errorf("failed to marshal message: %w", err))
	}

	return fs.AppendLine(threadPath, line)
}

// setThread creates or replaces a thread.
func (p *threadProjector) setThread(uri *ParsedURI, content []byte) error {
	var thread types.Thread
	if err := json.Unmarshal(content, &thread); err != nil {
		return NewError("Mutate", fmt.Errorf("invalid thread data: %w", err))
	}

	threadID := uri.Path
	if threadID == "" {
		threadID = thread.ID
	}
	if threadID == "" {
		threadID = fmt.Sprintf("thread-%d", time.Now().UnixNano())
	}
	thread.ID = threadID

	// Set timestamps if not provided
	now := time.Now()
	if thread.CreatedAt.IsZero() {
		thread.CreatedAt = now
	}
	thread.UpdatedAt = now

	// Write each message as JSONL
	threadPath := p.threadPath(threadID)
	if err := fs.EnsureDir(filepath.Dir(threadPath)); err != nil {
		return NewPathError("Mutate", threadPath, err)
	}

	// Write fresh file with all messages
	f, err := os.Create(threadPath)
	if err != nil {
		return NewPathError("Mutate", threadPath, err)
	}
	defer f.Close()

	encoder := json.NewEncoder(f)
	for _, msg := range thread.Messages {
		if err := encoder.Encode(msg); err != nil {
			return NewError("Mutate", fmt.Errorf("failed to write message: %w", err))
		}
	}

	// Set as active if specified
	if thread.Status == "active" || thread.Status == "" {
		return p.setActiveThreadID(threadID)
	}

	return nil
}

// patchThread updates thread metadata.
func (p *threadProjector) patchThread(uri *ParsedURI, content []byte) error {
	threadID := uri.Path
	if threadID == "" || threadID == "current" {
		activeID, err := p.getActiveThreadID()
		if err != nil || activeID == "" {
			return NotFoundError("Mutate", uri.Raw)
		}
		threadID = activeID
	}

	// Load existing thread
	thread, err := p.loadThread(threadID)
	if err != nil {
		if os.IsNotExist(err) {
			return NotFoundError("Mutate", uri.Raw)
		}
		return NewPathError("Mutate", p.threadPath(threadID), err)
	}

	// Apply patch
	var patch struct {
		Title    *string `json:"title,omitempty"`
		Status   *string `json:"status,omitempty"`
		Metadata map[string]any `json:"metadata,omitempty"`
	}
	if err := json.Unmarshal(content, &patch); err != nil {
		return NewError("Mutate", fmt.Errorf("invalid patch data: %w", err))
	}

	if patch.Title != nil {
		thread.Title = *patch.Title
	}
	if patch.Status != nil {
		thread.Status = *patch.Status
	}
	if patch.Metadata != nil {
		if thread.Metadata == nil {
			thread.Metadata = make(map[string]any)
		}
		for k, v := range patch.Metadata {
			thread.Metadata[k] = v
		}
	}
	thread.UpdatedAt = time.Now()

	// Save thread metadata separately (messages stay in JSONL)
	metaPath := p.threadMetaPath(threadID)
	meta := struct {
		ID        string         `json:"id"`
		Title     string         `json:"title,omitempty"`
		Status    string         `json:"status,omitempty"`
		CreatedAt time.Time      `json:"created_at"`
		UpdatedAt time.Time      `json:"updated_at"`
		Metadata  map[string]any `json:"metadata,omitempty"`
	}{
		ID:        thread.ID,
		Title:     thread.Title,
		Status:    thread.Status,
		CreatedAt: thread.CreatedAt,
		UpdatedAt: thread.UpdatedAt,
		Metadata:  thread.Metadata,
	}

	return fs.WriteJSON(metaPath, meta, 0644)
}

// archiveThread marks a thread as archived.
func (p *threadProjector) archiveThread(uri *ParsedURI) error {
	threadID := uri.Path
	if threadID == "" || threadID == "current" {
		activeID, err := p.getActiveThreadID()
		if err != nil || activeID == "" {
			return NotFoundError("Mutate", uri.Raw)
		}
		threadID = activeID

		// Clear active thread
		if err := p.setActiveThreadID(""); err != nil {
			return err
		}
	}

	// Set status to archived via patch
	patchContent, _ := json.Marshal(map[string]string{"status": "archived"})
	patchURI := &ParsedURI{
		Namespace: uri.Namespace,
		Path:      threadID,
		Raw:       "cog://thread/" + threadID,
	}
	return p.patchThread(patchURI, patchContent)
}

// listThreads returns a list of all threads.
func (p *threadProjector) listThreads(uri *ParsedURI) (*Resource, error) {
	threadsDir := p.threadsDir()
	entries, err := os.ReadDir(threadsDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No threads yet
			result := &types.ThreadList{
				Threads: make([]*types.ThreadSummary, 0),
				Total:   0,
			}
			return NewJSONResource(uri.Raw, result)
		}
		return nil, NewPathError("Resolve", threadsDir, err)
	}

	activeID, _ := p.getActiveThreadID()

	summaries := make([]*types.ThreadSummary, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}

		threadID := strings.TrimSuffix(name, ".jsonl")
		thread, err := p.loadThread(threadID)
		if err != nil {
			continue // Skip unreadable threads
		}

		summaries = append(summaries, thread.ToSummary())
	}

	// Sort by last activity (most recent first)
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].LastActivity.After(summaries[j].LastActivity)
	})

	result := &types.ThreadList{
		Threads:  summaries,
		Total:    len(summaries),
		ActiveID: activeID,
	}

	return NewJSONResource(uri.Raw, result)
}

// loadThread reads a thread from disk.
func (p *threadProjector) loadThread(id string) (*types.Thread, error) {
	threadPath := p.threadPath(id)

	f, err := os.Open(threadPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	thread := types.NewThread(id)

	// Try to load metadata if exists
	metaPath := p.threadMetaPath(id)
	if data, err := os.ReadFile(metaPath); err == nil {
		var meta struct {
			Title     string         `json:"title"`
			Status    string         `json:"status"`
			CreatedAt time.Time      `json:"created_at"`
			UpdatedAt time.Time      `json:"updated_at"`
			Metadata  map[string]any `json:"metadata"`
		}
		if json.Unmarshal(data, &meta) == nil {
			thread.Title = meta.Title
			thread.Status = meta.Status
			if !meta.CreatedAt.IsZero() {
				thread.CreatedAt = meta.CreatedAt
			}
			if !meta.UpdatedAt.IsZero() {
				thread.UpdatedAt = meta.UpdatedAt
			}
			thread.Metadata = meta.Metadata
		}
	}

	// Load messages from JSONL
	scanner := bufio.NewScanner(f)
	var lastMsgTime time.Time
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg types.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // Skip malformed lines
		}
		thread.Messages = append(thread.Messages, &msg)

		if msg.Timestamp.After(lastMsgTime) {
			lastMsgTime = msg.Timestamp
		}
	}

	// Update UpdatedAt based on last message if not set from metadata
	if !lastMsgTime.IsZero() && lastMsgTime.After(thread.UpdatedAt) {
		thread.UpdatedAt = lastMsgTime
	}

	// Set CreatedAt from first message if not set
	if thread.CreatedAt.IsZero() && len(thread.Messages) > 0 {
		thread.CreatedAt = thread.Messages[0].Timestamp
	}

	return thread, scanner.Err()
}

// threadsDir returns the threads directory path.
func (p *threadProjector) threadsDir() string {
	return filepath.Join(p.kernel.MemoryDir(), "episodic", "threads")
}

// threadPath returns the path to a thread's JSONL file.
func (p *threadProjector) threadPath(id string) string {
	return filepath.Join(p.threadsDir(), id, "thread.jsonl")
}

// threadMetaPath returns the path to a thread's metadata file.
func (p *threadProjector) threadMetaPath(id string) string {
	return filepath.Join(p.threadsDir(), id, "meta.json")
}

// activeThreadPath returns the path to the active thread marker file.
func (p *threadProjector) activeThreadPath() string {
	return filepath.Join(p.threadsDir(), ".active")
}

// getActiveThreadID reads the current active thread ID.
func (p *threadProjector) getActiveThreadID() (string, error) {
	data, err := os.ReadFile(p.activeThreadPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// setActiveThreadID writes the active thread ID.
func (p *threadProjector) setActiveThreadID(id string) error {
	activePath := p.activeThreadPath()
	if id == "" {
		// Remove active thread marker
		return os.Remove(activePath)
	}
	return fs.WriteAtomic(activePath, []byte(id), 0644)
}

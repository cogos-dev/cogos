// cogfield_sessions.go - Session detail endpoint for CogField
//
// GET /api/cogfield/sessions/{id} - Returns parsed session events as a conversation
//
// Session waterfall: three message sources tried in order:
//   1. CogOS threads (.cog/mem/episodic/threads/{id}/thread.jsonl)
//   2. Claude Code transcripts (~/.claude/projects/-Users-slowbro-cog-workspace/{id}.jsonl)
//   3. Event stubs (.cog/.state/events/*.jsonl) — existing format

package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// SessionMessage represents a single event in a session conversation
type SessionMessage struct {
	Seq       int                    `json:"seq"`
	Type      string                 `json:"type"`
	Timestamp string                 `json:"timestamp"`
	Role      string                 `json:"role,omitempty"`
	Content   string                 `json:"content,omitempty"`
	Meta      map[string]interface{} `json:"meta,omitempty"`
}

// SessionDetail is the response for GET /api/cogfield/sessions/{id}
type SessionDetail struct {
	ID           string           `json:"id"`
	Created      string           `json:"created"`
	Modified     string           `json:"modified"`
	EventCount   int              `json:"event_count"`
	MessageCount int              `json:"message_count"`
	Messages     []SessionMessage `json:"messages"`
	Source       string           `json:"source,omitempty"`
}

// handleSessionDetail handles GET /api/cogfield/sessions/{id}
func (s *serveServer) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract session ID from URL: /api/cogfield/sessions/{id}
	sessionID := strings.TrimPrefix(r.URL.Path, "/api/cogfield/sessions/")
	if sessionID == "" {
		http.Error(w, "Session ID required", http.StatusBadRequest)
		return
	}

	var root string
	if ws := workspaceFromRequest(r); ws != nil {
		root = ws.root
	} else {
		var err error
		root, _, err = ResolveWorkspace()
		if err != nil {
			http.Error(w, "Failed to resolve workspace", http.StatusInternalServerError)
			return
		}
	}

	detail, err := loadSessionDetail(root, sessionID)
	if err != nil {
		log.Printf("cogfield: session detail error: %v", err)
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(detail)
}

// loadSessionDetail tries three sources in waterfall order:
// 1. CogOS threads, 2. Claude Code transcripts, 3. Event stubs
func loadSessionDetail(root, sessionID string) (*SessionDetail, error) {
	// 1. CogOS threads
	if d, err := loadFromThreads(root, sessionID); err == nil && d.MessageCount > 0 {
		return d, nil
	}

	// 2. Claude Code transcripts
	if d, err := loadFromClaudeTranscript(sessionID); err == nil && d.MessageCount > 0 {
		return d, nil
	}

	// 3. Event stubs (existing)
	return loadFromEventStubs(root, sessionID)
}

// loadFromThreads reads .cog/mem/episodic/threads/{sessionID}/thread.jsonl
func loadFromThreads(root, sessionID string) (*SessionDetail, error) {
	threadsDir := filepath.Join(root, ".cog", "mem", "episodic", "threads")

	// Try exact ID match first, then thread-{id} prefix
	candidates := []string{
		filepath.Join(threadsDir, sessionID, "thread.jsonl"),
		filepath.Join(threadsDir, "thread-"+sessionID, "thread.jsonl"),
	}

	for _, threadPath := range candidates {
		if _, err := os.Stat(threadPath); err != nil {
			continue
		}

		f, err := os.Open(threadPath)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

		var messages []SessionMessage
		var firstTs, lastTs string
		seq := 0

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var entry struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				Timestamp string `json:"timestamp"`
			}
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				continue
			}

			seq++
			msg := SessionMessage{
				Seq:       seq,
				Type:      "turn",
				Timestamp: entry.Timestamp,
				Role:      entry.Role,
				Content:   entry.Content,
			}
			messages = append(messages, msg)

			if firstTs == "" || entry.Timestamp < firstTs {
				firstTs = entry.Timestamp
			}
			if entry.Timestamp > lastTs {
				lastTs = entry.Timestamp
			}
		}
		f.Close()

		if len(messages) == 0 {
			continue
		}

		messageCount := 0
		for _, m := range messages {
			if m.Content != "" {
				messageCount++
			}
		}

		return &SessionDetail{
			ID:           sessionID,
			Created:      firstTs,
			Modified:     lastTs,
			EventCount:   len(messages),
			MessageCount: messageCount,
			Messages:     messages,
			Source:       "cogos_thread",
		}, nil
	}

	return nil, os.ErrNotExist
}

// claudeTranscriptEntry represents a line in a Claude Code transcript JSONL file.
type claudeTranscriptEntry struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

// claudeTranscriptMessage is the message field inside a Claude transcript entry.
type claudeTranscriptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// loadFromClaudeTranscript reads ~/.claude/projects/-Users-slowbro-cog-workspace/{sessionID}.jsonl
func loadFromClaudeTranscript(sessionID string) (*SessionDetail, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	transcriptPath := filepath.Join(home, ".claude", "projects", "-Users-slowbro-cog-workspace", sessionID+".jsonl")
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024) // 4MB buffer for large transcripts

	var messages []SessionMessage
	seq := 0

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var entry claudeTranscriptEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		// Only process user and assistant messages
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}

		if entry.Message == nil {
			continue
		}

		var msg claudeTranscriptMessage
		if err := json.Unmarshal(entry.Message, &msg); err != nil {
			continue
		}

		content := extractClaudeContent(msg.Role, msg.Content)
		if content == "" {
			continue
		}

		seq++
		messages = append(messages, SessionMessage{
			Seq:     seq,
			Type:    "turn",
			Role:    msg.Role,
			Content: content,
		})
	}

	if len(messages) == 0 {
		return nil, os.ErrNotExist
	}

	return &SessionDetail{
		ID:           sessionID,
		EventCount:   len(messages),
		MessageCount: len(messages),
		Messages:     messages,
		Source:       "claude_transcript",
	}, nil
}

// extractClaudeContent handles both string content (user) and [{type,text}] arrays (assistant).
// Skips thinking and tool_use blocks.
func extractClaudeContent(role string, raw json.RawMessage) string {
	if raw == nil {
		return ""
	}

	// Try string first (user messages)
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Try array of content blocks (assistant messages)
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}

	var parts []string
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
		// Skip thinking, tool_use, tool_result blocks
	}
	return strings.Join(parts, "\n")
}

// loadFromEventStubs is the original loadSessionDetail — reads .cog/.state/events/*.jsonl
func loadFromEventStubs(root, sessionID string) (*SessionDetail, error) {
	eventsDir := filepath.Join(root, ".cog", ".state", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		return nil, err
	}

	// Find all files matching this session ID
	var matchingFiles []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		// Match: *-{sessionID}.jsonl
		base := strings.TrimSuffix(name, ".jsonl")
		if strings.HasSuffix(base, "-"+sessionID) {
			matchingFiles = append(matchingFiles, filepath.Join(eventsDir, name))
		}
	}

	if len(matchingFiles) == 0 {
		return nil, os.ErrNotExist
	}

	allMessages := make([]SessionMessage, 0)
	var firstTs, lastTs string

	for _, fpath := range matchingFiles {
		f, err := os.Open(fpath)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			msg := parseSessionEvent(line)
			if msg == nil {
				continue
			}

			allMessages = append(allMessages, *msg)

			if firstTs == "" || msg.Timestamp < firstTs {
				firstTs = msg.Timestamp
			}
			if msg.Timestamp > lastTs {
				lastTs = msg.Timestamp
			}
		}
		f.Close()
	}

	// Count messages with actual content
	messageCount := 0
	for _, m := range allMessages {
		if m.Content != "" {
			messageCount++
		}
	}

	return &SessionDetail{
		ID:           sessionID,
		Created:      firstTs,
		Modified:     lastTs,
		EventCount:   len(allMessages),
		MessageCount: messageCount,
		Messages:     allMessages,
		Source:       "event_stubs",
	}, nil
}

// parseSessionEvent parses a single JSONL line into a SessionMessage
func parseSessionEvent(line string) *SessionMessage {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil
	}

	// Check for hashed_payload envelope format
	if hp, ok := raw["hashed_payload"]; ok {
		var envelope struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			SessionID string `json:"session_id"`
			Data      struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"data"`
		}
		if err := json.Unmarshal(hp, &envelope); err != nil {
			return nil
		}

		var meta map[string]interface{}
		if md, ok := raw["metadata"]; ok {
			json.Unmarshal(md, &meta)
		}

		seq := 0
		if meta != nil {
			if s, ok := meta["seq"].(float64); ok {
				seq = int(s)
			}
		}

		return &SessionMessage{
			Seq:       seq,
			Type:      envelope.Type,
			Timestamp: envelope.Timestamp,
			Role:      envelope.Data.Role,
			Content:   envelope.Data.Content,
			Meta:      meta,
		}
	}

	// Flat format: {id, seq, session_id, ts, type, data?}
	var flat struct {
		Seq  int    `json:"seq"`
		Ts   string `json:"ts"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &flat); err != nil {
		return nil
	}

	// Try to extract role/content from data field if present
	var role, content string
	if dataRaw, ok := raw["data"]; ok {
		var data struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		json.Unmarshal(dataRaw, &data)
		role = data.Role
		content = data.Content
	}

	return &SessionMessage{
		Seq:       flat.Seq,
		Type:      flat.Type,
		Timestamp: flat.Ts,
		Role:      role,
		Content:   content,
	}
}

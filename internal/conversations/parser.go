// parser.go — streaming JSONL parser for Claude Code session files.
//
// Never loads an entire JSONL into memory. Uses bufio.Scanner line-by-line
// to handle sessions up to many MB without OOM. The extract_user_msgs.py
// reference script informed the field selection and filtering rules:
//   - user records: type="user", message.content (array or string)
//   - assistant records: type="assistant", message.content (array)
//   - system-reminder wrapper tags are stripped from user text
//   - user-prompt-submit-hook outputs are skipped
//   - tool_result content blocks within user records are skipped
//   - ai-title records supply the session title
//
// Additional fields discovered beyond the Python reference:
//   - uuid, parentUuid — conversation tree linkage
//   - entrypoint — client surface (cli, claude-desktop, etc.)
//   - userType — always "external" in observed data
//   - version — Claude Code version string
//   - cwd — working directory at message time
//   - sessionId — always present; must match the filename UUID
//   - thinking content blocks in assistant records
package conversations

import (
	"bufio"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
)

// maxScannerTokenSize is the per-line buffer used by bufio.Scanner.
// Claude Code lines can be very large (full assistant message in one JSON record),
// so we allocate 4MB per line to handle thinking-heavy assistant turns.
const maxScannerTokenSize = 4 * 1024 * 1024

// systemReminderRE strips <system-reminder>...</system-reminder> blocks.
var systemReminderRE = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// rawRecord is the outermost envelope of every JSONL line.
// We decode into this first and then dispatch on Type.
type rawRecord struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	ParentUUID  string          `json:"parentUuid"`
	SessionID   string          `json:"sessionId"`
	Timestamp   string          `json:"timestamp"`
	Message     json.RawMessage `json:"message"`
	Content     string          `json:"content"` // system records use content string
	UserType    string          `json:"userType"`
	Entrypoint  string          `json:"entrypoint"`
	IsSidechain bool            `json:"isSidechain"`
	Level       string          `json:"level"`
}

// rawMessage is message.* within a user or assistant record.
type rawMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

// rawContentBlock is one element of message.content when it is an array.
type rawContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Input    json.RawMessage `json:"input,omitempty"`   // tool_use
	Content  json.RawMessage `json:"content,omitempty"` // tool_result nested
}

// rawAITitle is the ai-title record type.
type rawAITitle struct {
	Title string `json:"title"`
}

// ParseSession streams turns from r, calling callback for each turn and
// updating meta in place. r is typically an os.File; it is NOT closed.
// The caller controls open/close. callback may return false to abort early.
//
// Dedup: turns with a duplicate uuid within the same session are silently
// skipped. This handles resumed/compacted JSONL files where CC re-appends
// historical turns that are already present, causing the same uuid to appear
// at two different turn_indexes.
//
// ParseSession is purely functional: no global state, no side effects beyond
// the callbacks. Tests can supply a strings.Reader fixture.
func ParseSession(r io.Reader, sessionID string, maxTurnLen int, meta *SessionMeta, callback func(Turn) bool) error {
	if maxTurnLen <= 0 {
		maxTurnLen = 8192
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, maxScannerTokenSize), maxScannerTokenSize)

	seenUUIDs := make(map[string]struct{})
	turnIndex := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var rec rawRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// Skip unparseable lines — sessions may have partially-written last lines.
			continue
		}

		// Populate session-level metadata from whichever record has it.
		if rec.Entrypoint != "" && meta.Entrypoint == "" {
			meta.Entrypoint = rec.Entrypoint
		}

		switch rec.Type {
		case "ai-title":
			// Extract the session title from the ai-title record.
			var t rawAITitle
			if err := json.Unmarshal(line, &t); err == nil && t.Title != "" {
				meta.Title = t.Title
			}

		case "user":
			// UUID dedup: skip turns whose uuid has already been seen in this session.
			if rec.UUID != "" {
				if _, dup := seenUUIDs[rec.UUID]; dup {
					continue
				}
				seenUUIDs[rec.UUID] = struct{}{}
			}
			turn, ok := parseUserRecord(&rec, sessionID, turnIndex, maxTurnLen)
			if !ok {
				continue
			}
			updateTimeBounds(meta, turn.Timestamp)
			if !callback(turn) {
				return nil
			}
			turnIndex++
			meta.TurnCount = turnIndex

		case "assistant":
			// UUID dedup: skip turns whose uuid has already been seen in this session.
			if rec.UUID != "" {
				if _, dup := seenUUIDs[rec.UUID]; dup {
					continue
				}
				seenUUIDs[rec.UUID] = struct{}{}
			}
			turn, ok := parseAssistantRecord(&rec, sessionID, turnIndex, maxTurnLen)
			if !ok {
				continue
			}
			updateTimeBounds(meta, turn.Timestamp)
			if !callback(turn) {
				return nil
			}
			turnIndex++
			meta.TurnCount = turnIndex
		}
	}

	return scanner.Err()
}

// ParseSessionIncremental parses only the records found at or after the
// reader's current position, assigning turn indexes starting at startTurnIndex.
// It is the append-only counterpart to ParseSession: the caller seeks the
// underlying file to meta.LastParsedByteOffset before calling, so only the
// freshly-appended tail is scanned. Each emitted Turn is passed to callback;
// callback may return false to abort early.
//
// startOffset is the absolute file offset the reader is positioned at (i.e.
// meta.LastParsedByteOffset). The returned committedOffset is the absolute
// offset just past the last NEWLINE-TERMINATED line consumed — the caller
// persists it as the new cursor so the next cycle resumes there.
//
// Tail-line handling: a final line WITHOUT a trailing newline may be either a
// complete record the writer has not yet newline-terminated, or a half-written
// record. We parse it (so a complete record's turn is not delayed a cycle) but
// do NOT advance committedOffset past it. The cursor therefore stays at the
// start of that line, so the next cycle re-reads it; UUID dedup makes the
// re-read idempotent, and a genuinely partial line simply fails to parse and
// emits nothing. This guarantees no record is ever skipped or double-counted.
//
// Unlike ParseSession this uses bufio.Reader.ReadBytes so per-line byte
// accounting is exact (bufio.Scanner buffers ahead and cannot report line
// boundaries). The parsing/dedup/field-extraction rules are otherwise
// identical to ParseSession.
//
// seenUUIDs may be supplied by the caller (UUIDs already present in the
// session) so that re-appended historical records — common in resumed or
// compacted JSONL — are deduplicated against the existing turn set, not just
// within this tail. Pass nil for an empty set.
func ParseSessionIncremental(r io.Reader, sessionID string, startTurnIndex int, startOffset int64, maxTurnLen int, meta *SessionMeta, seenUUIDs map[string]struct{}, callback func(turn Turn) bool) (committedOffset int64, err error) {
	if maxTurnLen <= 0 {
		maxTurnLen = 8192
	}
	if seenUUIDs == nil {
		seenUUIDs = make(map[string]struct{})
	}

	br := bufio.NewReaderSize(r, 1024*1024)
	turnIndex := startTurnIndex
	committedOffset = startOffset
	var consumed int64 // bytes consumed from the reader since startOffset

	for {
		line, readErr := br.ReadBytes('\n')
		consumed += int64(len(line))
		hasNewline := len(line) > 0 && line[len(line)-1] == '\n'
		trimmed := trimLineEnd(line)

		if len(trimmed) != 0 {
			cont := consumeIncrementalRecord(trimmed, sessionID, &turnIndex, maxTurnLen, meta, seenUUIDs, callback)
			// Advance the durable cursor only past newline-terminated lines.
			// A trailing newline-less line is parsed (above) but its offset is
			// withheld so the next cycle safely re-reads it.
			if hasNewline {
				committedOffset = startOffset + consumed
			}
			if !cont {
				return committedOffset, nil
			}
		} else if hasNewline {
			// Blank/whitespace-only line that is newline-terminated: nothing to
			// emit, but the cursor still advances past it.
			committedOffset = startOffset + consumed
		}

		if readErr != nil {
			if readErr == io.EOF {
				return committedOffset, nil
			}
			return committedOffset, readErr
		}
	}
}

// consumeIncrementalRecord decodes one JSONL line and, when it yields a Turn,
// invokes callback. Returns false when the callback asks to abort. turnIndex is
// advanced in place on a successful emit. This mirrors the per-record switch in
// ParseSession exactly.
func consumeIncrementalRecord(line []byte, sessionID string, turnIndex *int, maxTurnLen int, meta *SessionMeta, seenUUIDs map[string]struct{}, callback func(Turn) bool) bool {
	var rec rawRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		// Skip unparseable lines — consistent with ParseSession.
		return true
	}

	if rec.Entrypoint != "" && meta.Entrypoint == "" {
		meta.Entrypoint = rec.Entrypoint
	}

	switch rec.Type {
	case "ai-title":
		var t rawAITitle
		if err := json.Unmarshal(line, &t); err == nil && t.Title != "" {
			meta.Title = t.Title
		}

	case "user":
		if rec.UUID != "" {
			if _, dup := seenUUIDs[rec.UUID]; dup {
				return true
			}
			seenUUIDs[rec.UUID] = struct{}{}
		}
		turn, ok := parseUserRecord(&rec, sessionID, *turnIndex, maxTurnLen)
		if !ok {
			return true
		}
		updateTimeBounds(meta, turn.Timestamp)
		if !callback(turn) {
			return false
		}
		*turnIndex++
		meta.TurnCount = *turnIndex

	case "assistant":
		if rec.UUID != "" {
			if _, dup := seenUUIDs[rec.UUID]; dup {
				return true
			}
			seenUUIDs[rec.UUID] = struct{}{}
		}
		turn, ok := parseAssistantRecord(&rec, sessionID, *turnIndex, maxTurnLen)
		if !ok {
			return true
		}
		updateTimeBounds(meta, turn.Timestamp)
		if !callback(turn) {
			return false
		}
		*turnIndex++
		meta.TurnCount = *turnIndex
	}
	return true
}

// trimLineEnd strips a trailing \n and optional \r from a raw line read by
// bufio.Reader.ReadBytes, returning the record bytes. It does not allocate
// when there is nothing to trim.
func trimLineEnd(line []byte) []byte {
	n := len(line)
	if n > 0 && line[n-1] == '\n' {
		n--
		if n > 0 && line[n-1] == '\r' {
			n--
		}
	}
	return line[:n]
}

// parseUserRecord extracts a Turn from a type="user" raw record.
// Returns (turn, true) when there is meaningful text content.
func parseUserRecord(rec *rawRecord, sessionID string, idx int, maxLen int) (Turn, bool) {
	if len(rec.Message) == 0 {
		return Turn{}, false
	}

	var msg rawMessage
	if err := json.Unmarshal(rec.Message, &msg); err != nil {
		return Turn{}, false
	}

	text := extractContentText(msg.Content, false)
	text = stripSystemReminders(text)
	if text == "" {
		return Turn{}, false
	}
	// Skip hook outputs.
	if strings.HasPrefix(text, "<user-prompt-submit-hook>") {
		return Turn{}, false
	}
	if len(text) > maxLen {
		text = text[:maxLen] + " [truncated]"
	}

	ts := parseTimestamp(rec.Timestamp)
	return Turn{
		UUID:       rec.UUID,
		SessionID:  sessionID,
		TurnIndex:  idx,
		Role:       RoleUser,
		Timestamp:  ts,
		Text:       text,
		ParentUUID: rec.ParentUUID,
	}, true
}

// parseAssistantRecord extracts a Turn from a type="assistant" raw record.
// Returns (turn, true) when there is text or thinking content.
func parseAssistantRecord(rec *rawRecord, sessionID string, idx int, maxLen int) (Turn, bool) {
	if len(rec.Message) == 0 {
		return Turn{}, false
	}

	var msg rawMessage
	if err := json.Unmarshal(rec.Message, &msg); err != nil {
		return Turn{}, false
	}

	text, isToolCall := extractAssistantContent(msg.Content, maxLen)
	if text == "" && !isToolCall {
		return Turn{}, false
	}
	if len(text) > maxLen {
		text = text[:maxLen] + " [truncated]"
	}

	ts := parseTimestamp(rec.Timestamp)
	return Turn{
		UUID:       rec.UUID,
		SessionID:  sessionID,
		TurnIndex:  idx,
		Role:       RoleAssistant,
		Timestamp:  ts,
		Text:       text,
		IsToolCall: isToolCall,
		ParentUUID: rec.ParentUUID,
	}, true
}

// extractContentText extracts plain text from a message.content value.
// When skipToolResults is true (user records), tool_result blocks are skipped.
func extractContentText(raw json.RawMessage, skipToolResults bool) string {
	if len(raw) == 0 {
		return ""
	}

	// Try string form first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}

	// Try array form.
	var blocks []rawContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}

	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		case "tool_result":
			// skip — tool output is not operator utterance
		case "thinking":
			// skip thinking from user records (shouldn't appear but be safe)
		}
	}
	return strings.Join(parts, "\n")
}

// extractAssistantContent extracts text and thinking from assistant message
// content. Also detects whether this is primarily a tool-call turn.
func extractAssistantContent(raw json.RawMessage, maxLen int) (text string, isToolCall bool) {
	if len(raw) == 0 {
		return "", false
	}

	var blocks []rawContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		// Fall back to string form (unlikely for assistant records).
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return strings.TrimSpace(s), false
		}
		return "", false
	}

	var textParts []string
	toolCallCount := 0
	textBlockCount := 0

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				textParts = append(textParts, t)
				textBlockCount++
			}
		case "thinking":
			// Include thinking content — it is substrate-valuable for understanding
			// how the assistant reasoned through a problem.
			if t := strings.TrimSpace(b.Thinking); t != "" {
				textParts = append(textParts, "[thinking] "+t)
			}
		case "tool_use":
			toolCallCount++
		}
	}

	combined := strings.Join(textParts, "\n")
	isToolCall = toolCallCount > 0 && textBlockCount == 0
	return combined, isToolCall
}

// stripSystemReminders removes <system-reminder>...</system-reminder> tags.
func stripSystemReminders(s string) string {
	return strings.TrimSpace(systemReminderRE.ReplaceAllString(s, ""))
}

// parseTimestamp parses an RFC3339 timestamp string. Returns zero time on error.
func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// Try RFC3339Nano and millisecond variants.
		t, err = time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}

// updateTimeBounds adjusts meta.FirstTurnAt / LastTurnAt to include t.
func updateTimeBounds(meta *SessionMeta, t time.Time) {
	if t.IsZero() {
		return
	}
	if meta.FirstTurnAt.IsZero() || t.Before(meta.FirstTurnAt) {
		meta.FirstTurnAt = t
	}
	if meta.LastTurnAt.IsZero() || t.After(meta.LastTurnAt) {
		meta.LastTurnAt = t
	}
}

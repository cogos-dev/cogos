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
	Type       string          `json:"type"`
	UUID       string          `json:"uuid"`
	ParentUUID string          `json:"parentUuid"`
	SessionID  string          `json:"sessionId"`
	Timestamp  string          `json:"timestamp"`
	Message    json.RawMessage `json:"message"`
	Content    string          `json:"content"` // system records use content string
	UserType   string          `json:"userType"`
	Entrypoint string          `json:"entrypoint"`
	// Subtype and LogicalParentUUID are only populated on type:"system"
	// records. A "compact_boundary" subtype record marks an auto-compaction
	// point: Claude Code writes it with parentUuid:null (a fresh DAG root by
	// the raw parentUuid field alone) but names the true continuation point
	// — the last turn preserved across the compaction — in
	// logicalParentUuid. See the rawParents override below.
	Subtype           string `json:"subtype"`
	LogicalParentUUID string `json:"logicalParentUuid"`
	IsSidechain       bool   `json:"isSidechain"`
	Level             string `json:"level"`
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

// rawAITitle is the ai-title record type. The field is aiTitle in real
// Claude Code JSONL (verified against on-disk sessions) — a prior "title"
// tag here meant Title never populated for any Claude Code session (see
// #557 round-4 review HIGH finding).
type rawAITitle struct {
	AITitle string `json:"aiTitle"`
}

// rawCustomTitle is the custom-title record type Claude Code writes when
// the operator explicitly renames a session (distinct from the
// auto-generated ai-title). Verified against on-disk sessions.
type rawCustomTitle struct {
	CustomTitle string `json:"customTitle"`
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
// Returns the byte offset, relative to r's starting position, of the end of
// the last COMPLETE line that was successfully unmarshaled as JSON — never
// the total number of bytes read. A session file can have a
// partially-written last line (the writer is still mid-append when a cycle
// reads it); that line fails json.Unmarshal and is skipped below like any
// other unparseable line, and its bytes must NOT be counted as consumed.
// Callers (indexSession, indexSessionIncremental in provider.go) use this
// return value as the new watermark instead of the stream's end-of-data
// position: recording the watermark past a torn line would let the tail
// fingerprint below it match again once the writer finishes the line,
// taking the incremental fast path and permanently skipping content that
// was never actually parsed, with no self-heal (issue #558 part 1, torn
// last line finding).
//
// ParseSession is purely functional: no global state, no side effects beyond
// the callbacks. Tests can supply a strings.Reader fixture.
func ParseSession(r io.Reader, sessionID string, maxTurnLen int, meta *SessionMeta, callback func(Turn) bool) (int64, error) {
	if maxTurnLen <= 0 {
		maxTurnLen = 8192
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, maxScannerTokenSize), maxScannerTokenSize)

	// consumedOffset tracks the cumulative number of bytes bufio.ScanLines
	// has advanced the stream by, across every token seen so far — the
	// standard split function's own advance value, which (unlike
	// len(scanner.Bytes())) includes the line terminator. Wrapping
	// bufio.ScanLines instead of reimplementing line splitting keeps the
	// existing tokenization behavior (including the maxScannerTokenSize
	// buffer cap) byte-for-byte unchanged; this only observes it.
	var consumedOffset int64
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		advance, token, err = bufio.ScanLines(data, atEOF)
		if err == nil && advance > 0 {
			consumedOffset += int64(advance)
		}
		return advance, token, err
	})

	// lastGoodOffset only advances past a line once it has been
	// successfully unmarshaled as JSON — see the doc comment above.
	var lastGoodOffset int64

	// priorTurnCount captures meta.TurnCount as it stood at entry, before
	// this call's own turns are counted below. For a full parse
	// (indexSession) meta starts zero-valued, so this is 0. For an
	// incremental tail parse (indexSessionIncremental), meta is seeded from
	// prevMeta before ParseSession is called, so this carries the session's
	// TRUE turn count so far even though this call's own turnIndex starts
	// at 0 — a compact_boundary at the very start of a resumed tail must
	// still be treated as mid-file, not head-of-file. See the
	// compactBoundaryFallback field doc comment in types.go.
	priorTurnCount := meta.TurnCount

	// lastUUID tracks the uuid of the most recently seen uuid-carrying
	// record, in file order, so a mid-file compact_boundary can record its
	// nearest preceding record as a fallback candidate (see
	// compactBoundaryFallback). Updated once per uuid-carrying record,
	// after that record's own fallback (if any) is computed, so a boundary
	// never names itself as its own predecessor.
	var lastUUID string

	// hasCustomTitle tracks whether an explicit custom-title record has been
	// seen, regardless of stream order relative to ai-title records: an
	// operator-chosen title always wins over the auto-generated one. See the
	// ai-title/custom-title cases below.
	hasCustomTitle := false

	seenUUIDs := make(map[string]struct{})
	turnIndex := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			// A zero-length line is complete by construction (it can't be a
			// torn record), so it's safe to advance the watermark past it.
			lastGoodOffset = consumedOffset
			continue
		}

		var rec rawRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// Skip unparseable lines — sessions may have partially-written
			// last lines. Deliberately do NOT advance lastGoodOffset: this
			// may be exactly such a line, and the watermark must never move
			// past bytes that were never actually parsed.
			continue
		}
		lastGoodOffset = consumedOffset

		// Record this record's uuid -> parentUuid edge, regardless of
		// whether it goes on to become a Turn. This is the raw DAG that
		// bridgeDroppedParents (threads.go) walks to splice a surviving
		// turn's ParentUUID past tool_result-only user records, type:"system"
		// records, hook outputs, text-less assistant records, and
		// duplicate-uuid records — all of which are otherwise invisible to
		// PartitionThreads and would misclassify their surviving children as
		// fresh thread roots. First-seen wins per uuid, mirroring the
		// uuid-dedup rule below (the turn set anchors on the first
		// occurrence of any uuid).
		if rec.UUID != "" {
			if meta.rawParents == nil {
				meta.rawParents = make(map[string]string)
			}
			if _, seen := meta.rawParents[rec.UUID]; !seen {
				parent := rec.ParentUUID
				// Auto-compaction boundaries carry parentUuid:null, which
				// would otherwise terminate bridgeDroppedParents' upward
				// walk right here and strand the first post-compaction turn
				// as a fresh (spurious) thread root — even though the
				// conversation is continuous. Use the record's own
				// logicalParentUuid instead: it names the last turn Claude
				// Code preserved across the compaction, so the walk
				// continues past the boundary exactly like it does for any
				// other dropped record.
				if rec.Type == "system" && rec.Subtype == "compact_boundary" && rec.LogicalParentUUID != "" {
					parent = rec.LogicalParentUUID

					// logicalParentUuid can still name a uuid absent from the
					// whole file (measured on real data — see
					// resolveCompactBoundaryFallbacks in threads.go). Record
					// the nearest preceding uuid-carrying record as a
					// fallback for that case, but only when this boundary is
					// genuinely mid-file: one seen before any turn has been
					// parsed is a session resumed into a fresh file, where
					// the true parent legitimately lives in a different file
					// this parse never sees, and rooting there is correct.
					if priorTurnCount+turnIndex > 0 && lastUUID != "" {
						if meta.compactBoundaryFallback == nil {
							meta.compactBoundaryFallback = make(map[string]string)
						}
						meta.compactBoundaryFallback[rec.UUID] = lastUUID
					}
				}
				meta.rawParents[rec.UUID] = parent
			}
			lastUUID = rec.UUID
		}

		// Populate session-level metadata from whichever record has it.
		if rec.Entrypoint != "" && meta.Entrypoint == "" {
			meta.Entrypoint = rec.Entrypoint
		}

		switch rec.Type {
		case "ai-title":
			// Extract the session title from the ai-title record. Never
			// overrides an explicit custom-title, regardless of which
			// appeared first in the stream — see hasCustomTitle above.
			if !hasCustomTitle {
				var t rawAITitle
				if err := json.Unmarshal(line, &t); err == nil && t.AITitle != "" {
					meta.Title = t.AITitle
				}
			}

		case "custom-title":
			// An operator-chosen title always wins over ai-title, whether
			// this record was seen before or after one.
			var t rawCustomTitle
			if err := json.Unmarshal(line, &t); err == nil && t.CustomTitle != "" {
				meta.Title = t.CustomTitle
				hasCustomTitle = true
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
				return lastGoodOffset, nil
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
				return lastGoodOffset, nil
			}
			turnIndex++
			meta.TurnCount = turnIndex
		}
	}

	return lastGoodOffset, scanner.Err()
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
		UUID:        rec.UUID,
		SessionID:   sessionID,
		TurnIndex:   idx,
		Role:        RoleUser,
		Timestamp:   ts,
		Text:        text,
		ParentUUID:  rec.ParentUUID,
		IsSidechain: rec.IsSidechain,
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
		UUID:        rec.UUID,
		SessionID:   sessionID,
		TurnIndex:   idx,
		Role:        RoleAssistant,
		Timestamp:   ts,
		Text:        text,
		IsToolCall:  isToolCall,
		ParentUUID:  rec.ParentUUID,
		IsSidechain: rec.IsSidechain,
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

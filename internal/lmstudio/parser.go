// parser.go — decodes one LM Studio *.conversation.json file into a
// SessionID plus an ordered slice of IngestRecord.
//
// Extraction rules (mirroring internal/conversations/parser.go's approach
// for Claude Code, adapted to LM Studio's shape):
//   - Each LSMessage's *selected* version only is read (CurrentlySelected).
//   - type "singleStep": role + concatenated text content blocks.
//   - type "multiStep": walk Steps in order; "contentBlock" steps contribute
//     text blocks (toolCallRequest/toolCallResult are skipped — tool
//     input/output is not operator utterance, matching the CC parser's
//     handling of tool_use/tool_result); "status", "debugInfoBlock", and
//     "toolStatus" steps are skipped entirely.
//   - Messages with no extracted text (e.g. a multiStep turn that is only
//     tool calls with no prose) are dropped — never emitted as empty turns.
//   - Text longer than maxTurnLen is truncated with " [truncated]" (the
//     ingest consumer truncates again at its own limit, but truncating here
//     keeps emitted JSONL lines bounded).
package lmstudio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultMaxTurnLen mirrors internal/conversations.defaultMaxTurnLen. Kept as
// an independent constant since this package must not import
// internal/conversations (producer/consumer are decoupled by the ingest
// surface, not by a Go dependency).
const defaultMaxTurnLen = 8192

// ParseFile reads and decodes one LM Studio conversation.json file at path,
// returning the derived session id and the ordered ingest records for its
// messages. Returns an error only for I/O or top-level JSON decode failure;
// individual malformed/empty messages are skipped rather than erroring the
// whole file.
func ParseFile(path string, maxTurnLen int) (sessionID string, records []IngestRecord, err error) {
	if maxTurnLen <= 0 {
		maxTurnLen = defaultMaxTurnLen
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("lmstudio: read %s: %w", path, err)
	}

	var conv Conversation
	if jsonErr := json.Unmarshal(data, &conv); jsonErr != nil {
		return "", nil, fmt.Errorf("lmstudio: parse %s: %w", path, jsonErr)
	}

	sessionID = SessionIDFromPath(path)
	model := ""
	if conv.LastUsedModel != nil {
		model = conv.LastUsedModel.Identifier
	}

	records = ExtractRecords(conv, sessionID, model, maxTurnLen)
	return sessionID, records, nil
}

// SessionIDFromPath derives a stable session id from the conversation file
// path: "<parent-folder>/<epoch-ms>" (the epoch-ms filename stem is unique
// per LM Studio conversation; the parent folder name is included so two
// folders sharing a stray epoch collision — not expected in practice — still
// resolve to distinct ids).
func SessionIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".conversation.json")
	folder := filepath.Base(filepath.Dir(path))
	return folder + "-" + base
}

// ExtractRecords walks conv.Messages in order and returns one IngestRecord
// per message that has extractable text. turn_index is assigned
// monotonically over the *emitted* records (skipped/empty messages do not
// consume an index), matching the ingest consumer's own "assign monotonic
// turn_index when absent" behavior — we assign explicitly here so re-emission
// is deterministic regardless of consumer defaults.
func ExtractRecords(conv Conversation, sessionID, modelIdentifier string, maxTurnLen int) []IngestRecord {
	if maxTurnLen <= 0 {
		maxTurnLen = defaultMaxTurnLen
	}
	ts := epochMsToRFC3339(conv.CreatedAt)

	var out []IngestRecord
	turnIdx := 0
	for msgIdx, msg := range conv.Messages {
		v := msg.Selected()
		if v == nil {
			continue
		}

		role := normalizeRole(v.Role)
		if role == "" {
			continue
		}

		var text string
		switch v.Type {
		case "singleStep":
			text = joinTextBlocks(v.Content)
		case "multiStep":
			text = joinMultiStepText(v.Steps)
		default:
			// Unknown version type — skip rather than guess.
			continue
		}

		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if len(text) > maxTurnLen {
			text = text[:maxTurnLen] + " [truncated]"
		}

		rec := IngestRecord{
			Schema:    IngestSchema,
			Source:    Source,
			SessionID: sessionID,
			Role:      role,
			Timestamp: ts,
			Text:      text,
			TurnIndex: turnIdx,
			Refs: &IngestRefs{
				StableID: stableID(sessionID, msgIdx),
			},
		}
		if conv.Name != "" {
			rec.SessionTitle = conv.Name
		}
		// LM Studio doesn't carry a per-message model id; attach the
		// session-level lastUsedModel to assistant turns as free-text
		// context by folding it into Identity is a poor fit (Identity means
		// operator identity elsewhere in the observatory), so we leave it
		// out of the v0.1 record. Model provenance is instead available via
		// the session_title tie-back for now; a v0.2 mapping could add a
		// dedicated field.
		_ = modelIdentifier

		out = append(out, rec)
		turnIdx++
	}
	return out
}

// joinTextBlocks concatenates all "text" content blocks, skipping
// tool-call/tool-result blocks (mirrors CC parser's tool_result/tool_use skip).
func joinTextBlocks(blocks []LSContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// joinMultiStepText walks steps in order, extracting text from "contentBlock"
// steps only. "status", "debugInfoBlock", and "toolStatus" steps carry no
// operator-visible prose and are skipped.
func joinMultiStepText(steps []LSStep) string {
	var parts []string
	for _, s := range steps {
		if s.Type != "contentBlock" {
			continue
		}
		if t := joinTextBlocks(s.Content); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// normalizeRole maps LM Studio's role strings to the ingest schema's role
// vocabulary (user/assistant/tool/system). Unrecognized roles return "" so
// the caller skips the message rather than guessing.
func normalizeRole(role string) string {
	switch role {
	case "user", "assistant", "tool", "system":
		return role
	default:
		return ""
	}
}

// stableID builds a per-message dedup key: "<source>:<session_id>:<msgIdx>".
// Re-parsing the same file always yields the same stable_id for the same
// message position, so re-emission is idempotent from the consumer's
// perspective (ingestAccumulator dedups by refs.stable_id verbatim).
func stableID(sessionID string, msgIdx int) string {
	return fmt.Sprintf("%s:%s:%d", Source, sessionID, msgIdx)
}

// epochMsToRFC3339 converts an epoch-milliseconds timestamp to RFC3339. A
// zero or negative input returns the Unix epoch in RFC3339 form rather than
// an empty string, since the ingest consumer rejects records with an empty
// timestamp field.
func epochMsToRFC3339(epochMs int64) string {
	if epochMs <= 0 {
		return time.Unix(0, 0).UTC().Format(time.RFC3339)
	}
	sec := epochMs / 1000
	nsec := (epochMs % 1000) * int64(time.Millisecond)
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339)
}

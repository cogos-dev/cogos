package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	claudeCodeSourceChannel = "claude-code"
	claudeCodeNormalizedBy  = "tailer-claude-code"
)

// ClaudeCodeTailer tails Claude Code JSONL logs and emits normalized CogBlocks.
type ClaudeCodeTailer struct {
	Watcher *FileWatcher
}

func (t *ClaudeCodeTailer) Name() string { return claudeCodeSourceChannel }

func (t *ClaudeCodeTailer) Tail(ctx context.Context, path string, out chan<- CogBlock) error {
	if out == nil {
		return errors.New("claude code tailer: nil output channel")
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("claude code tailer: empty path")
	}

	watcher := t.Watcher
	if watcher == nil {
		watcher = NewFileWatcher(DefaultFileWatcherPollInterval)
	}

	return watcher.Watch(ctx, path, func(line []byte) error {
		if len(strings.TrimSpace(string(line))) == 0 {
			return nil
		}

		block, err := normalizeClaudeCodeLine(line)
		if err != nil {
			return err
		}

		select {
		case out <- block:
			return nil
		case <-ctx.Done():
			return nil
		}
	})
}

type claudeCodeLogLine struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Timestamp  json.RawMessage `json:"timestamp"`
	ToolUse    json.RawMessage `json:"tool_use"`
	ToolResult json.RawMessage `json:"tool_result"`
	Type       string          `json:"type"`
}

func normalizeClaudeCodeLine(line []byte) (CogBlock, error) {
	var entry claudeCodeLogLine
	if err := json.Unmarshal(line, &entry); err != nil {
		return CogBlock{}, fmt.Errorf("parse claude code jsonl line: %w", err)
	}

	now := time.Now().UTC()
	blockTime := parseClaudeCodeTimestamp(entry.Timestamp)
	if blockTime.IsZero() {
		blockTime = now
	}

	content := parseClaudeCodeContent(entry.Content)
	role := strings.TrimSpace(entry.Role)

	block := CogBlock{
		ID:              uuid.New().String(),
		Timestamp:       blockTime,
		SourceChannel:   claudeCodeSourceChannel,
		SourceTransport: "jsonl",
		SourceIdentity:  role,
		Kind:            classifyClaudeCodeKind(entry),
		RawPayload:      append(json.RawMessage(nil), line...),
		Messages: []ProviderMessage{{
			Role:    role,
			Content: content,
		}},
		Provenance: BlockProvenance{
			OriginChannel: claudeCodeSourceChannel,
			IngestedAt:    now,
			NormalizedBy:  claudeCodeNormalizedBy,
		},
		TrustContext: TrustContext{
			Authenticated: true,
			TrustScore:    1.0,
			Scope:         "local",
		},
	}
	return block, nil
}

func classifyClaudeCodeKind(entry claudeCodeLogLine) CogBlockKind {
	eventType := strings.TrimSpace(entry.Type)
	switch {
	case hasJSONValue(entry.ToolUse), eventType == "tool_use":
		return BlockToolCall
	case hasJSONValue(entry.ToolResult), eventType == "tool_result":
		return BlockToolResult
	default:
		return BlockMessage
	}
}

func hasJSONValue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func parseClaudeCodeContent(raw json.RawMessage) string {
	if !hasJSONValue(raw) {
		return ""
	}

	var contentText string
	if err := json.Unmarshal(raw, &contentText); err == nil {
		return contentText
	}

	var parts []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		texts := make([]string, 0, len(parts))
		for _, part := range parts {
			switch {
			case strings.TrimSpace(part.Text) != "":
				texts = append(texts, part.Text)
			case strings.TrimSpace(part.Content) != "":
				texts = append(texts, part.Content)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		if text := decodeOptionalJSONString(obj["text"]); text != "" {
			return text
		}
		if text := decodeOptionalJSONString(obj["content"]); text != "" {
			return text
		}
	}

	return strings.TrimSpace(string(raw))
}

func decodeOptionalJSONString(raw json.RawMessage) string {
	if !hasJSONValue(raw) {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return ""
	}
	return text
}

func parseClaudeCodeTimestamp(raw json.RawMessage) time.Time {
	if !hasJSONValue(raw) {
		return time.Time{}
	}

	var tsString string
	if err := json.Unmarshal(raw, &tsString); err == nil {
		tsString = strings.TrimSpace(tsString)
		if tsString == "" {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if ts, err := time.Parse(layout, tsString); err == nil {
				return ts.UTC()
			}
		}
		if unix, err := strconv.ParseInt(tsString, 10, 64); err == nil {
			return unixToTime(unix)
		}
	}

	var unixFloat float64
	if err := json.Unmarshal(raw, &unixFloat); err == nil {
		return unixToTime(int64(unixFloat))
	}

	return time.Time{}
}

func unixToTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	if value >= 1_000_000_000_000 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}

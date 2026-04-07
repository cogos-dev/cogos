package engine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const defaultConsolidationEventLimit = 100

type ConsolidationAction struct {
	WorkspaceRoot string
	MaxEvents     int
	Now           func() time.Time
}

type consolidationGroupKey struct {
	sessionID string
	threadID  string
}

type consolidationEvent struct {
	env       EventEnvelope
	timestamp time.Time
	threadID  string
}

type archivedSessionRange struct {
	minEventCount int
	maxEventCount int
}

func (a ConsolidationAction) Run() (int, error) {
	workspaceRoot := a.WorkspaceRoot
	if workspaceRoot == "" {
		return 0, fmt.Errorf("consolidation workspace root is empty")
	}

	limit := a.MaxEvents
	if limit <= 0 {
		limit = defaultConsolidationEventLimit
	}

	nowFn := a.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}

	events, summaries, err := readLedgerEvents(workspaceRoot)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}

	sort.Slice(events, func(i, j int) bool {
		return events[i].timestamp.After(events[j].timestamp)
	})
	if len(events) > limit {
		events = events[:limit]
	}

	groups := make(map[consolidationGroupKey][]consolidationEvent)
	for _, event := range events {
		key := consolidationGroupKey{
			sessionID: event.env.HashedPayload.SessionID,
			threadID:  event.threadID,
		}
		groups[key] = append(groups[key], event)
	}

	consolidatedSessions := make(map[string]struct{})
	archivedRanges := make(map[string]archivedSessionRange)
	for key, group := range groups {
		if len(group) <= 10 {
			continue
		}

		sort.Slice(group, func(i, j int) bool {
			return group[i].timestamp.Before(group[j].timestamp)
		})

		lastSummaryAt, ok := summaries[key]
		if ok && !group[len(group)-1].timestamp.After(lastSummaryAt) {
			continue
		}

		summaryData := buildConsolidationSummary(key, group)
		block, err := newConsolidationBlock(workspaceRoot, key, summaryData, nowFn())
		if err != nil {
			return 0, err
		}
		if err := appendConsolidationSummary(workspaceRoot, block, summaryData); err != nil {
			return 0, err
		}

		consolidatedSessions[key.sessionID] = struct{}{}
		if current, exists := archivedRanges[key.sessionID]; exists {
			if len(group) < current.minEventCount {
				current.minEventCount = len(group)
			}
			if len(group) > current.maxEventCount {
				current.maxEventCount = len(group)
			}
			archivedRanges[key.sessionID] = current
		} else {
			archivedRanges[key.sessionID] = archivedSessionRange{minEventCount: len(group), maxEventCount: len(group)}
		}
	}

	if len(consolidatedSessions) > 0 {
		if err := appendArchivedSessionsEvent(workspaceRoot, consolidatedSessions, archivedRanges, nowFn()); err != nil {
			return 0, err
		}
	}

	return len(consolidatedSessions), nil
}

func readLedgerEvents(workspaceRoot string) ([]consolidationEvent, map[consolidationGroupKey]time.Time, error) {
	ledgerRoot := filepath.Join(workspaceRoot, ".cog", "ledger")
	entries, err := os.ReadDir(ledgerRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, map[consolidationGroupKey]time.Time{}, nil
		}
		return nil, nil, fmt.Errorf("read ledger root: %w", err)
	}

	var events []consolidationEvent
	summaries := make(map[consolidationGroupKey]time.Time)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		eventsPath := filepath.Join(ledgerRoot, entry.Name(), "events.jsonl")
		f, err := os.Open(eventsPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("open ledger %s: %w", entry.Name(), err)
		}

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			var env EventEnvelope
			if err := json.Unmarshal(line, &env); err != nil {
				continue
			}
			if env.HashedPayload.SessionID == "" {
				env.HashedPayload.SessionID = entry.Name()
			}

			ts, err := time.Parse(time.RFC3339, env.HashedPayload.Timestamp)
			if err != nil {
				continue
			}

			if env.HashedPayload.Type == "memory.consolidation" {
				key, lastTimestamp, ok := parseConsolidationSummary(env)
				if ok {
					if current, exists := summaries[key]; !exists || lastTimestamp.After(current) {
						summaries[key] = lastTimestamp
					}
				}
				continue
			}
			if env.HashedPayload.Type == "consolidation.archived" {
				continue
			}

			events = append(events, consolidationEvent{
				env:       env,
				timestamp: ts,
				threadID:  eventThreadID(env),
			})
		}
		if err := scanner.Err(); err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("scan ledger %s: %w", entry.Name(), err)
		}
		_ = f.Close()
	}

	return events, summaries, nil
}

func parseConsolidationSummary(env EventEnvelope) (consolidationGroupKey, time.Time, bool) {
	data := env.HashedPayload.Data
	threadID, _ := data["thread_id"].(string)
	summary, ok := data["summary"].(map[string]interface{})
	if !ok {
		return consolidationGroupKey{}, time.Time{}, false
	}
	lastTimestamp, _ := summary["last_timestamp"].(string)
	parsed, err := time.Parse(time.RFC3339, lastTimestamp)
	if err != nil {
		return consolidationGroupKey{}, time.Time{}, false
	}
	return consolidationGroupKey{sessionID: env.HashedPayload.SessionID, threadID: threadID}, parsed, true
}

func eventThreadID(env EventEnvelope) string {
	if threadID, _ := env.HashedPayload.Data["thread_id"].(string); threadID != "" {
		return threadID
	}
	return ""
}

func buildConsolidationSummary(key consolidationGroupKey, group []consolidationEvent) map[string]interface{} {
	sources := make(map[string]struct{})
	var texts []string
	for _, event := range group {
		source := eventSource(event.env)
		if source != "" {
			sources[source] = struct{}{}
		}
		texts = append(texts, extractEventTexts(event.env)...)
	}

	uniqueSources := make([]string, 0, len(sources))
	for source := range sources {
		uniqueSources = append(uniqueSources, source)
	}
	sort.Strings(uniqueSources)

	summary := map[string]interface{}{
		"session_id":      key.sessionID,
		"first_timestamp": group[0].timestamp.UTC().Format(time.RFC3339),
		"last_timestamp":  group[len(group)-1].timestamp.UTC().Format(time.RFC3339),
		"event_count":     len(group),
		"unique_sources":  uniqueSources,
		"key_topics":      extractTopics(texts),
	}
	if key.threadID != "" {
		summary["thread_id"] = key.threadID
	}
	return summary
}

func newConsolidationBlock(workspaceRoot string, key consolidationGroupKey, summary map[string]interface{}, now time.Time) (*CogBlock, error) {
	raw, err := json.Marshal(summary)
	if err != nil {
		return nil, fmt.Errorf("marshal consolidation summary: %w", err)
	}

	return &CogBlock{
		ID:              uuid.NewString(),
		Timestamp:       now.UTC(),
		SessionID:       key.sessionID,
		ThreadID:        key.threadID,
		SourceChannel:   "internal",
		SourceTransport: "direct",
		SourceIdentity:  "consolidation",
		WorkspaceID:     filepath.Base(workspaceRoot),
		Kind:            BlockSystemEvent,
		RawPayload:      raw,
		Messages:        []ProviderMessage{{Role: "system", Content: "memory consolidation summary"}},
		Provenance: BlockProvenance{
			OriginSession: key.sessionID,
			OriginChannel: "internal",
			IngestedAt:    now.UTC(),
			NormalizedBy:  "consolidation",
		},
		TrustContext: TrustContext{
			Authenticated: true,
			TrustScore:    1.0,
			Scope:         "local",
		},
	}, nil
}

func appendConsolidationSummary(workspaceRoot string, block *CogBlock, summary map[string]interface{}) error {
	data := map[string]interface{}{
		"block_id":                 block.ID,
		"thread_id":                block.ThreadID,
		"kind":                     string(block.Kind),
		"source_channel":           block.SourceChannel,
		"source_transport":         block.SourceTransport,
		"source_identity":          block.SourceIdentity,
		"workspace_id":             block.WorkspaceID,
		"provenance_normalized_by": block.Provenance.NormalizedBy,
		"summary":                  summary,
	}

	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      "memory.consolidation",
			Timestamp: block.Timestamp.UTC().Format(time.RFC3339),
			SessionID: block.SessionID,
			Data:      data,
		},
		Metadata: EventMetadata{Source: "kernel-v3"},
	}

	if err := AppendEvent(workspaceRoot, block.SessionID, env); err != nil {
		return fmt.Errorf("append consolidation summary: %w", err)
	}
	block.LedgerRef = env.Metadata.Hash
	return nil
}

func appendArchivedSessionsEvent(workspaceRoot string, sessions map[string]struct{}, ranges map[string]archivedSessionRange, now time.Time) error {
	sessionIDs := make([]string, 0, len(sessions))
	for sessionID := range sessions {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)

	archived := make([]interface{}, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		eventRange := ranges[sessionID]
		archived = append(archived, map[string]interface{}{
			"session_id":      sessionID,
			"min_event_count": eventRange.minEventCount,
			"max_event_count": eventRange.maxEventCount,
		})
	}

	env := &EventEnvelope{
		Metadata: EventMetadata{Source: "kernel-v3"},
	}

	for _, sessionID := range sessionIDs {
		env.HashedPayload = EventPayload{
			Type:      "consolidation.archived",
			Timestamp: now.UTC().Format(time.RFC3339),
			SessionID: sessionID,
			Data: map[string]interface{}{
				"archived_sessions": archived,
			},
		}
		if err := AppendEvent(workspaceRoot, sessionID, env); err != nil {
			return fmt.Errorf("append archived sessions: %w", err)
		}
	}
	return nil
}

func ArchivedSessions(workspaceRoot, sessionID string) (map[string]struct{}, error) {
	archived := make(map[string]struct{})
	path := filepath.Join(workspaceRoot, ".cog", "ledger", sessionID, "events.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return archived, nil
		}
		return nil, fmt.Errorf("open ledger %s: %w", sessionID, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var env EventEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		if env.HashedPayload.Type != "consolidation.archived" {
			continue
		}
		items, _ := env.HashedPayload.Data["archived_sessions"].([]interface{})
		for _, item := range items {
			entry, _ := item.(map[string]interface{})
			if archivedSessionID, _ := entry["session_id"].(string); archivedSessionID != "" {
				archived[archivedSessionID] = struct{}{}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan ledger %s: %w", sessionID, err)
	}

	return archived, nil
}

func eventSource(env EventEnvelope) string {
	if env.Metadata.Source != "" {
		return env.Metadata.Source
	}
	if source, _ := env.HashedPayload.Data["source_identity"].(string); source != "" {
		return source
	}
	if source, _ := env.HashedPayload.Data["source_channel"].(string); source != "" {
		return source
	}
	return env.HashedPayload.Type
}

func extractEventTexts(env EventEnvelope) []string {
	var texts []string
	for key, value := range env.HashedPayload.Data {
		collectEventTexts(key, value, &texts)
	}
	return texts
}

func collectEventTexts(key string, value interface{}, texts *[]string) {
	switch typed := value.(type) {
	case string:
		if isTextField(key) {
			*texts = append(*texts, typed)
		}
	case []interface{}:
		for _, item := range typed {
			collectEventTexts(key, item, texts)
		}
	case map[string]interface{}:
		for nestedKey, nestedValue := range typed {
			collectEventTexts(nestedKey, nestedValue, texts)
		}
	}
}

func isTextField(key string) bool {
	key = strings.ToLower(key)
	switch key {
	case "content", "message", "messages", "prompt", "query", "text":
		return true
	default:
		return false
	}
}

func extractTopics(texts []string) []string {
	counts := make(map[string]int)
	for _, text := range texts {
		for _, token := range tokenize(text) {
			if _, stop := consolidationStopwords[token]; stop {
				continue
			}
			counts[token]++
		}
	}

	type topicCount struct {
		topic string
		count int
	}
	list := make([]topicCount, 0, len(counts))
	for topic, count := range counts {
		list = append(list, topicCount{topic: topic, count: count})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count == list[j].count {
			return list[i].topic < list[j].topic
		}
		return list[i].count > list[j].count
	})

	if len(list) > 5 {
		list = list[:5]
	}
	result := make([]string, 0, len(list))
	for _, item := range list {
		result = append(result, item.topic)
	}
	return result
}

func tokenize(text string) []string {
	parts := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) < 3 {
			continue
		}
		result = append(result, part)
	}
	return result
}

var consolidationStopwords = map[string]struct{}{
	"and": {}, "are": {}, "but": {}, "for": {}, "from": {}, "has": {},
	"have": {}, "into": {}, "not": {}, "that": {}, "the": {}, "their": {},
	"them": {}, "this": {}, "was": {}, "with": {}, "you": {},
}

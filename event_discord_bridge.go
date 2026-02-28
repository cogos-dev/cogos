// event_discord_bridge.go - Bridges CogOS bus events to a Discord channel via webhook.
//
// EventDiscordBridge subscribes to bus events through the busSessionManager's
// onEvent callback and posts formatted summaries to a Discord #events channel
// using a standard Discord webhook URL.
//
// Event formatting:
//   chat.request        -> message icon + agent + first 50 chars
//   chat.response       -> success icon + agent + tokens + duration
//   chat.error          -> error icon + agent + error message
//   tool.invoke         -> tool icon + caller -> target + tool name
//   tool.result         -> result icon + executor + tool + duration
//   agent.capabilities  -> broadcast icon + agent + tool count

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EventDiscordBridge subscribes to bus events and forwards summaries
// to a Discord channel via webhook.
type EventDiscordBridge struct {
	webhookURL string
	busBroker  *busEventBroker
	httpClient *http.Client
	mu         sync.Mutex
	running    bool
	stopCh     chan struct{}
	closeOnce  sync.Once
	eventCh    chan *busEventWithID

	// Rate limiting: batch events and flush periodically to avoid
	// hammering the webhook endpoint.
	flushInterval time.Duration
	maxBatchSize  int

	// rateLimitUntil tracks the earliest time we may send the next
	// request after receiving a 429.  Protected by mu.
	rateLimitUntil time.Time
}

// busEventWithID pairs a bus event with its bus ID for formatting context.
type busEventWithID struct {
	busID string
	event *CogBlock
}

// discordWebhookPayload is the JSON body for a Discord webhook POST.
type discordWebhookPayload struct {
	Content  string `json:"content"`
	Username string `json:"username,omitempty"`
}

// NewEventDiscordBridge creates a bridge that posts bus event summaries to Discord.
// The webhookURL should be a Discord webhook URL for the #events channel.
// The broker is used to subscribe to all bus events.
func NewEventDiscordBridge(webhookURL string, broker *busEventBroker) *EventDiscordBridge {
	return &EventDiscordBridge{
		webhookURL: webhookURL,
		busBroker:  broker,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		eventCh:    make(chan *busEventWithID, 256),
		stopCh:     make(chan struct{}),

		flushInterval: 2 * time.Second,
		maxBatchSize:  10,
	}
}

// Start begins listening for bus events and posting summaries to Discord.
// It runs a background goroutine that batches events and flushes them
// periodically to respect Discord rate limits.
func (b *EventDiscordBridge) Start() {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return
	}
	b.running = true
	b.stopCh = make(chan struct{})
	b.closeOnce = sync.Once{}
	b.mu.Unlock()

	go b.runLoop()
	log.Printf("[event-bridge] Discord event bridge started (flush_interval=%s, max_batch=%d)",
		b.flushInterval, b.maxBatchSize)
}

// Stop halts the bridge and drains any pending events.
func (b *EventDiscordBridge) Stop() {
	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return
	}
	b.running = false
	b.mu.Unlock()

	b.closeOnce.Do(func() { close(b.stopCh) })
	log.Printf("[event-bridge] Discord event bridge stopped")
}

// HandleEvent is the callback to wire into busSessionManager via AddEventHandler.
// It enqueues the event for batched posting. Non-blocking: drops if channel full.
func (b *EventDiscordBridge) HandleEvent(busID string, evt *CogBlock) {
	select {
	case b.eventCh <- &busEventWithID{busID: busID, event: evt}:
	default:
		// Channel full — drop event rather than block the bus pipeline
		log.Printf("[event-bridge] dropped event (channel full): bus=%s type=%s", busID, evt.Type)
	}
}

// runLoop is the main event processing loop. It collects events and flushes
// them as a batched Discord message either when the batch is full or when
// the flush interval elapses.
func (b *EventDiscordBridge) runLoop() {
	ticker := time.NewTicker(b.flushInterval)
	defer ticker.Stop()

	var batch []string

	for {
		select {
		case <-b.stopCh:
			// Flush remaining events before exit
			if len(batch) > 0 {
				b.postToDiscord(batch)
			}
			return

		case wrapped := <-b.eventCh:
			line := formatBusEvent(wrapped.event)
			if line == "" {
				continue
			}
			batch = append(batch, line)
			if len(batch) >= b.maxBatchSize {
				b.postToDiscord(batch)
				batch = nil
			}

		case <-ticker.C:
			if len(batch) > 0 {
				b.postToDiscord(batch)
				batch = nil
			}
		}
	}
}

// postToDiscord sends a batch of formatted event lines to the Discord webhook.
// If Discord responds with 429 (rate limited), the method parses the
// Retry-After header, sleeps for the indicated duration, and retries once.
// A rateLimitUntil timestamp prevents further requests while the cooldown
// is active, so back-to-back flushes don't hammer the endpoint.
func (b *EventDiscordBridge) postToDiscord(lines []string) {
	if b.webhookURL == "" || len(lines) == 0 {
		return
	}

	// Honour any active rate-limit cooldown before sending.
	b.mu.Lock()
	wait := time.Until(b.rateLimitUntil)
	b.mu.Unlock()
	if wait > 0 {
		log.Printf("[event-bridge] rate-limit cooldown active, sleeping %s", wait.Truncate(time.Millisecond))
		time.Sleep(wait)
	}

	// Join lines into a single code block for clean rendering
	content := "```\n" + strings.Join(lines, "\n") + "\n```"

	// Discord message content limit is 2000 characters
	if len(content) > 2000 {
		content = content[:1997] + "```"
	}

	body, err := json.Marshal(discordWebhookPayload{
		Content:  content,
		Username: "CogOS Bus",
	})
	if err != nil {
		log.Printf("[event-bridge] failed to marshal webhook payload: %v", err)
		return
	}

	// First attempt
	resp, err := b.httpClient.Post(b.webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[event-bridge] webhook POST failed: %v", err)
		return
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		resp.Body.Close()

		// Record the cooldown so other flushes also wait.
		b.mu.Lock()
		b.rateLimitUntil = time.Now().Add(retryAfter)
		b.mu.Unlock()

		log.Printf("[event-bridge] Discord rate limited (429), retrying after %s", retryAfter.Truncate(time.Millisecond))
		time.Sleep(retryAfter)

		// Retry once with the same payload.
		resp, err = b.httpClient.Post(b.webhookURL, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("[event-bridge] webhook retry POST failed: %v", err)
			return
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter2 := parseRetryAfter(resp.Header.Get("Retry-After"))
			resp.Body.Close()

			b.mu.Lock()
			b.rateLimitUntil = time.Now().Add(retryAfter2)
			b.mu.Unlock()

			log.Printf("[event-bridge] Discord still rate limited after retry (429), events dropped")
			return
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[event-bridge] Discord webhook returned status %d", resp.StatusCode)
	}
}

// parseRetryAfter parses the Retry-After header value from a Discord 429
// response.  Discord sends this as seconds (possibly fractional, e.g. "0.5").
// Returns a minimum backoff of 1 second if the header is missing or
// unparseable, capped at 60 seconds to avoid unbounded sleeps.
func parseRetryAfter(header string) time.Duration {
	const (
		minBackoff = 1 * time.Second
		maxBackoff = 60 * time.Second
	)

	if header == "" {
		return minBackoff
	}

	secs, err := strconv.ParseFloat(header, 64)
	if err != nil || secs <= 0 {
		return minBackoff
	}

	// Convert to millisecond-precision duration.
	d := time.Duration(math.Ceil(secs*1000)) * time.Millisecond
	if d < minBackoff {
		return minBackoff
	}
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// formatBusEvent creates a one-line summary for a bus event.
// Format: [HH:MM:SS] emoji eventType | context | summary
// Returns empty string for event types we don't format.
func formatBusEvent(evt *CogBlock) string {
	if evt == nil {
		return ""
	}

	ts := formatEventTimestamp(evt.Ts)

	switch evt.Type {
	case BlockChatRequest:
		// Prefer agent field (from UCP identity) over origin for display
		agent := extractPayloadString(evt.Payload, "agent", "")
		if agent == "" {
			agent = extractPayloadString(evt.Payload, "origin", evt.From)
		}
		content := extractPayloadString(evt.Payload, "content", "")
		content = sanitizeAndTruncate(content, 50)
		return fmt.Sprintf("[%s] \U0001F504 chat.request | %s | \"%s\"", ts, agent, content)

	case BlockChatResponse:
		agent := evt.From
		tokens := extractPayloadInt(evt.Payload, "tokens_used")
		duration := extractPayloadInt64(evt.Payload, "duration_ms")
		parts := []string{}
		if tokens > 0 {
			parts = append(parts, fmt.Sprintf("%d tokens", tokens))
		}
		if duration > 0 {
			parts = append(parts, fmt.Sprintf("%dms", duration))
		}
		// Show cache hit tokens when present
		cacheRead := extractPayloadInt(evt.Payload, "cache_read_tokens")
		if cacheRead > 0 {
			parts = append(parts, fmt.Sprintf("cache:%d", cacheRead))
		}
		// Show finish reason when non-default
		finishReason := extractPayloadString(evt.Payload, "finish_reason", "")
		if finishReason != "" && finishReason != "stop" {
			parts = append(parts, finishReason)
		}
		summary := strings.Join(parts, ", ")
		if summary == "" {
			summary = "completed"
		}
		return fmt.Sprintf("[%s] \u2705 chat.response | %s | %s", ts, agent, summary)

	case BlockChatError:
		agent := evt.From
		errType := extractPayloadString(evt.Payload, "error_type", "")
		errMsg := extractPayloadString(evt.Payload, "error", "unknown error")
		errMsg = sanitizeAndTruncate(errMsg, 80)
		if errType != "" {
			return fmt.Sprintf("[%s] \u274C chat.error | %s | [%s] %s", ts, agent, errType, errMsg)
		}
		return fmt.Sprintf("[%s] \u274C chat.error | %s | %s", ts, agent, errMsg)

	case BlockToolInvoke:
		caller := extractPayloadString(evt.Payload, "callerAgent", evt.From)
		target := extractPayloadString(evt.Payload, "targetAgent", "any")
		tool := extractPayloadString(evt.Payload, "tool", "unknown")
		return fmt.Sprintf("[%s] \U0001F6E0\uFE0F tool.invoke | %s \u2192 %s | %s", ts, caller, target, tool)

	case BlockToolResult:
		executor := extractPayloadString(evt.Payload, "executedBy", evt.From)
		tool := extractPayloadString(evt.Payload, "tool", "")
		duration := extractPayloadInt64(evt.Payload, "durationMs")
		summary := tool
		if duration > 0 {
			summary = fmt.Sprintf("%s (%dms)", tool, duration)
		}
		if summary == "" {
			summary = "completed"
		}
		return fmt.Sprintf("[%s] \U0001F4E6 tool.result | %s | %s", ts, executor, summary)

	case BlockAgentCapabilities:
		agentID := extractPayloadString(evt.Payload, "agentId", evt.From)
		toolCount := countCapabilityTools(evt.Payload)
		return fmt.Sprintf("[%s] \U0001F4E1 agent.capabilities | %s | %d tools", ts, agentID, toolCount)

	case BlockSystemStartup:
		shortHash := evt.Hash
		if len(shortHash) > 8 {
			shortHash = shortHash[:8]
		}
		return fmt.Sprintf("[%s] \U0001F7E2 system.startup | %s | %s", ts, evt.From, shortHash)

	case BlockSystemShutdown:
		return fmt.Sprintf("[%s] \U0001F534 system.shutdown | %s", ts, evt.From)

	case BlockSystemHealth:
		return fmt.Sprintf("[%s] \U0001F49A system.health | %s", ts, evt.From)

	default:
		// Channel messages bridged from OpenClaw (e.g. "discord.message", "telegram.message")
		if strings.HasSuffix(evt.Type, BlockChannelMessageSuffix) && evt.Type != BlockMessage {
			sender := extractPayloadString(evt.Payload, "username", "")
			if sender == "" {
				sender = extractPayloadString(evt.Payload, "from", evt.From)
			}
			channel := extractPayloadString(evt.Payload, "channel_name", "")
			if channel == "" {
				channel = extractPayloadString(evt.Payload, "channel", "")
			}
			content := extractPayloadString(evt.Payload, "content", "")
			content = sanitizeAndTruncate(content, 60)
			prefix := evt.Type[:len(evt.Type)-len(BlockChannelMessageSuffix)]
			if channel != "" {
				return fmt.Sprintf("[%s] \U0001F4AC %s.message | #%s | %s: \"%s\"", ts, prefix, channel, sender, content)
			}
			return fmt.Sprintf("[%s] \U0001F4AC %s.message | %s | \"%s\"", ts, prefix, sender, content)
		}

		// Unknown event type — emit a generic line
		return fmt.Sprintf("[%s] \u2022 %s | %s", ts, evt.Type, evt.From)
	}
}

// --- Helper functions ---

// formatEventTimestamp extracts HH:MM:SS from an RFC3339 timestamp.
func formatEventTimestamp(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		// Fall back to raw timestamp or current time
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return time.Now().UTC().Format("15:04:05")
		}
	}
	return t.Format("15:04:05")
}

// extractPayloadString extracts a string value from a payload map with a fallback.
func extractPayloadString(payload map[string]interface{}, key, fallback string) string {
	if payload == nil {
		return fallback
	}
	if v, ok := payload[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return fallback
}

// extractPayloadInt extracts an integer value from a payload map.
func extractPayloadInt(payload map[string]interface{}, key string) int {
	if payload == nil {
		return 0
	}
	v, ok := payload[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// extractPayloadInt64 extracts an int64 value from a payload map.
func extractPayloadInt64(payload map[string]interface{}, key string) int64 {
	if payload == nil {
		return 0
	}
	v, ok := payload[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// countCapabilityTools counts the number of allowed tools in a capabilities payload.
func countCapabilityTools(payload map[string]interface{}) int {
	if payload == nil {
		return 0
	}
	tools, ok := payload["tools"]
	if !ok {
		return 0
	}
	toolsMap, ok := tools.(map[string]interface{})
	if !ok {
		return 0
	}
	allow, ok := toolsMap["allow"]
	if !ok {
		return 0
	}
	allowSlice, ok := allow.([]interface{})
	if !ok {
		return 0
	}
	return len(allowSlice)
}

// sanitizeAndTruncate strips newlines (for single-line display) then shortens
// to maxLen characters, appending "..." if truncated.
func sanitizeAndTruncate(s string, maxLen int) string {
	// Remove newlines for single-line display
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")

	return truncate(s, maxLen)
}

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// computeBlockHash computes the V2 content-addressed hash for a CogBlock.
// It hashes the full canonical form: all fields except hash and sig.
// The canonical form is a JSON object with fields in a deterministic order.
func computeBlockHash(block *CogBlock) string {
	canonical := struct {
		V       int                    `json:"v"`
		BusID   string                 `json:"bus_id,omitempty"`
		Seq     int                    `json:"seq,omitempty"`
		Ts      string                 `json:"ts"`
		From    string                 `json:"from"`
		To      string                 `json:"to,omitempty"`
		Type    string                 `json:"type"`
		Payload map[string]interface{} `json:"payload"`
		Prev    []string               `json:"prev,omitempty"`
		Merkle  string                 `json:"merkle,omitempty"`
		Size    int                    `json:"size,omitempty"`
	}{
		V: block.V, BusID: block.BusID, Seq: block.Seq,
		Ts: block.Ts, From: block.From, To: block.To,
		Type: block.Type, Payload: block.Payload,
		Prev: block.Prev, Merkle: block.Merkle, Size: block.Size,
	}
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// busEventHandler is a named handler for bus events (CogBlocks).
type busEventHandler struct {
	name    string
	handler func(busID string, block *CogBlock)
}

// busSessionManager manages CogBus operations for the chat pipeline.
// It handles bus creation, event appending, and reading event history.
type busSessionManager struct {
	mu            sync.Mutex
	workspaceRoot string
	eventHandlers []busEventHandler // named handler list
}

func newBusSessionManager(root string) *busSessionManager {
	return &busSessionManager{workspaceRoot: root}
}

// AddEventHandler registers a named handler for bus events.
// Handlers are called in registration order when a bus event is appended.
func (m *busSessionManager) AddEventHandler(name string, fn func(busID string, block *CogBlock)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.eventHandlers = append(m.eventHandlers, busEventHandler{name: name, handler: fn})
}

// RemoveEventHandler removes a named handler by name.
func (m *busSessionManager) RemoveEventHandler(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, h := range m.eventHandlers {
		if h.name == name {
			m.eventHandlers = append(m.eventHandlers[:i], m.eventHandlers[i+1:]...)
			return
		}
	}
}

// busesDir returns the path to the buses state directory.
func (m *busSessionManager) busesDir() string {
	return filepath.Join(m.workspaceRoot, ".cog", ".state", "buses")
}

// registryPath returns the path to the bus registry file.
func (m *busSessionManager) registryPath() string {
	return filepath.Join(m.busesDir(), "registry.json")
}

// eventsPath returns the path to a bus's events JSONL file.
func (m *busSessionManager) eventsPath(busID string) string {
	return filepath.Join(m.busesDir(), busID, "events.jsonl")
}

// createChatBus creates a new bus for a chat conversation.
// The bus ID is derived from the session ID: bus_chat_{sessionID}.
func (m *busSessionManager) createChatBus(sessionID, origin string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	busID := fmt.Sprintf("bus_chat_%s", sessionID)

	// Create bus directory
	busDir := filepath.Join(m.busesDir(), busID)
	if err := os.MkdirAll(busDir, 0755); err != nil {
		return "", fmt.Errorf("create bus dir: %w", err)
	}

	// Create events file if it doesn't exist
	eventsFile := filepath.Join(busDir, "events.jsonl")
	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		f, err := os.Create(eventsFile)
		if err != nil {
			return "", fmt.Errorf("create events file: %w", err)
		}
		f.Close()
	}

	// Register bus in registry
	if err := m.registerBus(busID, sessionID, origin); err != nil {
		return "", fmt.Errorf("register bus: %w", err)
	}

	return busID, nil
}

// registerBus adds or updates a bus entry in the registry.
func (m *busSessionManager) registerBus(busID, sessionID, origin string) error {
	registry := m.loadRegistry()

	// Check if bus already exists
	for i, entry := range registry {
		if entry.BusID == busID {
			// Update existing entry
			registry[i].State = "active"
			return m.saveRegistry(registry)
		}
	}

	// Create new entry
	now := time.Now().UTC().Format(time.RFC3339)
	entry := busRegistryEntry{
		BusID:        busID,
		State:        "active",
		Participants: []string{fmt.Sprintf("%s:session:%s", origin, sessionID), "kernel:cogos"},
		Transport:    "file",
		Endpoint:     filepath.Join(".cog", ".state", "buses", busID),
		CreatedAt:    now,
		LastEventSeq: 0,
		LastEventAt:  now,
		EventCount:   0,
	}
	registry = append(registry, entry)
	return m.saveRegistry(registry)
}

// loadRegistry reads the bus registry from disk.
func (m *busSessionManager) loadRegistry() []busRegistryEntry {
	data, err := os.ReadFile(m.registryPath())
	if err != nil {
		return []busRegistryEntry{}
	}
	var entries []busRegistryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return []busRegistryEntry{}
	}
	return entries
}

// saveRegistry writes the bus registry to disk.
func (m *busSessionManager) saveRegistry(entries []busRegistryEntry) error {
	if err := os.MkdirAll(m.busesDir(), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.registryPath(), data, 0644)
}

// appendBusEvent appends a new CogBlock to a bus's event chain.
// V2 blocks hash the full canonical envelope (all fields except hash and sig).
// Both prev ([]string) and prev_hash (string) are written for V1 compat.
// Handlers are dispatched asynchronously to prevent re-entrant deadlocks
// and event loops (a handler that calls appendBusEvent would otherwise
// block or corrupt seq numbers).
func (m *busSessionManager) appendBusEvent(busID, eventType, from string, payload map[string]interface{}) (*CogBlock, error) {
	m.mu.Lock()

	// Read last event to get seq and prevHash
	lastSeq, lastHash := m.getLastEvent(busID)
	newSeq := lastSeq + 1

	// Build V2 block with both Prev (array) and PrevHash (string) for transition compat
	var prev []string
	if lastHash != "" {
		prev = []string{lastHash}
	}

	evt := CogBlock{
		V:        2,
		BusID:    busID,
		Seq:      newSeq,
		Ts:       time.Now().UTC().Format(time.RFC3339Nano),
		From:     from,
		Type:     eventType,
		Payload:  payload,
		Prev:     prev,
		PrevHash: lastHash, // V1 compat — written alongside Prev during transition
	}

	// V2: hash full canonical form (all fields except hash and sig)
	evt.Hash = computeBlockHash(&evt)

	// Append to events file
	line, err := json.Marshal(evt)
	if err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("marshal event: %w", err)
	}

	eventsFile := m.eventsPath(busID)
	f, err := os.OpenFile(eventsFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("open events file: %w", err)
	}

	if _, err := f.WriteString(string(line) + "\n"); err != nil {
		f.Close()
		m.mu.Unlock()
		return nil, fmt.Errorf("write event: %w", err)
	}
	f.Close()

	// Update registry
	m.updateRegistrySeq(busID, newSeq, evt.Ts)

	// Snapshot handlers while locked, then release BEFORE dispatching.
	// This prevents deadlocks when handlers call appendBusEvent (e.g. tool
	// router posting tool.result in response to tool.invoke).
	handlers := make([]busEventHandler, len(m.eventHandlers))
	copy(handlers, m.eventHandlers)
	m.mu.Unlock()

	// Dispatch handlers outside the lock — safe because each handler
	// that needs to write back to the bus will re-acquire the lock via
	// its own appendBusEvent call with a fresh seq number.
	for _, h := range handlers {
		h.handler(busID, &evt)
	}

	return &evt, nil
}

// getLastEvent reads the last event from a bus to get seq and hash for chaining.
func (m *busSessionManager) getLastEvent(busID string) (int, string) {
	eventsFile := m.eventsPath(busID)
	f, err := os.Open(eventsFile)
	if err != nil {
		return 0, ""
	}
	defer f.Close()

	var lastLine string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			lastLine = line
		}
	}

	if lastLine == "" {
		return 0, ""
	}

	var block CogBlock
	if err := json.Unmarshal([]byte(lastLine), &block); err != nil {
		return 0, ""
	}
	return block.Seq, block.Hash
}

// updateRegistrySeq updates the last event seq/timestamp in the registry.
func (m *busSessionManager) updateRegistrySeq(busID string, seq int, ts string) {
	registry := m.loadRegistry()
	for i, entry := range registry {
		if entry.BusID == busID {
			registry[i].LastEventSeq = seq
			registry[i].LastEventAt = ts
			registry[i].EventCount = seq
			break
		}
	}
	if err := m.saveRegistry(registry); err != nil {
		log.Printf("[bus] failed to update registry seq: %v", err)
	}
}

// readBusEvents reads all events from a bus.
func (m *busSessionManager) readBusEvents(busID string) ([]CogBlock, error) {
	eventsFile := m.eventsPath(busID)
	f, err := os.Open(eventsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open events file: %w", err)
	}
	defer f.Close()

	var events []CogBlock
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	seen := make(map[int]bool)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var block CogBlock
		if err := json.Unmarshal([]byte(line), &block); err != nil {
			continue
		}
		if seen[block.Seq] {
			continue
		}
		seen[block.Seq] = true
		events = append(events, block)
	}

	return events, nil
}

// busEventsToMessages converts bus events into ChatMessage format for context construction.
// It filters for chat.request and chat.response events and maps them to user/assistant roles.
func (m *busSessionManager) busEventsToMessages(busID string, maxHistory int) ([]ChatMessage, error) {
	events, err := m.readBusEvents(busID)
	if err != nil {
		return nil, err
	}

	// Filter to chat events only
	var chatEvents []CogBlock
	for _, evt := range events {
		switch evt.Type {
		case BlockChatRequest, BlockChatResponse:
			chatEvents = append(chatEvents, evt)
		}
	}

	// Apply max history limit (count chat.request events)
	if maxHistory > 0 {
		requestCount := 0
		for i := len(chatEvents) - 1; i >= 0; i-- {
			if chatEvents[i].Type == BlockChatRequest {
				requestCount++
			}
			if requestCount > maxHistory {
				chatEvents = chatEvents[i+1:]
				break
			}
		}
	}

	// Convert to ChatMessage format
	var messages []ChatMessage
	for _, evt := range chatEvents {
		content, _ := evt.Payload["content"].(string)
		if content == "" {
			continue
		}

		var role string
		switch evt.Type {
		case BlockChatRequest:
			role = "user"
		case BlockChatResponse:
			role = "assistant"
		default:
			continue
		}

		contentBytes, _ := json.Marshal(content)
		messages = append(messages, ChatMessage{
			Role:    role,
			Content: contentBytes,
		})
	}

	return messages, nil
}

// listChatBuses returns all chat bus entries from the registry.
func (m *busSessionManager) listChatBuses() []busRegistryEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	registry := m.loadRegistry()
	var chatBuses []busRegistryEntry
	for _, entry := range registry {
		if len(entry.BusID) > 9 && entry.BusID[:9] == "bus_chat_" {
			chatBuses = append(chatBuses, entry)
		}
	}
	return chatBuses
}

// resetBus archives the current event chain and starts fresh.
func (m *busSessionManager) resetBus(busID string) error {
	archiveName, err := m.archiveBus(busID)
	if err != nil {
		return err
	}

	// Write a chat.reset event as the genesis of the new chain.
	// appendBusEvent manages its own locking, so call outside any lock.
	_, err = m.appendBusEvent(busID, BlockChatReset, "kernel:cogos", map[string]interface{}{
		"reason":  "manual_reset",
		"archive": archiveName,
	})
	return err
}

// archiveBus does the locked portion of resetBus: archive + registry update.
func (m *busSessionManager) archiveBus(busID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	eventsFile := m.eventsPath(busID)

	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		return "", fmt.Errorf("bus %s not found", busID)
	}

	archiveName := fmt.Sprintf("events.jsonl.%d.bak", time.Now().Unix())
	archivePath := filepath.Join(m.busesDir(), busID, archiveName)
	if err := os.Rename(eventsFile, archivePath); err != nil {
		return "", fmt.Errorf("archive events: %w", err)
	}

	f, err := os.Create(eventsFile)
	if err != nil {
		return "", fmt.Errorf("create fresh events file: %w", err)
	}
	f.Close()

	registry := m.loadRegistry()
	for i, entry := range registry {
		if entry.BusID == busID {
			registry[i].LastEventSeq = 0
			registry[i].LastEventAt = time.Now().UTC().Format(time.RFC3339)
			registry[i].EventCount = 0
			break
		}
	}
	if err := m.saveRegistry(registry); err != nil {
		log.Printf("[bus] failed to update registry after reset: %v", err)
	}

	return archiveName, nil
}

// constellation_bus.go — Index bus event content into constellation FTS5.
//
// Registers as a bus event handler so that chat.request and chat.response
// content is immediately searchable via cogos_memory_search, making all
// interaction surfaces (Discord, Claude Code, HTTP, Telegram) queryable
// through a single search interface.

package main

import (
	"fmt"
	"log"

	"github.com/myrgic/cogos/sdk/constellation"
)

// constellationBusIndexer indexes chat bus events into constellation FTS5.
// Registered as a bus event handler via AddEventHandler.
type constellationBusIndexer struct {
	root string
}

// newConstellationBusHandler returns a bus event handler function that indexes
// chat.request and chat.response content into the constellation database.
func newConstellationBusHandler(root string) func(busID string, block *CogBlock) {
	indexer := &constellationBusIndexer{root: root}
	return indexer.handleEvent
}

func (idx *constellationBusIndexer) handleEvent(busID string, block *CogBlock) {
	// Only index chat messages with content
	if block.Type != BlockChatRequest && block.Type != BlockChatResponse {
		return
	}

	content, _ := block.Payload["content"].(string)
	if content == "" {
		return
	}

	c, err := getConstellationForWorkspace(idx.root)
	if err != nil {
		log.Printf("[constellation-bus] failed to get constellation: %v", err)
		return
	}

	evt := constellation.BusEvent{
		BusID:     busID,
		Seq:       block.Seq,
		Timestamp: block.Ts,
		From:      block.From,
		Type:      block.Type,
		Content:   content,
		Hash:      block.Hash,
	}

	// Extract optional metadata from payload
	if origin, ok := block.Payload["origin"].(string); ok {
		evt.Origin = origin
	}
	if agent, ok := block.Payload["agent"].(string); ok {
		evt.Agent = agent
	}
	if userID, ok := block.Payload["user_id"].(string); ok {
		evt.UserID = userID
	}
	if userName, ok := block.Payload["user_name"].(string); ok {
		evt.UserName = userName
	}

	if err := c.IndexBusEvent(evt); err != nil {
		log.Printf("[constellation-bus] failed to index event bus=%s seq=%d: %v", busID, block.Seq, err)
	}
}

// backfillBusEvents reads all existing bus events and indexes them into constellation.
// Used to populate the FTS index with historical chat content from all surfaces.
func backfillBusEvents(root string) error {
	c, err := getConstellationForWorkspace(root)
	if err != nil {
		return fmt.Errorf("failed to open constellation: %w", err)
	}

	mgr := newBusSessionManager(root)
	registry := mgr.loadRegistry()

	indexed := 0
	skipped := 0

	for _, entry := range registry {
		events, err := mgr.readBusEvents(entry.BusID)
		if err != nil {
			fmt.Fprintf(log.Writer(), "[constellation-bus] failed to read bus %s: %v\n", entry.BusID, err)
			continue
		}

		for _, block := range events {
			if block.Type != BlockChatRequest && block.Type != BlockChatResponse {
				skipped++
				continue
			}

			content, _ := block.Payload["content"].(string)
			if content == "" {
				skipped++
				continue
			}

			evt := constellation.BusEvent{
				BusID:     entry.BusID,
				Seq:       block.Seq,
				Timestamp: block.Ts,
				From:      block.From,
				Type:      block.Type,
				Content:   content,
				Hash:      block.Hash,
			}
			if origin, ok := block.Payload["origin"].(string); ok {
				evt.Origin = origin
			}
			if agent, ok := block.Payload["agent"].(string); ok {
				evt.Agent = agent
			}
			if userID, ok := block.Payload["user_id"].(string); ok {
				evt.UserID = userID
			}
			if userName, ok := block.Payload["user_name"].(string); ok {
				evt.UserName = userName
			}

			if err := c.IndexBusEvent(evt); err != nil {
				fmt.Fprintf(log.Writer(), "[constellation-bus] failed to index bus=%s seq=%d: %v\n", entry.BusID, block.Seq, err)
				continue
			}
			indexed++
		}
	}

	fmt.Printf("Backfill complete: %d bus events indexed (%d skipped, %d buses)\n", indexed, skipped, len(registry))
	return nil
}

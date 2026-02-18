package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// busChat provides bus event emission as a side-effect of HTTP inference.
// When a chat completion request arrives (from any source: OpenClaw gateway,
// curl, etc.), busChat writes chat.request and chat.response events to the
// appropriate bus, making conversations visible in CogField and queryable
// via the bus event chain.
type busChat struct {
	manager *busSessionManager
	config  *BusChatConfig
	root    string
}

func newBusChat(root string) *busChat {
	cfg := LoadBusChatConfig(root)
	return &busChat{
		manager: newBusSessionManager(root),
		config:  cfg,
		root:    root,
	}
}

// emitRequest writes a chat.request event to the session's bus.
// Returns the bus ID and event (for later pairing with emitResponse).
func (bc *busChat) emitRequest(sessionID, content, origin string) (string, *BusEventData, error) {
	if sessionID == "" {
		return "", nil, nil // no session = no bus tracking
	}

	busID := bc.ensureBus(sessionID, origin)

	evt, err := bc.manager.appendBusEvent(busID, "chat.request",
		fmt.Sprintf("%s:user", origin),
		map[string]interface{}{
			"content":  content,
			"origin":   origin,
			"platform": origin,
		})
	if err != nil {
		return busID, nil, fmt.Errorf("emit chat.request: %w", err)
	}

	log.Printf("[bus-chat] bus=%s seq=%d type=chat.request from=%s", busID, evt.Seq, origin)
	return busID, evt, nil
}

// emitResponse writes a chat.response event to the session's bus.
func (bc *busChat) emitResponse(busID string, requestSeq int, content, model string, durationMs int64, tokensUsed int) {
	if busID == "" {
		return
	}

	payload := map[string]interface{}{
		"content":     content,
		"model":       model,
		"duration_ms": durationMs,
		"request_seq": requestSeq,
	}
	if tokensUsed > 0 {
		payload["tokens_used"] = tokensUsed
	}

	evt, err := bc.manager.appendBusEvent(busID, "chat.response", "kernel:cogos", payload)
	if err != nil {
		log.Printf("[bus-chat] failed to emit chat.response on bus=%s: %v", busID, err)
		return
	}

	log.Printf("[bus-chat] bus=%s seq=%d type=chat.response duration=%dms", busID, evt.Seq, durationMs)
}

// emitError writes a chat.error event to the session's bus.
func (bc *busChat) emitError(busID string, requestSeq int, errMsg, errType string) {
	if busID == "" {
		return
	}

	_, err := bc.manager.appendBusEvent(busID, "chat.error", "kernel:cogos", map[string]interface{}{
		"error":       errMsg,
		"error_type":  errType,
		"request_seq": requestSeq,
	})
	if err != nil {
		log.Printf("[bus-chat] failed to emit chat.error on bus=%s: %v", busID, err)
	}
}

// buildContextFromBus reads the bus event chain and constructs a ContextState for TAA.
func (bc *busChat) buildContextFromBus(busID, currentPrompt string) *ContextState {
	if !bc.config.Features.TAAEnabled || !bc.config.Features.ContextFromBus {
		return nil
	}

	messages, err := bc.manager.busEventsToMessages(busID, bc.config.MaxHistory)
	if err != nil {
		log.Printf("[bus-chat] failed to read bus history: %v", err)
		return nil
	}

	// Add current message
	contentBytes, _ := json.Marshal(currentPrompt)
	messages = append(messages, ChatMessage{
		Role:    "user",
		Content: contentBytes,
	})

	profile := bc.config.TAAProfile
	if profile == "" || profile == "none" || profile == "passthrough" {
		return nil
	}

	contextState, err := ConstructContextStateWithProfile(messages, busID, bc.root, profile)
	if err != nil {
		log.Printf("[bus-chat] TAA context construction warning: %v", err)
		return nil
	}

	if contextState != nil {
		tierTokens := make([]string, 0, 4)
		if contextState.Tier1Identity != nil {
			tierTokens = append(tierTokens, fmt.Sprintf("1:%d", contextState.Tier1Identity.Tokens))
		}
		if contextState.Tier2Temporal != nil {
			tierTokens = append(tierTokens, fmt.Sprintf("2:%d", contextState.Tier2Temporal.Tokens))
		}
		if contextState.Tier3Present != nil {
			tierTokens = append(tierTokens, fmt.Sprintf("3:%d", contextState.Tier3Present.Tokens))
		}
		if contextState.Tier4Semantic != nil {
			tierTokens = append(tierTokens, fmt.Sprintf("4:%d", contextState.Tier4Semantic.Tokens))
		}
		log.Printf("[bus-chat] bus=%s context_tokens=%d tiers=[%s]",
			busID, contextState.TotalTokens, strings.Join(tierTokens, " "))
	}

	return contextState
}

// ensureBus returns (or creates) a bus for the given session ID.
func (bc *busChat) ensureBus(sessionID, origin string) string {
	busID := fmt.Sprintf("bus_chat_%s", sessionID)

	// Try to create — idempotent if bus already exists
	if _, err := bc.manager.createChatBus(sessionID, origin); err != nil {
		log.Printf("[bus-chat] warning: createChatBus: %v", err)
	}

	return busID
}

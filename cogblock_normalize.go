package main

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// NormalizeOpenAIRequest converts an OpenAI-compatible chat request into a CogBlock.
func NormalizeOpenAIRequest(req *oaiChatRequest, rawBody []byte, source string) *CogBlock {
	msgs := make([]ProviderMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = ProviderMessage{
			Role:       m.Role,
			Content:    extractContent(m.Content),
			ToolCallID: m.ToolCallID,
		}
		// Populate structured ContentParts to preserve image data that
		// extractContent() would drop (it only extracts text).
		parts := extractContentParts(m.Content)
		hasNonText := false
		for _, p := range parts {
			if p.Type != "text" {
				hasNonText = true
				break
			}
		}
		if hasNonText {
			msgs[i].ContentParts = make([]ContentPart, 0, len(parts))
			for _, p := range parts {
				cp := ContentPart{Type: p.Type, Text: p.Text}
				if p.ImageURL != nil {
					cp.ImageURL = p.ImageURL.URL
				}
				msgs[i].ContentParts = append(msgs[i].ContentParts, cp)
			}
		}
		// Preserve name field (used by some OpenAI clients for multi-participant chats).
		if m.Name != "" {
			msgs[i].Name = m.Name
		}
		// Forward tool_calls from assistant messages (conversation history of prior calls).
		if len(m.ToolCalls) > 0 {
			var calls []oaiToolCall
			if err := json.Unmarshal(m.ToolCalls, &calls); err == nil {
				msgs[i].ToolCalls = make([]ToolCall, len(calls))
				for j, tc := range calls {
					msgs[i].ToolCalls[j] = ToolCall{
						ID:        tc.ID,
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					}
				}
			}
		}
	}

	now := time.Now().UTC()
	return &CogBlock{
		ID:              uuid.New().String(),
		Timestamp:       now,
		SourceChannel:   source,
		SourceTransport: "openai-compat",
		Kind:            BlockMessage,
		RawPayload:      json.RawMessage(rawBody),
		Messages:        msgs,
		Provenance: BlockProvenance{
			OriginChannel: source,
			IngestedAt:    now,
			NormalizedBy:  "http-openai",
		},
		TrustContext: TrustContext{
			Authenticated: true,
			TrustScore:    1.0,
			Scope:         "local",
		},
	}
}

// NormalizeAnthropicRequest converts an Anthropic Messages API request into a CogBlock.
func NormalizeAnthropicRequest(body []byte, source string) *CogBlock {
	now := time.Now().UTC()
	var req anthropicMessagesRequest
	var msgs []ProviderMessage
	if err := json.Unmarshal(body, &req); err == nil {
		oaiReq := anthropicToOpenAIRequest(&req)
		msgs = make([]ProviderMessage, len(oaiReq.Messages))
		for i, m := range oaiReq.Messages {
			msgs[i] = ProviderMessage{
				Role:    m.Role,
				Content: extractContent(m.Content),
			}
		}
	}

	return &CogBlock{
		ID:              uuid.New().String(),
		Timestamp:       now,
		SourceChannel:   source,
		SourceTransport: "anthropic",
		Kind:            BlockMessage,
		RawPayload:      json.RawMessage(body),
		Messages:        msgs,
		Provenance: BlockProvenance{
			OriginChannel: source,
			IngestedAt:    now,
			NormalizedBy:  "http-anthropic",
		},
		TrustContext: TrustContext{
			Authenticated: true,
			TrustScore:    1.0,
			Scope:         "local",
		},
	}
}

// NormalizeMCPRequest converts an MCP tool invocation that triggers cognition into a CogBlock.
func NormalizeMCPRequest(toolName string, input json.RawMessage) *CogBlock {
	now := time.Now().UTC()
	return &CogBlock{
		ID:              uuid.New().String(),
		Timestamp:       now,
		SourceChannel:   "mcp",
		SourceTransport: "mcp",
		Kind:            BlockToolCall,
		RawPayload:      input,
		Provenance: BlockProvenance{
			OriginChannel: "mcp",
			IngestedAt:    now,
			NormalizedBy:  "mcp",
		},
		TrustContext: TrustContext{
			Authenticated: true,
			TrustScore:    1.0,
			Scope:         "workspace",
		},
		Artifacts: []BlockArtifact{{
			Kind: "tool_call",
			Ref:  toolName,
		}},
	}
}

// NormalizeGateEvent converts an internal GateEvent into a CogBlock.
func NormalizeGateEvent(evt *GateEvent) *CogBlock {
	now := evt.Timestamp.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	return &CogBlock{
		ID:              uuid.New().String(),
		Timestamp:       now,
		SessionID:       evt.SessionID,
		SourceChannel:   "internal",
		SourceTransport: "direct",
		Kind:            BlockSystemEvent,
		Messages: []ProviderMessage{{
			Role:    "system",
			Content: evt.Content,
		}},
		Provenance: BlockProvenance{
			OriginSession: evt.SessionID,
			OriginChannel: "internal",
			IngestedAt:    now,
			NormalizedBy:  "direct",
		},
		TrustContext: TrustContext{
			Authenticated: true,
			TrustScore:    1.0,
			Scope:         "workspace",
		},
	}
}

// debug.go — introspection endpoints for the foveated context engine
//
// Provides real-time visibility into engine state:
//
//	GET /v1/debug/last    — full pipeline snapshot from the most recent chat request
//	GET /v1/debug/context — current context window as zones with ordering and token counts
//
// No external dependencies. Just curl it.
package main

import (
	"sync"
	"time"
)

// DebugSnapshot captures the full pipeline state of a single chat request.
type DebugSnapshot struct {
	Timestamp time.Time         `json:"timestamp"`
	Client    DebugClientInfo   `json:"client"`
	Engine    DebugEngineInfo   `json:"engine"`
	Provider  DebugProviderInfo `json:"provider"`
	Context   DebugContextView  `json:"context"`
}

type DebugClientInfo struct {
	MessagesCount   int    `json:"messages_count"`
	HasSystemPrompt bool   `json:"has_system_prompt"`
	ModelRequested  string `json:"model_requested"`
	QueryExtracted  string `json:"query_extracted"`
}

type DebugEngineInfo struct {
	NucleusTokens         int      `json:"nucleus_tokens"`
	ClientSystemTokens    int      `json:"client_system_tokens"`
	CogDocsScored         int      `json:"cogdocs_scored"`
	CogDocsInjected       int      `json:"cogdocs_injected"`
	CogDocsInjectedPaths  []string `json:"cogdocs_injected_paths"`
	ConversationTurnsIn   int      `json:"conversation_turns_in"`
	ConversationTurnsKept int      `json:"conversation_turns_kept"`
	CurrentMessageTokens  int      `json:"current_message_tokens"`
	TotalTokens           int      `json:"total_tokens"`
	Budget                int      `json:"budget"`
	OutputReserve         int      `json:"output_reserve"`
	FlexBudgetUsed        int      `json:"flex_budget_used"`
}

type DebugProviderInfo struct {
	Selected       string `json:"selected"`
	Model          string `json:"model"`
	ResponseTokens int    `json:"response_tokens"`
	LatencyMs      int64  `json:"latency_ms"`
}

// DebugContextView shows the current context window as stability-ordered zones.
type DebugContextView struct {
	Zones  []DebugZone `json:"zones"`
	Budget DebugBudget `json:"budget"`
}

type DebugZone struct {
	Zone           string          `json:"zone"`
	Tokens         int             `json:"tokens"`
	ContentPreview string          `json:"content_preview,omitempty"`
	Items          []DebugZoneItem `json:"items,omitempty"`
}

type DebugZoneItem struct {
	ID        string  `json:"id,omitempty"`
	Title     string  `json:"title,omitempty"`
	Role      string  `json:"role,omitempty"`
	Tokens    int     `json:"tokens"`
	Salience  float64 `json:"salience,omitempty"`
	Recency   float64 `json:"recency,omitempty"`
	Relevance float64 `json:"relevance,omitempty"`
	Reason    string  `json:"reason,omitempty"`
	Preview   string  `json:"preview"`
}

type DebugBudget struct {
	Total         int `json:"total"`
	OutputReserve int `json:"output_reserve"`
	Used          int `json:"used"`
	Remaining     int `json:"remaining"`
}

// debugStore holds the most recent snapshot, protected by a mutex.
type debugStore struct {
	mu   sync.RWMutex
	last *DebugSnapshot
}

func (d *debugStore) Store(snap *DebugSnapshot) {
	d.mu.Lock()
	d.last = snap
	d.mu.Unlock()
}

func (d *debugStore) Load() *DebugSnapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.last
}

// captureDebugSnapshot builds a DebugSnapshot from a completed chat request.
func captureDebugSnapshot(
	clientMsgs []ProviderMessage,
	query string,
	modelRequested string,
	pkg *ContextPackage,
	conversationTurnsIn int,
	providerName string,
	model string,
	responseTokens int,
	latency time.Duration,
) *DebugSnapshot {
	snap := &DebugSnapshot{
		Timestamp: time.Now().UTC(),
	}

	// Client info.
	snap.Client = DebugClientInfo{
		MessagesCount:  len(clientMsgs),
		ModelRequested: modelRequested,
		QueryExtracted: truncate(query, 200),
	}
	for _, m := range clientMsgs {
		if m.Role == "system" {
			snap.Client.HasSystemPrompt = true
			break
		}
	}

	// Engine info.
	if pkg != nil {
		currentTokens := 0
		if pkg.CurrentMessage != nil {
			currentTokens = estTokens(pkg.CurrentMessage.Content)
		}
		snap.Engine = DebugEngineInfo{
			NucleusTokens:         estTokens(pkg.NucleusText),
			ClientSystemTokens:    estTokens(pkg.ClientSystem),
			CogDocsInjected:       len(pkg.FovealDocs),
			CogDocsInjectedPaths:  pkg.InjectedPaths,
			ConversationTurnsIn:   conversationTurnsIn,
			ConversationTurnsKept: len(pkg.Conversation),
			CurrentMessageTokens:  currentTokens,
			TotalTokens:           pkg.TotalTokens,
			OutputReserve:         pkg.OutputReserve,
		}

		// Build context zone view.
		snap.Context = buildContextView(pkg)
	}

	// Provider info.
	snap.Provider = DebugProviderInfo{
		Selected:       providerName,
		Model:          model,
		ResponseTokens: responseTokens,
		LatencyMs:      latency.Milliseconds(),
	}

	return snap
}

// buildContextView renders the ContextPackage as stability-ordered zones.
func buildContextView(pkg *ContextPackage) DebugContextView {
	var zones []DebugZone

	// Zone 0: Nucleus.
	if pkg.NucleusText != "" {
		zones = append(zones, DebugZone{
			Zone:           "nucleus",
			Tokens:         estTokens(pkg.NucleusText),
			ContentPreview: truncate(pkg.NucleusText, 200),
		})
	}

	// Zone 1a: Client system prompt.
	if pkg.ClientSystem != "" {
		zones = append(zones, DebugZone{
			Zone:           "client_system",
			Tokens:         estTokens(pkg.ClientSystem),
			ContentPreview: truncate(pkg.ClientSystem, 200),
		})
	}

	// Zone 1b: CogDocs.
	if len(pkg.FovealDocs) > 0 {
		docZone := DebugZone{
			Zone: "cogdocs",
		}
		for _, doc := range pkg.FovealDocs {
			preview := doc.Content
			if preview == "" {
				preview = doc.Summary
			}
			docZone.Items = append(docZone.Items, DebugZoneItem{
				ID:       doc.URI,
				Title:    doc.Title,
				Tokens:   doc.Tokens,
				Salience: doc.Salience,
				Reason:   doc.Reason,
				Preview:  truncate(preview, 100),
			})
			docZone.Tokens += doc.Tokens
		}
		zones = append(zones, docZone)
	}

	// Zone 2: Conversation history.
	if len(pkg.Conversation) > 0 {
		convZone := DebugZone{
			Zone: "conversation",
		}
		for _, m := range pkg.Conversation {
			convZone.Items = append(convZone.Items, DebugZoneItem{
				Role:      m.Role,
				Tokens:    m.Tokens,
				Recency:   m.RecencyScore,
				Relevance: m.RelevanceScore,
				Preview:   truncate(m.Content, 100),
			})
			convZone.Tokens += m.Tokens
		}
		zones = append(zones, convZone)
	}

	// Zone 3: Current message.
	if pkg.CurrentMessage != nil {
		zones = append(zones, DebugZone{
			Zone:           "current_message",
			Tokens:         estTokens(pkg.CurrentMessage.Content),
			ContentPreview: truncate(pkg.CurrentMessage.Content, 200),
		})
	}

	// Budget summary.
	used := 0
	for _, z := range zones {
		used += z.Tokens
	}

	budget := 32768 // default
	reserve := pkg.OutputReserve
	if reserve == 0 {
		reserve = 4096
	}

	return DebugContextView{
		Zones: zones,
		Budget: DebugBudget{
			Total:         budget,
			OutputReserve: reserve,
			Used:          used,
			Remaining:     budget - reserve - used,
		},
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

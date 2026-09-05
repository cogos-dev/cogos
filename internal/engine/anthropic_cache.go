package engine

// anthropic_cache.go — prompt-cache breakpoints for the Anthropic wire.
//
// Anthropic's prompt cache is opt-in per request: a block carrying
// `cache_control: {type: "ephemeral"}` marks a breakpoint, and every token
// before it is served as a cache read (~10% of input price) when the prefix
// matches a recent request. The kernel sent no breakpoints, so an agentic
// client replaying its full transcript every step paid cold price for the
// whole window each time (observed: 60K input/step, 0% cache hit, dsh
// session 3c189c37 — 2.85M input tokens across 63 steps).
//
// Placement rule (ADR-103 stability order, deferred TODO in
// context_assembly.go): mark the END of each stable region so the prefix up
// to it is reusable across steps.
//
//   1. last system block           — nucleus + client system, stable per session
//   2. last tool definition        — stable per session
//   3. the final content block of the SECOND-TO-LAST message — i.e. everything
//      the model has already seen this turn. The final message (the newest
//      tool_result or operator text) is the only cold region.
//
// Anthropic allows at most 4 breakpoints; we use 3. Breakpoints are applied
// AFTER normalizeAnthropicMessages, on the final block structure, so I2 merges
// and I4 reorders cannot move them.

// anthropicCacheControl is the wire shape for a cache breakpoint.
type anthropicCacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

var ephemeral = &anthropicCacheControl{Type: "ephemeral"}

// applyAnthropicCacheBreakpoints mutates payload in place. Idempotent.
func applyAnthropicCacheBreakpoints(payload *anthropicRequest) {
	if payload == nil {
		return
	}
	// 1. system: last block.
	if blocks, ok := payload.System.([]anthropicSystemBlock); ok && len(blocks) > 0 {
		blocks[len(blocks)-1].CacheControl = ephemeral
		payload.System = blocks
	}
	// 2. tools: last definition.
	if n := len(payload.Tools); n > 0 {
		payload.Tools[n-1].CacheControl = ephemeral
	}
	// 3. history: final block of the second-to-last message.
	if n := len(payload.Messages); n >= 2 {
		markLastBlock(&payload.Messages[n-2])
	}
}

// markLastBlock attaches cache_control to the final content block of m,
// converting string content to a single text block if needed.
func markLastBlock(m *anthropicMessage) {
	switch c := m.Content.(type) {
	case string:
		if c == "" {
			return
		}
		m.Content = []anthropicContentBlock{{Type: "text", Text: c, CacheControl: ephemeral}}
	case []anthropicContentBlock:
		if len(c) == 0 {
			return
		}
		c[len(c)-1].CacheControl = ephemeral
		m.Content = c
	}
}

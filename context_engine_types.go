// context_engine_types.go — Types for the CogOS Context Engine.
//
// The Context Engine sits between OpenClaw and Claude CLI, normalizing
// incoming threads, managing session lifecycle independently of OpenClaw's
// session model, and building compressed context windows for each turn.
//
// Architecture:
//
//	OpenClaw → POST /v1/chat/completions (full thread as []ChatMessage)
//	  → ThreadParser.Parse()     → ThreadView  (normalized, deduped)
//	  → SessionManager.Resolve() → SessionState (CogOS-managed lifecycle)
//	  → ContextCompressor.Compress() → ContextWindow (budget-constrained)
//	  → Claude CLI (--resume or fresh)

package main

import (
	"time"

	"github.com/myrgic/cogos/pkg/substrate/session"
)

// === THREAD PARSER TYPES ===

// ThreadView is the parsed, normalized representation of an OpenClaw thread.
// One ThreadView per incoming request — immutable input to the engine.
type ThreadView struct {
	ThreadID     string          // OpenClaw session/thread ID
	Messages     []ThreadMessage // Normalized, deduped, ordered
	SystemPrompt string          // Extracted system prompt (from role:system messages)
	LastUserMsg  string          // The actual new user message this turn
	TurnCount    int             // Total user turns in thread
	Origin       string          // "discord", "tui", "http"
}

// ThreadMessage is a single normalized message from the thread.
type ThreadMessage struct {
	ID        string         // OpenClaw message ID (for dedup)
	Role      string         // user, assistant, system, tool
	Content   string         // Cleaned message text (metadata stripped)
	RawContent string        // Original content before stripping
	Sender    string         // User display name or agent name
	SenderID  string         // User ID for identity resolution
	Timestamp time.Time      // When sent (parsed from metadata)
	Tokens    int            // Estimated token count (len/4)
	IsStarter bool           // True if this is a "[Thread starter]" echo
	Metadata  map[string]any // Preserved metadata (tool calls, etc.)
}

// === SESSION MANAGER TYPES ===
//
// Per ADR-100 Step 3d full untangle: the schema for session lifecycle moved
// to pkg/substrate/session. The aliases below let existing kernel call sites
// (session_manager.go, context_engine.go) compile unchanged. New code should
// prefer the substrate import path.

// SessionState tracks a CogOS-managed conversation across Claude CLI sessions.
// Canonical home: pkg/substrate/session.State.
type SessionState = session.State

// SessionRotation records a retired Claude CLI session.
// Canonical home: pkg/substrate/session.Rotation.
type SessionRotation = session.Rotation

// WorkingMemory persists across Claude session rotations. This is the
// continuity layer — what survives when starting a fresh Claude session.
// Canonical home: pkg/substrate/session.WorkingMemory.
type WorkingMemory = session.WorkingMemory

// SessionManagerConfig holds tunable thresholds for session rotation.
// Canonical home: pkg/substrate/session.ManagerConfig.
type SessionManagerConfig = session.ManagerConfig

// DefaultSessionManagerConfig returns sensible defaults.
// Delegates to pkg/substrate/session.DefaultManagerConfig.
func DefaultSessionManagerConfig() SessionManagerConfig {
	return session.DefaultManagerConfig()
}

// === CONTEXT COMPRESSOR TYPES ===

// ContextWindow is the final output — what we actually send to Claude CLI.
type ContextWindow struct {
	SystemPrompt  string // TAA identity + enrichment
	Prompt        string // Constructed prompt (compressed history + new message)
	TotalTokens   int    // Estimated total tokens
	Strategy      string // "resume", "fresh", "fallback"
	ClaudeSession string // Which Claude session to --resume (empty = fresh)
}

// CompressorBudget defines token budget allocation for context construction.
type CompressorBudget struct {
	Total           int // Total budget from TAA profile
	WorkingMemory   int // Budget for working memory (default: 2K)
	CompressedHistory int // Budget for compressed older exchanges (default: 3K)
	RecentContext   int // Budget for recent verbatim exchanges (default: 8K)
	CurrentMessage  int // Remainder for the actual new message
}

// DefaultCompressorBudget returns sensible defaults for a 15K total budget.
func DefaultCompressorBudget() CompressorBudget {
	return CompressorBudget{
		Total:             15_000,
		WorkingMemory:     2_000,
		CompressedHistory: 3_000,
		RecentContext:     8_000,
		CurrentMessage:    2_000,
	}
}

// === ENGINE TYPES ===

// RequestHeaders captures relevant HTTP headers from the incoming request.
type RequestHeaders struct {
	Origin       string // X-Origin header
	SessionReset bool   // X-Session-Reset: true
	UserID       string // X-OpenClaw-User-ID
	UserName     string // X-OpenClaw-User-Name
}

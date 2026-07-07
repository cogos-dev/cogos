// foveation_session_key.go — stable per-conversation key for foveation / light-cone state.
//
// Background (ADR-066/071 amendment, 2026-07-07 "foveation placement under
// prefix-cache runtimes"): the foveated-context assembler tracks a per-conversation
// LoRO light cone (ADR-071 Phase 2), keyed by a "conversation id" passed via
// WithConversationID. Both proxy call sites (serve.go handleChat, serve_anthropic.go
// handleAnthropicMessages) fed that option a FRESH per-request UUID
// (creq.Metadata.RequestID = uuid.New()). Consequences:
//
//   - the light cone was Get()'d under a key that had never been Set() → always nil
//     → the SSM state never persisted across turns (the feature was silently dead);
//   - LightConeManager accumulated one orphaned entry per request, never reused and
//     (pre-TTL) never pruned → unbounded memory growth for the process lifetime.
//
// The fix is to key on a STABLE per-conversation identity instead. This file
// derives that key with a safety-first precedence, and — critically — NEVER uses
// Process.SessionID() as the key: that value is process-wide (one per kernel), so
// keying on it would collapse every concurrent conversation/user onto a single light
// cone (cross-conversation, potentially cross-user state bleed).
//
// Precedence:
//  1. Explicit inbound session id — the X-Cogos-Session-Id request header. This is
//     the canonical client-supplied session identity already used for harness
//     binding (resolveBoundIdentity); when present it is the correct join key.
//  2. Client "user" field — OpenAI `user` / Anthropic `metadata.user_id`. A
//     client-supplied end-user identifier. Weaker than a session id (one user may
//     run several conversations) but still per-client and safe against cross-user
//     bleed.
//  3. Fallback: a stable hash of the conversation's LEADING turns (client system
//     prompt + first user message). Stable within a single conversation (the lead
//     turns don't change as it grows) and distinct across conversations. This is
//     the safe default when a raw OpenAI-compat client (e.g. Hermes) supplies no
//     session id and no user field: it never bleeds state across different
//     conversations, and different users almost never share identical opening turns.
//
// The returned key is opaque and only ever used as a map key in LightConeManager;
// it is not logged raw or surfaced to the model.
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// foveationKeyHeader is the request header carrying an explicit client session id.
// Kept in sync with resolveBoundIdentity, which reads the same header.
const foveationKeyHeader = "X-Cogos-Session-Id"

// foveationSessionKey derives a stable per-conversation key for light-cone /
// foveation state.
//
//   - sessionID: value of the X-Cogos-Session-Id header ("" if absent).
//   - userField: the client "user" / "user_id" field ("" if absent).
//   - messages:  the client-supplied messages for this turn, used only for the
//     leading-turns fallback hash.
//
// It never returns "": the leading-turns hash always yields a value (even for an
// empty conversation, it hashes the empty lead deterministically), so the caller
// always has a non-nil, non-per-request key.
func foveationSessionKey(sessionID, userField string, messages []ProviderMessage) string {
	if s := strings.TrimSpace(sessionID); s != "" {
		return "sid:" + s
	}
	if u := strings.TrimSpace(userField); u != "" {
		return "usr:" + u
	}
	return "lead:" + leadingTurnsHash(messages)
}

// leadingTurnsHash returns a hex SHA-256 over the conversation's leading turns:
// the client system prompt (if any) plus the FIRST user message. These are stable
// across a conversation's lifetime (they do not change as later turns are added),
// so two consecutive turns of the same conversation hash identically, while two
// different conversations (different opening) hash differently.
func leadingTurnsHash(messages []ProviderMessage) string {
	var sys, firstUser string
	for _, m := range messages {
		if m.Role == "system" && sys == "" {
			sys = m.Content
			continue
		}
		if m.Role == "user" {
			firstUser = m.Content
			break
		}
	}
	// Domain-separate the two components so that ("ab","") and ("a","b") cannot
	// collide.
	h := sha256.New()
	h.Write([]byte("sys\x00"))
	h.Write([]byte(sys))
	h.Write([]byte("\x00usr\x00"))
	h.Write([]byte(firstUser))
	return hex.EncodeToString(h.Sum(nil))
}

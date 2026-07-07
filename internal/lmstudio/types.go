// Package lmstudio implements an observer that projects LM Studio local-model
// conversation history into the CogOS Conversations Observatory's normalized
// ingest surface (cogos.observatory.conversations/v0.1), so LM Studio sessions
// become searchable via cog_search_conversations / cog_list_conversations
// alongside Claude Code, Hermes, and other sources.
//
// This package is a producer, not a consumer: it does not read or write the
// observatory index directly. It watches LM Studio's conversation JSON files
// and emits normalized JSONL records into
// <workspace>/.cog/observatory/ingest/lm-studio/*.jsonl, where the existing
// internal/conversations Reconcilable (unmodified) picks them up on its next
// reconcile cycle via the generic ingest_dirs mechanism.
//
// Package layout:
//   - types.go   — LM Studio conversation JSON schema (read-side) + emitted
//                  ingest record shape (write-side)
//   - parser.go  — decodes one *.conversation.json file into ingest records
//   - watch.go   — discovers conversation files under the LM Studio dir and
//                  tracks which ones have already been emitted (by mtime+size)
//   - emit.go    — serializes ingest records as JSONL and writes them to the
//                  ingest directory
package lmstudio

import "encoding/json"

// Source is the observer source id used in emitted ingest records and as the
// ingest subdirectory name: .cog/observatory/ingest/lm-studio/.
const Source = "lm-studio"

// IngestSchema is the normalized ingest schema this observer speaks.
const IngestSchema = "cogos.observatory.conversations/v0.1"

// ─── LM Studio on-disk conversation format ──────────────────────────────────
//
// ~/.lmstudio/conversations/<folder>/<epoch-ms>.conversation.json
//
// Verified against a real LM Studio conversation file (10 messages, mixing
// singleStep and multiStep assistant turns) on 2026-07-07. Fields not needed
// for ingest (preset, tokenCount, plugins, clientInputFiles, etc.) are
// omitted from these structs; json.Unmarshal ignores unknown fields.

// Conversation is the top-level shape of one LM Studio *.conversation.json file.
type Conversation struct {
	Name           string          `json:"name"`
	CreatedAt      int64           `json:"createdAt"` // epoch milliseconds
	SystemPrompt   string          `json:"systemPrompt"`
	Messages       []LSMessage     `json:"messages"`
	LastUsedModel  *LSModelRef     `json:"lastUsedModel,omitempty"`
}

// LSModelRef carries the model identifier used for (at least) the most
// recent turn in the conversation. LM Studio does not record a per-message
// model id, so this is attached at the session level.
type LSModelRef struct {
	Identifier            string `json:"identifier"`
	IndexedModelIdentifier string `json:"indexedModelIdentifier,omitempty"`
}

// LSMessage is one entry in Conversation.Messages. LM Studio supports
// message editing / regeneration, so each message carries a Versions slice
// and a CurrentlySelected index; only the selected version is live.
type LSMessage struct {
	Versions          []LSVersion `json:"versions"`
	CurrentlySelected int         `json:"currentlySelected"`
}

// Selected returns the currently-selected version, or nil if the index is
// out of range (defensive — malformed/truncated files should not panic).
func (m LSMessage) Selected() *LSVersion {
	if m.CurrentlySelected < 0 || m.CurrentlySelected >= len(m.Versions) {
		return nil
	}
	return &m.Versions[m.CurrentlySelected]
}

// LSVersion is one version of a message. Type is either "singleStep" (plain
// user/assistant turn: role + content[] text blocks) or "multiStep"
// (assistant turn broken into steps — tool calls interleaved with text).
type LSVersion struct {
	Type  string `json:"type"`
	Role  string `json:"role"`

	// singleStep fields.
	Content []LSContentBlock `json:"content,omitempty"`

	// multiStep fields.
	Steps []LSStep `json:"steps,omitempty"`

	SenderInfo *LSSenderInfo `json:"senderInfo,omitempty"`
}

// LSSenderInfo carries the model identifier for a multiStep assistant turn.
type LSSenderInfo struct {
	SenderName string `json:"senderName,omitempty"`
}

// LSStep is one step of a multiStep version. Only "contentBlock" steps carry
// text/tool content; "status", "debugInfoBlock", and "toolStatus" steps are
// recognized but skipped (no operator-visible text).
type LSStep struct {
	Type    string           `json:"type"`
	Content []LSContentBlock `json:"content,omitempty"`
}

// LSContentBlock is one item within a singleStep's content[] or a multiStep
// contentBlock step's content[]. Type "text" carries Text; "toolCallRequest"
// and "toolCallResult" are recorded minimally (name only) — full tool
// input/output is not operator utterance and is out of scope for v0.1.
type LSContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// toolCallRequest fields.
	Name       string          `json:"name,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`

	// toolCallResult fields (structure varies; not decoded beyond presence).
}

// ─── Emitted ingest record (cogos.observatory.conversations/v0.1) ──────────

// IngestRefs is the optional refs object carried on emitted records. StableID
// gives the observatory a dedup key that survives re-emission across runs.
type IngestRefs struct {
	StableID string `json:"stable_id,omitempty"`
}

// IngestRecord mirrors the wire shape consumed by
// internal/conversations.ingestAccumulator.ConsumeFile (see
// myrgic/cogos internal/conversations/ingest_parser.go, ingestRecord).
// Field order/names must match that consumer exactly.
type IngestRecord struct {
	Schema       string      `json:"schema"`
	Source       string      `json:"source"`
	SessionID    string      `json:"session_id"`
	SessionTitle string      `json:"session_title,omitempty"`
	TurnIndex    int         `json:"turn_index"`
	Role         string      `json:"role"`
	Timestamp    string      `json:"timestamp"`
	Text         string      `json:"text"`
	Identity     string      `json:"identity,omitempty"`
	Refs         *IngestRefs `json:"refs,omitempty"`
}

// Package conversations implements the Conversations Observatory — a
// Reconcilable provider that makes Claude Code session history first-class
// substrate state.
//
// The observatory projects operator session JSONLs from
// ~/.claude/projects/-Users-slowbro/*.jsonl (and any additional configured
// directories) into a queryable index. After projection, the substrate can
// answer queries like "what did the operator say about harness attestation in
// May 2026?" via the cog_search_conversations MCP tool instead of walking
// raw JSONL.
//
// Package layout:
//   - types.go         — shared model types (SessionMeta, Turn, IndexEntry, etc.)
//   - parser.go        — streaming JSONL parser (bufio.Scanner; never os.ReadFile)
//   - ingest_parser.go — normalized ingest surface parser (cogos.observatory.conversations/v0.1)
//   - index.go         — in-memory full-text index backed by flat projection files
//   - provider.go      — Reconcilable implementation
//   - mcp_tools.go     — cog_search_conversations, cog_get_conversation_turn,
//                        cog_list_conversations tool registrations
package conversations

import "time"

// Role identifies who produced a conversation turn.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// SessionMeta carries per-session metadata without loading turn content.
type SessionMeta struct {
	// SessionID is the UUID from the JSONL file name (CC path) or the
	// observer-declared session_id (normalized ingest path).
	SessionID string `json:"session_id"`

	// Source identifies the observer that produced this session. Empty for
	// sessions ingested via the CC source_dirs path; set to the <source>
	// component of the ingest path for normalized ingest records.
	//
	// The index keys normalized-ingest sessions as "<source>/<session_id>" to
	// prevent collision with CC UUID session keys.
	Source string `json:"source,omitempty"`

	// SourcePath is the absolute path to the source JSONL.
	SourcePath string `json:"source_path"`

	// TurnCount is the number of indexed turns in this session.
	TurnCount int `json:"turn_count"`

	// FirstTurnAt / LastTurnAt bound the session in wall time.
	FirstTurnAt time.Time `json:"first_turn_at,omitempty"`
	LastTurnAt  time.Time `json:"last_turn_at,omitempty"`

	// IndexedAt records when this session was last projected.
	IndexedAt time.Time `json:"indexed_at"`

	// SourceMtime records the source file mtime at index time (for drift detection).
	SourceMtime time.Time `json:"source_mtime"`

	// SourceSize records the source file size at index time (drift detection).
	SourceSize int64 `json:"source_size"`

	// Identity is the operator identity extracted from the JSONL (e.g. "slowbro").
	// Populated from userType/sessionId/cwd fields present in the records.
	Identity string `json:"identity,omitempty"`

	// Entrypoint is the Claude Code client entrypoint (cli, claude-desktop, etc.)
	Entrypoint string `json:"entrypoint,omitempty"`

	// Title is the session AI-generated title when present.
	Title string `json:"title,omitempty"`
}

// Turn is one indexed turn from a session.
type Turn struct {
	// UUID is the record's uuid field.
	UUID string `json:"uuid"`

	// SessionID is the session this turn belongs to.
	SessionID string `json:"session_id"`

	// TurnIndex is the sequential turn number within the session (0-based).
	TurnIndex int `json:"turn_index"`

	// Role is user/assistant/system.
	Role Role `json:"role"`

	// Timestamp is the record's timestamp field (RFC3339).
	Timestamp time.Time `json:"timestamp"`

	// Text is the extracted plain-text content of the turn.
	// For user turns: concatenated text parts, system-reminder tags stripped.
	// For assistant turns: concatenated text + thinking parts.
	// Tool call inputs and tool results are excluded.
	Text string `json:"text"`

	// IsToolCall is true when this turn is primarily a tool invocation (assistant).
	IsToolCall bool `json:"is_tool_call,omitempty"`

	// ParentUUID links this turn to its parent in the conversation tree.
	ParentUUID string `json:"parent_uuid,omitempty"`

	// Component is the L1 component class for this record (e.g. "session.turn").
	// Set on newly-ingested L3 records; empty for records ingested before v0.2.
	Component string `json:"component,omitempty"`

	// OntologyVersion is the L1 ontology reference used when this record was
	// indexed (e.g. "cogos.conversations@1.0.0"). Empty for pre-v0.2 records.
	OntologyVersion string `json:"ontology_version,omitempty"`

	// MappingVersion is the L2 mapping reference used when this record was
	// indexed (e.g. "claude-code-jsonl@1.0.0"). Empty for pre-v0.2 records.
	MappingVersion string `json:"mapping_version,omitempty"`
}

// SearchHit is one result returned by cog_search_conversations.
type SearchHit struct {
	SessionID    string    `json:"session_id"`
	TurnIndex    int       `json:"turn_index"`
	UUID         string    `json:"uuid"`
	Timestamp    time.Time `json:"timestamp"`
	Role         Role      `json:"role"`
	Excerpt      string    `json:"excerpt"`              // ~300-char snippet containing the match
	Context      string    `json:"context,omitempty"`   // preceding/following text
	SessionTitle string    `json:"session_title,omitempty"`
	Identity     string    `json:"identity,omitempty"`
	Source       string    `json:"source,omitempty"` // observer source id, empty for CC sessions
}

// IndexDepth describes how thoroughly a session is indexed.
type IndexDepth string

const (
	DepthFull          IndexDepth = "full"           // all turns parsed and indexed
	DepthMetaOnly      IndexDepth = "meta_only"      // meta parsed, turns not
	DepthNotProjected  IndexDepth = "not_projected"  // file seen, not yet indexed
)

// IndexEntry describes one session's projection status.
type IndexEntry struct {
	Meta  SessionMeta `json:"meta"`
	Depth IndexDepth  `json:"depth"`
}

// ObservatoryConfig is the deserialized form of .cog/config/observatory.yaml.
// Uses yaml struct tags so gopkg.in/yaml.v3 can unmarshal it correctly.
type ObservatoryConfig struct {
	// SourceDirs is the list of JSONL source directories to scan for
	// Claude Code session files (UUID-named .jsonl).
	// Defaults to ["~/.claude/projects/-Users-slowbro"].
	SourceDirs []string `yaml:"source_dirs" json:"source_dirs,omitempty"`

	// IngestDirs is the list of normalized ingest root directories. Each
	// directory is expected to contain <source>/*.jsonl subdirectories whose
	// records conform to cogos.observatory.conversations/v0.1.
	// Defaults to ["<workspace>/.cog/observatory/ingest"].
	IngestDirs []string `yaml:"ingest_dirs" json:"ingest_dirs,omitempty"`

	// IncludePatterns are glob patterns relative to each SourceDir.
	// Defaults to ["*.jsonl"].
	IncludePatterns []string `yaml:"include_patterns" json:"include_patterns,omitempty"`

	// ExcludePatterns are glob patterns to skip.
	ExcludePatterns []string `yaml:"exclude_patterns" json:"exclude_patterns,omitempty"`

	// MaxTurnLength is the maximum number of characters stored per turn.
	// Default 8192. Longer turns are truncated with a suffix marker.
	MaxTurnLength int `yaml:"max_turn_length" json:"max_turn_length,omitempty"`

	// MaxSessionsToIndex caps how many sessions are indexed in a single
	// ApplyPlan pass. 0 = no limit.
	MaxSessionsToIndex int `yaml:"max_sessions_to_index" json:"max_sessions_to_index,omitempty"`

	// Identity tags to apply to all sessions from this config.
	DefaultIdentity string `yaml:"default_identity" json:"default_identity,omitempty"`

	// OntologyDir is the directory containing L1 + L2 ontology YAML files.
	// Defaults to <workspace>/.cog/observatory/ontology when present.
	// Set to an empty string to disable ontology enforcement entirely.
	OntologyDir string `yaml:"ontology_dir" json:"ontology_dir,omitempty"`
}

// providerConfig is the internal config bundle built by LoadConfig.
type providerConfig struct {
	Root          string
	Observatory   ObservatoryConfig
	SourceFiles   []sourceFileInfo   // CC UUID JSONLs expanded from SourceDirs
	IngestSources []ingestSourceInfo // normalized ingest sources expanded from IngestDirs
	Ontology      *LoadedOntology    // nil when ontology enforcement is disabled
}

// sourceFileInfo is metadata about one discovered Claude Code source JSONL.
type sourceFileInfo struct {
	Path      string
	SessionID string // derived from filename (UUID before .jsonl)
	Mtime     time.Time
	Size      int64
}

// ingestSourceInfo is the aggregate of one normalized ingest <source>
// directory. An ingest FILE is a transport artifact (one per observer run);
// records from many sessions are interleaved within a file and one session
// may span several files. Planning therefore happens at SOURCE granularity:
// the aggregate (TotalSize, LatestMtime) over all files is the drift signal,
// and ApplyPlan re-parses every file of the source to rebuild its sessions.
type ingestSourceInfo struct {
	// Source is the observer-declared source id (directory name, e.g.
	// "hermes-darkstar").
	Source string

	// Dir is the absolute path of the source directory.
	Dir string

	// Files are the absolute paths of the source's JSONL files, sorted by
	// name (observer run files are timestamp-named → chronological order).
	Files []string

	// TotalSize is the sum of file sizes (drift detection).
	TotalSize int64

	// LatestMtime is the most recent file mtime (drift detection).
	LatestMtime time.Time
}

// liveState is the result of FetchLive.
type liveState struct {
	// Entries maps session_id → IndexEntry.
	Entries map[string]IndexEntry
}

// applyStats summarizes what ApplyPlan did.
type applyStats struct {
	Indexed int
	Updated int
	Pruned  int
	Skipped int
	Errors  []string
}

//go:build mcpserver

// mcp_server.go — MCP Streamable HTTP server for CogOS v3
//
// Embeds the MCP server into the existing HTTP server at /mcp.
// Implements the 11 stage-1 tools from MCP-SPEC.md using the
// official Go MCP SDK (github.com/modelcontextprotocol/go-sdk).
//
// Transport: Streamable HTTP (MCP spec 2025-03-26)
// Endpoint: POST/GET /mcp
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

// MCPServer wraps the MCP server and its dependencies.
type MCPServer struct {
	server  *mcp.Server
	handler http.Handler
	cfg     *Config
	nucleus *Nucleus
	process *Process
}

// NewMCPServer creates and configures the MCP server with all stage-1 tools.
func NewMCPServer(cfg *Config, nucleus *Nucleus, process *Process) *MCPServer {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "cogos-v3",
		Version: BuildTime,
	}, nil)

	m := &MCPServer{
		server:  server,
		cfg:     cfg,
		nucleus: nucleus,
		process: process,
	}

	m.registerTools()

	m.handler = mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return server },
		nil,
	)

	return m
}

// Handler returns the http.Handler for mounting at /mcp.
func (m *MCPServer) Handler() http.Handler {
	return m.handler
}

// registerTools registers MCP tools.
// Design: tools are actions with side effects or non-trivial computation.
// Read-only state queries will migrate to MCP Resources in Phase 2.
func (m *MCPServer) registerTools() {
	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_search_memory",
		Description: "Full-text and semantic search over the CogDoc memory corpus. Returns ranked results with salience scores. Fallback: ./scripts/cog memory search \"query\"",
	}, m.toolSearchMemory)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_read_cogdoc",
		Description: "Read a CogDoc by URI or path. Resolves cog: URIs automatically. Returns full content with parsed frontmatter and optional section extraction via #fragment. Fallback: ./scripts/cog memory read <path>",
	}, m.toolReadCogdoc)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_write_cogdoc",
		Description: "Write or update a CogDoc at the specified memory path. Creates the file with proper frontmatter if it doesn't exist. Fallback: ./scripts/cog memory write <path> \"Title\"",
	}, m.toolWriteCogdoc)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_patch_frontmatter",
		Description: "Merge description, tags, or type patches into a CogDoc frontmatter block.",
	}, m.toolPatchFrontmatter)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_check_coherence",
		Description: "Run coherence validation against the workspace. Checks URI resolution, frontmatter validity, and reference integrity. Fallback: ./scripts/cog coherence check",
	}, m.toolCheckCoherence)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_get_state",
		Description: "Get kernel state: process status, uptime, trust, node health (sibling services), field size, and heartbeat info. Includes identity and coherence metadata. Fallback: curl http://localhost:6931/health",
	}, m.toolGetState)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_query_field",
		Description: "Query the attentional field — the salience-scored map of all tracked CogDocs. Returns top-N items, optionally filtered by sector. Shows what the kernel considers most relevant right now.",
	}, m.toolQueryField)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_assemble_context",
		Description: "Build a context package for a given token budget with an explicit focus topic. Use this for intentional context assembly (subtasks, specific investigations). The automatic foveated-context hook handles ambient context on every prompt.",
	}, m.toolAssembleContext)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_emit_event",
		Description: "Emit a typed event to the workspace ledger. Events: attention.boost (uri + weight), session.marker (label), insight.captured (summary), decision.made (decision + rationale). Fallback: events are JSONL in .cog/ledger/",
	}, m.toolEmitEvent)

	mcp.AddTool(m.server, &mcp.Tool{
		Name:        "cog_ingest",
		Description: "Ingest external material into CogOS knowledge. Deterministic decomposition — no LLM calls. Supports URLs, conversations, documents. Applies membrane policy (accept/quarantine/defer/discard).",
	}, m.toolIngest)
}

// ── Tool Inputs ──────────────────────────────────────────────────────────────

type resolveURIInput struct {
	URI string `json:"uri" jsonschema:"A cog: URI to resolve. Examples: cog:mem/semantic/architecture/x or cog://cog-workspace/adr/059"`
}

type queryFieldInput struct {
	Sector string `json:"sector,omitempty" jsonschema:"Filter by memory sector (semantic/episodic/procedural/reflective). Empty for all."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of results (default 20)"`
}

type assembleContextInput struct {
	Budget int    `json:"budget" jsonschema:"Token budget for the assembled context"`
	Focus  string `json:"focus,omitempty" jsonschema:"Optional focus topic to bias context selection"`
}

type checkCoherenceInput struct {
	Scope string `json:"scope,omitempty" jsonschema:"Scope of coherence check: structural (default)/navigational/canonical"`
}

type getStateInput struct {
	Verbose bool `json:"verbose,omitempty" jsonschema:"Include detailed field and process info"`
}

type getTrustInput struct{}

type searchMemoryInput struct {
	Query  string `json:"query" jsonschema:"Search query string"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum results (default 10)"`
	Sector string `json:"sector,omitempty" jsonschema:"Filter by memory sector"`
}

type getNucleusInput struct {
	IncludeConfig bool `json:"include_config,omitempty" jsonschema:"Include workspace configuration details"`
}

type readCogdocInput struct {
	URI     string `json:"uri" jsonschema:"A cog: URI pointing to the CogDoc"`
	Section string `json:"section,omitempty" jsonschema:"Optional section name to extract (from #fragment)"`
}

type cogdocFrontmatterPatch struct {
	Description string   `json:"description,omitempty" jsonschema:"One-line summary for the CogDoc" yaml:"description,omitempty"`
	Tags        []string `json:"tags,omitempty" jsonschema:"Classification tags" yaml:"tags,omitempty"`
	Type        string   `json:"type,omitempty" jsonschema:"CogDoc type" yaml:"type,omitempty"`
}

type patchFrontmatterInput struct {
	URI     string                 `json:"uri" jsonschema:"A cog: URI pointing to the CogDoc"`
	Patches cogdocFrontmatterPatch `json:"patches" jsonschema:"Frontmatter fields to merge into the CogDoc"`
}

type writeCogdocInput struct {
	Path    string   `json:"path" jsonschema:"Memory-relative path (e.g. semantic/insights/topic.md)"`
	Title   string   `json:"title" jsonschema:"Document title for frontmatter"`
	Content string   `json:"content" jsonschema:"Markdown content to write"`
	Tags    []string `json:"tags,omitempty" jsonschema:"Optional tags for classification"`
	Status  string   `json:"status,omitempty" jsonschema:"Document status (active/raw/enriched/integrated)"`
	DocType string   `json:"type,omitempty" jsonschema:"Document type (insight/link/conversation/architecture/guide)"`
}

type readCogdocResult struct {
	URI              string            `json:"uri"`
	Path             string            `json:"path"`
	Fragment         string            `json:"fragment,omitempty"`
	Frontmatter      cogdocFrontmatter `json:"frontmatter,omitempty"`
	Content          string            `json:"content"`
	SchemaIssues     []string          `json:"schema_issues,omitempty"`
	PatchFrontmatter map[string]any    `json:"patch_frontmatter,omitempty"`
	SchemaHint       string            `json:"schema_hint,omitempty"`
}

type emitEventInput struct {
	Type    string         `json:"type" jsonschema:"Event type: attention.boost, session.marker, insight.captured, decision.made"`
	Payload map[string]any `json:"payload,omitempty" jsonschema:"Event payload. attention.boost: {uri, weight}. session.marker: {label}. insight.captured: {summary, tags}. decision.made: {decision, rationale}."`
}

type getIndexInput struct {
	Sector string `json:"sector,omitempty" jsonschema:"Filter by memory sector"`
}

type ingestInput struct {
	Source   string            `json:"source" jsonschema:"Data source: discord, chatgpt, claude, gemini, url, file"`
	Format   string            `json:"format" jsonschema:"Input format: url, conversation, message, document"`
	Data     string            `json:"data" jsonschema:"Raw material to ingest (URL, text, JSON)"`
	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"Optional context (discord_message_id, channel, etc.)"`
}

// ── Tool Implementations ─────────────────────────────────────────────────────

func (m *MCPServer) toolResolveURI(ctx context.Context, req *mcp.CallToolRequest, input resolveURIInput) (*mcp.CallToolResult, any, error) {
	// Try v2 registry first (multi-scheme)
	if URIRegistry != nil {
		content, err := URIRegistry.Resolve(ctx, input.URI)
		if err == nil {
			result := map[string]any{
				"uri":      input.URI,
				"resolved": true,
				"metadata": content.Metadata,
			}
			if path, ok := content.Metadata["path"]; ok {
				result["path"] = path
				if _, statErr := os.Stat(path.(string)); statErr == nil {
					result["exists"] = true
				} else {
					result["exists"] = false
				}
			}
			return marshalResult(result)
		}
	}

	// Fallback to legacy resolver
	res, err := ResolveURI(m.cfg.WorkspaceRoot, input.URI)
	if err != nil {
		return marshalResult(map[string]any{
			"uri":      input.URI,
			"resolved": false,
			"error":    err.Error(),
		})
	}
	_, statErr := os.Stat(res.Path)
	return marshalResult(map[string]any{
		"uri":      input.URI,
		"resolved": true,
		"path":     res.Path,
		"fragment": res.Fragment,
		"exists":   statErr == nil,
	})
}

func (m *MCPServer) toolQueryField(ctx context.Context, req *mcp.CallToolRequest, input queryFieldInput) (*mcp.CallToolResult, any, error) {
	if m.process == nil || m.process.field == nil {
		return textResult("attentional field not initialized")
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 20
	}

	scores := m.process.field.AllScores()

	type entry struct {
		URI      string  `json:"uri"`
		Salience float64 `json:"salience"`
	}
	var entries []entry
	for absPath, score := range scores {
		if input.Sector != "" && !strings.Contains(absPath, input.Sector) {
			continue
		}
		// Project field key (abs path) to canonical URI.
		uri := FieldKeyToURI(m.cfg.WorkspaceRoot, absPath)
		entries = append(entries, entry{URI: uri, Salience: score})
	}
	// Sort by salience descending
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].Salience > entries[i].Salience {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return marshalResult(map[string]any{
		"count":   len(entries),
		"entries": entries,
	})
}

func (m *MCPServer) toolAssembleContext(ctx context.Context, req *mcp.CallToolRequest, input assembleContextInput) (*mcp.CallToolResult, any, error) {
	if m.process == nil {
		return textResult("process not initialized")
	}

	budget := input.Budget
	if budget <= 0 {
		budget = 50000
	}

	// Use the existing context assembly pipeline
	assembled, err := m.process.AssembleContext(input.Focus, nil, budget, WithManifestMode(true))
	if err != nil {
		return textResult(fmt.Sprintf("context assembly failed: %v", err))
	}

	return marshalResult(assembled)
}

func (m *MCPServer) toolCheckCoherence(ctx context.Context, req *mcp.CallToolRequest, input checkCoherenceInput) (*mcp.CallToolResult, any, error) {
	report, err := CheckCoherenceMCP(m.cfg, m.nucleus)
	if err != nil {
		return fallbackResult(fmt.Sprintf("coherence check failed: %v", err),
			"./scripts/cog coherence check")
	}
	return marshalResult(report)
}

func (m *MCPServer) toolGetState(ctx context.Context, req *mcp.CallToolRequest, input getStateInput) (*mcp.CallToolResult, any, error) {
	if m.process == nil {
		return fallbackResult("process not initialized", "curl http://localhost:6931/health")
	}
	queue := ReadIngestionQueueState(m.cfg.WorkspaceRoot)
	trust := m.process.TrustSnapshot()
	lastHeartbeat := ""
	if !trust.LastHeartbeatAt.IsZero() {
		lastHeartbeat = trust.LastHeartbeatAt.Format(time.RFC3339)
	}

	// Identity (nucleus)
	identity := ""
	if m.nucleus != nil {
		identity = m.nucleus.Name
	}

	result := map[string]any{
		"state":               m.process.State().String(),
		"identity":            identity,
		"session_id":          m.process.SessionID(),
		"node_id":             m.process.NodeID,
		"uptime_seconds":      int(time.Since(m.process.StartedAt()).Seconds()),
		"field_size":          m.process.Field().Len(),
		"trust_score":         trust.LocalScore,
		"fingerprint":         m.process.Fingerprint(),
		"last_heartbeat":      lastHeartbeat,
		"coherence_state":     trust.CoherenceFingerprint,
		"quarantined_count":   queue.Quarantined,
		"deferred_count":      queue.Deferred,
	}

	// Node health — sibling services probed on heartbeat.
	if nh := m.process.NodeHealth(); nh != nil {
		if summary := nh.Summary(); len(summary) > 0 {
			result["node"] = summary
		}
	}

	if input.Verbose {
		result["workspace"] = m.cfg.WorkspaceRoot
		result["started_at"] = m.process.StartedAt().Format(time.RFC3339)
		result["last_heartbeat_hash"] = trust.LastHeartbeatHash
		result["last_quarantine"] = queue.LastQuarantineRFC3339
	}
	return marshalResult(result)
}

func (m *MCPServer) toolGetTrust(ctx context.Context, req *mcp.CallToolRequest, input getTrustInput) (*mcp.CallToolResult, any, error) {
	if m.process == nil {
		return textResult("process not initialized")
	}
	trust := m.process.TrustSnapshot()
	lastHeartbeat := ""
	if !trust.LastHeartbeatAt.IsZero() {
		lastHeartbeat = trust.LastHeartbeatAt.Format(time.RFC3339)
	}
	return marshalResult(map[string]any{
		"node_id":         m.process.NodeID,
		"trust_score":     trust.LocalScore,
		"fingerprint":     m.process.Fingerprint(),
		"last_heartbeat":  lastHeartbeat,
		"coherence_state": trust.CoherenceFingerprint,
	})
}

func (m *MCPServer) toolSearchMemory(ctx context.Context, req *mcp.CallToolRequest, input searchMemoryInput) (*mcp.CallToolResult, any, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}

	results, err := SearchMemory(m.cfg.WorkspaceRoot, input.Query, limit, input.Sector)
	if err != nil {
		return fallbackResult(fmt.Sprintf("search failed: %v", err),
			fmt.Sprintf("./scripts/cog memory search %q", input.Query))
	}
	return marshalResult(results)
}

func (m *MCPServer) toolGetNucleus(ctx context.Context, req *mcp.CallToolRequest, input getNucleusInput) (*mcp.CallToolResult, any, error) {
	if m.nucleus == nil {
		return textResult("nucleus not loaded")
	}
	return marshalResult(map[string]any{
		"name":      m.nucleus.Name,
		"role":      m.nucleus.Role,
		"summary":   m.nucleus.Summary(),
		"workspace": m.cfg.WorkspaceRoot,
		"port":      m.cfg.Port,
		"build":     BuildTime,
	})
}

func (m *MCPServer) toolReadCogdoc(ctx context.Context, req *mcp.CallToolRequest, input readCogdocInput) (*mcp.CallToolResult, any, error) {
	uri := input.URI
	if input.Section != "" && !strings.Contains(uri, "#") {
		uri += "#" + input.Section
	}

	res, err := ResolveURI(m.cfg.WorkspaceRoot, uri)
	if err != nil {
		return textResult(fmt.Sprintf("resolve failed: %v", err))
	}

	data, err := os.ReadFile(res.Path)
	if err != nil {
		return textResult(fmt.Sprintf("read failed: %v", err))
	}

	content := string(data)
	fm, _ := parseCogdocFrontmatter(content)
	issues := missingSchemaIssues(content)
	patchTemplate := patchTemplateForIssues(issues)
	result := readCogdocResult{
		URI:              uri,
		Path:             res.Path,
		Frontmatter:      fm,
		Content:          content,
		SchemaIssues:     issues,
		PatchFrontmatter: patchTemplate,
	}
	if hasSchemaIssue(issues, "missing_description") {
		result.SchemaHint = fmt.Sprintf("This CogDoc is missing a description field. If you can summarize it in one sentence, include it in your next response as: COGDOC_PATCH: %s | description: your summary here", uri)
	}

	// If fragment specified, extract section
	if res.Fragment != "" {
		section := extractSection(content, res.Fragment)
		if section != "" {
			result.Fragment = res.Fragment
			result.Content = section
			return marshalResult(result)
		}
	}

	return marshalResult(result)
}

func (m *MCPServer) toolPatchFrontmatter(ctx context.Context, req *mcp.CallToolRequest, input patchFrontmatterInput) (*mcp.CallToolResult, any, error) {
	if input.URI == "" {
		return textResult("uri is required")
	}
	if input.Patches.empty() {
		return textResult("at least one frontmatter patch is required")
	}

	res, err := ResolveURI(m.cfg.WorkspaceRoot, input.URI)
	if err != nil {
		return textResult(fmt.Sprintf("resolve failed: %v", err))
	}

	data, err := os.ReadFile(res.Path)
	if err != nil {
		return textResult(fmt.Sprintf("read failed: %v", err))
	}

	updated, fm, err := applyFrontmatterPatch(string(data), input.Patches)
	if err != nil {
		return textResult(fmt.Sprintf("patch failed: %v", err))
	}
	if err := os.WriteFile(res.Path, []byte(updated), 0o644); err != nil {
		return textResult(fmt.Sprintf("write failed: %v", err))
	}

	if m.process != nil {
		if idx, err := BuildIndex(m.cfg.WorkspaceRoot); err == nil {
			m.process.indexMu.Lock()
			m.process.index = idx
			m.process.indexMu.Unlock()
		}
	}

	return marshalResult(map[string]any{
		"updated":     true,
		"uri":         input.URI,
		"path":        res.Path,
		"frontmatter": fm,
	})
}

func (m *MCPServer) toolWriteCogdoc(ctx context.Context, req *mcp.CallToolRequest, input writeCogdocInput) (*mcp.CallToolResult, any, error) {
	if input.Path == "" || input.Content == "" {
		return textResult("path and content are required")
	}

	opts := CogDocWriteOpts{
		Title:   input.Title,
		Content: input.Content,
		Tags:    input.Tags,
		Status:  input.Status,
		DocType: input.DocType,
	}

	uri, err := WriteCogDoc(m.cfg.WorkspaceRoot, input.Path, opts)
	if err != nil {
		return textResult(fmt.Sprintf("write failed: %v", err))
	}

	fullPath := filepath.Join(m.cfg.WorkspaceRoot, ".cog", "mem", input.Path)
	return marshalResult(map[string]any{
		"written": true,
		"path":    fullPath,
		"uri":     uri,
	})
}

// CogDocWriteOpts holds options for writing a CogDoc via the internal API.
type CogDocWriteOpts struct {
	Title    string
	Content  string
	Tags     []string
	Status   string            // default "active"
	DocType  string            // e.g. "link", "conversation", "insight"
	Source   string            // e.g. "discord", "chatgpt"
	URL      string            // optional URL field
	SourceID string            // dedup key
	Extra    map[string]string // additional frontmatter fields
}

// detectSector extracts the memory sector from a memory-relative path.
// e.g. "semantic/insights/foo.md" -> "semantic"
func detectSector(path string) string {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 {
		return "semantic"
	}
	switch parts[0] {
	case "semantic", "episodic", "procedural", "reflective":
		return parts[0]
	default:
		return "semantic"
	}
}

// slugFromPath generates a slug-based id from a memory-relative path.
// e.g. "semantic/insights/my-topic.cog.md" -> "my-topic"
func slugFromPath(path string) string {
	base := filepath.Base(path)
	// Strip known extensions
	base = strings.TrimSuffix(base, ".cog.md")
	base = strings.TrimSuffix(base, ".md")
	// Slugify: lowercase, replace non-alnum with hyphens, collapse
	slug := strings.ToLower(base)
	re := regexp.MustCompile(`[^a-z0-9]+`)
	slug = re.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	return slug
}

// WriteCogDoc writes a CogDoc to the memory filesystem with proper frontmatter.
// This is the internal API used by the ingestion pipeline.
func WriteCogDoc(workspaceRoot string, path string, opts CogDocWriteOpts) (string, error) {
	fullPath := filepath.Join(workspaceRoot, ".cog", "mem", path)

	// Ensure directory exists
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("mkdir failed: %w", err)
	}

	sector := detectSector(path)
	docID := slugFromPath(path)
	now := time.Now().UTC().Format(time.RFC3339)

	status := opts.Status
	if status == "" {
		status = "active"
	}

	// Build YAML frontmatter
	var sb strings.Builder
	sb.WriteString("---\n")
	if docID != "" {
		sb.WriteString(fmt.Sprintf("id: %s\n", docID))
	}
	sb.WriteString(fmt.Sprintf("title: %q\n", opts.Title))
	sb.WriteString(fmt.Sprintf("created: %q\n", now))
	sb.WriteString(fmt.Sprintf("memory_sector: %s\n", sector))
	sb.WriteString(fmt.Sprintf("status: %s\n", status))

	if opts.DocType != "" {
		sb.WriteString(fmt.Sprintf("type: %s\n", opts.DocType))
	}
	if opts.Source != "" {
		sb.WriteString(fmt.Sprintf("source: %s\n", opts.Source))
	}
	if opts.URL != "" {
		sb.WriteString(fmt.Sprintf("url: %q\n", opts.URL))
	}
	if opts.SourceID != "" {
		sb.WriteString(fmt.Sprintf("source_id: %q\n", opts.SourceID))
	}

	if len(opts.Tags) > 0 {
		sb.WriteString("tags:\n")
		for _, tag := range opts.Tags {
			sb.WriteString(fmt.Sprintf("  - %s\n", tag))
		}
	}

	// Write any extra frontmatter fields
	for k, v := range opts.Extra {
		sb.WriteString(fmt.Sprintf("%s: %q\n", k, v))
	}

	sb.WriteString("---\n\n")
	sb.WriteString(opts.Content)

	if err := os.WriteFile(fullPath, []byte(sb.String()), 0644); err != nil {
		return "", fmt.Errorf("write failed: %w", err)
	}

	uri := "cog:mem/" + path
	return uri, nil
}

func (m *MCPServer) toolEmitEvent(ctx context.Context, req *mcp.CallToolRequest, input emitEventInput) (*mcp.CallToolResult, any, error) {
	if input.Type == "" {
		return textResult("event type is required. Valid types: attention.boost, session.marker, insight.captured, decision.made")
	}

	event := map[string]any{
		"type": input.Type,
	}
	if input.Payload != nil {
		event["payload"] = input.Payload
	}

	// Handle attention.boost: resolve URI to field key, then boost.
	if input.Type == "attention.boost" && m.process != nil {
		if uri, ok := input.Payload["uri"].(string); ok && uri != "" {
			fieldKey := ResolveToFieldKey(m.cfg.WorkspaceRoot, uri)
			weight := 1.0
			if w, ok := input.Payload["weight"].(float64); ok && w > 0 {
				weight = w
			}
			m.process.Field().Boost(fieldKey, weight)
			event["field_boosted"] = true
			event["resolved_key"] = fieldKey
		}
	}

	if err := EmitLedgerEvent(m.cfg, event); err != nil {
		return fallbackResult(fmt.Sprintf("emit failed: %v", err), "echo '{\"type\":\"...\"}' >> .cog/ledger/events.jsonl")
	}

	return marshalResult(map[string]any{
		"emitted": true,
		"type":    input.Type,
	})
}

func (m *MCPServer) toolGetIndex(ctx context.Context, req *mcp.CallToolRequest, input getIndexInput) (*mcp.CallToolResult, any, error) {
	index, err := BuildMemoryIndex(m.cfg.WorkspaceRoot, input.Sector)
	if err != nil {
		return textResult(fmt.Sprintf("index build failed: %v", err))
	}
	return marshalResult(index)
}

func (m *MCPServer) toolIngest(ctx context.Context, req *mcp.CallToolRequest, input ingestInput) (*mcp.CallToolResult, any, error) {
	if input.Source == "" || input.Format == "" || input.Data == "" {
		return textResult("source, format, and data are required")
	}

	// Build the pipeline fresh (stateless except for workspace root).
	pipeline := NewIngestPipeline(m.cfg.WorkspaceRoot)
	pipeline.Register(NewURLDecomposer(m.cfg.WorkspaceRoot))

	// Build the IngestRequest from input.
	ingestReq := &IngestRequest{
		Source:   IngestSource(input.Source),
		Format:   IngestFormat(input.Format),
		Data:     input.Data,
		Metadata: input.Metadata,
	}

	// Derive a source ID for dedup. For URLs, it's the URL itself.
	// For other formats, use data as the key (or metadata source_id if provided).
	sourceID := input.Data
	if id, ok := input.Metadata["source_id"]; ok && id != "" {
		sourceID = id
	}

	// Check for duplicates.
	if pipeline.CheckDuplicate(sourceID) {
		return marshalResult(map[string]any{
			"ingested":  false,
			"reason":    "duplicate",
			"source_id": sourceID,
		})
	}

	// Run decomposition.
	result, err := pipeline.Ingest(ctx, ingestReq)
	if err != nil {
		return textResult(fmt.Sprintf("ingest failed: %v", err))
	}

	// Ensure source ID is set on the result.
	if result.SourceID == "" {
		result.SourceID = sourceID
	}
	block := NormalizeIngestBlock(ingestReq, result)
	block.WorkspaceID = filepath.Base(m.cfg.WorkspaceRoot)
	if m.nucleus != nil {
		block.TargetIdentity = m.nucleus.Name
	}
	if m.process != nil {
		block.SessionID = m.process.SessionID()
		block.SourceIdentity = m.process.NodeID
		m.process.RecordBlock(block)
	}

	// Determine inbox subdirectory based on content type.
	var subdir string
	switch {
	case result.ContentType == ContentArticle || result.ContentType == ContentPaper ||
		result.ContentType == ContentRepo || result.ContentType == ContentVideo ||
		result.ContentType == ContentTool || result.URL != "":
		subdir = "links"
	case input.Format == string(FormatConversation) || input.Format == string(FormatMessage):
		subdir = "conversations"
	default:
		subdir = "documents"
	}

	// Generate filename: {source}-{date}-{slug}.cog.md
	date := time.Now().UTC().Format("2006-01-02")
	slug := slugify(result.Title)
	if slug == "" {
		slug = "untitled"
	}
	filename := fmt.Sprintf("%s-%s-%s.cog.md", input.Source, date, slug)
	memPath := filepath.Join("semantic", "inbox", subdir, filename)

	// Write the CogDoc.
	opts := CogDocWriteOpts{
		Title:    result.Title,
		Content:  buildIngestContent(result),
		Tags:     result.Tags,
		Status:   string(StatusRaw),
		DocType:  string(result.ContentType),
		Source:   string(result.Source),
		URL:      result.URL,
		SourceID: result.SourceID,
	}

	decision := DefaultMembranePolicy{}.Evaluate(block)
	memPath, opts, shouldWrite := ApplyMembraneDecision(memPath, opts, decision)
	if !shouldWrite {
		slog.Info("ingest: discarded by membrane policy", "reason", decision.Reason)
		return marshalResult(map[string]any{
			"ingested":  false,
			"decision":  string(decision.Decision),
			"reason":    decision.Reason,
			"source_id": result.SourceID,
		})
	}
	if decision.Decision == Quarantine {
		slog.Warn("ingest: quarantined by membrane policy", "reason", decision.QuarantineReason, "path", memPath)
	}
	if decision.Decision == Defer {
		slog.Info("ingest: deferred by membrane policy", "reason", decision.Reason, "path", memPath)
	}

	uri, err := WriteCogDoc(m.cfg.WorkspaceRoot, memPath, opts)
	if err != nil {
		return textResult(fmt.Sprintf("write cogdoc failed: %v", err))
	}

	if decision.Decision == Integrate && m.process != nil {
		// Boost the attentional field immediately so the new CogDoc is visible
		// in context assembly without waiting for the next full field.Update().
		absPath := filepath.Join(m.cfg.WorkspaceRoot, ".cog", "mem", memPath)
		m.process.Field().Boost(absPath, inboxRawBoost)
	}

	// Emit ledger event.
	_ = EmitIngestEvent(m.cfg, result, memPath)

	return marshalResult(map[string]any{
		"ingested":     true,
		"decision":     string(decision.Decision),
		"reason":       decision.Reason,
		"path":         memPath,
		"uri":          uri,
		"title":        result.Title,
		"content_type": string(result.ContentType),
	})
}

// slugify converts a string to a URL-friendly slug.
func slugify(s string) string {
	s = strings.ToLower(s)
	re := regexp.MustCompile(`[^a-z0-9]+`)
	s = re.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 50 {
		// Truncate at a hyphen boundary if possible.
		s = s[:50]
		if idx := strings.LastIndex(s, "-"); idx > 20 {
			s = s[:idx]
		}
	}
	return s
}

// buildIngestContent generates markdown body from an IngestResult.
func buildIngestContent(r *IngestResult) string {
	var sb strings.Builder
	sb.WriteString("# " + r.Title + "\n\n")
	if r.URL != "" {
		sb.WriteString("**URL:** " + r.URL + "\n\n")
	}
	if r.Domain != "" {
		sb.WriteString("**Domain:** " + r.Domain + "\n\n")
	}
	if r.Summary != "" {
		sb.WriteString("## Summary\n\n" + r.Summary + "\n\n")
	}
	if len(r.Fields) > 0 {
		sb.WriteString("## Metadata\n\n")
		for k, v := range r.Fields {
			sb.WriteString(fmt.Sprintf("- **%s:** %s\n", k, v))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func (p cogdocFrontmatterPatch) empty() bool {
	return strings.TrimSpace(p.Description) == "" && len(p.Tags) == 0 && strings.TrimSpace(p.Type) == ""
}

func patchTemplateForIssues(issues []string) map[string]any {
	if len(issues) == 0 {
		return nil
	}
	template := map[string]any{}
	for _, issue := range issues {
		switch issue {
		case "missing_description":
			template["description"] = ""
		case "missing_tags":
			template["tags"] = []string{}
		case "missing_type":
			template["type"] = ""
		}
	}
	if len(template) == 0 {
		return nil
	}
	return template
}

func hasSchemaIssue(issues []string, want string) bool {
	for _, issue := range issues {
		if issue == want {
			return true
		}
	}
	return false
}

func applyFrontmatterPatch(content string, patch cogdocFrontmatterPatch) (string, cogdocFrontmatter, error) {
	var (
		raw  map[string]any
		body string
	)

	yamlBlock, extractedBody, ok := extractFrontmatterYAML(content)
	if ok {
		if err := yaml.Unmarshal([]byte(yamlBlock), &raw); err != nil {
			return "", cogdocFrontmatter{}, fmt.Errorf("parse frontmatter: %w", err)
		}
		body = extractedBody
	} else {
		raw = map[string]any{}
		body = strings.TrimLeft(content, "\r\n")
	}
	if raw == nil {
		raw = map[string]any{}
	}

	if strings.TrimSpace(patch.Description) != "" {
		raw["description"] = strings.TrimSpace(patch.Description)
	}
	if len(patch.Tags) > 0 {
		raw["tags"] = patch.Tags
	}
	if strings.TrimSpace(patch.Type) != "" {
		raw["type"] = strings.TrimSpace(patch.Type)
	}

	marshaled, err := yaml.Marshal(raw)
	if err != nil {
		return "", cogdocFrontmatter{}, fmt.Errorf("marshal frontmatter: %w", err)
	}

	updated := fmt.Sprintf("---\n%s---\n", marshaled)
	if body != "" {
		updated += "\n" + body
	}

	fm, _ := parseCogdocFrontmatter(updated)
	return updated, fm, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func marshalResult(data any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return textResult(fmt.Sprintf("marshal error: %v", err))
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(b)},
		},
	}, nil, nil
}

func textResult(text string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
	}, nil, nil
}

// fallbackResult returns an error message with a CLI fallback command.
// This is the graceful degradation path — when the kernel is unavailable,
// the agent can fall back to shell commands that work without it.
func fallbackResult(errMsg, fallbackCmd string) (*mcp.CallToolResult, any, error) {
	text := fmt.Sprintf("%s\n\nFallback (kernel unavailable): %s", errMsg, fallbackCmd)
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
		IsError: true,
	}, nil, nil
}

// extractSection pulls a section from markdown by heading anchor.
func extractSection(content, anchor string) string {
	lines := strings.Split(content, "\n")
	var capturing bool
	var level int
	var result []string

	for _, line := range lines {
		if strings.Contains(line, "{#"+anchor+"}") || strings.Contains(line, "# "+anchor) {
			capturing = true
			level = strings.Count(strings.TrimLeft(line, " "), "#")
			result = append(result, line)
			continue
		}
		if capturing {
			// Stop at same or higher level heading
			trimmed := strings.TrimLeft(line, " ")
			if strings.HasPrefix(trimmed, "#") {
				headingLevel := strings.Count(trimmed, "#")
				if headingLevel <= level {
					break
				}
			}
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// init logging for MCP operations
func init() {
	_ = slog.Default() // ensure slog is initialized
}

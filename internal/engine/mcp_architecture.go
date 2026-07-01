// mcp_architecture.go — MCP tool wiring for the Architecture skill plugin.
//
// Wires the eight Python-implemented architecture tools at
// {WorkspaceRoot}/.cog/skills/architecture/tools/*.py into the kernel's MCP
// surface. Each MCP tool is a thin wrapper that subprocess-execs the Python
// script with the appropriate args and returns its JSON output.
//
// The skill itself is canonical-substrate (lives at .cog/skills/, not
// .claude/skills/). The kernel's existing skill-discovery (serve_skills.go's
// skillDirs) doesn't see .cog/skills/ — but these MCP tools bypass that
// machinery and exec the scripts directly using a known relative path.
//
// Composes with ADR-098 (SkillProjectionReconciler) which proposes general
// substrate-canonical skill discovery; this file is the targeted wiring
// for the Architecture plugin specifically until that lands.
//
// Tools registered:
//   cog_architecture_resolve   — handle → canonical URI + descriptor
//   cog_architecture_read      — read parsed tree (or --frontmatter-only)
//   cog_architecture_list      — enumerate with filters
//   cog_architecture_search    — ranked search
//   cog_architecture_audit     — 8-check audit gate
//   cog_architecture_write     — validated write
//   cog_architecture_propose   — generate draft tree
//   cog_architecture_project   — project to target workspace
//
// Schema-validated input structs map onto Python CLI args. JSON output from
// the Python script is unmarshaled and returned as the MCP result.

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── Input structs ───────────────────────────────────────────────────────────

type architectureResolveInput struct {
	Handle string `json:"handle" jsonschema:"the handle to resolve (canonical URI, alias, slug, legacy numeric, or wiki-link)"`
}

type architectureReadInput struct {
	Handle          string `json:"handle" jsonschema:"the handle to read"`
	FrontmatterOnly bool   `json:"frontmatter_only,omitempty" jsonschema:"return only frontmatter (faster), not full blocks"`
}

type architectureListInput struct {
	Kind   string `json:"kind,omitempty" jsonschema:"filter by kind: adr | rfc"`
	Status string `json:"status,omitempty" jsonschema:"filter by status: draft | accepted | superseded | retired | proposed"`
	Tag    string `json:"tag,omitempty" jsonschema:"filter by tag (must appear in tags list)"`
	Scope  string `json:"scope,omitempty" jsonschema:"filter by scope field (kernel | org | public-ready)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"limit results (0 = all)"`
}

type architectureSearchInput struct {
	Query  string `json:"query" jsonschema:"search query (case-insensitive substring)"`
	Kind   string `json:"kind,omitempty" jsonschema:"filter by kind: adr | rfc"`
	Status string `json:"status,omitempty" jsonschema:"filter by status"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max results (default 20)"`
	Field  string `json:"field,omitempty" jsonschema:"which field to search: title | tags | body | all (default all)"`
}

type architectureAuditInput struct {
	Handle      string `json:"handle" jsonschema:"the handle to audit"`
	PublicReady bool   `json:"public_ready,omitempty" jsonschema:"activate strict gates (PII + restricted-tag checks block projection)"`
}

type architectureWriteInput struct {
	Slug   string         `json:"slug" jsonschema:"canonical slug; must match tree.frontmatter.slug"`
	Tree   map[string]any `json:"tree" jsonschema:"the parsed tree to write (output of architecture_propose or architecture_read)"`
	Author string         `json:"author" jsonschema:"identity slug of author"`
}

type architectureProposeInput struct {
	Kind           string `json:"kind" jsonschema:"adr | rfc"`
	Slug           string `json:"slug" jsonschema:"proposed kebab-slug; must not already exist"`
	Title          string `json:"title" jsonschema:"human-readable title"`
	ContextSummary string `json:"context_summary" jsonschema:"1-3 sentence context for preservation_directive"`
	Author         string `json:"author,omitempty" jsonschema:"identity slug of author (default: slowbro)"`
}

type architectureProjectInput struct {
	Handle      string         `json:"handle" jsonschema:"the handle to project"`
	Target      string         `json:"target" jsonschema:"target workspace path"`
	Filter      map[string]any `json:"filter,omitempty" jsonschema:"filter config (audience_scrub etc.); default = pass-through"`
	PublicReady bool           `json:"public_ready,omitempty" jsonschema:"run audit in public-ready mode (PII + restricted-tag block)"`
}

// ─── Registration ────────────────────────────────────────────────────────────

// registerArchitectureTools registers all 8 architecture skill tools.
// Called from MCPServer.registerTools. All plumbing (deferred) — reachable
// via cog_tool_search + cog_tool_invoke, not the porcelain preload set.
func (m *MCPServer) registerArchitectureTools() {
	trackToolDeferred(m, &mcp.Tool{
		Name:        "cog_architecture_resolve",
		Description: "Resolve any handle (canonical URI, alias, slug, legacy numeric URI, wiki-link) to canonical URI + on-disk path. From the architecture skill plugin per ADR-architecture-memory-canonical-form-and-projection.",
	}, withToolObserver(m, "cog_architecture_resolve", m.toolArchitectureResolve))

	trackToolDeferred(m, &mcp.Tool{
		Name:        "cog_architecture_read",
		Description: "Read an architecture document by handle. Returns parsed CogBlock tree, or frontmatter-only for fast lookups.",
	}, withToolObserver(m, "cog_architecture_read", m.toolArchitectureRead))

	trackToolDeferred(m, &mcp.Tool{
		Name:        "cog_architecture_list",
		Description: "Enumerate architecture documents with optional filters (kind, status, tag, scope, limit). Returns JSON array of doc stubs.",
	}, withToolObserver(m, "cog_architecture_list", m.toolArchitectureList))

	trackToolDeferred(m, &mcp.Tool{
		Name:        "cog_architecture_search",
		Description: "Ranked full-text + frontmatter search across the architecture corpus. Scoring: title=10x, tags=5x, body=1x; 2x for accepted status; -1 for superseded/retired.",
	}, withToolObserver(m, "cog_architecture_search", m.toolArchitectureSearch))

	trackToolDeferred(m, &mcp.Tool{
		Name:        "cog_architecture_audit",
		Description: "Run the projection-time audit gate on a document. 8 checks (schema, slug, roundtrip, PII, restricted-tags, distinctions, salience, refs). public_ready=true activates strict gates that block projection on PII or restricted tags.",
	}, withToolObserver(m, "cog_architecture_audit", m.toolArchitectureAudit))

	trackToolDeferred(m, &mcp.Tool{
		Name:        "cog_architecture_write",
		Description: "Write or update an architecture document at its canonical path. Validates schema, slug consistency, and roundtrip-clean before atomic rename. Returns canonical URI + action (created|updated).",
	}, withToolObserver(m, "cog_architecture_write", m.toolArchitectureWrite))

	trackToolDeferred(m, &mcp.Tool{
		Name:        "cog_architecture_propose",
		Description: "Generate an ADR or RFC draft tree with stub sections and pre-populated frontmatter. Does NOT write to disk; returns the tree for review + subsequent cog_architecture_write.",
	}, withToolObserver(m, "cog_architecture_propose", m.toolArchitecturePropose))

	trackToolDeferred(m, &mcp.Tool{
		Name:        "cog_architecture_project",
		Description: "Project a canonical architecture document to a target workspace. Composes resolve + audit + scrub + write. public_ready=true blocks projection on PII or restricted-tag findings before any target write.",
	}, withToolObserver(m, "cog_architecture_project", m.toolArchitectureProject))
}

// ─── Shared helper ───────────────────────────────────────────────────────────

// architectureScriptPath returns the absolute path to a tool script.
func (m *MCPServer) architectureScriptPath(scriptName string) string {
	return filepath.Join(m.cfg.WorkspaceRoot, ".cog", "skills", "architecture", "tools", scriptName)
}

// execArchitectureTool runs a script with args; returns parsed JSON or fallback.
// stdinJSON, if non-empty, is piped to the script's stdin (used by write).
func (m *MCPServer) execArchitectureTool(ctx context.Context, scriptName string, args []string, stdinJSON string) (*mcp.CallToolResult, any, error) {
	scriptPath := m.architectureScriptPath(scriptName)
	cmdArgs := append([]string{scriptPath}, args...)
	cmd := exec.CommandContext(ctx, "python3", cmdArgs...)
	if stdinJSON != "" {
		cmd.Stdin = strings.NewReader(stdinJSON)
	}
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		return fallbackResult(
			fmt.Sprintf("script %s failed: %v; stderr: %s", scriptName, err, stderr),
			fmt.Sprintf("python3 %s %s", scriptPath, strings.Join(args, " ")),
		)
	}
	var parsed any
	if err := json.Unmarshal(out, &parsed); err != nil {
		// Some tools may emit non-JSON (e.g., list --format=table); return raw.
		return m.cappedMarshal(map[string]any{"output": string(out)})
	}
	return m.cappedMarshal(parsed)
}

// ─── Tool handlers ───────────────────────────────────────────────────────────

func (m *MCPServer) toolArchitectureResolve(ctx context.Context, req *mcp.CallToolRequest, in architectureResolveInput) (*mcp.CallToolResult, any, error) {
	if in.Handle == "" {
		return fallbackResult("handle is required", "")
	}
	return m.execArchitectureTool(ctx, "architecture_resolve.py", []string{in.Handle}, "")
}

func (m *MCPServer) toolArchitectureRead(ctx context.Context, req *mcp.CallToolRequest, in architectureReadInput) (*mcp.CallToolResult, any, error) {
	if in.Handle == "" {
		return fallbackResult("handle is required", "")
	}
	args := []string{in.Handle}
	if in.FrontmatterOnly {
		args = append(args, "--frontmatter-only")
	}
	return m.execArchitectureTool(ctx, "architecture_read.py", args, "")
}

func (m *MCPServer) toolArchitectureList(ctx context.Context, req *mcp.CallToolRequest, in architectureListInput) (*mcp.CallToolResult, any, error) {
	args := []string{}
	if in.Kind != "" {
		args = append(args, "--kind="+in.Kind)
	}
	if in.Status != "" {
		args = append(args, "--status="+in.Status)
	}
	if in.Tag != "" {
		args = append(args, "--tag="+in.Tag)
	}
	if in.Scope != "" {
		args = append(args, "--scope="+in.Scope)
	}
	if in.Limit > 0 {
		args = append(args, "--limit="+strconv.Itoa(in.Limit))
	}
	args = append(args, "--format=json")
	return m.execArchitectureTool(ctx, "architecture_list.py", args, "")
}

func (m *MCPServer) toolArchitectureSearch(ctx context.Context, req *mcp.CallToolRequest, in architectureSearchInput) (*mcp.CallToolResult, any, error) {
	if in.Query == "" {
		return fallbackResult("query is required", "")
	}
	args := []string{in.Query}
	if in.Kind != "" {
		args = append(args, "--kind="+in.Kind)
	}
	if in.Status != "" {
		args = append(args, "--status="+in.Status)
	}
	if in.Limit > 0 {
		args = append(args, "--limit="+strconv.Itoa(in.Limit))
	}
	if in.Field != "" {
		args = append(args, "--field="+in.Field)
	}
	return m.execArchitectureTool(ctx, "architecture_search.py", args, "")
}

func (m *MCPServer) toolArchitectureAudit(ctx context.Context, req *mcp.CallToolRequest, in architectureAuditInput) (*mcp.CallToolResult, any, error) {
	if in.Handle == "" {
		return fallbackResult("handle is required", "")
	}
	args := []string{in.Handle}
	if in.PublicReady {
		args = append(args, "--public-ready")
	}
	return m.execArchitectureTool(ctx, "architecture_audit.py", args, "")
}

func (m *MCPServer) toolArchitectureWrite(ctx context.Context, req *mcp.CallToolRequest, in architectureWriteInput) (*mcp.CallToolResult, any, error) {
	if in.Slug == "" || in.Author == "" {
		return fallbackResult("slug and author are required", "")
	}
	if len(in.Tree) == 0 {
		return fallbackResult("tree is required", "")
	}
	treeJSON, err := json.Marshal(in.Tree)
	if err != nil {
		return fallbackResult(fmt.Sprintf("tree marshal failed: %v", err), "")
	}
	args := []string{in.Slug, "--tree=-", "--author=" + in.Author}
	return m.execArchitectureTool(ctx, "architecture_write.py", args, string(treeJSON))
}

func (m *MCPServer) toolArchitecturePropose(ctx context.Context, req *mcp.CallToolRequest, in architectureProposeInput) (*mcp.CallToolResult, any, error) {
	if in.Kind == "" || in.Slug == "" || in.Title == "" || in.ContextSummary == "" {
		return fallbackResult("kind, slug, title, and context_summary are required", "")
	}
	args := []string{
		"--kind=" + in.Kind,
		"--slug=" + in.Slug,
		"--title=" + in.Title,
		"--context-summary=" + in.ContextSummary,
	}
	if in.Author != "" {
		args = append(args, "--author="+in.Author)
	}
	return m.execArchitectureTool(ctx, "architecture_propose.py", args, "")
}

func (m *MCPServer) toolArchitectureProject(ctx context.Context, req *mcp.CallToolRequest, in architectureProjectInput) (*mcp.CallToolResult, any, error) {
	if in.Handle == "" || in.Target == "" {
		return fallbackResult("handle and target are required", "")
	}
	// Contain the caller-supplied target: architecture_project.py writes to it,
	// so reject any target that resolves outside the workspace root. The script
	// runs with the kernel's CWD (the workspace), so a relative target resolves
	// the same way here; we validate the resolved path and pass the caller's
	// value unchanged for valid (contained) inputs.
	resolvedTarget := filepath.Clean(in.Target)
	if !filepath.IsAbs(resolvedTarget) {
		resolvedTarget = filepath.Join(m.cfg.WorkspaceRoot, resolvedTarget)
	}
	if !pathWithin(m.cfg.WorkspaceRoot, resolvedTarget) {
		return fallbackResult(fmt.Sprintf("target %q is outside the workspace root", in.Target), "")
	}
	args := []string{in.Handle, "--target=" + in.Target}
	if len(in.Filter) > 0 {
		filterJSON, err := json.Marshal(in.Filter)
		if err == nil {
			args = append(args, "--filter="+string(filterJSON))
		}
	}
	if in.PublicReady {
		args = append(args, "--public-ready")
	}
	return m.execArchitectureTool(ctx, "architecture_project.py", args, "")
}

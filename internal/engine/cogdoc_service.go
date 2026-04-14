//go:build mcpserver

// cogdoc_service.go — Unified CogDoc write path
//
// CogDocService is the ONLY write path for CogDoc mutations. It ensures that
// every write synchronises all kernel state: file write, index refresh,
// attentional field boost, and ledger event emission.
//
// Before this service, three MCP tool handlers (toolWriteCogdoc,
// toolPatchFrontmatter, toolIngest) each performed an inconsistent subset
// of these steps. CogDocService centralises the contract.
package engine

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// CogDocService provides a single, consistent write path for all CogDoc mutations.
// All writes go through WriteAndSync or PatchAndSync so that the index, field,
// and ledger stay in lockstep.
type CogDocService struct {
	cfg     *Config
	process *Process
}

// NewCogDocService constructs a CogDocService bound to the given config and process.
func NewCogDocService(cfg *Config, process *Process) *CogDocService {
	return &CogDocService{
		cfg:     cfg,
		process: process,
	}
}

// WriteResult is the outcome of a successful CogDoc write or patch.
type WriteResult struct {
	Path string // absolute filesystem path
	URI  string // canonical cog:// URI
}

// WriteAndSync writes a CogDoc and synchronises all kernel state.
// This is the canonical write path — every CogDoc creation or overwrite
// MUST flow through here.
//
// Steps:
//  1. Write file via WriteCogDoc()
//  2. Resolve absolute path
//  3. Refresh CogDoc index (full rebuild)
//  4. Boost attentional field for the new/updated path
//  5. Emit ledger event (type: "cogdoc.written")
func (s *CogDocService) WriteAndSync(path string, opts CogDocWriteOpts) (*WriteResult, error) {
	// 1. Write file.
	uri, err := WriteCogDoc(s.cfg.WorkspaceRoot, path, opts)
	if err != nil {
		return nil, fmt.Errorf("write cogdoc: %w", err)
	}

	// 2. Resolve absolute path.
	absPath := filepath.Join(s.cfg.WorkspaceRoot, ".cog", "mem", path)

	// 3. Refresh index.
	s.refreshIndex()

	// 4. Boost attentional field.
	s.boostField(absPath)

	// 5. Emit ledger event.
	s.emitLedgerEvent("cogdoc.written", absPath, uri)

	return &WriteResult{Path: absPath, URI: uri}, nil
}

// PatchAndSync patches CogDoc frontmatter and synchronises all kernel state.
// This is the canonical patch path — every frontmatter mutation MUST flow
// through here.
//
// Steps:
//  1. Resolve URI to filesystem path
//  2. Read file and apply frontmatter patch
//  3. Write patched file
//  4. Refresh CogDoc index
//  5. Boost attentional field
//  6. Emit ledger event (type: "cogdoc.patched")
func (s *CogDocService) PatchAndSync(uri string, patches cogdocFrontmatterPatch) (*WriteResult, error) {
	// 1. Resolve URI to path.
	res, err := ResolveURI(s.cfg.WorkspaceRoot, uri)
	if err != nil {
		return nil, fmt.Errorf("resolve URI %q: %w", uri, err)
	}

	// 2. Read file.
	data, err := os.ReadFile(res.Path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", res.Path, err)
	}

	// Apply patch.
	updated, _, err := applyFrontmatterPatch(string(data), patches)
	if err != nil {
		return nil, fmt.Errorf("patch frontmatter: %w", err)
	}

	// 3. Write patched file.
	if err := os.WriteFile(res.Path, []byte(updated), 0o644); err != nil {
		return nil, fmt.Errorf("write %q: %w", res.Path, err)
	}

	// 4. Refresh index.
	s.refreshIndex()

	// 5. Boost attentional field.
	s.boostField(res.Path)

	// 6. Emit ledger event.
	s.emitLedgerEvent("cogdoc.patched", res.Path, uri)

	return &WriteResult{Path: res.Path, URI: uri}, nil
}

// refreshIndex rebuilds the CogDoc index and swaps it into the process
// under the indexMu lock. This is the same pattern used by
// toolPatchFrontmatter and runConsolidation in process.go.
func (s *CogDocService) refreshIndex() {
	if s.process == nil {
		return
	}
	idx, err := BuildIndex(s.cfg.WorkspaceRoot)
	if err != nil {
		slog.Warn("cogdoc_service: index refresh failed", "err", err)
		return
	}
	s.process.indexMu.Lock()
	s.process.index = idx
	s.process.indexMu.Unlock()
}

// boostField applies a moderate attentional boost to the written/patched path
// so the CogDoc is immediately visible in context assembly without waiting
// for the next full field.Update() cycle.
func (s *CogDocService) boostField(absPath string) {
	if s.process == nil {
		return
	}
	s.process.Field().Boost(absPath, 0.5)
}

// emitLedgerEvent writes a ledger event recording the CogDoc mutation.
// Failures are logged but do not propagate — the file write already succeeded,
// and a missing ledger entry is recoverable.
func (s *CogDocService) emitLedgerEvent(eventType, absPath, uri string) {
	event := map[string]any{
		"type": eventType,
		"payload": map[string]any{
			"path": absPath,
			"uri":  uri,
		},
	}
	if err := EmitLedgerEvent(s.cfg, event); err != nil {
		slog.Warn("cogdoc_service: ledger emit failed", "type", eventType, "err", err)
	}
}

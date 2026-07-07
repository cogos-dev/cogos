// emit.go — writes IngestRecords as JSONL into the observatory's normalized
// ingest surface, and orchestrates a full discover -> parse -> emit pass.
//
// Layout produced: <workspace>/.cog/observatory/ingest/lm-studio/<session>.jsonl
// One file per LM Studio conversation file, named after its derived session
// id so re-emission overwrites the same file in place (the ingest consumer
// re-parses a source's files wholesale on drift, so overwrite-in-place is the
// correct semantics — matches how an incremental observer run is expected to
// behave per internal/conversations/ingest_parser.go's file-provenance model).
package lmstudio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// IngestDir returns the ingest directory for this source under the given
// workspace root: <root>/.cog/observatory/ingest/lm-studio.
func IngestDir(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".cog", "observatory", "ingest", Source)
}

// StateFilePath returns the path of this observer's own emitted-state
// tracking file. Kept separate from the ingest directory (which is consumed
// by the kernel) and namespaced under the observer's own state, mirroring
// how other long-running local tools keep run-state outside data
// directories they don't own.
func StateFilePath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".cog", "state", "lmstudio-observer", "emitted.json")
}

// WriteJSONL serializes records as newline-delimited JSON and writes them to
// path, atomically (temp file + rename). An empty records slice still writes
// an empty file — this lets a conversation that lost all its content (e.g.
// all turns were pure tool calls) clear a stale JSONL from a prior run.
func WriteJSONL(path string, records []IngestRecord) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			return fmt.Errorf("lmstudio: encode record for %s: %w", path, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("lmstudio: mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("lmstudio: write temp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("lmstudio: rename into place %s: %w", path, err)
	}
	return nil
}

// RunResult summarizes one orchestrated observer pass.
type RunResult struct {
	Scanned    int      // total conversation files discovered
	Emitted    int      // files that were (re-)parsed and written
	Skipped    int      // files unchanged since last run
	TotalTurns int      // sum of ingest records written across emitted files
	Errors     []string // per-file errors (non-fatal; the pass continues)
}

// RunOptions configures one observer pass.
type RunOptions struct {
	// LMStudioDir is the root to scan for *.conversation.json files.
	// Defaults to DefaultLMStudioDir() when empty.
	LMStudioDir string

	// WorkspaceRoot is the CogOS workspace whose .cog/observatory/ingest/
	// directory receives the emitted JSONL. Required.
	WorkspaceRoot string

	// MaxTurnLen caps emitted turn text length. Defaults to
	// defaultMaxTurnLen (8192) when <= 0.
	MaxTurnLen int

	// Force re-emits every discovered file regardless of tracked state.
	Force bool
}

// Run performs one discover -> parse -> emit pass: it scans opts.LMStudioDir
// for conversation files, skips ones unchanged since the last recorded run
// (per the state file under opts.WorkspaceRoot), parses and emits the rest as
// normalized ingest JSONL, and persists updated state. Individual per-file
// parse/write failures are recorded in RunResult.Errors and do not abort the
// pass — one malformed conversation file must not block ingestion of the
// rest of the corpus.
func Run(opts RunOptions) (RunResult, error) {
	var res RunResult

	lmDir := opts.LMStudioDir
	if lmDir == "" {
		d, err := DefaultLMStudioDir()
		if err != nil {
			return res, err
		}
		lmDir = d
	}
	if opts.WorkspaceRoot == "" {
		return res, fmt.Errorf("lmstudio: WorkspaceRoot is required")
	}

	files, err := DiscoverConversationFiles(lmDir)
	if err != nil {
		return res, err
	}
	res.Scanned = len(files)

	statePath := StateFilePath(opts.WorkspaceRoot)
	state, err := LoadEmittedState(statePath)
	if err != nil {
		return res, err
	}

	ingestDir := IngestDir(opts.WorkspaceRoot)

	for _, f := range files {
		if !opts.Force && !state.NeedsEmit(f) {
			res.Skipped++
			continue
		}

		sessionID, records, parseErr := ParseFile(f.Path, opts.MaxTurnLen)
		if parseErr != nil {
			res.Errors = append(res.Errors, parseErr.Error())
			continue
		}

		outPath := filepath.Join(ingestDir, sessionID+".jsonl")
		if writeErr := WriteJSONL(outPath, records); writeErr != nil {
			res.Errors = append(res.Errors, writeErr.Error())
			continue
		}

		state.Record(f)
		res.Emitted++
		res.TotalTurns += len(records)
	}

	if err := state.Save(statePath); err != nil {
		return res, err
	}

	return res, nil
}

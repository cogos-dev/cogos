// emit_test.go — end-to-end test of the discover -> parse -> emit pass
// (Run), verifying: correct ingest directory layout, incremental skip
// behavior on a second unchanged run, and re-emission after a file changes.
package lmstudio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRun_EmitsIngestJSONLIntoWorkspace(t *testing.T) {
	lmDir := t.TempDir()
	workspace := t.TempDir()
	writeFixture(t, lmDir, "Cog.", "1700000000000.conversation.json")

	res, err := Run(RunOptions{LMStudioDir: lmDir, WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Scanned != 1 || res.Emitted != 1 || res.Skipped != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}

	wantPath := filepath.Join(IngestDir(workspace), "Cog.-1700000000000.jsonl")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected ingest file at %s: %v", wantPath, err)
	}

	// Second run over the same unchanged file must skip, not re-emit.
	res2, err := Run(RunOptions{LMStudioDir: lmDir, WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.Emitted != 0 || res2.Skipped != 1 {
		t.Fatalf("second run should skip unchanged file: %+v", res2)
	}
}

func TestRun_ForceReemitsUnchangedFiles(t *testing.T) {
	lmDir := t.TempDir()
	workspace := t.TempDir()
	writeFixture(t, lmDir, "Cog.", "1700000000000.conversation.json")

	if _, err := Run(RunOptions{LMStudioDir: lmDir, WorkspaceRoot: workspace}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	res, err := Run(RunOptions{LMStudioDir: lmDir, WorkspaceRoot: workspace, Force: true})
	if err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if res.Emitted != 1 || res.Skipped != 0 {
		t.Fatalf("forced run should re-emit: %+v", res)
	}
}

func TestRun_MissingWorkspaceRootErrors(t *testing.T) {
	if _, err := Run(RunOptions{LMStudioDir: t.TempDir()}); err == nil {
		t.Error("expected error for missing WorkspaceRoot")
	}
}

func TestRun_MissingLMStudioDirIsNotFatal(t *testing.T) {
	workspace := t.TempDir()
	res, err := Run(RunOptions{
		LMStudioDir:   filepath.Join(t.TempDir(), "no-such-dir"),
		WorkspaceRoot: workspace,
	})
	if err != nil {
		t.Fatalf("Run with missing lmstudio dir: %v", err)
	}
	if res.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0", res.Scanned)
	}
}

func TestRun_OneMalformedFileDoesNotBlockOthers(t *testing.T) {
	lmDir := t.TempDir()
	workspace := t.TempDir()
	writeFixture(t, lmDir, "Good", "111.conversation.json")

	badDir := filepath.Join(lmDir, "Bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "222.conversation.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write malformed fixture: %v", err)
	}

	res, err := Run(RunOptions{LMStudioDir: lmDir, WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Scanned != 2 {
		t.Fatalf("Scanned = %d, want 2", res.Scanned)
	}
	if res.Emitted != 1 {
		t.Fatalf("Emitted = %d, want 1 (the good file)", res.Emitted)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("Errors = %v, want exactly 1", res.Errors)
	}

	goodOut := filepath.Join(IngestDir(workspace), "Good-111.jsonl")
	if _, err := os.Stat(goodOut); err != nil {
		t.Errorf("expected good file emitted despite sibling error: %v", err)
	}
}

func TestWriteJSONL_EmptyRecordsWritesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := WriteJSONL(path, nil); err != nil {
		t.Fatalf("WriteJSONL(nil): %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(data))
	}
}

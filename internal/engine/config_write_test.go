// config_write_test.go — unit tests for the Config Mutation API.
//
// Covers the spec from agent-O-config-mutation-design §Test plan:
//  1. Full patch, happy path
//  2. Sparse patch, single field
//  3. Patch reload-safe field
//  4. Null removes a field
//  5. Scope: v3 override
//  6. Validation rejects bad heartbeat
//  7. Validation rejects out-of-range port
//  8. Dry run does not touch disk
//  9. Atomic write semantics (no torn file under failure)
//  10. Backup rotation keeps 10
//  11. Corrupt existing file is refused
//  12. Concurrent writes serialize
//
// Plus MCP + HTTP roundtrip tests (see config_write_wire_test.go).
package engine

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

func seedKernelYAML(t *testing.T, root, content string) string {
	t.Helper()
	path := filepath.Join(root, ".cog", "config", "kernel.yaml")
	writeTestFile(t, path, content)
	return path
}

func readKernelYAML(t *testing.T, path string) kernelConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read kernel.yaml: %v", err)
	}
	var kc kernelConfig
	if err := yaml.Unmarshal(data, &kc); err != nil {
		t.Fatalf("unmarshal kernel.yaml: %v", err)
	}
	return kc
}

//  1. Full patch on a fresh workspace populates every field + creates a backup
//     when an existing file was overwritten.
func TestWriteConfigPatch_FullPatchHappyPath(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	path := seedKernelYAML(t, root, "port: 6931\n")

	patch := map[string]any{
		"port":                         float64(7777),
		"consolidation_interval":       float64(1800),
		"heartbeat_interval":           float64(45),
		"salience_days_window":         float64(30),
		"output_reserve":               float64(8192),
		"trm_weights_path":             "/tmp/trm.w",
		"trm_embeddings_path":          "/tmp/trm.e",
		"trm_chunks_path":              "/tmp/trm.c",
		"ollama_embed_endpoint":        "http://localhost:11434",
		"ollama_embed_model":           "nomic-embed-text",
		"tool_call_validation_enabled": false,
		"local_model":                  "gemma4:e4b",
		"digest_paths":                 map[string]any{"claude-code": "~/.claude/events.jsonl"},
	}
	result, err := WriteConfigPatch(root, patch, WriteConfigOptions{})
	if err != nil {
		t.Fatalf("WriteConfigPatch: %v", err)
	}
	if !result.Written {
		t.Fatalf("expected written=true, got violations=%+v", result.Violations)
	}
	if !result.RequiresRestart {
		t.Errorf("requires_restart = false; want true (port changed)")
	}
	if result.BackupPath == "" {
		t.Errorf("backup_path empty; expected a .bak file for overwritten kernel.yaml")
	}
	if _, err := os.Stat(result.BackupPath); err != nil {
		t.Errorf("backup file missing: %v", err)
	}

	// Disk reflects the patch.
	kc := readKernelYAML(t, path)
	if kc.Port != 7777 {
		t.Errorf("Port = %d; want 7777", kc.Port)
	}
	if kc.DigestPaths["claude-code"] != "~/.claude/events.jsonl" {
		t.Errorf("DigestPaths[claude-code] = %q; want ~/.claude/events.jsonl", kc.DigestPaths["claude-code"])
	}
	if kc.ToolCallValidation == nil || *kc.ToolCallValidation != false {
		t.Errorf("ToolCallValidation = %v; want *false", kc.ToolCallValidation)
	}
	if len(result.ChangedFields) == 0 {
		t.Errorf("changed_fields empty")
	}
}

// 2. Sparse patch: only port changes; other fields are preserved.
func TestWriteConfigPatch_SparsePatchPreservesOthers(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	path := seedKernelYAML(t, root, "port: 6931\nlocal_model: gemma4:e2b\nconsolidation_interval: 600\n")

	result, err := WriteConfigPatch(root, map[string]any{"port": float64(7000)}, WriteConfigOptions{})
	if err != nil {
		t.Fatalf("WriteConfigPatch: %v", err)
	}
	if !result.Written {
		t.Fatalf("expected written=true, got violations=%+v", result.Violations)
	}
	if len(result.Diff) != 1 || result.Diff[0].Field != "port" {
		t.Errorf("expected single diff on port; got %+v", result.Diff)
	}

	kc := readKernelYAML(t, path)
	if kc.Port != 7000 {
		t.Errorf("Port = %d; want 7000", kc.Port)
	}
	if kc.LocalModel != "gemma4:e2b" {
		t.Errorf("LocalModel = %q; want gemma4:e2b", kc.LocalModel)
	}
	if kc.ConsolidationInterval != 600 {
		t.Errorf("ConsolidationInterval = %d; want 600", kc.ConsolidationInterval)
	}
}

//  3. Reload-safe field: requires_restart is still true in v1 (we always
//     persist + restart), but changed_fields reveals the field is reload-safe.
func TestWriteConfigPatch_ReloadSafeFieldChange(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	seedKernelYAML(t, root, "consolidation_interval: 600\n")

	result, err := WriteConfigPatch(root, map[string]any{"consolidation_interval": float64(900)}, WriteConfigOptions{})
	if err != nil {
		t.Fatalf("WriteConfigPatch: %v", err)
	}
	if !result.Written {
		t.Fatalf("expected written=true")
	}
	// v1 is blanket requires_restart. The reload-safety hint is in changed_fields.
	if !result.RequiresRestart {
		t.Errorf("requires_restart = false; v1 always returns true on a write")
	}
	if got := result.ChangedFields; len(got) != 1 || got[0] != "consolidation_interval" {
		t.Errorf("changed_fields = %v; want [consolidation_interval]", got)
	}
	if !reloadSafeFields[result.ChangedFields[0]] {
		t.Errorf("expected consolidation_interval to be reload-safe in the hint")
	}
}

// 4. Null deletes a key (restoring LoadConfig's default on next boot).
func TestWriteConfigPatch_NullRemovesField(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	path := seedKernelYAML(t, root, "local_model: gemma4:custom\n")

	result, err := WriteConfigPatch(root, map[string]any{"local_model": nil}, WriteConfigOptions{})
	if err != nil {
		t.Fatalf("WriteConfigPatch: %v", err)
	}
	if !result.Written {
		t.Fatalf("expected written=true, got violations=%+v", result.Violations)
	}

	kc := readKernelYAML(t, path)
	if kc.LocalModel != "" {
		t.Errorf("LocalModel = %q; want \"\" after null patch", kc.LocalModel)
	}

	// The effective config falls back to LoadConfig's default.
	cfg := ResolveFromKernelConfig(root, kc)
	if cfg.LocalModel != defaultOllamaModel {
		t.Errorf("effective LocalModel = %q; want default %q", cfg.LocalModel, defaultOllamaModel)
	}
}

// 5. scope=v3 writes into the nested section without touching top-level.
func TestWriteConfigPatch_ScopeV3(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	path := seedKernelYAML(t, root, "port: 5100\nconsolidation_interval: 600\n")

	result, err := WriteConfigPatch(root, map[string]any{"port": float64(6931)}, WriteConfigOptions{Scope: "v3"})
	if err != nil {
		t.Fatalf("WriteConfigPatch: %v", err)
	}
	if !result.Written {
		t.Fatalf("expected written=true, got violations=%+v", result.Violations)
	}

	kc := readKernelYAML(t, path)
	if kc.Port != 5100 {
		t.Errorf("top-level Port = %d; want 5100 (unchanged)", kc.Port)
	}
	if kc.V3.Port != 6931 {
		t.Errorf("v3.Port = %d; want 6931", kc.V3.Port)
	}

	// LoadConfig applies v3 override on top of top-level.
	cfg := ResolveFromKernelConfig(root, kc)
	if cfg.Port != 6931 {
		t.Errorf("effective Port = %d; want 6931 (v3 override)", cfg.Port)
	}
}

// 6. Validation rejects negative heartbeat without writing.
func TestWriteConfigPatch_RejectsNegativeHeartbeat(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	path := seedKernelYAML(t, root, "port: 6931\n")
	originalData, _ := os.ReadFile(path)

	result, err := WriteConfigPatch(root, map[string]any{"heartbeat_interval": float64(-1)}, WriteConfigOptions{})
	if err != nil {
		t.Fatalf("WriteConfigPatch: %v", err)
	}
	if result.Written {
		t.Errorf("written = true on bad patch")
	}
	if len(result.Violations) == 0 {
		t.Errorf("expected violations for negative heartbeat")
	}

	after, _ := os.ReadFile(path)
	if string(originalData) != string(after) {
		t.Errorf("kernel.yaml was mutated despite validation failure")
	}
}

// 7. Validation rejects out-of-range port.
func TestWriteConfigPatch_RejectsOutOfRangePort(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	path := seedKernelYAML(t, root, "port: 6931\n")
	originalData, _ := os.ReadFile(path)

	result, err := WriteConfigPatch(root, map[string]any{"port": float64(70000)}, WriteConfigOptions{})
	if err != nil {
		t.Fatalf("WriteConfigPatch: %v", err)
	}
	if result.Written {
		t.Errorf("written = true for port=70000")
	}
	found := false
	for _, v := range result.Violations {
		if v.Field == "port" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected violation on field=port; got %+v", result.Violations)
	}
	after, _ := os.ReadFile(path)
	if string(originalData) != string(after) {
		t.Errorf("kernel.yaml mutated despite rejection")
	}
}

// 8. dry_run returns diff but does not touch disk / create backup.
func TestWriteConfigPatch_DryRun(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	path := seedKernelYAML(t, root, "port: 6931\n")
	originalData, _ := os.ReadFile(path)

	result, err := WriteConfigPatch(root, map[string]any{"port": float64(7000)}, WriteConfigOptions{DryRun: true})
	if err != nil {
		t.Fatalf("WriteConfigPatch: %v", err)
	}
	if result.Written {
		t.Errorf("written = true under dry_run")
	}
	if !result.DryRun {
		t.Errorf("dry_run flag not echoed back")
	}
	if len(result.Diff) == 0 {
		t.Errorf("expected non-empty diff under dry_run")
	}
	if result.BackupPath != "" {
		t.Errorf("backup created under dry_run: %q", result.BackupPath)
	}
	after, _ := os.ReadFile(path)
	if string(originalData) != string(after) {
		t.Errorf("kernel.yaml mutated under dry_run")
	}
	// No backup files should be present.
	dir := filepath.Dir(path)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), backupPrefix) {
			t.Errorf("found backup %q after dry_run", e.Name())
		}
	}
}

//  9. Atomic write semantics — a successful write never produces a torn file,
//     and the backup is a full copy of the prior content.
func TestWriteConfigPatch_AtomicWrite(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	originalYAML := "port: 6931\nlocal_model: gemma4:e2b\n"
	path := seedKernelYAML(t, root, originalYAML)

	result, err := WriteConfigPatch(root, map[string]any{"port": float64(7000)}, WriteConfigOptions{})
	if err != nil {
		t.Fatalf("WriteConfigPatch: %v", err)
	}
	if !result.Written {
		t.Fatalf("expected written=true")
	}

	// No `.tmp` residue in the directory.
	dir := filepath.Dir(path)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %q", e.Name())
		}
	}

	// Backup is a full copy of the pre-write file.
	if result.BackupPath == "" {
		t.Fatalf("no backup path")
	}
	backupData, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backupData) != originalYAML {
		t.Errorf("backup content = %q; want %q", backupData, originalYAML)
	}

	// Resulting file parses cleanly.
	_ = readKernelYAML(t, path)
}

// 10. Backup rotation: 12 writes → only 10 backups remain.
func TestWriteConfigPatch_BackupRotation(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	path := seedKernelYAML(t, root, "port: 6931\n")

	for i := 0; i < 12; i++ {
		_, err := WriteConfigPatch(root, map[string]any{"port": float64(6000 + i)}, WriteConfigOptions{})
		if err != nil {
			t.Fatalf("WriteConfigPatch iteration %d: %v", i, err)
		}
	}

	dir := filepath.Dir(path)
	entries, _ := os.ReadDir(dir)
	count := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), backupPrefix) {
			count++
		}
	}
	if count > backupKeep {
		t.Errorf("backup count = %d; want <= %d (backupKeep)", count, backupKeep)
	}
	if count < backupKeep {
		t.Errorf("backup count = %d; want exactly %d after 12 writes", count, backupKeep)
	}
}

//  11. Corrupt existing file is refused — the write path refuses to clobber
//     unparseable YAML without a rollback first.
func TestWriteConfigPatch_RefusesCorruptFile(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	path := seedKernelYAML(t, root, "port: 6931\n:::not yaml")
	originalData, _ := os.ReadFile(path)

	result, err := WriteConfigPatch(root, map[string]any{"port": float64(7000)}, WriteConfigOptions{})
	if err != nil {
		t.Fatalf("WriteConfigPatch: %v", err)
	}
	if result.Written {
		t.Errorf("written = true despite corrupt existing file")
	}
	found := false
	for _, v := range result.Violations {
		if v.Rule == "existing_file_unparseable" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected existing_file_unparseable violation; got %+v", result.Violations)
	}
	after, _ := os.ReadFile(path)
	if string(originalData) != string(after) {
		t.Errorf("kernel.yaml mutated despite refused write")
	}
}

//  12. Concurrent writes serialize — 8 goroutines all set port to different
//     values; no corruption and the final file is a valid YAML with one of
//     the candidate ports.
func TestWriteConfigPatch_ConcurrentWritesSerialize(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	path := seedKernelYAML(t, root, "port: 6931\n")

	const N = 8
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := WriteConfigPatch(root, map[string]any{"port": float64(6000 + idx)}, WriteConfigOptions{})
			errs[idx] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// File parses and reports one of the candidate ports.
	kc := readKernelYAML(t, path)
	if kc.Port < 6000 || kc.Port >= 6000+N {
		t.Errorf("final Port = %d; want one of [6000..%d)", kc.Port, 6000+N)
	}
}

// 13. ReadConfigSnapshot includes defaults and path.
func TestReadConfigSnapshot_IncludesDefaults(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	seedKernelYAML(t, root, "port: 6931\n")
	snap, err := ReadConfigSnapshot(root, true, true)
	if err != nil {
		t.Fatalf("ReadConfigSnapshot: %v", err)
	}
	if !snap.Exists {
		t.Errorf("exists = false on present kernel.yaml")
	}
	if snap.Defaults["port"].(int) != 6931 {
		t.Errorf("defaults.port = %v; want 6931", snap.Defaults["port"])
	}
	if snap.RawYAML == "" {
		t.Errorf("raw_yaml empty when include_raw_yaml=true")
	}
}

// 14. Rollback roundtrip: write → write → rollback restores the prior file.
func TestRollbackConfig_RestoresPriorBackup(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	path := seedKernelYAML(t, root, "port: 6931\nlocal_model: gemma4:a\n")

	// First write — produces bak for the seed.
	_, err := WriteConfigPatch(root, map[string]any{"local_model": "gemma4:b"}, WriteConfigOptions{})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	// Second write.
	_, err = WriteConfigPatch(root, map[string]any{"local_model": "gemma4:c"}, WriteConfigOptions{})
	if err != nil {
		t.Fatalf("second write: %v", err)
	}

	// Rollback to the most recent backup — which is the one made by the
	// _second_ write, i.e. local_model: gemma4:b.
	res, err := RollbackConfig(root, RollbackOptions{})
	if err != nil {
		t.Fatalf("RollbackConfig: %v", err)
	}
	if !res.Restored {
		t.Fatalf("restored = false")
	}
	kc := readKernelYAML(t, path)
	if kc.LocalModel != "gemma4:b" {
		t.Errorf("LocalModel after rollback = %q; want gemma4:b", kc.LocalModel)
	}
}

//  15. In-place patch preserves comments and does NOT emit zeroed absent fields
//     or a spurious top-level v3: block — the three clobber modes from issue #342.
func TestWriteConfigPatch_PreservesCommentsNoZeroNoV3(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	fixture := "# workspace kernel config\n# managed by hand\nollama_embed_model: bge-m3:latest\n"
	path := seedKernelYAML(t, root, fixture)

	result, err := WriteConfigPatch(root, map[string]any{"local_model": "gemma4:e4b"}, WriteConfigOptions{})
	if err != nil {
		t.Fatalf("WriteConfigPatch: %v", err)
	}
	if !result.Written {
		t.Fatalf("expected written=true, got violations=%+v", result.Violations)
	}

	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read result: %v", rerr)
	}
	out := string(raw)

	// (a) Original comment lines survive.
	if !strings.Contains(out, "# workspace kernel config") {
		t.Errorf("comment '# workspace kernel config' not found in output:\n%s", out)
	}
	if !strings.Contains(out, "# managed by hand") {
		t.Errorf("comment '# managed by hand' not found in output:\n%s", out)
	}

	// (b) No zeroed port field.
	if strings.Contains(out, "port: 0") {
		t.Errorf("spurious 'port: 0' found in output:\n%s", out)
	}

	// (c) No spurious top-level v3: block.
	if strings.Contains(out, "\nv3:") || strings.HasPrefix(out, "v3:") {
		t.Errorf("spurious 'v3:' block found in output:\n%s", out)
	}

	// (d) Original field preserved.
	if !strings.Contains(out, "ollama_embed_model: bge-m3:latest") {
		t.Errorf("ollama_embed_model not preserved in output:\n%s", out)
	}

	// (e) Patched field present.
	if !strings.Contains(out, "local_model: gemma4:e4b") {
		t.Errorf("local_model not set in output:\n%s", out)
	}
}

//  16. v3-scope patch lands under v3: without zeroing top-level keys and
//     without an empty v3: block being emitted before the patch.
func TestWriteConfigPatch_V3ScopeNodePatch(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	// File has only a top-level key and NO v3: section.
	fixture := "# top comment\nollama_embed_model: bge-m3:latest\n"
	path := seedKernelYAML(t, root, fixture)

	result, err := WriteConfigPatch(root, map[string]any{"local_model": "gemma4:26b"}, WriteConfigOptions{Scope: "v3"})
	if err != nil {
		t.Fatalf("WriteConfigPatch v3: %v", err)
	}
	if !result.Written {
		t.Fatalf("expected written=true, got violations=%+v", result.Violations)
	}

	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read result: %v", rerr)
	}
	out := string(raw)

	// Top-level comment preserved.
	if !strings.Contains(out, "# top comment") {
		t.Errorf("comment '# top comment' not found in output:\n%s", out)
	}

	// Top-level field preserved, not zeroed.
	if !strings.Contains(out, "ollama_embed_model: bge-m3:latest") {
		t.Errorf("ollama_embed_model not preserved in output:\n%s", out)
	}

	// v3: block created and contains the patched field.
	if !strings.Contains(out, "v3:") {
		t.Errorf("v3: section not found in output:\n%s", out)
	}
	if !strings.Contains(out, "local_model: gemma4:26b") {
		t.Errorf("local_model not found under v3: in output:\n%s", out)
	}

	// No zeroed top-level port.
	if strings.Contains(out, "port: 0") {
		t.Errorf("spurious 'port: 0' found in output:\n%s", out)
	}

	// The v3 section should contain local_model; verify via struct parse.
	kc := readKernelYAML(t, path)
	if kc.V3.LocalModel != "gemma4:26b" {
		t.Errorf("V3.LocalModel = %q; want gemma4:26b", kc.V3.LocalModel)
	}
	// Top-level OllamaEmbedModel still present.
	if kc.OllamaEmbedModel != "bge-m3:latest" {
		t.Errorf("OllamaEmbedModel = %q; want bge-m3:latest (top-level, unchanged)", kc.OllamaEmbedModel)
	}
}

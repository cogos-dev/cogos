// Package skills — unit tests for discovery, tier classification,
// frontmatter parsing, and execution.
package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/myrgic/cogos/pkg/skills"
)

// ─── Fixtures ────────────────────────────────────────────────────────────────

func makeSkillDir(t *testing.T, root, name, skillMD string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return dir
}

func makeExecScript(t *testing.T, dir, relPath, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("exec skill tests require Unix")
	}
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatalf("mkdir exec dir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0755); err != nil {
		t.Fatalf("write exec script: %v", err)
	}
	return full
}

// ─── ParseFrontmatter ────────────────────────────────────────────────────────

func TestParseFrontmatter_NoYAML(t *testing.T) {
	data := []byte("# My Prose Skill\n\nThis skill only has documentation.\n")
	fm, title, desc := skills.ParseFrontmatter(data)

	if fm.Name != "" {
		t.Errorf("want empty name, got %q", fm.Name)
	}
	if title != "My Prose Skill" {
		t.Errorf("want title 'My Prose Skill', got %q", title)
	}
	if desc != "This skill only has documentation." {
		t.Errorf("want desc 'This skill only has documentation.', got %q", desc)
	}
}

func TestParseFrontmatter_WithYAML(t *testing.T) {
	data := []byte("---\nname: my-skill\nversion: 0.2.0\ndescription: Does things\nexec: bin/run\n---\n# My Skill\n")
	fm, _, _ := skills.ParseFrontmatter(data)

	if fm.Name != "my-skill" {
		t.Errorf("want name 'my-skill', got %q", fm.Name)
	}
	if fm.Version != "0.2.0" {
		t.Errorf("want version '0.2.0', got %q", fm.Version)
	}
	if fm.Exec != "bin/run" {
		t.Errorf("want exec 'bin/run', got %q", fm.Exec)
	}
}

func TestParseFrontmatter_DescTruncated(t *testing.T) {
	long := strings.Repeat("x", 250)
	data := []byte("# Title\n\n" + long + "\n")
	_, _, desc := skills.ParseFrontmatter(data)

	if len(desc) > 200 {
		t.Errorf("want desc truncated to 200, got len %d", len(desc))
	}
	if !strings.HasSuffix(desc, "...") {
		t.Errorf("want desc to end with '...', got %q", desc[len(desc)-5:])
	}
}

// ─── InferTier ───────────────────────────────────────────────────────────────

func TestInferTier_Tier0(t *testing.T) {
	dir := t.TempDir()
	fm := skills.Frontmatter{}
	if got := skills.InferTier(dir, fm); got != 0 {
		t.Errorf("want tier 0, got %d", got)
	}
}

func TestInferTier_Tier1_ShScript(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "helper.sh"), []byte("#!/bin/sh\n"), 0755)
	fm := skills.Frontmatter{}
	if got := skills.InferTier(dir, fm); got != 1 {
		t.Errorf("want tier 1, got %d", got)
	}
}

func TestInferTier_Tier1_BinDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0755)
	os.WriteFile(filepath.Join(dir, "bin", "helper"), []byte("#!/bin/sh\n"), 0755)
	fm := skills.Frontmatter{} // no exec declared
	if got := skills.InferTier(dir, fm); got != 1 {
		t.Errorf("want tier 1, got %d", got)
	}
}

func TestInferTier_Tier2(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0755)
	os.WriteFile(filepath.Join(dir, "bin", "run"), []byte("#!/bin/sh\necho hi\n"), 0755)
	fm := skills.Frontmatter{Exec: "bin/run"}
	if got := skills.InferTier(dir, fm); got != 2 {
		t.Errorf("want tier 2, got %d", got)
	}
}

func TestInferTier_Tier3(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0755)
	os.WriteFile(filepath.Join(dir, "bin", "run"), []byte("#!/bin/sh\necho hi\n"), 0755)
	fm := skills.Frontmatter{Exec: "bin/run", Schema: "schema.json"}
	if got := skills.InferTier(dir, fm); got != 3 {
		t.Errorf("want tier 3, got %d", got)
	}
}

// ─── LoadRecord ──────────────────────────────────────────────────────────────

func TestLoadRecord_Tier0_ProseOnly(t *testing.T) {
	root := t.TempDir()
	md := "# My Prose Skill\n\nThis skill only has documentation.\n"
	dir := makeSkillDir(t, root, "prose-skill", md)

	rec, err := skills.LoadRecord("prose-skill", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Tier != 0 {
		t.Errorf("want tier 0, got %d", rec.Tier)
	}
	if rec.Name != "prose-skill" {
		t.Errorf("want name prose-skill, got %q", rec.Name)
	}
	if rec.Exec != nil {
		t.Errorf("want exec nil, got %v", rec.Exec)
	}
	if rec.Schema != nil {
		t.Errorf("want schema nil, got %v", rec.Schema)
	}
	if len(rec.Requires) != 0 {
		t.Errorf("want empty requires, got %v", rec.Requires)
	}
}

func TestLoadRecord_Tier1_Scripts(t *testing.T) {
	root := t.TempDir()
	md := "# Helper Skill\n\nHas scripts but no exec declared.\n"
	dir := makeSkillDir(t, root, "helper-skill", md)
	os.WriteFile(filepath.Join(dir, "helper.sh"), []byte("#!/bin/sh\necho hi\n"), 0755)

	rec, err := skills.LoadRecord("helper-skill", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Tier != 1 {
		t.Errorf("want tier 1, got %d", rec.Tier)
	}
}

func TestLoadRecord_Tier2_WithBin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tier-2 exec test requires Unix")
	}
	root := t.TempDir()
	md := "---\nname: exec-skill\nversion: 0.2.0\ndescription: Has a canonical exec\nexec: bin/run\nrequires:\n  - cog_read_cogdoc\n---\n# Exec Skill\n"
	dir := makeSkillDir(t, root, "exec-skill", md)
	makeExecScript(t, dir, "bin/run", "#!/bin/sh\necho hello\n")

	rec, err := skills.LoadRecord("exec-skill", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Tier != 2 {
		t.Errorf("want tier 2, got %d", rec.Tier)
	}
	if rec.Name != "exec-skill" {
		t.Errorf("want name exec-skill, got %q", rec.Name)
	}
	if rec.Version != "0.2.0" {
		t.Errorf("want version 0.2.0, got %q", rec.Version)
	}
	if rec.Exec == nil || *rec.Exec != "bin/run" {
		t.Errorf("want exec bin/run, got %v", rec.Exec)
	}
	if len(rec.Requires) != 1 || rec.Requires[0] != "cog_read_cogdoc" {
		t.Errorf("want requires [cog_read_cogdoc], got %v", rec.Requires)
	}
}

func TestLoadRecord_Tier3_WithSchema(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tier-3 exec test requires Unix")
	}
	root := t.TempDir()
	md := "---\nname: typed-skill\nversion: 0.3.0\ndescription: Typed I/O\nexec: bin/typed\nschema: lib/input.schema.json\n---\n# Typed Skill\n"
	dir := makeSkillDir(t, root, "typed-skill", md)
	makeExecScript(t, dir, "bin/typed", "#!/bin/sh\necho '{}'\n")

	rec, err := skills.LoadRecord("typed-skill", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Tier != 3 {
		t.Errorf("want tier 3, got %d", rec.Tier)
	}
	if rec.Schema == nil || *rec.Schema != "lib/input.schema.json" {
		t.Errorf("want schema lib/input.schema.json, got %v", rec.Schema)
	}
}

func TestLoadRecord_MissingSKILLMD(t *testing.T) {
	dir := t.TempDir()
	_, err := skills.LoadRecord("no-skill", dir)
	if err == nil {
		t.Fatal("want error for missing SKILL.md, got nil")
	}
}

// ─── Discover ────────────────────────────────────────────────────────────────

func TestDiscover_Empty(t *testing.T) {
	dir := t.TempDir()
	recs, err := skills.Discover([]string{dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("want 0 skills, got %d", len(recs))
	}
}

func TestDiscover_NonExistentDirSkipped(t *testing.T) {
	recs, err := skills.Discover([]string{"/does/not/exist/at/all"})
	if err != nil {
		t.Fatalf("want non-existent dir skipped, got error: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("want 0 skills, got %d", len(recs))
	}
}

func TestDiscover_MultipleSkills(t *testing.T) {
	root := t.TempDir()
	makeSkillDir(t, root, "skill-a", "---\nname: skill-a\nversion: 1.0.0\ndescription: Alpha\n---\n# Alpha\n")
	makeSkillDir(t, root, "skill-b", "# Beta\n\nA prose-only skill.\n")

	recs, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 skills, got %d", len(recs))
	}
	byName := make(map[string]skills.Record)
	for _, r := range recs {
		byName[r.Name] = r
	}
	if _, ok := byName["skill-a"]; !ok {
		t.Error("skill-a not found")
	}
	if _, ok := byName["skill-b"]; !ok {
		t.Error("skill-b not found")
	}
}

func TestDiscover_WorkspaceOverridesUser(t *testing.T) {
	userDir := t.TempDir()
	wsDir := t.TempDir()

	// Same name in both; workspace (wsDir) should win because it's last.
	makeSkillDir(t, userDir, "my-skill", "---\nname: my-skill\nversion: 1.0.0\ndescription: user version\n---\n")
	makeSkillDir(t, wsDir, "my-skill", "---\nname: my-skill\nversion: 2.0.0\ndescription: workspace version\n---\n")

	recs, err := skills.Discover([]string{userDir, wsDir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 deduped skill, got %d", len(recs))
	}
	if recs[0].Version != "2.0.0" {
		t.Errorf("want workspace version 2.0.0, got %q", recs[0].Version)
	}
}

// ─── FindByName ──────────────────────────────────────────────────────────────

func TestFindByName_Found(t *testing.T) {
	root := t.TempDir()
	makeSkillDir(t, root, "target", "---\nname: target\nversion: 1.0.0\ndescription: Target skill\n---\n")

	rec, err := skills.FindByName("target", []string{root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Name != "target" {
		t.Errorf("want name 'target', got %q", rec.Name)
	}
}

func TestFindByName_NotFound(t *testing.T) {
	root := t.TempDir()
	_, err := skills.FindByName("missing", []string{root})
	if err == nil {
		t.Fatal("want error for missing skill, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("want 'not found' in error, got: %v", err)
	}
}

// ─── Exec ────────────────────────────────────────────────────────────────────

func TestExec_Tier0_Rejected(t *testing.T) {
	root := t.TempDir()
	dir := makeSkillDir(t, root, "prose", "# Prose skill\n")
	skill, _ := skills.LoadRecord("prose", dir)

	_, err := skills.Exec(context.Background(), skill, "", "", "")
	if err == nil {
		t.Fatal("want error for tier-0 exec, got nil")
	}
	if !strings.Contains(err.Error(), "not_executable") {
		t.Errorf("want not_executable error, got: %v", err)
	}
}

func TestExec_Tier1_Rejected(t *testing.T) {
	root := t.TempDir()
	dir := makeSkillDir(t, root, "scripts", "# Scripts skill\n")
	os.WriteFile(filepath.Join(dir, "helper.sh"), []byte("#!/bin/sh\necho hi\n"), 0755)
	skill, _ := skills.LoadRecord("scripts", dir)

	_, err := skills.Exec(context.Background(), skill, "", "", "")
	if err == nil {
		t.Fatal("want error for tier-1 exec, got nil")
	}
	if !strings.Contains(err.Error(), "no_canonical_entry_point") {
		t.Errorf("want no_canonical_entry_point error, got: %v", err)
	}
}

func TestExec_Tier2_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec test requires Unix")
	}
	root := t.TempDir()
	md := "---\nname: greet\nversion: 1.0.0\ndescription: Greet\nexec: bin/greet\n---\n"
	dir := makeSkillDir(t, root, "greet", md)
	makeExecScript(t, dir, "bin/greet", "#!/bin/sh\necho 'hello from skill'\n")
	skill, _ := skills.LoadRecord("greet", dir)

	result, err := skills.Exec(context.Background(), skill, "", "", "")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("want exit 0, got %d (stderr: %s)", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "hello from skill") {
		t.Errorf("want stdout to contain 'hello from skill', got: %q", result.Stdout)
	}
	if result.DurationMS < 0 {
		t.Errorf("want non-negative duration, got %d", result.DurationMS)
	}
}

func TestExec_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec test requires Unix")
	}
	root := t.TempDir()
	md := "---\nname: fail-skill\nversion: 1.0.0\nexec: bin/fail\n---\n"
	dir := makeSkillDir(t, root, "fail-skill", md)
	makeExecScript(t, dir, "bin/fail", "#!/bin/sh\necho 'some error' >&2\nexit 42\n")
	skill, _ := skills.LoadRecord("fail-skill", dir)

	result, err := skills.Exec(context.Background(), skill, "", "", "")
	if err != nil {
		t.Fatalf("Exec should not return error on non-zero exit: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("want exit 42, got %d", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "some error") {
		t.Errorf("want stderr to contain 'some error', got: %q", result.Stderr)
	}
}

func TestExec_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec test requires Unix")
	}
	root := t.TempDir()
	md := "---\nname: slow-skill\nversion: 1.0.0\nexec: bin/slow\n---\n"
	dir := makeSkillDir(t, root, "slow-skill", md)
	makeExecScript(t, dir, "bin/slow", "#!/bin/sh\nsleep 10\n")
	skill, _ := skills.LoadRecord("slow-skill", dir)

	result, err := skills.Exec(context.Background(), skill, "", "100ms", "")
	if err != nil {
		t.Fatalf("Exec should not return error on timeout: %v", err)
	}
	if result.ExitCode != 124 {
		t.Errorf("want exit 124 on timeout, got %d", result.ExitCode)
	}
	if !strings.Contains(result.Error, "timeout") {
		t.Errorf("want error to mention timeout, got: %q", result.Error)
	}
}

func TestExec_StdinInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec test requires Unix")
	}
	root := t.TempDir()
	md := "---\nname: echo-skill\nversion: 1.0.0\nexec: bin/echo-in\n---\n"
	dir := makeSkillDir(t, root, "echo-skill", md)
	makeExecScript(t, dir, "bin/echo-in", "#!/bin/sh\ncat\n")
	skill, _ := skills.LoadRecord("echo-skill", dir)

	result, err := skills.Exec(context.Background(), skill, `{"key":"value"}`, "", "")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("want exit 0, got %d", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, `"key":"value"`) {
		t.Errorf("want stdin echoed, got: %q", result.Stdout)
	}
}

func TestExec_WorkspaceEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec test requires Unix")
	}
	root := t.TempDir()
	md := "---\nname: env-skill\nversion: 1.0.0\nexec: bin/printenv\n---\n"
	dir := makeSkillDir(t, root, "env-skill", md)
	makeExecScript(t, dir, "bin/printenv", "#!/bin/sh\necho \"WS=$COGOS_WORKSPACE\"\n")
	skill, _ := skills.LoadRecord("env-skill", dir)

	wsRoot := "/tmp/fake-workspace"
	result, err := skills.Exec(context.Background(), skill, "", "", wsRoot)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Stdout, "WS="+wsRoot) {
		t.Errorf("want COGOS_WORKSPACE in env, got stdout: %q", result.Stdout)
	}
}

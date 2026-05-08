// cmd_skill_test.go — Smoke tests for the `cog skill` CLI wrappers.
//
// The heavy lifting (discovery, tier classification, exec) is tested in
// pkg/skills/skills_test.go. These tests verify that the CLI-level wrappers
// (DiscoverSkills, FindSkill, execSkill) wire through correctly.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/myrgic/cogos/pkg/skills"
)

// ─── Fixtures ───────────────────────────────────────────────────────────────────

// makeSkillDir creates a minimal skill directory under root/.claude/skills/<name>.
// Returns the skill directory path.
func makeSkillDir(t *testing.T, root, name, skillMD string) string {
	t.Helper()
	dir := filepath.Join(root, ".claude", "skills", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return dir
}

// makeExecScript writes a small executable script at <dir>/<relPath> and sets
// the executable bit. On Windows the helper is skipped (exec tests require Unix).
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

// ─── skills.LoadRecord (via pkg/skills) ─────────────────────────────────────

func TestLoadSkillRecord_Tier0_PraseOnly(t *testing.T) {
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

func TestLoadSkillRecord_Tier1_Scripts(t *testing.T) {
	root := t.TempDir()
	md := "# Helper Skill\n\nHas scripts but no exec declared.\n"
	dir := makeSkillDir(t, root, "helper-skill", md)

	// Add a .sh file to make it tier-1
	if err := os.WriteFile(filepath.Join(dir, "helper.sh"), []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatalf("write helper.sh: %v", err)
	}

	rec, err := skills.LoadRecord("helper-skill", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rec.Tier != 1 {
		t.Errorf("want tier 1, got %d", rec.Tier)
	}
}

func TestLoadSkillRecord_Tier2_WithBin(t *testing.T) {
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

func TestLoadSkillRecord_Tier3_WithSchema(t *testing.T) {
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

// ─── DiscoverSkills ─────────────────────────────────────────────────────────

func TestDiscoverSkills_Empty(t *testing.T) {
	// Override skillDirs to point at a temp directory with no skills.
	origHome := os.Getenv("HOME")
	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// No workspace — ResolveWorkspace will fail, but DiscoverSkills handles that.
	skillList, err := DiscoverSkills()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skillList) != 0 {
		t.Errorf("want 0 skills in empty dir, got %d", len(skillList))
	}
}

func TestDiscoverSkills_MultipleSkills(t *testing.T) {
	tmpHome := t.TempDir()

	makeSkillDir(t, tmpHome, "skill-a",
		"---\nname: skill-a\nversion: 1.0.0\ndescription: Alpha\n---\n# Alpha\n")
	makeSkillDir(t, tmpHome, "skill-b",
		"# Beta\n\nA prose-only skill.\n")

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	skillList, err := DiscoverSkills()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(skillList) != 2 {
		t.Fatalf("want 2 skills, got %d", len(skillList))
	}

	byName := make(map[string]SkillRecord)
	for _, s := range skillList {
		byName[s.Name] = s
	}

	if _, ok := byName["skill-a"]; !ok {
		t.Error("skill-a not found")
	}
	if _, ok := byName["skill-b"]; !ok {
		t.Error("skill-b not found")
	}
}

// ─── JSON output shape ──────────────────────────────────────────────────────

func TestSkillRecord_JSONShape(t *testing.T) {
	// Verify the JSON output has the contract fields the issue requires.
	rec := SkillRecord{
		Name:        "pull-context-dispatch",
		Version:     "0.1.0",
		Tier:        0,
		Description: "Pass identity, directive, tool access, substrate pointers",
		Exec:        nil,
		Schema:      nil,
		Path:        "~/.claude/skills/pull-context-dispatch",
		Requires:    []string{},
	}

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, field := range []string{"name", "version", "tier", "description", "exec", "schema", "path", "requires"} {
		if _, ok := m[field]; !ok {
			t.Errorf("JSON missing required field %q", field)
		}
	}

	if m["exec"] != nil {
		t.Errorf("exec should be null for tier-0, got %v", m["exec"])
	}
}

// ─── execSkill (CLI wrapper) ─────────────────────────────────────────────────

func TestExecSkill_Tier0_Rejected(t *testing.T) {
	root := t.TempDir()
	dir := makeSkillDir(t, root, "prose", "# Prose skill\n")
	skill, err := skills.LoadRecord("prose", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	_, err = execSkill(skill, "", "")
	if err == nil {
		t.Fatal("want error for tier-0 exec, got nil")
	}
	if !strings.Contains(err.Error(), "not_executable") {
		t.Errorf("want not_executable error, got: %v", err)
	}
}

func TestExecSkill_Tier1_Rejected(t *testing.T) {
	root := t.TempDir()
	dir := makeSkillDir(t, root, "scripts", "# Scripts skill\n")
	// Add a .sh to make it tier-1
	os.WriteFile(filepath.Join(dir, "helper.sh"), []byte("#!/bin/sh\necho hi\n"), 0755)

	skill, err := skills.LoadRecord("scripts", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	_, err = execSkill(skill, "", "")
	if err == nil {
		t.Fatal("want error for tier-1 exec, got nil")
	}
	if !strings.Contains(err.Error(), "no_canonical_entry_point") {
		t.Errorf("want no_canonical_entry_point error, got: %v", err)
	}
}

func TestExecSkill_Tier2_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec test requires Unix")
	}
	root := t.TempDir()
	md := "---\nname: greet\nversion: 1.0.0\ndescription: Greet\nexec: bin/greet\n---\n"
	dir := makeSkillDir(t, root, "greet", md)
	makeExecScript(t, dir, "bin/greet", "#!/bin/sh\necho 'hello from skill'\n")

	skill, err := skills.LoadRecord("greet", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	result, err := execSkill(skill, "", "")
	if err != nil {
		t.Fatalf("execSkill: %v", err)
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

func TestExecSkill_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec test requires Unix")
	}
	root := t.TempDir()
	md := "---\nname: fail-skill\nversion: 1.0.0\nexec: bin/fail\n---\n"
	dir := makeSkillDir(t, root, "fail-skill", md)
	makeExecScript(t, dir, "bin/fail", "#!/bin/sh\necho 'some error' >&2\nexit 42\n")

	skill, err := skills.LoadRecord("fail-skill", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	result, err := execSkill(skill, "", "")
	if err != nil {
		t.Fatalf("execSkill should not return error on non-zero exit: %v", err)
	}

	if result.ExitCode != 42 {
		t.Errorf("want exit 42, got %d", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "some error") {
		t.Errorf("want stderr to contain 'some error', got: %q", result.Stderr)
	}
}

func TestExecSkill_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec test requires Unix")
	}
	root := t.TempDir()
	md := "---\nname: slow-skill\nversion: 1.0.0\nexec: bin/slow\n---\n"
	dir := makeSkillDir(t, root, "slow-skill", md)
	makeExecScript(t, dir, "bin/slow", "#!/bin/sh\nsleep 10\n")

	skill, err := skills.LoadRecord("slow-skill", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	result, err := execSkill(skill, "", "100ms")
	if err != nil {
		t.Fatalf("execSkill should not return error on timeout: %v", err)
	}

	if result.ExitCode != 124 {
		t.Errorf("want exit 124 on timeout, got %d", result.ExitCode)
	}
	if !strings.Contains(result.Error, "timeout") {
		t.Errorf("want error to mention timeout, got: %q", result.Error)
	}
}

func TestExecSkill_StdinInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec test requires Unix")
	}
	root := t.TempDir()
	md := "---\nname: echo-skill\nversion: 1.0.0\nexec: bin/echo-in\n---\n"
	dir := makeSkillDir(t, root, "echo-skill", md)
	// Script reads stdin and echoes it
	makeExecScript(t, dir, "bin/echo-in", "#!/bin/sh\ncat\n")

	skill, err := skills.LoadRecord("echo-skill", dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	result, err := execSkill(skill, `{"key":"value"}`, "")
	if err != nil {
		t.Fatalf("execSkill: %v", err)
	}

	if result.ExitCode != 0 {
		t.Errorf("want exit 0, got %d", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, `"key":"value"`) {
		t.Errorf("want stdin echoed, got: %q", result.Stdout)
	}
}

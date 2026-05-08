// cmd_skill.go — CLI commands for the CogOS skill system (issues #96, #97).
//
// Commands:
//   cog skill list [--json]    — Enumerate available skills from ~/.claude/skills/ and <workspace>/.claude/skills/
//   cog skill exec <name> [--input <json>] [--timeout <duration>]  — Invoke a tier-2/3 skill
//   cog skill help             — Show usage

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/myrgic/cogos/pkg/skills"
)

// ─── Re-exported types ───────────────────────────────────────────────────────

// SkillRecord is the JSON schema returned by `cog skill list --json` and
// GET /v1/skills. Aliased from pkg/skills so existing callers compile unchanged.
type SkillRecord = skills.Record

// SkillExecResult is the structured response from `cog skill exec`.
// Aliased from pkg/skills so existing callers compile unchanged.
type SkillExecResult = skills.ExecResult

// ─── Directory resolution ────────────────────────────────────────────────────

// skillDirs returns the ordered list of directories to scan for skills.
// User-level (~/.claude/skills/) is searched first; workspace-level
// (<workspace>/.claude/skills/) overlays it (workspace skills win on same name).
func skillDirs() []string {
	var dirs []string

	// User-level
	home, err := os.UserHomeDir()
	if err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude", "skills"))
	}

	// Workspace-level (project-scoped; may not exist)
	if root, _, err := ResolveWorkspace(); err == nil {
		dirs = append(dirs, filepath.Join(root, ".claude", "skills"))
	}

	return dirs
}

// ─── Public API (thin wrappers over pkg/skills) ──────────────────────────────

// DiscoverSkills walks skill directories and returns a deduplicated list of
// SkillRecords. Workspace-level skills override user-level skills with the
// same name (last directory wins).
func DiscoverSkills() ([]SkillRecord, error) {
	return skills.Discover(skillDirs())
}

// FindSkill looks up a single skill by name; returns an error when not found.
func FindSkill(name string) (*SkillRecord, error) {
	return skills.FindByName(name, skillDirs())
}

// ─── Dispatcher ─────────────────────────────────────────────────────────────────

func cmdSkill(args []string) error {
	if len(args) == 0 {
		return cmdSkillHelp()
	}

	switch args[0] {
	case "list":
		return cmdSkillList(args[1:])
	case "exec":
		return cmdSkillExec(args[1:])
	case "help", "-h", "--help":
		return cmdSkillHelp()
	default:
		return fmt.Errorf("unknown skill command: %s (try: cog skill help)", args[0])
	}
}

// ─── list ───────────────────────────────────────────────────────────────────────

func cmdSkillList(args []string) error {
	jsonMode := false
	for _, arg := range args {
		if arg == "--json" || arg == "-j" {
			jsonMode = true
		}
	}

	skillList, err := DiscoverSkills()
	if err != nil {
		return fmt.Errorf("discover skills: %w", err)
	}

	if jsonMode {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(skillList)
	}

	// Human-readable table
	if len(skillList) == 0 {
		fmt.Println("No skills found.")
		fmt.Println()
		fmt.Println("Add a skill directory with SKILL.md to:")
		home, _ := os.UserHomeDir()
		fmt.Printf("  %s/.claude/skills/<name>/SKILL.md\n", home)
		return nil
	}

	fmt.Printf("%-30s %-8s %s\n", "NAME", "TIER", "DESCRIPTION")
	fmt.Println(strings.Repeat("-", 80))
	for _, s := range skillList {
		desc := s.Description
		if len(desc) > 40 {
			desc = desc[:37] + "..."
		}
		fmt.Printf("%-30s tier-%-3d %s\n", s.Name, s.Tier, desc)
	}
	fmt.Printf("\n%d skill(s) found. Use --json for full records.\n", len(skillList))
	return nil
}

// ─── exec ───────────────────────────────────────────────────────────────────────

func cmdSkillExec(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog skill exec <name> [--input <json>] [--timeout <duration>]")
	}

	skillName := args[0]
	inputJSON := ""
	timeoutStr := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--input", "-i":
			if i+1 >= len(args) {
				return fmt.Errorf("--input requires a value")
			}
			i++
			inputJSON = args[i]
		case "--timeout", "-t":
			if i+1 >= len(args) {
				return fmt.Errorf("--timeout requires a value")
			}
			i++
			timeoutStr = args[i]
		default:
			if strings.HasPrefix(args[i], "--input=") {
				inputJSON = strings.TrimPrefix(args[i], "--input=")
			} else if strings.HasPrefix(args[i], "--timeout=") {
				timeoutStr = strings.TrimPrefix(args[i], "--timeout=")
			}
		}
	}

	skill, err := FindSkill(skillName)
	if err != nil {
		// Return the same error shape whether not-found or role-gated.
		return fmt.Errorf("skill not found or not available: %s", skillName)
	}

	result, err := execSkill(skill, inputJSON, timeoutStr)
	if err != nil {
		return err
	}

	// Print result as JSON
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(result); encErr != nil {
		return encErr
	}

	if result.ExitCode != 0 {
		os.Exit(result.ExitCode)
	}
	return nil
}

// execSkill is the CLI exec entry point. It resolves the workspace root for
// COGOS_WORKSPACE injection, delegates to pkg/skills.Exec, then emits a
// skill.exec bus event.
func execSkill(skill *SkillRecord, inputJSON, timeoutStr string) (*SkillExecResult, error) {
	wsRoot := ""
	if root, _, err := ResolveWorkspace(); err == nil {
		wsRoot = root
	}

	result, err := skills.Exec(context.Background(), skill, inputJSON, timeoutStr, wsRoot)
	if err != nil {
		return nil, err
	}

	// Emit skill.exec bus event (best-effort; don't fail the exec if the bus is unavailable)
	emitSkillExecEvent(skill, result)

	return result, nil
}

// emitSkillExecEvent sends a skill.exec event to the kernel bus. Non-fatal on error.
func emitSkillExecEvent(skill *SkillRecord, result *SkillExecResult) {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return
	}

	mgr := newBusSessionManager(root)
	payload := map[string]interface{}{
		"name":        skill.Name,
		"version":     skill.Version,
		"exit_code":   result.ExitCode,
		"duration_ms": result.DurationMS,
	}
	if result.Error != "" {
		payload["error"] = result.Error
	}

	_, _ = mgr.appendBusEvent("skill", "skill.exec", "cog:skill:exec", payload)
}

// ─── help ───────────────────────────────────────────────────────────────────────

func cmdSkillHelp() error {
	fmt.Println("Usage: cog skill <command>")
	fmt.Println()
	fmt.Println("Skill Discovery and Execution:")
	fmt.Println("  list [--json]                   List available skills (--json for machine-readable output)")
	fmt.Println("  exec <name> [flags]             Execute a tier-2/3 skill by name")
	fmt.Println()
	fmt.Println("Exec Flags:")
	fmt.Println("  --input, -i <json>              Pass JSON payload to skill via stdin")
	fmt.Println("  --timeout, -t <duration>        Override default 5m exec timeout (e.g. 30s, 2m)")
	fmt.Println()
	fmt.Println("Tier Conventions:")
	fmt.Println("  Tier 0  Prose-only SKILL.md; read and act per its instructions")
	fmt.Println("  Tier 1  Helper scripts present; no canonical entry point declared")
	fmt.Println("  Tier 2  exec: declared in front-matter; bin/<exec> exists")
	fmt.Println("  Tier 3  Tier-2 + schema: declared (typed input/output contract)")
	fmt.Println()
	fmt.Println("Skill Directories:")
	home, _ := os.UserHomeDir()
	fmt.Printf("  User-level:      %s/.claude/skills/\n", home)
	fmt.Println("  Workspace-level: <workspace>/.claude/skills/")
	fmt.Println()
	fmt.Println("Example SKILL.md front-matter (tier-2):")
	fmt.Println("  ---")
	fmt.Println("  name: my-skill")
	fmt.Println("  version: 0.1.0")
	fmt.Println("  description: What this skill does")
	fmt.Println("  exec: bin/run")
	fmt.Println("  requires: []")
	fmt.Println("  ---")
	return nil
}

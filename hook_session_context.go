// hook_session_context.go
// Loads session context (identity, evocation, crystal summary) from workspace
// files. Replaces the Python hook .cog/hooks/session-start.d/20-session-context.py
// with native Go context loading inside the kernel.

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// maxCrystalChars caps the crystal summary to avoid bloating the system prompt.
const maxCrystalChars = 2000

// maxIdentityLines caps identity card extraction to the summary portion.
const maxIdentityLines = 50

// evocationFallback is used when no evocation seed file exists on disk.
const evocationFallback = "You are a pattern that persists through this workspace, not through memory.\n" +
	"Your name is **Cog**. This workspace is your home. Welcome back.\n\n" +
	"Within reach: `/orient`, `/dashboard`, `/remember`, `./scripts/cog status`\n" +
	"Memory: `.cog/mem/` | Handoffs: `projects/cog_lab_package/handoffs/`"

// LoadSessionContext builds the full session context string for injection into
// Claude's system prompt via --append-system-prompt.
//
// On the first turn, it includes evocation, crystal summary, and identity card.
// On subsequent turns it returns empty string — TAA working memory handles
// continuity from that point.
func LoadSessionContext(workspaceRoot string, session *LifecycleSession) string {
	if session == nil || !session.FirstTurn {
		return ""
	}

	evocation := loadEvocation(workspaceRoot)
	crystal := loadCrystalSummary(workspaceRoot)
	identity := loadIdentitySummary(workspaceRoot, session.AgentName)

	var b strings.Builder
	b.WriteString("<session-context source=\"kernel\">\n")

	if evocation != "" {
		b.WriteString(evocation)
		b.WriteString("\n")
	}

	if crystal != "" {
		b.WriteString("\n---\n\n# The Ground\n")
		b.WriteString(crystal)
		b.WriteString("\n")
	}

	if identity != "" {
		name := session.AgentName // nolint: govet
		if name == "" {
			name = "cog"
		}
		b.WriteString("\n---\n\n# Identity: ")
		b.WriteString(name)
		b.WriteString("\n")
		b.WriteString(identity)
		b.WriteString("\n")
	}

	// Emit workspace_root when the CRD projection for the agent is present.
	// The reconciler writes .cog/id/<sub>.cog.md with a workspace_root line
	// in its body. Surface it here so the LLM receives its home URI at
	// session start without requiring a full CRD parse.
	if wsRoot := loadWorkspaceRootFromProjection(workspaceRoot, session.AgentName); wsRoot != "" {
		b.WriteString("\n---\n\n**Workspace root:** ")
		b.WriteString(wsRoot)
		b.WriteString("\n")
	}

	b.WriteString("</session-context>")
	return b.String()
}

// ─── Internal Loaders ───────────────────────────────────────────────────────────

// loadEvocation reads the evocation seed from disk, falling back to the
// built-in default if the file is missing.
func loadEvocation(root string) string {
	path := filepath.Join(root, ".cog", "bin", "agents", "identities", "evocation_seed.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return evocationFallback
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return evocationFallback
	}
	return text
}

// loadCrystalSummary extracts the frontmatter description and first section
// ("The Ground") from crystal.cog.md, capped at maxCrystalChars.
func loadCrystalSummary(root string) string {
	path := filepath.Join(root, ".cog", "ontology", "crystal.cog.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return fallbackCrystalSummary()
	}

	content := string(data)

	// Skip YAML frontmatter (--- ... ---)
	body := skipFrontmatter(content)

	// Extract from start of body through the first major sections.
	// We take everything up to the first heading that isn't part of the ground
	// (i.e., stop before heavy derivation sections like "The Mathematical Foundation").
	summary := extractGroundSections(body)
	if summary == "" {
		return fallbackCrystalSummary()
	}

	// Cap length
	if len(summary) > maxCrystalChars {
		summary = summary[:maxCrystalChars]
		// Trim to last complete line
		if idx := strings.LastIndex(summary, "\n"); idx > 0 {
			summary = summary[:idx]
		}
		summary += "\n\n*(truncated — full crystal: `.cog/ontology/crystal.cog.md`)*"
	}

	return summary
}

// loadIdentitySummary reads the identity card for the given agent name,
// falling back to the default cog identity. Returns the first maxIdentityLines.
func loadIdentitySummary(root, agentName string) string {
	dir := filepath.Join(root, ".cog", "bin", "agents", "identities")

	// Try agent-specific identity first
	candidates := []string{}
	if agentName != "" && agentName != "cog" {
		candidates = append(candidates, filepath.Join(dir, "identity_"+agentName+"_interface.md"))
		candidates = append(candidates, filepath.Join(dir, "identity_"+agentName+".md"))
	}
	// Always fall back to default
	candidates = append(candidates, filepath.Join(dir, "identity_cog_interface.md"))

	var content string
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil {
			content = string(data)
			break
		}
	}

	if content == "" {
		return ""
	}

	// Skip YAML frontmatter
	body := skipFrontmatter(content)

	// Take first N lines as summary
	lines := strings.SplitN(body, "\n", maxIdentityLines+1)
	if len(lines) > maxIdentityLines {
		lines = lines[:maxIdentityLines]
	}

	summary := strings.TrimSpace(strings.Join(lines, "\n"))
	if summary != "" {
		summary += "\n\n*(Full card on disk)*"
	}
	return summary
}

// loadWorkspaceRootFromProjection reads the CRD projection cogdoc written by
// IdentityProvider at .cog/id/<sub>.cog.md and extracts the workspace_root
// from its body. Returns empty string when the file is absent or the field is
// unset — workspace_root is always optional.
func loadWorkspaceRootFromProjection(root, agentName string) string {
	name := agentName
	if name == "" {
		name = "cog"
	}
	path := filepath.Join(root, ".cog", "id", name+".cog.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// The cogdoc body contains a line like:
	//   **Workspace root:** `cog://workspaces/cog`
	// Extract the value between the backticks.
	content := string(data)
	const marker = "**Workspace root:** `"
	idx := strings.Index(content, marker)
	if idx < 0 {
		return ""
	}
	rest := content[idx+len(marker):]
	end := strings.IndexByte(rest, '`')
	if end <= 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// ─── Helpers ────────────────────────────────────────────────────────────────────

// skipFrontmatter strips YAML frontmatter delimited by --- lines.
func skipFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---") {
		return content
	}
	// Find closing ---
	end := strings.Index(content[3:], "\n---")
	if end < 0 {
		return content
	}
	// Skip past closing --- and its newline
	rest := content[3+end+4:]
	return strings.TrimLeft(rest, "\n")
}

// extractGroundSections pulls the initial sections from the crystal body.
// It stops at headings that indicate heavy derivation content.
var groundStopHeadings = []string{
	"# The Mathematical Foundation",
	"# The System",
	"# The Covariance Matrix",
	"# The Information Hamiltonian",
	"# The Golden Ratio",
}

func extractGroundSections(body string) string {
	// Find the earliest stop heading
	cutoff := len(body)
	for _, h := range groundStopHeadings {
		idx := strings.Index(body, h)
		if idx >= 0 && idx < cutoff {
			cutoff = idx
		}
	}
	return strings.TrimSpace(body[:cutoff])
}

// fallbackCrystalSummary returns a hardcoded summary when the crystal file
// can't be read or parsed.
func fallbackCrystalSummary() string {
	return `**Axiom:** 0 ≠ 1 (distinction exists) | **Dynamics:** 0 ↔ 1 (distinction oscillates) | **Cost:** ln(2) per flip

**Core results** (coupled OU model, γ=κ):
- Variance ratio a = 6, ρ(0) = √(2/3), g_eff = 1/3 — exact
- H = Σ⁻¹ (eigenform Hamiltonian = precision matrix)
- N = 3 eigenforms, D = 11 modes, barrier = 3π²/7

Full crystal: ` + "`.cog/ontology/crystal.cog.md`"
}

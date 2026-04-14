package engine

import (
	"os"
	"path/filepath"
	"strings"
)

// ResolveToFieldKey normalizes any pointer form to the attentional field's
// canonical key (absolute filesystem path). Accepts:
//
//   - cog:// URIs:     cog://mem/semantic/insights/foo.cog.md
//   - short cog: URIs: cog:mem/semantic/insights/foo.cog.md
//   - memory-relative: semantic/insights/foo.cog.md
//   - absolute paths:  /Users/.../cog/.cog/mem/semantic/insights/foo.cog.md
//
// This is the "resolve locally" half of the holographic pointer — any form
// collapses to the same key regardless of where in the system it originated.
func ResolveToFieldKey(workspaceRoot, pointer string) string {
	// Already an absolute path — return as-is if it exists or looks plausible.
	if filepath.IsAbs(pointer) {
		return pointer
	}

	// cog:// URI — resolve through the projection system.
	if strings.HasPrefix(pointer, "cog://") {
		res, err := ResolveURI(workspaceRoot, pointer)
		if err == nil {
			return res.Path
		}
		// Fall through to heuristics.
	}

	// cog: shorthand (no //) — normalize to cog:// and resolve.
	if strings.HasPrefix(pointer, "cog:") {
		expanded := "cog://" + strings.TrimPrefix(pointer, "cog:")
		res, err := ResolveURI(workspaceRoot, expanded)
		if err == nil {
			return res.Path
		}
	}

	// Memory-relative path (semantic/..., episodic/..., etc.)
	memSectors := []string{"semantic/", "episodic/", "procedural/", "reflective/"}
	for _, prefix := range memSectors {
		if strings.HasPrefix(pointer, prefix) {
			return filepath.Join(workspaceRoot, ".cog", "mem", pointer)
		}
	}

	// Workspace-relative path (.cog/mem/..., .cog/config/..., etc.)
	if strings.HasPrefix(pointer, ".cog/") {
		return filepath.Join(workspaceRoot, pointer)
	}

	// Last resort: treat as workspace-relative.
	return filepath.Join(workspaceRoot, pointer)
}

// FieldKeyToURI converts an absolute filesystem path (field key) back to a
// canonical cog:// URI. This is the "project outward" half of the holographic
// pointer — the internal key becomes a portable, context-free identifier.
//
// Returns the path unchanged if it can't be mapped to a cog:// URI.
func FieldKeyToURI(workspaceRoot, absPath string) string {
	rel, err := filepath.Rel(workspaceRoot, absPath)
	if err != nil {
		return absPath
	}

	// .cog/mem/ → cog://mem/
	if strings.HasPrefix(rel, ".cog/mem/") {
		return "cog://mem/" + strings.TrimPrefix(rel, ".cog/mem/")
	}
	// .cog/docs/ → cog://docs/
	if strings.HasPrefix(rel, ".cog/docs/") {
		return "cog://docs/" + strings.TrimPrefix(rel, ".cog/docs/")
	}
	// .cog/adr/ → cog://adr/
	if strings.HasPrefix(rel, ".cog/adr/") {
		return "cog://adr/" + strings.TrimPrefix(rel, ".cog/adr/")
	}
	// .cog/config/ → cog://conf/
	if strings.HasPrefix(rel, ".cog/config/") {
		return "cog://conf/" + strings.TrimPrefix(rel, ".cog/config/")
	}
	// .cog/ontology/ → cog://ontology/
	if strings.HasPrefix(rel, ".cog/ontology/") {
		stem := strings.TrimPrefix(rel, ".cog/ontology/")
		stem = strings.TrimSuffix(stem, ".cog.md")
		return "cog://ontology/" + stem
	}
	// .claude/skills/ → cog://skill/
	if strings.HasPrefix(rel, ".claude/skills/") {
		return "cog://skill/" + strings.TrimPrefix(rel, ".claude/skills/")
	}

	// Can't map — return the relative path as a workspace URI.
	return "cog://workspace/" + rel
}

// PathExistsOnDisk reports whether the resolved path actually exists.
func PathExistsOnDisk(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// coherence.go — importable graded git-tree coherence surface.
//
// First Instruments Module B (M1-A). This package makes the CLI's
// canonical-vs-current git-tree-hash coherence check (checkCoherence /
// CoherenceState, root-package cog.go) importable from internal/testkernel
// and Module D's experiment runner, which cannot import package main.
//
// PACKAGE-BOUNDARY NOTE (IMPL-SPEC.md Module B "Where" + naming-collision
// hazard): this is a MINIMAL, DUPLICATED extraction per the RFC-0001
// "duplicate-minimally" allowance (docs/rfcs/0001-root-package-refactor.md),
// not a move. The root-package originals (cog.go: CoherenceState @ ~779,
// checkCoherence @ ~2173, isPathTracked @ ~2149, gitDiffTree @ ~1652) are
// UNTOUCHED by this package — root `package main` is confirmed dead code
// (RFC-0001 §Background: "not built into the shipped binary, not imported by
// anything") so leaving it alone is correct; deleting/rewiring 204 root files
// is RFC-0001's job, not First Instruments'.
//
// There are TWO OTHER pre-existing types this package must not collide with
// or be confused with:
//  1. sdk/types.CoherenceState (sdk/types/coherence.go:6) — a different type
//     with the same name, package types, fields Coherent/CanonicalHash/
//     CurrentHash/Drift/Timestamp. Unrelated to this package's CoherenceState;
//     do not add Score to it, do not create a third CoherenceState under a
//     third name — this package's type is distinct and fully qualified as
//     coherence.CoherenceState at every call site.
//  2. internal/engine.CoherenceReport / RunCoherence (internal/engine/
//     coherence.go) — an entirely different 4-layer validation stack
//     (schema/invariants/policy/consistency), not a git-tree-hash diff. Do
//     not conflate M1-A's graded git-distance score with that report.
//
// The tracked/excluded path lists here are the same hardcoded defaults the
// root package falls back to (cog.go trackedPaths/excludedPaths) when no
// ontology cache is loaded. The root package's ontology-aware override
// (getEffectiveTrackedPaths/getEffectiveExcludedPaths, reading a
// package-global ontologyCache loaded from .cog/ontology/) is root-package-
// only state and is NOT duplicated here — duplicating minimally means this
// package computes coherence against the same conservative default surface
// every fresh kernel boot sees before any ontology is cached, which is
// exactly the measurement-harness use case (a testkernel boot never loads
// the root package's ontology cache).
package coherence

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// trackedPaths mirrors cog.go's package-level default (relative to .cog/).
var trackedPaths = []string{
	"mem/",
	"schemas/",
	"adr/",
	"roles/",
	"coordination/",
}

// excludedPaths mirrors cog.go's package-level default (relative to .cog/).
var excludedPaths = []string{
	"status/",
	"signals/",
	"work/",
	"run/",
	"var/",
}

// CoherenceState represents the coherence check result, extended with the
// First Instruments M1-A graded score (Score field). Side-effect-free to
// compute (K3/K8 one-way-readout discipline, IMPL-SPEC §0/§2): does not
// mutate the boolean Coherent field's meaning, does not touch any gate.
type CoherenceState struct {
	Coherent      bool     `json:"coherent"`
	CanonicalHash string   `json:"canonical_hash,omitempty"`
	CurrentHash   string   `json:"current_hash,omitempty"`
	Timestamp     string   `json:"timestamp"`
	Drift         []string `json:"drift,omitempty"`

	// Score is the M1-A graded git-distance coherence score (IMPL-SPEC B1):
	//   C_A = 1 - min(1, len(Drift)/N_tracked)
	// where N_tracked is the count of tracked .cog/ paths present at the time
	// of the check. Score is in [0,1]; 1.0 = no drift, 0.0 = drift at or
	// beyond every tracked path. Does NOT replace or feed back into Coherent.
	Score float64 `json:"score"`
}

// gitCmd creates an exec.Cmd for a local git operation with a 30-second
// timeout, mirroring cog.go's gitCmd helper. The returned cancel func MUST
// be deferred by the caller.
func gitCmd(args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	return exec.CommandContext(ctx, "git", args...), cancel
}

// isPathTracked mirrors cog.go's isPathTracked, against the hardcoded
// default tracked/excluded lists (no ontology-cache override — see package
// doc comment).
func isPathTracked(filePath string) bool {
	if !strings.HasPrefix(filePath, ".cog/") {
		return false
	}
	relPath := filePath[5:] // remove ".cog/" prefix

	for _, excluded := range excludedPaths {
		if strings.HasPrefix(relPath, excluded) {
			return false
		}
	}
	for _, tracked := range trackedPaths {
		if strings.HasPrefix(relPath, tracked) {
			return true
		}
	}
	return false
}

// countTrackedPaths walks root/.cog and counts entries whose relative path
// (".cog/..." form) is tracked per isPathTracked. This is N_tracked in the
// B1 formula: the baseline denominator against which drift is graded.
func countTrackedPaths(root string) (int, error) {
	cogRoot := filepath.Join(root, ".cog")
	count := 0
	err := filepath.Walk(cogRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// Best-effort: a single unreadable entry should not abort the
			// whole count; skip it and continue.
			return nil
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if isPathTracked(rel) {
			count++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("coherence: count tracked paths: %w", err)
	}
	return count, nil
}

// gitCogTreeHash computes the tree hash of the .cog/ working tree without
// mutating the real git index (mirrors cog.go's gitCogTreeHash, K3
// one-way-readout: stages into a throwaway GIT_INDEX_FILE so the caller's
// staged changes are never touched).
func gitCogTreeHash(gitRoot string) (string, error) {
	tmpIdx, err := os.CreateTemp("", "cog-idx-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp index: %w", err)
	}
	tmpIdxPath := tmpIdx.Name()
	tmpIdx.Close()
	os.Remove(tmpIdxPath) //nolint:errcheck // best-effort; path is still unique
	defer os.Remove(tmpIdxPath)

	env := append(os.Environ(), "GIT_INDEX_FILE="+tmpIdxPath)

	stageCmd, stageCancel := gitCmd("-C", gitRoot, "add", "-A", ".cog/")
	defer stageCancel()
	stageCmd.Env = env
	if err := stageCmd.Run(); err != nil {
		// Non-fatal: may fail on an empty .cog/ or a bare repo.
		_ = err
	}

	writeCmd, writeCancel := gitCmd("-C", gitRoot, "write-tree", "--prefix=.cog/")
	defer writeCancel()
	writeCmd.Env = env
	out, err := writeCmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to compute tree hash: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCanonicalHash reads the last validated tree hash from
// .cog/run/coherence/canonical-hash (mirrors cog.go's gitCanonicalHash;
// ADR-021 holographic workspace model — canonical state is a stored hash,
// not a separate git branch).
func gitCanonicalHash(gitRoot string) (string, error) {
	hashFile := filepath.Join(gitRoot, ".cog", "run", "coherence", "canonical-hash")
	data, err := os.ReadFile(hashFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no canonical hash found (run baseline to establish)")
		}
		return "", fmt.Errorf("failed to read canonical hash: %w", err)
	}
	hash := strings.TrimSpace(string(data))
	if hash == "" {
		return "", fmt.Errorf("canonical hash file is empty")
	}
	return hash, nil
}

// gitDiffTree computes which files differ between two tree hashes (mirrors
// cog.go's gitDiffTree). Read-only: `git diff-tree` does not mutate any
// index or working tree state.
func gitDiffTree(gitRoot, fromHash, toHash string) ([]string, error) {
	cmd, cancel := gitCmd("-C", gitRoot, "diff-tree", "-r", "--name-only", fromHash, toHash)
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var result []string
	for _, line := range lines {
		if line != "" {
			result = append(result, ".cog/"+line)
		}
	}
	return result, nil
}

// nowISO mirrors cog.go's nowISO timestamp format.
func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// CheckCoherence checks overall coherence of the workspace and computes the
// M1-A graded score (IMPL-SPEC B1). Side-effect-free: gitCogTreeHash uses a
// throwaway index (K3); no kernel state is mutated.
//
//	C_A = 1 - min(1, len(Drift)/N_tracked)
//
// N_tracked is the count of tracked .cog/ paths (isPathTracked) at the time
// of the check. When N_tracked is 0 (no tracked paths exist, e.g. a minimal
// test workspace with an empty .cog/mem/), Score is defined as 1.0 (fully
// coherent by construction — there is nothing tracked to have drifted, the
// same "nothing to reconcile is in-sync" reasoning as the B2 empty-plan fix,
// applied to the git-distance side of the surface).
func CheckCoherence(root string) (*CoherenceState, error) {
	canonical, canonicalErr := gitCanonicalHash(root)
	current, currentErr := gitCogTreeHash(root)

	state := &CoherenceState{
		Timestamp: nowISO(),
	}

	if canonicalErr != nil {
		// No baseline = coherent by default (mirrors cog.go's checkCoherence).
		state.Coherent = true
		state.CurrentHash = current
		state.Score = 1.0
		return state, nil
	}

	state.CanonicalHash = canonical
	state.CurrentHash = current
	state.Coherent = canonical == current

	if !state.Coherent && canonical != "" && current != "" {
		drift, err := gitDiffTree(root, canonical, current)
		if err == nil {
			state.Drift = drift
		}
	}

	nTracked, countErr := countTrackedPaths(root)
	switch {
	case countErr != nil || nTracked == 0:
		// Nothing tracked to have drifted -> fully coherent on this axis.
		state.Score = 1.0
	default:
		fraction := float64(len(state.Drift)) / float64(nTracked)
		if fraction > 1 {
			fraction = 1
		}
		state.Score = 1 - fraction
	}

	return state, currentErr
}

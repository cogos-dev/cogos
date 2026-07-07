// injectors.go — First Instruments Module D1: perturbation injectors.
//
// Each injector writes to the filesystem/git the kernel watches, then calls
// daemon.Trigger(providerType) DIRECTLY (never relying on the fsnotify
// path — D2's tick-attribution guard requires PollInterval set very high so
// Trigger() is the sole cycle driver).
package experiment

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// MemoryFileDriftInjector mutates N cogdocs under workspaceRoot/.cog/mem/semantic/.
// K1-respecting: writes to the real filesystem the kernel would watch, not
// a synthetic in-memory event.
type MemoryFileDriftInjector struct {
	WorkspaceRoot string
	N             int
}

// Inject writes N small markdown files under .cog/mem/semantic/, returning
// the paths written.
func (m MemoryFileDriftInjector) Inject() ([]string, error) {
	dir := filepath.Join(m.WorkspaceRoot, ".cog", "mem", "semantic")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("injector: mkdir %s: %w", dir, err)
	}
	var written []string
	for i := 0; i < m.N; i++ {
		p := filepath.Join(dir, fmt.Sprintf("fi-drift-%d.cog.md", i))
		content := fmt.Sprintf("# First Instruments drift injection %d\n\ntest content.\n", i)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			return nil, fmt.Errorf("injector: write %s: %w", p, err)
		}
		written = append(written, p)
	}
	return written, nil
}

// WorktreeDivergenceInjector creates N divergent commits under .cog/ in a
// git worktree, simulating "worktree divergence" drift — the FROZEN
// confirmatory perturbation cell (medium, commits_ahead=4, PREREG §6.7).
type WorktreeDivergenceInjector struct {
	WorkspaceRoot string
	CommitsAhead  int
}

// Inject creates CommitsAhead commits, each touching one file under
// .cog/mem/semantic/. Requires WorkspaceRoot to be a git repository
// (typically true for a testkernel boot's temp workspace only if the
// caller has git-inited it — testkernel's makeMinimalWorkspace does NOT
// init git, so callers using this injector against a testkernel boot must
// git-init the workspace themselves first, or this injector will surface a
// clear error rather than silently no-op).
func (w WorktreeDivergenceInjector) Inject() error {
	dir := filepath.Join(w.WorkspaceRoot, ".cog", "mem", "semantic")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("injector: mkdir %s: %w", dir, err)
	}
	runGit := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = w.WorkspaceRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("injector: git %v: %w\n%s", args, err, out)
		}
		return nil
	}
	// Verify this is a git repo before proceeding (clear error, not silent no-op).
	if err := runGit("rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("injector: WorktreeDivergenceInjector requires WorkspaceRoot to be a git repository: %w", err)
	}
	for i := 0; i < w.CommitsAhead; i++ {
		p := filepath.Join(dir, fmt.Sprintf("fi-worktree-divergence-%d.cog.md", i))
		content := fmt.Sprintf("# worktree divergence commit %d\n", i)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			return fmt.Errorf("injector: write %s: %w", p, err)
		}
		if err := runGit("add", filepath.Join(".cog", "mem", "semantic", fmt.Sprintf("fi-worktree-divergence-%d.cog.md", i))); err != nil {
			return err
		}
		if err := runGit("commit", "-m", fmt.Sprintf("first-instruments: worktree divergence commit %d/%d", i+1, w.CommitsAhead)); err != nil {
			return err
		}
	}
	return nil
}

// ConfigDriftInjector rewrites .cog/config/kernel.yaml vs. the frozen
// in-memory *Config the kernel loaded at boot.
type ConfigDriftInjector struct {
	WorkspaceRoot string
	YAML          string // raw kernel.yaml content to write
}

func (c ConfigDriftInjector) Inject() error {
	dir := filepath.Join(c.WorkspaceRoot, ".cog", "config")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("injector: mkdir %s: %w", dir, err)
	}
	p := filepath.Join(dir, "kernel.yaml")
	if err := os.WriteFile(p, []byte(c.YAML), 0644); err != nil {
		return fmt.Errorf("injector: write %s: %w", p, err)
	}
	return nil
}

// ProjectionSymlinkInjector deletes/repoints a projection target. Its
// results are pre-registered as DRIFT-CONFIRMATION, NOT invariant-search
// (PREREG §3.4/§7) — routed through direct Trigger, never through the
// bidirectional-reconcile fsnotify path (the 500ms debounce would
// contaminate timing if exercised; see IMPL-SPEC D1).
type ProjectionSymlinkInjector struct {
	TargetPath string
	RepointTo  string // empty = delete; non-empty = repoint symlink to this path
}

func (p ProjectionSymlinkInjector) Inject() error {
	if p.RepointTo == "" {
		if err := os.Remove(p.TargetPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("injector: remove symlink %s: %w", p.TargetPath, err)
		}
		return nil
	}
	_ = os.Remove(p.TargetPath) // best-effort; may not exist yet
	if err := os.Symlink(p.RepointTo, p.TargetPath); err != nil {
		return fmt.Errorf("injector: symlink %s -> %s: %w", p.TargetPath, p.RepointTo, err)
	}
	return nil
}

// FrozenConfirmatoryInjector is the FROZEN confirmatory perturbation cell
// (PREREG §6.7): worktree-divergence, medium size, commits_ahead=4 —
// identical across all 9 clock cells so only the clock config varies.
const FrozenConfirmatoryCommitsAhead = 4

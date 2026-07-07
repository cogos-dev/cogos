// shellgate_test.go — regression tests for scripts/check-shell-hardening.sh
// (ledger L15: cheap CI gate requiring `set -euo pipefail` — or a
// documented `# no-pipefail:` opt-out — in every tracked scripts/*.sh
// file, plus a shellcheck error-severity pass).
//
// This package holds no production code; it exists solely so `go test
// ./...` (the CI test job) exercises the gate script's pass/fail logic
// directly, rather than only ever observing it green against the
// already-hardened tree. Each test builds a throwaway git repo with a
// `scripts/` directory and a handful of fixture .sh files, then shells
// out to the real check-shell-hardening.sh against that fixture tree.
package shellgate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot walks up from this file's directory to find the repository
// root (identified by the presence of go.work or go.mod at the top).
// internal/shellgate -> internal -> <repo root>.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	// this file: <root>/internal/shellgate/shellgate_test.go
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	gatePath := filepath.Join(root, "scripts", "check-shell-hardening.sh")
	if _, err := os.Stat(gatePath); err != nil {
		t.Fatalf("expected gate script at %s: %v", gatePath, err)
	}
	return root
}

// newFixtureRepo creates a temp directory, git-inits it, writes the given
// scripts/<name> -> content files, and `git add`s them so the gate's
// `git ls-files` scoping picks them up (the gate only checks *tracked*
// files by design).
func newFixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	runOK := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
		}
	}

	runOK("git", "init", "-q")
	runOK("git", "config", "user.email", "gate-test@example.invalid")
	runOK("git", "config", "user.name", "gate-test")

	scriptsDir := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}

	for name, content := range files {
		p := filepath.Join(scriptsDir, name)
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	runOK("git", "add", "-A")
	runOK("git", "commit", "-q", "-m", "fixture")

	return dir
}

// runGate invokes the real check-shell-hardening.sh against the fixture
// repo's scripts/ dir and returns (exitCode, combinedOutput).
func runGate(t *testing.T, fixtureDir string) (int, string) {
	t.Helper()
	root := repoRoot(t)
	gate := filepath.Join(root, "scripts", "check-shell-hardening.sh")

	cmd := exec.Command("bash", gate, filepath.Join(fixtureDir, "scripts"))
	out, err := cmd.CombinedOutput()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run gate script: %v\n%s", err, out)
		}
	}
	return exitCode, string(out)
}

func TestGate_PassesHardenedBashScript(t *testing.T) {
	dir := newFixtureRepo(t, map[string]string{
		"good.sh": "#!/usr/bin/env bash\nset -euo pipefail\necho hi\n",
	})
	code, out := runGate(t, dir)
	if code != 0 {
		t.Fatalf("expected pass (exit 0), got exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "PASSED") {
		t.Fatalf("expected PASSED in output, got:\n%s", out)
	}
}

func TestGate_PassesHardenedPosixShScript(t *testing.T) {
	// POSIX sh has no pipefail — `set -eu` is the correct hardening form.
	dir := newFixtureRepo(t, map[string]string{
		"good.sh": "#!/bin/sh\nset -eu\necho hi\n",
	})
	code, out := runGate(t, dir)
	if code != 0 {
		t.Fatalf("expected pass (exit 0), got exit %d:\n%s", code, out)
	}
}

func TestGate_PassesDocumentedOptOut(t *testing.T) {
	dir := newFixtureRepo(t, map[string]string{
		"lib.sh": "#!/bin/sh\n# no-pipefail: sourced library, set -e would leak into the caller's shell\necho hi\n",
	})
	code, out := runGate(t, dir)
	if code != 0 {
		t.Fatalf("expected pass (exit 0) for documented opt-out, got exit %d:\n%s", code, out)
	}
}

func TestGate_FailsUnhardenedScript(t *testing.T) {
	dir := newFixtureRepo(t, map[string]string{
		"bad.sh": "#!/usr/bin/env bash\necho hi\n",
	})
	code, out := runGate(t, dir)
	if code == 0 {
		t.Fatalf("expected failure (nonzero exit) for un-hardened script, got exit 0:\n%s", out)
	}
	if !strings.Contains(out, "bad.sh") {
		t.Fatalf("expected failure output to name bad.sh, got:\n%s", out)
	}
}

func TestGate_SkipsNonShellShebang(t *testing.T) {
	// A .sh-suffixed file whose shebang is not a shell interpreter (e.g. a
	// misnamed Python script) must be skipped entirely, not flagged.
	dir := newFixtureRepo(t, map[string]string{
		"not_shell.sh": "#!/usr/bin/env python3\nprint('hi')\n",
	})
	code, out := runGate(t, dir)
	if code != 0 {
		t.Fatalf("expected pass (exit 0) — non-shell-shebang file should be skipped, got exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "skipped 1") {
		t.Fatalf("expected output to report 1 skipped file, got:\n%s", out)
	}
}

func TestGate_FailsOnShellcheckErrorSeverity(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck not installed — skipping error-severity test")
	}
	// SC2154-class parse error: an unterminated quote is a shellcheck
	// error-severity finding (not just a warning), and also independently
	// hardened so this isolates the shellcheck check specifically.
	dir := newFixtureRepo(t, map[string]string{
		"broken.sh": "#!/usr/bin/env bash\nset -euo pipefail\necho \"unterminated\n",
	})
	code, out := runGate(t, dir)
	if code == 0 {
		t.Fatalf("expected failure (nonzero exit) for shellcheck error-severity finding, got exit 0:\n%s", out)
	}
	if !strings.Contains(out, "broken.sh") {
		t.Fatalf("expected failure output to name broken.sh, got:\n%s", out)
	}
}

func TestGate_MixedTreePassesOnlyWhenAllFilesClean(t *testing.T) {
	dir := newFixtureRepo(t, map[string]string{
		"good.sh":      "#!/usr/bin/env bash\nset -euo pipefail\necho hi\n",
		"lib.sh":       "#!/bin/sh\n# no-pipefail: sourced library\necho hi\n",
		"not_shell.sh": "#!/usr/bin/env python3\nprint('hi')\n",
	})
	code, out := runGate(t, dir)
	if code != 0 {
		t.Fatalf("expected pass for a fully-compliant mixed tree, got exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "checked 2") || !strings.Contains(out, "skipped 1") {
		t.Fatalf("expected checked=2 skipped=1 in output, got:\n%s", out)
	}
}

// TestRealScriptsDirectoryPassesGate is the actual acceptance check: run
// the gate against this repo's own scripts/ directory (the real thing the
// CI job invokes), so a regression in the checked-in scripts themselves
// fails `go test ./...`, not just the CI-only shell step.
func TestRealScriptsDirectoryPassesGate(t *testing.T) {
	root := repoRoot(t)
	gate := filepath.Join(root, "scripts", "check-shell-hardening.sh")

	cmd := exec.Command("bash", gate, filepath.Join(root, "scripts"))
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check-shell-hardening.sh failed against repo scripts/:\n%s", out)
	}
	if !strings.Contains(string(out), "PASSED") {
		t.Fatalf("expected PASSED, got:\n%s", out)
	}
}

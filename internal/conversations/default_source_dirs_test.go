// default_source_dirs_test.go — coverage for defaultSourceDirs()'s HOME-derived
// project-slug computation.
//
// defaultSourceDirs() is production code, not a test/doc site: it decides which
// JSONL directories the Observatory ingests when observatory.yaml declares none.
// It previously hardcoded one developer's home-directory slug; it now derives the
// slug from $HOME by replacing each path separator with a hyphen, matching Claude
// Code's own per-project bucket naming under ~/.claude/projects/.
//
// The failure mode this guards is silent: a broken separator substitution (reversed
// ReplaceAll arguments, or mishandling the Windows separator) still returns a
// plausible-looking path, so discovery degrades to the generic-fallback branch for
// every user instead of erroring. These tests pin the slug shape exactly and pin
// the preferred/fallback/absent branch selection.
package conversations

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setFakeHome points os.UserHomeDir() at dir for the duration of the test.
// os.UserHomeDir reads $HOME everywhere except Windows, where it reads
// %USERPROFILE%; set both so the test is meaningful on either platform.
func setFakeHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", dir)
	}
}

// expectedSlug builds the bucket name the way Claude Code does, formulated
// independently of the implementation (split/join rather than ReplaceAll) so a
// reversed-argument regression in the implementation cannot mirror itself here.
func expectedSlug(home string) string {
	return strings.Join(strings.Split(home, string(filepath.Separator)), "-")
}

func TestDefaultSourceDirs_PrefersHomeSlugBucket(t *testing.T) {
	home := t.TempDir()
	setFakeHome(t, home)

	slug := expectedSlug(home)
	want := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatalf("mkdir home-slug bucket: %v", err)
	}

	got := defaultSourceDirs()
	if len(got) != 1 || got[0] != want {
		t.Errorf("defaultSourceDirs() = %v, want [%s]", got, want)
	}
}

// TestDefaultSourceDirs_SlugReplacesSeparatorsNotHyphens is the discriminating
// case: a home directory whose own name already contains a hyphen. The correct
// substitution (separator -> hyphen) leaves the existing hyphen alone; the
// reversed substitution (hyphen -> separator) would split it into a subdirectory
// and miss the bucket entirely.
func TestDefaultSourceDirs_SlugReplacesSeparatorsNotHyphens(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "example-user")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	setFakeHome(t, home)

	slug := expectedSlug(home)
	if !strings.HasSuffix(slug, "-example-user") {
		t.Fatalf("expectedSlug(%q) = %q, want it to end in -example-user", home, slug)
	}
	if strings.Contains(slug, string(filepath.Separator)) {
		t.Errorf("slug %q still contains a path separator", slug)
	}

	want := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatalf("mkdir home-slug bucket: %v", err)
	}

	got := defaultSourceDirs()
	if len(got) != 1 || got[0] != want {
		t.Errorf("defaultSourceDirs() = %v, want [%s]", got, want)
	}
}

func TestDefaultSourceDirs_FallsBackToGenericProjectsDir(t *testing.T) {
	home := t.TempDir()
	setFakeHome(t, home)

	// Only the generic projects dir exists — no bucket for this home's own slug.
	generic := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(generic, 0o755); err != nil {
		t.Fatalf("mkdir generic projects dir: %v", err)
	}

	got := defaultSourceDirs()
	if len(got) != 1 || got[0] != generic {
		t.Errorf("defaultSourceDirs() = %v, want [%s]", got, generic)
	}
}

func TestDefaultSourceDirs_NilWhenNoClaudeProjectsDir(t *testing.T) {
	setFakeHome(t, t.TempDir())

	if got := defaultSourceDirs(); got != nil {
		t.Errorf("defaultSourceDirs() = %v, want nil when ~/.claude/projects is absent", got)
	}
}

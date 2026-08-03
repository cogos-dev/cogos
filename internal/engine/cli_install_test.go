//go:build !windows

package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// realHomeAtLoad captures the process's actual $HOME before any test in this
// package can override it. Package-level var initializers run before any
// test (and before TestMain, if one existed), so this is guaranteed to see
// the real value regardless of t.Setenv calls made later. It backs
// TestAddCogBinToPathNeverTouchesRealHome below — the regression guard for
// the incident where a HOME-mutation race let addCogBinToPath resolve and
// append to the operator's real ~/.zshrc.
var realHomeAtLoad = os.Getenv("HOME")

// NOT t.Parallel: these tests point $HOME at a t.TempDir, and $HOME is process-
// global. Combining t.Parallel with a global os.Setenv("HOME", …) leaked the
// fake home to every concurrently-running test — which stayed invisible only
// while nothing else resolved paths from $HOME. Node identity is now machine-
// anchored (see node_identity.go), so a concurrent test that boots a Process
// would materialize <tmp>/.cog/node inside this test's TempDir and break its
// cleanup. t.Setenv is the correct primitive and refuses to run under
// t.Parallel, which is exactly the constraint being honored here.
func TestAddCogBinToPathWritesRCLine(t *testing.T) {
	// Use a temp home with a pre-existing .bashrc so detectShellRC finds it.
	tmp := t.TempDir()
	bashrc := filepath.Join(tmp, ".bashrc")
	if err := os.WriteFile(bashrc, []byte("# existing\n"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", tmp)
	t.Setenv("SHELL", "/bin/bash")

	if err := addCogBinToPath(); err != nil {
		t.Fatalf("addCogBinToPath: %v", err)
	}

	content, err := os.ReadFile(bashrc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), ".cog/bin") {
		t.Errorf("expected .cog/bin in %s; got:\n%s", bashrc, string(content))
	}
}

func TestAddCogBinToPathIdempotent(t *testing.T) {
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, ".cog", "bin")
	bashrc := filepath.Join(tmp, ".bashrc")
	// Pre-seed with the export line.
	seed := "export PATH=\"" + binDir + ":$PATH\"\n"
	if err := os.WriteFile(bashrc, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", tmp)
	t.Setenv("SHELL", "/bin/bash")

	if err := addCogBinToPath(); err != nil {
		t.Fatalf("addCogBinToPath (second call): %v", err)
	}

	// File should be unchanged (no duplicate line added).
	content, err := os.ReadFile(bashrc)
	if err != nil {
		t.Fatal(err)
	}
	count := strings.Count(string(content), binDir)
	if count != 1 {
		t.Errorf("expected exactly 1 occurrence of binDir in %s; got %d:\n%s", bashrc, count, string(content))
	}
}

func TestDetectShellRCZsh(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHELL", "/bin/zsh")
	rc := detectShellRC()
	if !strings.HasSuffix(rc, ".zshrc") {
		t.Errorf("expected .zshrc; got %s", rc)
	}
}

// TestAddCogBinToPathNeverTouchesRealHome is a regression guard for the
// 2026-08 incident: a HOME-mutation race (t.Parallel plus a global,
// unsynchronized os.Setenv("HOME", …) — see the historical note above)
// let addCogBinToPath resolve the operator's REAL ~/.zshrc mid-test and
// append an export line pointing at a t.TempDir() path. That race is now
// closed (t.Setenv, no t.Parallel on any test that touches HOME/SHELL), but
// this test proves the property directly rather than trusting the
// mechanism: with HOME overridden, addCogBinToPath must never open, read,
// or write any file under the real home captured at package load — under
// zsh specifically, since that's the shell the incident hit.
func TestAddCogBinToPathNeverTouchesRealHome(t *testing.T) {
	if realHomeAtLoad == "" {
		t.Skip("real $HOME unavailable at package load; cannot assert isolation")
	}

	// Every rc file addCogBinToPath's shell detection could possibly resolve
	// under the REAL home, snapshotted before the function under test runs.
	candidates := []string{
		filepath.Join(realHomeAtLoad, ".zshrc"),
		filepath.Join(realHomeAtLoad, ".bashrc"),
		filepath.Join(realHomeAtLoad, ".bash_profile"),
		filepath.Join(realHomeAtLoad, ".config", "fish", "config.fish"),
	}
	before := snapshotFileHashes(t, candidates)

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHELL", "/bin/zsh")

	// Belt-and-suspenders: the resolved rc path itself must not fall under
	// the real home, independent of whether its content later changes.
	if rc := detectShellRC(); strings.HasPrefix(rc, realHomeAtLoad+string(filepath.Separator)) {
		t.Fatalf("detectShellRC resolved a path under the REAL home (%s) while HOME was overridden to %s: %s", realHomeAtLoad, tmp, rc)
	}

	if err := addCogBinToPath(); err != nil {
		t.Fatalf("addCogBinToPath: %v", err)
	}

	after := snapshotFileHashes(t, candidates)
	for _, c := range candidates {
		if before[c] != after[c] {
			t.Errorf("real home file %s changed while HOME was overridden to %s (this is the exact class of the 2026-08 ~/.zshrc incident)", c, tmp)
		}
	}
}

// snapshotFileHashes reads each path read-only and returns a sha256 hex
// digest, or the sentinel "<absent>" when the file does not exist. It never
// writes anything.
func snapshotFileHashes(t *testing.T, paths []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			out[p] = "<absent>"
			continue
		}
		sum := sha256.Sum256(b)
		out[p] = hex.EncodeToString(sum[:])
	}
	return out
}

// TestNoTestSetsHOMEGlobally makes the incident's sibling audit executable.
//
// TestAddCogBinToPathNeverTouchesRealHome (above) proves addCogBinToPath
// honors $HOME, so it catches the production-side regression: any change
// that stops reading the live $HOME — caching os.UserHomeDir() in a package
// var, or switching to os/user.Current(), whose HomeDir comes from the
// passwd entry and ignores $HOME entirely — makes it fail loudly.
//
// It cannot, however, catch the ORIGINAL incident, and that gap is why this
// test exists. That bug was a concurrency race between tests: t.Parallel
// plus a global os.Setenv("HOME", …) whose t.Cleanup restored the real
// HOME/SHELL while a sibling was still inside addCogBinToPath. Go parks
// parallel tests until the serial ones finish, so a serial guard (this file
// uses t.Setenv, which refuses to run under t.Parallel) never overlaps the
// offending window and cannot observe the race. Reproducing the pre-fix code
// confirms this: the race resurfaces — nondeterministically, and usually as
// a TempDir cleanup failure rather than a dotfile write — while the guard
// above passes.
//
// So the real protection is structural: nothing may set HOME process-wide in
// a test. t.Setenv is the only sanctioned primitive, and it is compile-time
// incompatible with t.Parallel. This asserts that invariant across the whole
// module, deterministically and on every run, rather than trusting a
// point-in-time grep. It parses the AST rather than grepping so that prose
// mentions of os.Setenv("HOME", …) in comments — several appear above — are
// not false positives.
func TestNoTestSetsHOMEGlobally(t *testing.T) {
	root := moduleRoot(t)

	fset := token.NewFileSet()
	var offenders []string

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable subtree is not this test's concern
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // not parseable (build-tagged fixture, etc.); skip
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Setenv" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			if name == "HOME" || name == "USERPROFILE" {
				rel, rerr := filepath.Rel(root, path)
				if rerr != nil {
					rel = path
				}
				offenders = append(offenders, fmt.Sprintf("%s:%d", rel, fset.Position(call.Pos()).Line))
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}

	if len(offenders) > 0 {
		t.Errorf("tests must not set %s process-wide via os.Setenv; use t.Setenv, "+
			"which scopes the override to the test and refuses to run under t.Parallel "+
			"(this is the 2026-08 ~/.zshrc incident's root cause). Offenders:\n  %s",
			"HOME/USERPROFILE", strings.Join(offenders, "\n  "))
	}
}

// moduleRoot returns the directory holding go.mod, walking up from the
// working directory so the module-wide scan above is independent of where
// `go test` was invoked.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found at or above %s", dir)
		}
		dir = parent
	}
}

// git_optional_locks_conformance_test.go — every `git status` the kernel runs
// under a context deadline must decline the index lock.
//
// # WHY THIS EXISTS
//
// `git status` is not read-only on disk. It opportunistically refreshes the
// index, and to do that it takes `.git/index.lock`. Every call site in this
// package runs git via exec.CommandContext, whose deadline expiry path is
// Process.Kill() — SIGKILL, which git cannot trap. A status killed inside its
// refresh window leaves a zero-byte index.lock behind.
//
// That is not a local annoyance. Linked worktrees SHARE the main repo's .git,
// so a single killed *read* of one worktree blocks every *write* in every
// worktree of that repo until a human deletes the file. The worktree
// reconciler polls every worktree of a repo on a timer, which is precisely the
// shape that turns a rare race into a daily outage.
//
// `git --no-optional-locks status` makes git refuse to take a lock it does not
// strictly need, so there is nothing to orphan. It is a GLOBAL option and must
// precede the subcommand.
//
// This is a CONFORMANCE probe over the package source — the public shape of
// "how this package shells out to git" — rather than a line-pinned assertion
// about one function. New git callers can be added freely; they just have to
// pass the flag. A test that pinned line 886 would pass forever while the next
// caller reintroduced the bug three functions down.
package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Matches a Go exec argument list that invokes `git` followed immediately by
// `status` — i.e. with no global option in between. Deliberately tolerant of
// whitespace, and deliberately blind to which exec helper is used, so it also
// catches exec.Command, exec.CommandContext, and any wrapper taking the same
// variadic string args.
var bareGitStatus = regexp.MustCompile(`"git"\s*,\s*"status"`)

func TestGitStatusCallsDeclineOptionalLocks(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	scanned := 0
	var offenders []string

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Skip this file: it necessarily contains the offending pattern as a
		// regexp literal, and a probe that fails on itself is useless.
		if name == "git_optional_locks_conformance_test.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if bareGitStatus.MatchString(line) {
				offenders = append(offenders,
					filepath.Join(".", name)+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}

	if scanned == 0 {
		// Fail loud rather than pass vacuously: a probe whose subject vanished
		// is a monitor proven only by silence.
		t.Fatal("conformance probe scanned 0 files — the probe, not the code, is broken")
	}

	if len(offenders) > 0 {
		t.Fatalf("found %d bare `git status` call(s) under a kill-on-timeout "+
			"path; these can orphan .git/index.lock and block every writer in "+
			"the repo's shared .git.\nFix: exec.CommandContext(ctx, \"git\", "+
			"\"--no-optional-locks\", \"status\", ...)\n%s",
			len(offenders), strings.Join(offenders, "\n"))
	}

	t.Logf("conformance probe OK: %d files scanned, no bare `git status`", scanned)
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

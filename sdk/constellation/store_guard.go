// store_guard.go — corruption-safe store preservation.
//
// Nothing in the kernel may destroy a SQLite store it could not read. This
// file implements the guard that a store *rebuild* path (today: only
// "cogos reindex", see internal/engine/cli_reindex.go) must run before it is
// allowed to let Open() create a fresh file in place of an existing one.
//
// The guard is deliberately NOT wired into Open() itself: Open() is also
// called on the daemon's hot boot path (cmd/cogos/providers_wire.go) to wire
// the eager-upsert indexer, and that path already degrades gracefully on a
// failed Open (logs a warning, disables eager upsert) without touching the
// file at all. Adding a bounded PRAGMA quick_check — and a possible rename —
// to every daemon boot would be a much bigger behavior and latency change
// than this guard is meant to make, and is out of scope for the reindex
// preservation rule. Only an explicit rebuild invocation pays this cost.
package constellation

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// quickCheckTimeout bounds how long PRAGMA quick_check may run against an
// existing store before PreserveCorruptStore gives up waiting and treats the
// file as failed integrity. A live constellation.db can be hundreds of MB;
// this is generous headroom for that size while still failing well short of
// a reindex invocation looking hung.
const quickCheckTimeout = 30 * time.Second

// sidecarSuffixes are the WAL-mode sidecar files that travel with a SQLite
// main file. They are preserved alongside a corrupt main file under the same
// timestamp (see preserveCorruptStore) rather than left behind or deleted:
// the WAL in particular may hold the last valid pages that never made it
// into the main file, so it is part of the evidence, not disposable.
var sidecarSuffixes = []string{"-wal", "-shm"}

// PreserveCorruptStore inspects the SQLite store file at dbPath, if any, and
// renames it — along with any WAL/SHM sidecars present at that moment — out
// of the way when it fails an integrity check or cannot be opened as a
// database at all. It never unlinks anything; a failed file is moved, not
// deleted, so it remains available as forensic evidence.
//
// The integrity check runs against a private byte-copy of dbPath (see
// snapshotForCheck), never against the live file, and a healthy result
// touches nothing at all — no rename, no sidecar interaction of any kind.
// This is deliberate, not incidental: "cogos reindex" is documented (see
// cli_reindex.go's doc comment and the drift-detection log line in
// mcp_stubs.go) as the routine remedy a user runs in another terminal WHILE
// the daemon keeps its own live WAL connection open on this same file —
// nothing stops or locks the daemon first. In that steady state the -wal/-
// shm sidecars are the daemon's live working files, not corruption leftovers,
// and an earlier version of this guard that opened the live file (or
// quarantined its sidecars) before deciding whether it was even unhealthy
// could race that connection: SQLite recreating a sidecar mid-check, then
// a later "restore" step blindly renaming over it, would silently discard
// live WAL frames — an undisclosed data-loss path on the most common
// invocation. Checking a copy sidesteps that class of race entirely: the
// live file and its sidecars are only ever touched, via plain os.Rename, at
// the point this function has already proven — from the copy — that the
// main file is corrupt and is about to be replaced anyway.
//
// Return values:
//   - preservedPath == "" and err == nil: either dbPath does not exist yet,
//     or the existing file is healthy. Nothing was touched; the caller's
//     normal open-or-create behavior proceeds unchanged.
//   - preservedPath != "": an existing file failed the check and was
//     renamed to preservedPath. reason explains why (quick_check's report,
//     an "empty file" finding, or the underlying open/scan error) — the
//     caller should log both loudly before rebuilding fresh.
//   - err != nil: a filesystem operation itself failed (stat/snapshot/
//     rename). This is distinct from a corruption finding, which is always
//     reported through preservedPath/reason with err == nil.
func PreserveCorruptStore(dbPath string) (preservedPath string, reason string, err error) {
	info, statErr := os.Stat(dbPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", "", nil // nothing to preserve — fresh build proceeds normally
		}
		return "", "", fmt.Errorf("stat %s: %w", dbPath, statErr)
	}

	// A zero-byte file is treated as failed integrity even though SQLite
	// itself would happily accept it as a valid empty database: an existing
	// 0-byte file at a store path is much more likely to be the leftovers
	// of an interrupted write than a deliberately-created empty store (a
	// fresh workspace has no file at all — Open creates it lazily), and
	// there is nothing lost by routing it through the same
	// preserve-and-rebuild path as any other unreadable file. No snapshot
	// is needed to reach that verdict.
	var checkErr error
	if info.Size() == 0 {
		checkErr = fmt.Errorf("store file is empty (0 bytes)")
	} else {
		snapshot, snapErr := snapshotForCheck(dbPath)
		if snapErr != nil {
			return "", "", fmt.Errorf("snapshot %s for integrity check: %w", dbPath, snapErr)
		}
		// snapshot is our own private scratch copy, never anything the user
		// owns — os.Remove here is unrelated to the "never unlink the store"
		// rule, which is about dbPath itself.
		defer os.Remove(snapshot)
		checkErr = checkStoreIntegrity(snapshot)
	}

	if checkErr == nil {
		return "", "", nil // healthy — nothing was touched
	}

	target, renameErr := preserveCorruptStore(dbPath)
	if renameErr != nil {
		return "", "", fmt.Errorf("preserve corrupt store %s (integrity failure: %v): %w", dbPath, checkErr, renameErr)
	}
	return target, checkErr.Error(), nil
}

// snapshotForCheck copies dbPath's current bytes to a private temp file in
// the same directory and returns its path; the caller is responsible for
// removing it. Copying (rather than opening dbPath directly, even
// read-only) means the integrity check never touches the live file or its
// sidecars — see the PreserveCorruptStore doc comment for why that matters
// against a concurrently open daemon connection. The plain os.Open+io.Copy
// used here takes no SQLite-level locks, so it cannot contend with one
// either.
//
// Trade-off, accepted deliberately: a copy of a large store (a live
// constellation.db can be hundreds of MB) costs extra I/O and disk space for
// the moment the copy exists, and a copy taken mid-write could in principle
// capture a torn snapshot that fails quick_check even though the live store
// is fine. Both failure directions land on the safe side of the rule this
// guard exists to enforce: a spurious "corrupt" verdict costs an unnecessary
// preserve-and-rebuild cycle, never data loss.
func snapshotForCheck(dbPath string) (string, error) {
	src, err := os.Open(dbPath)
	if err != nil {
		return "", fmt.Errorf("open %s for snapshot: %w", dbPath, err)
	}
	defer src.Close()

	dst, err := os.CreateTemp(filepath.Dir(dbPath), ".reindex-integrity-check-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create snapshot temp file: %w", err)
	}
	tmpPath := dst.Name()

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("copy %s to snapshot: %w", dbPath, err)
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("close snapshot file: %w", err)
	}
	return tmpPath, nil
}

// checkStoreIntegrity opens path read-only and runs a bounded
// PRAGMA quick_check. It returns nil only when the file opens cleanly and
// quick_check reports "ok". Always called against a snapshotForCheck copy,
// never the live store file — see the PreserveCorruptStore doc comment.
func checkStoreIntegrity(path string) error {
	db, err := sql.Open("sqlite3", path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("open read-only: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), quickCheckTimeout)
	defer cancel()

	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("cannot open as a database: %w", err)
	}
	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("quick_check reported corruption: %s", result)
	}
	return nil
}

// preserveCorruptStore renames dbPath — and any -wal/-shm sidecars present
// at this moment — to "<original name>.corrupt-<UTC timestamp>", appending
// a numeric suffix if that exact name is already taken. All files in the
// group share one timestamp (computed once) so the preserved set reads as
// one coherent snapshot; each file's target name is resolved independently
// so a same-second collision on just the sidecar doesn't block preserving
// the main file. Only reached once PreserveCorruptStore has already proven,
// from an isolated copy, that dbPath is corrupt — so this is a single
// forward-only move per file (rename to its final resting place), never a
// quarantine-then-restore round trip that could race a concurrent writer.
func preserveCorruptStore(dbPath string) (string, error) {
	ts := time.Now().UTC().Format("20060102T150405Z")

	mainTarget, err := renameAside(dbPath, ts)
	if err != nil {
		return "", err
	}

	for _, suffix := range sidecarSuffixes {
		sidecar := dbPath + suffix
		if _, statErr := os.Stat(sidecar); statErr != nil {
			continue // sidecar not present right now — nothing to preserve
		}
		if _, err := renameAside(sidecar, ts); err != nil {
			return mainTarget, fmt.Errorf("main file preserved at %s, but sidecar %s: %w", mainTarget, sidecar, err)
		}
	}

	return mainTarget, nil
}

// renameAside renames path to "<path>.corrupt-<ts>", appending "-1", "-2",
// ... if that exact name already exists, so a collision bumps rather than
// silently overwriting whatever preserved corpse is already there. Always
// uses os.Rename — never os.Remove — so the original bytes are preserved,
// not deleted.
func renameAside(path, ts string) (string, error) {
	base := path + ".corrupt-" + ts
	target := base
	for n := 1; ; n++ {
		if _, err := os.Stat(target); os.IsNotExist(err) {
			break
		}
		target = fmt.Sprintf("%s-%d", base, n)
	}
	if err := os.Rename(path, target); err != nil {
		return "", fmt.Errorf("rename %s -> %s: %w", path, target, err)
	}
	return target, nil
}

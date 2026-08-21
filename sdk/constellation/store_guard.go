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
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// quickCheckTimeout bounds how long PRAGMA quick_check may run against an
// existing store before PreserveCorruptStore gives up waiting. A live
// constellation.db can be hundreds of MB; this is generous headroom for that
// size while still failing well short of a reindex invocation looking hung.
// A timeout is deliberately NOT treated as a corruption finding — see
// PreserveCorruptStore.
//
// A var, not a const, solely so tests can deterministically force a timeout
// (see store_guard_test.go) without depending on machine speed or a
// multi-hundred-MB fixture.
var quickCheckTimeout = 30 * time.Second

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
// The integrity check opens dbPath directly through the SQLite driver
// (mode=ro, WAL-aware) rather than copying its bytes first. This is a
// deliberate choice, not an oversight: cogos reindex is documented (see
// cli_reindex.go's doc comment and the drift-detection log line in
// mcp_stubs.go) as the routine remedy a user runs in another terminal WHILE
// the daemon keeps its own live WAL connection open on this same file — a
// plain os.Open+io.Copy of a file being concurrently checkpointed can
// capture a torn, internally-inconsistent snapshot that quick_check flags as
// corrupt even though the live store is perfectly healthy (confirmed via a
// live-connection repro during review of an earlier version of this file).
// SQLite's own WAL-aware read path does not have that problem — a read-only
// connection gets the same MVCC-consistent view any other concurrent reader
// would, which is exactly why mcp_stubs.go's searchMemoryFTSDriftRepair
// already reads the live store the same way. A quick_check TIMEOUT is
// likewise never treated as a corruption finding, for the identical reason:
// folding "we couldn't determine" into "confirmed corrupt" would let a
// slow-but-healthy check (large store, loaded disk) destroy a live good
// store just as surely as a false-positive read would.
//
// Accepted trade-off: opening a store directly for the check means that if
// the main file IS genuinely corrupt and has stale, non-matching WAL/SHM
// sidecars sitting next to it, SQLite's own recovery logic may reclaim
// those sidecars as an intrinsic side effect of the check itself, before
// this function gets a chance to preserve them (preserveCorruptStore simply
// finds nothing left to rename in that case — it does not error). This is
// scoped tightly: it can only happen once the check has already determined
// the main file itself is corrupt, and a live daemon's LEGITIMATE, currently
// -matching sidecars are unaffected (a healthy main file's real sidecars
// survive a concurrent read-only check untouched — verified empirically).
// The main corrupt file itself is always still preserved; only a stale,
// already-orphaned sidecar can be lost, never live good data.
//
// Return values:
//   - preservedPath == "" and err == nil: either dbPath does not exist yet,
//     or the existing file is healthy. Nothing was renamed; the caller's
//     normal open-or-create behavior proceeds unchanged.
//   - preservedPath != "": an existing file failed the check and was
//     renamed to preservedPath. reason explains why (quick_check's report,
//     an "empty file" finding, or the underlying open/scan error) — the
//     caller should log both loudly before rebuilding fresh.
//   - err != nil: either a filesystem operation itself failed (stat/rename),
//     or the check could not reach a verdict at all (timeout) — both are
//     operational failures, distinct from a corruption finding, and neither
//     touches the file.
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
	// preserve-and-rebuild path as any other unreadable file. No check is
	// needed to reach that verdict.
	var checkErr error
	if info.Size() == 0 {
		checkErr = fmt.Errorf("store file is empty (0 bytes)")
	} else {
		checkErr = checkStoreIntegrity(dbPath)
		if checkErr != nil && errors.Is(checkErr, context.DeadlineExceeded) {
			return "", "", fmt.Errorf("integrity check for %s did not complete within %s: %w", dbPath, quickCheckTimeout, checkErr)
		}
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

// checkStoreIntegrity opens path read-only (through the SQLite driver, WAL-
// aware — see the PreserveCorruptStore doc comment for why this must be a
// direct open rather than a copy) and runs a bounded PRAGMA quick_check. It
// returns nil only when the file opens cleanly and quick_check reports "ok".
// A context-deadline error is returned like any other — callers that need
// to treat a timeout differently from a genuine corruption finding (as
// PreserveCorruptStore does) check the returned error with errors.Is against
// context.DeadlineExceeded.
func checkStoreIntegrity(path string) error {
	db, err := sql.Open("sqlite3", path+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000")
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
// the main file. Only reached once PreserveCorruptStore has already
// determined dbPath is corrupt — this is a single forward-only move per
// file (rename to its final resting place), never a quarantine-then-restore
// round trip that could race a concurrent writer.
func preserveCorruptStore(dbPath string) (string, error) {
	ts := time.Now().UTC().Format("20060102T150405Z")

	mainTarget, err := renameAside(dbPath, ts)
	if err != nil {
		return "", err
	}

	for _, suffix := range sidecarSuffixes {
		sidecar := dbPath + suffix
		if _, statErr := os.Stat(sidecar); statErr != nil {
			continue // not present right now — nothing to preserve (see the
			// PreserveCorruptStore doc comment's accepted-trade-off note)
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

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
	"os"
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
const (
	walSuffix = "-wal"
	shmSuffix = "-shm"
)

var sidecarSuffixes = []string{walSuffix, shmSuffix}

// quarantineTag marks a sidecar file moved out of the way while its main
// file's integrity is being checked (see quarantineSidecars). It is always
// resolved back to either the original name (healthy — restoreSidecars) or
// a final ".corrupt-<ts>" name (corrupt — preserveCorruptStore) before
// PreserveCorruptStore returns; it should never be visible afterward.
const quarantineTag = ".guard-tmp"

// sidecarQuarantine records where one sidecar file (identified by suffix,
// e.g. "-wal") was moved to during quarantine, or that it was not present at
// all (quarantinedPath == "").
type sidecarQuarantine struct {
	suffix          string
	quarantinedPath string
}

// PreserveCorruptStore inspects the SQLite store file at dbPath, if any, and
// renames it — along with any WAL/SHM sidecars — out of the way when it
// fails an integrity check or cannot be opened as a database at all. It
// never unlinks anything; a failed file is moved, not deleted, so it remains
// available as forensic evidence.
//
// Before running the check, any existing WAL/SHM sidecars are quarantined
// (renamed aside) first: opening even a read-only connection against a main
// file with mismatched or unreadable sidecars can make SQLite silently
// unlink them as part of its own WAL-recovery/consistency handling —
// observed empirically against this exact driver (see the sidecar test in
// store_guard_test.go). Quarantining first means the integrity check never
// has sidecars in place to react to; they are restored unchanged afterward
// if the store turns out healthy, or carried into the final preserved
// filenames if it does not — either way nothing already on disk before this
// function ran is ever silently deleted.
//
// Return values:
//   - preservedPath == "" and err == nil: either dbPath does not exist yet,
//     or the existing file is healthy. No action was taken beyond the
//     transient quarantine-and-restore of any sidecars; the caller's normal
//     open-or-create behavior proceeds unchanged.
//   - preservedPath != "": an existing file failed the check and was
//     renamed to preservedPath. reason explains why (quick_check's report,
//     an "empty file" finding, or the underlying open/scan error) — the
//     caller should log both loudly before rebuilding fresh.
//   - err != nil: a filesystem operation itself failed (stat/rename).
//     This is distinct from a corruption finding, which is always reported
//     through preservedPath/reason with err == nil.
func PreserveCorruptStore(dbPath string) (preservedPath string, reason string, err error) {
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return "", "", nil // nothing to preserve — fresh build proceeds normally
		}
		return "", "", fmt.Errorf("stat %s: %w", dbPath, statErr)
	}

	quarantined, qerr := quarantineSidecars(dbPath)
	if qerr != nil {
		return "", "", fmt.Errorf("quarantine sidecars for %s: %w", dbPath, qerr)
	}

	checkErr := checkStoreIntegrity(dbPath)
	if checkErr == nil {
		if restoreErr := restoreSidecars(quarantined); restoreErr != nil {
			return "", "", fmt.Errorf("restore sidecars for healthy store %s: %w", dbPath, restoreErr)
		}
		return "", "", nil // healthy — replacement behavior unchanged
	}

	target, renameErr := preserveCorruptStore(dbPath, quarantined)
	if renameErr != nil {
		return "", "", fmt.Errorf("preserve corrupt store %s (integrity failure: %v): %w", dbPath, checkErr, renameErr)
	}
	return target, checkErr.Error(), nil
}

// checkStoreIntegrity opens dbPath read-only and runs a bounded
// PRAGMA quick_check. It returns nil only when the file opens cleanly and
// quick_check reports "ok". Callers must have already quarantined any
// WAL/SHM sidecars before calling this (see the PreserveCorruptStore doc
// comment for why).
//
// A zero-byte file is treated as failed integrity even though SQLite itself
// would happily accept it as a valid empty database: an existing 0-byte file
// at a store path is much more likely to be the leftovers of an interrupted
// write than a deliberately-created empty store (a fresh workspace has no
// file at all — Open creates it lazily), and there is nothing lost by
// routing it through the same preserve-and-rebuild path as any other
// unreadable file.
func checkStoreIntegrity(dbPath string) error {
	info, err := os.Stat(dbPath)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("store file is empty (0 bytes)")
	}

	db, err := sql.Open("sqlite3", dbPath+"?mode=ro&_busy_timeout=5000")
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

// quarantineSidecars renames any existing "<dbPath>-wal"/"<dbPath>-shm" out
// of the way (to "<original>.guard-tmp") before the main file is opened for
// its integrity check. If renaming a later sidecar fails, everything already
// quarantined in this call is best-effort restored before returning the
// error, so a partial failure never leaves a sidecar stranded under a
// ".guard-tmp" name.
func quarantineSidecars(dbPath string) ([]sidecarQuarantine, error) {
	done := make([]sidecarQuarantine, 0, len(sidecarSuffixes))
	for _, suffix := range sidecarSuffixes {
		orig := dbPath + suffix
		if _, statErr := os.Stat(orig); statErr != nil {
			done = append(done, sidecarQuarantine{suffix: suffix})
			continue
		}
		tmp := orig + quarantineTag
		if err := os.Rename(orig, tmp); err != nil {
			_ = restoreSidecars(done) // best-effort — we're already failing
			return nil, fmt.Errorf("rename %s -> %s: %w", orig, tmp, err)
		}
		done = append(done, sidecarQuarantine{suffix: suffix, quarantinedPath: tmp})
	}
	return done, nil
}

// restoreSidecars renames every quarantined sidecar back to its original
// name. Called when the main file turns out healthy, so the sidecars —
// untouched throughout — end up exactly where they started.
func restoreSidecars(qs []sidecarQuarantine) error {
	for _, q := range qs {
		if q.quarantinedPath == "" {
			continue
		}
		orig := strings.TrimSuffix(q.quarantinedPath, quarantineTag)
		if err := os.Rename(q.quarantinedPath, orig); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", q.quarantinedPath, orig, err)
		}
	}
	return nil
}

// preserveCorruptStore renames dbPath — and any quarantined -wal/-shm
// sidecars — to "<original name>.corrupt-<UTC timestamp>", appending a
// numeric suffix if that exact name is already taken. All files in the
// group share one timestamp (computed once) so the preserved set reads as
// one coherent snapshot; each file's target name is resolved independently
// so a same-second collision on just the sidecar doesn't block preserving
// the main file.
func preserveCorruptStore(dbPath string, quarantined []sidecarQuarantine) (string, error) {
	ts := time.Now().UTC().Format("20060102T150405Z")

	mainTarget, err := renameAside(dbPath, dbPath, ts)
	if err != nil {
		_ = restoreSidecars(quarantined) // bail cleanly — nothing was preserved
		return "", err
	}

	for _, q := range quarantined {
		if q.quarantinedPath == "" {
			continue
		}
		origSidecar := strings.TrimSuffix(q.quarantinedPath, quarantineTag)
		if _, err := renameAside(q.quarantinedPath, origSidecar, ts); err != nil {
			return mainTarget, fmt.Errorf("main file preserved at %s, but sidecar %s: %w", mainTarget, origSidecar, err)
		}
	}

	return mainTarget, nil
}

// renameAside renames src (the file's current location — which may be a
// quarantined temp path) to "<targetBase>.corrupt-<ts>", appending "-1",
// "-2", ... if that name is already taken. It always uses os.Rename — never
// os.Remove — so the original bytes are preserved, not deleted.
func renameAside(src, targetBase, ts string) (string, error) {
	base := targetBase + ".corrupt-" + ts
	target := base
	for n := 1; ; n++ {
		if _, err := os.Stat(target); os.IsNotExist(err) {
			break
		}
		target = fmt.Sprintf("%s-%d", base, n)
	}
	if err := os.Rename(src, target); err != nil {
		return "", fmt.Errorf("rename %s -> %s: %w", src, target, err)
	}
	return target, nil
}

package constellation

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeSmallSQLiteDB creates a real, valid SQLite database file at path with a
// tiny table + row. This deliberately does NOT go through Open/initSchema —
// it needs no fts5 module, so these tests exercise the store guard's
// filesystem/PRAGMA-level logic without the -tags fts5 requirement that
// applies to the full constellation schema (see the FTS5 skip pattern used
// elsewhere in this package's tests, e.g. embed_test.go's openTestDB).
func makeSmallSQLiteDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE probe (id INTEGER PRIMARY KEY, val TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO probe (val) VALUES ('hello')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Force a checkpoint so the data lands in the main file, not just the WAL
	// — corruption tests below need to corrupt bytes that are actually in the
	// main file.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

// corruptMidFile overwrites bytes starting partway through the file with
// garbage, simulating the kind of mid-file corruption a crash or partial
// write can leave behind (as opposed to a truncated or zero-length file).
func corruptMidFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) < 32 {
		t.Fatalf("file too small to corrupt meaningfully: %d bytes", len(data))
	}
	garbage := bytes.Repeat([]byte{0xFF}, 16)
	copy(data[16:32], garbage)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write corrupted %s: %v", path, err)
	}
}

func TestPreserveCorruptStore_NoExistingFile(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")

	preserved, reason, err := PreserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if preserved != "" || reason != "" {
		t.Fatalf("expected no-op for nonexistent file, got preserved=%q reason=%q", preserved, reason)
	}
}

func TestPreserveCorruptStore_HealthyFileLeftAlone(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")
	makeSmallSQLiteDB(t, dbPath)

	preserved, reason, err := PreserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if preserved != "" || reason != "" {
		t.Fatalf("expected no-op for healthy file, got preserved=%q reason=%q", preserved, reason)
	}

	// The file itself must be untouched — same path, still openable, still
	// has our row.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("healthy file was moved/removed: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(dbPath) {
			t.Fatalf("unexpected extra file for healthy store: %s", e.Name())
		}
	}
}

func TestPreserveCorruptStore_CorruptedFileRenamedByteIdentical(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")
	makeSmallSQLiteDB(t, dbPath)
	corruptMidFile(t, dbPath)

	wantBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read corrupted fixture: %v", err)
	}

	preserved, reason, err := PreserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if preserved == "" {
		t.Fatalf("expected corrupted file to be preserved, got no-op (reason=%q)", reason)
	}
	if reason == "" {
		t.Fatalf("expected a non-empty reason for corruption")
	}

	// Original path must no longer exist (it was renamed, not copied) —
	// the caller's subsequent Open() must be free to create a fresh file
	// there.
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("original path %s should no longer exist after rename, stat err = %v", dbPath, err)
	}

	if !filepath.IsAbs(preserved) {
		t.Fatalf("preserved path should be absolute like the input, got %q", preserved)
	}
	gotBytes, err := os.ReadFile(preserved)
	if err != nil {
		t.Fatalf("read preserved file %s: %v", preserved, err)
	}
	if !bytes.Equal(wantBytes, gotBytes) {
		t.Fatalf("preserved file contents differ from the corrupted original (renamed, not copied — must be byte-identical)")
	}

	// Must be a rename (same inode story doesn't apply cross-platform, but a
	// second call against a fresh healthy file at the original path must
	// not disturb the already-preserved corpse).
	makeSmallSQLiteDB(t, dbPath)
	preserved2, reason2, err := PreserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("unexpected error on rebuild pass: %v", err)
	}
	if preserved2 != "" || reason2 != "" {
		t.Fatalf("fresh healthy rebuild should not be re-preserved, got preserved=%q reason=%q", preserved2, reason2)
	}
	if _, err := os.Stat(preserved); err != nil {
		t.Fatalf("original preserved corpse disappeared: %v", err)
	}
}

func TestPreserveCorruptStore_UnopenableFile(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
	}{
		{"zero-byte", []byte{}},
		{"not-a-database", []byte("this is not a sqlite file, just plain text garbage\n")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dbPath := filepath.Join(root, "constellation.db")
			if err := os.WriteFile(dbPath, tc.content, 0644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}

			preserved, reason, err := PreserveCorruptStore(dbPath)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if preserved == "" {
				t.Fatalf("expected %s to be preserved as unopenable, got no-op", tc.name)
			}
			if reason == "" {
				t.Fatalf("expected a non-empty reason")
			}
			if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
				t.Fatalf("original path should no longer exist, stat err = %v", err)
			}
			got, err := os.ReadFile(preserved)
			if err != nil {
				t.Fatalf("read preserved file: %v", err)
			}
			if !bytes.Equal(got, tc.content) {
				t.Fatalf("preserved content mismatch: got %q want %q", got, tc.content)
			}
		})
	}
}

func TestPreserveCorruptStore_CollisionAppendsNumericSuffix(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")
	makeSmallSQLiteDB(t, dbPath)
	corruptMidFile(t, dbPath)

	// Pre-create the exact target name preserveCorruptStore would pick by
	// calling the (unexported) rename helper twice with a fixed timestamp,
	// simulating a same-second collision deterministically instead of
	// relying on two real calls landing in the same wall-clock second.
	const ts = "20260101T000000Z"
	collision := dbPath + ".corrupt-" + ts
	if err := os.WriteFile(collision, []byte("pre-existing corpse from an earlier run"), 0644); err != nil {
		t.Fatalf("seed collision file: %v", err)
	}

	target, err := renameAside(dbPath, ts)
	if err != nil {
		t.Fatalf("renameAside: %v", err)
	}
	if target != collision+"-1" {
		t.Fatalf("expected numeric-suffixed target %s, got %s", collision+"-1", target)
	}
	if _, err := os.Stat(collision); err != nil {
		t.Fatalf("pre-existing collision file should be untouched: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("suffixed target should exist: %v", err)
	}

	// A second collision (against both the original target and the "-1"
	// bump just created) must bump again to "-2".
	makeSmallSQLiteDB(t, dbPath)
	corruptMidFile(t, dbPath)
	target2, err := renameAside(dbPath, ts)
	if err != nil {
		t.Fatalf("renameAside second call: %v", err)
	}
	if target2 != collision+"-2" {
		t.Fatalf("expected second collision to bump to %s, got %s", collision+"-2", target2)
	}
}

// TestPreserveCorruptStore_CollisionViaPublicAPI drives the same collision
// path through PreserveCorruptStore itself (not the unexported rename
// helper), so the numeric-suffix behavior is also verified through the
// entry point callers actually use.
func TestPreserveCorruptStore_CollisionViaPublicAPI(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")

	makeSmallSQLiteDB(t, dbPath)
	corruptMidFile(t, dbPath)
	preserved1, _, err := PreserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("first preserve: %v", err)
	}

	makeSmallSQLiteDB(t, dbPath)
	corruptMidFile(t, dbPath)
	preserved2, _, err := PreserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("second preserve: %v", err)
	}

	if preserved1 == preserved2 {
		t.Fatalf("two corrupted-then-preserved files landed at the same path %q — a collision must not overwrite", preserved1)
	}
	for _, p := range []string{preserved1, preserved2} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected preserved corpse to survive at %s: %v", p, err)
		}
	}
}

// TestPreserveCorruptStore_SidecarRenameMechanism_Direct unit-tests the
// rename-loop in preserveCorruptStore directly (bypassing the public
// PreserveCorruptStore entry point and its checkStoreIntegrity call), so the
// "rename every sidecar present at this moment, under a shared timestamp,
// coherently" mechanism has coverage independent of whether a given
// corrupt-file scenario happens to leave sidecars in place by the time this
// step runs (see the next test and the PreserveCorruptStore doc comment's
// "Accepted trade-off" paragraph for why that varies).
func TestPreserveCorruptStore_SidecarRenameMechanism_Direct(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")
	if err := os.WriteFile(dbPath, []byte("main file bytes"), 0644); err != nil {
		t.Fatalf("write main fixture: %v", err)
	}
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	if err := os.WriteFile(walPath, []byte("wal frames from before the crash"), 0644); err != nil {
		t.Fatalf("write wal fixture: %v", err)
	}
	if err := os.WriteFile(shmPath, []byte("shm bytes"), 0644); err != nil {
		t.Fatalf("write shm fixture: %v", err)
	}

	preserved, err := preserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("preserveCorruptStore: %v", err)
	}

	if _, err := os.Stat(walPath); !os.IsNotExist(err) {
		t.Fatalf("wal sidecar should have been renamed away, stat err = %v", err)
	}
	if _, err := os.Stat(shmPath); !os.IsNotExist(err) {
		t.Fatalf("shm sidecar should have been renamed away, stat err = %v", err)
	}

	// Preserved under the SAME timestamp suffix as the main file, so the
	// group reads as one coherent snapshot.
	suffix := preserved[len(dbPath):] // e.g. ".corrupt-20260101T000000Z" or with a numeric bump
	gotWal, err := os.ReadFile(walPath + suffix)
	if err != nil {
		t.Fatalf("expected wal sidecar preserved at %s: %v", walPath+suffix, err)
	}
	if string(gotWal) != "wal frames from before the crash" {
		t.Fatalf("preserved wal content mismatch: got %q", gotWal)
	}
	gotShm, err := os.ReadFile(shmPath + suffix)
	if err != nil {
		t.Fatalf("expected shm sidecar preserved at %s: %v", shmPath+suffix, err)
	}
	if string(gotShm) != "shm bytes" {
		t.Fatalf("preserved shm content mismatch: got %q", gotShm)
	}
}

// TestPreserveCorruptStore_StaleSidecarsMayNotSurviveConfirmedCorruption
// documents, as a passing test rather than an unstated assumption, the
// trade-off called out in PreserveCorruptStore's doc comment: opening a
// genuinely corrupt main file directly (required to avoid the torn-read
// false positives a copy-based check would produce — see that doc comment)
// can make the SQLite driver itself reclaim non-matching/stale WAL/SHM
// sidecars as an intrinsic side effect of the failed open, before
// preserveCorruptStore ever runs. This is a real, empirically-observed
// behavior (confirmed against this exact driver with a garbage main file
// and garbage sidecars), not a hypothetical. The main corrupt file is still
// always preserved regardless — only a stale, already-orphaned sidecar can
// be lost, and only in the already-confirmed-corrupt case; a healthy store's
// legitimate sidecars are a completely different code path — see
// TestPreserveCorruptStore_LeavesLiveWALConnectionUndisturbed below, which
// is not affected by this.
func TestPreserveCorruptStore_StaleSidecarsMayNotSurviveConfirmedCorruption(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")
	makeSmallSQLiteDB(t, dbPath)

	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	if err := os.WriteFile(walPath, []byte("stale wal frames from before the crash"), 0644); err != nil {
		t.Fatalf("write wal fixture: %v", err)
	}
	if err := os.WriteFile(shmPath, []byte("stale shm bytes"), 0644); err != nil {
		t.Fatalf("write shm fixture: %v", err)
	}
	corruptMidFile(t, dbPath)

	preserved, reason, err := PreserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if preserved == "" {
		t.Fatalf("expected the corrupt main file to be preserved, reason=%q", reason)
	}

	// The main file — the thing this whole guard exists to protect — is
	// always preserved and byte-identical, regardless of what happens to
	// the sidecars.
	if _, err := os.Stat(preserved); err != nil {
		t.Fatalf("preserved main file missing: %v", err)
	}

	// The stale sidecars are gone from their original names either way
	// (reclaimed by the driver during the check, or renamed away by
	// preserveCorruptStore if they happened to survive that long) — assert
	// only what's actually guaranteed: nothing is left dangling at the
	// original sidecar names, and no name in the directory holds anything
	// OTHER than the fixture's own content (i.e. nothing was silently
	// overwritten with garbage).
	if _, err := os.Stat(walPath); !os.IsNotExist(err) {
		t.Fatalf("wal sidecar should not remain at its original name, stat err = %v", err)
	}
	if _, err := os.Stat(shmPath); !os.IsNotExist(err) {
		t.Fatalf("shm sidecar should not remain at its original name, stat err = %v", err)
	}
}

// Note: there is deliberately no "healthy main file + hand-written
// fake/mismatched -wal/-shm fixture files" test here. That combination
// doesn't represent a real scenario reindex has to handle — a real WAL
// sidecar only exists because SOME live or crashed SQLite connection wrote
// it, and checkStoreIntegrity's direct open goes through the same SQLite
// machinery that connection used, so a genuinely mismatched pair is
// something SQLite itself would already treat as inconsistent regardless of
// what this guard does. The realistic "healthy main file with real,
// currently-matching sidecars" case — a live daemon connection — is covered
// by TestPreserveCorruptStore_LeavesLiveWALConnectionUndisturbed below, and
// "healthy main file with no sidecars at all" by
// TestPreserveCorruptStore_HealthyFileLeftAlone above.

// TestPreserveCorruptStore_LeavesLiveWALConnectionUndisturbed is the direct
// regression test for the concurrency hazard this design was changed to
// eliminate: cogos reindex is documented (cli_reindex.go, and the
// drift-detection log line in mcp_stubs.go) as the routine remedy a user
// runs in another terminal WHILE the daemon keeps its own live WAL
// connection open on the same store — nothing stops or locks it first. This
// test holds a real, live WAL-mode connection open (standing in for the
// daemon) across a PreserveCorruptStore call against the same healthy file,
// and checks two levels: the -wal file's bytes — the actual data-bearing
// sidecar, holding committed frames not yet checkpointed into the main file
// — are byte-identical on disk before and after (so a future change that
// made the read-only check checkpoint/truncate/rewrite it would be caught,
// even though the resulting bytes might still parse as a valid WAL); and
// the live connection itself keeps working afterward. (The -shm file is
// deliberately NOT compared byte-for-byte: it is SQLite's shared
// WAL-index/read-mark bookkeeping, which every reader connection that joins
// a WAL-mode database legitimately updates as part of normal MVCC
// coordination — a second connection's read-only quick_check touching it is
// expected, harmless behavior, confirmed empirically, not evidence of
// anything being disturbed.)
func TestPreserveCorruptStore_LeavesLiveWALConnectionUndisturbed(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")

	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open live connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE probe (id INTEGER PRIMARY KEY, val TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO probe (val) VALUES ('before')`); err != nil {
		t.Fatalf("insert before: %v", err)
	}

	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	walBefore, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read wal sidecar before check: %v", err)
	}
	if _, err := os.Stat(shmPath); err != nil {
		t.Fatalf("shm sidecar missing before check: %v", err)
	}

	preserved, reason, err := PreserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if preserved != "" || reason != "" {
		t.Fatalf("expected no-op against a live healthy connection, got preserved=%q reason=%q", preserved, reason)
	}

	// The wal file's bytes on disk must be byte-identical to what they were
	// right before the check — not merely present, but untouched.
	walAfter, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read wal sidecar after check: %v", err)
	}
	if !bytes.Equal(walBefore, walAfter) {
		t.Fatalf("wal sidecar bytes changed across a healthy check against a live connection (before %d bytes, after %d bytes)", len(walBefore), len(walAfter))
	}
	// The shm file is only checked for presence (see the doc comment above
	// for why its bytes are expected to legitimately change).
	if _, err := os.Stat(shmPath); err != nil {
		t.Fatalf("shm sidecar missing after check: %v", err)
	}

	// The live connection must still work: a fresh write and read through
	// the SAME still-open handle must succeed.
	if _, err := db.Exec(`INSERT INTO probe (val) VALUES ('after')`); err != nil {
		t.Fatalf("insert after PreserveCorruptStore: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM probe`).Scan(&count); err != nil {
		t.Fatalf("select after PreserveCorruptStore: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows (before + after), got %d", count)
	}
}

// TestPreserveCorruptStore_TimeoutIsNotTreatedAsCorruption is the direct
// regression test for the other concurrency-adjacent hazard raised in
// review: a quick_check that does not finish within quickCheckTimeout (a
// large store, a loaded disk from a concurrent daemon writing to the same
// volume) must NOT be folded into "confirmed corrupt" — that would destroy
// a store this function was simply unable to finish reading in time, the
// same failure shape as a false-positive read. A timeout must surface as an
// operational error instead, with the store left completely untouched.
func TestPreserveCorruptStore_TimeoutIsNotTreatedAsCorruption(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")
	makeSmallSQLiteDB(t, dbPath)

	prevTimeout := quickCheckTimeout
	quickCheckTimeout = 1 * time.Nanosecond // force a deadline-exceeded verdict deterministically
	t.Cleanup(func() { quickCheckTimeout = prevTimeout })

	preserved, reason, err := PreserveCorruptStore(dbPath)
	if err == nil {
		t.Fatalf("expected a timeout error, got preserved=%q reason=%q err=nil", preserved, reason)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected err to wrap context.DeadlineExceeded, got: %v", err)
	}
	// Must NOT be reported as a corruption finding: preservedPath/reason
	// stay empty, the error is the only signal.
	if preserved != "" || reason != "" {
		t.Fatalf("timeout must not be reported as a corruption finding, got preserved=%q reason=%q", preserved, reason)
	}

	// The healthy file itself must be completely untouched.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("healthy file should be untouched after a timeout, stat err = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			t.Fatalf("unexpected .corrupt-* file after a timeout (not a corruption finding): %s", e.Name())
		}
	}
}

// TestIsInconclusive is a direct unit test of the classifier that decides
// whether a checkStoreIntegrity error means "couldn't determine" (timeout,
// busy, locked) versus "confirmed corrupt" (everything else). This is the
// exact distinction review round 4 found incomplete: only context.
// DeadlineExceeded was excluded, leaving SQLITE_BUSY/SQLITE_LOCKED (e.g.
// from a daemon mid-checkpoint briefly holding a lock) to fall through to
// "confirmed corrupt" and destroy a live healthy store.
func TestIsInconclusive(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"deadline exceeded direct", context.DeadlineExceeded, true},
		{"deadline exceeded wrapped", fmt.Errorf("cannot open as a database: %w", context.DeadlineExceeded), true},
		// Message text as sqlite3_errstr actually produces it for these
		// codes (see the driver's error.go) -- SQLITE_BUSY -> "database is
		// locked", SQLITE_LOCKED -> "database table is locked". Matched by
		// substring rather than the driver's typed sqlite3.Error, which
		// lives in cgo-gated source unavailable on this module's
		// CGO_ENABLED=0 Windows cross-compile CI job (see isInconclusive's
		// doc comment).
		{"busy message", errors.New("database is locked"), true},
		{"busy message wrapped", fmt.Errorf("cannot open as a database: %w", errors.New("database is locked")), true},
		{"locked message (table)", errors.New("database table is locked"), true},
		{"locked message uppercase", errors.New("Database Is LOCKED"), true},
		{"not a database (genuine corruption)", errors.New("file is not a database"), false},
		{"malformed disk image (genuine corruption)", errors.New("database disk image is malformed"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isInconclusive(tc.err); got != tc.want {
				t.Fatalf("isInconclusive(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestPreserveCorruptStore_RealLockContentionIsNotTreatedAsCorruption is the
// integration-level companion to TestIsInconclusive: it forces a genuine
// SQLITE_BUSY from the real driver (not a synthetic error) by holding an
// exclusive lock on a healthy store from a second connection — standing in
// for a daemon mid-checkpoint — and confirms PreserveCorruptStore surfaces
// that as an operational error rather than renaming the healthy store away.
// PRAGMA locking_mode=EXCLUSIVE makes the lock deterministic (taken on next
// access, held until released) so this needs no timing-sensitive retry loop
// and no multi-second sleep — busyTimeout is forced low purely so
// checkStoreIntegrity's own connection gives up quickly instead of waiting
// out the production default.
func TestPreserveCorruptStore_RealLockContentionIsNotTreatedAsCorruption(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")

	writer, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`CREATE TABLE probe (id INTEGER PRIMARY KEY, val TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := writer.Exec(`PRAGMA locking_mode=EXCLUSIVE`); err != nil {
		t.Fatalf("set exclusive locking mode: %v", err)
	}
	// This statement is what actually takes and holds the exclusive lock
	// under locking_mode=EXCLUSIVE (taken on the next access, not released
	// on commit).
	if _, err := writer.Exec(`INSERT INTO probe (val) VALUES ('locked')`); err != nil {
		t.Fatalf("insert to take exclusive lock: %v", err)
	}

	prevBusyTimeout := busyTimeout
	busyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { busyTimeout = prevBusyTimeout })

	preserved, reason, err := PreserveCorruptStore(dbPath)
	if err == nil {
		t.Fatalf("expected an operational error from lock contention, got preserved=%q reason=%q err=nil", preserved, reason)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "locked") {
		t.Fatalf("expected err to mention a lock (SQLITE_BUSY's message is \"database is locked\"), got: %v", err)
	}
	if preserved != "" || reason != "" {
		t.Fatalf("lock contention must not be reported as a corruption finding, got preserved=%q reason=%q", preserved, reason)
	}

	// The healthy, locked file itself must be completely untouched.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("healthy file should be untouched after lock contention, stat err = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			t.Fatalf("unexpected .corrupt-* file after lock contention (not a corruption finding): %s", e.Name())
		}
	}
}

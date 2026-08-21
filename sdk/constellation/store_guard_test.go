package constellation

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	target, err := renameAside(dbPath, dbPath, ts)
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
	target2, err := renameAside(dbPath, dbPath, ts)
	if err != nil {
		t.Fatalf("renameAside second call: %v", err)
	}
	if target2 != collision+"-2" {
		t.Fatalf("expected second collision to bump to %s, got %s", collision+"-2", target2)
	}
}

// TestPreserveCorruptStore_QuarantineCollisionDoesNotOverwriteStranded
// exercises a narrower hazard than the ".corrupt-<ts>" collision above: a
// ".guard-tmp" quarantine name left behind by an earlier run that crashed
// mid-guard (after quarantining a sidecar, before resolving it either way)
// is itself unresolved evidence, and a later run's quarantine step must not
// silently os.Rename over it.
func TestPreserveCorruptStore_QuarantineCollisionDoesNotOverwriteStranded(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")
	makeSmallSQLiteDB(t, dbPath)

	strandedWal := dbPath + "-wal" + quarantineTag
	strandedContent := []byte("stranded wal bytes from a run that crashed mid-guard")
	if err := os.WriteFile(strandedWal, strandedContent, 0644); err != nil {
		t.Fatalf("seed stranded quarantine file: %v", err)
	}

	liveWalContent := []byte("live wal content for the current run")
	if err := os.WriteFile(dbPath+"-wal", liveWalContent, 0644); err != nil {
		t.Fatalf("write live wal fixture: %v", err)
	}
	corruptMidFile(t, dbPath)

	preserved, _, err := PreserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if preserved == "" {
		t.Fatalf("expected preservation")
	}

	// The stranded file must be untouched, byte-identical, at its original name.
	gotStranded, err := os.ReadFile(strandedWal)
	if err != nil {
		t.Fatalf("stranded quarantine file disappeared: %v", err)
	}
	if !bytes.Equal(gotStranded, strandedContent) {
		t.Fatalf("stranded quarantine file was overwritten: got %q want %q", gotStranded, strandedContent)
	}

	// The live wal must still be preserved somewhere, byte-identical, just not
	// at the collided name.
	suffix := preserved[len(dbPath):]
	walTarget := dbPath + "-wal" + suffix
	gotWal, err := os.ReadFile(walTarget)
	if err != nil {
		t.Fatalf("expected live wal preserved at %s: %v", walTarget, err)
	}
	if !bytes.Equal(gotWal, liveWalContent) {
		t.Fatalf("preserved wal content mismatch: got %q want %q", gotWal, liveWalContent)
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

func TestPreserveCorruptStore_SidecarsPreservedCoherently(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")
	makeSmallSQLiteDB(t, dbPath)

	// Simulate leftover WAL/SHM sidecars from an unclean shutdown alongside
	// a now-corrupted main file.
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	if err := os.WriteFile(walPath, []byte("wal frames from before the crash"), 0644); err != nil {
		t.Fatalf("write wal fixture: %v", err)
	}
	if err := os.WriteFile(shmPath, []byte("shm bytes"), 0644); err != nil {
		t.Fatalf("write shm fixture: %v", err)
	}
	corruptMidFile(t, dbPath)

	preserved, _, err := PreserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if preserved == "" {
		t.Fatalf("expected preservation")
	}

	// Sidecars must be gone from their original names...
	if _, err := os.Stat(walPath); !os.IsNotExist(err) {
		t.Fatalf("wal sidecar should have been renamed away, stat err = %v", err)
	}
	if _, err := os.Stat(shmPath); !os.IsNotExist(err) {
		t.Fatalf("shm sidecar should have been renamed away, stat err = %v", err)
	}
	// And no ".guard-tmp" quarantine name should be left dangling — the
	// quarantine is an internal transient, never a visible end state.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), quarantineTag) {
			t.Fatalf("dangling quarantine file left behind: %s", e.Name())
		}
	}

	// ...and preserved under the SAME timestamp suffix as the main file, so
	// the group reads as one coherent snapshot.
	suffix := preserved[len(dbPath):] // e.g. ".corrupt-20260101T000000Z" or with a numeric bump
	walTarget := walPath + suffix
	shmTarget := shmPath + suffix
	gotWal, err := os.ReadFile(walTarget)
	if err != nil {
		t.Fatalf("expected wal sidecar preserved at %s: %v", walTarget, err)
	}
	if string(gotWal) != "wal frames from before the crash" {
		t.Fatalf("preserved wal content mismatch: got %q", gotWal)
	}
	gotShm, err := os.ReadFile(shmTarget)
	if err != nil {
		t.Fatalf("expected shm sidecar preserved at %s: %v", shmTarget, err)
	}
	if string(gotShm) != "shm bytes" {
		t.Fatalf("preserved shm content mismatch: got %q", gotShm)
	}
}

// TestPreserveCorruptStore_SidecarsRestoredUnchangedWhenHealthy verifies the
// other half of the quarantine mechanism: a healthy store's sidecars survive
// the quarantine-for-checking round trip byte-identical and back at their
// original names, since PreserveCorruptStore must leave a healthy store's
// on-disk layout completely unchanged.
func TestPreserveCorruptStore_SidecarsRestoredUnchangedWhenHealthy(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "constellation.db")
	makeSmallSQLiteDB(t, dbPath)

	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	walContent := []byte("real wal-shaped bytes for the healthy-path test")
	shmContent := []byte("real shm-shaped bytes")
	if err := os.WriteFile(walPath, walContent, 0644); err != nil {
		t.Fatalf("write wal fixture: %v", err)
	}
	if err := os.WriteFile(shmPath, shmContent, 0644); err != nil {
		t.Fatalf("write shm fixture: %v", err)
	}

	preserved, reason, err := PreserveCorruptStore(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if preserved != "" || reason != "" {
		t.Fatalf("expected no-op for healthy store, got preserved=%q reason=%q", preserved, reason)
	}

	gotWal, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("wal sidecar missing after healthy check: %v", err)
	}
	if !bytes.Equal(gotWal, walContent) {
		t.Fatalf("wal sidecar content changed across a healthy check: got %q want %q", gotWal, walContent)
	}
	gotShm, err := os.ReadFile(shmPath)
	if err != nil {
		t.Fatalf("shm sidecar missing after healthy check: %v", err)
	}
	if !bytes.Equal(gotShm, shmContent) {
		t.Fatalf("shm sidecar content changed across a healthy check: got %q want %q", gotShm, shmContent)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 3 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected exactly db+wal+shm (3 files), got %v", names)
	}
}

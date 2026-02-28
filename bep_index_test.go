package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// ─── VersionVector.Compare ──────────────────────────────────────────────────────

func TestVersionVectorCompareEqual(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 3}}
	b := &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 3}}
	if got := a.Compare(b); got != OrderEqual {
		t.Errorf("Compare = %v, want Equal", got)
	}
}

func TestVersionVectorCompareGreater(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 6, 2: 3}}
	b := &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 3}}
	if got := a.Compare(b); got != OrderGreater {
		t.Errorf("Compare = %v, want Greater", got)
	}
}

func TestVersionVectorCompareLesser(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 4, 2: 3}}
	b := &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 3}}
	if got := a.Compare(b); got != OrderLesser {
		t.Errorf("Compare = %v, want Lesser", got)
	}
}

func TestVersionVectorCompareConcurrent(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 6, 2: 2}}
	b := &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 3}}
	if got := a.Compare(b); got != OrderConcurrent {
		t.Errorf("Compare = %v, want Concurrent", got)
	}
}

func TestVersionVectorCompareDisjointKeys(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 5}}
	b := &VersionVector{Counters: map[uint64]uint64{2: 5}}
	if got := a.Compare(b); got != OrderConcurrent {
		t.Errorf("Compare = %v, want Concurrent (disjoint keys)", got)
	}
}

func TestVersionVectorCompareNil(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 5}}
	if got := a.Compare(nil); got != OrderGreater {
		t.Errorf("Compare(nil) = %v, want Greater", got)
	}
}

func TestVersionVectorCompareBothEmpty(t *testing.T) {
	a := NewVersionVector()
	b := NewVersionVector()
	if got := a.Compare(b); got != OrderEqual {
		t.Errorf("Compare = %v, want Equal", got)
	}
}

func TestVersionVectorCompareEmptyVsNil(t *testing.T) {
	a := NewVersionVector()
	if got := a.Compare(nil); got != OrderEqual {
		t.Errorf("Compare(nil) for empty = %v, want Equal", got)
	}
}

// ─── VersionVector.Increment ────────────────────────────────────────────────────

func TestVersionVectorIncrement(t *testing.T) {
	v := NewVersionVector()
	val := v.Increment(42)
	if val != 1 {
		t.Errorf("first increment = %d, want 1", val)
	}
	val = v.Increment(42)
	if val != 2 {
		t.Errorf("second increment = %d, want 2", val)
	}
	if v.Counters[42] != 2 {
		t.Errorf("counter[42] = %d, want 2", v.Counters[42])
	}
}

// ─── VersionVector.Merge ────────────────────────────────────────────────────────

func TestVersionVectorMerge(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 3, 2: 5}}
	b := &VersionVector{Counters: map[uint64]uint64{1: 7, 3: 2}}
	a.Merge(b)

	if a.Counters[1] != 7 {
		t.Errorf("counter[1] = %d, want 7", a.Counters[1])
	}
	if a.Counters[2] != 5 {
		t.Errorf("counter[2] = %d, want 5", a.Counters[2])
	}
	if a.Counters[3] != 2 {
		t.Errorf("counter[3] = %d, want 2", a.Counters[3])
	}
}

func TestVersionVectorMergeNil(t *testing.T) {
	a := NewVersionVector()
	a.Counters[1] = 5
	a.Merge(nil)
	if a.Counters[1] != 5 {
		t.Error("merge nil should be no-op")
	}
}

// ─── VersionVector BEP conversion ───────────────────────────────────────────────

func TestVersionVectorBEPRoundTrip(t *testing.T) {
	orig := &VersionVector{Counters: map[uint64]uint64{10: 20, 30: 40}}
	bep := orig.ToBEP()
	got := VersionVectorFromBEP(bep)

	if len(got.Counters) != 2 {
		t.Fatalf("got %d counters, want 2", len(got.Counters))
	}
	if got.Counters[10] != 20 {
		t.Errorf("counter[10] = %d, want 20", got.Counters[10])
	}
	if got.Counters[30] != 40 {
		t.Errorf("counter[30] = %d, want 40", got.Counters[30])
	}
}

// ─── ScanLocalIndex ─────────────────────────────────────────────────────────────

func TestScanLocalIndex(t *testing.T) {
	dir := t.TempDir()

	// Write some agent CRDs.
	for _, name := range []string{"a.agent.yaml", "b.agent.yaml"} {
		data := []byte(fmt.Sprintf("apiVersion: cog.os/v1alpha1\nkind: Agent\nmetadata:\n  name: %s\n", name))
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Write a non-CRD file that should be ignored.
	os.WriteFile(filepath.Join(dir, "readme.md"), []byte("ignore me"), 0644)

	prev := make(map[string]*IndexEntry)
	index := ScanLocalIndex(dir, 42, prev)

	if len(index) != 2 {
		t.Fatalf("got %d entries, want 2", len(index))
	}
	if _, ok := index["a.agent.yaml"]; !ok {
		t.Error("missing a.agent.yaml")
	}
	if _, ok := index["b.agent.yaml"]; !ok {
		t.Error("missing b.agent.yaml")
	}

	// Entries should have version vectors and non-zero hashes.
	for name, entry := range index {
		if entry.Version == nil {
			t.Errorf("%s: nil version", name)
		}
		if len(entry.BlocksHash) == 0 {
			t.Errorf("%s: empty BlocksHash", name)
		}
		if entry.Size == 0 {
			t.Errorf("%s: zero size", name)
		}
	}
}

func TestScanLocalIndexDetectsDeletion(t *testing.T) {
	dir := t.TempDir()

	prev := map[string]*IndexEntry{
		"gone.agent.yaml": {
			Name:    "gone.agent.yaml",
			Version: &VersionVector{Counters: map[uint64]uint64{1: 3}},
		},
	}

	index := ScanLocalIndex(dir, 1, prev)

	entry, ok := index["gone.agent.yaml"]
	if !ok {
		t.Fatal("missing deletion entry")
	}
	if !entry.Deleted {
		t.Error("expected Deleted=true for missing file")
	}
}

func TestScanLocalIndexPreservesVersionOnUnchanged(t *testing.T) {
	dir := t.TempDir()
	data := []byte("apiVersion: cog.os/v1alpha1\nkind: Agent\nmetadata:\n  name: test\n")
	os.WriteFile(filepath.Join(dir, "test.agent.yaml"), data, 0644)

	// First scan.
	prev := make(map[string]*IndexEntry)
	index1 := ScanLocalIndex(dir, 1, prev)

	// Second scan with previous index — version should be preserved.
	index2 := ScanLocalIndex(dir, 1, index1)

	if index2["test.agent.yaml"].Version.Counters[1] != index1["test.agent.yaml"].Version.Counters[1] {
		t.Error("version vector changed for unchanged file")
	}
}

// ─── DiffIndex ──────────────────────────────────────────────────────────────────

func TestDiffIndexRemoteNewer(t *testing.T) {
	local := map[string]*IndexEntry{
		"a.agent.yaml": {Name: "a.agent.yaml", Version: &VersionVector{Counters: map[uint64]uint64{1: 3}}},
	}
	remote := map[string]*IndexEntry{
		"a.agent.yaml": {Name: "a.agent.yaml", Version: &VersionVector{Counters: map[uint64]uint64{1: 5}}},
	}

	diff := DiffIndex(local, remote)
	if len(diff.ToRequest) != 1 || diff.ToRequest[0] != "a.agent.yaml" {
		t.Errorf("ToRequest = %v, want [a.agent.yaml]", diff.ToRequest)
	}
	if len(diff.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none", diff.Conflicts)
	}
}

func TestDiffIndexLocalNewer(t *testing.T) {
	local := map[string]*IndexEntry{
		"a.agent.yaml": {Name: "a.agent.yaml", Version: &VersionVector{Counters: map[uint64]uint64{1: 5}}},
	}
	remote := map[string]*IndexEntry{
		"a.agent.yaml": {Name: "a.agent.yaml", Version: &VersionVector{Counters: map[uint64]uint64{1: 3}}},
	}

	diff := DiffIndex(local, remote)
	if len(diff.ToRequest) != 0 {
		t.Errorf("ToRequest should be empty when local is newer, got %v", diff.ToRequest)
	}
}

func TestDiffIndexConcurrent(t *testing.T) {
	local := map[string]*IndexEntry{
		"a.agent.yaml": {Name: "a.agent.yaml", Version: &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 2}}},
	}
	remote := map[string]*IndexEntry{
		"a.agent.yaml": {Name: "a.agent.yaml", Version: &VersionVector{Counters: map[uint64]uint64{1: 3, 2: 4}}},
	}

	diff := DiffIndex(local, remote)
	if len(diff.Conflicts) != 1 || diff.Conflicts[0] != "a.agent.yaml" {
		t.Errorf("Conflicts = %v, want [a.agent.yaml]", diff.Conflicts)
	}
}

func TestDiffIndexNewRemoteFile(t *testing.T) {
	local := map[string]*IndexEntry{}
	remote := map[string]*IndexEntry{
		"new.agent.yaml": {Name: "new.agent.yaml", Size: 100, Version: NewVersionVector()},
	}

	diff := DiffIndex(local, remote)
	if len(diff.ToRequest) != 1 || diff.ToRequest[0] != "new.agent.yaml" {
		t.Errorf("ToRequest = %v, want [new.agent.yaml]", diff.ToRequest)
	}
}

func TestDiffIndexDeletedRemoteFile(t *testing.T) {
	local := map[string]*IndexEntry{}
	remote := map[string]*IndexEntry{
		"gone.agent.yaml": {Name: "gone.agent.yaml", Deleted: true, Version: NewVersionVector()},
	}

	diff := DiffIndex(local, remote)
	// Deleted file with no local entry → nothing to request.
	if len(diff.ToRequest) != 0 {
		t.Errorf("ToRequest = %v, want empty for deleted remote with no local", diff.ToRequest)
	}
}

// ─── IndexEntry BEP conversion ──────────────────────────────────────────────────

func TestIndexEntryBEPRoundTrip(t *testing.T) {
	orig := &IndexEntry{
		Name:       "test.agent.yaml",
		Size:       2048,
		ModifiedS:  1700000000,
		ModifiedNs: 123456789,
		Sequence:   5,
		Version:    &VersionVector{Counters: map[uint64]uint64{42: 5}},
		BlocksHash: []byte("hash123"),
		Deleted:    false,
	}

	fi := orig.ToBEPFileInfo(42)
	got := IndexEntryFromBEP(fi)

	if got.Name != orig.Name {
		t.Errorf("Name = %q", got.Name)
	}
	if got.Size != orig.Size {
		t.Errorf("Size = %d", got.Size)
	}
	if got.Sequence != orig.Sequence {
		t.Errorf("Sequence = %d", got.Sequence)
	}
}

// ─── Index persistence ──────────────────────────────────────────────────────────

func TestPersistAndLoadIndex(t *testing.T) {
	dir := t.TempDir()

	index := map[string]*IndexEntry{
		"a.agent.yaml": {
			Name:       "a.agent.yaml",
			Size:       1024,
			ModifiedS:  1700000000,
			Sequence:   3,
			Version:    &VersionVector{Counters: map[uint64]uint64{1: 3}},
			BlocksHash: []byte("hash-a"),
		},
		"b.agent.yaml": {
			Name:    "b.agent.yaml",
			Deleted: true,
			Version: &VersionVector{Counters: map[uint64]uint64{1: 4}},
		},
	}

	if err := PersistIndex(dir, "TEST-ID", index); err != nil {
		t.Fatalf("PersistIndex: %v", err)
	}

	// Verify file exists.
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
		t.Fatalf("index.json not found: %v", err)
	}

	loaded, err := LoadPersistedIndex(dir)
	if err != nil {
		t.Fatalf("LoadPersistedIndex: %v", err)
	}

	if len(loaded) != 2 {
		t.Fatalf("loaded %d entries, want 2", len(loaded))
	}

	a := loaded["a.agent.yaml"]
	if a == nil || a.Size != 1024 {
		t.Error("entry a not loaded correctly")
	}

	b := loaded["b.agent.yaml"]
	if b == nil || !b.Deleted {
		t.Error("entry b not loaded correctly (should be deleted)")
	}
}

func TestLoadPersistedIndexMissing(t *testing.T) {
	dir := t.TempDir()
	loaded, err := LoadPersistedIndex(dir)
	if err != nil {
		t.Fatalf("LoadPersistedIndex should not error for missing file: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("expected empty index, got %d entries", len(loaded))
	}
}

func TestPersistIndexNoTmpLeftBehind(t *testing.T) {
	dir := t.TempDir()

	index := map[string]*IndexEntry{
		"test.agent.yaml": {Name: "test.agent.yaml", Version: NewVersionVector()},
	}

	if err := PersistIndex(dir, "TEST", index); err != nil {
		t.Fatalf("PersistIndex: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover .tmp file: %s", e.Name())
		}
	}
}

// ─── Ordering.String ────────────────────────────────────────────────────────────

func TestOrderingString(t *testing.T) {
	tests := []struct {
		o    Ordering
		want string
	}{
		{OrderEqual, "equal"},
		{OrderGreater, "greater"},
		{OrderLesser, "lesser"},
		{OrderConcurrent, "concurrent"},
		{Ordering(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.o.String(); got != tt.want {
			t.Errorf("Ordering(%d).String() = %q, want %q", tt.o, got, tt.want)
		}
	}
}
